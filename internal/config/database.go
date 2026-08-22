package config

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// ConnectMongo opens the connection and verifies it before returning.
//
// The target is validated first. A refused target produces no connection
// attempt at all, so pointing this service at the legacy production cluster
// fails at startup without touching it. See guard.go.
func ConnectMongo(cfg *Config) (*mongo.Database, error) {
	if err := guardTarget(cfg.MongoURI, cfg.MongoDatabase); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.MongoTimeout)
	defer cancel()

	opts := options.Client().
		ApplyURI(cfg.MongoURI).
		SetConnectTimeout(cfg.MongoTimeout).
		SetServerSelectionTimeout(cfg.MongoTimeout).
		SetMaxPoolSize(50).
		SetMinPoolSize(5).
		SetMaxConnIdleTime(5 * time.Minute)

	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("config: connect mongo: %w", err)
	}

	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		return nil, fmt.Errorf("config: ping mongo: %w", err)
	}

	slog.Info("mongodb connected",
		"database", cfg.MongoDatabase,
		"collectionPrefix", CollectionPrefix,
	)
	return client.Database(cfg.MongoDatabase), nil
}

// EnsureIndexes creates the indexes the service depends on.
//
// This is the only code path that modifies the database at startup. It operates
// exclusively on prefixed collections, so it cannot alter a legacy collection
// even if it were somehow run against a shared database.
func EnsureIndexes(ctx context.Context, db *mongo.Database) error {
	type indexSpec struct {
		collection string
		model      mongo.IndexModel
	}

	// Index keys must be an ORDERED document. bson.D, not a map: a Go map has
	// no defined iteration order, and the driver refuses a multi-key map
	// outright ("multi-key map passed in for ordered parameter keys"). Order
	// also decides which queries an index can serve — {a:1, b:1} answers a
	// query on `a`, but {b:1, a:1} does not.
	unique := func(keys bson.D) mongo.IndexModel {
		return mongo.IndexModel{Keys: keys, Options: options.Index().SetUnique(true)}
	}
	plain := func(keys bson.D) mongo.IndexModel {
		return mongo.IndexModel{Keys: keys}
	}
	geo := func(field string) mongo.IndexModel {
		return mongo.IndexModel{Keys: bson.D{{Key: field, Value: "2dsphere"}}}
	}

	specs := []indexSpec{
		// Catalogue lookups are always by kind, and usually by kind plus code.
		// Uniqueness is per company, not global. Two companies may each define
		// an item type coded "BOX"; one company may not define it twice.
		// Platform-global entries store an empty companyId, so there can be
		// exactly one global "BOX" alongside any number of company ones.
		{Collection("catalog_items"), unique(bson.D{{Key: "companyId", Value: 1}, {Key: "kind", Value: 1}, {Key: "code", Value: 1}})},
		// The listing query: one kind, visible to one company (its own entries
		// plus the globals). companyId leads because it is the most selective
		// and because every read filters on it.
		{Collection("catalog_items"), plain(bson.D{{Key: "companyId", Value: 1}, {Key: "kind", Value: 1}, {Key: "active", Value: 1}, {Key: "name", Value: 1}})},
		{Collection("catalog_items"), plain(bson.D{{Key: "kind", Value: 1}, {Key: "parentId", Value: 1}})},

		// Company catalogues are always scoped to one company.
		{Collection("trucks"), unique(bson.D{{Key: "companyId", Value: 1}, {Key: "policeNumber", Value: 1}})},
		{Collection("trucks"), plain(bson.D{{Key: "companyId", Value: 1}, {Key: "deleted", Value: 1}})},
		{Collection("trucks"), plain(bson.D{{Key: "driverIds", Value: 1}})},
		{Collection("trucks"), plain(bson.D{{Key: "truckGroupId", Value: 1}})},

		{Collection("warehouses"), plain(bson.D{{Key: "companyId", Value: 1}, {Key: "deleted", Value: 1}})},
		{Collection("warehouses"), plain(bson.D{{Key: "cityId", Value: 1}})},
		// The geospatial index backs the proximity query the business service
		// uses for geofenced shipment completion.
		{Collection("warehouses"), geo("location")},

		{Collection("truck_groups"), plain(bson.D{{Key: "companyId", Value: 1}, {Key: "deleted", Value: 1}})},
		{Collection("customers"), plain(bson.D{{Key: "companyId", Value: 1}, {Key: "deleted", Value: 1}})},
		{Collection("points"), plain(bson.D{{Key: "companyId", Value: 1}})},
		{Collection("points"), geo("location")},
		{Collection("saved_routes"), plain(bson.D{{Key: "companyId", Value: 1}})},
	}

	for _, spec := range specs {
		if _, err := db.Collection(spec.collection).Indexes().CreateOne(ctx, spec.model); err != nil {
			return fmt.Errorf("config: create index on %s: %w", spec.collection, err)
		}
	}

	slog.Info("mongodb indexes ensured", "count", len(specs))
	return nil
}
