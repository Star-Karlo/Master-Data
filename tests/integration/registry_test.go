//go:build integration

package integration

import (
	"errors"
	"testing"

	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/services"
)

const (
	companyA = "11111111-1111-1111-1111-111111111111"
	companyB = "22222222-2222-2222-2222-222222222222"
)

func str(s string) *string { return &s }

// A company sees the shared catalogue plus its own entries, and never
// another company's — the rule that keeps one client's customers out of
// another's pickers.
func TestCatalogueScopingKeepsCompaniesApart(t *testing.T) {
	db := testDB(t)
	cat := services.NewCatalogService(db)

	if _, err := cat.Create(ctx(), "brand", "", true, map[string]any{"name": "Hino"}); err != nil {
		t.Fatalf("staff create global: %v", err)
	}
	if _, err := cat.Create(ctx(), "brand", companyA, false, map[string]any{"name": "Karoseri A"}); err != nil {
		t.Fatalf("A create: %v", err)
	}
	if _, err := cat.Create(ctx(), "brand", companyB, false, map[string]any{"name": "Karoseri B"}); err != nil {
		t.Fatalf("B create: %v", err)
	}

	p := query.Params{Page: 0, PageSize: 50}
	names := func(companyID string) map[string]bool {
		rows, _, err := cat.List(ctx(), "brand", companyID, "", false, p)
		if err != nil {
			t.Fatalf("list for %s: %v", companyID, err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.Name] = true
		}
		return out
	}
	a, b := names(companyA), names(companyB)
	if !a["Hino"] || !a["Karoseri A"] || a["Karoseri B"] {
		t.Errorf("A sees %v; want the shared entry and its own only", a)
	}
	if !b["Hino"] || !b["Karoseri B"] || b["Karoseri A"] {
		t.Errorf("B sees %v; want the shared entry and its own only", b)
	}
}

// A fitting is one operation across three records: the device leaves where
// it was, the vehicle's previous device of the same kind comes off, the
// vehicle's trackerId follows the GPS unit — and a dashcam never touches it.
func TestFitUnfitKeepsThreeRecordsInStep(t *testing.T) {
	db := testDB(t)
	reg := services.NewRegistryService(db)

	v1, err := reg.CreateVehicle(ctx(), companyA, services.VehicleInput{LicensePlate: str("B 1 TST")})
	if err != nil {
		t.Fatalf("vehicle: %v", err)
	}
	v2, err := reg.CreateVehicle(ctx(), companyA, services.VehicleInput{LicensePlate: str("B 2 TST")})
	if err != nil {
		t.Fatalf("vehicle: %v", err)
	}
	gps1, err := reg.CreateTracker(ctx(), companyA, false, services.TrackerInput{DeviceID: str("861234567890001")})
	if err != nil {
		t.Fatalf("gps1: %v", err)
	}
	gps2, err := reg.CreateTracker(ctx(), companyA, false, services.TrackerInput{DeviceID: str("861234567890002")})
	if err != nil {
		t.Fatalf("gps2: %v", err)
	}
	cam, err := reg.CreateTracker(ctx(), companyA, false, services.TrackerInput{Kind: str("dashcam"), DeviceID: str("VSS-TEST-1")})
	if err != nil {
		t.Fatalf("dashcam: %v", err)
	}

	fit := func(tracker, vehicle string) {
		t.Helper()
		if _, err := reg.Fit(ctx(), companyA, tracker, services.FitInput{VehicleID: vehicle}); err != nil {
			t.Fatalf("fit %s → %s: %v", tracker, vehicle, err)
		}
	}
	trackerOf := func(vehicle string) string {
		t.Helper()
		v, err := reg.GetVehicle(ctx(), companyA, vehicle)
		if err != nil {
			t.Fatalf("get vehicle: %v", err)
		}
		if v.TrackerID == nil {
			return ""
		}
		return *v.TrackerID
	}

	fit(gps1.ID.Hex(), v1.ID.Hex())
	fit(cam.ID.Hex(), v1.ID.Hex())
	if got := trackerOf(v1.ID.Hex()); got != gps1.ID.Hex() {
		t.Fatalf("after fitting a dashcam, v1.trackerId = %q; want the GPS unit %q", got, gps1.ID.Hex())
	}

	fit(gps2.ID.Hex(), v1.ID.Hex()) // replaces gps1 on v1
	if got := trackerOf(v1.ID.Hex()); got != gps2.ID.Hex() {
		t.Fatalf("after replacing the GPS unit, v1.trackerId = %q; want %q", got, gps2.ID.Hex())
	}
	fit(gps1.ID.Hex(), v2.ID.Hex()) // gps1 moves to v2
	if got := trackerOf(v2.ID.Hex()); got != gps1.ID.Hex() {
		t.Fatalf("v2.trackerId = %q; want %q", got, gps1.ID.Hex())
	}

	// One open fitting per (vehicle, kind).
	onV1, _, err := reg.ListTrackers(ctx(), companyA, query.Params{PageSize: 50}, "", nil, v1.ID.Hex())
	if err != nil {
		t.Fatalf("list on v1: %v", err)
	}
	kinds := map[string]int{}
	for _, tr := range onV1 {
		kinds[string(tr.Kind)]++
	}
	if kinds["gps"] != 1 || kinds["dashcam"] != 1 {
		t.Fatalf("v1 carries %v; want one gps and one dashcam", kinds)
	}

	// A vehicle with a device fitted cannot be retired.
	if err := reg.DeleteVehicle(ctx(), companyA, v1.ID.Hex()); !errors.Is(err, services.ErrConflict) {
		t.Fatalf("delete fitted vehicle: got %v, want ErrConflict", err)
	}
	if err := reg.Unfit(ctx(), companyA, gps2.ID.Hex(), services.UnfitInput{}); err != nil {
		t.Fatalf("unfit: %v", err)
	}
	if got := trackerOf(v1.ID.Hex()); got != "" {
		t.Fatalf("after unfit, v1.trackerId = %q; want empty", got)
	}
}
