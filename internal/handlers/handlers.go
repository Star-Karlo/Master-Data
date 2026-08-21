// Package handlers exposes the master data service over HTTP.
package handlers

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/authctx"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/platform/response"
	"github.com/karlo/masterdata-service/internal/repository"
	"github.com/karlo/masterdata-service/internal/services"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// CatalogHandler serves the global catalogues.
type CatalogHandler struct {
	catalog *services.CatalogService
}

func NewCatalogHandler(catalog *services.CatalogService) *CatalogHandler {
	return &CatalogHandler{catalog: catalog}
}

// List pages one catalogue.
//
// @Summary  List catalogue entries
// @Tags     Catalog
// @Security BearerAuth
// @Param    kind path string true "Catalogue kind"
// @Success  200 {object} response.Meta
// @Router   /catalog/{kind} [get]
func (h *CatalogHandler) List(c *gin.Context) {
	kind := c.Param("kind")

	var parentID *primitive.ObjectID
	if raw := c.Query("parentId"); raw != "" {
		id, err := primitive.ObjectIDFromHex(raw)
		if err != nil {
			response.BadRequest(c, "Invalid parentId")
			return
		}
		parentID = &id
	}

	params := parseQuery(c, repository.CatalogFields())

	items, total, err := h.catalog.List(c.Request.Context(), kind, parentID, params)
	if err != nil {
		writeError(c, err)
		return
	}

	response.Paginated(c, items, meta(params, total))
}

// Get resolves one catalogue entry.
//
// @Summary  Get catalogue entry
// @Tags     Catalog
// @Security BearerAuth
// @Param    kind path string true "Catalogue kind"
// @Param    id   path string true "Entry ID"
// @Success  200 {object} models.CatalogItem
// @Router   /catalog/{kind}/{id} [get]
func (h *CatalogHandler) Get(c *gin.Context) {
	id, ok := pathObjectID(c)
	if !ok {
		return
	}

	item, err := h.catalog.Get(c.Request.Context(), c.Param("kind"), id)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, item)
}

// Upsert creates or replaces a catalogue entry. Platform staff only; the route
// carries the role check.
//
// @Summary  Create or update a catalogue entry
// @Tags     Catalog
// @Security BearerAuth
// @Param    kind path string true "Catalogue kind"
// @Success  200 {object} models.CatalogItem
// @Router   /catalog/{kind} [put]
func (h *CatalogHandler) Upsert(c *gin.Context) {
	var item models.CatalogItem
	if err := c.ShouldBindJSON(&item); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	item.Kind = models.CatalogKind(c.Param("kind"))
	if item.Code == "" {
		response.BadRequest(c, "code is required")
		return
	}

	if err := h.catalog.Upsert(c.Request.Context(), &item); err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, item)
}

// Kinds lists the catalogues this service serves, so a client can discover them
// rather than hardcoding the list.
//
// @Summary  List catalogue kinds
// @Tags     Catalog
// @Success  200 {object} object
// @Router   /catalog [get]
func (h *CatalogHandler) Kinds(c *gin.Context) {
	kinds := make([]string, 0, len(models.AllCatalogKinds))
	for _, k := range models.AllCatalogKinds {
		kinds = append(kinds, string(k))
	}
	response.OK(c, gin.H{"kinds": kinds})
}

// ---------------------------------------------------------------------------
// Fleet
// ---------------------------------------------------------------------------

// FleetHandler serves company-owned master data.
type FleetHandler struct {
	fleet *services.FleetService
}

func NewFleetHandler(fleet *services.FleetService) *FleetHandler {
	return &FleetHandler{fleet: fleet}
}

// ListTrucks pages the caller's fleet.
//
// @Summary  List trucks
// @Tags     Trucks
// @Security BearerAuth
// @Success  200 {object} response.Meta
// @Router   /trucks [get]
func (h *FleetHandler) ListTrucks(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}

	params := parseQuery(c, repository.TruckFields())

	trucks, total, err := h.fleet.ListTrucks(c.Request.Context(), companyID, params)
	if err != nil {
		writeError(c, err)
		return
	}

	response.Paginated(c, trucks, meta(params, total))
}

// GetTruck resolves one truck within the caller's company.
//
// @Summary  Get truck
// @Tags     Trucks
// @Security BearerAuth
// @Param    id path string true "Truck ID"
// @Success  200 {object} models.Truck
// @Router   /trucks/{id} [get]
func (h *FleetHandler) GetTruck(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	id, ok := pathObjectID(c)
	if !ok {
		return
	}

	truck, err := h.fleet.GetTruck(c.Request.Context(), companyID, id)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, truck)
}

// CreateTruck registers a vehicle.
//
// @Summary  Create truck
// @Tags     Trucks
// @Security BearerAuth
// @Success  201 {object} models.Truck
// @Router   /trucks [post]
func (h *FleetHandler) CreateTruck(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}

	var truck models.Truck
	if err := c.ShouldBindJSON(&truck); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	// The tenant comes from the token, never from the body. Trusting the body
	// would let any caller write into any company's fleet.
	truck.CompanyID = companyID

	if err := h.fleet.CreateTruck(c.Request.Context(), &truck); err != nil {
		writeError(c, err)
		return
	}

	response.Created(c, truck)
}

// UpdateTruck applies a partial update.
//
// @Summary  Update truck
// @Tags     Trucks
// @Security BearerAuth
// @Param    id path string true "Truck ID"
// @Success  200 {object} object
// @Router   /trucks/{id} [put]
func (h *FleetHandler) UpdateTruck(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	id, ok := pathObjectID(c)
	if !ok {
		return
	}

	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if err := h.fleet.UpdateTruck(c.Request.Context(), companyID, id, body); err != nil {
		writeError(c, err)
		return
	}

	response.OKWithMessage(c, "Truck updated", nil)
}

// DeleteTruck soft-deletes a vehicle.
//
// @Summary  Delete truck
// @Tags     Trucks
// @Security BearerAuth
// @Param    id path string true "Truck ID"
// @Success  200 {object} object
// @Router   /trucks/{id} [delete]
func (h *FleetHandler) DeleteTruck(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	id, ok := pathObjectID(c)
	if !ok {
		return
	}

	if err := h.fleet.DeleteTruck(c.Request.Context(), companyID, id); err != nil {
		writeError(c, err)
		return
	}

	response.OKWithMessage(c, "Truck deleted", nil)
}

// AddDriver pairs a driver with a truck.
//
// @Summary  Pair driver with truck
// @Tags     Trucks
// @Security BearerAuth
// @Param    id path string true "Truck ID"
// @Success  200 {object} object
// @Router   /trucks/{id}/drivers [post]
func (h *FleetHandler) AddDriver(c *gin.Context) {
	h.driverPairing(c, true)
}

// RemoveDriver unpairs a driver.
//
// @Summary  Unpair driver from truck
// @Tags     Trucks
// @Security BearerAuth
// @Param    id path string true "Truck ID"
// @Success  200 {object} object
// @Router   /trucks/{id}/drivers [delete]
func (h *FleetHandler) RemoveDriver(c *gin.Context) {
	h.driverPairing(c, false)
}

func (h *FleetHandler) driverPairing(c *gin.Context, add bool) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	id, ok := pathObjectID(c)
	if !ok {
		return
	}

	var body struct {
		DriverID string `json:"driverId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	var err error
	if add {
		err = h.fleet.AddDriver(c.Request.Context(), companyID, id, body.DriverID)
	} else {
		err = h.fleet.RemoveDriver(c.Request.Context(), companyID, id, body.DriverID)
	}
	if err != nil {
		writeError(c, err)
		return
	}

	response.OKWithMessage(c, "Truck drivers updated", nil)
}

// ListWarehouses pages the caller's locations.
//
// @Summary  List warehouses
// @Tags     Warehouses
// @Security BearerAuth
// @Success  200 {object} response.Meta
// @Router   /warehouses [get]
func (h *FleetHandler) ListWarehouses(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}

	params := parseQuery(c, repository.WarehouseFields())

	items, total, err := h.fleet.ListWarehouses(c.Request.Context(), companyID, params)
	if err != nil {
		writeError(c, err)
		return
	}

	response.Paginated(c, items, meta(params, total))
}

// GetWarehouse resolves one location.
//
// @Summary  Get warehouse
// @Tags     Warehouses
// @Security BearerAuth
// @Param    id path string true "Warehouse ID"
// @Success  200 {object} models.Warehouse
// @Router   /warehouses/{id} [get]
func (h *FleetHandler) GetWarehouse(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	id, ok := pathObjectID(c)
	if !ok {
		return
	}

	w, err := h.fleet.GetWarehouse(c.Request.Context(), companyID, id)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, w)
}

type warehouseCreateRequest struct {
	Name           string  `json:"name" binding:"required"`
	Code           string  `json:"code"`
	Address        string  `json:"address"`
	CityID         string  `json:"cityId"`
	ProvinceID     string  `json:"provinceId"`
	PostalCode     string  `json:"postalCode"`
	Latitude       float64 `json:"latitude" binding:"required"`
	Longitude      float64 `json:"longitude" binding:"required"`
	GeofenceRadius int     `json:"geofenceRadius"`
	PICName        string  `json:"picName"`
	PICPhone       string  `json:"picPhone"`
}

// CreateWarehouse registers a location.
//
// @Summary  Create warehouse
// @Tags     Warehouses
// @Security BearerAuth
// @Success  201 {object} models.Warehouse
// @Router   /warehouses [post]
func (h *FleetHandler) CreateWarehouse(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}

	var req warehouseCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	w := &models.Warehouse{
		CompanyID:      companyID,
		Name:           req.Name,
		Code:           req.Code,
		Address:        req.Address,
		PostalCode:     req.PostalCode,
		Location:       models.NewGeoPoint(req.Latitude, req.Longitude),
		GeofenceRadius: req.GeofenceRadius,
		PICName:        req.PICName,
		PICPhone:       req.PICPhone,
	}

	if req.CityID != "" {
		id, err := primitive.ObjectIDFromHex(req.CityID)
		if err != nil {
			response.BadRequest(c, "Invalid cityId")
			return
		}
		w.CityID = &id
	}
	if req.ProvinceID != "" {
		id, err := primitive.ObjectIDFromHex(req.ProvinceID)
		if err != nil {
			response.BadRequest(c, "Invalid provinceId")
			return
		}
		w.ProvinceID = &id
	}

	if err := h.fleet.CreateWarehouse(c.Request.Context(), w); err != nil {
		writeError(c, err)
		return
	}

	response.Created(c, w)
}

// UpdateWarehouse applies a partial update.
//
// @Summary  Update warehouse
// @Tags     Warehouses
// @Security BearerAuth
// @Param    id path string true "Warehouse ID"
// @Success  200 {object} object
// @Router   /warehouses/{id} [put]
func (h *FleetHandler) UpdateWarehouse(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	id, ok := pathObjectID(c)
	if !ok {
		return
	}

	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if err := h.fleet.UpdateWarehouse(c.Request.Context(), companyID, id, body); err != nil {
		writeError(c, err)
		return
	}

	response.OKWithMessage(c, "Warehouse updated", nil)
}

// DeleteWarehouse soft-deletes a location.
//
// @Summary  Delete warehouse
// @Tags     Warehouses
// @Security BearerAuth
// @Param    id path string true "Warehouse ID"
// @Success  200 {object} object
// @Router   /warehouses/{id} [delete]
func (h *FleetHandler) DeleteWarehouse(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	id, ok := pathObjectID(c)
	if !ok {
		return
	}

	if err := h.fleet.DeleteWarehouse(c.Request.Context(), companyID, id); err != nil {
		writeError(c, err)
		return
	}

	response.OKWithMessage(c, "Warehouse deleted", nil)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// callerCompany returns the tenant from the token. Every company-scoped handler
// starts here, and none of them accept a company id from the request.
func callerCompany(c *gin.Context) (string, bool) {
	principal, ok := authctx.Gin(c)
	if !ok {
		response.Unauthorized(c, "No token provided.")
		return "", false
	}
	if principal.CompanyID == "" {
		response.Forbidden(c, "This account is not attached to a company")
		return "", false
	}
	return principal.CompanyID, true
}

// pathObjectID parses the :id path parameter, writing a 400 when malformed.
func pathObjectID(c *gin.Context) (primitive.ObjectID, bool) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid id")
		return primitive.NilObjectID, false
	}
	return id, true
}

func parseQuery(c *gin.Context, fields query.FieldSet) query.Params {
	return query.Parse(
		c.DefaultQuery("page", "0"),
		c.DefaultQuery("pageSize", "20"),
		c.Query("filtered"),
		c.Query("sorted"),
		c.Query("search"),
		fields,
	)
}

func meta(p query.Params, total int64) *response.Meta {
	return &response.Meta{
		Page:       p.Page,
		Limit:      p.PageSize,
		TotalRows:  total,
		TotalPages: p.TotalPages(total),
	}
}

func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		response.NotFound(c, "Not found")
	case errors.Is(err, repository.ErrConflict):
		response.Conflict(c, err.Error())
	case errors.Is(err, services.ErrInvalidKind), errors.Is(err, services.ErrValidation):
		response.BadRequest(c, err.Error())
	default:
		response.InternalError(c, "Request failed")
	}
}
