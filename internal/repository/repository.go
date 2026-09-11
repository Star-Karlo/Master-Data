// Package repository reads and writes the master data collections.
//
// One generic store rather than a bespoke repository per collection. The
// collections share a shape — a tenant key, a soft delete, timestamps, and a
// set of normalised fields — and eighteen near-identical files would be
// eighteen places for the same mistake.
//
// The important part is Create and Update: both call BeforeWrite, which fills
// the normalised fields that every unique index is built on. That makes this
// package the single place the guarantee lives. A caller that writes to a
// collection directly, bypassing the store, produces a document with those
// fields missing — the partial index then ignores it, and the duplicate the
// whole schema exists to prevent is created with no error at all.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	ErrNotFound = errors.New("repository: not found")

	// ErrDuplicate is returned when a unique index refuses a write.
	//
	// Surfaced as its own error because it is the schema working, not a fault:
	// the caller should tell the operator "that vehicle is already registered",
	// not "internal error".
	ErrDuplicate = errors.New("repository: already exists")
)

// Document is what the store can write.
//
// BeforeWrite fills the normalised twins. Every model that has one implements
// this, and the store calls it — so the rule cannot be forgotten at a call
// site, only by omitting it from the model itself.
type Document interface {
	CollectionName() string
	BeforeWrite()
}

// Validated is implemented by models with a rule the database cannot express —
// "a head carries nothing" being the one that exists today. The store calls it
// when present.
type Validated interface {
	Validate() error
}

// Store is a typed handle on one collection.
//
// Two type parameters, not one, and the reason is BeforeWrite. It must take a
// POINTER receiver — it mutates the document to fill the normalised fields — so
// a value type does not satisfy Document, while a pointer type makes `var zero
// T` a nil pointer that CollectionName cannot be called on. This is the
// standard Go answer: T is the value type, PT constrains *T to the interface,
// and PT(&zero) is a real addressable document.
//
// Call it as NewStore[models.Vehicle](db) — PT is inferred.
type Store[T any, PT interface {
	*T
	Document
}] struct {
	col *mongo.Collection
}

func NewStore[T any, PT interface {
	*T
	Document
}](db *mongo.Database) *Store[T, PT] {
	var zero T
	return &Store[T, PT]{col: db.Collection(PT(&zero).CollectionName())}
}

// Collection exposes the driver handle for queries the store does not cover.
func (s *Store[T, PT]) Collection() *mongo.Collection { return s.col }

// Create inserts one document.
func (s *Store[T, PT]) Create(ctx context.Context, doc *T) error {
	// Through PT, so the pointer-receiver BeforeWrite actually runs on the
	// caller's document. Calling it on a copy would fill the normalised fields
	// of something that is then discarded — the partial unique indexes would
	// ignore the inserted document and a duplicate would be created with no
	// error at all.
	PT(doc).BeforeWrite()
	if v, ok := any(doc).(Validated); ok {
		if err := v.Validate(); err != nil {
			return err
		}
	}
	stampCreate(doc)

	if _, err := s.col.InsertOne(ctx, doc); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("%w: %v", ErrDuplicate, err)
		}
		return fmt.Errorf("repository: create: %w", err)
	}
	return nil
}

// Update replaces one document.
//
// A full replace rather than a partial $set, deliberately: a $set that touched
// a name without touching its normalised twin would leave the two disagreeing,
// and the index would then be enforcing uniqueness on a stale value.
func (s *Store[T, PT]) Update(ctx context.Context, id primitive.ObjectID, doc *T) error {
	PT(doc).BeforeWrite()
	if v, ok := any(doc).(Validated); ok {
		if err := v.Validate(); err != nil {
			return err
		}
	}
	stampUpdate(doc)

	res, err := s.col.ReplaceOne(ctx, bson.M{"_id": id}, doc)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("%w: %v", ErrDuplicate, err)
		}
		return fmt.Errorf("repository: update: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// FindByID reads one document, scoped to a tenant.
//
// companyID is a REQUIRED argument rather than an optional filter. There is no
// cross-database foreign key to the authentication service, so tenancy is
// enforced by every query carrying it — and a signature that lets a caller omit
// it is a signature that will eventually be called without it.
//
// Pass an empty companyID only for the global reference lists, where a row with
// no company is the shared entry every tenant reads.
func (s *Store[T, PT]) FindByID(ctx context.Context, companyID string, id primitive.ObjectID) (*T, error) {
	filter := bson.M{"_id": id, "deleted": bson.M{"$ne": true}}
	if companyID != "" {
		filter["$or"] = []bson.M{{"companyId": companyID}, {"companyId": nil}}
	}

	var out T
	if err := s.col.FindOne(ctx, filter).Decode(&out); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("repository: find: %w", err)
	}
	return &out, nil
}

// List pages documents for a tenant, including the global entries.
func (s *Store[T, PT]) List(ctx context.Context, companyID string, extra bson.M, skip, limit int64, sort bson.D) ([]T, int64, error) {
	filter := bson.M{"deleted": bson.M{"$ne": true}}
	for k, v := range extra {
		filter[k] = v
	}
	if companyID != "" {
		filter["$or"] = []bson.M{{"companyId": companyID}, {"companyId": nil}}
	}

	total, err := s.col.CountDocuments(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("repository: count: %w", err)
	}

	opts := options.Find().SetSkip(skip).SetLimit(limit)
	if len(sort) > 0 {
		opts.SetSort(sort)
	}
	cur, err := s.col.Find(ctx, filter, opts)
	if err != nil {
		return nil, 0, fmt.Errorf("repository: list: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()

	out := []T{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, 0, fmt.Errorf("repository: decode: %w", err)
	}
	return out, total, nil
}

// SoftDelete marks a document deleted.
//
// Marked rather than removed because every partial unique index filters on
// `deleted` — so deleting a vehicle frees its plate for reuse, which is what an
// operator expects when a truck is sold on, while the record of it remains.
func (s *Store[T, PT]) SoftDelete(ctx context.Context, companyID string, id primitive.ObjectID) error {
	filter := bson.M{"_id": id}
	if companyID != "" {
		filter["companyId"] = companyID
	}
	res, err := s.col.UpdateOne(ctx, filter,
		bson.M{"$set": bson.M{"deleted": true, "updatedAt": time.Now().UTC()}})
	if err != nil {
		return fmt.Errorf("repository: delete: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// stampCreate and stampUpdate maintain the timestamps.
//
// MongoDB has no equivalent of the trigger that did this in PostgreSQL, so a
// raw write leaves updatedAt untouched and "what changed recently" silently
// misses rows. Doing it here means every write through the store is stamped.
func stampCreate(doc any) {
	now := time.Now().UTC()
	if b, ok := doc.(interface{ Stamp(time.Time, bool) }); ok {
		b.Stamp(now, true)
	}
}

func stampUpdate(doc any) {
	now := time.Now().UTC()
	if b, ok := doc.(interface{ Stamp(time.Time, bool) }); ok {
		b.Stamp(now, false)
	}
}
