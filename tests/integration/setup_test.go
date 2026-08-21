//go:build integration

// Package integration exercises the master data repositories against a real
// MongoDB.
//
// What is covered here cannot be covered by unit tests: whether the indexes
// declared at startup actually exist, whether the 2dsphere index really answers
// a proximity query, and whether upserts are genuinely idempotent under the
// unique constraint.
//
//	go test -tags=integration ./tests/integration/... -v
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/karlo/masterdata-service/internal/config"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// testDatabaseName is deliberately not "karlo_masterdata": an integration run
// must not touch a developer's working database, and it must not be a name the
// connection guard rejects.
const testDatabaseName = "karlo_masterdata_test"

func testDB(t *testing.T) *mongo.Database {
	t.Helper()

	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("MONGO_TEST_URI is not set; skipping integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("could not connect to %s: %v", uri, err)
	}

	t.Cleanup(func() {
		disconnectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Disconnect(disconnectCtx)
	})

	return client.Database(testDatabaseName)
}

// resetCollections drops this service's collections between tests.
//
// It drops only prefixed names, so a stray run against a shared database still
// cannot remove anything the legacy system owns.
func resetCollections(t *testing.T, db *mongo.Database) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for _, name := range []string{
		"catalog_items", "trucks", "warehouses",
		"truck_groups", "customers", "points", "saved_routes",
	} {
		if err := db.Collection(config.Collection(name)).Drop(ctx); err != nil {
			t.Fatalf("could not drop %s: %v", name, err)
		}
	}

	// Recreate the indexes the service depends on, since dropping a collection
	// removes them too.
	if err := config.EnsureIndexes(ctx, db); err != nil {
		t.Fatalf("could not ensure indexes: %v", err)
	}
}

// indexNames returns the indexes present on a collection.
func indexNames(t *testing.T, db *mongo.Database, collection string) map[string]bson.M {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cur, err := db.Collection(collection).Indexes().List(ctx)
	if err != nil {
		t.Fatalf("could not list indexes on %s: %v", collection, err)
	}
	defer func() { _ = cur.Close(ctx) }()

	out := map[string]bson.M{}
	for cur.Next(ctx) {
		var idx bson.M
		if err := cur.Decode(&idx); err != nil {
			t.Fatalf("could not decode an index: %v", err)
		}
		if name, ok := idx["name"].(string); ok {
			out[name] = idx
		}
	}
	return out
}

func ctx() context.Context { return context.Background() }
