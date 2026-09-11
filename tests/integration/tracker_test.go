//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/repository"
)

// TestTrackerFittingKeepsBothLinksHonest covers the invariant that makes the
// plate-to-IMEI mapping trustworthy.
//
// Three records must agree after a fit: the tracker's vehicleId, the vehicle's
// trackerId, and exactly one open assignment row. If they disagree, telemetry
// is attributed to the wrong truck and nothing downstream can detect it — the
// positions are all real, they are simply against the wrong vehicle.
func TestTrackerFittingKeepsBothLinksHonest(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := repository.NewTrackerRepository(db)

	const company = "11111111-1111-1111-1111-111111111111"
	truckA := seedVehicle(t, db, company, "B 1111 AAA")
	truckB := seedVehicle(t, db, company, "B 2222 BBB")

	tracker := &models.Tracker{IMEI: "860000000000001", CompanyID: company}
	if err := repo.Create(ctx, tracker); err != nil {
		t.Fatalf("create tracker: %v", err)
	}
	t.Cleanup(func() {
		db.Collection(models.Tracker{}.CollectionName()).DeleteMany(ctx, bson.M{"companyId": company})
		db.Collection(models.TrackerAssignment{}.CollectionName()).DeleteMany(ctx, bson.M{"companyId": company})
	})

	if err := repo.Fit(ctx, company, tracker.ID, truckA, "initial fitting"); err != nil {
		t.Fatalf("fit to A: %v", err)
	}
	assertLinks(t, db, company, tracker.ID, &truckA)

	// Moving the device to another vehicle must close the first posting rather
	// than leave two open. Two open assignments would make "which vehicle was
	// this on" ambiguous for every reading after the move.
	if err := repo.Fit(ctx, company, tracker.ID, truckB, "moved"); err != nil {
		t.Fatalf("fit to B: %v", err)
	}
	assertLinks(t, db, company, tracker.ID, &truckB)

	var open int64
	open, _ = db.Collection(models.TrackerAssignment{}.CollectionName()).
		CountDocuments(ctx, bson.M{"trackerId": tracker.ID, "unfittedAt": nil})
	if open != 1 {
		t.Errorf("expected exactly one open assignment after a move, found %d", open)
	}

	// Truck A must no longer claim the device.
	var a models.Truck
	if err := db.Collection(models.Truck{}.CollectionName()).
		FindOne(ctx, bson.M{"_id": truckA}).Decode(&a); err != nil {
		t.Fatalf("read truck A: %v", err)
	}
	if a.TrackerID != nil {
		t.Error("the vehicle a device was moved OFF must not still point at it")
	}
}

// TestVehicleAtTimeAttributesHistoryCorrectly is the reason assignment history
// exists at all.
//
// A device carries one continuous IMEI trail across every vehicle it is fitted
// to. Reading that trail against the CURRENT vehicle would credit everything an
// earlier truck drove to whichever truck holds the device now, corrupting
// distance reports and driver scoring without ever raising an error.
func TestVehicleAtTimeAttributesHistoryCorrectly(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := repository.NewTrackerRepository(db)

	const company = "22222222-2222-2222-2222-222222222222"
	truckA := seedVehicle(t, db, company, "B 3333 CCC")
	truckB := seedVehicle(t, db, company, "B 4444 DDD")

	tracker := &models.Tracker{IMEI: "860000000000002", CompanyID: company}
	if err := repo.Create(ctx, tracker); err != nil {
		t.Fatalf("create tracker: %v", err)
	}
	t.Cleanup(func() {
		db.Collection(models.Tracker{}.CollectionName()).DeleteMany(ctx, bson.M{"companyId": company})
		db.Collection(models.TrackerAssignment{}.CollectionName()).DeleteMany(ctx, bson.M{"companyId": company})
	})

	if err := repo.Fit(ctx, company, tracker.ID, truckA, ""); err != nil {
		t.Fatalf("fit A: %v", err)
	}
	duringA := time.Now().UTC()

	time.Sleep(10 * time.Millisecond)
	if err := repo.Fit(ctx, company, tracker.ID, truckB, ""); err != nil {
		t.Fatalf("fit B: %v", err)
	}
	duringB := time.Now().UTC()

	was, err := repo.VehicleAt(ctx, tracker.IMEI, duringA)
	if err != nil {
		t.Fatalf("resolve during A: %v", err)
	}
	if was.VehicleID != truckA {
		t.Error("a reading from before the move must be attributed to the earlier vehicle")
	}

	was, err = repo.VehicleAt(ctx, tracker.IMEI, duringB)
	if err != nil {
		t.Fatalf("resolve during B: %v", err)
	}
	if was.VehicleID != truckB {
		t.Error("a reading from after the move must be attributed to the later vehicle")
	}

	// Before the device was ever fitted, there is no answer — and saying "the
	// first vehicle" would invent one.
	if _, err := repo.VehicleAt(ctx, tracker.IMEI, time.Now().UTC().Add(-24*time.Hour)); err == nil {
		t.Error("a time before the first fitting must resolve to nothing")
	}
}

// TestIMEIIsGloballyUnique covers the constraint that stops two companies
// claiming one physical device.
func TestIMEIIsGloballyUnique(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := repository.NewTrackerRepository(db)

	const imei = "860000000000003"
	first := &models.Tracker{IMEI: imei, CompanyID: "aaaa1111-1111-1111-1111-111111111111"}
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	t.Cleanup(func() {
		db.Collection(models.Tracker{}.CollectionName()).DeleteMany(ctx, bson.M{"imei": imei})
	})

	second := &models.Tracker{IMEI: imei, CompanyID: "bbbb2222-2222-2222-2222-222222222222"}
	err := repo.Create(ctx, second)
	if err == nil {
		t.Fatal("a second company must not be able to register the same IMEI: " +
			"a device is one object in the world")
	}
	// The message must not confirm that another tenant holds it.
	if got := err.Error(); got != repository.ErrIMEITaken.Error() {
		t.Errorf("unexpected error %q", got)
	}
}

// TestFitRefusesAnotherCompanysVehicle covers the tenancy check.
func TestFitRefusesAnotherCompanysVehicle(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := repository.NewTrackerRepository(db)

	const mine = "cccc3333-3333-3333-3333-333333333333"
	const theirs = "dddd4444-4444-4444-4444-444444444444"
	theirTruck := seedVehicle(t, db, theirs, "B 5555 EEE")

	tracker := &models.Tracker{IMEI: "860000000000004", CompanyID: mine}
	if err := repo.Create(ctx, tracker); err != nil {
		t.Fatalf("create tracker: %v", err)
	}
	t.Cleanup(func() {
		db.Collection(models.Tracker{}.CollectionName()).DeleteMany(ctx, bson.M{"imei": "860000000000004"})
	})

	if err := repo.Fit(ctx, mine, tracker.ID, theirTruck, ""); err == nil {
		t.Fatal("fitting a device to another company's vehicle must be refused: " +
			"it would let one tenant read another's positions")
	}
}

// assertLinks checks that the tracker, the vehicle and the open assignment all
// name each other.
func assertLinks(t *testing.T, db *mongo.Database, company string, trackerID primitive.ObjectID, wantVehicle *primitive.ObjectID) {
	t.Helper()
	ctx := context.Background()

	var tracker models.Tracker
	if err := db.Collection(models.Tracker{}.CollectionName()).
		FindOne(ctx, bson.M{"_id": trackerID}).Decode(&tracker); err != nil {
		t.Fatalf("read tracker: %v", err)
	}
	if tracker.VehicleID == nil || *tracker.VehicleID != *wantVehicle {
		t.Errorf("the tracker points at %v, expected %v", tracker.VehicleID, *wantVehicle)
	}

	var vehicle models.Truck
	if err := db.Collection(models.Truck{}.CollectionName()).
		FindOne(ctx, bson.M{"_id": *wantVehicle}).Decode(&vehicle); err != nil {
		t.Fatalf("read vehicle: %v", err)
	}
	if vehicle.TrackerID == nil || *vehicle.TrackerID != trackerID {
		t.Errorf("the vehicle points at %v, expected %v", vehicle.TrackerID, trackerID)
	}

	var assignment models.TrackerAssignment
	err := db.Collection(models.TrackerAssignment{}.CollectionName()).
		FindOne(ctx, bson.M{"trackerId": trackerID, "unfittedAt": nil}).Decode(&assignment)
	if err != nil {
		t.Fatalf("no open assignment after fitting: %v", err)
	}
	if assignment.VehicleID != *wantVehicle {
		t.Errorf("the open assignment names %v, expected %v", assignment.VehicleID, *wantVehicle)
	}
	if assignment.IMEI != tracker.IMEI {
		t.Error("the assignment must carry the IMEI, so history survives the tracker record")
	}
}

// seedVehicle inserts a truck and returns its id.
func seedVehicle(t *testing.T, db *mongo.Database, company, plate string) primitive.ObjectID {
	t.Helper()
	ctx := context.Background()

	now := time.Now().UTC()
	res, err := db.Collection(models.Truck{}.CollectionName()).InsertOne(ctx, models.Truck{
		CompanyID:    company,
		PoliceNumber: plate,
		Status:       models.TruckStatusActive,
		DriverIDs:    []string{},
		CreatedAt:    now,
		UpdatedAt:    now,
	})
	if err != nil {
		t.Fatalf("seed vehicle %s: %v", plate, err)
	}
	id := res.InsertedID.(primitive.ObjectID)
	t.Cleanup(func() {
		db.Collection(models.Truck{}.CollectionName()).DeleteOne(ctx, bson.M{"_id": id})
	})
	return id
}
