//go:build integration

package integration

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/karlo/masterdata-service/internal/config"
	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/repository"
	"go.mongodb.org/mongo-driver/bson"
)

// TestEnsureIndexesCreatesWhatTheQueriesNeed confirms the indexes actually
// exist. A missing index is invisible in a test dataset and catastrophic in a
// real one: the query still returns the right answer, just by scanning the
// whole collection.
func TestEnsureIndexesCreatesWhatTheQueriesNeed(t *testing.T) {
	db := testDB(t)
	resetCollections(t, db)

	t.Run("catalogue code is unique within a kind", func(t *testing.T) {
		indexes := indexNames(t, db, config.Collection("catalog_items"))

		var found bool
		for name, idx := range indexes {
			if strings.Contains(name, "kind") && strings.Contains(name, "code") {
				if unique, _ := idx["unique"].(bool); unique {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("no unique (kind, code) index; got %v", keysOf(indexes))
		}
	})

	t.Run("warehouses carry a geospatial index", func(t *testing.T) {
		indexes := indexNames(t, db, config.Collection("warehouses"))

		var found bool
		for name := range indexes {
			if strings.Contains(name, "location") {
				found = true
			}
		}
		if !found {
			t.Errorf("no location index; the proximity query would scan. Got %v", keysOf(indexes))
		}
	})

	t.Run("police numbers are unique within a company", func(t *testing.T) {
		indexes := indexNames(t, db, config.Collection("trucks"))

		var found bool
		for name, idx := range indexes {
			if strings.Contains(name, "policeNumber") {
				if unique, _ := idx["unique"].(bool); unique {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("no unique (companyId, policeNumber) index; got %v", keysOf(indexes))
		}
	})
}

// TestCatalogUpsertIsIdempotent is what makes the seed loader safe to re-run.
func TestCatalogUpsertIsIdempotent(t *testing.T) {
	db := testDB(t)
	resetCollections(t, db)

	repo := repository.NewCatalogRepository(db)

	item := &models.CatalogItem{
		Kind:   models.KindTruckType,
		Code:   "CDD",
		Name:   "Colt Diesel Double",
		Active: true,
	}
	if err := repo.Upsert(ctx(), item); err != nil {
		t.Fatalf("first upsert failed: %v", err)
	}
	firstID := item.ID

	// A second upsert of the same (kind, code) must update, not duplicate.
	updated := &models.CatalogItem{
		Kind:   models.KindTruckType,
		Code:   "CDD",
		Name:   "Colt Diesel Double (updated)",
		Active: true,
	}
	if err := repo.Upsert(ctx(), updated); err != nil {
		t.Fatalf("second upsert failed: %v", err)
	}

	found, err := repo.FindByCode(ctx(), models.KindTruckType, "CDD")
	if err != nil {
		t.Fatalf("could not resolve by code: %v", err)
	}
	if found.ID != firstID {
		t.Error("the upsert replaced the document rather than updating it; references would break")
	}
	if found.Name != "Colt Diesel Double (updated)" {
		t.Errorf("the update did not take: name = %q", found.Name)
	}

	_, total, err := repo.List(ctx(), models.KindTruckType, nil, query.Params{PageSize: 50})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if total != 1 {
		t.Errorf("%d documents exist, want 1", total)
	}

	// The same code under a different kind is a different entry.
	other := &models.CatalogItem{Kind: models.KindTruckBody, Code: "CDD", Name: "Something else", Active: true}
	if err := repo.Upsert(ctx(), other); err != nil {
		t.Fatalf("upsert under a second kind failed: %v", err)
	}
	if other.ID == firstID {
		t.Error("two kinds sharing a code collided")
	}
}

// TestCatalogLookupIsScopedToItsKind: an id from one catalogue must not resolve
// when asked for under another, or a caller could read a rate card by passing a
// truck-type id.
func TestCatalogLookupIsScopedToItsKind(t *testing.T) {
	db := testDB(t)
	resetCollections(t, db)

	repo := repository.NewCatalogRepository(db)

	item := &models.CatalogItem{Kind: models.KindTruckType, Code: "FUSO", Name: "Fuso", Active: true}
	if err := repo.Upsert(ctx(), item); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}

	if _, err := repo.FindByID(ctx(), models.KindTruckType, item.ID); err != nil {
		t.Fatalf("the correct kind did not resolve: %v", err)
	}

	if _, err := repo.FindByID(ctx(), models.KindRateCard, item.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("an id resolved under the wrong kind: %v", err)
	}
}

// TestTruckReadsAreScopedToTheCompany is the tenancy guarantee for company-owned
// master data.
func TestTruckReadsAreScopedToTheCompany(t *testing.T) {
	db := testDB(t)
	resetCollections(t, db)

	repo := repository.NewTruckRepository(db)

	const ours = "company-a"
	const theirs = "company-b"

	truck := &models.Truck{
		CompanyID:    ours,
		PoliceNumber: "B 1234 XYZ",
		Status:       models.TruckStatusActive,
	}
	if err := repo.Create(ctx(), truck); err != nil {
		t.Fatalf("could not create the truck: %v", err)
	}

	if _, err := repo.FindByID(ctx(), ours, truck.ID); err != nil {
		t.Fatalf("the owner could not read their own truck: %v", err)
	}

	// ErrNotFound rather than a permission error: a distinct response would
	// confirm the id exists.
	if _, err := repo.FindByID(ctx(), theirs, truck.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("another company read the truck: %v", err)
	}

	_, total, err := repo.List(ctx(), theirs, query.Params{PageSize: 50})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if total != 0 {
		t.Errorf("another company's listing returned %d trucks", total)
	}

	// The cross-service read deliberately skips the tenant filter, for the gRPC
	// path where the caller is another service holding a legitimate id.
	if _, err := repo.FindByIDAnyCompany(ctx(), truck.ID); err != nil {
		t.Errorf("the cross-service read failed: %v", err)
	}
}

// TestPoliceNumberIsUniquePerCompany: two companies may legitimately register
// the same plate (a vehicle changes hands), but one company may not list it
// twice.
func TestPoliceNumberIsUniquePerCompany(t *testing.T) {
	db := testDB(t)
	resetCollections(t, db)

	repo := repository.NewTruckRepository(db)

	first := &models.Truck{CompanyID: "company-a", PoliceNumber: "B 9999 AA"}
	if err := repo.Create(ctx(), first); err != nil {
		t.Fatalf("could not create the first truck: %v", err)
	}

	duplicate := &models.Truck{CompanyID: "company-a", PoliceNumber: "B 9999 AA"}
	if err := repo.Create(ctx(), duplicate); !errors.Is(err, repository.ErrConflict) {
		t.Errorf("a duplicate plate within one company was accepted: %v", err)
	}

	otherCompany := &models.Truck{CompanyID: "company-b", PoliceNumber: "B 9999 AA"}
	if err := repo.Create(ctx(), otherCompany); err != nil {
		t.Errorf("another company could not register the same plate: %v", err)
	}
}

// TestDriverPairingIsIdempotent covers the $addToSet behaviour the assign flow
// depends on.
func TestDriverPairingIsIdempotent(t *testing.T) {
	db := testDB(t)
	resetCollections(t, db)

	repo := repository.NewTruckRepository(db)

	const company = "company-a"
	const driver = "driver-uuid-1"

	truck := &models.Truck{CompanyID: company, PoliceNumber: "B 4321 CD"}
	if err := repo.Create(ctx(), truck); err != nil {
		t.Fatalf("could not create the truck: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := repo.AddDriver(ctx(), company, truck.ID, driver); err != nil {
			t.Fatalf("pairing failed on attempt %d: %v", i+1, err)
		}
	}

	found, err := repo.FindByID(ctx(), company, truck.ID)
	if err != nil {
		t.Fatalf("could not reload the truck: %v", err)
	}
	if len(found.DriverIDs) != 1 {
		t.Errorf("driverIds = %v, want exactly one entry", found.DriverIDs)
	}

	// The lookup the business service uses before allowing an assignment.
	trucks, err := repo.FindByDriver(ctx(), driver)
	if err != nil {
		t.Fatalf("lookup by driver failed: %v", err)
	}
	if len(trucks) != 1 || trucks[0].ID != truck.ID {
		t.Errorf("the driver's truck was not found: %v", trucks)
	}

	if err := repo.RemoveDriver(ctx(), company, truck.ID, driver); err != nil {
		t.Fatalf("unpairing failed: %v", err)
	}

	trucks, err = repo.FindByDriver(ctx(), driver)
	if err != nil {
		t.Fatalf("lookup by driver failed: %v", err)
	}
	if len(trucks) != 0 {
		t.Errorf("the driver is still paired after removal: %v", trucks)
	}
}

// TestWarehouseProximityQuery exercises the 2dsphere index that backs geofenced
// shipment completion.
func TestWarehouseProximityQuery(t *testing.T) {
	db := testDB(t)
	resetCollections(t, db)

	repo := repository.NewWarehouseRepository(db)

	// Two real Jakarta locations roughly 8 km apart, and one in Surabaya.
	const (
		monasLat, monasLng       = -6.1754, 106.8272
		kotaTuaLat, kotaTuaLng   = -6.1352, 106.8133
		surabayaLat, surabayaLng = -7.2575, 112.7521
	)

	for _, w := range []*models.Warehouse{
		{CompanyID: "c", Name: "Monas Depot", Location: models.NewGeoPoint(monasLat, monasLng), GeofenceRadius: 200},
		{CompanyID: "c", Name: "Kota Tua Depot", Location: models.NewGeoPoint(kotaTuaLat, kotaTuaLng), GeofenceRadius: 200},
		{CompanyID: "c", Name: "Surabaya Depot", Location: models.NewGeoPoint(surabayaLat, surabayaLng), GeofenceRadius: 200},
	} {
		if err := repo.Create(ctx(), w); err != nil {
			t.Fatalf("could not create %s: %v", w.Name, err)
		}
	}

	// Standing at Monas, within a 1 km radius, only Monas Depot is near.
	near, err := repo.FindNear(ctx(), monasLat, monasLng, 1000)
	if err != nil {
		t.Fatalf("proximity query failed: %v", err)
	}
	if len(near) != 1 || near[0].Name != "Monas Depot" {
		var names []string
		for _, w := range near {
			names = append(names, w.Name)
		}
		t.Errorf("within 1 km got %v, want just Monas Depot", names)
	}

	// Widen to 15 km and both Jakarta depots appear, nearest first.
	near, err = repo.FindNear(ctx(), monasLat, monasLng, 15000)
	if err != nil {
		t.Fatalf("proximity query failed: %v", err)
	}
	if len(near) != 2 {
		t.Fatalf("within 15 km got %d warehouses, want 2", len(near))
	}
	if near[0].Name != "Monas Depot" {
		t.Errorf("results are not ordered by distance: first is %s", near[0].Name)
	}

	// Surabaya is ~700 km away and must never appear.
	for _, w := range near {
		if w.Name == "Surabaya Depot" {
			t.Error("a warehouse 700 km away matched a 15 km query")
		}
	}
}

// TestWarehouseCoordinatesRoundTrip guards the GeoJSON ordering across a real
// write and read. Transposing latitude and longitude puts every Indonesian
// warehouse in Somalia, and nothing in the code would complain.
func TestWarehouseCoordinatesRoundTrip(t *testing.T) {
	db := testDB(t)
	resetCollections(t, db)

	repo := repository.NewWarehouseRepository(db)

	const lat, lng = -6.2088, 106.8456

	w := &models.Warehouse{
		CompanyID: "c",
		Name:      "Jakarta",
		Location:  models.NewGeoPoint(lat, lng),
	}
	if err := repo.Create(ctx(), w); err != nil {
		t.Fatalf("could not create: %v", err)
	}

	found, err := repo.FindByID(ctx(), "c", w.ID)
	if err != nil {
		t.Fatalf("could not read back: %v", err)
	}

	if found.Location.Lat() != lat {
		t.Errorf("latitude round-tripped as %v, want %v", found.Location.Lat(), lat)
	}
	if found.Location.Lng() != lng {
		t.Errorf("longitude round-tripped as %v, want %v", found.Location.Lng(), lng)
	}

	// A default radius must be applied, or the geofence check would compare
	// against zero metres and reject every arrival.
	if found.GeofenceRadius != models.DefaultGeofenceRadius {
		t.Errorf("geofenceRadius = %d, want the default %d",
			found.GeofenceRadius, models.DefaultGeofenceRadius)
	}
}

// keysOf lists index names, for a readable failure message.
func keysOf(indexes map[string]bson.M) []string {
	out := make([]string, 0, len(indexes))
	for name := range indexes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
