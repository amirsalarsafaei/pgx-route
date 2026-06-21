# pgx-router

![GitHub license](https://img.shields.io/badge/license-MIT-blue.svg)
[![Latest Release](https://img.shields.io/github/v/release/amirsalarsafaei/pgx-router)](https://github.com/amirsalarsafaei/pgx-router/releases/latest)
[![codecov](https://codecov.io/github/amirsalarsafaei/pgx-router/graph/badge.svg)](https://codecov.io/github/amirsalarsafaei/pgx-router?token=7ue7iNJkCH)

`pgx-router` is a Go library that automatically routes PostgreSQL queries to a primary (read-write) or replica (read-only) connection pool based on the type of SQL statement. It wraps [`pgxpool.Pool`](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool) from [pgx](https://github.com/jackc/pgx) and is designed to be a drop-in addition for applications that want to offload read traffic to replicas without changing query code.

## Features

- **Automatic read/write routing** — uses a full PostgreSQL query parser ([pg_query_go](https://github.com/pganalyze/pg_query_go)) to classify every statement. `SELECT` queries go to the replica; `INSERT`, `UPDATE`, `DELETE`, and other mutating statements go to the primary.
- **Comment-based overrides** — prepend a SQL comment to force a specific pool for any individual query (useful for transactions, read-your-writes scenarios, etc.).
- **Locking clause detection** — `SELECT … FOR UPDATE / FOR SHARE` is treated as a write and routed to the primary.
- **Writable CTE detection** — `WITH … INSERT/UPDATE/DELETE … SELECT` is correctly classified as a write.
- **Automatic fallback on read-only errors** — if the replica returns a PostgreSQL `read_only_sql_transaction` error (e.g. after a failover), the query is transparently retried on the primary.
- **Custom retry hook** — supply a `WithRetryOnError` callback to implement your own fallback policy in addition to the built-in detection (e.g. retry on `pgx.ErrNoRows` or connection errors).
- **Optional classification cache** — enable `WithCache` to memoize the read/write classification per SQL string, so the PostgreSQL parser runs at most once per distinct query instead of on every execution.

## Installation

```sh
go get github.com/amirsalarsafaei/pgx-router
```

## Quick Start

```go
import (
    pgxrouter "github.com/amirsalarsafaei/pgx-router"
    "github.com/jackc/pgx/v5/pgxpool"
)

// Create your primary and replica pools as usual.
primaryPool, _ := pgxpool.New(ctx, "postgres://user:pass@primary/db")
replicaPool, _ := pgxpool.New(ctx, "postgres://user:pass@replica/db")

// Wrap them in a router pool.
pool := pgxrouter.New(primaryPool, replicaPool)
defer pool.Close()

// Use pool just like *pgxpool.Pool.
// This SELECT is automatically sent to the replica.
rows, err := pool.Query(ctx, "SELECT id, name FROM users WHERE active = true")

// This INSERT is automatically sent to the primary.
_, err = pool.Exec(ctx, "INSERT INTO events (type) VALUES ($1)", "signup")
```

## API

### `New`

```go
func New(main, read *pgxpool.Pool, opts ...Option) *Pool
```

Creates a new routing pool. Both pools are required and must be distinct — the router is built for primary/replica setups and has no single-pool mode. Passing `nil` for either pool, or the same pool for both, panics.

### `Pool` methods

`Pool` embeds `*pgxpool.Pool`, so all methods of the underlying pool are available. The following methods have routing logic applied:

| Method | Behaviour |
|--------|-----------|
| `Exec(ctx, sql, args...)` | Routes to read or primary based on the SQL statement. |
| `Query(ctx, sql, args...)` | Routes to read or primary. Retries on primary if a read-only error is encountered during row iteration. |
| `QueryRow(ctx, sql, args...)` | Routes to read or primary. Retries on primary when `Scan` returns a read-only error. |
| `Close()` | Closes both pools. |
| `Reset()` | Resets both pools. |

### Accessors

```go
pool.MainPool() *pgxpool.Pool  // returns the primary pool
pool.ReadPool()  *pgxpool.Pool  // returns the replica pool
```

### Options

#### `WithRetryOnError`

```go
func WithRetryOnError(fn func(error) bool) Option
```

Registers a custom function that decides whether a failed read-replica query should be retried on the primary pool. The function receives the error returned by the replica and returns `true` to trigger a retry. This is evaluated _in addition to_ the built-in `read_only_sql_transaction` detection, so either condition can trigger the fallback.

```go
pool := pgxrouter.New(primary, replica,
    pgxrouter.WithRetryOnError(func(err error) bool {
        // Retry on not-found: the replica may lag behind the primary.
        if errors.Is(err, pgx.ErrNoRows) {
            return true
        }
        // Also retry on connection-level errors.
        var netErr *net.OpError
        return errors.As(err, &netErr)
    }),
)
```

#### `WithCache`

```go
func WithCache(c Cache) Option
```

By default, every query is parsed by [pg_query_go](https://github.com/pganalyze/pg_query_go) to classify it as a read or a write. `WithCache` memoizes that classification keyed by the SQL text, so the parser runs at most once per distinct query string:

```go
pool := pgxrouter.New(primary, replica,
    pgxrouter.WithCache(pgxrouter.NewMapCache()),
)
```

`NewMapCache` returns a built-in, concurrency-safe, **unbounded** cache backed by a `sync.Map`. It is a good fit for applications that issue a bounded set of distinct query strings — for example parameterized queries using `$1`, `$2` placeholders. For workloads that interpolate literal values into the SQL text (where the number of distinct strings is unbounded), supply your own implementation of the `Cache` interface — e.g. a bounded LRU:

```go
type Cache interface {
    Get(sql string) (mode classify.QueryMode, ok bool)
    Set(sql string, mode classify.QueryMode)
}
```

Comment overrides (`-- rw: write`) are part of the SQL text, so they are honored by the cache as well. Caching is disabled when no `WithCache` option is supplied.

> If you use [sqlc](https://github.com/sqlc-dev/sqlc), consider [**sqlc-pgx-route**](https://github.com/amirsalarsafaei/sqlc-pgx-route) (see below) instead — it classifies queries at code-generation time and avoids runtime parsing entirely, with no cache required.

## Routing Rules

### Automatic classification

Queries are classified using the PostgreSQL parser. The default rules are:

| Statement type | Pool |
|----------------|------|
| `SELECT` (no locking clause) | Replica |
| `SELECT … FOR UPDATE / FOR SHARE / FOR NO KEY UPDATE / FOR KEY SHARE` | Primary |
| `INSERT`, `UPDATE`, `DELETE` | Primary |
| `WITH … (mutating CTE) … SELECT` | Primary |
| `EXPLAIN …` | Primary |
| Any statement that fails to parse | Primary (safe fallback) |

### Comment overrides

Prepend a line comment or block comment with `rw: read` / `rw: write` (or the long form `rw_mode: read` / `rw_mode: write`) to override routing for a specific query:

```sql
-- rw: write
SELECT * FROM users WHERE id = $1
```

```sql
-- rw: read
INSERT INTO audit_log (event) VALUES ($1) RETURNING id
```

```sql
/* rw_mode: write */
SELECT pg_advisory_lock(1)
```

The keyword match is **case-insensitive**.

## sqlc Integration

If you use [sqlc](https://github.com/sqlc-dev/sqlc) for query code generation, consider [**sqlc-pgx-route**](https://github.com/amirsalarsafaei/sqlc-pgx-route) — a drop-in replacement for `sqlc-gen-go` that determines read/write routing **at code generation time** rather than at runtime.

Instead of parsing SQL on every query execution, `sqlc-pgx-route` uses the PostgreSQL parser once during code generation to classify each query and emits a `PoolRouteQueries` wrapper that calls the correct pool directly. This gives you:

- **Zero per-query routing overhead** — no runtime SQL parsing.
- **Auditable routing** — because the pool assignment is part of the generated code, code reviews and pull request diffs make it immediately visible which pool each query will use, before any code is merged.

See the [sqlc-pgx-route repository](https://github.com/amirsalarsafaei/sqlc-pgx-route) for installation and configuration instructions.

## License

[MIT](LICENSE)
