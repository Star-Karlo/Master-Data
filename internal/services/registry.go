package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/cache"
	"github.com/karlo/masterdata-service/internal/platform/normalise"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/repository"
)

// RegistryService is the writable fleet register: drivers, vehicles,
// devices and their fittings, vehicle groups, documents.
//
// It exists because the read-only FleetService answered what TMS asked —
// "which trucks can I assign" — and nothing more. FMS moving its fleet
// register here means the register has to be maintained here, by both
// products, through one set of rules. Every rule that spans two records —
// a fitting closes the previous one, a driver assignment writes the auth
// alias, a deletion refuses while something is fitted — lives in this file
// and nowhere else.
type RegistryService struct {
	drivers     *repository.Store[models.Driver, *models.Driver]
	vehicles    *repository.Store[models.Vehicle, *models.Vehicle]
	trackers    *repository.Store[models.Tracker, *models.Tracker]
	assignments *mongo.Collection
	members     *mongo.Collection
	documents   *repository.Store[models.Document, *models.Document]
	models      *mongo.Collection
	db          *mongo.Database

	// notices carries change notices to the other platform services; the
	// no-op cache when there is no Redis.
	notices cache.Cache
}

// ChangeChannel is the Redis pub/sub channel every vehicle and driver write
// is announced on. FMS subscribes to project the fleet into its own tables
// the moment it changes, instead of on its next timer tick; anything else
// that mirrors master data can do the same. The payload is a hint, not the
// data: subscribers re-read what they need.
const ChangeChannel = "karlo:masterdata"

func NewRegistryService(db *mongo.Database, notices cache.Cache) *RegistryService {
	if notices == nil {
		notices = cache.NewNoop()
	}
	return &RegistryService{
		notices:     notices,
		drivers:     repository.NewStore[models.Driver](db),
		vehicles:    repository.NewStore[models.Vehicle](db),
		trackers:    repository.NewStore[models.Tracker](db),
		assignments: db.Collection(models.TrackerAssignment{}.CollectionName()),
		members:     db.Collection(models.VehicleGroupMember{}.CollectionName()),
		documents:   repository.NewStore[models.Document](db),
		models:      db.Collection(models.TrackerModel{}.CollectionName()),
		db:          db,
	}
}

// ErrConflict is a write the current state refuses: deleting a vehicle with
// a device fitted, changing a device's identity, fitting a device twice.
var ErrConflict = errors.New("conflict")

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

func oid(id string) (primitive.ObjectID, error) {
	o, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return primitive.NilObjectID, ErrNotFound
	}
	return o, nil
}

// scoped is the filter every company-owned read uses.
func scoped(companyID string, id primitive.ObjectID) bson.M {
	return bson.M{"_id": id, "companyId": companyID, "deleted": bson.M{"$ne": true}}
}

func decodeOne[T any](ctx context.Context, col *mongo.Collection, filter bson.M) (*T, error) {
	var out T
	if err := col.FindOne(ctx, filter).Decode(&out); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &out, nil
}

func page[T any](ctx context.Context, col *mongo.Collection, filter bson.M, p query.Params) ([]T, int64, error) {
	total, err := col.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	cur, err := col.Find(ctx, filter, findOptions(p))
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = cur.Close(ctx) }()
	out := []T{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// patch applies "present means set, absent means keep" from a JSON body:
// a pointer field that is nil was not sent; one pointing at "" clears.
func str(dst **string, src *string) {
	if src == nil {
		return
	}
	v := strings.TrimSpace(*src)
	if v == "" {
		*dst = nil
		return
	}
	*dst = &v
}

func dup(err error, what string) error {
	if errors.Is(err, repository.ErrDuplicate) {
		return fmt.Errorf("%w: %s", ErrConflict, what)
	}
	return err
}

// ---------------------------------------------------------------------------
// Drivers
// ---------------------------------------------------------------------------

// DriverInput is a driver as a client sends one. Pointers throughout, so an
// update can tell "not mentioned" from "cleared".
type DriverInput struct {
	FullName      *string                `json:"fullName"`
	Phone         *string                `json:"phone"`
	EmployeeNo    *string                `json:"employeeNo"`
	LicenseNo     *string                `json:"licenseNo"`
	LicenseClass  *string                `json:"licenseClass"`
	LicenseExpiry *string                `json:"licenseExpiry"` // YYYY-MM-DD; "" clears
	Status        *string                `json:"status"`
	UserID        *string                `json:"userId"`
	Notes         *string                `json:"notes"`
	Attributes    map[string]interface{} `json:"attributes"`
}

func (in DriverInput) apply(d *models.Driver) error {
	if in.FullName != nil {
		d.FullName = *in.FullName
	}
	str(&d.Phone, in.Phone)
	str(&d.EmployeeNo, in.EmployeeNo)
	str(&d.LicenseNo, in.LicenseNo)
	str(&d.LicenseClass, in.LicenseClass)
	str(&d.UserID, in.UserID)
	str(&d.Notes, in.Notes)
	if in.Status != nil {
		d.Status = strings.ToLower(strings.TrimSpace(*in.Status))
	}
	if in.LicenseExpiry != nil {
		v := strings.TrimSpace(*in.LicenseExpiry)
		if v == "" {
			d.LicenseExpiry = nil
		} else {
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				return fmt.Errorf("%w: licenseExpiry must be YYYY-MM-DD", ErrValidation)
			}
			d.LicenseExpiry = &t
		}
	}
	if in.Attributes != nil {
		if d.Attributes == nil {
			d.Attributes = map[string]interface{}{}
		}
		for k, v := range in.Attributes {
			d.Attributes[k] = v
		}
	}
	return nil
}

func (s *RegistryService) ListDrivers(ctx context.Context, companyID string, p query.Params, status string) ([]models.Driver, int64, error) {
	filter := bson.M{"companyId": companyID, "deleted": bson.M{"$ne": true}}
	if status != "" {
		filter["status"] = status
	}
	if p.Search != "" {
		filter["$or"] = []bson.M{
			{"nameNormalised": bson.M{"$regex": escapeRegex(normalise.Name(p.Search))}},
			{"phoneNormalised": bson.M{"$regex": "^" + escapeRegex(normalise.IMEI(p.Search))}},
			{"employeeNo": bson.M{"$regex": "^" + escapeRegex(p.Search), "$options": "i"}},
		}
	}
	return page[models.Driver](ctx, s.drivers.Collection(), filter, p)
}

// ListDriversForSync pages every driver of a company including retired
// ones, for a service keeping a projection. Ordered by _id so a page
// boundary is stable; updatedSince makes a refresh incremental.
func (s *RegistryService) ListDriversForSync(ctx context.Context, companyID string, pageNo, pageSize int, updatedSince *time.Time) ([]models.Driver, int64, error) {
	filter := bson.M{"companyId": companyID}
	if updatedSince != nil {
		filter["updatedAt"] = bson.M{"$gte": updatedSince.UTC()}
	}
	p := query.Params{Page: pageNo, PageSize: pageSize, Sorts: []query.Sort{{Field: "_id"}}}
	return page[models.Driver](ctx, s.drivers.Collection(), filter, p)
}

func (s *RegistryService) GetDriver(ctx context.Context, companyID, id string) (*models.Driver, error) {
	o, err := oid(id)
	if err != nil {
		return nil, err
	}
	return decodeOne[models.Driver](ctx, s.drivers.Collection(), scoped(companyID, o))
}

// GetDriverByID resolves a driver without a company clause. For the gRPC
// edge only, where the caller is another platform service holding an id from
// its own records and no tenant to assert; HTTP callers go through GetDriver
// with the actor's company.
func (s *RegistryService) GetDriverByID(ctx context.Context, id string) (*models.Driver, error) {
	o, err := oid(id)
	if err != nil {
		return nil, err
	}
	return decodeOne[models.Driver](ctx, s.drivers.Collection(), bson.M{"_id": o, "deleted": bson.M{"$ne": true}})
}

func (s *RegistryService) CreateDriver(ctx context.Context, companyID string, in DriverInput) (*models.Driver, error) {
	d := &models.Driver{CompanyID: companyID}
	if err := in.apply(d); err != nil {
		return nil, err
	}
	d.ID = primitive.NewObjectID()
	if err := s.drivers.Create(ctx, d); err != nil {
		return nil, dup(validation(err), "a driver with that phone, employee number, licence or login already exists")
	}
	s.announce(ctx, "driver", companyID, d.ID.Hex(), "create")
	return d, nil
}

func (s *RegistryService) UpdateDriver(ctx context.Context, companyID, id string, in DriverInput) (*models.Driver, error) {
	d, err := s.GetDriver(ctx, companyID, id)
	if err != nil {
		return nil, err
	}
	if err := in.apply(d); err != nil {
		return nil, err
	}
	if err := s.drivers.Update(ctx, d.ID, d); err != nil {
		return nil, dup(validation(err), "a driver with that phone, employee number, licence or login already exists")
	}
	s.announce(ctx, "driver", companyID, id, "update")
	return d, nil
}

// DeleteDriver retires a driver. A vehicle currently assigned to them is
// released, so no truck keeps naming a driver that no longer exists.
func (s *RegistryService) DeleteDriver(ctx context.Context, companyID, id string) error {
	d, err := s.GetDriver(ctx, companyID, id)
	if err != nil {
		return err
	}
	if err := s.drivers.SoftDelete(ctx, companyID, d.ID); err != nil {
		return err
	}
	_, err = s.vehicles.Collection().UpdateMany(ctx,
		bson.M{"companyId": companyID, "currentDriverId": id},
		bson.M{"$set": bson.M{"currentDriverId": nil, "currentDriverUserId": nil, "updatedAt": time.Now().UTC()}})
	s.announce(ctx, "driver", companyID, id, "delete")
	return err
}

// announce publishes a change notice for one kind of record of one company.
func (s *RegistryService) announce(ctx context.Context, kind, companyID, id, op string) {
	payload, err := json.Marshal(map[string]string{"kind": kind, "company_id": companyID, "id": id, "op": op})
	if err != nil {
		return
	}
	s.notices.Publish(ctx, ChangeChannel, payload)
}

// validation maps the model's own errors onto ErrValidation so the handler
// answers 400 rather than 500.
func validation(err error) error {
	switch {
	case errors.Is(err, models.ErrDriverNameRequired),
		errors.Is(err, models.ErrDriverStatus),
		errors.Is(err, models.ErrPlateRequired):
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return err
}

// ---------------------------------------------------------------------------
// Vehicles
// ---------------------------------------------------------------------------

type VehicleInput struct {
	LicensePlate    *string                `json:"licensePlate"`
	ChassisNumber   *string                `json:"chassisNumber"`
	EngineNumber    *string                `json:"engineNumber"`
	UnitType        *string                `json:"unitType"`
	TruckHeadID     *string                `json:"truckHeadId"`
	TruckBodyID     *string                `json:"truckBodyId"`
	BrandID         *string                `json:"brandId"`
	TruckGroupID    *string                `json:"truckGroupId"`
	UnitYear        *int                   `json:"unitYear"`
	Color           *string                `json:"color"`
	Status          *string                `json:"status"`
	IsAvailable     *bool                  `json:"isAvailable"`
	OdometerKm      *float64               `json:"odometerKm"`
	HourmeterHours  *float64               `json:"hourmeterHours"`
	FuelTankLiters  *float64               `json:"fuelTankLiters"`
	FuelRatioKmpl   *float64               `json:"fuelRatioKmpl"`
	CurrentDriverID *string                `json:"currentDriverId"`
	Notes           *string                `json:"notes"`
	Attributes      map[string]interface{} `json:"attributes"`
}

func (in VehicleInput) apply(v *models.Vehicle) error {
	if in.LicensePlate != nil {
		v.LicensePlate = strings.TrimSpace(*in.LicensePlate)
	}
	str(&v.ChassisNumber, in.ChassisNumber)
	str(&v.EngineNumber, in.EngineNumber)
	str(&v.TruckHeadID, in.TruckHeadID)
	str(&v.TruckBodyID, in.TruckBodyID)
	str(&v.BrandID, in.BrandID)
	str(&v.TruckGroupID, in.TruckGroupID)
	str(&v.Color, in.Color)
	str(&v.Notes, in.Notes)
	if in.UnitType != nil {
		switch ut := models.UnitType(strings.ToLower(strings.TrimSpace(*in.UnitType))); ut {
		case models.UnitRigid, models.UnitHead, models.UnitBody:
			v.UnitType = ut
		default:
			return fmt.Errorf("%w: unitType must be rigid, head or body", ErrValidation)
		}
	}
	if in.UnitYear != nil {
		if *in.UnitYear == 0 {
			v.UnitYear = nil
		} else {
			y := *in.UnitYear
			v.UnitYear = &y
		}
	}
	if in.Status != nil {
		v.Status = strings.ToLower(strings.TrimSpace(*in.Status))
	}
	if in.IsAvailable != nil {
		v.IsAvailable = *in.IsAvailable
	}
	num := func(dst **float64, src *float64) {
		if src != nil {
			x := *src
			*dst = &x
		}
	}
	num(&v.OdometerKm, in.OdometerKm)
	num(&v.HourmeterHours, in.HourmeterHours)
	num(&v.FuelTankLiters, in.FuelTankLiters)
	num(&v.FuelRatioKmpl, in.FuelRatioKmpl)
	if in.Attributes != nil {
		if v.Attributes == nil {
			v.Attributes = map[string]interface{}{}
		}
		for k, val := range in.Attributes {
			v.Attributes[k] = val
		}
	}
	return nil
}

// VehicleView is a vehicle with its current device and driver resolved, so
// a list screen needs no second round of requests.
type VehicleView struct {
	models.Vehicle
	Tracker *TrackerRef `json:"tracker,omitempty"`
	Driver  *DriverRef  `json:"driver,omitempty"`
}

type TrackerRef struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	DeviceID string `json:"deviceId"`
}

type DriverRef struct {
	ID       string  `json:"id"`
	FullName string  `json:"fullName"`
	Phone    *string `json:"phone,omitempty"`
}

func (s *RegistryService) ListVehicles(ctx context.Context, companyID string, p query.Params, status string, available *bool, groupID string) ([]VehicleView, int64, error) {
	filter := bson.M{"companyId": companyID, "deleted": bson.M{"$ne": true}}
	if status != "" {
		filter["status"] = status
	}
	if available != nil {
		filter["isAvailable"] = *available
	}
	if groupID != "" {
		filter["truckGroupId"] = groupID
	}
	if p.Search != "" {
		filter["$or"] = []bson.M{
			{"plateNormalised": bson.M{"$regex": "^" + escapeRegex(normalise.Plate(p.Search))}},
			{"chassisNormalised": bson.M{"$regex": "^" + escapeRegex(normalise.Serial(p.Search))}},
		}
	}
	vehicles, total, err := page[models.Vehicle](ctx, s.vehicles.Collection(), filter, p)
	if err != nil {
		return nil, 0, err
	}
	return s.resolveVehicles(ctx, vehicles), total, nil
}

func (s *RegistryService) GetVehicle(ctx context.Context, companyID, id string) (*VehicleView, error) {
	v, err := s.findVehicle(ctx, companyID, id)
	if err != nil {
		return nil, err
	}
	views := s.resolveVehicles(ctx, []models.Vehicle{*v})
	return &views[0], nil
}

func (s *RegistryService) findVehicle(ctx context.Context, companyID, id string) (*models.Vehicle, error) {
	o, err := oid(id)
	if err != nil {
		return nil, err
	}
	return decodeOne[models.Vehicle](ctx, s.vehicles.Collection(), scoped(companyID, o))
}

func (s *RegistryService) resolveVehicles(ctx context.Context, vehicles []models.Vehicle) []VehicleView {
	trackerIDs := []primitive.ObjectID{}
	driverIDs := []primitive.ObjectID{}
	for _, v := range vehicles {
		if v.TrackerID != nil {
			if o, err := primitive.ObjectIDFromHex(*v.TrackerID); err == nil {
				trackerIDs = append(trackerIDs, o)
			}
		}
		if v.CurrentDriverID != nil {
			if o, err := primitive.ObjectIDFromHex(*v.CurrentDriverID); err == nil {
				driverIDs = append(driverIDs, o)
			}
		}
	}
	trackers := map[string]TrackerRef{}
	if len(trackerIDs) > 0 {
		cur, err := s.trackers.Collection().Find(ctx, bson.M{"_id": bson.M{"$in": trackerIDs}})
		if err == nil {
			var rows []models.Tracker
			_ = cur.All(ctx, &rows)
			for _, t := range rows {
				trackers[t.ID.Hex()] = TrackerRef{ID: t.ID.Hex(), Kind: string(t.Kind), DeviceID: t.DeviceID}
			}
		}
	}
	drivers := map[string]DriverRef{}
	if len(driverIDs) > 0 {
		cur, err := s.drivers.Collection().Find(ctx, bson.M{"_id": bson.M{"$in": driverIDs}})
		if err == nil {
			var rows []models.Driver
			_ = cur.All(ctx, &rows)
			for _, d := range rows {
				drivers[d.ID.Hex()] = DriverRef{ID: d.ID.Hex(), FullName: d.FullName, Phone: d.Phone}
			}
		}
	}
	out := make([]VehicleView, 0, len(vehicles))
	for _, v := range vehicles {
		view := VehicleView{Vehicle: v}
		if v.TrackerID != nil {
			if t, ok := trackers[*v.TrackerID]; ok {
				view.Tracker = &t
			}
		}
		if v.CurrentDriverID != nil {
			if d, ok := drivers[*v.CurrentDriverID]; ok {
				view.Driver = &d
			}
		}
		out = append(out, view)
	}
	return out
}

func (s *RegistryService) CreateVehicle(ctx context.Context, companyID string, in VehicleInput) (*VehicleView, error) {
	v := &models.Vehicle{CompanyID: companyID, IsAvailable: true}
	if err := in.apply(v); err != nil {
		return nil, err
	}
	if err := s.checkGroup(ctx, companyID, v.TruckGroupID); err != nil {
		return nil, err
	}
	if err := s.setDriver(ctx, companyID, v, in.CurrentDriverID); err != nil {
		return nil, err
	}
	v.ID = primitive.NewObjectID()
	if err := s.vehicles.Create(ctx, v); err != nil {
		return nil, dup(validation(err), "a vehicle with that plate or chassis number already exists")
	}
	s.announce(ctx, "vehicle", companyID, v.ID.Hex(), "create")
	return s.GetVehicle(ctx, companyID, v.ID.Hex())
}

func (s *RegistryService) UpdateVehicle(ctx context.Context, companyID, id string, in VehicleInput) (*VehicleView, error) {
	v, err := s.findVehicle(ctx, companyID, id)
	if err != nil {
		return nil, err
	}
	if err := in.apply(v); err != nil {
		return nil, err
	}
	if err := s.checkGroup(ctx, companyID, v.TruckGroupID); err != nil {
		return nil, err
	}
	if err := s.setDriver(ctx, companyID, v, in.CurrentDriverID); err != nil {
		return nil, err
	}
	if err := s.vehicles.Update(ctx, v.ID, v); err != nil {
		return nil, dup(validation(err), "a vehicle with that plate or chassis number already exists")
	}
	s.announce(ctx, "vehicle", companyID, id, "update")
	return s.GetVehicle(ctx, companyID, id)
}

// setDriver is the one place a vehicle's driver changes. It writes both
// halves — the driver document id and, when that driver has a login, the
// auth user id business-service reads — so the two cannot disagree.
func (s *RegistryService) setDriver(ctx context.Context, companyID string, v *models.Vehicle, driverID *string) error {
	if driverID == nil {
		return nil
	}
	if strings.TrimSpace(*driverID) == "" {
		v.CurrentDriverID = nil
		v.CurrentDriverUserID = nil
		return nil
	}
	d, err := s.GetDriver(ctx, companyID, *driverID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: no such driver", ErrValidation)
		}
		return err
	}
	if d.Status != "active" {
		return fmt.Errorf("%w: that driver is inactive", ErrValidation)
	}
	hex := d.ID.Hex()
	if v.CurrentDriverID != nil && *v.CurrentDriverID == hex {
		return nil
	}
	// One truck per driver. Pairing them here unpairs them anywhere else in
	// the company, and that other truck is announced so FMS follows; two
	// trucks both claiming the same driver is exactly the inconsistency the
	// register exists to prevent.
	others, err := s.unpairDriverElsewhere(ctx, companyID, hex, v.ID)
	if err != nil {
		return err
	}
	for _, id := range others {
		s.announce(ctx, "vehicle", companyID, id, "update")
	}
	v.CurrentDriverID = &hex
	v.CurrentDriverUserID = d.UserID
	return nil
}

// unpairDriverElsewhere clears a driver from every vehicle of the company
// except the one being paired, returning the ids it changed.
func (s *RegistryService) unpairDriverElsewhere(ctx context.Context, companyID, driverID string, except primitive.ObjectID) ([]string, error) {
	filter := bson.M{"companyId": companyID, "currentDriverId": driverID, "_id": bson.M{"$ne": except}}
	cur, err := s.vehicles.Collection().Find(ctx, filter, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	if err := cur.All(ctx, &rows); err != nil || len(rows) == 0 {
		return nil, err
	}
	if _, err := s.vehicles.Collection().UpdateMany(ctx, filter,
		bson.M{"$set": bson.M{"currentDriverId": nil, "currentDriverUserId": nil, "updatedAt": time.Now()}}); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID.Hex())
	}
	return out, nil
}

// DeleteVehicle retires a vehicle. Refused while a device is fitted: the
// fitting has to be closed first, or the device disappears from every
// screen while still reporting positions for a vehicle that "does not exist".
func (s *RegistryService) DeleteVehicle(ctx context.Context, companyID, id string) error {
	v, err := s.findVehicle(ctx, companyID, id)
	if err != nil {
		return err
	}
	open, err := s.assignments.CountDocuments(ctx, bson.M{"vehicleId": id, "unfittedAt": nil})
	if err != nil {
		return err
	}
	if open > 0 || v.TrackerID != nil {
		return fmt.Errorf("%w: a device is still fitted to this vehicle; unfit it first", ErrConflict)
	}
	if err := s.vehicles.SoftDelete(ctx, companyID, v.ID); err != nil {
		return err
	}
	_, err = s.members.DeleteMany(ctx, bson.M{"companyId": companyID, "vehicleId": id})
	s.announce(ctx, "vehicle", companyID, id, "delete")
	return err
}

// ---------------------------------------------------------------------------
// Trackers and fittings
// ---------------------------------------------------------------------------

type TrackerInput struct {
	Kind        *string `json:"kind"`
	DeviceID    *string `json:"deviceId"`
	IMEI        *string `json:"imei"` // accepted as an alias of deviceId for gps
	ICCID       *string `json:"iccid"`
	SIMProvider *string `json:"simProvider"`
	PhoneNo     *string `json:"phoneNo"`
	ModelID     *string `json:"modelId"`
	// CompanyID moves a device between Karlo's stock and a company (or
	// between companies). Platform staff only; ignored for everyone else.
	// This is how the backfilled stock is handed to the companies that
	// actually run the devices.
	CompanyID  *string                `json:"companyId"`
	Owner      *string                `json:"owner"`
	OwnerName  *string                `json:"ownerName"`
	Status     *string                `json:"status"`
	Attributes map[string]interface{} `json:"attributes"`
}

// TrackerView is a device with where it currently is.
type TrackerView struct {
	models.Tracker
	CurrentVehicleID *string    `json:"currentVehicleId,omitempty"`
	FittedAt         *time.Time `json:"fittedAt,omitempty"`
}

func (s *RegistryService) ListTrackers(ctx context.Context, companyID string, p query.Params, kind string, fitted *bool, vehicleID string) ([]TrackerView, int64, error) {
	// Karlo's stock (companyId nil) is visible to staff acting for nobody;
	// a company sees its own devices.
	filter := bson.M{"deleted": bson.M{"$ne": true}}
	if companyID != "" {
		filter["companyId"] = companyID
	}
	if kind != "" {
		filter["kind"] = kind
	}
	if p.Search != "" {
		filter["deviceId"] = bson.M{"$regex": "^" + escapeRegex(strings.TrimSpace(p.Search)), "$options": "i"}
	}
	if fitted != nil || vehicleID != "" {
		q := bson.M{"unfittedAt": nil}
		if vehicleID != "" {
			q["vehicleId"] = vehicleID
		}
		ids, err := s.assignments.Distinct(ctx, "trackerId", q)
		if err != nil {
			return nil, 0, err
		}
		oids := make([]primitive.ObjectID, 0, len(ids))
		for _, id := range ids {
			if h, ok := id.(string); ok {
				if o, err := primitive.ObjectIDFromHex(h); err == nil {
					oids = append(oids, o)
				}
			}
		}
		if vehicleID != "" || (fitted != nil && *fitted) {
			filter["_id"] = bson.M{"$in": oids}
		} else {
			filter["_id"] = bson.M{"$nin": oids}
		}
	}
	rows, total, err := page[models.Tracker](ctx, s.trackers.Collection(), filter, p)
	if err != nil {
		return nil, 0, err
	}
	return s.resolveTrackers(ctx, rows), total, nil
}

func (s *RegistryService) resolveTrackers(ctx context.Context, rows []models.Tracker) []TrackerView {
	ids := make([]string, 0, len(rows))
	for _, t := range rows {
		ids = append(ids, t.ID.Hex())
	}
	current := map[string]models.TrackerAssignment{}
	if len(ids) > 0 {
		cur, err := s.assignments.Find(ctx, bson.M{"trackerId": bson.M{"$in": ids}, "unfittedAt": nil})
		if err == nil {
			var as []models.TrackerAssignment
			_ = cur.All(ctx, &as)
			for _, a := range as {
				if a.TrackerID != nil {
					current[*a.TrackerID] = a
				}
			}
		}
	}
	out := make([]TrackerView, 0, len(rows))
	for _, t := range rows {
		view := TrackerView{Tracker: t}
		if a, ok := current[t.ID.Hex()]; ok {
			vid := a.VehicleID
			fa := a.FittedAt
			view.CurrentVehicleID = &vid
			view.FittedAt = &fa
		}
		out = append(out, view)
	}
	return out
}

func (s *RegistryService) findTracker(ctx context.Context, companyID, id string) (*models.Tracker, error) {
	o, err := oid(id)
	if err != nil {
		return nil, err
	}
	filter := bson.M{"_id": o, "deleted": bson.M{"$ne": true}}
	if companyID != "" {
		filter["companyId"] = companyID
	}
	return decodeOne[models.Tracker](ctx, s.trackers.Collection(), filter)
}

func (s *RegistryService) GetTracker(ctx context.Context, companyID, id string) (*TrackerView, error) {
	t, err := s.findTracker(ctx, companyID, id)
	if err != nil {
		return nil, err
	}
	views := s.resolveTrackers(ctx, []models.Tracker{*t})
	return &views[0], nil
}

func (in TrackerInput) apply(t *models.Tracker, creating bool) error {
	if in.Kind != nil {
		switch k := models.TrackerKind(strings.ToLower(strings.TrimSpace(*in.Kind))); k {
		case models.TrackerGPS, models.TrackerDashcam:
			if !creating && t.Kind != "" && t.Kind != k {
				return fmt.Errorf("%w: a device's kind cannot change", ErrConflict)
			}
			t.Kind = k
		default:
			return fmt.Errorf("%w: kind must be gps or dashcam", ErrValidation)
		}
	}
	device := in.DeviceID
	if device == nil && in.IMEI != nil {
		device = in.IMEI
	}
	if device != nil {
		v := strings.TrimSpace(*device)
		if !creating && t.DeviceID != "" && normaliseDevice(t.Kind, v) != t.DeviceID {
			return fmt.Errorf("%w: a device's id cannot change; retire it and register the new one", ErrConflict)
		}
		t.DeviceID = v
	}
	str(&t.ICCID, in.ICCID)
	str(&t.SIMProvider, in.SIMProvider)
	str(&t.PhoneNo, in.PhoneNo)
	str(&t.ModelID, in.ModelID)
	str(&t.OwnerName, in.OwnerName)
	if in.Owner != nil {
		switch o := models.TrackerOwner(strings.ToLower(strings.TrimSpace(*in.Owner))); o {
		case models.OwnerKarlo, models.OwnerCustomer, models.OwnerVendor:
			t.Owner = o
		default:
			return fmt.Errorf("%w: owner must be karlo, customer or vendor", ErrValidation)
		}
	}
	if in.Status != nil {
		t.Status = strings.ToLower(strings.TrimSpace(*in.Status))
	}
	if in.Attributes != nil {
		if t.Attributes == nil {
			t.Attributes = map[string]interface{}{}
		}
		for k, v := range in.Attributes {
			t.Attributes[k] = v
		}
	}
	return nil
}

func normaliseDevice(kind models.TrackerKind, v string) string {
	if kind == models.TrackerGPS || kind == "" {
		return normalise.IMEI(v)
	}
	return strings.TrimSpace(v)
}

func (s *RegistryService) CreateTracker(ctx context.Context, companyID string, staff bool, in TrackerInput) (*TrackerView, error) {
	t := &models.Tracker{}
	if companyID != "" {
		c := companyID
		t.CompanyID = &c
	}
	// A company registering its own device owns it; Karlo stock is Karlo's.
	if staff {
		t.Owner = models.OwnerKarlo
	} else {
		t.Owner = models.OwnerCustomer
	}
	if err := in.apply(t, true); err != nil {
		return nil, err
	}
	if t.Kind == "" {
		t.Kind = models.TrackerGPS
	}
	if normaliseDevice(t.Kind, t.DeviceID) == "" {
		return nil, fmt.Errorf("%w: deviceId is required", ErrValidation)
	}
	if t.Kind == models.TrackerGPS && len(normalise.IMEI(t.DeviceID)) != 15 {
		return nil, fmt.Errorf("%w: a GPS tracker's IMEI is 15 digits", ErrValidation)
	}
	if err := s.checkModel(ctx, t); err != nil {
		return nil, err
	}
	t.ID = primitive.NewObjectID()
	if err := s.trackers.Create(ctx, t); err != nil {
		return nil, dup(err, "a device with that id is already registered")
	}
	s.announce(ctx, "tracker", companyID, t.ID.Hex(), "create")
	return s.GetTracker(ctx, companyID, t.ID.Hex())
}

func (s *RegistryService) UpdateTracker(ctx context.Context, companyID, id string, in TrackerInput) (*TrackerView, error) {
	return s.updateTracker(ctx, companyID, id, in, false)
}

// UpdateTrackerAsStaff is UpdateTracker for platform staff: the device is
// found regardless of company, and in.CompanyID may re-own it.
func (s *RegistryService) UpdateTrackerAsStaff(ctx context.Context, id string, in TrackerInput) (*TrackerView, error) {
	return s.updateTracker(ctx, "", id, in, true)
}

func (s *RegistryService) updateTracker(ctx context.Context, companyID, id string, in TrackerInput, staff bool) (*TrackerView, error) {
	t, err := s.findTracker(ctx, companyID, id)
	if err != nil {
		return nil, err
	}
	if err := in.apply(t, false); err != nil {
		return nil, err
	}
	if staff && in.CompanyID != nil {
		if c := strings.TrimSpace(*in.CompanyID); c == "" {
			t.CompanyID = nil
		} else {
			t.CompanyID = &c
		}
		if t.CompanyID != nil {
			companyID = *t.CompanyID
		}
	}
	if err := s.checkModel(ctx, t); err != nil {
		return nil, err
	}
	if err := s.trackers.Update(ctx, t.ID, t); err != nil {
		return nil, dup(err, "a device with that id is already registered")
	}
	s.announce(ctx, "tracker", companyID, id, "update")
	if staff {
		return s.GetTracker(ctx, "", id)
	}
	return s.GetTracker(ctx, companyID, id)
}

// checkModel refuses a model of the wrong kind: a dashcam registered as a
// Teltonika FMB is a mistake the form should not be able to make.
func (s *RegistryService) checkModel(ctx context.Context, t *models.Tracker) error {
	if t.ModelID == nil {
		return nil
	}
	o, err := primitive.ObjectIDFromHex(*t.ModelID)
	if err != nil {
		return fmt.Errorf("%w: no such tracker model", ErrValidation)
	}
	var m models.TrackerModel
	if err := s.models.FindOne(ctx, bson.M{"_id": o, "deleted": bson.M{"$ne": true}}).Decode(&m); err != nil {
		return fmt.Errorf("%w: no such tracker model", ErrValidation)
	}
	if m.Kind != "" && m.Kind != t.Kind {
		return fmt.Errorf("%w: model %s %s is a %s model, not %s", ErrValidation, m.Vendor, m.Model, m.Kind, t.Kind)
	}
	return nil
}

func (s *RegistryService) DeleteTracker(ctx context.Context, companyID, id string) error {
	t, err := s.findTracker(ctx, companyID, id)
	if err != nil {
		return err
	}
	open, err := s.assignments.CountDocuments(ctx, bson.M{"trackerId": id, "unfittedAt": nil})
	if err != nil {
		return err
	}
	if open > 0 {
		return fmt.Errorf("%w: this device is fitted to a vehicle; unfit it first", ErrConflict)
	}
	if err := s.trackers.SoftDelete(ctx, "", t.ID); err != nil {
		return err
	}
	s.announce(ctx, "tracker", companyID, id, "delete")
	return nil
}

type FitInput struct {
	VehicleID         string     `json:"vehicleId"`
	FittedAt          *time.Time `json:"fittedAt"`
	InstallPhotoKey   *string    `json:"installPhotoKey"`
	InstalledByUserID *string    `json:"installedByUserId"`
	InstallNotes      *string    `json:"installNotes"`
}

// Fit puts a device on a vehicle. One operation, three records: the open
// assignment on this device closes, the open assignment of the same KIND on
// that vehicle closes (a vehicle carries one GPS and one dashcam, not two of
// either), the new assignment opens, and for GPS the vehicle's trackerId is
// set. Done as a transaction where the deployment supports one, and in a
// fixed order otherwise so a failure part-way leaves a state the next Fit
// repairs rather than one that lies.
func (s *RegistryService) Fit(ctx context.Context, companyID, trackerID string, in FitInput) (*models.TrackerAssignment, error) {
	t, err := s.findTracker(ctx, companyID, trackerID)
	if err != nil {
		return nil, err
	}
	if t.Status != "active" && t.Status != "" {
		return nil, fmt.Errorf("%w: device is %s", ErrValidation, t.Status)
	}
	v, err := s.findVehicle(ctx, companyID, in.VehicleID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: no such vehicle", ErrValidation)
		}
		return nil, err
	}
	at := time.Now().UTC()
	if in.FittedAt != nil {
		at = in.FittedAt.UTC()
	}
	if at.After(time.Now().Add(time.Minute)) {
		return nil, fmt.Errorf("%w: fittedAt is in the future", ErrValidation)
	}

	// Which devices of this kind are on that vehicle now.
	sameKind, err := s.trackers.Collection().Distinct(ctx, "_id",
		bson.M{"kind": t.Kind, "deleted": bson.M{"$ne": true}})
	if err != nil {
		return nil, err
	}
	kindIDs := make([]string, 0, len(sameKind))
	for _, id := range sameKind {
		if o, ok := id.(primitive.ObjectID); ok {
			kindIDs = append(kindIDs, o.Hex())
		}
	}

	assignment := &models.TrackerAssignment{
		CompanyID:         companyID,
		VehicleID:         v.ID.Hex(),
		Kind:              t.Kind,
		DeviceID:          t.DeviceID,
		IMEI:              t.IMEI,
		FittedAt:          at,
		InstallPhotoKey:   in.InstallPhotoKey,
		InstalledByUserID: in.InstalledByUserID,
		InstallNotes:      in.InstallNotes,
		CreatedAt:         time.Now().UTC(),
	}
	tid := t.ID.Hex()
	assignment.TrackerID = &tid
	assignment.BeforeWrite()

	run := func(ctx context.Context) error {
		// 1. This device leaves wherever it was.
		if _, err := s.assignments.UpdateMany(ctx,
			bson.M{"trackerId": tid, "unfittedAt": nil},
			bson.M{"$set": bson.M{"unfittedAt": at}}); err != nil {
			return err
		}
		// 2. The vehicle's current device of this kind comes off.
		if _, err := s.assignments.UpdateMany(ctx,
			bson.M{"vehicleId": v.ID.Hex(), "unfittedAt": nil, "trackerId": bson.M{"$in": kindIDs}},
			bson.M{"$set": bson.M{"unfittedAt": at}}); err != nil {
			return err
		}
		// 3. The new fitting.
		res, err := s.assignments.InsertOne(ctx, assignment)
		if err != nil {
			return err
		}
		assignment.ID = res.InsertedID.(primitive.ObjectID)
		// 4. The vehicle's shortcut, and the previous vehicle's.
		if t.Kind == models.TrackerGPS {
			if _, err := s.vehicles.Collection().UpdateMany(ctx,
				bson.M{"trackerId": tid, "_id": bson.M{"$ne": v.ID}},
				bson.M{"$set": bson.M{"trackerId": nil, "updatedAt": at}}); err != nil {
				return err
			}
			if _, err := s.vehicles.Collection().UpdateByID(ctx, v.ID,
				bson.M{"$set": bson.M{"trackerId": tid, "updatedAt": at}}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.transact(ctx, run); err != nil {
		return nil, err
	}
	// A fitting changes both the device and the vehicle it now reports for.
	s.announce(ctx, "tracker", companyID, trackerID, "fit")
	s.announce(ctx, "vehicle", companyID, in.VehicleID, "update")
	return assignment, nil
}

type UnfitInput struct {
	UnfittedAt *time.Time `json:"unfittedAt"`
}

func (s *RegistryService) Unfit(ctx context.Context, companyID, trackerID string, in UnfitInput) error {
	t, err := s.findTracker(ctx, companyID, trackerID)
	if err != nil {
		return err
	}
	at := time.Now().UTC()
	if in.UnfittedAt != nil {
		at = in.UnfittedAt.UTC()
	}
	tid := t.ID.Hex()
	// Which vehicle it comes off, for the change notice.
	vehicleID := ""
	var open models.TrackerAssignment
	if err := s.assignments.FindOne(ctx, bson.M{"trackerId": tid, "unfittedAt": nil}).Decode(&open); err == nil {
		vehicleID = open.VehicleID
	}
	err = s.transact(ctx, func(ctx context.Context) error {
		res, err := s.assignments.UpdateMany(ctx,
			bson.M{"trackerId": tid, "unfittedAt": nil},
			bson.M{"$set": bson.M{"unfittedAt": at}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return fmt.Errorf("%w: this device is not fitted to anything", ErrConflict)
		}
		if t.Kind == models.TrackerGPS {
			_, err = s.vehicles.Collection().UpdateMany(ctx,
				bson.M{"trackerId": tid},
				bson.M{"$set": bson.M{"trackerId": nil, "updatedAt": at}})
		}
		return err
	})
	if err != nil {
		return err
	}
	s.announce(ctx, "tracker", companyID, trackerID, "unfit")
	if vehicleID != "" {
		s.announce(ctx, "vehicle", companyID, vehicleID, "update")
	}
	return nil
}

func (s *RegistryService) Assignments(ctx context.Context, companyID, trackerID string) ([]models.TrackerAssignment, error) {
	t, err := s.findTracker(ctx, companyID, trackerID)
	if err != nil {
		return nil, err
	}
	cur, err := s.assignments.Find(ctx, bson.M{"trackerId": t.ID.Hex()},
		findOptions(query.Params{PageSize: 500, Sorts: []query.Sort{{Field: "fittedAt", Desc: true}}}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()
	out := []models.TrackerAssignment{}
	return out, cur.All(ctx, &out)
}

// transact runs fn in a session transaction when the server is a replica
// set, and plainly otherwise. A standalone MongoDB (the local dev default)
// refuses transactions outright, and a fitting must still work there.
func (s *RegistryService) transact(ctx context.Context, fn func(context.Context) error) error {
	session, err := s.db.Client().StartSession()
	if err != nil {
		return fn(ctx)
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc mongo.SessionContext) (interface{}, error) {
		return nil, fn(sc)
	})
	if err != nil && isNoTransactions(err) {
		slog.Debug("masterdata: transactions unsupported, running unwrapped")
		return fn(ctx)
	}
	if err == nil {
		slog.Debug("masterdata: fitting committed in a transaction")
	}
	return err
}

func isNoTransactions(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "Transaction numbers are only allowed") ||
		strings.Contains(msg, "replica set") ||
		strings.Contains(msg, "IllegalOperation")
}

// ---------------------------------------------------------------------------
// Vehicle groups
// ---------------------------------------------------------------------------

// checkGroup refuses a truckGroupId that is not one of the company's own
// vehicle groups, so a vehicle can never point at a group that is not there.
func (s *RegistryService) checkGroup(ctx context.Context, companyID string, groupID *string) error {
	if groupID == nil || *groupID == "" {
		return nil
	}
	oid, err := primitive.ObjectIDFromHex(*groupID)
	if err != nil {
		return fmt.Errorf("%w: truckGroupId is not a valid id", ErrValidation)
	}
	n, err := s.db.Collection(models.VehicleGroup{}.CollectionName()).CountDocuments(ctx,
		bson.M{"_id": oid, "companyId": companyID, "deleted": bson.M{"$ne": true}})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: no such vehicle group", ErrValidation)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Documents
// ---------------------------------------------------------------------------

type DocumentInput struct {
	VehicleID *string `json:"vehicleId"`
	DriverID  *string `json:"driverId"`
	DocType   *string `json:"docType"`
	Number    *string `json:"number"`
	IssuedOn  *string `json:"issuedOn"`  // YYYY-MM-DD
	ExpiresOn *string `json:"expiresOn"` // YYYY-MM-DD
	FileKey   *string `json:"fileKey"`
	Notes     *string `json:"notes"`
}

func date(dst **time.Time, src *string) error {
	if src == nil {
		return nil
	}
	v := strings.TrimSpace(*src)
	if v == "" {
		*dst = nil
		return nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return fmt.Errorf("%w: dates are YYYY-MM-DD", ErrValidation)
	}
	*dst = &t
	return nil
}

func (s *RegistryService) ListDocuments(ctx context.Context, companyID string, p query.Params, vehicleID, driverID string, expiringWithinDays int) ([]models.Document, int64, error) {
	filter := bson.M{"companyId": companyID, "deleted": bson.M{"$ne": true}}
	switch {
	case vehicleID != "":
		filter["vehicleId"] = vehicleID
	case driverID != "":
		filter["driverId"] = driverID
	case expiringWithinDays > 0:
		// A company-wide expiry sweep needs no owner.
	default:
		return nil, 0, fmt.Errorf("%w: vehicleId or driverId is required", ErrValidation)
	}
	if expiringWithinDays > 0 {
		filter["expiresOn"] = bson.M{"$lte": time.Now().UTC().AddDate(0, 0, expiringWithinDays)}
	}
	return page[models.Document](ctx, s.documents.Collection(), filter, p)
}

func (s *RegistryService) findDocument(ctx context.Context, companyID, id string) (*models.Document, error) {
	o, err := oid(id)
	if err != nil {
		return nil, err
	}
	return decodeOne[models.Document](ctx, s.documents.Collection(), scoped(companyID, o))
}

func (s *RegistryService) CreateDocument(ctx context.Context, companyID string, in DocumentInput) (*models.Document, error) {
	d := &models.Document{CompanyID: companyID}
	vehicle := in.VehicleID != nil && strings.TrimSpace(*in.VehicleID) != ""
	driver := in.DriverID != nil && strings.TrimSpace(*in.DriverID) != ""
	if vehicle == driver {
		return nil, fmt.Errorf("%w: a document belongs to exactly one of a vehicle or a driver", ErrValidation)
	}
	if vehicle {
		if _, err := s.findVehicle(ctx, companyID, *in.VehicleID); err != nil {
			return nil, fmt.Errorf("%w: no such vehicle", ErrValidation)
		}
		d.VehicleID = strings.TrimSpace(*in.VehicleID)
	} else {
		if _, err := s.GetDriver(ctx, companyID, *in.DriverID); err != nil {
			return nil, fmt.Errorf("%w: no such driver", ErrValidation)
		}
		id := strings.TrimSpace(*in.DriverID)
		d.DriverID = &id
	}
	if err := applyDocument(d, in); err != nil {
		return nil, err
	}
	if d.DocType == "" {
		return nil, fmt.Errorf("%w: docType is required", ErrValidation)
	}
	d.ID = primitive.NewObjectID()
	if err := s.documents.Create(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

func applyDocument(d *models.Document, in DocumentInput) error {
	if in.DocType != nil {
		d.DocType = strings.ToUpper(strings.TrimSpace(*in.DocType))
	}
	str(&d.Number, in.Number)
	str(&d.FileKey, in.FileKey)
	str(&d.Notes, in.Notes)
	if err := date(&d.IssuedOn, in.IssuedOn); err != nil {
		return err
	}
	return date(&d.ExpiresOn, in.ExpiresOn)
}

func (s *RegistryService) UpdateDocument(ctx context.Context, companyID, id string, in DocumentInput) (*models.Document, error) {
	d, err := s.findDocument(ctx, companyID, id)
	if err != nil {
		return nil, err
	}
	// The owner is fixed; a document does not move between a truck and a person.
	if err := applyDocument(d, in); err != nil {
		return nil, err
	}
	if err := s.documents.Update(ctx, d.ID, d); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *RegistryService) DeleteDocument(ctx context.Context, companyID, id string) error {
	d, err := s.findDocument(ctx, companyID, id)
	if err != nil {
		return err
	}
	return s.documents.SoftDelete(ctx, companyID, d.ID)
}
