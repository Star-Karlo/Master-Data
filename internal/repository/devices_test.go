package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/karlo/masterdata-service/internal/models"
)

func deviceTestDB(t *testing.T) *mongo.Database {
	t.Helper()
	uri := os.Getenv("TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set TEST_MONGO_URI to run the device resolver against MongoDB")
	}
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	return client.Database(os.Getenv("TEST_MONGO_DB"))
}

// A device that moved between vehicles is the case this whole history exists
// for. Resolving March against today's assignment credits one truck's journey
// to another, and nothing about the answer looks wrong.
func TestIMEIsAtRespectsTheAssignmentWindow(t *testing.T) {
	db := deviceTestDB(t)
	col := db.Collection(models.TrackerAssignment{}.CollectionName())
	ctx := context.Background()

	company := "test-co-" + time.Now().Format("150405.000")
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	jun := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	_, err := col.InsertMany(ctx, []any{
		// One device, moved from truck A to truck B in June.
		bson.M{"companyId": company, "vehicleId": "truck-a", "imei": "111", "fittedAt": jan, "unfittedAt": jun},
		bson.M{"companyId": company, "vehicleId": "truck-b", "imei": "111", "fittedAt": jun, "unfittedAt": nil},
		// A second device, never moved.
		bson.M{"companyId": company, "vehicleId": "truck-c", "imei": "222", "fittedAt": jan, "unfittedAt": nil},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _, _ = col.DeleteMany(ctx, bson.M{"companyId": company}) })

	resolver := NewDeviceResolver(db)
	vehicles := []string{"truck-a", "truck-b", "truck-c"}

	// March: the device is still on truck A, and truck B has nothing.
	march, err := resolver.IMEIsAt(ctx, company, vehicles, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("IMEIsAt(March): %v", err)
	}
	if march["truck-a"] != "111" {
		t.Errorf("March truck-a = %q, want 111", march["truck-a"])
	}
	if _, present := march["truck-b"]; present {
		t.Error("truck-b had no device in March and should be absent, not empty")
	}

	// September: the device has moved to truck B and truck A has nothing.
	sept, err := resolver.IMEIsAt(ctx, company, vehicles, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("IMEIsAt(September): %v", err)
	}
	if sept["truck-b"] != "111" {
		t.Errorf("September truck-b = %q, want 111", sept["truck-b"])
	}
	if _, present := sept["truck-a"]; present {
		t.Error("truck-a lost its device in June and should be absent in September")
	}
	// The unmoved device resolves at both moments.
	if march["truck-c"] != "222" || sept["truck-c"] != "222" {
		t.Error("a device that never moved should resolve at any moment in its window")
	}
}

// A fitting dated in the future must not match now, or a scheduled
// installation would be treated as live.
func TestIMEIsAtIgnoresFutureFittings(t *testing.T) {
	db := deviceTestDB(t)
	col := db.Collection(models.TrackerAssignment{}.CollectionName())
	ctx := context.Background()

	company := "test-future-" + time.Now().Format("150405.000")
	_, err := col.InsertOne(ctx, bson.M{
		"companyId": company, "vehicleId": "truck-x", "imei": "333",
		"fittedAt": time.Now().AddDate(0, 1, 0), "unfittedAt": nil,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _, _ = col.DeleteMany(ctx, bson.M{"companyId": company}) })

	got, err := NewDeviceResolver(db).IMEIsAt(ctx, company, []string{"truck-x"}, time.Now())
	if err != nil {
		t.Fatalf("IMEIsAt: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a fitting dated next month resolved as live: %v", got)
	}
}

// The reverse direction, which a telemetry trail asks.
func TestVehicleAtResolvesTheCarrierAtThatMoment(t *testing.T) {
	db := deviceTestDB(t)
	col := db.Collection(models.TrackerAssignment{}.CollectionName())
	ctx := context.Background()

	company := "test-rev-" + time.Now().Format("150405.000")
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	jun := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	_, err := col.InsertMany(ctx, []any{
		bson.M{"companyId": company, "vehicleId": "truck-a", "imei": "999", "fittedAt": jan, "unfittedAt": jun},
		bson.M{"companyId": company, "vehicleId": "truck-b", "imei": "999", "fittedAt": jun, "unfittedAt": nil},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _, _ = col.DeleteMany(ctx, bson.M{"companyId": company}) })

	resolver := NewDeviceResolver(db)

	vehicle, found, err := resolver.VehicleAt(ctx, "999", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || !found {
		t.Fatalf("VehicleAt(March): %v found=%v", err, found)
	}
	if vehicle != "truck-a" {
		t.Errorf("March carrier = %q, want truck-a — resolving against today would credit truck-a's journey to truck-b", vehicle)
	}

	vehicle, _, _ = resolver.VehicleAt(ctx, "999", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if vehicle != "truck-b" {
		t.Errorf("September carrier = %q, want truck-b", vehicle)
	}
}

// Timestamps must actually be set, and this test exists because they were not.
//
// stampCreate asserts for the method through an anonymous interface. While that
// method was unexported and declared in another package, the assertion could
// never match — so every document was written with a zero createdAt, and the
// collection validators that require the field rejected the insert outright.
// The failure was invisible from Go: the assertion returns ok=false and moves on.
func TestCreateStampsTimestamps(t *testing.T) {
	db := deviceTestDB(t)
	ctx := context.Background()

	store := NewStore[models.Brand](db)
	name := "Test Brand " + time.Now().Format("150405.000")

	brand := models.Brand{Name: name}
	brand.IsActive = true
	if err := store.Create(ctx, &brand); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.Collection().DeleteMany(ctx, bson.M{"name": name})
	})

	if brand.CreatedAt.IsZero() {
		t.Error("createdAt was not stamped; the collection validator will refuse this document")
	}
	if brand.UpdatedAt.IsZero() {
		t.Error("updatedAt was not stamped")
	}

	// And BeforeWrite must have run, which is the other half of the contract:
	// the unique index is on the normalised twin, so an empty one means the
	// partial index skips the document and a duplicate is created silently.
	if brand.NameNormalised == "" {
		t.Error("BeforeWrite did not run; the unique index is built on nameNormalised")
	}
}

// Embedded Base and Owned must be written FLAT, not nested.
//
// The mongo driver does not flatten an embedded struct without an explicit
// `bson:",inline"` tag. Untagged, every document went to Mongo shaped
// {base: {createdAt, ...}, name: ...} — which no validator requiring createdAt
// accepts, and which no index built on a Base field can ever match. Nothing in
// Go reports it: the struct is valid, the marshal succeeds, and only the
// database objects.
func TestEmbeddedBaseIsWrittenFlat(t *testing.T) {
	db := deviceTestDB(t)
	ctx := context.Background()

	store := NewStore[models.Brand](db)
	name := "Inline Check " + time.Now().Format("150405.000")

	brand := models.Brand{Name: name}
	brand.IsActive = true
	if err := store.Create(ctx, &brand); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.Collection().DeleteMany(ctx, bson.M{"name": name})
	})

	// Found by name, not by id: Store.Create does not write Mongo's generated
	// _id back onto the struct, so brand.ID is still zero here. Callers that
	// need the id mint it themselves before writing.
	var raw bson.M
	if err := store.Collection().FindOne(ctx, bson.M{"name": name}).Decode(&raw); err != nil {
		t.Fatalf("read back: %v", err)
	}

	for _, field := range []string{"createdAt", "updatedAt", "deleted", "isActive", "name"} {
		if _, present := raw[field]; !present {
			t.Errorf("%q is not a top-level field: the embedded struct was nested", field)
		}
	}
	for _, nested := range []string{"base", "owned"} {
		if _, present := raw[nested]; present {
			t.Errorf("found a nested %q sub-document; the inline tag is missing", nested)
		}
	}
}
