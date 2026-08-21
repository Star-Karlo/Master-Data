// Command seed loads the global catalogues into a fresh master data database.
//
// It is deliberately awkward to misuse:
//
//   - It refuses any target the connection guard rejects, so it cannot be
//     pointed at the legacy production cluster.
//   - It writes only to prefixed collections, which the legacy system does not
//     use.
//   - It performs no writes at all unless -confirm is passed. The default is a
//     dry run that reports what it would do.
//   - Writes are upserts keyed on (kind, code), so running it twice is the same
//     as running it once. It never deletes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/karlo/masterdata-service/internal/config"
	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/logger"
	"github.com/karlo/masterdata-service/internal/repository"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// seedEntry is one catalogue row as it appears in seed/catalog.json.
type seedEntry struct {
	Code        string                 `json:"code"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	SortOrder   int                    `json:"sortOrder"`
	Attributes  map[string]interface{} `json:"attributes"`
	// ParentCode links a hierarchical entry to its parent by code, since the
	// parent's generated id is not known when the file is written.
	ParentCode string `json:"parentCode"`
}

func main() {
	var (
		file    = flag.String("file", "seed/catalog.json", "path to the seed file")
		confirm = flag.Bool("confirm", false, "actually write; without this the run is a dry run")
	)
	flag.Parse()

	if err := run(*file, *confirm); err != nil {
		slog.Error("seed failed", "error", err)
		os.Exit(1)
	}
}

func run(file string, confirm bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger.InitFromEnv("masterdata-seed")
	defer logger.Close()

	// The path is a command-line flag supplied by whoever runs the seeder, not
	// input from a request.
	raw, err := os.ReadFile(file) // #nosec G304
	if err != nil {
		return fmt.Errorf("read seed file: %w", err)
	}

	var data map[string][]seedEntry
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("parse seed file: %w", err)
	}

	// Validate the whole file before touching the database, so a typo in the
	// last catalogue does not leave the first half loaded.
	total := 0
	for kind, entries := range data {
		if !models.IsValidKind(kind) {
			return fmt.Errorf("seed file names unknown catalogue %q", kind)
		}
		for i, e := range entries {
			if e.Code == "" {
				return fmt.Errorf("%s[%d]: code is required", kind, i)
			}
			if e.Name == "" {
				return fmt.Errorf("%s[%d] (%s): name is required", kind, i, e.Code)
			}
		}
		total += len(entries)
	}

	if !confirm {
		slog.Info("dry run: no changes will be written",
			"database", cfg.MongoDatabase,
			"collection", config.Collection("catalog_items"),
			"catalogues", len(data),
			"entries", total,
		)
		for kind, entries := range data {
			slog.Info("would upsert", "catalogue", kind, "entries", len(entries))
		}
		slog.Info("re-run with -confirm to apply")
		return nil
	}

	// ConnectMongo applies the legacy-target guard.
	db, err := config.ConnectMongo(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	defer func() {
		if err := db.Client().Disconnect(ctx); err != nil {
			slog.Error("disconnect failed", "error", err)
		}
	}()

	if err := config.EnsureIndexes(ctx, db); err != nil {
		return err
	}

	repo := repository.NewCatalogRepository(db)

	// Parents are loaded first so that child entries can resolve ParentCode to
	// a real id. Provinces before cities, cities before districts.
	order := orderedKinds(data)

	// codeToID accumulates the ids of everything written so far, keyed by
	// kind and code.
	codeToID := map[string]primitive.ObjectID{}
	written := 0

	for _, kind := range order {
		for _, entry := range data[kind] {
			item := &models.CatalogItem{
				Kind:        models.CatalogKind(kind),
				Code:        entry.Code,
				Name:        entry.Name,
				Description: entry.Description,
				SortOrder:   entry.SortOrder,
				Attributes:  entry.Attributes,
				Active:      true,
			}

			if entry.ParentCode != "" {
				parentKind, ok := parentKindOf(models.CatalogKind(kind))
				if !ok {
					return fmt.Errorf("%s/%s: parentCode set but %s has no parent catalogue", kind, entry.Code, kind)
				}
				parentID, ok := codeToID[string(parentKind)+":"+entry.ParentCode]
				if !ok {
					return fmt.Errorf("%s/%s: parent %s/%s not found in seed data",
						kind, entry.Code, parentKind, entry.ParentCode)
				}
				item.ParentID = &parentID
			}

			if err := repo.Upsert(ctx, item); err != nil {
				return fmt.Errorf("upsert %s/%s: %w", kind, entry.Code, err)
			}

			// Upsert fills ID only on insert. On a repeat run the row already
			// exists, so read it back to resolve children.
			if item.ID.IsZero() {
				existing, ferr := repo.FindByCode(ctx, models.CatalogKind(kind), entry.Code)
				if ferr != nil {
					return fmt.Errorf("resolve %s/%s after upsert: %w", kind, entry.Code, ferr)
				}
				item.ID = existing.ID
			}

			codeToID[kind+":"+entry.Code] = item.ID
			written++
		}
		slog.Info("seeded catalogue", "catalogue", kind, "entries", len(data[kind]))
	}

	slog.Info("seed complete", "database", cfg.MongoDatabase, "entries", written)
	return nil
}

// parentHierarchy records which catalogues nest inside which.
var parentHierarchy = map[models.CatalogKind]models.CatalogKind{
	models.KindKota:     models.KindProvinsi,
	models.KindDistrict: models.KindKota,
}

func parentKindOf(kind models.CatalogKind) (models.CatalogKind, bool) {
	parent, ok := parentHierarchy[kind]
	return parent, ok
}

// orderedKinds returns the catalogues in dependency order: a catalogue whose
// entries reference a parent is loaded after that parent.
func orderedKinds(data map[string][]seedEntry) []string {
	var (
		out      []string
		deferred []string
	)

	for kind := range data {
		if _, hasParent := parentHierarchy[models.CatalogKind(kind)]; hasParent {
			deferred = append(deferred, kind)
			continue
		}
		out = append(out, kind)
	}

	// Two levels of nesting is all the hierarchy has (province, city,
	// district), so one pass of deferral is enough. Sorting by depth keeps
	// cities ahead of districts.
	depth := func(kind string) int {
		d := 0
		k := models.CatalogKind(kind)
		for {
			parent, ok := parentHierarchy[k]
			if !ok {
				return d
			}
			d++
			k = parent
		}
	}
	for i := 0; i < len(deferred); i++ {
		for j := i + 1; j < len(deferred); j++ {
			if depth(deferred[j]) < depth(deferred[i]) {
				deferred[i], deferred[j] = deferred[j], deferred[i]
			}
		}
	}

	return append(out, deferred...)
}
