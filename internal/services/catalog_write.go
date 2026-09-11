package services

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/repository"
)

// ErrForbidden is a caller acting outside their authority — editing a global
// entry they do not own, most often.
var ErrForbidden = errors.New("forbidden")

// writer is the write half of one catalogue.
//
// An interface with a generic implementation, rather than ten hand-written
// services, because the ten collections differ only in their FIELDS and are
// identical in their rules: decode, scope to the caller, write through the
// store so BeforeWrite runs.
//
// That last part is the reason this is not a bson.M insert. Every reference
// list has a normalised twin of its name, and every unique index is built on
// the twin rather than on what the user typed. A raw write leaves it empty, the
// partial index skips the document, and a duplicate is created with no error at
// all — the exact failure this service exists to end.
type writer interface {
	// owned says whether the document carries a companyId at all. The device
	// catalogues do not, and writing one would add a field their validator
	// does not declare.
	Create(ctx context.Context, ownerID *string, owned bool, payload map[string]any) (string, error)
	Update(ctx context.Context, id string, scope *string, payload map[string]any) error
	Delete(ctx context.Context, id string, scope *string) error
}

type storeWriter[T any, PT interface {
	*T
	repository.Document
}] struct {
	store *repository.Store[T, PT]
}

func newWriter[T any, PT interface {
	*T
	repository.Document
}](db *mongo.Database) writer {
	return &storeWriter[T, PT]{store: repository.NewStore[T, PT](db)}
}

// decode turns a JSON payload into the typed document.
//
// Through BSON rather than JSON because the models' field names are declared in
// bson tags — `nameNormalised`, `companyId` — and a JSON round trip would look
// for tags that are not there, silently dropping every field.
func decode[T any](payload map[string]any, into *T) error {
	raw, err := bson.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if err := bson.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return nil
}

func (w *storeWriter[T, PT]) Create(ctx context.Context, ownerID *string, owned bool, payload map[string]any) (string, error) {
	// Fields the caller must not set. The id is minted here, the timestamps by
	// the store, the normalised twin by BeforeWrite, and `deleted` is what soft
	// delete owns — a payload that set any of them would either be overwritten
	// confusingly or, in the case of deleted, create a row nothing can see.
	for _, reserved := range []string{"_id", "id", "createdAt", "updatedAt", "deleted", "nameNormalised", "plateNormalised"} {
		delete(payload, reserved)
	}
	// The payload's own companyId is discarded here: the service has already
	// resolved the owner from the caller's authority, and honouring the
	// payload as well would let the two disagree.
	delete(payload, "companyId")

	// Ownership is decided HERE, never taken from the payload. A company that
	// could set companyId to null would publish an entry into every other
	// company's picker.
	if owned {
		if ownerID == nil {
			payload["companyId"] = nil
		} else {
			payload["companyId"] = *ownerID
		}
	}
	if _, set := payload["isActive"]; !set {
		payload["isActive"] = true
	}

	var doc T
	if err := decode(payload, &doc); err != nil {
		return "", err
	}

	id := primitive.NewObjectID()
	setObjectID(&doc, id)

	if err := w.store.Create(ctx, &doc); err != nil {
		if errors.Is(err, repository.ErrDuplicate) {
			return "", fmt.Errorf("%w: an entry with that name already exists", ErrValidation)
		}
		return "", err
	}
	return id.Hex(), nil
}

func (w *storeWriter[T, PT]) Update(ctx context.Context, id string, scope *string, payload map[string]any) error {
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return ErrNotFound
	}

	// Read the current document first, so the update is a full replace of a
	// document that EXISTS and is the caller's to change. Store.Update replaces
	// rather than $sets — a $set that touched a name without its normalised
	// twin would leave the index enforcing uniqueness on a stale value.
	existing, err := w.load(ctx, oid, scope)
	if err != nil {
		return err
	}

	merged := toMap(existing)
	for key, value := range payload {
		switch key {
		case "_id", "id", "companyId", "createdAt", "updatedAt", "deleted",
			"nameNormalised", "plateNormalised":
			// Ownership and identity are not editable. Moving an entry between
			// companies is not an edit; it is a different operation with
			// different consequences for everyone already referencing it.
		default:
			merged[key] = value
		}
	}

	var doc T
	if err := decode(merged, &doc); err != nil {
		return err
	}
	setObjectID(&doc, oid)

	if err := w.store.Update(ctx, oid, &doc); err != nil {
		if errors.Is(err, repository.ErrDuplicate) {
			return fmt.Errorf("%w: an entry with that name already exists", ErrValidation)
		}
		return err
	}
	return nil
}

// Delete removes an entry, softly.
//
// Soft because every partial unique index filters on `deleted`, so removing an
// entry frees its name for reuse while the documents that reference it keep
// resolving. A hard delete would leave orders pointing at nothing.
func (w *storeWriter[T, PT]) Delete(ctx context.Context, id string, scope *string) error {
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return ErrNotFound
	}
	if _, err := w.load(ctx, oid, scope); err != nil {
		return err
	}

	res, err := w.store.Collection().UpdateOne(ctx,
		bson.M{"_id": oid},
		bson.M{"$set": bson.M{"deleted": true}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// load fetches a document the caller is allowed to change.
//
// scope nil means platform staff, who may edit the global entries. A company
// may edit only its own: attempting a global one is ErrForbidden rather than
// ErrNotFound, because the entry is visibly there in their picker and "you
// cannot edit Karlo's list" is the useful answer.
func (w *storeWriter[T, PT]) load(ctx context.Context, oid primitive.ObjectID, scope *string) (bson.M, error) {
	var doc bson.M
	err := w.store.Collection().FindOne(ctx,
		bson.M{"_id": oid, "deleted": bson.M{"$ne": true}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	owner, _ := doc["companyId"].(string)
	switch {
	case scope == nil:
		// Platform staff may edit anything.
	case owner == "":
		return nil, fmt.Errorf("%w: this entry is maintained by Karlo and cannot be changed here", ErrForbidden)
	case owner != *scope:
		// Another company's private entry. Not found rather than forbidden —
		// they should not have been able to see it at all, and confirming it
		// exists would leak that it does.
		return nil, ErrNotFound
	}

	return doc, nil
}

func toMap(doc bson.M) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	delete(out, "_id")
	return out
}

// setObjectID stamps the id onto a typed document.
//
// The models embed Base or Owned, both of which carry the ID, but Go generics
// cannot reach an embedded field through a type parameter. A tiny interface
// assertion does it without reflection.
func setObjectID(doc any, id primitive.ObjectID) {
	if s, ok := doc.(interface{ SetID(primitive.ObjectID) }); ok {
		s.SetID(id)
	}
}

// writers is every catalogue that can be written, by the same names the read
// path uses. A kind present in catalogKinds but absent here is readable and
// not writable, which is a deliberate state rather than an oversight.
func newWriters(db *mongo.Database) map[string]writer {
	return map[string]writer{
		"brand":           newWriter[models.Brand](db),
		"cargoType":       newWriter[models.CargoType](db),
		"itemCategory":    newWriter[models.ItemCategory](db),
		"itemSubCategory": newWriter[models.ItemSubCategory](db),
		"item":            newWriter[models.Item](db),
		"truckHead":       newWriter[models.TruckHead](db),
		"truckBody":       newWriter[models.TruckBody](db),
		"truckClass":      newWriter[models.TruckClass](db),
		"vehicleGroup":    newWriter[models.VehicleGroup](db),
		"customer":        newWriter[models.Customer](db),

		// The Karlo-maintained device catalogues. Writable, but only by staff:
		// the service refuses a non-staff write because these documents carry
		// no companyId to scope one to.
		"trackerModel": newWriter[models.TrackerModel](db),
		"sensorType":   newWriter[models.SensorType](db),
	}
}
