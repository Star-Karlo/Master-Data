// Package services holds the master data business rules.
package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/query"
	"github.com/karlo/masterdata-service/internal/repository"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ErrInvalidKind is returned when a caller names a catalogue that does not
// exist. It is a client error, not a lookup miss.
var ErrInvalidKind = errors.New("unknown catalogue")

// CatalogService serves the global catalogues, with a small read-through cache.
//
// Catalogues are read constantly (every order form loads half a dozen) and
// written almost never. Caching them in process removes that load without
// introducing a separate cache tier; the cost is that an edit takes up to one
// TTL to appear everywhere, which is acceptable for reference data.
type CatalogService struct {
	repo *repository.CatalogRepository
	ttl  time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	items     []models.CatalogItem
	total     int64
	expiresAt time.Time
}

func NewCatalogService(repo *repository.CatalogRepository, ttl time.Duration) *CatalogService {
	return &CatalogService{
		repo:  repo,
		ttl:   ttl,
		cache: make(map[string]cacheEntry),
	}
}

// Get resolves one catalogue entry.
func (s *CatalogService) Get(ctx context.Context, kind string, id primitive.ObjectID) (*models.CatalogItem, error) {
	if !models.IsValidKind(kind) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidKind, kind)
	}
	return s.repo.FindByID(ctx, models.CatalogKind(kind), id)
}

// List pages a catalogue, serving unfiltered first pages from cache.
func (s *CatalogService) List(ctx context.Context, kind string, parentID *primitive.ObjectID, p query.Params) ([]models.CatalogItem, int64, error) {
	if !models.IsValidKind(kind) {
		return nil, 0, fmt.Errorf("%w: %q", ErrInvalidKind, kind)
	}

	// Only the plain listing is cached. A filtered or searched request has too
	// many shapes to cache usefully, and caching them would mostly evict the
	// entries that do get reused.
	cacheable := len(p.Filters) == 0 && p.Search == "" && p.Page == 0
	key := cacheKey(kind, parentID, p)

	if cacheable {
		if items, total, ok := s.fromCache(key); ok {
			return items, total, nil
		}
	}

	items, total, err := s.repo.List(ctx, models.CatalogKind(kind), parentID, p)
	if err != nil {
		return nil, 0, err
	}

	if cacheable {
		s.store(key, items, total)
	}
	return items, total, nil
}

// Resolve batch-resolves references across catalogues.
func (s *CatalogService) Resolve(ctx context.Context, refs []repository.CatalogRef) ([]models.CatalogItem, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	return s.repo.FindManyByRefs(ctx, refs)
}

// Validate reports which of the given references do not exist. The business
// service calls this before committing an order, so a bad reference is caught
// at write time rather than surfacing as a blank field on a screen later.
func (s *CatalogService) Validate(ctx context.Context, refs []repository.CatalogRef) ([]repository.CatalogRef, error) {
	found, err := s.repo.ExistingIDs(ctx, refs)
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

// Upsert writes a catalogue entry and drops the cached listings for its kind.
func (s *CatalogService) Upsert(ctx context.Context, item *models.CatalogItem) error {
	if !models.IsValidKind(string(item.Kind)) {
		return fmt.Errorf("%w: %q", ErrInvalidKind, item.Kind)
	}
	if err := s.repo.Upsert(ctx, item); err != nil {
		return err
	}
	s.invalidate(string(item.Kind))
	return nil
}

func (s *CatalogService) fromCache(key string) ([]models.CatalogItem, int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.cache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, 0, false
	}
	return entry.items, entry.total, true
}

func (s *CatalogService) store(key string, items []models.CatalogItem, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cache[key] = cacheEntry{
		items:     items,
		total:     total,
		expiresAt: time.Now().Add(s.ttl),
	}
}

// invalidate drops every cached listing for one kind.
func (s *CatalogService) invalidate(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key := range s.cache {
		if len(key) >= len(kind) && key[:len(kind)] == kind {
			delete(s.cache, key)
		}
	}
}

func cacheKey(kind string, parentID *primitive.ObjectID, p query.Params) string {
	key := kind + "|"
	if parentID != nil {
		key += parentID.Hex()
	}
	return key + "|" + fmt.Sprintf("%d:%d", p.Page, p.PageSize)
}
