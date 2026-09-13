package config

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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

// EnsureIndexes is gone, and deliberately.
//
// It created the collections and indexes for the PREVIOUS master data shape —
// catalog_items, trucks, warehouses, customers, points, saved_routes — none of
// which exist any more. Left in place it would have RESURRECTED them at every
// startup, alongside the collections that replaced them, with indexes
// enforcing the old rules.
//
// That is the failure this service exists to prevent, in its own startup path:
// two definitions of the schema, disagreeing, with no way to tell which one a
// given deployment ended up with.
//
// The schema now has ONE source: schema/master_data.js. It creates every
// collection with a $jsonSchema validator, drops any index it does not declare,
// and exits non-zero if an index cannot be created. Run it before starting the
// service:
//
//	docker compose exec -T mongodb mongosh karlo_masterdata < schema/master_data.js
//
// It is idempotent, so running it again is safe and is how a schema change is
// applied.
