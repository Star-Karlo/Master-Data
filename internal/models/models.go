// Package models holds the MongoDB documents this service owns.
//
// Master data is the single-use data shared across TMS and FMS: one record per
// real-world thing — a vehicle, a GPS device, a site, an item — read by both
// products. The point is that a truck is not one record in FMS and a different
// one in TMS, free to disagree.
//
// Two conventions run through everything here, and both exist because MongoDB
// cannot enforce what PostgreSQL would.
//
// # Normalised twins
//
// Every human-entered name, code or serial has a `*Normalised` companion, and
// the unique index is on the COMPANION, not on what the operator typed.
// MongoDB has no functional indexes, so "B 1234 XYZ" and "b1234xyz" are two
// different strings to a unique index on the raw field — the old service
// accepted them as two trucks, and a third for "B1234XYZ".
//
// The twins are written by BeforeWrite, which every repository calls. That
// makes it the guarantee: a write path that skips it produces a document with
// the field missing, the partial index ignores the document, and the duplicate
// is created silently.
//
// # Global versus company rows
//
// A reference list holds Karlo's own entries and each company's additions in
// one collection. CompanyID nil means global. CompanyKey exists because a
// compound unique index does not treat two missing fields as equal, so a global
// entry needs a stand-in value for two global rows to collide with each other.
package models

import (
	"strings"
	"time"

	"github.com/karlo/masterdata-service/internal/config"
	"github.com/karlo/masterdata-service/internal/platform/normalise"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// UnitType distinguishes the three kinds of vehicle.
//
// A head and a body are SEPARATE VEHICLES that couple, not one record. Each has
// its own plate, its own STNK and KIR and its own inspection dates — in
// Indonesia a trailer is registered separately — and each is bought, sold and
// repaired independently.
type UnitType string

const (
	// UnitRigid is one unit: engine and cargo bed together. Most light trucks.
	UnitRigid UnitType = "rigid"
	// UnitHead is a tractor unit. It pulls a body and carries nothing itself,
	// which is why cargo figures must never be set on one.
	UnitHead UnitType = "head"
	// UnitBody is a trailer or container chassis. It carries the load and has
	// no engine.
	UnitBody UnitType = "body"
)

// TrackerOwner says who owns a device, which is a different question from where
// it is deployed — and the two differ constantly, because Karlo lends devices
// and a customer may bring their own.
// TrackerKind is what kind of device a trackers row is.
type TrackerKind string

const (
	TrackerGPS     TrackerKind = "gps"
	TrackerDashcam TrackerKind = "dashcam"
)

type TrackerOwner string

const (
	OwnerKarlo    TrackerOwner = "karlo"
	OwnerCustomer TrackerOwner = "customer"
	OwnerVendor   TrackerOwner = "vendor"
)

// SiteType says what a place is for. All three are a coordinate with a radius;
// what differs is who uses them and why.
type SiteType string

const (
	SiteWarehouse SiteType = "warehouse" // a loading or unloading point on an order
	SiteDepot     SiteType = "depot"     // where a transporter keeps its fleet
	SitePort      SiteType = "port"      // a sea or dry port
	SiteCustomer  SiteType = "customer"
	SiteOther     SiteType = "other"
)

// SiteRelation is what a company may do with a site it did not create.
//
// This is what lets a transporter SEE a shipper's warehouse — which is where
// they load — without being able to move its geofence.
type SiteRelation string

const (
	RelationOwner    SiteRelation = "owner"
	RelationOperator SiteRelation = "operator"
	RelationUser     SiteRelation = "user"
)

// Base is the envelope every master data document carries.
// Base is embedded by every document, and every embedder MUST tag it
// `bson:",inline"`.
//
// The driver does not flatten an embedded struct on its own — untagged, these
// fields are written into a nested `base` sub-document instead of at the top
// level. Nothing in Go complains; the write simply produces a shape the
// collection validators reject for missing createdAt, and every unique index
// built on a Base field silently matches nothing.
type Base struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CreatedAt time.Time          `bson:"createdAt" json:"createdAt"`
	UpdatedAt time.Time          `bson:"updatedAt" json:"updatedAt"`

	// Deleted is a soft delete, and every partial unique index filters on it —
	// so removing a vehicle frees its plate, which a hard boolean-free design
	// could not express.
	Deleted bool `bson:"deleted" json:"-"`
}

// Owned is the envelope for a reference list that Karlo and companies share.
type Owned struct {
	Base `bson:",inline"`

	// CompanyID nil means a global entry Karlo maintains; a value means the
	// company added their own and only they see it.
	CompanyID *string `bson:"companyId" json:"companyId,omitempty"`

	CreatedByUserID *string `bson:"createdByUserId" json:"createdByUserId,omitempty"`
	IsActive        bool    `bson:"isActive" json:"isActive"`
}

// The global-versus-company uniqueness rule needs no stored key.
//
// An earlier design stored a CompanyKey — CompanyID with a stand-in for nil —
// because a compound unique index does not treat two missing fields as equal,
// so two global rows sharing a name would both have been accepted.
//
// TWO PARTIAL INDEXES solve it without the extra field, and are what the schema
// uses: one unique on the name WHERE companyId is null, one unique on
// (companyId, name) WHERE it is not. Two globals with one name collide; a
// company may still use a name a global entry already has. The stored key was
// written into every document and read by nothing.

// SetID stamps the document's identity.
//
// Exists so a generic writer can mint an id without reflection: Go generics
// cannot reach an embedded field through a type parameter, but they can assert
// a one-method interface.
func (b *Base) SetID(id primitive.ObjectID) { b.ID = id }

// Stamp maintains the timestamps.
//
// MongoDB has no equivalent of the trigger that did this in PostgreSQL, so
// without it a raw write leaves updatedAt untouched and "what changed recently"
// silently misses rows.
//
// EXPORTED, and it has to be. It was unexported, with a note saying that kept
// anyone else from setting these by hand — but the repository asserts for it
// through an anonymous interface in ANOTHER package, and an unexported method
// can never satisfy one of those. The assertion failed silently on every write,
// so createdAt and updatedAt were never set: exactly the problem the method
// exists to prevent, caused by the attempt to enforce it. The collection
// validators require both fields, so every insert was refused.
//
// The discipline it was reaching for belongs in the repository, which is the
// only caller: Store.Create and Store.Update stamp, and nothing else writes.
func (b *Base) Stamp(now time.Time, creating bool) {
	if creating {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
}

// ---------------------------------------------------------------------------
// Reference lists
// ---------------------------------------------------------------------------
// Each is its own collection rather than one table with a `kind` column.
//
// The earlier design funnelled brands, truck heads, truck bodies, cargo types
// and item categories into one collection, told apart by `kind`. It meant no
// field could be required — every row was a different sort of thing — and a
// diagram of it showed five arrows into one box, which says nothing. Separately,
// each list carries the fields that actually belong to it: a brand has a logo,
// a body has dimensions, and neither has the other's.

// Brand is a vehicle manufacturer.
type Brand struct {
	Owned          `bson:",inline"`
	Name           string `bson:"name" json:"name"`
	NameNormalised string `bson:"nameNormalised" json:"-"`
}

func (Brand) CollectionName() string { return config.Collection("brands") }

func (b *Brand) BeforeWrite() {
	b.NameNormalised = normalise.Name(b.Name)
}

// CargoType is what is being moved: general cargo, reefer, liquid, bulk.
type CargoType struct {
	Owned          `bson:",inline"`
	Name           string  `bson:"name" json:"name"`
	NameNormalised string  `bson:"nameNormalised" json:"-"`
	Description    *string `bson:"description" json:"description,omitempty"`

	// How it must be handled — refrigerated, hazardous, fragile. An array
	// rather than its own collection: a short list of adjectives read with the
	// cargo type and never queried on its own.
	Characteristics []string `bson:"characteristics" json:"characteristics"`
}

func (CargoType) CollectionName() string { return config.Collection("cargo_types") }

func (c *CargoType) BeforeWrite() {
	c.NameNormalised = normalise.Name(c.Name)
	if c.Characteristics == nil {
		c.Characteristics = []string{}
	}
}

// ItemCategory is the top level of the goods classification.
type ItemCategory struct {
	Owned          `bson:",inline"`
	Name           string  `bson:"name" json:"name"`
	NameNormalised string  `bson:"nameNormalised" json:"-"`
	Description    *string `bson:"description" json:"description,omitempty"`
}

func (ItemCategory) CollectionName() string { return config.Collection("item_categories") }

func (c *ItemCategory) BeforeWrite() {
	c.NameNormalised = normalise.Name(c.Name)
}

// ItemSubCategory sits under a category.
//
// Its own collection rather than a category with a parent pointer. A
// sub-category is a different level with a different meaning, and separating
// them means "the top-level list" is a collection rather than a filter somebody
// can forget to apply.
type ItemSubCategory struct {
	Owned `bson:",inline"`

	// CategoryID is required: a sub-category with no category is not one.
	CategoryID     string  `bson:"categoryId" json:"categoryId"`
	Name           string  `bson:"name" json:"name"`
	NameNormalised string  `bson:"nameNormalised" json:"-"`
	Description    *string `bson:"description" json:"description,omitempty"`
}

func (ItemSubCategory) CollectionName() string {
	return config.Collection("item_sub_categories")
}

func (c *ItemSubCategory) BeforeWrite() {
	c.NameNormalised = normalise.Name(c.Name)
}

// Item is a specific thing a company ships.
type Item struct {
	Base `bson:",inline"`

	// Always company-owned: what a company ships is its own business, and there
	// is no useful global list of goods.
	CompanyID string `bson:"companyId" json:"companyId"`

	CategoryID    *string `bson:"categoryId" json:"categoryId,omitempty"`
	SubCategoryID *string `bson:"subCategoryId" json:"subCategoryId,omitempty"`
	CargoTypeID   *string `bson:"cargoTypeId" json:"cargoTypeId,omitempty"`

	Name           string  `bson:"name" json:"name"`
	Code           *string `bson:"code" json:"code,omitempty"`
	CodeNormalised *string `bson:"codeNormalised" json:"-"`
	Description    *string `bson:"description" json:"description,omitempty"`

	// What ONE unit weighs and measures, so an order can total a load and
	// compare it against a vehicle's capacity. This is the figure that makes
	// "will this fit" answerable.
	Unit     *string  `bson:"unit" json:"unit,omitempty"`
	WeightKg *float64 `bson:"weightKg" json:"weightKg,omitempty"`
	VolumeM3 *float64 `bson:"volumeM3" json:"volumeM3,omitempty"`

	IsActive        bool                   `bson:"isActive" json:"isActive"`
	Attributes      map[string]interface{} `bson:"attributes" json:"attributes,omitempty"`
	CreatedByUserID *string                `bson:"createdByUserId" json:"createdByUserId,omitempty"`
}

func (Item) CollectionName() string { return config.Collection("items") }

func (i *Item) BeforeWrite() {
	i.CodeNormalised = normalise.Optional(normalise.Code, i.Code)
	if i.Attributes == nil {
		i.Attributes = map[string]interface{}{}
	}
}

// TruckHead is a KIND of tractor unit — not a physical one.
//
// Separate from TruckBody because the two describe different things: a head has
// axles and a drive configuration and carries nothing; a body has dimensions
// and a weight limit and has no engine. As one collection with a discriminator
// every field had to be nullable, because half applied to half the rows.
type TruckHead struct {
	Owned          `bson:",inline"`
	Name           string  `bson:"name" json:"name"`
	NameNormalised string  `bson:"nameNormalised" json:"-"`
	Code           *string `bson:"code" json:"code,omitempty"`

	Axles         *int    `bson:"axles" json:"axles,omitempty"`
	Configuration *string `bson:"configuration" json:"configuration,omitempty"`
	ImageKey      *string `bson:"imageKey" json:"imageKey,omitempty"`
}

func (TruckHead) CollectionName() string { return config.Collection("truck_heads") }

func (h *TruckHead) BeforeWrite() {
	h.NameNormalised = normalise.Name(h.Name)
}

// TruckBody is a KIND of body or trailer, and it carries the SPEC.
//
// MaxWeightKg here is what a body of this kind generally holds. A vehicle
// overrides it only when its own figures differ — see Vehicle. Two places, two
// questions: this is the spec, that is the exception.
type TruckBody struct {
	Owned          `bson:",inline"`
	Name           string  `bson:"name" json:"name"`
	NameNormalised string  `bson:"nameNormalised" json:"-"`
	Code           *string `bson:"code" json:"code,omitempty"`

	MaxWeightKg *float64 `bson:"maxWeightKg" json:"maxWeightKg,omitempty"`
	VolumeM3    *float64 `bson:"volumeM3" json:"volumeM3,omitempty"`
	LengthM     *float64 `bson:"lengthM" json:"lengthM,omitempty"`
	WidthM      *float64 `bson:"widthM" json:"widthM,omitempty"`
	HeightM     *float64 `bson:"heightM" json:"heightM,omitempty"`

	// Which cargo this body suits. Ids into cargo types, held inline because
	// the list is short and always read with the body.
	CargoTypeIDs []string `bson:"cargoTypeIds" json:"cargoTypeIds"`

	ImageKey *string `bson:"imageKey" json:"imageKey,omitempty"`
}

func (TruckBody) CollectionName() string { return config.Collection("truck_bodies") }

// TruckClass is the SIZE axis: CDE, CDD, Tronton, Trailer 40FT.
//
// Separate from TruckBody because the two are independent. A wingbox exists as
// a CDD and as a Tronton, and an agreement prices the COMBINATION — so one list
// of "CDD Wingbox" names would be the product of the two, and could not answer
// "every Tronton we carry" without parsing strings.
type TruckClass struct {
	Owned          `bson:",inline"`
	Name           string  `bson:"name" json:"name"`
	NameNormalised string  `bson:"nameNormalised" json:"-"`
	Code           *string `bson:"code" json:"code,omitempty"`

	// Indicative only. What a truck of this size generally carries; the BODY
	// holds the figure a load is priced against.
	TypicalMaxWeightKg *float64 `bson:"typicalMaxWeightKg" json:"typicalMaxWeightKg,omitempty"`
	Axles              *int     `bson:"axles" json:"axles,omitempty"`

	// These have a natural order — smallest to largest — that alphabetical
	// sorting destroys. CDD before CDE before Tronton is not how anybody thinks
	// about trucks.
	SortOrder *int `bson:"sortOrder" json:"sortOrder,omitempty"`
}

func (TruckClass) CollectionName() string { return config.Collection("truck_classes") }

func (c *TruckClass) BeforeWrite() {
	c.NameNormalised = normalise.Name(c.Name)
}

func (b *TruckBody) BeforeWrite() {
	b.NameNormalised = normalise.Name(b.Name)
	if b.CargoTypeIDs == nil {
		b.CargoTypeIDs = []string{}
	}
}

// ---------------------------------------------------------------------------
// Vehicles
// ---------------------------------------------------------------------------

// Vehicle is one physical unit: a rigid truck, a tractor head, or a trailer.
//
// This is the record the whole service exists for. FMS held it as `vehicles` in
// Postgres and TMS as `md_trucks` in Mongo — same plate, same chassis, two rows
// that nothing kept in step. One record, read by both.
type Vehicle struct {
	Base `bson:",inline"`

	CompanyID string `bson:"companyId" json:"companyId"`

	// What everyone calls the truck, exactly as the operator typed it.
	LicensePlate string `bson:"licensePlate" json:"licensePlate"`

	// What the unique index is actually on.
	//
	// The plate above keeps the operator's spacing; this is the folded form. An
	// index on the raw plate accepted "B 1234 XYZ", "b1234xyz" and "B1234XYZ"
	// as three trucks, which is the duplication this service was built to end.
	PlateNormalised string `bson:"plateNormalised" json:"-"`

	// The numbers that identify the PHYSICAL vehicle rather than its
	// registration. A plate is reissued when a vehicle is sold; these are not,
	// which is why the chassis number is unique everywhere and the plate only
	// within a company.
	ChassisNumber     *string `bson:"chassisNumber" json:"chassisNumber,omitempty"`
	ChassisNormalised *string `bson:"chassisNormalised" json:"-"`
	EngineNumber      *string `bson:"engineNumber" json:"engineNumber,omitempty"`

	UnitType UnitType `bson:"unitType" json:"unitType"`

	// A head names a head type; a body or a rigid names a body type. Both
	// fields rather than one, because a rigid truck has a body and no separate
	// head — forcing them through one field would mean guessing which list an
	// id belongs to.
	TruckHeadID *string `bson:"truckHeadId" json:"truckHeadId,omitempty"`
	TruckBodyID *string `bson:"truckBodyId" json:"truckBodyId,omitempty"`

	BrandID *string `bson:"brandId" json:"brandId,omitempty"`

	// TruckGroupID is the vehicle group (vehicle_groups) this unit belongs
	// to — one, since FMS's fleet group is single-valued and the two products
	// share the field (Truck.truck_group_id on the wire).
	TruckGroupID *string `bson:"truckGroupId" json:"truckGroupId,omitempty"`

	UnitYear *int    `bson:"unitYear" json:"unitYear,omitempty"`
	Color    *string `bson:"color" json:"color,omitempty"`

	// TrackerID is a COPY of the live tracker assignment, kept so "which device
	// is on this vehicle" does not need a query over history.
	//
	// Nothing in MongoDB keeps it in step. In PostgreSQL a trigger did; here
	// every writer must, and a writer that forgets shows a device on the wrong
	// truck. Use the tracker repository's fitting methods rather than setting
	// this directly.
	TrackerID *string `bson:"trackerId" json:"trackerId,omitempty"`
	// CurrentDriverID is the drivers document; CurrentDriverUserID is that
	// driver's auth user, copied from the document when they have a login.
	// Both are written by the same assignment path; the second stays because
	// business-service's dispatch reads driver ids as auth users and a
	// driver with no login must contribute nothing there.
	CurrentDriverID     *string `bson:"currentDriverId" json:"currentDriverId,omitempty"`
	CurrentDriverUserID *string `bson:"currentDriverUserId" json:"currentDriverUserId,omitempty"`

	Status string `bson:"status" json:"status"`
	// TMS asks this before assigning a load; FMS does not care.
	IsAvailable bool `bson:"isAvailable" json:"isAvailable"`

	OdometerKm     *float64 `bson:"odometerKm" json:"odometerKm,omitempty"`
	HourmeterHours *float64 `bson:"hourmeterHours" json:"hourmeterHours,omitempty"`
	FuelTankLiters *float64 `bson:"fuelTankLiters" json:"fuelTankLiters,omitempty"`
	FuelRatioKmpl  *float64 `bson:"fuelRatioKmpl" json:"fuelRatioKmpl,omitempty"`

	// The cargo figures are NOT here.
	//
	// They belong to the body type — a body of a given kind holds what it holds
	// — and repeating them on the vehicle meant two places to look with nothing
	// saying which won. Read TruckBodyID and take the figures from there.
	//
	// What this gives up is a single unit that differs from its type: a truck
	// derated after damage. If that turns out to matter the field returns here
	// as a nullable override meaning "use the type's".

	Notes      *string                `bson:"notes" json:"notes,omitempty"`
	Attributes map[string]interface{} `bson:"attributes,omitempty" json:"attributes,omitempty"`
}

func (Vehicle) CollectionName() string { return config.Collection("vehicles") }

func (v *Vehicle) BeforeWrite() {
	v.PlateNormalised = normalise.Plate(v.LicensePlate)
	v.ChassisNormalised = normalise.Optional(normalise.Serial, v.ChassisNumber)
	if v.UnitType == "" {
		v.UnitType = UnitRigid
	}
	if v.Status == "" {
		v.Status = "active"
	}
	if v.Attributes == nil {
		v.Attributes = map[string]interface{}{}
	}
}

// Validate enforces what the database cannot.
//
// $jsonSchema can require a field and check its type, but it cannot compare one
// field against another, so anything conditional lives here and must be called
// before every write.
//
// Only one rule remains. The head-carries-nothing check went with the cargo
// figures themselves: they are on the body type now, so a head has nowhere to
// record a weight it could not carry.
func (v *Vehicle) Validate() error {
	// A plate that normalises to nothing — "---" — would sit outside the
	// unique index's reach, and two such vehicles would both be accepted.
	if v.LicensePlate == "" || v.PlateNormalised == "" {
		return ErrPlateRequired
	}
	return nil
}

// VehicleGroup groups vehicles for notifications and bulk tasks — "alert me
// about the Jakarta fleet".
type VehicleGroup struct {
	Base           `bson:",inline"`
	CompanyID      string  `bson:"companyId" json:"companyId"`
	Name           string  `bson:"name" json:"name"`
	NameNormalised string  `bson:"nameNormalised" json:"-"`
	Description    *string `bson:"description" json:"description,omitempty"`

	// Who to tell about this group. The point of a group is usually that
	// somebody is responsible for it.
	PICUserIDs []string `bson:"picUserIds" json:"picUserIds"`
}

func (VehicleGroup) CollectionName() string { return config.Collection("vehicle_groups") }

func (g *VehicleGroup) BeforeWrite() {
	g.NameNormalised = normalise.Name(g.Name)
	if g.PICUserIDs == nil {
		g.PICUserIDs = []string{}
	}
}

// Customer is one entry in a company's own consignee register: an
// end-recipient its orders are delivered to.
//
// NOT a platform user. A customer never signs in and usually has no account
// anywhere — it exists so an order can name who is receiving the goods and an
// invoice can be addressed. That is why the only required field beyond the
// owner is a name; everything else is filled in as it becomes known.
//
// Company-scoped with no shared variant, unlike a cargo type or a truck body:
// one company's customer list is its commercial relationships, and must never
// appear in another company's pickers.
// Driver is a person who drives, as master data: a name, a phone number and
// a licence. Not an identity — most drivers never sign in — but linked to
// one through UserID when they do, so the TMS driver app and the FMS
// scorecard describe the same person. FMS's employees and TMS's drivers are
// one register here.
type Driver struct {
	Base      `bson:",inline"`
	CompanyID string `bson:"companyId" json:"companyId"`

	FullName       string  `bson:"fullName" json:"fullName"`
	NameNormalised string  `bson:"nameNormalised" json:"-"`
	Phone          *string `bson:"phone" json:"phone,omitempty"`
	// PhoneNormalised is digits only, for the uniqueness index.
	PhoneNormalised *string `bson:"phoneNormalised" json:"-"`

	EmployeeNo *string `bson:"employeeNo" json:"employeeNo,omitempty"`

	LicenseNo    *string `bson:"licenseNo" json:"licenseNo,omitempty"`
	LicenseClass *string `bson:"licenseClass" json:"licenseClass,omitempty"`
	// LicenseExpiry is a date; stored at UTC midnight, no time is meaningful.
	LicenseExpiry *time.Time `bson:"licenseExpiry" json:"licenseExpiry,omitempty"`

	// Status is active or inactive. Retirement is Deleted, like everywhere.
	Status string `bson:"status" json:"status"`

	// UserID is the auth user when this person has a login. Absent — not
	// null — when nobody has looked: an import cannot assert there is no
	// account, only that it did not link one.
	UserID *string `bson:"userId,omitempty" json:"userId,omitempty"`

	Notes      *string                `bson:"notes" json:"notes,omitempty"`
	Attributes map[string]interface{} `bson:"attributes,omitempty" json:"attributes,omitempty"`
}

func (Driver) CollectionName() string { return config.Collection("drivers") }

func (d *Driver) BeforeWrite() {
	d.FullName = strings.TrimSpace(d.FullName)
	d.NameNormalised = normalise.Name(d.FullName)
	d.Phone = normalise.Optional(strings.TrimSpace, d.Phone)
	d.PhoneNormalised = normalise.Optional(normalise.IMEI, d.Phone) // digits only
	d.EmployeeNo = normalise.Optional(strings.TrimSpace, d.EmployeeNo)
	d.LicenseNo = normalise.Optional(normalise.Serial, d.LicenseNo)
	d.LicenseClass = normalise.Optional(strings.TrimSpace, d.LicenseClass)
	if d.LicenseExpiry != nil {
		t := d.LicenseExpiry.UTC()
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		d.LicenseExpiry = &day
	}
	if d.Status == "" {
		d.Status = "active"
	}
	if d.Attributes == nil {
		d.Attributes = map[string]interface{}{}
	}
}

func (d *Driver) Validate() error {
	if d.FullName == "" {
		return ErrDriverNameRequired
	}
	if d.Status != "active" && d.Status != "inactive" {
		return ErrDriverStatus
	}
	return nil
}

type Customer struct {
	Base           `bson:",inline"`
	CompanyID      string `bson:"companyId" json:"companyId"`
	Name           string `bson:"name" json:"name"`
	NameNormalised string `bson:"nameNormalised" json:"-"`
	// The customer's own reference for itself, printed on documents.
	Code         *string `bson:"code" json:"code,omitempty"`
	NPWP         *string `bson:"npwp" json:"npwp,omitempty"`
	Address      *string `bson:"address" json:"address,omitempty"`
	ContactName  *string `bson:"contactName" json:"contactName,omitempty"`
	ContactPhone *string `bson:"contactPhone" json:"contactPhone,omitempty"`
	ContactEmail *string `bson:"contactEmail" json:"contactEmail,omitempty"`
}

func (Customer) CollectionName() string { return config.Collection("customers") }

func (c *Customer) BeforeWrite() {
	c.NameNormalised = normalise.Name(c.Name)
}

// VehicleGroupMember puts a vehicle in a group.
//
// Superseded by Vehicle.TruckGroupID: membership is single-valued so that TMS
// and FMS (whose fleet group is one per vehicle) agree on it. The collection is
// kept for the notification-rule and PIC use it was designed for; the API no
// longer reads it for fleet membership.
type VehicleGroupMember struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CompanyID string             `bson:"companyId" json:"companyId"`
	GroupID   string             `bson:"groupId" json:"groupId"`
	VehicleID string             `bson:"vehicleId" json:"vehicleId"`

	AddedByUserID *string   `bson:"addedByUserId" json:"addedByUserId,omitempty"`
	CreatedAt     time.Time `bson:"createdAt" json:"createdAt"`
}

func (VehicleGroupMember) CollectionName() string {
	return config.Collection("vehicle_group_members")
}

// ---------------------------------------------------------------------------
// Trackers
// ---------------------------------------------------------------------------

// TrackerModel is the make and model of a GPS device.
//
// Without it a tracker was an IMEI and nothing else, so "does this vehicle
// report fuel" had no answer until somebody looked at the data and saw whether
// the field arrived.
type TrackerModel struct {
	ID     primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Vendor string             `bson:"vendor" json:"vendor"`
	Model  string             `bson:"model" json:"model"`
	// Kind says which register entries of this model belong to; gps unless
	// said otherwise, so every existing row keeps meaning what it did.
	Kind TrackerKind `bson:"kind" json:"kind"`

	// How it speaks, which is what the ingest service needs in order to decode
	// it: codec8, gt06.
	Protocol   *string                `bson:"protocol" json:"protocol,omitempty"`
	Attributes map[string]interface{} `bson:"attributes,omitempty" json:"attributes,omitempty"`

	IsActive  bool      `bson:"isActive" json:"isActive"`
	CreatedAt time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt" json:"updatedAt"`
}

func (TrackerModel) CollectionName() string { return config.Collection("tracker_models") }

// SetID and Stamp are spelled out rather than inherited from Base.
//
// This model predates Base and has its own id and timestamp fields; embedding
// Base now would rename them through the inline tag and orphan every existing
// document. Two small methods are the cheaper correctness.
func (m *TrackerModel) SetID(id primitive.ObjectID) { m.ID = id }

func (m *TrackerModel) Stamp(now time.Time, creating bool) {
	if creating {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
}

// BeforeWrite trims the pair the unique index is on.
//
// There is no normalised twin here: the index is on (vendor, model) verbatim,
// so trailing space is the only way two entries of the same device differ.
func (m *TrackerModel) BeforeWrite() {
	m.Vendor = strings.TrimSpace(m.Vendor)
	m.Model = strings.TrimSpace(m.Model)
	if m.Kind == "" {
		m.Kind = TrackerGPS
	}
	if !m.IsActive {
		m.IsActive = true
	}
}

// Tracker is one physical GPS device.
type Tracker struct {
	Base `bson:",inline"`

	// Where the device is DEPLOYED. Nil while it sits in Karlo's stock, which
	// is why this is optional when nearly every other CompanyID is not.
	CompanyID *string `bson:"companyId" json:"companyId,omitempty"`

	// Who OWNS it — a different question from where it is, and the two differ
	// constantly. Without the distinction nobody can answer "which of our units
	// are out with customers" or "who do we chase when this one fails".
	Owner     TrackerOwner `bson:"owner" json:"owner"`
	OwnerName *string      `bson:"ownerName" json:"ownerName,omitempty"`

	// Kind is what the device is: a GPS tracker or a dashcam. One register,
	// because both are a physical device fitted to a vehicle with a fitting
	// history, and two registers is how one vehicle ends up with a device
	// in each that nobody can see together.
	Kind TrackerKind `bson:"kind" json:"kind"`

	// DeviceID is the device's identity within its kind — the IMEI for a
	// GPS tracker, the vendor's device id for a dashcam — and it is GLOBAL:
	// two companies cannot hold the same physical device, and a duplicate
	// means one vehicle's telemetry appearing on another's map.
	DeviceID string `bson:"deviceId" json:"deviceId"`

	// IMEI is kept for GPS trackers, equal to DeviceID, because every
	// telemetry reading and assignment is keyed on it. Empty for a dashcam.
	IMEI string `bson:"imei,omitempty" json:"imei,omitempty"`

	// The SIM card itself. Survives the number changing, which a phone number
	// does not — a number is reassigned, an ICCID identifies the card.
	ICCID       *string `bson:"iccid" json:"iccid,omitempty"`
	SIMProvider *string `bson:"simProvider" json:"simProvider,omitempty"`
	// PhoneNo is the SIM's MSISDN — how the device is reached for SMS
	// commands. FMS keeps it per device; kept here so the two agree.
	PhoneNo *string `bson:"phoneNo" json:"phoneNo,omitempty"`

	// The model, always by reference. A free-text name sat beside this for
	// devices not yet catalogued, and it was the wrong answer: two spellings of
	// one model cannot be compared, so "every device of this model" — the
	// question asked when a firmware fault appears — would miss half of them.
	ModelID *string `bson:"modelId" json:"modelId,omitempty"`

	Status     string                 `bson:"status" json:"status"`
	Attributes map[string]interface{} `bson:"attributes,omitempty" json:"attributes,omitempty"`
}

func (Tracker) CollectionName() string { return config.Collection("trackers") }

func (t *Tracker) BeforeWrite() {
	if t.Kind == "" {
		t.Kind = TrackerGPS
	}
	switch t.Kind {
	case TrackerGPS:
		// Either field may have been supplied; they are the same number.
		if t.DeviceID == "" {
			t.DeviceID = t.IMEI
		}
		t.DeviceID = normalise.IMEI(t.DeviceID)
		t.IMEI = t.DeviceID
	default:
		t.DeviceID = strings.TrimSpace(t.DeviceID)
		t.IMEI = ""
	}
	t.ICCID = normalise.Optional(normalise.ICCID, t.ICCID)
	if t.Owner == "" {
		t.Owner = OwnerKarlo
	}
	if t.Status == "" {
		t.Status = "active"
	}
}

// SensorType is a kind of sensor. A list rather than free text, so "fuel" and
// "Fuel Level" cannot become two things nothing can compare.
type SensorType struct {
	ID   primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Code string             `bson:"code" json:"code"`
	Name string             `bson:"name" json:"name"`

	Unit *string `bson:"unit" json:"unit,omitempty"`
	// Decides how a reading is stored and charted. A door is not a quantity.
	ValueKind   string  `bson:"valueKind" json:"valueKind"`
	Description *string `bson:"description" json:"description,omitempty"`

	IsActive  bool      `bson:"isActive" json:"isActive"`
	CreatedAt time.Time `bson:"createdAt" json:"createdAt"`
}

func (SensorType) CollectionName() string { return config.Collection("sensor_types") }

func (t *SensorType) SetID(id primitive.ObjectID) { t.ID = id }

func (t *SensorType) Stamp(now time.Time, creating bool) {
	if creating {
		t.CreatedAt = now
	}
}

// BeforeWrite folds the code the unique index is on.
//
// Uppercased rather than lowercased because sensor codes are read as constants
// — FUEL, DOOR, TEMP — and a list that mixes "fuel" and "FUEL" as two sensors
// is the duplication the index exists to prevent.
func (t *SensorType) BeforeWrite() {
	t.Code = strings.ToUpper(strings.TrimSpace(t.Code))
	t.Name = strings.TrimSpace(t.Name)
	if t.ValueKind == "" {
		// A reading with no declared kind cannot be charted or compared, and
		// "number" is what all but the boolean sensors are.
		t.ValueKind = "number"
	}
	if !t.IsActive {
		t.IsActive = true
	}
}

// TrackerSensor is one sensor fitted to one device.
//
// Per DEVICE, not per model: two units of the same model can be wired
// differently, and it is the individual device that is connected to a fuel
// probe. Recording it against the model would claim every unit has one.
type TrackerSensor struct {
	ID           primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	TrackerID    string             `bson:"trackerId" json:"trackerId"`
	SensorTypeID string             `bson:"sensorTypeId" json:"sensorTypeId"`

	// Which input it is wired to. Two fuel probes on one tracker is normal on a
	// truck with two tanks, which is why the uniqueness includes the channel.
	Channel *string `bson:"channel" json:"channel,omitempty"`

	// ChannelKey is Channel with an empty string for absent. A compound unique
	// index does not treat two missing fields as equal, so without it an
	// unchannelled sensor could be fitted to the same device twice.
	ChannelKey string `bson:"channelKey" json:"-"`

	// The numbers that turn a raw reading into litres or degrees. Per fitting,
	// because the same probe in a different tank calibrates differently.
	Calibration map[string]interface{} `bson:"calibration" json:"calibration,omitempty"`

	FittedAt  time.Time  `bson:"fittedAt" json:"fittedAt"`
	RemovedAt *time.Time `bson:"removedAt" json:"removedAt,omitempty"`
	CreatedAt time.Time  `bson:"createdAt" json:"createdAt"`
}

func (TrackerSensor) CollectionName() string { return config.Collection("tracker_sensors") }

func (s *TrackerSensor) BeforeWrite() {
	s.ChannelKey = normalise.ChannelKey(s.Channel)
	if s.Calibration == nil {
		s.Calibration = map[string]interface{}{}
	}
}

// TrackerAssignment records which device was on which vehicle, and when.
//
// Without the history, telemetry recorded last month is attributed to whatever
// vehicle the device is on today — so a trip report for one truck silently
// includes another's journeys.
type TrackerAssignment struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CompanyID string             `bson:"companyId" json:"companyId"`

	// Nil once the device document is deleted. The copied IMEI below is what
	// still identifies it — which is the whole reason the IMEI is duplicated
	// here rather than joined from the tracker.
	TrackerID *string `bson:"trackerId" json:"trackerId,omitempty"`
	VehicleID string  `bson:"vehicleId" json:"vehicleId"`
	// What was fitted, copied from the device so the history outlives the
	// device document. Kind and DeviceID always; IMEI for GPS devices only,
	// because telemetry is keyed on it.
	Kind     TrackerKind `bson:"kind" json:"kind"`
	DeviceID string      `bson:"deviceId" json:"deviceId"`
	IMEI     string      `bson:"imei,omitempty" json:"imei,omitempty"`

	FittedAt   time.Time  `bson:"fittedAt" json:"fittedAt"`
	UnfittedAt *time.Time `bson:"unfittedAt" json:"unfittedAt,omitempty"`

	// Proof the installation actually happened.
	//
	// Fitting is done in a yard, often by a subcontractor, and "was this really
	// installed" is the first question when a device never reports. A photo
	// taken at the time answers it. An object-store key, never a URL — a signed
	// URL expires, so a stored one rots.
	InstallPhotoKey   *string `bson:"installPhotoKey" json:"installPhotoKey,omitempty"`
	InstalledByUserID *string `bson:"installedByUserId" json:"installedByUserId,omitempty"`
	InstallNotes      *string `bson:"installNotes" json:"installNotes,omitempty"`

	CreatedAt time.Time `bson:"createdAt" json:"createdAt"`
}

func (TrackerAssignment) CollectionName() string {
	return config.Collection("tracker_assignments")
}

func (a *TrackerAssignment) BeforeWrite() {
	if a.Kind == "" {
		a.Kind = TrackerGPS
	}
	if a.Kind == TrackerGPS {
		if a.DeviceID == "" {
			a.DeviceID = a.IMEI
		}
		a.DeviceID = normalise.IMEI(a.DeviceID)
		a.IMEI = a.DeviceID
	} else {
		a.IMEI = ""
	}
}

// ---------------------------------------------------------------------------
// Sites and documents
// ---------------------------------------------------------------------------

// GeoPoint is GeoJSON, so a 2dsphere index can answer "which site is this truck
// at". Two plain numbers cannot.
type GeoPoint struct {
	Type string `bson:"type" json:"type"`
	// Longitude FIRST, then latitude. GeoJSON's order is the reverse of how
	// coordinates are usually spoken, and getting it wrong puts an Indonesian
	// site in the Indian Ocean without any error.
	Coordinates []float64 `bson:"coordinates" json:"coordinates"`
}

// Site is anywhere a vehicle stops: a warehouse, a depot, a port.
//
// One collection with a type rather than one per kind, because all three are a
// name, a coordinate and a radius answering the same question — is the vehicle
// there — and a geofence check written twice is one that disagrees with itself.
type Site struct {
	Base `bson:",inline"`

	CompanyID      string   `bson:"companyId" json:"companyId"`
	SiteType       SiteType `bson:"siteType" json:"siteType"`
	Name           string   `bson:"name" json:"name"`
	NameNormalised string   `bson:"nameNormalised" json:"-"`

	// What the geocoder returned, stored so the lookup happens once rather than
	// on every read — and because a coordinate alone cannot be shown to a
	// driver or printed on a document.
	// City and province are NAMES, not ids into a region table. There is no
	// region table, and an id referencing nothing is worse than the name it
	// stands for: it cannot be read, printed or searched without a lookup that
	// does not exist.
	Address  *string `bson:"address" json:"address,omitempty"`
	Street   *string `bson:"street" json:"street,omitempty"`
	District *string `bson:"district" json:"district,omitempty"`
	City     *string `bson:"city" json:"city,omitempty"`
	Province *string `bson:"province" json:"province,omitempty"`
	Postcode *string `bson:"postcode" json:"postcode,omitempty"`

	Location *GeoPoint `bson:"location" json:"location,omitempty"`
	// How close counts as "arrived". Per site, because a roadside drop needs a
	// wider radius than a fenced yard.
	GeofenceRadiusM *int `bson:"geofenceRadiusM" json:"geofenceRadiusM,omitempty"`

	// Who to call at the gate — the person in charge of this site, not the
	// company switchboard.
	// PICs are the people at the gate. One is the default: the one the
	// driver app proposes for the handover code and the planner sees first
	// when placing an order. SitePICName / SitePICPhone mirror that default
	// for readers that predate the list (gRPC, the FMS projection).
	PICs         []SitePIC `bson:"pics" json:"pics"`
	SitePICName  *string   `bson:"sitePicName" json:"sitePicName,omitempty"`
	SitePICPhone *string   `bson:"sitePicPhone" json:"sitePicPhone,omitempty"`
	Notes        *string   `bson:"notes" json:"notes,omitempty"`
	PICUserIDs   []string  `bson:"picUserIds" json:"picUserIds"`
	// CustomerCompanyID says whose site this is when a transporter keeps
	// its customers' warehouses in its own register (MyWarehouse groups
	// them per customer). Empty for the company's own sites.
	CustomerCompanyID *string `bson:"customerCompanyId" json:"customerCompanyId,omitempty"`

	IsActive   bool                   `bson:"isActive" json:"isActive"`
	Attributes map[string]interface{} `bson:"attributes,omitempty" json:"attributes,omitempty"`
}

func (Site) CollectionName() string { return config.Collection("sites") }

// SitePIC is one contact at a site.
type SitePIC struct {
	ID        string `bson:"id" json:"id"`
	Name      string `bson:"name" json:"name"`
	Phone     string `bson:"phone" json:"phone,omitempty"`
	IsDefault bool   `bson:"isDefault" json:"isDefault"`
}

func (s *Site) BeforeWrite() {
	s.NameNormalised = normalise.Name(s.Name)
	if s.PICs == nil {
		s.PICs = []SitePIC{}
	}
	// A nil map would be written as BSON null, and the collection validator
	// declares attributes an object. Dropping the field entirely — through
	// omitempty on the tag — is what "none set" actually means; writing an
	// empty object would say something subtly different and cost a key on
	// every document.
	if s.SiteType == "" {
		s.SiteType = SiteWarehouse
	}
	if s.PICUserIDs == nil {
		s.PICUserIDs = []string{}
	}
}

// SiteLink says which companies may see a site, and in what capacity.
//
// A shipper's warehouse is where a TRANSPORTER loads. Both need it, and with a
// single owner the transporter creates their own copy — two records of one
// gate, two geofences that drift apart, and a truck inside one and outside the
// other.
type SiteLink struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	SiteID    string             `bson:"siteId" json:"siteId"`
	CompanyID string             `bson:"companyId" json:"companyId"`

	Relation       SiteRelation `bson:"relation" json:"relation"`
	LinkedByUserID *string      `bson:"linkedByUserId" json:"linkedByUserId,omitempty"`
	CreatedAt      time.Time    `bson:"createdAt" json:"createdAt"`
}

func (SiteLink) CollectionName() string { return config.Collection("site_links") }

// Document is a vehicle's paperwork: an STNK, a KIR, an insurance certificate.
//
// VEHICLES ONLY. It once carried an ownerType covering sites, trackers and
// people as well, and that was wrong in both directions: a person's documents —
// a SIM, a KTP — already live in the authentication service's user_documents,
// beside the account they belong to, and a site or a device rarely has paperwork
// worth tracking at all. A polymorphic owner bought nothing and split a person's
// documents across two services.
//
// Its own collection rather than an array on the vehicle, because EXPIRY is the
// point: "what expires in the next thirty days" has to be answerable across
// every vehicle at once, and an array inside each vehicle cannot be indexed for
// that across vehicles.
type Document struct {
	Base `bson:",inline"`

	CompanyID string `bson:"companyId" json:"companyId"`

	// Exactly one owner: a vehicle (STNK, KIR, insurance) or a driver (SIM,
	// KTP, medical). VehicleID was the only owner once, so it stays a plain
	// string; empty means "not a vehicle document".
	VehicleID string  `bson:"vehicleId,omitempty" json:"vehicleId,omitempty"`
	DriverID  *string `bson:"driverId,omitempty" json:"driverId,omitempty"`

	DocType string  `bson:"docType" json:"docType"`
	Number  *string `bson:"number" json:"number,omitempty"`

	IssuedOn  *time.Time `bson:"issuedOn" json:"issuedOn,omitempty"`
	ExpiresOn *time.Time `bson:"expiresOn" json:"expiresOn,omitempty"`

	// The object-store key, never a URL: a signed URL expires, so a stored one
	// rots. The key is resolved to a fresh URL when someone asks to see it.
	FileKey *string `bson:"fileKey" json:"fileKey,omitempty"`

	IsVerified       bool    `bson:"isVerified" json:"isVerified"`
	VerifiedByUserID *string `bson:"verifiedByUserId" json:"verifiedByUserId,omitempty"`
	Notes            *string `bson:"notes" json:"notes,omitempty"`
}

func (Document) CollectionName() string { return config.Collection("documents") }

func (d *Document) BeforeWrite() {
	d.DocType = strings.ToUpper(strings.TrimSpace(d.DocType))
	d.Number = normalise.Optional(strings.TrimSpace, d.Number)
}
