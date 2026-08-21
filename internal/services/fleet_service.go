package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/repository"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ErrValidation reports a rejected write.
var ErrValidation = errors.New("validation failed")

// FleetService manages a company's trucks and warehouses.
type FleetService struct {
	trucks     *repository.TruckRepository
	warehouses *repository.WarehouseRepository
	catalog    *CatalogService
}

func NewFleetService(
	trucks *repository.TruckRepository,
	warehouses *repository.WarehouseRepository,
	catalog *CatalogService,
) *FleetService {
	return &FleetService{trucks: trucks, warehouses: warehouses, catalog: catalog}
}

// ---------------------------------------------------------------------------
// Trucks
// ---------------------------------------------------------------------------

func (s *FleetService) GetTruck(ctx context.Context, companyID string, id primitive.ObjectID) (*models.Truck, error) {
	return s.trucks.FindByID(ctx, companyID, id)
}

// GetTruckForService is the cross-service read that skips the tenant filter.
func (s *FleetService) GetTruckForService(ctx context.Context, id primitive.ObjectID) (*models.Truck, error) {
	return s.trucks.FindByIDAnyCompany(ctx, id)
}

func (s *FleetService) ListTrucks(ctx context.Context, companyID string, p query.Params) ([]models.Truck, int64, error) {
	return s.trucks.List(ctx, companyID, p)
}

func (s *FleetService) TrucksByDriver(ctx context.Context, driverID string) ([]models.Truck, error) {
	return s.trucks.FindByDriver(ctx, driverID)
}

// CreateTruck registers a vehicle, validating its catalogue references first so
// a truck cannot be stored pointing at a truck type that does not exist.
func (s *FleetService) CreateTruck(ctx context.Context, truck *models.Truck) error {
	if truck.CompanyID == "" {
		return fmt.Errorf("%w: companyId is required", ErrValidation)
	}
	if truck.PoliceNumber == "" {
		return fmt.Errorf("%w: policeNumber is required", ErrValidation)
	}

	if err := s.validateTruckRefs(ctx, truck); err != nil {
		return err
	}

	if truck.Status == "" {
		truck.Status = models.TruckStatusActive
	}
	return s.trucks.Create(ctx, truck)
}

func (s *FleetService) validateTruckRefs(ctx context.Context, truck *models.Truck) error {
	var refs []repository.CatalogRef
	add := func(kind models.CatalogKind, id *primitive.ObjectID) {
		if id != nil {
			refs = append(refs, repository.CatalogRef{Kind: kind, ID: *id})
		}
	}

	add(models.KindTruckType, truck.TruckTypeID)
	add(models.KindTruckHead, truck.TruckHeadID)
	add(models.KindTruckBody, truck.TruckBodyID)
	add(models.KindBrand, truck.BrandID)

	if len(refs) == 0 {
		return nil
	}

	invalid, err := s.catalog.Validate(ctx, refs)
	if err != nil {
		return err
	}
	if len(invalid) > 0 {
		kinds := make([]string, 0, len(invalid))
		for _, ref := range invalid {
			kinds = append(kinds, string(ref.Kind))
		}
		return fmt.Errorf("%w: unknown reference for %v", ErrValidation, kinds)
	}
	return nil
}

// truckUpdatable is the allowlist of fields a client may change on a truck.
// Notably absent: companyId and driverIds. Moving a truck between companies is
// not an update, and driver pairing has its own endpoints with their own rules.
var truckUpdatable = map[string]string{
	"policeNumber":  "policeNumber",
	"chassisNumber": "chassisNumber",
	"engineNumber":  "engineNumber",
	"year":          "year",
	"truckTypeId":   "truckTypeId",
	"truckHeadId":   "truckHeadId",
	"truckBodyId":   "truckBodyId",
	"brandId":       "brandId",
	"truckGroupId":  "truckGroupId",
	"status":        "status",
	"isAvailable":   "isAvailable",
	"documents":     "documents",
}

// UpdateTruck applies an allowlisted partial update.
func (s *FleetService) UpdateTruck(ctx context.Context, companyID string, id primitive.ObjectID, input map[string]interface{}) error {
	fields := projectAllowed(input, truckUpdatable)
	if len(fields) == 0 {
		return fmt.Errorf("%w: no updatable fields supplied", ErrValidation)
	}

	// Catalogue ids arrive as hex strings and must be stored as ObjectIds, or
	// later lookups silently match nothing.
	for _, key := range []string{"truckTypeId", "truckHeadId", "truckBodyId", "brandId", "truckGroupId"} {
		if raw, ok := fields[key]; ok {
			oid, err := toObjectID(raw)
			if err != nil {
				return fmt.Errorf("%w: %s must be a valid id", ErrValidation, key)
			}
			fields[key] = oid
		}
	}

	return s.trucks.Update(ctx, companyID, id, fields)
}

func (s *FleetService) DeleteTruck(ctx context.Context, companyID string, id primitive.ObjectID) error {
	return s.trucks.SoftDelete(ctx, companyID, id)
}

// AddDriver pairs a driver with a truck.
func (s *FleetService) AddDriver(ctx context.Context, companyID string, truckID primitive.ObjectID, driverID string) error {
	if driverID == "" {
		return fmt.Errorf("%w: driverId is required", ErrValidation)
	}
	return s.trucks.AddDriver(ctx, companyID, truckID, driverID)
}

func (s *FleetService) RemoveDriver(ctx context.Context, companyID string, truckID primitive.ObjectID, driverID string) error {
	return s.trucks.RemoveDriver(ctx, companyID, truckID, driverID)
}

// ---------------------------------------------------------------------------
// Warehouses
// ---------------------------------------------------------------------------

func (s *FleetService) GetWarehouse(ctx context.Context, companyID string, id primitive.ObjectID) (*models.Warehouse, error) {
	return s.warehouses.FindByID(ctx, companyID, id)
}

// GetWarehouseForService is the cross-service read; a shipment spans two
// companies' warehouses, so the caller has no single tenant to filter by.
func (s *FleetService) GetWarehouseForService(ctx context.Context, id primitive.ObjectID) (*models.Warehouse, error) {
	return s.warehouses.FindByIDAnyCompany(ctx, id)
}

func (s *FleetService) ListWarehouses(ctx context.Context, companyID string, p query.Params) ([]models.Warehouse, int64, error) {
	return s.warehouses.List(ctx, companyID, p)
}

// CreateWarehouse registers a location.
func (s *FleetService) CreateWarehouse(ctx context.Context, w *models.Warehouse) error {
	if w.CompanyID == "" {
		return fmt.Errorf("%w: companyId is required", ErrValidation)
	}
	if w.Name == "" {
		return fmt.Errorf("%w: name is required", ErrValidation)
	}
	if err := validateCoordinates(w.Location); err != nil {
		return err
	}
	return s.warehouses.Create(ctx, w)
}

var warehouseUpdatable = map[string]string{
	"name":           "name",
	"code":           "code",
	"address":        "address",
	"cityId":         "cityId",
	"provinceId":     "provinceId",
	"postalCode":     "postalCode",
	"geofenceRadius": "geofenceRadius",
	"picName":        "picName",
	"picPhone":       "picPhone",
	"operatingHours": "operatingHours",
}

// UpdateWarehouse applies an allowlisted partial update. Position is handled
// separately because it must be written in GeoJSON form.
func (s *FleetService) UpdateWarehouse(ctx context.Context, companyID string, id primitive.ObjectID, input map[string]interface{}) error {
	fields := projectAllowed(input, warehouseUpdatable)

	lat, hasLat := floatFrom(input["latitude"])
	lng, hasLng := floatFrom(input["longitude"])
	switch {
	case hasLat && hasLng:
		point := models.NewGeoPoint(lat, lng)
		if err := validateCoordinates(point); err != nil {
			return err
		}
		fields["location"] = point
	case hasLat != hasLng:
		// Writing one without the other would leave the stored point half
		// updated and pointing somewhere real but wrong.
		return fmt.Errorf("%w: latitude and longitude must be supplied together", ErrValidation)
	}

	for _, key := range []string{"cityId", "provinceId"} {
		if raw, ok := fields[key]; ok {
			oid, err := toObjectID(raw)
			if err != nil {
				return fmt.Errorf("%w: %s must be a valid id", ErrValidation, key)
			}
			fields[key] = oid
		}
	}

	if len(fields) == 0 {
		return fmt.Errorf("%w: no updatable fields supplied", ErrValidation)
	}
	return s.warehouses.Update(ctx, companyID, id, fields)
}

func (s *FleetService) DeleteWarehouse(ctx context.Context, companyID string, id primitive.ObjectID) error {
	return s.warehouses.SoftDelete(ctx, companyID, id)
}

// WarehousesNear answers a proximity question for the business service.
func (s *FleetService) WarehousesNear(ctx context.Context, lat, lng float64, radius int) ([]models.Warehouse, error) {
	if radius <= 0 {
		radius = models.DefaultGeofenceRadius
	}
	return s.warehouses.FindNear(ctx, lat, lng, radius)
}

// validateCoordinates rejects positions outside the valid range, and rejects
// the null island point, which is almost always an unset value rather than a
// real location in the Gulf of Guinea.
func validateCoordinates(p models.GeoPoint) error {
	lat, lng := p.Lat(), p.Lng()
	if lat < -90 || lat > 90 {
		return fmt.Errorf("%w: latitude %.6f is out of range", ErrValidation, lat)
	}
	if lng < -180 || lng > 180 {
		return fmt.Errorf("%w: longitude %.6f is out of range", ErrValidation, lng)
	}
	if lat == 0 && lng == 0 {
		return fmt.Errorf("%w: coordinates are required", ErrValidation)
	}
	return nil
}

// projectAllowed maps a client body onto storage fields through an allowlist.
func projectAllowed(input map[string]interface{}, allowed map[string]string) bson.M {
	out := bson.M{}
	for k, v := range input {
		if field, ok := allowed[k]; ok {
			out[field] = v
		}
	}
	return out
}

func toObjectID(raw interface{}) (primitive.ObjectID, error) {
	switch v := raw.(type) {
	case primitive.ObjectID:
		return v, nil
	case string:
		return primitive.ObjectIDFromHex(v)
	default:
		return primitive.NilObjectID, fmt.Errorf("cannot read %T as an id", raw)
	}
}

func floatFrom(raw interface{}) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}
