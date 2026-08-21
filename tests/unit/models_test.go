package unit

import (
	"strings"
	"testing"

	"github.com/karlo/masterdata-service/internal/models"
)

func TestGeoPointOrdering(t *testing.T) {
	// GeoJSON stores [longitude, latitude], which is the reverse of how people
	// state coordinates. NewGeoPoint takes them the spoken way round, so this
	// test pins the transposition that would otherwise be easy to get wrong.
	const (
		jakartaLat = -6.2088
		jakartaLng = 106.8456
	)

	p := models.NewGeoPoint(jakartaLat, jakartaLng)

	if p.Type != "Point" {
		t.Errorf("Type = %q, want %q", p.Type, "Point")
	}
	if len(p.Coordinates) != 2 {
		t.Fatalf("expected 2 coordinates, got %d", len(p.Coordinates))
	}
	if p.Coordinates[0] != jakartaLng {
		t.Errorf("stored coordinates[0] = %v, want the longitude %v", p.Coordinates[0], jakartaLng)
	}
	if p.Coordinates[1] != jakartaLat {
		t.Errorf("stored coordinates[1] = %v, want the latitude %v", p.Coordinates[1], jakartaLat)
	}
	if p.Lat() != jakartaLat {
		t.Errorf("Lat() = %v, want %v", p.Lat(), jakartaLat)
	}
	if p.Lng() != jakartaLng {
		t.Errorf("Lng() = %v, want %v", p.Lng(), jakartaLng)
	}
}

func TestGeoPointAccessorsOnMalformedData(t *testing.T) {
	// A document written by something else may not carry both coordinates.
	// The accessors must not panic on it.
	empty := models.GeoPoint{Type: "Point"}
	if empty.Lat() != 0 || empty.Lng() != 0 {
		t.Error("accessors on an empty point should return zero")
	}

	partial := models.GeoPoint{Type: "Point", Coordinates: []float64{1}}
	if partial.Lng() != 1 {
		t.Errorf("Lng() = %v, want 1", partial.Lng())
	}
	if partial.Lat() != 0 {
		t.Errorf("Lat() on a one-element point = %v, want 0", partial.Lat())
	}
}

func TestIsValidKind(t *testing.T) {
	for _, kind := range models.AllCatalogKinds {
		if !models.IsValidKind(string(kind)) {
			t.Errorf("IsValidKind(%q) = false for a declared kind", kind)
		}
	}

	// The kind selects which data is read, so an unknown one must be rejected
	// before it reaches a query.
	for _, bad := range []string{"", "users", "orders", "TRUCKTYPE", "../etc"} {
		if models.IsValidKind(bad) {
			t.Errorf("IsValidKind(%q) = true for an unknown kind", bad)
		}
	}
}

func TestCollectionNamesArePrefixed(t *testing.T) {
	names := []string{
		models.CatalogItem{}.CollectionName(),
		models.Truck{}.CollectionName(),
		models.Warehouse{}.CollectionName(),
		models.Customer{}.CollectionName(),
		models.Point{}.CollectionName(),
		models.TruckGroup{}.CollectionName(),
		models.SavedRoute{}.CollectionName(),
	}

	for _, name := range names {
		if !strings.HasPrefix(name, "md_") {
			t.Errorf("collection %q is missing the md_ prefix that keeps it clear of legacy collections", name)
		}
	}
}
