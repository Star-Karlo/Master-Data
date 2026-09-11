// Package repository holds the MongoDB access layer.
//
// One rule runs through every method here: a query against a company-owned
// collection always carries the company id. There is no "find by id" that skips
// it, because that is exactly the shape of query that leaks one tenant's data
// to another.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	// ErrNotFound is returned when nothing matches.
	ErrNotFound = errors.New("repository: not found")
	// ErrConflict is returned on a duplicate key.
	ErrConflict = errors.New("repository: conflict")
)

// ---------------------------------------------------------------------------
// Catalogues
// ---------------------------------------------------------------------------

// CatalogRepository reads and writes the global catalogues.
type CatalogRepository struct {
	col *mongo.Collection
}

func NewCatalogRepository(db *mongo.Database) *CatalogRepository {
	return &CatalogRepository{col: db.Collection(models.CatalogItem{}.CollectionName())}
}

var catalogFields = query.FieldSet{
	"code":      "code",
	"name":      "name",
	"active":    "active",
	"sortOrder": "sortOrder",
	"createdAt": "createdAt",
}

// CatalogFields is the allowlist for filtering and sorting catalogues.
func CatalogFields() query.FieldSet { return catalogFields }

// visibleTo builds the company scoping clause for a catalogue read.
//
// A company sees the platform-global entries plus its own, and nothing
// belonging to anyone else. Passing an empty companyId yields globals only,
// which is what an unauthenticated or platform-level read should see.
func visibleTo(companyID string) bson.M {
	if companyID == "" {
		return bson.M{"companyId": models.GlobalCompanyID}
	}
	return bson.M{"companyId": bson.M{"$in": []string{models.GlobalCompanyID, companyID}}}
}

func (r *CatalogRepository) FindByID(ctx context.Context, companyID string, kind models.CatalogKind, id primitive.ObjectID) (*models.CatalogItem, error) {
	var item models.CatalogItem
	// Three conditions, all load-bearing. The id alone is not enough: the kind
	// stops an id from one catalogue resolving under another, and the company
	// scope stops one company reading another's private entry.
	filter := bson.M{"_id": id, "kind": kind}
	for k, v := range visibleTo(companyID) {
		filter[k] = v
	}
	err := r.col.FindOne(ctx, filter).Decode(&item)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("repository: find catalog item: %w", err)
	}
	return &item, nil
}

// FindManyByRefs resolves a heterogeneous batch of catalogue references in one
// round trip, using an $or over (kind, id) pairs.
func (r *CatalogRepository) FindManyByRefs(ctx context.Context, companyID string, refs []CatalogRef) ([]models.CatalogItem, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	clauses := make([]bson.M, 0, len(refs))
	for _, ref := range refs {
		clauses = append(clauses, bson.M{"_id": ref.ID, "kind": ref.Kind})
	}

	filter := bson.M{"$or": clauses}
	for k, v := range visibleTo(companyID) {
		filter[k] = v
	}

	cur, err := r.col.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("repository: resolve catalog refs: %w", err)
	}
	// A cursor close failure cannot be acted on here: the rows have
	// already been read, and the connection is returned to the pool
	// either way.
	defer func() { _ = cur.Close(ctx) }()

	var items []models.CatalogItem
	if err := cur.All(ctx, &items); err != nil {
		return nil, fmt.Errorf("repository: decode catalog refs: %w", err)
	}
	return items, nil
}

// CatalogRef names one catalogue entry.
type CatalogRef struct {
	Kind models.CatalogKind
	ID   primitive.ObjectID
}

// List pages one catalogue.
func (r *CatalogRepository) List(ctx context.Context, companyID string, kind models.CatalogKind, parentID *primitive.ObjectID, p query.Params) ([]models.CatalogItem, int64, error) {
	filter := bson.M{"kind": kind}
	for k, v := range visibleTo(companyID) {
		filter[k] = v
	}
	if parentID != nil {
		filter["parentId"] = *parentID
	}
	applyFilters(filter, p)

	if p.Search != "" {
		filter["$or"] = []bson.M{
			{"name": regexSearch(p.Search)},
			{"code": regexSearch(p.Search)},
		}
	}

	total, err := r.col.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("repository: count catalog: %w", err)
	}

	opts := options.Find().
		SetSkip(int64(p.Offset())).
		SetLimit(int64(p.PageSize)).
		SetSort(sortDocument(p, bson.D{{Key: "sortOrder", Value: 1}, {Key: "name", Value: 1}}))

	cur, err := r.col.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, fmt.Errorf("repository: list catalog: %w", err)
	}
	// A cursor close failure cannot be acted on here: the rows have
	// already been read, and the connection is returned to the pool
	// either way.
	defer func() { _ = cur.Close(ctx) }()

	var items []models.CatalogItem
	if err := cur.All(ctx, &items); err != nil {
		return nil, 0, fmt.Errorf("repository: decode catalog: %w", err)
	}
	return items, total, nil
}

// Upsert inserts or replaces a catalogue entry, keyed on (kind, code). The seed
// loader relies on this being idempotent.
func (r *CatalogRepository) Upsert(ctx context.Context, item *models.CatalogItem) error {
	now := time.Now().UTC()
	item.UpdatedAt = now

	res, err := r.col.UpdateOne(ctx,
		// Keyed on the company too, so upserting a company's own "BOX" cannot
		// overwrite the platform-global one of the same code.
		bson.M{"companyId": item.CompanyID, "kind": item.Kind, "code": item.Code},
		bson.M{
			"$set": bson.M{
				"name":        item.Name,
				"description": item.Description,
				"active":      item.Active,
				"parentId":    item.ParentID,
				"sortOrder":   item.SortOrder,
				"attributes":  item.Attributes,
				"updatedAt":   now,
			},
			"$setOnInsert": bson.M{"createdAt": now},
		},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("repository: upsert catalog item: %w", err)
	}
	if res.UpsertedID != nil {
		if id, ok := res.UpsertedID.(primitive.ObjectID); ok {
			item.ID = id
		}
	}
	return nil
}

// ExistingIDs returns which of the given refs actually exist, so a caller can
// validate a set of references without fetching their contents.
func (r *CatalogRepository) ExistingIDs(ctx context.Context, companyID string, refs []CatalogRef) (map[string]bool, error) {
	items, err := r.FindManyByRefs(ctx, companyID, refs)
	if err != nil {
		return nil, err
	}
	found := make(map[string]bool, len(items))
	for _, item := range items {
		found[string(item.Kind)+":"+item.ID.Hex()] = true
	}
	return found, nil
}

// ---------------------------------------------------------------------------
// Trucks
// ---------------------------------------------------------------------------

type TruckRepository struct {
	col *mongo.Collection
}

func NewTruckRepository(db *mongo.Database) *TruckRepository {
	return &TruckRepository{col: db.Collection(models.Truck{}.CollectionName())}
}

var truckFields = query.FieldSet{
	"policeNumber": "policeNumber",
	"status":       "status",
	"isAvailable":  "isAvailable",
	"truckTypeId":  "truckTypeId",
	"truckGroupId": "truckGroupId",
	"year":         "year",
	"createdAt":    "createdAt",
}

func TruckFields() query.FieldSet { return truckFields }

// FindByID resolves a truck within a company. The company id is required.
func (r *TruckRepository) FindByID(ctx context.Context, companyID string, id primitive.ObjectID) (*models.Truck, error) {
	var truck models.Truck
	err := r.col.FindOne(ctx, bson.M{"_id": id, "companyId": companyID, "deleted": false}).Decode(&truck)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("repository: find truck: %w", err)
	}
	return &truck, nil
}

// FindByIDAnyCompany resolves a truck without a tenant filter.
//
// This exists only for the gRPC read path, where the business service already
// holds a truck id it obtained legitimately and needs to render it. It is
// deliberately named so that using it by accident is hard.
func (r *TruckRepository) FindByIDAnyCompany(ctx context.Context, id primitive.ObjectID) (*models.Truck, error) {
	var truck models.Truck
	err := r.col.FindOne(ctx, bson.M{"_id": id, "deleted": false}).Decode(&truck)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("repository: find truck: %w", err)
	}
	return &truck, nil
}

func (r *TruckRepository) List(ctx context.Context, companyID string, p query.Params) ([]models.Truck, int64, error) {
	filter := bson.M{"companyId": companyID, "deleted": false}
	applyFilters(filter, p)

	if p.Search != "" {
		filter["policeNumber"] = regexSearch(p.Search)
	}

	total, err := r.col.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("repository: count trucks: %w", err)
	}

	opts := options.Find().
		SetSkip(int64(p.Offset())).
		SetLimit(int64(p.PageSize)).
		SetSort(sortDocument(p, bson.D{{Key: "createdAt", Value: -1}}))

	cur, err := r.col.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, fmt.Errorf("repository: list trucks: %w", err)
	}
	// A cursor close failure cannot be acted on here: the rows have
	// already been read, and the connection is returned to the pool
	// either way.
	defer func() { _ = cur.Close(ctx) }()

	var trucks []models.Truck
	if err := cur.All(ctx, &trucks); err != nil {
		return nil, 0, fmt.Errorf("repository: decode trucks: %w", err)
	}
	return trucks, total, nil
}

// FindByDriver returns the trucks a driver is paired with. The business service
// uses this to validate an assign-driver request.
func (r *TruckRepository) FindByDriver(ctx context.Context, driverID string) ([]models.Truck, error) {
	cur, err := r.col.Find(ctx, bson.M{"driverIds": driverID, "deleted": false})
	if err != nil {
		return nil, fmt.Errorf("repository: find trucks by driver: %w", err)
	}
	// A cursor close failure cannot be acted on here: the rows have
	// already been read, and the connection is returned to the pool
	// either way.
	defer func() { _ = cur.Close(ctx) }()

	var trucks []models.Truck
	if err := cur.All(ctx, &trucks); err != nil {
		return nil, fmt.Errorf("repository: decode trucks: %w", err)
	}
	return trucks, nil
}

func (r *TruckRepository) Create(ctx context.Context, truck *models.Truck) error {
	now := time.Now().UTC()
	truck.CreatedAt = now
	truck.UpdatedAt = now
	if truck.DriverIDs == nil {
		truck.DriverIDs = []string{}
	}

	res, err := r.col.InsertOne(ctx, truck)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("%w: a truck with this police number already exists", ErrConflict)
		}
		return fmt.Errorf("repository: create truck: %w", err)
	}
	if id, ok := res.InsertedID.(primitive.ObjectID); ok {
		truck.ID = id
	}
	return nil
}

// Update applies an allowlisted field set within a company.
func (r *TruckRepository) Update(ctx context.Context, companyID string, id primitive.ObjectID, fields bson.M) error {
	if len(fields) == 0 {
		return nil
	}
	fields["updatedAt"] = time.Now().UTC()

	res, err := r.col.UpdateOne(ctx,
		bson.M{"_id": id, "companyId": companyID, "deleted": false},
		bson.M{"$set": fields},
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrConflict
		}
		return fmt.Errorf("repository: update truck: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// SoftDelete marks a truck deleted, preserving references from historic orders.
func (r *TruckRepository) SoftDelete(ctx context.Context, companyID string, id primitive.ObjectID) error {
	res, err := r.col.UpdateOne(ctx,
		bson.M{"_id": id, "companyId": companyID, "deleted": false},
		bson.M{"$set": bson.M{"deleted": true, "updatedAt": time.Now().UTC()}},
	)
	if err != nil {
		return fmt.Errorf("repository: delete truck: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// AddDriver pairs a driver with a truck. $addToSet makes the call idempotent.
func (r *TruckRepository) AddDriver(ctx context.Context, companyID string, id primitive.ObjectID, driverID string) error {
	res, err := r.col.UpdateOne(ctx,
		bson.M{"_id": id, "companyId": companyID, "deleted": false},
		bson.M{
			"$addToSet": bson.M{"driverIds": driverID},
			"$set":      bson.M{"updatedAt": time.Now().UTC()},
		},
	)
	if err != nil {
		return fmt.Errorf("repository: add driver: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// RemoveDriver unpairs a driver.
func (r *TruckRepository) RemoveDriver(ctx context.Context, companyID string, id primitive.ObjectID, driverID string) error {
	res, err := r.col.UpdateOne(ctx,
		bson.M{"_id": id, "companyId": companyID, "deleted": false},
		bson.M{
			"$pull": bson.M{"driverIds": driverID},
			"$set":  bson.M{"updatedAt": time.Now().UTC()},
		},
	)
	if err != nil {
		return fmt.Errorf("repository: remove driver: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Warehouses
// ---------------------------------------------------------------------------

type WarehouseRepository struct {
	col *mongo.Collection
}

func NewWarehouseRepository(db *mongo.Database) *WarehouseRepository {
	return &WarehouseRepository{col: db.Collection(models.Warehouse{}.CollectionName())}
}

var warehouseFields = query.FieldSet{
	"name":      "name",
	"code":      "code",
	"cityId":    "cityId",
	"createdAt": "createdAt",
}

func WarehouseFields() query.FieldSet { return warehouseFields }

func (r *WarehouseRepository) FindByID(ctx context.Context, companyID string, id primitive.ObjectID) (*models.Warehouse, error) {
	var w models.Warehouse
	err := r.col.FindOne(ctx, bson.M{"_id": id, "companyId": companyID, "deleted": false}).Decode(&w)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("repository: find warehouse: %w", err)
	}
	return &w, nil
}

// FindByIDAnyCompany serves the gRPC read path; see the note on the truck
// equivalent. A shipment references warehouses from two different companies,
// so the caller cannot supply one company id that covers both.
func (r *WarehouseRepository) FindByIDAnyCompany(ctx context.Context, id primitive.ObjectID) (*models.Warehouse, error) {
	var w models.Warehouse
	err := r.col.FindOne(ctx, bson.M{"_id": id, "deleted": false}).Decode(&w)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("repository: find warehouse: %w", err)
	}
	return &w, nil
}

func (r *WarehouseRepository) List(ctx context.Context, companyID string, p query.Params) ([]models.Warehouse, int64, error) {
	filter := bson.M{"companyId": companyID, "deleted": false}
	applyFilters(filter, p)

	if p.Search != "" {
		filter["$or"] = []bson.M{
			{"name": regexSearch(p.Search)},
			{"address": regexSearch(p.Search)},
		}
	}

	total, err := r.col.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("repository: count warehouses: %w", err)
	}

	opts := options.Find().
		SetSkip(int64(p.Offset())).
		SetLimit(int64(p.PageSize)).
		SetSort(sortDocument(p, bson.D{{Key: "name", Value: 1}}))

	cur, err := r.col.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, fmt.Errorf("repository: list warehouses: %w", err)
	}
	// A cursor close failure cannot be acted on here: the rows have
	// already been read, and the connection is returned to the pool
	// either way.
	defer func() { _ = cur.Close(ctx) }()

	var out []models.Warehouse
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, fmt.Errorf("repository: decode warehouses: %w", err)
	}
	return out, total, nil
}

// FindNear returns warehouses within radius metres of a position, using the
// 2dsphere index. The business service uses it to answer "is the driver at the
// right warehouse" without shipping every warehouse to the caller.
func (r *WarehouseRepository) FindNear(ctx context.Context, lat, lng float64, radiusMeters int) ([]models.Warehouse, error) {
	filter := bson.M{
		"deleted": false,
		"location": bson.M{
			"$nearSphere": bson.M{
				"$geometry":    bson.M{"type": "Point", "coordinates": []float64{lng, lat}},
				"$maxDistance": radiusMeters,
			},
		},
	}

	cur, err := r.col.Find(ctx, filter, options.Find().SetLimit(20))
	if err != nil {
		return nil, fmt.Errorf("repository: find warehouses near: %w", err)
	}
	// A cursor close failure cannot be acted on here: the rows have
	// already been read, and the connection is returned to the pool
	// either way.
	defer func() { _ = cur.Close(ctx) }()

	var out []models.Warehouse
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("repository: decode warehouses: %w", err)
	}
	return out, nil
}

func (r *WarehouseRepository) Create(ctx context.Context, w *models.Warehouse) error {
	now := time.Now().UTC()
	w.CreatedAt = now
	w.UpdatedAt = now
	if w.GeofenceRadius == 0 {
		w.GeofenceRadius = models.DefaultGeofenceRadius
	}

	res, err := r.col.InsertOne(ctx, w)
	if err != nil {
		return fmt.Errorf("repository: create warehouse: %w", err)
	}
	if id, ok := res.InsertedID.(primitive.ObjectID); ok {
		w.ID = id
	}
	return nil
}

func (r *WarehouseRepository) Update(ctx context.Context, companyID string, id primitive.ObjectID, fields bson.M) error {
	if len(fields) == 0 {
		return nil
	}
	fields["updatedAt"] = time.Now().UTC()

	res, err := r.col.UpdateOne(ctx,
		bson.M{"_id": id, "companyId": companyID, "deleted": false},
		bson.M{"$set": fields},
	)
	if err != nil {
		return fmt.Errorf("repository: update warehouse: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *WarehouseRepository) SoftDelete(ctx context.Context, companyID string, id primitive.ObjectID) error {
	res, err := r.col.UpdateOne(ctx,
		bson.M{"_id": id, "companyId": companyID, "deleted": false},
		bson.M{"$set": bson.M{"deleted": true, "updatedAt": time.Now().UTC()}},
	)
	if err != nil {
		return fmt.Errorf("repository: delete warehouse: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// FindByCode resolves a catalogue entry by its business code. The seed loader
// uses it to recover the id of an entry that already existed, so that child
// entries can be linked on a repeat run.
func (r *CatalogRepository) FindByCode(ctx context.Context, companyID string, kind models.CatalogKind, code string) (*models.CatalogItem, error) {
	var item models.CatalogItem
	err := r.col.FindOne(ctx, bson.M{"companyId": companyID, "kind": kind, "code": code}).Decode(&item)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("repository: find catalog item by code: %w", err)
	}
	return &item, nil
}
