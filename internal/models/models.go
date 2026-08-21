// Package models holds the MongoDB documents this service owns.
//
// The master data splits into two families. Global catalogues are shared by
// every company and edited by Karlo staff; they share one collection and one
// envelope, distinguished by `kind`. Company catalogues belong to exactly one
// company and get a collection each, because their shapes and access rules
// genuinely differ.
package models

import (
	"time"

	"github.com/karlo/masterdata-service/internal/config"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// CatalogKind identifies a global catalogue.
type CatalogKind string

// The global catalogues, matching the legacy src/models/Master/* files.
const (
	KindBrand              CatalogKind = "brand"
	KindCargoType          CatalogKind = "cargoType"
	KindCargoTruckCapacity CatalogKind = "cargoTruckCapacity"
	KindCurrency           CatalogKind = "currency"
	KindDistrict           CatalogKind = "district"
	KindItem               CatalogKind = "item"
	KindItemCharacter      CatalogKind = "itemCharacter"
	KindItemType           CatalogKind = "itemType"
	KindKota               CatalogKind = "kota"
	KindPaymentType        CatalogKind = "paymentType"
	KindPricingType        CatalogKind = "pricingType"
	KindProvinsi           CatalogKind = "provinsi"
	KindRateCard           CatalogKind = "rateCard"
	KindRequirement        CatalogKind = "requirement"
	KindRoute              CatalogKind = "route"
	KindTruckBody          CatalogKind = "truckBody"
	KindTruckHead          CatalogKind = "truckHead"
	KindTruckType          CatalogKind = "truckType"
	KindFaq                CatalogKind = "faq"
	KindJobVacancy         CatalogKind = "jobVacancy"
)

// AllCatalogKinds is the authoritative list, used to validate a requested kind
// and to drive the seed loader.
var AllCatalogKinds = []CatalogKind{
	KindBrand, KindCargoType, KindCargoTruckCapacity, KindCurrency, KindDistrict,
	KindItem, KindItemCharacter, KindItemType, KindKota, KindPaymentType,
	KindPricingType, KindProvinsi, KindRateCard, KindRequirement, KindRoute,
	KindTruckBody, KindTruckHead, KindTruckType, KindFaq, KindJobVacancy,
}

// IsValidKind reports whether a string names a real catalogue. Callers must
// check this before it reaches a query, since the kind selects what is read.
func IsValidKind(s string) bool {
	for _, k := range AllCatalogKinds {
		if string(k) == s {
			return true
		}
	}
	return false
}

// CatalogItem is one entry in a global catalogue.
//
// Everything catalogue-specific lives in Attributes. A rate card's tariff table
// and a province's ISO code have nothing in common, and promoting either to a
// column would leave the other with an empty field on every document.
type CatalogItem struct {
	ID   primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Kind CatalogKind        `bson:"kind" json:"kind"`

	// Code is the stable business identifier, unique within a kind. Orders
	// reference catalogue entries by id, but imports and integrations match on
	// code, so it must not change.
	Code        string `bson:"code" json:"code"`
	Name        string `bson:"name" json:"name"`
	Description string `bson:"description,omitempty" json:"description,omitempty"`
	Active      bool   `bson:"active" json:"active"`

	// ParentID links hierarchical catalogues: a city to its province, a
	// district to its city.
	ParentID *primitive.ObjectID `bson:"parentId,omitempty" json:"parentId,omitempty"`

	// SortOrder controls presentation where the list is not alphabetical, such
	// as truck sizes.
	SortOrder int `bson:"sortOrder" json:"sortOrder"`

	Attributes map[string]interface{} `bson:"attributes,omitempty" json:"attributes,omitempty"`

	CreatedAt time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt" json:"updatedAt"`
}

// CollectionName is the single collection holding every global catalogue.
func (CatalogItem) CollectionName() string { return config.Collection("catalog_items") }

// Truck is a company's vehicle.
type Truck struct {
	ID primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	// CompanyID is the tenant key. Every query in this service filters on it;
	// a query that does not is a data leak between companies.
	CompanyID string `bson:"companyId" json:"companyId"`

	PoliceNumber  string `bson:"policeNumber" json:"policeNumber"`
	ChassisNumber string `bson:"chassisNumber,omitempty" json:"chassisNumber,omitempty"`
	EngineNumber  string `bson:"engineNumber,omitempty" json:"engineNumber,omitempty"`
	Year          int    `bson:"year,omitempty" json:"year,omitempty"`

	// These reference CatalogItem ids.
	TruckTypeID *primitive.ObjectID `bson:"truckTypeId,omitempty" json:"truckTypeId,omitempty"`
	TruckHeadID *primitive.ObjectID `bson:"truckHeadId,omitempty" json:"truckHeadId,omitempty"`
	TruckBodyID *primitive.ObjectID `bson:"truckBodyId,omitempty" json:"truckBodyId,omitempty"`
	BrandID     *primitive.ObjectID `bson:"brandId,omitempty" json:"brandId,omitempty"`

	// DriverIDs are authentication-service user ids, held as strings because
	// they are UUIDs in another database, not ObjectIds here.
	DriverIDs    []string            `bson:"driverIds" json:"driverIds"`
	TruckGroupID *primitive.ObjectID `bson:"truckGroupId,omitempty" json:"truckGroupId,omitempty"`

	Status      string `bson:"status" json:"status"`
	IsAvailable bool   `bson:"isAvailable" json:"isAvailable"`

	// Documents holds STNK, KIR and insurance records with their expiry dates.
	Documents map[string]interface{} `bson:"documents,omitempty" json:"documents,omitempty"`

	Deleted   bool      `bson:"deleted" json:"deleted"`
	CreatedAt time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt" json:"updatedAt"`
}

func (Truck) CollectionName() string { return config.Collection("trucks") }

// Truck statuses.
const (
	TruckStatusActive      = "active"
	TruckStatusMaintenance = "maintenance"
	TruckStatusInactive    = "inactive"
)

// TruckGroup partitions a fleet, and drives which drivers see which offers.
type TruckGroup struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CompanyID string             `bson:"companyId" json:"companyId"`
	Name      string             `bson:"name" json:"name"`
	Code      string             `bson:"code,omitempty" json:"code,omitempty"`
	// ManagerIDs are the users who administer this group.
	ManagerIDs []string  `bson:"managerIds" json:"managerIds"`
	Deleted    bool      `bson:"deleted" json:"deleted"`
	CreatedAt  time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt  time.Time `bson:"updatedAt" json:"updatedAt"`
}

func (TruckGroup) CollectionName() string { return config.Collection("truck_groups") }

// GeoPoint is a GeoJSON point, so Mongo's geospatial queries work directly.
type GeoPoint struct {
	Type string `bson:"type" json:"type"`
	// Coordinates are [longitude, latitude], which is GeoJSON order and the
	// reverse of how people usually say it.
	Coordinates []float64 `bson:"coordinates" json:"coordinates"`
}

// NewGeoPoint builds a point from latitude and longitude given in the usual
// spoken order, so callers cannot transpose them by accident.
func NewGeoPoint(lat, lng float64) GeoPoint {
	return GeoPoint{Type: "Point", Coordinates: []float64{lng, lat}}
}

// Lat returns the latitude.
func (g GeoPoint) Lat() float64 {
	if len(g.Coordinates) < 2 {
		return 0
	}
	return g.Coordinates[1]
}

// Lng returns the longitude.
func (g GeoPoint) Lng() float64 {
	if len(g.Coordinates) < 1 {
		return 0
	}
	return g.Coordinates[0]
}

// Warehouse is a loading or unloading location.
type Warehouse struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CompanyID string             `bson:"companyId" json:"companyId"`

	Name       string              `bson:"name" json:"name"`
	Code       string              `bson:"code,omitempty" json:"code,omitempty"`
	Address    string              `bson:"address" json:"address"`
	CityID     *primitive.ObjectID `bson:"cityId,omitempty" json:"cityId,omitempty"`
	ProvinceID *primitive.ObjectID `bson:"provinceId,omitempty" json:"provinceId,omitempty"`
	PostalCode string              `bson:"postalCode,omitempty" json:"postalCode,omitempty"`

	Location GeoPoint `bson:"location" json:"location"`
	// GeofenceRadius is in metres. The business service compares a driver's
	// reported position against it when a company enables geofenced completion.
	GeofenceRadius int `bson:"geofenceRadius" json:"geofenceRadius"`

	PICName  string `bson:"picName,omitempty" json:"picName,omitempty"`
	PICPhone string `bson:"picPhone,omitempty" json:"picPhone,omitempty"`

	// OperatingHours records opening times per weekday.
	OperatingHours map[string]interface{} `bson:"operatingHours,omitempty" json:"operatingHours,omitempty"`

	Deleted   bool      `bson:"deleted" json:"deleted"`
	CreatedAt time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt" json:"updatedAt"`
}

func (Warehouse) CollectionName() string { return config.Collection("warehouses") }

// DefaultGeofenceRadius is used when a warehouse does not set one.
const DefaultGeofenceRadius = 200

// Customer is a company's own client register, used on orders and invoices.
type Customer struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CompanyID string             `bson:"companyId" json:"companyId"`

	Name         string `bson:"name" json:"name"`
	Code         string `bson:"code,omitempty" json:"code,omitempty"`
	NPWP         string `bson:"npwp,omitempty" json:"npwp,omitempty"`
	Address      string `bson:"address,omitempty" json:"address,omitempty"`
	ContactName  string `bson:"contactName,omitempty" json:"contactName,omitempty"`
	ContactPhone string `bson:"contactPhone,omitempty" json:"contactPhone,omitempty"`
	ContactEmail string `bson:"contactEmail,omitempty" json:"contactEmail,omitempty"`

	Deleted   bool      `bson:"deleted" json:"deleted"`
	CreatedAt time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt" json:"updatedAt"`
}

func (Customer) CollectionName() string { return config.Collection("customers") }

// Point is a named waypoint: a rest stop, a weighbridge, a checkpoint.
type Point struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CompanyID string             `bson:"companyId" json:"companyId"`

	Name     string   `bson:"name" json:"name"`
	Type     string   `bson:"type" json:"type"`
	Location GeoPoint `bson:"location" json:"location"`
	Radius   int      `bson:"radius" json:"radius"`

	CreatedAt time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt" json:"updatedAt"`
}

func (Point) CollectionName() string { return config.Collection("points") }

// SavedRoute is a reusable origin-destination pair with its planned path.
type SavedRoute struct {
	ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	CompanyID string             `bson:"companyId" json:"companyId"`

	Name          string              `bson:"name" json:"name"`
	OriginID      *primitive.ObjectID `bson:"originId,omitempty" json:"originId,omitempty"`
	DestinationID *primitive.ObjectID `bson:"destinationId,omitempty" json:"destinationId,omitempty"`

	DistanceMeters  int `bson:"distanceMeters" json:"distanceMeters"`
	DurationSeconds int `bson:"durationSeconds" json:"durationSeconds"`

	// Waypoints is the encoded polyline of the planned route.
	Waypoints string  `bson:"waypoints,omitempty" json:"waypoints,omitempty"`
	TollCost  float64 `bson:"tollCost,omitempty" json:"tollCost,omitempty"`

	CreatedAt time.Time `bson:"createdAt" json:"createdAt"`
	UpdatedAt time.Time `bson:"updatedAt" json:"updatedAt"`
}

func (SavedRoute) CollectionName() string { return config.Collection("saved_routes") }
