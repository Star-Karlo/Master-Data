package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/repository"
)

// TrackerService owns the telematics device registry.
//
// The registry lives in master data, not in either product, because the
// sentence it stores — "this device is fitted to that vehicle" — is a fact
// about the physical world that both products need and neither owns. FMS reads
// telemetry keyed by IMEI; TMS knows a vehicle by its police number; the link
// between them belongs to whichever service both can trust.
type TrackerService struct {
	trackers *repository.TrackerRepository
	trucks   *repository.TruckRepository
}

func NewTrackerService(trackers *repository.TrackerRepository, trucks *repository.TruckRepository) *TrackerService {
	return &TrackerService{trackers: trackers, trucks: trucks}
}

// RegisterInput describes a new device.
type RegisterInput struct {
	IMEI       string
	Provider   string
	Generation string
	Firmware   string
	SIMNumber  string
}

// Register adds a device to a company's inventory.
//
// The IMEI is normalised and checked for shape before it reaches the database.
// A device identifier that is not 15 digits is almost always a serial number,
// a SIM number, or a value with a stray space pasted in from a spreadsheet —
// and storing one means telemetry for that vehicle never arrives, with nothing
// to explain why.
func (s *TrackerService) Register(ctx context.Context, companyID string, in RegisterInput) (*models.Tracker, error) {
	imei, err := normaliseIMEI(in.IMEI)
	if err != nil {
		return nil, err
	}

	tracker := &models.Tracker{
		IMEI:       imei,
		CompanyID:  companyID,
		Provider:   strings.TrimSpace(in.Provider),
		Generation: strings.TrimSpace(in.Generation),
		Firmware:   strings.TrimSpace(in.Firmware),
		SIMNumber:  strings.TrimSpace(in.SIMNumber),
		Status:     models.TrackerInStock,
	}
	if err := s.trackers.Create(ctx, tracker); err != nil {
		return nil, err
	}
	return tracker, nil
}

// List returns a company's devices.
func (s *TrackerService) List(ctx context.Context, companyID string) ([]models.Tracker, error) {
	return s.trackers.ListForCompany(ctx, companyID)
}

// Fit attaches a device to a vehicle.
func (s *TrackerService) Fit(ctx context.Context, companyID string, trackerID, vehicleID primitive.ObjectID, note string) error {
	return s.trackers.Fit(ctx, companyID, trackerID, vehicleID, note)
}

// Unfit removes a device from whatever it is on.
func (s *TrackerService) Unfit(ctx context.Context, companyID string, trackerID primitive.ObjectID) error {
	return s.trackers.Unfit(ctx, companyID, trackerID)
}

// History returns a vehicle's devices over time.
func (s *TrackerService) History(ctx context.Context, companyID string, vehicleID primitive.ObjectID) ([]models.TrackerAssignment, error) {
	return s.trackers.AssignmentHistory(ctx, companyID, vehicleID)
}

// ResolveVehicle answers "which vehicle was this device on at this moment",
// which is what makes a telemetry reading attributable to a truck.
//
// A nil time means now. Callers reading live positions should pass nothing;
// callers replaying history must pass the reading's timestamp, or every point
// from before a device was moved will be credited to the wrong vehicle.
func (s *TrackerService) ResolveVehicle(ctx context.Context, imei string, at *time.Time) (*models.TrackerAssignment, error) {
	imei, err := normaliseIMEI(imei)
	if err != nil {
		return nil, err
	}
	when := time.Now().UTC()
	if at != nil {
		when = *at
	}
	return s.trackers.VehicleAt(ctx, imei, when)
}

// ResolveIMEI is the reverse: the device currently fitted to a vehicle, so a
// caller holding a police number can ask the telemetry service about it.
func (s *TrackerService) ResolveIMEI(ctx context.Context, companyID string, vehicleID primitive.ObjectID) (string, error) {
	truck, err := s.trucks.FindByID(ctx, companyID, vehicleID)
	if err != nil {
		return "", err
	}
	if truck.TrackerID == nil {
		return "", repository.ErrNotFound
	}
	history, err := s.trackers.AssignmentHistory(ctx, companyID, vehicleID)
	if err != nil {
		return "", err
	}
	for _, a := range history {
		if a.UnfittedAt == nil {
			return a.IMEI, nil
		}
	}
	return "", repository.ErrNotFound
}

// normaliseIMEI trims a device identifier and checks its shape.
//
// Spreadsheets are where these come from, so leading apostrophes, spaces and
// non-breaking spaces are routine. An IMEI is exactly 15 digits; anything else
// is a different number entirely and will simply never match a telemetry
// reading.
func normaliseIMEI(raw string) (string, error) {
	imei := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, raw)

	if len(imei) != 15 {
		return "", fmt.Errorf("%w: %q is not an IMEI — expected 15 digits, got %d",
			ErrValidation, strings.TrimSpace(raw), len(imei))
	}
	return imei, nil
}

// NormaliseIMEIForTest exposes the shape check to the test package. The
// normalisation is the interesting part and it is unexported, so this is the
// seam rather than making the function itself public.
func NormaliseIMEIForTest(raw string) (string, error) { return normaliseIMEI(raw) }
