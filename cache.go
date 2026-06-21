package pgxrouter

import (
	"sync"

	"github.com/amirsalarsafaei/pgx-router/classify"
)

// Cache stores the read/write classification of a SQL statement keyed by its
// text, so the PostgreSQL parser only runs once per distinct query instead of
// on every execution.
//
// Implementations must be safe for concurrent use: a Pool may call Get and Set
// from multiple goroutines simultaneously.
type Cache interface {
	// Get returns the cached mode for sql and whether it was present.
	Get(sql string) (mode classify.QueryMode, ok bool)
	// Set records the classified mode for sql.
	Set(sql string, mode classify.QueryMode)
}

// MapCache is an unbounded, concurrency-safe Cache backed by a sync.Map.
//
// It suits applications that issue a bounded set of distinct query strings —
// e.g. parameterized queries using $1, $2 placeholders. Avoid it for workloads
// that interpolate literal values into the SQL text, since the number of
// distinct keys (and therefore memory use) is then unbounded; supply your own
// Cache implementation (e.g. a bounded LRU) via WithCache in that case.
type MapCache struct {
	m sync.Map // map[string]classify.QueryMode
}

// NewMapCache returns a ready-to-use MapCache.
func NewMapCache() *MapCache {
	return &MapCache{}
}

func (c *MapCache) Get(sql string) (classify.QueryMode, bool) {
	v, ok := c.m.Load(sql)
	if !ok {
		return 0, false
	}
	return v.(classify.QueryMode), true
}

func (c *MapCache) Set(sql string, mode classify.QueryMode) {
	c.m.Store(sql, mode)
}
