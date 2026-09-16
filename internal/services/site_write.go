package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/repository"
)

// SiteInput is a loading or unloading point as a person describes one.
//
// Not a Site: the model stores a GeoJSON point and a normalised name, neither
// of which a form supplies, and accepting the model directly would let a caller
// write a document the indexes cannot see.
type SiteInput struct {
	Name string

	// SiteType is warehouse, depot or port. Defaults to warehouse — the
	// overwhelming majority, and the one the TMS contract calls a warehouse.
	SiteType string

	Address  string
	Street   string
	District string
	City     string
	Province string
	Postcode string

	// Latitude and Longitude are what the map picker produced. Both or
	// neither: half a coordinate is not a location.
	Latitude  *float64
	Longitude *float64

	// GeofenceRadiusM decides how close counts as "arrived". Per site because a
	// roadside drop needs a wider radius than a fenced yard.
	GeofenceRadiusM *int

	PICPhone string
	PICName  string
	Notes    string
	// CustomerCompanyID is the customer this site belongs to, or "" for
	// the company's own. Sent as "" on update to clear.
	CustomerCompanyID *string
}

// DefaultGeofenceRadiusM is used when a site does not set its own.
//
// 200 metres covers a yard and its gate without reaching the road outside. It
// is a default rather than a requirement because most sites do not care, and
// forcing the question at creation time gets it answered with a guess.
const DefaultGeofenceRadiusM = 200

// CreateSite records a loading or unloading point.
func (s *FleetService) CreateSite(ctx context.Context, companyID string, in SiteInput) (*Warehouse, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: a name is required", ErrValidation)
	}
	if companyID == "" {
		return nil, fmt.Errorf("%w: a site belongs to a company", ErrValidation)
	}

	site := &models.Site{
		CompanyID: companyID,
		Name:      strings.TrimSpace(in.Name),
		SiteType:  models.SiteType(in.SiteType),
		IsActive:  true,
	}
	setOptional(&site.Address, in.Address)
	setOptional(&site.Street, in.Street)
	setOptional(&site.District, in.District)
	setOptional(&site.City, in.City)
	setOptional(&site.Province, in.Province)
	setOptional(&site.Postcode, in.Postcode)
	setOptional(&site.SitePICPhone, in.PICPhone)
	setOptional(&site.Notes, in.Notes)
	if in.CustomerCompanyID != nil {
		site.CustomerCompanyID = nilIfBlank(*in.CustomerCompanyID)
	}

	point, err := geoPoint(in.Latitude, in.Longitude)
	if err != nil {
		return nil, err
	}
	site.Location = point

	radius := DefaultGeofenceRadiusM
	if in.GeofenceRadiusM != nil {
		if *in.GeofenceRadiusM <= 0 {
			return nil, fmt.Errorf("%w: a geofence radius must be positive", ErrValidation)
		}
		radius = *in.GeofenceRadiusM
	}
	site.GeofenceRadiusM = &radius

	site.ID = primitive.NewObjectID()

	if err := s.sites.Create(ctx, site); err != nil {
		if errors.Is(err, repository.ErrDuplicate) {
			return nil, fmt.Errorf("%w: your company already has a site called %q",
				ErrValidation, site.Name)
		}
		return nil, err
	}

	out := toWarehouse(*site)
	return &out, nil
}

// UpdateSite changes a site the company owns.
func (s *FleetService) UpdateSite(ctx context.Context, companyID, id string, in SiteInput) (*Warehouse, error) {
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return nil, ErrNotFound
	}

	var existing models.Site
	err = s.sites.Collection().FindOne(ctx, bson.M{
		"_id": oid, "companyId": companyID, "deleted": bson.M{"$ne": true},
	}).Decode(&existing)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	// A full replace, so BeforeWrite refills the normalised name. A $set that
	// changed the name without its twin would leave the unique index enforcing
	// uniqueness on a value nothing matches.
	if strings.TrimSpace(in.Name) != "" {
		existing.Name = strings.TrimSpace(in.Name)
	}
	if in.SiteType != "" {
		existing.SiteType = models.SiteType(in.SiteType)
	}
	setOptional(&existing.Address, in.Address)
	setOptional(&existing.Street, in.Street)
	setOptional(&existing.District, in.District)
	setOptional(&existing.City, in.City)
	setOptional(&existing.Province, in.Province)
	setOptional(&existing.Postcode, in.Postcode)
	setOptional(&existing.SitePICPhone, in.PICPhone)
	setOptional(&existing.Notes, in.Notes)

	if in.Latitude != nil || in.Longitude != nil {
		point, err := geoPoint(in.Latitude, in.Longitude)
		if err != nil {
			return nil, err
		}
		existing.Location = point
	}
	if in.GeofenceRadiusM != nil {
		if *in.GeofenceRadiusM <= 0 {
			return nil, fmt.Errorf("%w: a geofence radius must be positive", ErrValidation)
		}
		existing.GeofenceRadiusM = in.GeofenceRadiusM
	}

	if err := s.sites.Update(ctx, oid, &existing); err != nil {
		if errors.Is(err, repository.ErrDuplicate) {
			return nil, fmt.Errorf("%w: your company already has a site called %q",
				ErrValidation, existing.Name)
		}
		return nil, err
	}

	out := toWarehouse(existing)
	return &out, nil
}

// DeleteSite retires a site.
//
// Soft, like every other removal here: orders already naming this site keep
// resolving, and the name frees up because the unique index filters on
// `deleted`.
func (s *FleetService) DeleteSite(ctx context.Context, companyID, id string) error {
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return ErrNotFound
	}

	res, err := s.sites.Collection().UpdateOne(ctx,
		bson.M{"_id": oid, "companyId": companyID, "deleted": bson.M{"$ne": true}},
		bson.M{"$set": bson.M{"deleted": true, "updatedAt": time.Now().UTC()}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// geoPoint builds the GeoJSON a 2dsphere index can use.
//
// Coordinates are [longitude, latitude] — GeoJSON order, and the reverse of how
// people say it. Transposing them puts an Indonesian site in the Indian Ocean,
// which usually surfaces later as a routing failure rather than as a visibly
// wrong pin, so the bounds are checked here where the mistake is still cheap.
func geoPoint(lat, lon *float64) (*models.GeoPoint, error) {
	if lat == nil && lon == nil {
		// No coordinate is allowed: a site can be recorded before anyone has
		// stood in it with a phone. It simply cannot be routed to until it has
		// one, which the planner sees as a missing distance.
		return nil, nil
	}
	if lat == nil || lon == nil {
		return nil, fmt.Errorf("%w: a location needs both a latitude and a longitude", ErrValidation)
	}
	if *lat < -90 || *lat > 90 {
		return nil, fmt.Errorf("%w: latitude %.6f is out of range", ErrValidation, *lat)
	}
	if *lon < -180 || *lon > 180 {
		return nil, fmt.Errorf("%w: longitude %.6f is out of range", ErrValidation, *lon)
	}
	// Nought, nought is a real place in the Atlantic and never a site here. It
	// is what an unset form sends, so it is refused rather than stored.
	if *lat == 0 && *lon == 0 {
		return nil, fmt.Errorf("%w: pick a point on the map", ErrValidation)
	}

	return &models.GeoPoint{Type: "Point", Coordinates: []float64{*lon, *lat}}, nil
}

func setOptional(target **string, value string) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return
	}
	*target = &trimmed
}

func nilIfBlank(v string) *string {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
