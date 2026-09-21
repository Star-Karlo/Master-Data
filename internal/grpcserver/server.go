// Package grpcserver is master data's service-to-service surface.
//
// It exists for one caller today — the business service, which must resolve
// truck plates, warehouse coordinates and catalogue references while writing an
// order. Those reads are on the critical path of every order listing, so they
// go over gRPC rather than HTTP.
//
// The proto still speaks the OLD vocabulary: trucks, warehouses, and a
// catalogue addressed by `kind`. The services package translates onto the
// current model; this layer only converts shapes.
package grpcserver

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/karlo/masterdata-service/internal/models"
	commonv1 "github.com/karlo/masterdata-service/internal/platform/genproto/karlo/common/v1"
	masterdatav1 "github.com/karlo/masterdata-service/internal/platform/genproto/karlo/masterdata/v1"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/services"
)

// Server implements MasterDataService.
type Server struct {
	masterdatav1.UnimplementedMasterDataServiceServer

	catalog  *services.CatalogService
	fleet    *services.FleetService
	registry *services.RegistryService
}

func New(catalog *services.CatalogService, fleet *services.FleetService, registry *services.RegistryService) *Server {
	return &Server{catalog: catalog, fleet: fleet, registry: registry}
}

// kindNames maps the proto enum onto the names the services package uses.
// Kinds the model does not serve (rate cards, routes, FAQ, vacancies) stay
// unmapped and are refused by name rather than answered with a substitute.
var kindNames = map[masterdatav1.CatalogKind]string{
	masterdatav1.CatalogKind_CATALOG_KIND_BRAND:          "brand",
	masterdatav1.CatalogKind_CATALOG_KIND_CARGO_TYPE:     "cargoType",
	masterdatav1.CatalogKind_CATALOG_KIND_ITEM:           "item",
	masterdatav1.CatalogKind_CATALOG_KIND_ITEM_TYPE:      "itemType",
	masterdatav1.CatalogKind_CATALOG_KIND_ITEM_CHARACTER: "itemCharacter",
	masterdatav1.CatalogKind_CATALOG_KIND_TRUCK_HEAD:     "truckHead",
	masterdatav1.CatalogKind_CATALOG_KIND_TRUCK_BODY:     "truckBody",
	masterdatav1.CatalogKind_CATALOG_KIND_TRUCK_TYPE:     "truckType",
	masterdatav1.CatalogKind_CATALOG_KIND_PRICING_TYPE:   "pricingType",
	masterdatav1.CatalogKind_CATALOG_KIND_PAYMENT_TYPE:   "paymentType",
	masterdatav1.CatalogKind_CATALOG_KIND_CURRENCY:       "currency",
	masterdatav1.CatalogKind_CATALOG_KIND_REQUIREMENT:    "requirement",
	masterdatav1.CatalogKind_CATALOG_KIND_PROVINSI:       "provinsi",
	masterdatav1.CatalogKind_CATALOG_KIND_KOTA:           "kota",
	masterdatav1.CatalogKind_CATALOG_KIND_DISTRICT:       "district",
}

func kindName(k masterdatav1.CatalogKind) (string, error) {
	name, ok := kindNames[k]
	if !ok {
		return "", status.Errorf(codes.InvalidArgument,
			"catalogue %s is not served by this model", k)
	}
	return name, nil
}

func (s *Server) GetCatalogItem(ctx context.Context, req *masterdatav1.GetCatalogItemRequest) (*masterdatav1.GetCatalogItemResponse, error) {
	name, err := kindName(req.GetKind())
	if err != nil {
		return nil, err
	}

	// Never staff-exempt: this runs for the business service acting on ONE
	// company's behalf, so it must see exactly what that company sees.
	entry, err := s.catalog.Get(ctx, name, req.GetId(), req.GetCompanyId(), false)
	if err != nil {
		return nil, mapError(err)
	}
	return &masterdatav1.GetCatalogItemResponse{Item: toProtoItem(req.GetKind(), *entry)}, nil
}

func (s *Server) ListCatalogItems(ctx context.Context, req *masterdatav1.ListCatalogItemsRequest) (*masterdatav1.ListCatalogItemsResponse, error) {
	name, err := kindName(req.GetKind())
	if err != nil {
		return nil, err
	}

	p := query.FromProto(req.GetQuery(), catalogFields)
	if p.Err != nil {
		return nil, status.Error(codes.InvalidArgument, p.Err.Error())
	}

	entries, total, err := s.catalog.List(ctx, name, req.GetCompanyId(), req.GetParentId(), false, p)
	if err != nil {
		return nil, mapError(err)
	}

	items := make([]*masterdatav1.CatalogItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, toProtoItem(req.GetKind(), e))
	}

	return &masterdatav1.ListCatalogItemsResponse{
		Items:    items,
		PageInfo: pageInfo(p, total),
	}, nil
}

func (s *Server) ResolveCatalogItems(ctx context.Context, req *masterdatav1.ResolveCatalogItemsRequest) (*masterdatav1.ResolveCatalogItemsResponse, error) {
	byKind, kinds, err := groupRefs(req.GetRefs())
	if err != nil {
		return nil, err
	}

	resolved, err := s.catalog.Resolve(ctx, req.GetCompanyId(), byKind)
	if err != nil {
		return nil, mapError(err)
	}

	items := make([]*masterdatav1.CatalogItem, 0, len(resolved))
	for id, entry := range resolved {
		items = append(items, toProtoItem(kinds[id], entry))
	}
	return &masterdatav1.ResolveCatalogItemsResponse{Items: items}, nil
}

func (s *Server) ValidateReferences(ctx context.Context, req *masterdatav1.ValidateReferencesRequest) (*masterdatav1.ValidateReferencesResponse, error) {
	byKind, kinds, err := groupRefs(req.GetRefs())
	if err != nil {
		return nil, err
	}

	invalidIDs, err := s.catalog.Validate(ctx, req.GetCompanyId(), byKind)
	if err != nil {
		return nil, mapError(err)
	}

	// Returned as refs rather than bare ids so the caller can say WHICH
	// catalogue each failure was in. "cargo type 66f… does not exist" is
	// actionable; a list of hex strings is not.
	invalid := make([]*masterdatav1.CatalogRef, 0, len(invalidIDs))
	for _, id := range invalidIDs {
		invalid = append(invalid, &masterdatav1.CatalogRef{Kind: kinds[id], Id: id})
	}

	return &masterdatav1.ValidateReferencesResponse{
		Valid:   len(invalid) == 0,
		Invalid: invalid,
	}, nil
}

// groupRefs buckets references by catalogue and remembers each id's kind, so a
// resolved entry can be labelled with the kind it was asked for.
func groupRefs(refs []*masterdatav1.CatalogRef) (map[string][]string, map[string]masterdatav1.CatalogKind, error) {
	byKind := map[string][]string{}
	kinds := map[string]masterdatav1.CatalogKind{}

	for _, ref := range refs {
		name, err := kindName(ref.GetKind())
		if err != nil {
			return nil, nil, err
		}
		byKind[name] = append(byKind[name], ref.GetId())
		kinds[ref.GetId()] = ref.GetKind()
	}
	return byKind, kinds, nil
}

func (s *Server) GetTruck(ctx context.Context, req *masterdatav1.GetTruckRequest) (*masterdatav1.GetTruckResponse, error) {
	// No company scoping: the caller is another service that has already
	// established the caller's authority over the order this truck belongs to,
	// and it holds only the truck id.
	truck, err := s.fleet.GetTruck(ctx, "", req.GetId())
	if err != nil {
		return nil, mapError(err)
	}
	return &masterdatav1.GetTruckResponse{Truck: toProtoTruck(*truck)}, nil
}

func (s *Server) ListDrivers(ctx context.Context, req *masterdatav1.ListDriversRequest) (*masterdatav1.ListDriversResponse, error) {
	if req.GetCompanyId() == "" {
		return nil, status.Error(codes.InvalidArgument, "company_id is required")
	}
	page := int(req.GetPage())
	if page < 0 {
		page = 0
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = 200
	}
	if size > 1000 {
		size = 1000
	}
	var since *time.Time
	if req.GetUpdatedSince() != nil {
		t := req.GetUpdatedSince().AsTime()
		since = &t
	}
	rows, total, err := s.registry.ListDriversForSync(ctx, req.GetCompanyId(), page, size, since)
	if err != nil {
		return nil, mapError(err)
	}
	out := &masterdatav1.ListDriversResponse{Total: total, Drivers: make([]*masterdatav1.Driver, 0, len(rows))}
	for i := range rows {
		out.Drivers = append(out.Drivers, toProtoDriver(&rows[i]))
	}
	return out, nil
}

func toProtoDriver(d *models.Driver) *masterdatav1.Driver {
	out := &masterdatav1.Driver{
		Id:           d.ID.Hex(),
		CompanyId:    d.CompanyID,
		FullName:     d.FullName,
		Phone:        deref(d.Phone),
		EmployeeNo:   deref(d.EmployeeNo),
		LicenseNo:    deref(d.LicenseNo),
		LicenseClass: deref(d.LicenseClass),
		Status:       d.Status,
		UserId:       deref(d.UserID),
		Deleted:      d.Deleted,
		CreatedAt:    timestamppb.New(d.CreatedAt),
		UpdatedAt:    timestamppb.New(d.UpdatedAt),
	}
	if d.LicenseExpiry != nil {
		out.LicenseExpiry = timestamppb.New(*d.LicenseExpiry)
	}
	// The alias lives in attributes, where the import put it; it may be a
	// float after a JSON round trip.
	switch v := d.Attributes["fmsEmployeeId"].(type) {
	case int64:
		out.FmsEmployeeId = v
	case int32:
		out.FmsEmployeeId = int64(v)
	case int:
		out.FmsEmployeeId = int64(v)
	case float64:
		out.FmsEmployeeId = int64(v)
	}
	return out
}

func (s *Server) ListTrucks(ctx context.Context, req *masterdatav1.ListTrucksRequest) (*masterdatav1.ListTrucksResponse, error) {
	p := query.FromProto(req.GetQuery(), truckFields)
	if p.Err != nil {
		return nil, status.Error(codes.InvalidArgument, p.Err.Error())
	}

	// The availability filter arrives as an ordinary query filter rather than
	// a field on the request, so it is lifted out here — the service asks for
	// it as a flag because it selects a different index.
	availableOnly := false
	for _, f := range p.Filters {
		if f.Field == "isAvailable" && f.Value == "true" {
			availableOnly = true
		}
	}

	// Timed, because FMS's fleet projection pages through this per company
	// and a slow page shows up there as a hung sync with nothing to blame.
	started := time.Now()
	trucks, total, err := s.fleet.ListTrucks(ctx, req.GetCompanyId(), p, availableOnly)
	elapsed := time.Since(started)
	if err != nil {
		slog.WarnContext(ctx, "grpc ListTrucks failed", "company", req.GetCompanyId(), "page", p.Page, "pageSize", p.PageSize, "ms", elapsed.Milliseconds(), "error", err)
		return nil, mapError(err)
	}
	logLevel := slog.LevelDebug
	if elapsed > 2*time.Second {
		logLevel = slog.LevelWarn
	}
	slog.Log(ctx, logLevel, "grpc ListTrucks", "company", req.GetCompanyId(), "page", p.Page, "pageSize", p.PageSize, "rows", len(trucks), "total", total, "ms", elapsed.Milliseconds())

	out := make([]*masterdatav1.Truck, 0, len(trucks))
	for _, t := range trucks {
		out = append(out, toProtoTruck(t))
	}
	return &masterdatav1.ListTrucksResponse{Trucks: out, PageInfo: pageInfo(p, total)}, nil
}

func (s *Server) GetDriver(ctx context.Context, req *masterdatav1.GetDriverRequest) (*masterdatav1.GetDriverResponse, error) {
	// Service callers resolve by id alone: the business service holds the
	// driver id from its own order and needs the person behind it, whichever
	// company they work for.
	d, err := s.registry.GetDriverByID(ctx, req.GetId())
	if err != nil {
		return nil, mapError(err)
	}
	return &masterdatav1.GetDriverResponse{Driver: toProtoDriver(d)}, nil
}

func (s *Server) GetTrucksByDriver(ctx context.Context, req *masterdatav1.GetTrucksByDriverRequest) (*masterdatav1.GetTrucksByDriverResponse, error) {
	// The request carries only a driver id. The pairing is unique across
	// companies — a driver works for one — so scoping is unnecessary here and
	// an empty company means "wherever this driver is".
	trucks, err := s.fleet.TrucksByDriver(ctx, "", req.GetDriverId())
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]*masterdatav1.Truck, 0, len(trucks))
	for _, t := range trucks {
		out = append(out, toProtoTruck(t))
	}
	return &masterdatav1.GetTrucksByDriverResponse{Trucks: out}, nil
}

func (s *Server) GetWarehouse(ctx context.Context, req *masterdatav1.GetWarehouseRequest) (*masterdatav1.GetWarehouseResponse, error) {
	site, err := s.fleet.GetWarehouse(ctx, "", req.GetId())
	if err != nil {
		return nil, mapError(err)
	}
	return &masterdatav1.GetWarehouseResponse{Warehouse: toProtoWarehouse(*site)}, nil
}

func (s *Server) ListWarehouses(ctx context.Context, req *masterdatav1.ListWarehousesRequest) (*masterdatav1.ListWarehousesResponse, error) {
	p := query.FromProto(req.GetQuery(), warehouseFields)
	if p.Err != nil {
		return nil, status.Error(codes.InvalidArgument, p.Err.Error())
	}

	sites, total, err := s.fleet.ListWarehouses(ctx, req.GetCompanyId(), p)
	if err != nil {
		return nil, mapError(err)
	}

	out := make([]*masterdatav1.Warehouse, 0, len(sites))
	for _, w := range sites {
		out = append(out, toProtoWarehouse(w))
	}
	return &masterdatav1.ListWarehousesResponse{Warehouses: out, PageInfo: pageInfo(p, total)}, nil
}

// ---------------------------------------------------------------------------
// Shape conversion
// ---------------------------------------------------------------------------

func toProtoItem(kind masterdatav1.CatalogKind, e services.CatalogEntry) *masterdatav1.CatalogItem {
	item := &masterdatav1.CatalogItem{
		Id: e.ID, Kind: kind,
		Code: e.Code, Name: e.Name, Description: e.Desc, Active: e.Active,
	}
	if e.CompanyID != nil {
		item.CompanyId = *e.CompanyID
	}
	if attrs, err := structpb.NewStruct(e.Attrs); err == nil {
		// A conversion failure yields nil rather than an error: attributes are
		// supplementary, and losing them beats failing the whole read.
		item.Attributes = attrs
	}
	return item
}

func toProtoTruck(t services.Truck) *masterdatav1.Truck {
	return &masterdatav1.Truck{
		Id:            t.ID,
		CompanyId:     t.CompanyID,
		PoliceNumber:  t.PoliceNumber,
		TruckTypeId:   t.TruckTypeID,
		TruckHeadId:   t.TruckHeadID,
		TruckBodyId:   t.TruckBodyID,
		BrandId:       t.BrandID,
		Year:          int32(t.Year), //nolint:gosec // a model year cannot overflow
		ChassisNumber: t.ChassisNumber,
		EngineNumber:  t.EngineNumber,
		DriverIds:     t.DriverIDs,
		Status:        t.Status,
		IsAvailable:   t.IsAvailable,
		Imei:          t.IMEI,
		CreatedAt:     timestamppb.New(t.CreatedAt),
		UpdatedAt:     timestamppb.New(t.UpdatedAt),
	}
}

func toProtoWarehouse(w services.Warehouse) *masterdatav1.Warehouse {
	return &masterdatav1.Warehouse{
		Id:        w.ID,
		CompanyId: w.CompanyID,
		Name:      w.Name,
		Address:   w.Address,
		// CityId and ProvinceId carry NAMES in this model — there is no region
		// table, so an id here would reference nothing. The field names are the
		// proto's and predate that decision.
		CityId:               w.City,
		ProvinceId:           w.Province,
		Latitude:             w.Latitude,
		Longitude:            w.Longitude,
		GeofenceRadiusMeters: int32(w.GeofenceRadiusMeters), //nolint:gosec // metres, bounded
		PicPhone:             w.PICPhone,
		CreatedAt:            timestamppb.New(w.CreatedAt),
		UpdatedAt:            timestamppb.New(w.UpdatedAt),
	}
}

func pageInfo(p query.Params, total int64) *commonv1.PageInfo {
	pages := 0
	if p.PageSize > 0 {
		pages = int((total + int64(p.PageSize) - 1) / int64(p.PageSize))
	}
	return &commonv1.PageInfo{
		Page:       int32(p.Page),     //nolint:gosec // clamped by query.Parse
		PageSize:   int32(p.PageSize), //nolint:gosec // clamped by query.Parse
		TotalRows:  total,
		TotalPages: int32(pages), //nolint:gosec // derived from the clamped size
	}
}

func mapError(err error) error {
	switch {
	case err == nil:
		return nil
	case isNotFound(err):
		return status.Error(codes.NotFound, "not found")
	default:
		return status.Error(codes.Internal, "request failed")
	}
}

func isNotFound(err error) bool { return err == services.ErrNotFound }

// The gRPC allowlists mirror the HTTP ones. Kept here rather than exported from
// handlers so the two surfaces cannot silently diverge on what is filterable
// without someone noticing this file.
var (
	catalogFields = query.FieldSet{
		"name": "name", "code": "code", "isActive": "isActive", "createdAt": "createdAt",
	}
	truckFields = query.FieldSet{
		"policeNumber": "licensePlate", "status": "status",
		"isAvailable": "isAvailable", "createdAt": "createdAt",
	}
	warehouseFields = query.FieldSet{
		"name": "name", "city": "city", "createdAt": "createdAt",
	}
)

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
