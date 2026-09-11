// Package services holds the master data business rules.
package services

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/cache"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/repository"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ErrInvalidKind is returned when a caller names a catalogue that does not
// exist. It is a client error, not a lookup miss.
var ErrInvalidKind = errors.New("unknown catalogue")

// CatalogService serves the global catalogues through a read-through cache.
//
// Catalogues are the best caching candidate in the platform: every order form
// loads half a dozen of them, they are shared by every company, and they change
// perhaps monthly. They are also completely non-sensitive — a truck type is not
// anybody's commercial data.
//
// The cache is Redis rather than in-process. An in-process cache would work,
// but with several Fargate tasks behind a load balancer it means N copies with
// N independent TTLs, and an edit that has to expire out of all of them
// separately. One shared cache gives a higher hit rate on a cold start and lets
// a write invalidate for everyone at once.
type CatalogService struct {
	repo  *repository.CatalogRepository
	cache cache.Cache
	ttl   time.Duration
}

func NewCatalogService(repo *repository.CatalogRepository, c cache.Cache, ttl time.Duration) *CatalogService {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &CatalogService{repo: repo, cache: c, ttl: ttl}
}

// cachedList is what a cached listing stores. The total is kept alongside the
// items because a client rendering a paginated table needs both, and fetching
// the count separately would defeat the point.
type cachedList struct {
	Items []models.CatalogItem `json:"items"`
	Total int64                `json:"total"`
}

// Get resolves one catalogue entry, read-through.
func (s *CatalogService) Get(ctx context.Context, companyID, kind string, id primitive.ObjectID) (*models.CatalogItem, error) {
	if !models.IsValidKind(kind) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidKind, kind)
	}

	// The company is part of the cache key.
	//
	// Without it, company A's request would populate an entry that company B
	// then reads — serving one tenant another's private catalogue entry from
	// cache, which no amount of correct database scoping would prevent.
	key := cache.Key("masterdata", "catalog", scopeOf(companyID), kind, "id", id.Hex())

	var cached models.CatalogItem
	if cache.GetJSON(ctx, s.cache, key, &cached) {
		return &cached, nil
	}

	item, err := s.repo.FindByID(ctx, companyID, models.CatalogKind(kind), id)
	if err != nil {
		// A miss is not cached. Negative caching would need its own invalidation
		// path, and a catalogue lookup that misses is already rare.
		return nil, err
	}

	cache.SetJSON(ctx, s.cache, key, item, s.ttl)
	return item, nil
}

// List pages a catalogue, read-through.
//
// Every page shape is cached, not just the first: the key carries the page,
// size, parent and a fingerprint of the filters, so two different queries
// cannot collide. Prefix invalidation on write means the number of distinct
// shapes does not have to be bounded by hand.
func (s *CatalogService) List(ctx context.Context, companyID, kind string, parentID *primitive.ObjectID, p query.Params) ([]models.CatalogItem, int64, error) {
	if !models.IsValidKind(kind) {
		return nil, 0, fmt.Errorf("%w: %q", ErrInvalidKind, kind)
	}

	key := s.listKey(companyID, kind, parentID, p)

	var cached cachedList
	if cache.GetJSON(ctx, s.cache, key, &cached) {
		return cached.Items, cached.Total, nil
	}

	items, total, err := s.repo.List(ctx, companyID, models.CatalogKind(kind), parentID, p)
	if err != nil {
		return nil, 0, err
	}

	cache.SetJSON(ctx, s.cache, key, cachedList{Items: items, Total: total}, s.ttl)
	return items, total, nil
}

// listKey builds a key that distinguishes every query shape.
//
// The filters and sorts are fingerprinted rather than embedded: they are
// caller-supplied, arbitrarily long, and would otherwise produce unbounded key
// lengths. The hash is deterministic, so the same query always hits the same
// entry.
func (s *CatalogService) listKey(companyID, kind string, parentID *primitive.ObjectID, p query.Params) string {
	parent := "root"
	if parentID != nil {
		parent = parentID.Hex()
	}

	shape := fmt.Sprintf("f=%v|s=%v|q=%s", p.Filters, p.Sorts, p.Search)

	return cache.Key("masterdata", "catalog", scopeOf(companyID), kind, "list",
		parent,
		strconv.Itoa(p.Page),
		strconv.Itoa(p.PageSize),
		cache.Fingerprint(shape),
	)
}

// Resolve batch-resolves references across catalogues.
//
// Deliberately uncached: a batch is a different set of ids every time, so the
// hit rate would be near zero while the writes churned the keyspace. The
// per-item Get cache already covers the entries this reads.
func (s *CatalogService) Resolve(ctx context.Context, companyID string, refs []repository.CatalogRef) ([]models.CatalogItem, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	return s.repo.FindManyByRefs(ctx, companyID, refs)
}

// Validate reports which of the given references do not exist.
//
// Also uncached, and for a stronger reason than Resolve: this gates a write in
// the business service. Answering "that reference is valid" from a stale cache
// after the entry was deleted would let a bad reference into an order.
func (s *CatalogService) Validate(ctx context.Context, companyID string, refs []repository.CatalogRef) ([]repository.CatalogRef, error) {
	found, err := s.repo.ExistingIDs(ctx, companyID, refs)
	if err != nil {
		return nil, err
	}

	var invalid []repository.CatalogRef
	for _, ref := range refs {
		if !found[string(ref.Kind)+":"+ref.ID.Hex()] {
			invalid = append(invalid, ref)
		}
	}
	return invalid, nil
}

// Upsert writes a catalogue entry and invalidates everything derived from it.
//
// The invalidation is explicit rather than left to the TTL because a catalogue
// edit is usually made by someone who then reloads the page to check it. Waiting
// fifteen minutes to see your own change is the kind of thing that makes people
// stop trusting the system.
func (s *CatalogService) Upsert(ctx context.Context, item *models.CatalogItem) error {
	if !models.IsValidKind(string(item.Kind)) {
		return fmt.Errorf("%w: %q", ErrInvalidKind, item.Kind)
	}

	// A company may only extend catalogues that are meant to vary. It does not
	// get its own list of Indonesian provinces, and letting it define its own
	// currency codes would break every integration that reads them.
	if !item.IsGlobal() && !models.AllowsCompanyEntries(item.Kind) {
		return fmt.Errorf("%w: %q is platform-wide and cannot be extended per company",
			ErrValidation, item.Kind)
	}

	if err := s.repo.Upsert(ctx, item); err != nil {
		return err
	}

	scope := scopeOf(item.CompanyID)

	// Every listing of this kind within this scope, and the item's own entry.
	s.cache.DeleteByPrefix(ctx, cache.Prefix("masterdata", "catalog", scope, string(item.Kind), "list"))
	s.cache.Delete(ctx, cache.Key("masterdata", "catalog", scope, string(item.Kind), "id", item.ID.Hex()))

	// A change to a GLOBAL entry is visible to every company, so every
	// company's cached listing of that kind is now stale. Sweeping the whole
	// kind is blunt, but a global catalogue edit is rare and serving a stale
	// one to some tenants and not others is worse than a brief cache miss.
	if item.IsGlobal() {
		s.cache.DeleteByPrefix(ctx, cache.Prefix("masterdata", "catalog"))
	}

	return nil
}

// scopeOf renders a company id for use in a cache key.
//
// Global entries get a literal token rather than an empty string, so a key
// never contains an empty segment that could collide with another shape.
func scopeOf(companyID string) string {
	if companyID == "" {
		return "global"
	}
	return companyID
}
