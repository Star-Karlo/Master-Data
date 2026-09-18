package services

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/normalise"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/repository"
)

// FleetService serves vehicles and sites.
//
// Callers still say "truck" and "warehouse". Those are the nouns the proto and
// the TMS frontend agree on, and they are narrower than what the model now
// holds: a vehicle may be a rigid truck, a tractor head or a trailer body, and
// a site may be a warehouse, a depot or a port. So the old names map onto
// FILTERED views of the new collections rather than onto them wholesale —
// returning a trailer body in a list of assignable trucks would offer the
// planner something that cannot pull a load.
type FleetService struct {
	vehicles *repository.Store[models.Vehicle, *models.Vehicle]
	sites    *repository.Store[models.Site, *models.Site]
	devices  *repository.DeviceResolver
	db       *mongo.Database
}

func NewFleetService(db *mongo.Database) *FleetService {
	return &FleetService{
		vehicles: repository.NewStore[models.Vehicle](db),
		sites:    repository.NewStore[models.Site](db),
		devices:  repository.NewDeviceResolver(db),
		db:       db,
	}
}

// Truck is a vehicle as the TMS contract describes one.
type Truck struct {
	ID            string   `json:"id"`
	CompanyID     string   `json:"companyId"`
	PoliceNumber  string   `json:"policeNumber"`
	TruckTypeID   string   `json:"truckTypeId,omitempty"`
	TruckHeadID   string   `json:"truckHeadId,omitempty"`
	TruckBodyID   string   `json:"truckBodyId,omitempty"`
	BrandID       string   `json:"brandId,omitempty"`
	Year          int      `json:"year,omitempty"`
	ChassisNumber string   `json:"chassisNumber,omitempty"`
	EngineNumber  string   `json:"engineNumber,omitempty"`
	DriverIDs     []string `json:"driverIds"`
	Status        string   `json:"status"`
	IsAvailable   bool     `json:"isAvailable"`

	// IMEI of the device fitted right now, empty when none is.
	//
	// Resolved from the tracker assignment rather than read from the vehicle's
	// cached TrackerID: nothing in MongoDB keeps that copy in step, and a stale
	// one shows a device on the wrong truck. The assignment is the record that
	// carries fitted and unfitted dates, so it is the one that can be right.
	IMEI string `json:"imei,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Warehouse is a site as the TMS contract describes one.
type Warehouse struct {
	ID        string `json:"id"`
	CompanyID string `json:"companyId"`
	// CustomerCompanyID: the customer this warehouse belongs to, when a
	// transporter registers customers' sites; empty for its own.
	CustomerCompanyID string `json:"customerCompanyId,omitempty"`
	Name              string `json:"name"`
	Address           string `json:"address,omitempty"`

	// City and District are NAMES, not ids. There is no region table in this
	// model — see the Site comment — so an id here would reference nothing.
	City     string `json:"city,omitempty"`
	District string `json:"district,omitempty"`
	Province string `json:"province,omitempty"`

	Latitude             float64 `json:"latitude"`
	Longitude            float64 `json:"longitude"`
	GeofenceRadiusMeters int     `json:"geofenceRadiusMeters"`

	PICPhone string `json:"picPhone,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// haulingUnits are the vehicle kinds that can take a load on their own.
//
// A trailer body cannot: it has no engine. Listing one as an assignable truck
// would let a planner dispatch a driver to a job in something that cannot move.
func haulingUnits() bson.M {
	return bson.M{"unitType": bson.M{"$in": []string{
		string(models.UnitRigid), string(models.UnitHead),
	}}}
}

// ListTrucks pages a company's assignable vehicles.
func (s *FleetService) ListTrucks(ctx context.Context, companyID string, p query.Params, availableOnly bool) ([]Truck, int64, error) {
	filter := bson.M{"companyId": companyID, "deleted": bson.M{"$ne": true}}
	for k, v := range haulingUnits() {
		filter[k] = v
	}
	if availableOnly {
		filter["isAvailable"] = true
	}
	if p.Search != "" {
		filter["plateNormalised"] = bson.M{"$regex": "^" + escapeRegex(normalise.Plate(p.Search))}
	}

	col := s.vehicles.Collection()

	total, err := col.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}

	cursor, err := col.Find(ctx, filter, findOptions(p))
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var vehicles []models.Vehicle
	if err := cursor.All(ctx, &vehicles); err != nil {
		return nil, 0, err
	}

	return s.withDevices(ctx, companyID, vehicles), total, nil
}

// GetTruck resolves one vehicle.
func (s *FleetService) GetTruck(ctx context.Context, companyID, id string) (*Truck, error) {
	vehicle, err := s.findVehicle(ctx, companyID, id)
	if err != nil {
		return nil, err
	}

	trucks := s.withDevices(ctx, vehicle.CompanyID, []models.Vehicle{*vehicle})
	return &trucks[0], nil
}

// TrucksByDriver answers the business service's assign-driver check.
//
// The id may be either the driver's master-data id or their login's user id:
// most drivers have no login, so the business service assigns by driver id,
// while older callers still hold the user id. Vehicles carry both.
func (s *FleetService) TrucksByDriver(ctx context.Context, companyID, driverID string) ([]Truck, error) {
	filter := bson.M{
		"deleted": bson.M{"$ne": true},
		"$or": []bson.M{
			{"currentDriverId": driverID},
			{"currentDriverUserId": driverID},
		},
	}
	if companyID != "" {
		filter["companyId"] = companyID
	}
	for k, v := range haulingUnits() {
		filter[k] = v
	}

	cursor, err := s.vehicles.Collection().Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var vehicles []models.Vehicle
	if err := cursor.All(ctx, &vehicles); err != nil {
		return nil, err
	}
	return s.withDevices(ctx, companyID, vehicles), nil
}

// withDevices attaches the currently fitted IMEI to each vehicle.
//
// One query for the whole page rather than one per vehicle: a sixty-truck fleet
// would otherwise be sixty round trips to render one list.
func (s *FleetService) withDevices(ctx context.Context, companyID string, vehicles []models.Vehicle) []Truck {
	ids := make([]string, 0, len(vehicles))
	for _, v := range vehicles {
		ids = append(ids, v.ID.Hex())
	}

	imeis, err := s.devices.IMEIsAt(ctx, companyID, ids, time.Now())
	if err != nil {
		// A list without IMEIs is still a usable list — the planner can pick a
		// truck by plate. Losing live positions is better than losing the
		// screen, so this degrades rather than fails.
		imeis = map[string]string{}
	}

	out := make([]Truck, 0, len(vehicles))
	for _, v := range vehicles {
		id := v.ID.Hex()
		t := Truck{
			ID:            id,
			CompanyID:     v.CompanyID,
			PoliceNumber:  v.LicensePlate,
			BrandID:       deref(v.BrandID),
			TruckHeadID:   deref(v.TruckHeadID),
			TruckBodyID:   deref(v.TruckBodyID),
			ChassisNumber: deref(v.ChassisNumber),
			EngineNumber:  deref(v.EngineNumber),
			Status:        v.Status,
			IsAvailable:   v.IsAvailable,
			IMEI:          imeis[id],
			DriverIDs:     []string{},
			CreatedAt:     v.CreatedAt,
			UpdatedAt:     v.UpdatedAt,
		}

		// The contract's single truckTypeId maps onto whichever of the two the
		// unit actually has: a head names a head type, a rigid names a body
		// type. Collapsing them earlier would have meant guessing which list
		// an id belonged to.
		if v.UnitType == models.UnitHead {
			t.TruckTypeID = deref(v.TruckHeadID)
		} else {
			t.TruckTypeID = deref(v.TruckBodyID)
		}

		if v.UnitYear != nil {
			t.Year = *v.UnitYear
		}
		// The master-data driver id, which is what an order is assigned to.
		// The login user id used to be here; a driver's login is optional
		// now and the business service resolves it from the driver record.
		if v.CurrentDriverID != nil && *v.CurrentDriverID != "" {
			t.DriverIDs = append(t.DriverIDs, *v.CurrentDriverID)
		}

		out = append(out, t)
	}
	return out
}

func (s *FleetService) findVehicle(ctx context.Context, companyID, id string) (*models.Vehicle, error) {
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return nil, ErrNotFound
	}

	filter := bson.M{"_id": oid, "deleted": bson.M{"$ne": true}}
	// An empty companyID means a service-to-service read that has already
	// established authority; a value scopes it. Never omitted by an HTTP
	// caller — the handler supplies the caller's own company.
	if companyID != "" {
		filter["companyId"] = companyID
	}

	var v models.Vehicle
	if err := s.vehicles.Collection().FindOne(ctx, filter).Decode(&v); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &v, nil
}

// ---------------------------------------------------------------------------
// Sites
// ---------------------------------------------------------------------------

// ListWarehouses pages a company's sites.
func (s *FleetService) ListWarehouses(ctx context.Context, companyID string, p query.Params) ([]Warehouse, int64, error) {
	filter := bson.M{"companyId": companyID, "deleted": bson.M{"$ne": true}}
	// ?customerCompanyId=<id> narrows to one customer's sites; "own" to the
	// company's own (no customer). This lived on the truck list by mistake,
	// where nothing sends it, so MyWarehouse showed every site under every
	// customer.
	for _, f := range p.Filters {
		if f.Field == "customerCompanyId" {
			if f.Value == "own" {
				filter["customerCompanyId"] = bson.M{"$in": bson.A{nil, ""}}
			} else {
				filter["customerCompanyId"] = f.Value
			}
		}
	}
	if p.Search != "" {
		filter["nameNormalised"] = bson.M{"$regex": "^" + escapeRegex(normalise.Name(p.Search))}
	}

	col := s.sites.Collection()

	total, err := col.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}

	cursor, err := col.Find(ctx, filter, findOptions(p))
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var sites []models.Site
	if err := cursor.All(ctx, &sites); err != nil {
		return nil, 0, err
	}

	out := make([]Warehouse, 0, len(sites))
	for _, site := range sites {
		out = append(out, toWarehouse(site))
	}
	return out, total, nil
}

// GetWarehouse resolves one site, including its geofence radius.
func (s *FleetService) GetWarehouse(ctx context.Context, companyID, id string) (*Warehouse, error) {
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return nil, ErrNotFound
	}

	filter := bson.M{"_id": oid, "deleted": bson.M{"$ne": true}}
	if companyID != "" {
		filter["companyId"] = companyID
	}

	var site models.Site
	if err := s.sites.Collection().FindOne(ctx, filter).Decode(&site); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	w := toWarehouse(site)
	return &w, nil
}

func toWarehouse(site models.Site) Warehouse {
	w := Warehouse{
		ID:                site.ID.Hex(),
		CompanyID:         site.CompanyID,
		Name:              site.Name,
		Address:           deref(site.Address),
		City:              deref(site.City),
		District:          deref(site.District),
		Province:          deref(site.Province),
		PICPhone:          deref(site.SitePICPhone),
		CustomerCompanyID: deref(site.CustomerCompanyID),
		CreatedAt:         site.CreatedAt,
		UpdatedAt:         site.UpdatedAt,
	}

	// GeoJSON order: coordinates are [longitude, latitude]. Transposing them
	// puts an Indonesian site in the Indian Ocean, which usually surfaces as a
	// routing failure rather than as a visibly wrong pin.
	if site.Location != nil && len(site.Location.Coordinates) == 2 {
		w.Longitude = site.Location.Coordinates[0]
		w.Latitude = site.Location.Coordinates[1]
	}
	if site.GeofenceRadiusM != nil {
		w.GeofenceRadiusMeters = *site.GeofenceRadiusM
	}

	return w
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
