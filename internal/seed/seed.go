// Package seed loads the Karlo-maintained reference lists — truck types,
// pricing and payment types, currencies, cargo and item types, provinces and
// cities — from seed/catalog.json into the catalogue collections.
//
// Idempotent: entries are matched by (collection, code) and upserted, so
// running it again after editing the file changes what changed and nothing
// else. Documents are global (no companyId), which is what makes them visible
// to every company; a company's own additions live beside them with a
// companyId and are never touched here.
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/karlo/masterdata-service/internal/platform/normalise"
)

// collections maps a seed-file kind onto its collection. Kinds absent here
// are skipped with a warning rather than written somewhere by guess.
var collections = map[string]string{
	"truckType":     "truck_types",
	"truckBody":     "truck_bodies",
	"truckHead":     "truck_heads",
	"truckClass":    "truck_classes",
	"brand":         "brands",
	"cargoType":     "cargo_types",
	"itemType":      "item_types",
	"itemCharacter": "item_characters",
	"paymentType":   "payment_types",
	"pricingType":   "pricing_types",
	"requirement":   "requirements",
	"currency":      "currencies",
	"provinsi":      "provinces",
	"kota":          "cities",
}

type entry struct {
	Code       string         `json:"code"`
	Name       string         `json:"name"`
	ParentCode string         `json:"parentCode"`
	SortOrder  int            `json:"sortOrder"`
	Attributes map[string]any `json:"attributes"`
}

// Run upserts every kind in the file. dryRun reports and writes nothing.
func Run(ctx context.Context, db *mongo.Database, path string, dryRun bool) error {
	raw, err := os.ReadFile(path) //nolint:gosec // an operator-supplied path on a one-off task, not user input
	if err != nil {
		return fmt.Errorf("seed: reading %s: %w", path, err)
	}
	var file map[string][]entry
	if err := json.Unmarshal(raw, &file); err != nil {
		return fmt.Errorf("seed: parsing %s: %w", path, err)
	}

	// Provinces first, so a city can point at its province's id.
	order := []string{"provinsi", "kota"}
	for k := range file {
		if k != "provinsi" && k != "kota" {
			order = append(order, k)
		}
	}
	provinceIDs := map[string]any{}

	now := time.Now()
	for _, kind := range order {
		entries, ok := file[kind]
		if !ok {
			continue
		}
		coll, known := collections[kind]
		if !known {
			slog.Warn("seed: no collection for kind, skipped", "kind", kind, "entries", len(entries))
			continue
		}
		c := db.Collection(coll)
		written := 0
		for _, e := range entries {
			if e.Code == "" || e.Name == "" {
				return fmt.Errorf("seed: %s entry without code or name: %+v", kind, e)
			}
			set := bson.M{
				"name":           e.Name,
				"nameNormalised": normalise.Name(e.Name),
				"code":           e.Code,
				"sortOrder":      e.SortOrder,
				"isActive":       true,
				"deleted":        false,
				"updatedAt":      now,
			}
			if len(e.Attributes) > 0 {
				set["attributes"] = e.Attributes
			}
			if e.ParentCode != "" {
				set["parentCode"] = e.ParentCode
				if kind == "kota" {
					if pid, ok := provinceIDs[e.ParentCode]; ok {
						set["provinceId"] = pid
					}
				}
			}
			if dryRun {
				written++
				continue
			}
			// Match by code, or by normalised name for a global entry that
			// predates codes (the brands the migration created), so a seed
			// never duplicates what an earlier hand has already written.
			res := c.FindOneAndUpdate(ctx,
				bson.M{"companyId": nil, "$or": []bson.M{{"code": e.Code}, {"nameNormalised": normalise.Name(e.Name)}}},
				bson.M{"$set": set, "$setOnInsert": bson.M{"companyId": nil, "createdAt": now}},
				options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After))
			var doc bson.M
			if err := res.Decode(&doc); err != nil {
				return fmt.Errorf("seed: %s %s: %w", kind, e.Code, err)
			}
			if kind == "provinsi" {
				provinceIDs[e.Code] = doc["_id"].(interface{ Hex() string }).Hex()
			}
			written++
		}
		slog.Info("seed: kind done", "kind", kind, "collection", coll, "entries", written, "dryRun", dryRun)
	}
	return nil
}
