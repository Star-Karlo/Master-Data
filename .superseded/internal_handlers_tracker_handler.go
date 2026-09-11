package handlers

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/karlo/masterdata-service/internal/platform/authctx"
	"github.com/karlo/masterdata-service/internal/platform/response"
	"github.com/karlo/masterdata-service/internal/services"
)

// TrackerHandler serves the telematics device registry.
type TrackerHandler struct {
	trackers *services.TrackerService
}

func NewTrackerHandler(t *services.TrackerService) *TrackerHandler {
	return &TrackerHandler{trackers: t}
}

// List returns the company's devices.
//
// @Summary  List telematics devices
// @Tags     Trackers
// @Security BearerAuth
// @Success  200 {array} models.Tracker
// @Router   /trackers [get]
func (h *TrackerHandler) List(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	rows, err := h.trackers.List(c.Request.Context(), companyID)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, rows)
}

type registerTrackerRequest struct {
	IMEI       string `json:"imei" binding:"required"`
	Provider   string `json:"provider"`
	Generation string `json:"generation"`
	Firmware   string `json:"firmware"`
	SIMNumber  string `json:"simNumber"`
}

// Register adds a device to the company's inventory.
//
// @Summary  Register a telematics device
// @Tags     Trackers
// @Security BearerAuth
// @Success  201 {object} models.Tracker
// @Router   /trackers [post]
func (h *TrackerHandler) Register(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	var body registerTrackerRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	tracker, err := h.trackers.Register(c.Request.Context(), companyID, services.RegisterInput{
		IMEI:       body.IMEI,
		Provider:   body.Provider,
		Generation: body.Generation,
		Firmware:   body.Firmware,
		SIMNumber:  body.SIMNumber,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response.Created(c, tracker)
}

type fitRequest struct {
	VehicleID string `json:"vehicleId" binding:"required"`
	Note      string `json:"note"`
}

// Fit attaches a device to a vehicle.
//
// @Summary  Fit a device to a vehicle
// @Tags     Trackers
// @Security BearerAuth
// @Param    id path string true "Tracker ID"
// @Success  200 {object} response.Envelope
// @Router   /trackers/{id}/fit [put]
func (h *TrackerHandler) Fit(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	trackerID, ok := pathObjectID(c)
	if !ok {
		return
	}
	var body fitRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	vehicleID, err := primitive.ObjectIDFromHex(body.VehicleID)
	if err != nil {
		response.BadRequest(c, "Invalid vehicleId")
		return
	}

	if err := h.trackers.Fit(c.Request.Context(), companyID, trackerID, vehicleID, body.Note); err != nil {
		writeError(c, err)
		return
	}
	response.OKWithMessage(c, "Device fitted. Telemetry from now on is attributed to this vehicle.", nil)
}

// Unfit removes a device from whatever it is on.
//
// @Summary  Remove a device from a vehicle
// @Tags     Trackers
// @Security BearerAuth
// @Param    id path string true "Tracker ID"
// @Success  200 {object} response.Envelope
// @Router   /trackers/{id}/fit [delete]
func (h *TrackerHandler) Unfit(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	trackerID, ok := pathObjectID(c)
	if !ok {
		return
	}
	if err := h.trackers.Unfit(c.Request.Context(), companyID, trackerID); err != nil {
		writeError(c, err)
		return
	}
	response.OKWithMessage(c, "Device removed. Its history stays against the vehicle it was on.", nil)
}

// History returns which devices a vehicle has carried, and when.
//
// @Summary  A vehicle's device history
// @Tags     Trackers
// @Security BearerAuth
// @Param    id path string true "Vehicle ID"
// @Success  200 {array} models.TrackerAssignment
// @Router   /trucks/{id}/trackers [get]
func (h *TrackerHandler) History(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	vehicleID, ok := pathObjectID(c)
	if !ok {
		return
	}
	rows, err := h.trackers.History(c.Request.Context(), companyID, vehicleID)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, rows)
}

// Resolve answers which vehicle carried a device at a moment in time.
//
// The `at` parameter is what makes historical telemetry attributable. Omitting
// it resolves to now, which is right for a live position and wrong for a replay:
// without it, every point recorded before a device was moved is credited to
// whichever vehicle holds it today.
//
// @Summary  Which vehicle carried this device
// @Tags     Trackers
// @Security BearerAuth
// @Param    imei path string true "Device IMEI"
// @Param    at query string false "RFC3339 instant; defaults to now"
// @Success  200 {object} models.TrackerAssignment
// @Router   /trackers/by-imei/{imei}/vehicle [get]
func (h *TrackerHandler) Resolve(c *gin.Context) {
	var at *time.Time
	if raw := c.Query("at"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			response.BadRequest(c, "at must be an RFC3339 instant")
			return
		}
		at = &parsed
	}

	assignment, err := h.trackers.ResolveVehicle(c.Request.Context(), c.Param("imei"), at)
	if err != nil {
		writeError(c, err)
		return
	}

	// A device belongs to one company, and the answer names a vehicle. Confirm
	// the caller is that company before returning it: without this check,
	// anyone holding an IMEI could learn which company operates the vehicle
	// carrying it.
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	if assignment.CompanyID != companyID && !isPlatformStaff(c) {
		response.NotFound(c, "No vehicle carried that device at that time")
		return
	}

	response.OK(c, assignment)
}

func isPlatformStaff(c *gin.Context) bool {
	p, ok := authctx.Gin(c)
	return ok && p.IsPlatformStaff
}
