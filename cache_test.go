package pgxrouter

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/amirsalarsafaei/pgx-router/classify"
)

func TestMapCache(t *testing.T) {
	c := NewMapCache()

	if _, ok := c.Get("SELECT 1"); ok {
		t.Fatal("expected miss on empty cache")
	}

	c.Set("SELECT 1", classify.ModeRead)

	mode, ok := c.Get("SELECT 1")
	if !ok {
		t.Fatal("expected hit after Set")
	}
	if mode != classify.ModeRead {
		t.Fatalf("Get = %v, want %v", mode, classify.ModeRead)
	}

	// Overwriting an existing key updates the stored mode.
	c.Set("SELECT 1", classify.ModeWrite)
	if mode, _ := c.Get("SELECT 1"); mode != classify.ModeWrite {
		t.Fatalf("Get after overwrite = %v, want %v", mode, classify.ModeWrite)
	}
}

// countingCache wraps a real cache to count Get/Set calls so tests can assert
// the parser is consulted at most once per distinct query.
type countingCache struct {
	store map[string]classify.QueryMode
	gets  int
	sets  int
}

func newCountingCache() *countingCache {
	return &countingCache{store: make(map[string]classify.QueryMode)}
}

func (c *countingCache) Get(sql string) (classify.QueryMode, bool) {
	c.gets++
	m, ok := c.store[sql]
	return m, ok
}

func (c *countingCache) Set(sql string, mode classify.QueryMode) {
	c.sets++
	c.store[sql] = mode
}

func TestRouteWithoutCache(t *testing.T) {
	main := &pgxpool.Pool{}
	read := &pgxpool.Pool{}
	p := New(main, read)

	if got := p.route("SELECT 1"); got != read {
		t.Error("SELECT should route to read pool")
	}
	if got := p.route("INSERT INTO t (v) VALUES (1)"); got != main {
		t.Error("INSERT should route to main pool")
	}
}

func TestRoutePopulatesAndReusesCache(t *testing.T) {
	main := &pgxpool.Pool{}
	read := &pgxpool.Pool{}
	cache := newCountingCache()
	p := New(main, read, WithCache(cache))

	const q = "SELECT * FROM users WHERE id = $1"

	// First call: miss -> classify -> store.
	if got := p.route(q); got != read {
		t.Fatal("SELECT should route to read pool")
	}
	if cache.sets != 1 {
		t.Fatalf("expected 1 Set after first route, got %d", cache.sets)
	}

	// Second call: hit -> no additional Set.
	if got := p.route(q); got != read {
		t.Fatal("cached SELECT should still route to read pool")
	}
	if cache.sets != 1 {
		t.Fatalf("expected no additional Set on cache hit, got %d", cache.sets)
	}
	if cache.gets != 2 {
		t.Fatalf("expected 2 Gets after two routes, got %d", cache.gets)
	}
}

func TestRouteHonorsCachedMode(t *testing.T) {
	main := &pgxpool.Pool{}
	read := &pgxpool.Pool{}
	cache := newCountingCache()
	p := New(main, read, WithCache(cache))

	// Pre-seed a mode that disagrees with what the parser would return; the
	// cached value must win and the parser must not be consulted.
	const q = "SELECT * FROM users"
	cache.store[q] = classify.ModeWrite

	if got := p.route(q); got != main {
		t.Fatal("cached write mode should route SELECT to main pool")
	}
	if cache.sets != 0 {
		t.Fatalf("expected no Set when value is already cached, got %d", cache.sets)
	}
}
