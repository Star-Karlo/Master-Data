//go:build integration

// Integration tests run against a real MongoDB, because the rules worth
// testing here are the ones the database enforces or the ones that span
// several documents — neither of which a mock can tell you about.
//
//	MONGO_TEST_URI=mongodb://localhost:27017 go test -tags=integration ./tests/integration/...
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func ctx() context.Context { return context.Background() }

// testDB is a fresh, empty database per test. Skipped, not failed, when no
// MongoDB is offered — a unit run should not need one.
func testDB(t *testing.T) *mongo.Database {
	t.Helper()
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("MONGO_TEST_URI not set")
	}
	c, cancel := context.WithTimeout(ctx(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(c, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	db := client.Database("karlo_masterdata_test_" + time.Now().Format("150405") + fmt.Sprintf("%03d", time.Now().Nanosecond()/1e6))
	t.Cleanup(func() {
		_ = db.Drop(ctx())
		_ = client.Disconnect(ctx())
	})
	return db
}
