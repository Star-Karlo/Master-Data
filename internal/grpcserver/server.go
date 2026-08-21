// Package grpcserver implements the MasterDataService contract.
package grpcserver

import (
	"context"
	"errors"

	"github.com/karlo/masterdata-service/internal/models"
	masterdatav1 "github.com/karlo/masterdata-service/internal/platform/genproto/karlo/masterdata/v1"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/platform/safeconv"
	"github.com/karlo/masterdata-service/internal/repository"
	"github.com/karlo/masterdata-service/internal/services"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server implements masterdatav1.MasterDataServiceServer.
type Server struct {
	masterdatav1.UnimplementedMasterDataServiceServer

	catalog *services.CatalogService
	fleet   *services.FleetService
}

func New(catalog *services.CatalogService, fleet *services.FleetService) *Server {
	return &Server{catalog: catalog, fleet: fleet}
}

func (s *Server) GetCatalogItem(ctx context.Context, req *masterdatav1.GetCatalogItemRequest) (*masterdatav1.GetCatalogItemResponse, error) {
	kind, err := kindFromProto(req.GetKind())
	if err != nil {
		return nil, err
	}
	id, err := primitive.ObjectIDFromHex(req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed item id")
	}

	item, err := s.catalog.Get(ctx, string(kind), id)
	if err != nil {
		return nil, mapError(err, "catalogue entry")
	}

	return &masterdatav1.GetCatalogItemResponse{Item: toProtoCatalogItem(item)}, nil
}

func (s *Server) ListCatalogItems(ctx context.Context, req *masterdatav1.ListCatalogItemsRequest) (*masterdatav1.ListCatalogItemsResponse, error) {
	kind, err := kindFromProto(req.GetKind())
	if err != nil {
		return nil, err
	}

	var parentID *primitive.ObjectID
	if raw := req.GetParentId(); raw != "" {
		id, perr := primitive.ObjectIDFromHex(raw)
		if perr != nil {
			return nil, status.Error(codes.InvalidArgument, "malformed parent id")
		}
		parentID = &id
	}

	params := query.FromProto(req.GetQuery(), repository.CatalogFields())

	items, total, err := s.catalog.List(ctx, string(kind), parentID, params)
	if err != nil {
		return nil, mapError(err, "catalogue")
	}

	out := make([]*masterdatav1.CatalogItem, 0, len(items))
	for i := range items {
		out = append(out, toProtoCatalogItem(&items[i]))
	}

	return &masterdatav1.ListCatalogItemsResponse{
		Items:    out,
		PageInfo: params.PageInfo(total),
	}, nil
}

// ResolveCatalogItems batch-resolves references. Malformed refs are dropped
// rather than failing the call: the caller is denormalising a page of rows, and
// one bad id should not cost them the other forty.
func (s *Server) ResolveCatalogItems(ctx context.Context, req *masterdatav1.ResolveCatalogItemsRequest) (*masterdatav1.ResolveCatalogItemsResponse, error) {
	refs := decodeRefs(req.GetRefs())
	if len(refs) == 0 {
		return &masterdatav1.ResolveCatalogItemsResponse{}, nil
	}

	items, err := s.catalog.Resolve(ctx, refs)
	if err != nil {
		return nil, mapError(err, "catalogue")
	}

	out := make([]*masterdatav1.CatalogItem, 0, len(items))
	for i := range items {
		out = append(out, toProtoCatalogItem(&items[i]))
	}
	return &masterdatav1.ResolveCatalogItemsResponse{Items: out}, nil
}

// ValidateReferences reports which references do not resolve.
//
// A malformed id counts as invalid rather than being ignored: this RPC exists
// to gate a write, so anything it cannot vouch for must be reported.
func (s *Server) ValidateReferences(ctx context.Context, req *masterdatav1.ValidateReferencesRequest) (*masterdatav1.ValidateReferencesResponse, error) {
	var (
		refs      []repository.CatalogRef
		malformed []*masterdatav1.CatalogRef
	)

	for _, r := range req.GetRefs() {
		kind, err := kindFromProto(r.GetKind())
		if err != nil {
			malformed = append(malformed, r)
			continue
		}
		id, err := primitive.ObjectIDFromHex(r.GetId())
		if err != nil {
			malformed = append(malformed, r)
			continue
		}
		refs = append(refs, repository.CatalogRef{Kind: kind, ID: id})
	}

	invalid, err := s.catalog.Validate(ctx, refs)
	if err != nil {
		return nil, mapError(err, "catalogue")
	}

	out := append([]*masterdatav1.CatalogRef{}, malformed...)
	for _, ref := range invalid {
		out = append(out, &masterdatav1.CatalogRef{
			Kind: kindToProto(ref.Kind),
			Id:   ref.ID.Hex(),
		})
	}

	return &masterdatav1.ValidateReferencesResponse{
		Valid:   len(out) == 0,
		Invalid: out,
	}, nil
}

func (s *Server) GetTruck(ctx context.Context, req *masterdatav1.GetTruckRequest) (*masterdatav1.GetTruckResponse, error) {
	id, err := primitive.ObjectIDFromHex(req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed truck id")
	}

	truck, err := s.fleet.GetTruckForService(ctx, id)
	if err != nil {
		return nil, mapError(err, "truck")
	}

	return &masterdatav1.GetTruckResponse{Truck: toProtoTruck(truck)}, nil
}

func (s *Server) ListTrucks(ctx context.Context, req *masterdatav1.ListTrucksRequest) (*masterdatav1.ListTrucksResponse, error) {
	// The company id is required here, unlike GetTruck: a listing without a
	// tenant filter would return every company's fleet.
	if req.GetCompanyId() == "" {
		return nil, status.Error(codes.InvalidArgument, "company_id is required")
	}

	params := query.FromProto(req.GetQuery(), repository.TruckFields())

	trucks, total, err := s.fleet.ListTrucks(ctx, req.GetCompanyId(), params)
	if err != nil {
		return nil, mapError(err, "trucks")
	}

	out := make([]*masterdatav1.Truck, 0, len(trucks))
	for i := range trucks {
		out = append(out, toProtoTruck(&trucks[i]))
	}

	return &masterdatav1.ListTrucksResponse{
		Trucks:   out,
		PageInfo: params.PageInfo(total),
	}, nil
}

func (s *Server) GetTrucksByDriver(ctx context.Context, req *masterdatav1.GetTrucksByDriverRequest) (*masterdatav1.GetTrucksByDriverResponse, error) {
	if req.GetDriverId() == "" {
		return nil, status.Error(codes.InvalidArgument, "driver_id is required")
	}

	trucks, err := s.fleet.TrucksByDriver(ctx, req.GetDriverId())
	if err != nil {
		return nil, mapError(err, "trucks")
	}

	out := make([]*masterdatav1.Truck, 0, len(trucks))
	for i := range trucks {
		out = append(out, toProtoTruck(&trucks[i]))
	}
	return &masterdatav1.GetTrucksByDriverResponse{Trucks: out}, nil
}

func (s *Server) GetWarehouse(ctx context.Context, req *masterdatav1.GetWarehouseRequest) (*masterdatav1.GetWarehouseResponse, error) {
	id, err := primitive.ObjectIDFromHex(req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed warehouse id")
	}

	w, err := s.fleet.GetWarehouseForService(ctx, id)
	if err != nil {
		return nil, mapError(err, "warehouse")
	}

	return &masterdatav1.GetWarehouseResponse{Warehouse: toProtoWarehouse(w)}, nil
}

func (s *Server) ListWarehouses(ctx context.Context, req *masterdatav1.ListWarehousesRequest) (*masterdatav1.ListWarehousesResponse, error) {
	if req.GetCompanyId() == "" {
		return nil, status.Error(codes.InvalidArgument, "company_id is required")
	}

	params := query.FromProto(req.GetQuery(), repository.WarehouseFields())

	items, total, err := s.fleet.ListWarehouses(ctx, req.GetCompanyId(), params)
	if err != nil {
		return nil, mapError(err, "warehouses")
	}

	out := make([]*masterdatav1.Warehouse, 0, len(items))
	for i := range items {
		out = append(out, toProtoWarehouse(&items[i]))
	}

	return &masterdatav1.ListWarehousesResponse{
		Warehouses: out,
		PageInfo:   params.PageInfo(total),
	}, nil
}

// ---------------------------------------------------------------------------
// Mapping
// ---------------------------------------------------------------------------

// kindMapping is the single source of truth for the enum-to-string mapping, so
// the two directions cannot disagree.
var kindMapping = map[masterdatav1.CatalogKind]models.CatalogKind{
	masterdatav1.CatalogKind_CATALOG_KIND_BRAND:                models.KindBrand,
	masterdatav1.CatalogKind_CATALOG_KIND_CARGO_TYPE:           models.KindCargoType,
	masterdatav1.CatalogKind_CATALOG_KIND_CARGO_TRUCK_CAPACITY: models.KindCargoTruckCapacity,
	masterdatav1.CatalogKind_CATALOG_KIND_CURRENCY:             models.KindCurrency,
	masterdatav1.CatalogKind_CATALOG_KIND_DISTRICT:             models.KindDistrict,
	masterdatav1.CatalogKind_CATALOG_KIND_ITEM:                 models.KindItem,
	masterdatav1.CatalogKind_CATALOG_KIND_ITEM_CHARACTER:       models.KindItemCharacter,
	masterdatav1.CatalogKind_CATALOG_KIND_ITEM_TYPE:            models.KindItemType,
	masterdatav1.CatalogKind_CATALOG_KIND_KOTA:                 models.KindKota,
	masterdatav1.CatalogKind_CATALOG_KIND_PAYMENT_TYPE:         models.KindPaymentType,
	masterdatav1.CatalogKind_CATALOG_KIND_PRICING_TYPE:         models.KindPricingType,
	masterdatav1.CatalogKind_CATALOG_KIND_PROVINSI:             models.KindProvinsi,
	masterdatav1.CatalogKind_CATALOG_KIND_RATE_CARD:            models.KindRateCard,
	masterdatav1.CatalogKind_CATALOG_KIND_REQUIREMENT:          models.KindRequirement,
	masterdatav1.CatalogKind_CATALOG_KIND_ROUTE:                models.KindRoute,
	masterdatav1.CatalogKind_CATALOG_KIND_TRUCK_BODY:           models.KindTruckBody,
	masterdatav1.CatalogKind_CATALOG_KIND_TRUCK_HEAD:           models.KindTruckHead,
	masterdatav1.CatalogKind_CATALOG_KIND_TRUCK_TYPE:           models.KindTruckType,
	masterdatav1.CatalogKind_CATALOG_KIND_FAQ:                  models.KindFaq,
	masterdatav1.CatalogKind_CATALOG_KIND_JOB_VACANCY:          models.KindJobVacancy,
}

var reverseKindMapping = func() map[models.CatalogKind]masterdatav1.CatalogKind {
	out := make(map[models.CatalogKind]masterdatav1.CatalogKind, len(kindMapping))
	for k, v := range kindMapping {
		out[v] = k
	}
	return out
}()

func kindFromProto(k masterdatav1.CatalogKind) (models.CatalogKind, error) {
	kind, ok := kindMapping[k]
	if !ok {
		return "", status.Errorf(codes.InvalidArgument, "unknown catalogue kind %s", k)
	}
	return kind, nil
}

func kindToProto(k models.CatalogKind) masterdatav1.CatalogKind {
	if v, ok := reverseKindMapping[k]; ok {
		return v
	}
	return masterdatav1.CatalogKind_CATALOG_KIND_UNSPECIFIED
}

func decodeRefs(in []*masterdatav1.CatalogRef) []repository.CatalogRef {
	out := make([]repository.CatalogRef, 0, len(in))
	for _, r := range in {
		kind, err := kindFromProto(r.GetKind())
		if err != nil {
			continue
		}
		id, err := primitive.ObjectIDFromHex(r.GetId())
		if err != nil {
			continue
		}
		out = append(out, repository.CatalogRef{Kind: kind, ID: id})
	}
	return out
}

func toProtoCatalogItem(item *models.CatalogItem) *masterdatav1.CatalogItem {
	if item == nil {
		return nil
	}
	return &masterdatav1.CatalogItem{
		Id:          item.ID.Hex(),
		Kind:        kindToProto(item.Kind),
		Code:        item.Code,
		Name:        item.Name,
		Description: item.Description,
		Active:      item.Active,
		Attributes:  toStruct(item.Attributes),
		CreatedAt:   timestamppb.New(item.CreatedAt),
		UpdatedAt:   timestamppb.New(item.UpdatedAt),
	}
}

func toProtoTruck(t *models.Truck) *masterdatav1.Truck {
	if t == nil {
		return nil
	}
	return &masterdatav1.Truck{
		Id:            t.ID.Hex(),
		CompanyId:     t.CompanyID,
		PoliceNumber:  t.PoliceNumber,
		TruckTypeId:   hexOrEmpty(t.TruckTypeID),
		TruckHeadId:   hexOrEmpty(t.TruckHeadID),
		TruckBodyId:   hexOrEmpty(t.TruckBodyID),
		BrandId:       hexOrEmpty(t.BrandID),
		Year:          safeconv.NonNegativeInt32(t.Year),
		ChassisNumber: t.ChassisNumber,
		EngineNumber:  t.EngineNumber,
		DriverIds:     t.DriverIDs,
		TruckGroupId:  hexOrEmpty(t.TruckGroupID),
		Status:        t.Status,
		IsAvailable:   t.IsAvailable,
		Documents:     toStruct(t.Documents),
		Deleted:       t.Deleted,
		CreatedAt:     timestamppb.New(t.CreatedAt),
		UpdatedAt:     timestamppb.New(t.UpdatedAt),
	}
}

func toProtoWarehouse(w *models.Warehouse) *masterdatav1.Warehouse {
	if w == nil {
		return nil
	}
	return &masterdatav1.Warehouse{
		Id:                   w.ID.Hex(),
		CompanyId:            w.CompanyID,
		Name:                 w.Name,
		Address:              w.Address,
		CityId:               hexOrEmpty(w.CityID),
		ProvinceId:           hexOrEmpty(w.ProvinceID),
		Latitude:             w.Location.Lat(),
		Longitude:            w.Location.Lng(),
		GeofenceRadiusMeters: safeconv.NonNegativeInt32(w.GeofenceRadius),
		PicName:              w.PICName,
		PicPhone:             w.PICPhone,
		Deleted:              w.Deleted,
		CreatedAt:            timestamppb.New(w.CreatedAt),
		UpdatedAt:            timestamppb.New(w.UpdatedAt),
	}
}

// toStruct converts a free-form map. A conversion failure yields nil rather
// than an error: attributes are supplementary, and losing them is better than
// failing the whole read.
func toStruct(m map[string]interface{}) *structpb.Struct {
	if len(m) == 0 {
		return nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

func hexOrEmpty(id *primitive.ObjectID) string {
	if id == nil {
		return ""
	}
	return id.Hex()
}

func mapError(err error, subject string) error {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return status.Errorf(codes.NotFound, "%s not found", subject)
	case errors.Is(err, services.ErrInvalidKind), errors.Is(err, services.ErrValidation):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Errorf(codes.Internal, "failed to load %s", subject)
	}
}
