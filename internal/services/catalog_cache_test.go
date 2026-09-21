package services

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karlo/masterdata-service/internal/platform/cache"
	"github.com/karlo/masterdata-service/internal/platform/query"
)

// memCache is the smallest Cache that records what was written and deleted,
// enough to prove the scoping rules without Redis.
type memCache struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemCache() *memCache { return &memCache{data: map[string][]byte{}} }

func (m *memCache) Get(_ context.Context, key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	return v, ok
}
func (m *memCache) Set(_ context.Context, key string, value []byte, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
}
func (m *memCache) Delete(_ context.Context, keys ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.data, k)
	}
}
func (m *memCache) DeleteByPrefix(_ context.Context, prefix string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			delete(m.data, k)
		}
	}
}
func (m *memCache) Increment(context.Context, string, time.Duration) (int64, error) {
	return 0, cache.ErrNoCache
}
func (m *memCache) SetIfAbsent(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, cache.ErrNoCache
}
func (m *memCache) Publish(context.Context, string, []byte) {}

func (m *memCache) Ping(context.Context) error { return nil }
func (m *memCache) Close() error               { return nil }

func TestCatalogKeysAreTenantScoped(t *testing.T) {
	p := query.Params{Page: 0, PageSize: 20}
	a := catalogListKey(catalogScope("companyA", false), "truckType", "", p)
	b := catalogListKey(catalogScope("companyB", false), "truckType", "", p)
	g := catalogListKey(catalogScope("", false), "truckType", "", p)
	s := catalogListKey(catalogScope("companyA", true), "truckType", "", p)

	for _, pair := range [][2]string{{a, b}, {a, g}, {a, s}, {g, s}} {
		if pair[0] == pair[1] {
			t.Fatalf("keys must differ by scope: %s", pair[0])
		}
	}
	if !strings.Contains(a, ":companyA:") || !strings.Contains(g, ":global:") || !strings.Contains(s, ":staff:") {
		t.Fatalf("scope segment missing: %s / %s / %s", a, g, s)
	}
}

func TestCatalogListKeyVariesWithQuery(t *testing.T) {
	base := query.Params{Page: 0, PageSize: 20}
	k1 := catalogListKey("global", "truckType", "", base)
	k2 := catalogListKey("global", "truckType", "", query.Params{Page: 1, PageSize: 20})
	k3 := catalogListKey("global", "truckType", "", query.Params{Page: 0, PageSize: 20, Search: "tro"})
	k4 := catalogListKey("global", "truckType", "parent1", base)
	k5 := catalogListKey("global", "truckType", "", query.Params{Page: 0, PageSize: 20, Sorts: []query.Sort{{Field: "name", Desc: true}}})

	seen := map[string]bool{}
	for _, k := range []string{k1, k2, k3, k4, k5} {
		if seen[k] {
			t.Fatalf("duplicate key for distinct query: %s", k)
		}
		seen[k] = true
	}
	if catalogListKey("global", "truckType", "", base) != k1 {
		t.Fatal("same query must produce the same key")
	}
}

func TestInvalidateScopes(t *testing.T) {
	ctx := context.Background()
	m := newMemCache()
	svc := &CatalogService{cache: m}
	p := query.Params{PageSize: 20}

	seed := func() {
		m.data = map[string][]byte{}
		m.Set(ctx, catalogListKey("companyA", "truckType", "", p), []byte("a"), 0)
		m.Set(ctx, catalogListKey("companyA", "cargoType", "", p), []byte("a2"), 0)
		m.Set(ctx, catalogListKey("companyB", "truckType", "", p), []byte("b"), 0)
		m.Set(ctx, catalogListKey("staff", "truckType", "", p), []byte("s"), 0)
		m.Set(ctx, catalogEntryKey("companyA", "truckType", "id1"), []byte("e"), 0)
	}

	// A company's write clears only its own kind, and the staff view of it.
	seed()
	svc.invalidate(ctx, "truckType", "companyA", false)
	if _, ok := m.Get(ctx, catalogListKey("companyA", "truckType", "", p)); ok {
		t.Fatal("companyA truckType listing should be gone")
	}
	if _, ok := m.Get(ctx, catalogEntryKey("companyA", "truckType", "id1")); ok {
		t.Fatal("companyA truckType entry should be gone")
	}
	if _, ok := m.Get(ctx, catalogListKey("staff", "truckType", "", p)); ok {
		t.Fatal("staff truckType listing should be gone")
	}
	if _, ok := m.Get(ctx, catalogListKey("companyB", "truckType", "", p)); !ok {
		t.Fatal("companyB must not be touched by companyA's write")
	}
	if _, ok := m.Get(ctx, catalogListKey("companyA", "cargoType", "", p)); !ok {
		t.Fatal("another kind must not be touched")
	}

	// A staff (possibly global) write sweeps everything.
	seed()
	svc.invalidate(ctx, "truckType", "", true)
	if len(m.data) != 0 {
		t.Fatalf("staff write should sweep the catalogue cache, %d keys left", len(m.data))
	}
}
