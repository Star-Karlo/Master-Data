package repository

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/karlo/masterdata-service/internal/models"
)

// DeviceResolver answers "which tracking device is on this vehicle".
//
// Master data owns that link and, crucially, its history: a device moves
// between vehicles, and resolving an old telemetry trail against the CURRENT
// assignment silently credits every kilometre an earlier truck drove to
// whichever vehicle holds the device today. That is why assignments carry
// fittedAt and unfittedAt rather than the vehicle simply holding an IMEI.
//
// The tracking service knows devices and nothing about vehicles; the business
// service knows vehicles and needs IMEIs to ask about them. This is the join,
// and it belongs here because the assignment history does.
type DeviceResolver struct {
	assignments *mongo.Collection
}

func NewDeviceResolver(db *mongo.Database) *DeviceResolver {
	return &DeviceResolver{
		assignments: db.Collection(models.TrackerAssignment{}.CollectionName()),
	}
}

// IMEIsAt returns the device fitted to each vehicle at a moment in time.
//
// `at` is not optional and there is no "now" convenience overload, deliberately.
// Every caller has to decide which moment it means, because the wrong answer is
// not an error — it is a plausible IMEI belonging to a different truck's
// journey. A live map passes time.Now(); a report over last March passes a date
// in last March.
//
// Vehicles with no device fitted at that moment are absent from the map rather
// than present with an empty string, so a caller cannot accidentally ask the
// tracking service about "".
func (r *DeviceResolver) IMEIsAt(ctx context.Context, companyID string, vehicleIDs []string, at time.Time) (map[string]string, error) {
	out := make(map[string]string, len(vehicleIDs))
	if len(vehicleIDs) == 0 {
		return out, nil
	}

	// An assignment covers `at` when it was fitted by then and either has not
	// been removed or was removed afterwards. Both halves matter: without the
	// first, a future-dated installation matches; without the second, every
	// device a vehicle has ever carried matches at once.
	filter := bson.M{
		"companyId": companyID,
		"vehicleId": bson.M{"$in": vehicleIDs},
		"fittedAt":  bson.M{"$lte": at},
		"$or": []bson.M{
			{"unfittedAt": nil},
			{"unfittedAt": bson.M{"$gt": at}},
		},
	}

	cursor, err := r.assignments.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	for cursor.Next(ctx) {
		var a models.TrackerAssignment
		if err := cursor.Decode(&a); err != nil {
			return nil, err
		}
		if a.IMEI == "" {
			continue
		}
		// A vehicle should hold one device at a time. If the data says
		// otherwise the later fitting wins, because that is the one an
		// installer most recently recorded — and silently keeping whichever
		// the cursor returned first would make the answer depend on index
		// order.
		if existing, clash := out[a.VehicleID]; clash && existing != a.IMEI {
			if prev, ok := r.fittedAtOf(ctx, a.VehicleID, existing); ok && prev.After(a.FittedAt) {
				continue
			}
		}
		out[a.VehicleID] = a.IMEI
	}

	return out, cursor.Err()
}

func (r *DeviceResolver) fittedAtOf(ctx context.Context, vehicleID, imei string) (time.Time, bool) {
	var a models.TrackerAssignment
	err := r.assignments.FindOne(ctx, bson.M{"vehicleId": vehicleID, "imei": imei}).Decode(&a)
	if err != nil {
		return time.Time{}, false
	}
	return a.FittedAt, true
}

// VehicleAt is the reverse: which vehicle was carrying this device.
//
// The question a telemetry trail asks. Same reasoning about `at`, from the
// other direction — attributing March's positions to today's vehicle is the
// exact mistake the assignment history exists to prevent.
func (r *DeviceResolver) VehicleAt(ctx context.Context, imei string, at time.Time) (string, bool, error) {
	filter := bson.M{
		"imei":     imei,
		"fittedAt": bson.M{"$lte": at},
		"$or": []bson.M{
			{"unfittedAt": nil},
			{"unfittedAt": bson.M{"$gt": at}},
		},
	}

	var a models.TrackerAssignment
	err := r.assignments.FindOne(ctx, filter).Decode(&a)
	if err == mongo.ErrNoDocuments {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return a.VehicleID, true, nil
}
