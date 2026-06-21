package pgxrouter

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/amirsalarsafaei/pgx-router/classify"
)

// Option configures the Pool behaviour.
type Option func(*poolConfig)

type poolConfig struct {
	retryOnError func(error) bool
	cache        Cache
}

func WithRetryOnError(fn func(error) bool) Option {
	return func(c *poolConfig) {
		c.retryOnError = fn
	}
}

// WithCache enables caching of query-mode classification results. The cache is
// consulted before parsing each SQL statement and populated with the result,
// so the PostgreSQL parser runs at most once per distinct query.
//
// Pass NewMapCache() for a simple built-in cache, or supply your own Cache
// implementation (e.g. a bounded LRU) for custom eviction behaviour. Caching is
// disabled by default.
func WithCache(c Cache) Option {
	return func(cfg *poolConfig) {
		cfg.cache = c
	}
}

type Pool struct {
	*pgxpool.Pool
	read *pgxpool.Pool
	cfg  poolConfig
}

// New creates a routing pool over a main (read-write) and a separate read
// (read-only) pool. Both pools are required and must be distinct — the router
// is built for primary/replica setups and has no single-pool mode.
func New(main, read *pgxpool.Pool, opts ...Option) *Pool {
	switch {
	case main == nil || read == nil:
		panic("pgxrouter: New requires both a main and a read pool")
	case main == read:
		panic("pgxrouter: main and read must be distinct pools")
	}
	var cfg poolConfig
	for _, o := range opts {
		o(&cfg)
	}
	return &Pool{Pool: main, read: read, cfg: cfg}
}

func (p *Pool) ReadPool() *pgxpool.Pool { return p.read }

func (p *Pool) MainPool() *pgxpool.Pool { return p.Pool }

func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	pool := p.route(sql)
	tag, err := pool.Exec(ctx, sql, args...)
	if pool == p.read && p.shouldRetryOnMain(err) {
		return p.Pool.Exec(ctx, sql, args...)
	}
	return tag, err
}

func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	pool := p.route(sql)
	rows, err := pool.Query(ctx, sql, args...)
	if pool == p.read {
		if p.shouldRetryOnMain(err) {
			return p.Pool.Query(ctx, sql, args...)
		}
		if err == nil {
			return &retryRows{ctx: ctx, sql: sql, args: args, pool: p, rows: rows, routed: pool}, nil
		}
	}
	return rows, err
}

func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	pool := p.route(sql)
	return &retryRow{ctx: ctx, sql: sql, args: args, pool: p, row: pool.QueryRow(ctx, sql, args...), routed: pool}
}

func (p *Pool) Close() {
	p.read.Close()
	p.Pool.Close()
}

func (p *Pool) Reset() {
	p.read.Reset()
	p.Pool.Reset()
}

func (p *Pool) route(sql string) *pgxpool.Pool {
	if p.classify(sql) == classify.ModeRead {
		return p.read
	}
	return p.Pool
}

// classify determines the query mode for sql, consulting and populating the
// optional cache when one is configured.
func (p *Pool) classify(sql string) classify.QueryMode {
	if p.cfg.cache != nil {
		if mode, ok := p.cfg.cache.Get(sql); ok {
			return mode
		}
	}

	mode := classify.Classify(sql, extractLeadingComments(sql))

	if p.cfg.cache != nil {
		p.cfg.cache.Set(sql, mode)
	}
	return mode
}

// shouldRetryOnMain returns true when an error from the read replica
// warrants retrying on the main pool.
func (p *Pool) shouldRetryOnMain(err error) bool {
	if err == nil {
		return false
	}

	// Always consult custom retry logic (if provided), then also apply
	// built-in read-only detection so fallback-to-main remains robust.
	customRetry := false
	if p.cfg.retryOnError != nil {
		customRetry = p.cfg.retryOnError(err)
	}

	return customRetry || isReadOnlyError(err)
}

// retryRow wraps a pgx.Row and retries on the main pool if the read replica
// returns an error that should be retried.
type retryRow struct {
	ctx    context.Context
	sql    string
	args   []any
	pool   *Pool
	row    pgx.Row
	routed *pgxpool.Pool
}

func (r *retryRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if r.routed == r.pool.read && r.pool.shouldRetryOnMain(err) {
		return r.pool.Pool.QueryRow(r.ctx, r.sql, r.args...).Scan(dest...)
	}
	return err
}

// retryRows wraps pgx.Rows so deferred read-only errors from the read replica
// can still fallback to main when first observed during iteration.
type retryRows struct {
	ctx       context.Context
	sql       string
	args      []any
	pool      *Pool
	rows      pgx.Rows
	routed    *pgxpool.Pool
	retried   bool
	overrideE error
}

func (r *retryRows) Close() {
	r.rows.Close()
}

func (r *retryRows) Err() error {
	if r.overrideE != nil {
		return r.overrideE
	}
	return r.rows.Err()
}

func (r *retryRows) CommandTag() pgconn.CommandTag {
	return r.rows.CommandTag()
}

func (r *retryRows) FieldDescriptions() []pgconn.FieldDescription {
	return r.rows.FieldDescriptions()
}

func (r *retryRows) Next() bool {
	if r.rows.Next() {
		return true
	}
	if r.retried || r.routed != r.pool.read {
		return false
	}
	if !r.pool.shouldRetryOnMain(r.rows.Err()) {
		return false
	}

	r.rows.Close()
	mainRows, err := r.pool.Pool.Query(r.ctx, r.sql, r.args...)
	r.retried = true
	if err != nil {
		r.overrideE = err
		return false
	}

	r.rows = mainRows
	return r.rows.Next()
}

func (r *retryRows) Scan(dest ...any) error {
	return r.rows.Scan(dest...)
}

func (r *retryRows) Values() ([]any, error) {
	return r.rows.Values()
}

func (r *retryRows) RawValues() [][]byte {
	return r.rows.RawValues()
}

func (r *retryRows) Conn() *pgx.Conn {
	return r.rows.Conn()
}

func isReadOnlyError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgerrcode.ReadOnlySQLTransaction
	}
	return false
}

func extractLeadingComments(sql string) []string {
	var comments []string
	s := strings.TrimSpace(sql)
	for {
		if strings.HasPrefix(s, "--") {
			end := strings.IndexByte(s, '\n')
			if end == -1 {
				comments = append(comments, s)
				break
			}
			comments = append(comments, s[:end])
			s = strings.TrimSpace(s[end+1:])
		} else if strings.HasPrefix(s, "/*") {
			end := strings.Index(s, "*/")
			if end == -1 {
				comments = append(comments, s)
				break
			}
			comments = append(comments, s[:end+2])
			s = strings.TrimSpace(s[end+2:])
		} else {
			break
		}
	}
	return comments
}
