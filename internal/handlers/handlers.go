// Package handlers exposes master data over HTTP.
//
// Every read is scoped to the caller's own company, taken from the token and
// never from the request. A companyId accepted in a query string would let any
// authenticated user page through another tenant's fleet, and the response
// would look entirely ordinary.
package handlers

import (
	"errors"
	"log/slog"
	"math"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/authctx"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/platform/response"
	"github.com/karlo/masterdata-service/internal/services"
)

// actingForHeader names the company a Karlo staff member is acting for.
//
// Its absence means "myself", which is what every ordinary request sends. The
// business service has honoured this since acting-for was introduced; master
// data did not, so a staff member could raise an order for a client but not
// see or edit that client's catalogues — every call answered 403 with a
// message about their own account, which points at the wrong thing entirely.
const actingForHeader = "X-Acting-For"

// caller resolves the company whose data this request may see.
func caller(c *gin.Context) (companyID string, ok bool) {
	p, found := authctx.Gin(c)
	if !found {
		response.Unauthorized(c, "No token provided.")
		return "", false
	}

	// Acting for a client.
	//
	// Checked BEFORE the own-company requirement: Karlo staff carry no company
	// of their own, so demanding one first would refuse the very people this
	// header exists for.
	if target := strings.TrimSpace(c.GetHeader(actingForHeader)); target != "" {
		if !p.IsPlatformStaff {
			// Not a mistake to be tolerated: a company naming another would be
			// reading and writing that company's data.
			response.Forbidden(c, "Only Karlo staff may act on another company's behalf.")
			return "", false
		}
		return target, true
	}

	if p.CompanyID == "" {
		response.Forbidden(c, "This account is not attached to a company")
		return "", false
	}
	return p.CompanyID, true
}

// params parses the list query, answering 400 and reporting whether to stop.
func params(c *gin.Context, allowed query.FieldSet) (query.Params, bool) {
	p := query.Parse(
		c.Query("page"), c.Query("pageSize"),
		c.Query("filtered"), c.Query("sorted"), c.Query("search"),
		allowed,
	)
	if p.Err != nil {
		response.BadRequest(c, p.Err.Error())
		return p, false
	}
	return p, true
}

func meta(p query.Params, total int64) *response.Meta {
	pages := 0
	if p.PageSize > 0 {
		pages = int(math.Ceil(float64(total) / float64(p.PageSize)))
	}
	return &response.Meta{Page: p.Page, Limit: p.PageSize, TotalRows: total, TotalPages: pages}
}

// writeError maps a service error onto a status.
//
// An unknown catalogue is 400 rather than 404: the caller asked for a LIST that
// does not exist, which is a client bug, where 404 would read as "this
// catalogue is empty" and send someone looking for missing data.
func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, services.ErrNotFound):
		response.NotFound(c, "Not found")
	case errors.Is(err, services.ErrForbidden):
		response.Forbidden(c, err.Error())
	case errors.Is(err, services.ErrUnknownKind), errors.Is(err, services.ErrValidation):
		response.BadRequest(c, err.Error())
	default:
		// Logged, not just counted. The response stays generic — an internal
		// error's detail is not the caller's business — but a 500 whose cause
		// appears nowhere is unactionable, and the request id ties the two
		// together.
		slog.ErrorContext(c.Request.Context(), "unhandled master data error",
			"error", err, "path", c.FullPath(), "request_id", c.GetString("request_id"))
		response.InternalError(c, "Request failed")
	}
}

// ---------------------------------------------------------------------------
// Catalogues
// ---------------------------------------------------------------------------

type CatalogHandler struct{ catalog *services.CatalogService }

func NewCatalogHandler(s *services.CatalogService) *CatalogHandler {
	return &CatalogHandler{catalog: s}
}

// catalogFields is the allowlist for filtering and sorting a reference list.
//
// Deliberately short. These are read-mostly pickers, and every field offered
// here is one a caller can probe; `nameNormalised` in particular is an index
// key rather than information and is not offered.
var catalogFields = query.FieldSet{
	"name":      "name",
	"code":      "code",
	"isActive":  "isActive",
	"createdAt": "createdAt",
}

// kindInfo describes one catalogue to a client building a Master Data screen.
type kindInfo struct {
	Kind string `json:"kind"`
	// Writable is false for a catalogue that can be read and not edited. Sent
	// so the UI can omit the add button rather than offering one that fails.
	Writable bool `json:"writable"`

	// Shareable is false for a catalogue whose entries always belong to one
	// company — a vehicle group, an item. Sent so a staff form offers "shared
	// with everyone" only where that is possible.
	Shareable bool `json:"shareable"`
}

// Kinds lists the catalogues this service serves.
// @Summary  Catalogue kinds
// @Tags     catalog
// @Security BearerAuth
// @Produce  json
// @Success  200 {array} kindInfo
// @Router   /catalog [get]
func (h *CatalogHandler) Kinds(c *gin.Context) {
	names := services.CatalogKinds()
	out := make([]kindInfo, 0, len(names))
	for _, name := range names {
		out = append(out, kindInfo{
			Kind:      name,
			Writable:  h.catalog.Writable(name),
			Shareable: services.Shareable(name),
		})
	}
	response.OK(c, out)
}

// staff reports whether this caller maintains the platform-global lists.
//
// Deliberately FALSE while acting for a client. Karlo staff are otherwise
// exempt from the company filter so they can maintain the shared catalogues,
// but that exemption must not survive an explicit "act as this client": a
// staff member working on Siba Surya's behalf who still saw every company's
// entries would be offered another client's customers in Siba Surya's own
// pickers, and could name one on Siba Surya's order.
//
// Write authority is unaffected — that is checked separately against the
// principal, not through this.
func staff(c *gin.Context) bool {
	p, ok := authctx.Gin(c)
	if !ok || !p.IsPlatformStaff {
		return false
	}
	return strings.TrimSpace(c.GetHeader(actingForHeader)) == ""
}

// Create adds an entry.
// @Summary  Add a catalogue entry
// @Description Karlo staff create shared entries; a company creates its own. Fields depend on the kind — see the models for each catalogue (Brand, CargoType, Item, TruckHead, TruckBody, TruckClass, Customer, VehicleGroup, TrackerModel…).
// @Tags     catalog
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    kind path string true "Catalogue kind, from GET /catalog"
// @Param    body body object true "Entry fields for the kind"
// @Success  201 {object} map[string]interface{}
// @Failure  400 {object} errorBody
// @Failure  403 {object} errorBody
// @Router   /catalog/{kind} [post]
func (h *CatalogHandler) Create(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}

	var payload map[string]any
	if err := c.ShouldBindJSON(&payload); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	id, err := h.catalog.Create(c.Request.Context(), c.Param("kind"), companyID, staff(c), payload)
	if err != nil {
		writeError(c, err)
		return
	}

	entry, err := h.catalog.Get(c.Request.Context(), c.Param("kind"), id, companyID, staff(c))
	if err != nil {
		// The write succeeded; only the read-back failed. Returning the id
		// alone is honest — telling the caller it failed would have them
		// create it twice.
		response.Created(c, gin.H{"id": id})
		return
	}
	response.Created(c, entry)
}

// Update changes an entry.
// @Summary  Edit a catalogue entry
// @Tags     catalog
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    kind path string true "Catalogue kind"
// @Param    id   path string true "Entry id"
// @Param    body body object true "Changed fields"
// @Success  200 {object} map[string]interface{}
// @Failure  404 {object} errorBody
// @Router   /catalog/{kind}/{id} [put]
func (h *CatalogHandler) Update(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}

	var payload map[string]any
	if err := c.ShouldBindJSON(&payload); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	kind, id := c.Param("kind"), c.Param("id")
	if err := h.catalog.Update(c.Request.Context(), kind, id, companyID, staff(c), payload); err != nil {
		writeError(c, err)
		return
	}

	entry, err := h.catalog.Get(c.Request.Context(), kind, id, companyID, staff(c))
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, entry)
}

// Delete retires an entry.
// @Summary  Retire a catalogue entry
// @Description Soft: documents already referencing it keep resolving; the name frees up.
// @Tags     catalog
// @Security BearerAuth
// @Produce  json
// @Param    kind path string true "Catalogue kind"
// @Param    id   path string true "Entry id"
// @Success  200 {object} map[string]interface{}
// @Failure  404 {object} errorBody
// @Router   /catalog/{kind}/{id} [delete]
func (h *CatalogHandler) Delete(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}

	if err := h.catalog.Delete(c.Request.Context(), c.Param("kind"), c.Param("id"), companyID, staff(c)); err != nil {
		writeError(c, err)
		return
	}
	response.OKWithMessage(c, "Entry removed.", nil)
}

// List pages one catalogue.
// @Summary  List one catalogue
// @Tags     catalog
// @Security BearerAuth
// @Produce  json
// @Param    kind     path  string true  "Catalogue kind"
// @Param    parentId query string false "Restrict to children of this entry (e.g. sub-categories of a category)"
// @Param    page     query int    false "Zero-based page"
// @Param    pageSize query int    false "Rows per page"
// @Param    search   query string false "Free-text search"
// @Param    sorted   query string false "Sort, e.g. name:asc"
// @Param    filtered query string false "Filters as field:value, comma separated"
// @Success  200 {object} map[string]interface{} "success, data[], meta{page,limit,totalRows,totalPages}"
// @Router   /catalog/{kind} [get]
func (h *CatalogHandler) List(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	p, ok := params(c, catalogFields)
	if !ok {
		return
	}

	items, total, err := h.catalog.List(c.Request.Context(),
		c.Param("kind"), companyID, c.Query("parentId"), staff(c), p)
	if err != nil {
		writeError(c, err)
		return
	}
	response.Paginated(c, items, meta(p, total))
}

// Get resolves one entry.
// @Summary  Get a catalogue entry
// @Tags     catalog
// @Security BearerAuth
// @Produce  json
// @Param    kind path string true "Catalogue kind"
// @Param    id   path string true "Entry id"
// @Success  200 {object} map[string]interface{}
// @Failure  404 {object} errorBody
// @Router   /catalog/{kind}/{id} [get]
func (h *CatalogHandler) Get(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}

	item, err := h.catalog.Get(c.Request.Context(), c.Param("kind"), c.Param("id"), companyID, staff(c))
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, item)
}

// ---------------------------------------------------------------------------
// Fleet
// ---------------------------------------------------------------------------

type FleetHandler struct{ fleet *services.FleetService }

func NewFleetHandler(s *services.FleetService) *FleetHandler {
	return &FleetHandler{fleet: s}
}

var truckFields = query.FieldSet{
	"policeNumber": "licensePlate",
	"status":       "status",
	"isAvailable":  "isAvailable",
	"createdAt":    "createdAt",
}

var warehouseFields = query.FieldSet{
	"name":      "name",
	"city":      "city",
	"createdAt": "createdAt",
}

// ListTrucks pages the company's assignable vehicles.
// @Summary  List the company's trucks
// @Tags     fleet
// @Security BearerAuth
// @Produce  json
// @Param    isAvailable query bool false "Only trucks free for assignment"
// @Param    page     query int    false "Zero-based page"
// @Param    pageSize query int    false "Rows per page"
// @Param    search   query string false "Free-text search"
// @Param    sorted   query string false "Sort, e.g. name:asc"
// @Param    filtered query string false "Filters as field:value, comma separated"
// @Success  200 {object} map[string]interface{} "success, data []models.Vehicle, meta"
// @Router   /trucks [get]
func (h *FleetHandler) ListTrucks(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	p, ok := params(c, truckFields)
	if !ok {
		return
	}

	trucks, total, err := h.fleet.ListTrucks(c.Request.Context(), companyID, p,
		c.Query("isAvailable") == "true")
	if err != nil {
		writeError(c, err)
		return
	}
	response.Paginated(c, trucks, meta(p, total))
}

// @Summary  Get a truck
// @Tags     fleet
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Truck id"
// @Success  200 {object} models.Vehicle
// @Failure  404 {object} errorBody
// @Router   /trucks/{id} [get]
func (h *FleetHandler) GetTruck(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}

	truck, err := h.fleet.GetTruck(c.Request.Context(), companyID, c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, truck)
}

// @Summary  List the company's sites
// @Tags     warehouses
// @Security BearerAuth
// @Produce  json
// @Param    page     query int    false "Zero-based page"
// @Param    pageSize query int    false "Rows per page"
// @Param    search   query string false "Free-text search"
// @Param    sorted   query string false "Sort, e.g. name:asc"
// @Param    filtered query string false "Filters as field:value, comma separated"
// @Success  200 {object} map[string]interface{} "success, data []models.Site, meta"
// @Router   /warehouses [get]
func (h *FleetHandler) ListWarehouses(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	p, ok := params(c, warehouseFields)
	if !ok {
		return
	}

	sites, total, err := h.fleet.ListWarehouses(c.Request.Context(), companyID, p)
	if err != nil {
		writeError(c, err)
		return
	}
	response.Paginated(c, sites, meta(p, total))
}

type warehouseRequest struct {
	Name     string `json:"name" binding:"required"`
	SiteType string `json:"siteType"`

	Address  string `json:"address"`
	Street   string `json:"street"`
	District string `json:"district"`
	City     string `json:"city"`
	Province string `json:"province"`
	Postcode string `json:"postcode"`

	// Pointers, so "not supplied" stays distinct from zero. Nought, nought is
	// a real place and the service refuses it; a nil pair means the site has
	// no coordinate yet, which is allowed.
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`

	GeofenceRadiusMeters *int `json:"geofenceRadiusMeters"`

	PICName  string `json:"picName"`
	PICPhone string `json:"picPhone"`
	Notes    string `json:"notes"`
}

func (r warehouseRequest) toInput() services.SiteInput {
	return services.SiteInput{
		Name: r.Name, SiteType: r.SiteType,
		Address: r.Address, Street: r.Street, District: r.District,
		City: r.City, Province: r.Province, Postcode: r.Postcode,
		Latitude: r.Latitude, Longitude: r.Longitude,
		GeofenceRadiusM: r.GeofenceRadiusMeters,
		PICName:         r.PICName, PICPhone: r.PICPhone, Notes: r.Notes,
	}
}

// CreateWarehouse records a loading or unloading point.
// @Summary  Add a site
// @Tags     warehouses
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    body body warehouseRequest true "Site"
// @Success  201 {object} models.Site
// @Failure  400 {object} errorBody
// @Router   /warehouses [post]
func (h *FleetHandler) CreateWarehouse(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}

	var req warehouseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	site, err := h.fleet.CreateSite(c.Request.Context(), companyID, req.toInput())
	if err != nil {
		writeError(c, err)
		return
	}
	response.Created(c, site)
}

// UpdateWarehouse changes a site the company owns.
// @Summary  Edit a site
// @Tags     warehouses
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    id   path string true "Site id"
// @Param    body body warehouseRequest true "Site"
// @Success  200 {object} models.Site
// @Failure  404 {object} errorBody
// @Router   /warehouses/{id} [put]
func (h *FleetHandler) UpdateWarehouse(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}

	var req warehouseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	site, err := h.fleet.UpdateSite(c.Request.Context(), companyID, c.Param("id"), req.toInput())
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, site)
}

// DeleteWarehouse retires a site.
// @Summary  Retire a site
// @Tags     warehouses
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Site id"
// @Success  200 {object} map[string]interface{}
// @Failure  404 {object} errorBody
// @Router   /warehouses/{id} [delete]
func (h *FleetHandler) DeleteWarehouse(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	if err := h.fleet.DeleteSite(c.Request.Context(), companyID, c.Param("id")); err != nil {
		writeError(c, err)
		return
	}
	response.OKWithMessage(c, "Site removed.", nil)
}

// @Summary  Get a site
// @Tags     warehouses
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Site id"
// @Success  200 {object} models.Site
// @Failure  404 {object} errorBody
// @Router   /warehouses/{id} [get]
func (h *FleetHandler) GetWarehouse(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}

	site, err := h.fleet.GetWarehouse(c.Request.Context(), companyID, c.Param("id"))
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, site)
}

// Referenced only by the API document; the handlers return whatever the
// service hands them.
var (
	_ models.Vehicle
	_ models.Site
)

// errorBody is the failure envelope every endpoint returns, for the API document.
type errorBody struct {
	Success bool   `json:"success" example:"false"`
	Message string `json:"message" example:"Entry not found."`
}
