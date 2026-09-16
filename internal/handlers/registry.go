package handlers

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/authctx"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/platform/response"
	"github.com/karlo/masterdata-service/internal/services"
)

// RegistryHandler is the writable fleet register over HTTP: drivers,
// vehicles, devices and fittings, groups, documents. Both products maintain
// it; see RequireAnyOf on the routes for how each is let in.
type RegistryHandler struct{ reg *services.RegistryService }

func NewRegistryHandler(s *services.RegistryService) *RegistryHandler {
	return &RegistryHandler{reg: s}
}

var (
	driverFields = query.FieldSet{
		"fullName": "fullName", "phone": "phone", "employeeNo": "employeeNo",
		"licenseExpiry": "licenseExpiry", "status": "status", "createdAt": "createdAt",
	}
	vehicleFields = query.FieldSet{
		"licensePlate": "licensePlate", "status": "status", "unitType": "unitType",
		"isAvailable": "isAvailable", "unitYear": "unitYear", "createdAt": "createdAt", "updatedAt": "updatedAt",
	}
	trackerFields = query.FieldSet{
		"deviceId": "deviceId", "kind": "kind", "owner": "owner", "status": "status", "createdAt": "createdAt",
	}
	documentFields = query.FieldSet{"docType": "docType", "expiresOn": "expiresOn", "issuedOn": "issuedOn", "createdAt": "createdAt"}
)

// actor is the signed-in user's id, for "who did this" fields.
func actor(c *gin.Context) string {
	p, ok := authctx.Gin(c)
	if !ok {
		return ""
	}
	return p.UserID
}

func writeRegistryError(c *gin.Context, err error) {
	if errors.Is(err, services.ErrConflict) {
		response.Conflict(c, err.Error())
		return
	}
	writeError(c, err)
}

func boolQuery(c *gin.Context, name string) *bool {
	v := c.Query(name)
	if v == "" {
		return nil
	}
	b := v == "true" || v == "1"
	return &b
}

// --- Drivers ---------------------------------------------------------------

// @Summary  List drivers
// @Tags     drivers
// @Security BearerAuth
// @Produce  json
// @Param    status   query string false "active or inactive"
// @Param    page     query int    false "Zero-based page"
// @Param    pageSize query int    false "Rows per page"
// @Param    search   query string false "Name, phone or employee number"
// @Success  200 {object} map[string]interface{} "success, data []models.Driver, meta"
// @Router   /drivers [get]
func (h *RegistryHandler) ListDrivers(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	p, ok := params(c, driverFields)
	if !ok {
		return
	}
	rows, total, err := h.reg.ListDrivers(c.Request.Context(), companyID, p, c.Query("status"))
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.Paginated(c, rows, meta(p, total))
}

// @Summary  Get a driver
// @Tags     drivers
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Driver id"
// @Success  200 {object} models.Driver
// @Router   /drivers/{id} [get]
func (h *RegistryHandler) GetDriver(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	d, err := h.reg.GetDriver(c.Request.Context(), companyID, c.Param("id"))
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OK(c, d)
}

// @Summary  Add a driver
// @Tags     drivers
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    body body services.DriverInput true "Driver"
// @Success  201 {object} models.Driver
// @Router   /drivers [post]
func (h *RegistryHandler) CreateDriver(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	var in services.DriverInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	d, err := h.reg.CreateDriver(c.Request.Context(), companyID, in)
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.Created(c, d)
}

// @Summary  Edit a driver
// @Tags     drivers
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    id   path string true "Driver id"
// @Param    body body services.DriverInput true "Changed fields"
// @Success  200 {object} models.Driver
// @Router   /drivers/{id} [put]
func (h *RegistryHandler) UpdateDriver(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	var in services.DriverInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	d, err := h.reg.UpdateDriver(c.Request.Context(), companyID, c.Param("id"), in)
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OK(c, d)
}

// @Summary  Retire a driver
// @Tags     drivers
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Driver id"
// @Success  200 {object} map[string]interface{}
// @Router   /drivers/{id} [delete]
func (h *RegistryHandler) DeleteDriver(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	if err := h.reg.DeleteDriver(c.Request.Context(), companyID, c.Param("id")); err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OKWithMessage(c, "Driver removed.", nil)
}

// --- Vehicles --------------------------------------------------------------

// @Summary  List vehicles
// @Tags     vehicles
// @Security BearerAuth
// @Produce  json
// @Param    status      query string false "Status"
// @Param    isAvailable query bool   false "Only vehicles free for assignment"
// @Param    groupId     query string false "Members of this vehicle group"
// @Param    page     query int    false "Zero-based page"
// @Param    pageSize query int    false "Rows per page"
// @Param    search   query string false "Plate or chassis prefix"
// @Success  200 {object} map[string]interface{} "success, data []services.VehicleView, meta"
// @Router   /vehicles [get]
func (h *RegistryHandler) ListVehicles(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	p, ok := params(c, vehicleFields)
	if !ok {
		return
	}
	rows, total, err := h.reg.ListVehicles(c.Request.Context(), companyID, p,
		c.Query("status"), boolQuery(c, "isAvailable"), c.Query("groupId"))
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.Paginated(c, rows, meta(p, total))
}

// @Summary  Get a vehicle
// @Tags     vehicles
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Vehicle id"
// @Success  200 {object} services.VehicleView
// @Router   /vehicles/{id} [get]
func (h *RegistryHandler) GetVehicle(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	v, err := h.reg.GetVehicle(c.Request.Context(), companyID, c.Param("id"))
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OK(c, v)
}

// @Summary  Add a vehicle
// @Tags     vehicles
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    body body services.VehicleInput true "Vehicle"
// @Success  201 {object} services.VehicleView
// @Router   /vehicles [post]
func (h *RegistryHandler) CreateVehicle(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	var in services.VehicleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	v, err := h.reg.CreateVehicle(c.Request.Context(), companyID, in)
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.Created(c, v)
}

// @Summary  Edit a vehicle
// @Tags     vehicles
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    id   path string true "Vehicle id"
// @Param    body body services.VehicleInput true "Changed fields; currentDriverId ” releases the driver"
// @Success  200 {object} services.VehicleView
// @Router   /vehicles/{id} [put]
func (h *RegistryHandler) UpdateVehicle(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	var in services.VehicleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	v, err := h.reg.UpdateVehicle(c.Request.Context(), companyID, c.Param("id"), in)
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OK(c, v)
}

// @Summary  Retire a vehicle
// @Description Refused (409) while a device is fitted.
// @Tags     vehicles
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Vehicle id"
// @Success  200 {object} map[string]interface{}
// @Failure  409 {object} errorBody
// @Router   /vehicles/{id} [delete]
func (h *RegistryHandler) DeleteVehicle(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	if err := h.reg.DeleteVehicle(c.Request.Context(), companyID, c.Param("id")); err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OKWithMessage(c, "Vehicle removed.", nil)
}

// --- Trackers --------------------------------------------------------------

// trackerScope is the company a device request is about. Karlo staff acting
// for nobody see Karlo's stock (companyId nil) as well as every company's.
func trackerScope(c *gin.Context) (string, bool, bool) {
	p, found := authctx.Gin(c)
	if !found {
		response.Unauthorized(c, "No token provided.")
		return "", false, false
	}
	if p.IsPlatformStaff && c.GetHeader(actingForHeader) == "" {
		return "", true, true
	}
	companyID, ok := caller(c)
	return companyID, p.IsPlatformStaff, ok
}

// @Summary  List devices
// @Tags     trackers
// @Security BearerAuth
// @Produce  json
// @Param    kind      query string false "gps or dashcam"
// @Param    fitted    query bool   false "Only fitted (true) or only spare (false)"
// @Param    vehicleId query string false "Devices currently on this vehicle"
// @Param    page     query int    false "Zero-based page"
// @Param    pageSize query int    false "Rows per page"
// @Param    search   query string false "Device id prefix"
// @Success  200 {object} map[string]interface{} "success, data []services.TrackerView, meta"
// @Router   /trackers [get]
func (h *RegistryHandler) ListTrackers(c *gin.Context) {
	companyID, _, ok := trackerScope(c)
	if !ok {
		return
	}
	p, ok := params(c, trackerFields)
	if !ok {
		return
	}
	rows, total, err := h.reg.ListTrackers(c.Request.Context(), companyID, p,
		c.Query("kind"), boolQuery(c, "fitted"), c.Query("vehicleId"))
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.Paginated(c, rows, meta(p, total))
}

// @Summary  Get a device
// @Tags     trackers
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Device id"
// @Success  200 {object} services.TrackerView
// @Router   /trackers/{id} [get]
func (h *RegistryHandler) GetTracker(c *gin.Context) {
	companyID, _, ok := trackerScope(c)
	if !ok {
		return
	}
	t, err := h.reg.GetTracker(c.Request.Context(), companyID, c.Param("id"))
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OK(c, t)
}

// @Summary  Register a device
// @Tags     trackers
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    body body services.TrackerInput true "Device; kind gps (deviceId = 15-digit IMEI) or dashcam"
// @Success  201 {object} services.TrackerView
// @Failure  409 {object} errorBody
// @Router   /trackers [post]
func (h *RegistryHandler) CreateTracker(c *gin.Context) {
	companyID, staff, ok := trackerScope(c)
	if !ok {
		return
	}
	var in services.TrackerInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	t, err := h.reg.CreateTracker(c.Request.Context(), companyID, staff, in)
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.Created(c, t)
}

// @Summary  Edit a device
// @Description kind and deviceId are immutable (409).
// @Tags     trackers
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    id   path string true "Device id"
// @Param    body body services.TrackerInput true "Changed fields"
// @Success  200 {object} services.TrackerView
// @Router   /trackers/{id} [put]
func (h *RegistryHandler) UpdateTracker(c *gin.Context) {
	companyID, _, ok := trackerScope(c)
	if !ok {
		return
	}
	var in services.TrackerInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	t, err := h.reg.UpdateTracker(c.Request.Context(), companyID, c.Param("id"), in)
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OK(c, t)
}

// @Summary  Retire a device
// @Description Refused (409) while fitted.
// @Tags     trackers
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Device id"
// @Success  200 {object} map[string]interface{}
// @Router   /trackers/{id} [delete]
func (h *RegistryHandler) DeleteTracker(c *gin.Context) {
	companyID, _, ok := trackerScope(c)
	if !ok {
		return
	}
	if err := h.reg.DeleteTracker(c.Request.Context(), companyID, c.Param("id")); err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OKWithMessage(c, "Device removed.", nil)
}

// @Summary  Fit a device to a vehicle
// @Description Closes the device's previous fitting and the vehicle's previous device of the same kind, opens the new one, and for GPS sets the vehicle's trackerId — one operation.
// @Tags     trackers
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    id   path string true "Device id"
// @Param    body body services.FitInput true "Vehicle and fitting details"
// @Success  201 {object} models.TrackerAssignment
// @Router   /trackers/{id}/fit [post]
func (h *RegistryHandler) Fit(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	var in services.FitInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if in.InstalledByUserID == nil {
		if a := actor(c); a != "" {
			in.InstalledByUserID = &a
		}
	}
	a, err := h.reg.Fit(c.Request.Context(), companyID, c.Param("id"), in)
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.Created(c, a)
}

// @Summary  Unfit a device
// @Tags     trackers
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    id   path string true "Device id"
// @Param    body body services.UnfitInput false "unfittedAt, default now"
// @Success  200 {object} map[string]interface{}
// @Router   /trackers/{id}/unfit [post]
func (h *RegistryHandler) Unfit(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	var in services.UnfitInput
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&in); err != nil {
			response.BadRequest(c, err.Error())
			return
		}
	}
	if err := h.reg.Unfit(c.Request.Context(), companyID, c.Param("id"), in); err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OKWithMessage(c, "Device unfitted.", nil)
}

// @Summary  A device's fitting history
// @Tags     trackers
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Device id"
// @Success  200 {array} models.TrackerAssignment
// @Router   /trackers/{id}/assignments [get]
func (h *RegistryHandler) Assignments(c *gin.Context) {
	companyID, _, ok := trackerScope(c)
	if !ok {
		return
	}
	rows, err := h.reg.Assignments(c.Request.Context(), companyID, c.Param("id"))
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OK(c, rows)
}

// --- Vehicle groups --------------------------------------------------------

// --- Documents -------------------------------------------------------------

// @Summary  List documents
// @Description One of vehicleId or driverId is required unless expiringWithinDays is set, which sweeps the company.
// @Tags     documents
// @Security BearerAuth
// @Produce  json
// @Param    vehicleId          query string false "Owner vehicle"
// @Param    driverId           query string false "Owner driver"
// @Param    expiringWithinDays query int    false "Only documents expiring within N days"
// @Success  200 {object} map[string]interface{} "success, data []models.Document, meta"
// @Router   /documents [get]
func (h *RegistryHandler) ListDocuments(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	p, ok := params(c, documentFields)
	if !ok {
		return
	}
	days, _ := strconv.Atoi(c.Query("expiringWithinDays"))
	rows, total, err := h.reg.ListDocuments(c.Request.Context(), companyID, p,
		c.Query("vehicleId"), c.Query("driverId"), days)
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.Paginated(c, rows, meta(p, total))
}

// @Summary  Add a document
// @Tags     documents
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    body body services.DocumentInput true "Document; exactly one of vehicleId / driverId"
// @Success  201 {object} models.Document
// @Router   /documents [post]
func (h *RegistryHandler) CreateDocument(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	var in services.DocumentInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	d, err := h.reg.CreateDocument(c.Request.Context(), companyID, in)
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.Created(c, d)
}

// @Summary  Edit a document
// @Tags     documents
// @Security BearerAuth
// @Accept   json
// @Produce  json
// @Param    id   path string true "Document id"
// @Param    body body services.DocumentInput true "Changed fields; owner cannot change"
// @Success  200 {object} models.Document
// @Router   /documents/{id} [put]
func (h *RegistryHandler) UpdateDocument(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	var in services.DocumentInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	d, err := h.reg.UpdateDocument(c.Request.Context(), companyID, c.Param("id"), in)
	if err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OK(c, d)
}

// @Summary  Delete a document
// @Tags     documents
// @Security BearerAuth
// @Produce  json
// @Param    id path string true "Document id"
// @Success  200 {object} map[string]interface{}
// @Router   /documents/{id} [delete]
func (h *RegistryHandler) DeleteDocument(c *gin.Context) {
	companyID, ok := caller(c)
	if !ok {
		return
	}
	if err := h.reg.DeleteDocument(c.Request.Context(), companyID, c.Param("id")); err != nil {
		writeRegistryError(c, err)
		return
	}
	response.OKWithMessage(c, "Document removed.", nil)
}

// Referenced only by the API document.
var (
	_ models.Driver
	_ models.Document
	_ models.TrackerAssignment
)
