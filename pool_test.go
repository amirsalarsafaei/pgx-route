package pgxrouter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const (
	testTimeout     = 30 * time.Second
	postgresImage   = "postgres:16"
	mainDBName      = "main"
	readDBName      = "read"
	postgresUser    = "postgres"
	postgresPass    = "postgres"
	mainMarkerLabel = "main"
	readMarkerLabel = "read"
)

// PostgresContainer wraps a postgres container with its connection pool.
type PostgresContainer struct {
	Container *postgres.PostgresContainer
	Pool      *pgxpool.Pool
	ConnStr   string
	DBName    string
}

// RouterTestSuite is the main test suite for pgxrouter integration tests.
type RouterTestSuite struct {
	suite.Suite
	ctx    context.Context
	cancel context.CancelFunc
	mainDB *PostgresContainer
	readDB *PostgresContainer
	pool   *Pool
}

func (s *RouterTestSuite) SetupSuite() {
	s.ctx, s.cancel = context.WithTimeout(context.Background(), 5*time.Minute)

	var err error
	s.mainDB, err = startContainer(s.ctx, mainDBName)
	s.Require().NoError(err, "failed to start main container")

	s.readDB, err = startContainer(s.ctx, readDBName)
	s.Require().NoError(err, "failed to start read container")
}

func (s *RouterTestSuite) TearDownSuite() {
	s.cancel()
	cleanupContainer(s.mainDB)
	cleanupContainer(s.readDB)
}

func (s *RouterTestSuite) SetupTest() {
	s.resetMarkerTables()
	s.pool = New(s.mainDB.Pool, s.readDB.Pool)
}

func (s *RouterTestSuite) resetMarkerTables() {
	ctx, cancel := context.WithTimeout(s.ctx, testTimeout)
	defer cancel()

	databases := []struct {
		pool  *pgxpool.Pool
		label string
	}{
		{s.mainDB.Pool, mainMarkerLabel},
		{s.readDB.Pool, readMarkerLabel},
	}

	for _, db := range databases {
		_, _ = db.pool.Exec(ctx, `DROP TABLE IF EXISTS marker`)
		_, err := db.pool.Exec(ctx, `CREATE TABLE marker (label TEXT)`)
		s.Require().NoError(err, "failed to create marker table for %s", db.label)

		_, err = db.pool.Exec(ctx, `INSERT INTO marker (label) VALUES ($1)`, db.label)
		s.Require().NoError(err, "failed to insert marker for %s", db.label)
	}
}

func (s *RouterTestSuite) testContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.ctx, testTimeout)
}

func TestRouterTestSuite(t *testing.T) {
	suite.Run(t, new(RouterTestSuite))
}

func (s *RouterTestSuite) TestQueryRouting() {
	tests := []struct {
		name          string
		query         string
		expectedLabel string
		isExec        bool
		rowsAffected  int64
	}{
		{
			name:          "SELECT routes to read pool",
			query:         `SELECT label FROM marker`,
			expectedLabel: readMarkerLabel,
		},
		{
			name:         "INSERT routes to main pool",
			query:        `INSERT INTO marker (label) VALUES ('insert_test')`,
			isExec:       true,
			rowsAffected: 1,
		},
		{
			name:         "UPDATE routes to main pool",
			query:        `UPDATE marker SET label = 'updated' WHERE label = 'main'`,
			isExec:       true,
			rowsAffected: 1,
		},
		{
			name:         "DELETE routes to main pool",
			query:        `DELETE FROM marker WHERE label = 'main'`,
			isExec:       true,
			rowsAffected: 1,
		},
	}

	for _, tc := range tests {
		s.Run(tc.name, func() {
			s.resetMarkerTables()
			ctx, cancel := s.testContext()
			defer cancel()

			if tc.isExec {
				tag, err := s.pool.Exec(ctx, tc.query)
				s.Require().NoError(err)
				s.Assert().Equal(tc.rowsAffected, tag.RowsAffected())
			} else {
				var label string
				err := s.pool.QueryRow(ctx, tc.query).Scan(&label)
				s.Require().NoError(err)
				s.Assert().Equal(tc.expectedLabel, label)
			}
		})
	}
}

func (s *RouterTestSuite) TestCommentOverrides() {
	tests := []struct {
		name          string
		query         string
		expectedLabel string
		verifyInPool  func() *pgxpool.Pool
		verifyLabel   string
	}{
		{
			name:          "write override routes SELECT to main",
			query:         "-- rw: write\nSELECT label FROM marker",
			expectedLabel: mainMarkerLabel,
		},
		{
			name:         "read override routes INSERT to read pool",
			query:        "-- rw: read\nINSERT INTO marker (label) VALUES ('override_read')",
			verifyInPool: func() *pgxpool.Pool { return s.pool.ReadPool() },
			verifyLabel:  "override_read",
		},
	}

	for _, tc := range tests {
		s.Run(tc.name, func() {
			s.resetMarkerTables()
			ctx, cancel := s.testContext()
			defer cancel()

			if tc.verifyInPool != nil {
				tag, err := s.pool.Exec(ctx, tc.query)
				s.Require().NoError(err)
				s.Assert().Equal(int64(1), tag.RowsAffected())

				var count int
				err = tc.verifyInPool().QueryRow(ctx,
					`SELECT count(*) FROM marker WHERE label = $1`, tc.verifyLabel).Scan(&count)
				s.Require().NoError(err)
				s.Assert().Equal(1, count)
			} else {
				var label string
				err := s.pool.QueryRow(ctx, tc.query).Scan(&label)
				s.Require().NoError(err)
				s.Assert().Equal(tc.expectedLabel, label)
			}
		})
	}
}

func (s *RouterTestSuite) TestQueryReturnsRows() {
	ctx, cancel := s.testContext()
	defer cancel()

	rows, err := s.pool.Query(ctx, `SELECT label FROM marker`)
	s.Require().NoError(err)
	defer rows.Close()

	labels := s.collectLabels(rows)
	s.Require().NoError(rows.Err())
	s.Assert().Equal([]string{readMarkerLabel}, labels)
}

func (s *RouterTestSuite) TestQueryRowNoRows() {
	ctx, cancel := s.testContext()
	defer cancel()

	var label string
	err := s.pool.QueryRow(ctx, `SELECT label FROM marker WHERE label = 'nonexistent'`).Scan(&label)
	s.Assert().True(errors.Is(err, pgx.ErrNoRows))
}

func (s *RouterTestSuite) TestPoolAccessors() {
	s.Assert().Equal(s.mainDB.Pool, s.pool.MainPool())
	s.Assert().Equal(s.readDB.Pool, s.pool.ReadPool())
}

func (s *RouterTestSuite) collectLabels(rows pgx.Rows) []string {
	var labels []string
	for rows.Next() {
		var l string
		s.Require().NoError(rows.Scan(&l))
		labels = append(labels, l)
	}
	return labels
}

// ReadOnlyRetrySuite tests retry behavior when read pool is read-only.
type ReadOnlyRetrySuite struct {
	suite.Suite
	ctx    context.Context
	cancel context.CancelFunc
	mainDB *PostgresContainer
	readDB *PostgresContainer
	pool   *Pool
}

func (s *ReadOnlyRetrySuite) SetupSuite() {
	s.ctx, s.cancel = context.WithTimeout(context.Background(), 5*time.Minute)

	var err error
	s.mainDB, err = startContainer(s.ctx, mainDBName)
	s.Require().NoError(err)

	s.readDB, err = startContainer(s.ctx, readDBName)
	s.Require().NoError(err)
}

func (s *ReadOnlyRetrySuite) TearDownSuite() {
	s.cancel()
	cleanupContainer(s.mainDB)
	cleanupContainer(s.readDB)
}

func (s *ReadOnlyRetrySuite) SetupTest() {
	ctx, cancel := context.WithTimeout(s.ctx, testTimeout)
	defer cancel()

	// Reset read-only mode using admin connection (connects to postgres db)
	resetReadOnly(s.T(), ctx, s.readDB)

	setupMarkerTable(s.T(), ctx, s.mainDB.Pool, mainMarkerLabel)
	setupMarkerTable(s.T(), ctx, s.readDB.Pool, readMarkerLabel)

	// Now make the read pool read-only for the test
	makeReadOnly(s.T(), ctx, s.readDB)

	s.pool = New(s.mainDB.Pool, s.readDB.Pool)
}

func TestReadOnlyRetrySuite(t *testing.T) {
	suite.Run(t, new(ReadOnlyRetrySuite))
}

func (s *ReadOnlyRetrySuite) TestExecRetriesToMain() {
	ctx, cancel := context.WithTimeout(s.ctx, testTimeout)
	defer cancel()

	tag, err := s.pool.Exec(ctx, "-- rw: read\nINSERT INTO marker (label) VALUES ('retry_test')")
	s.Require().NoError(err)
	s.Assert().Equal(int64(1), tag.RowsAffected())

	var count int
	err = s.pool.MainPool().QueryRow(ctx,
		`SELECT count(*) FROM marker WHERE label = 'retry_test'`).Scan(&count)
	s.Require().NoError(err)
	s.Assert().Equal(1, count)
}

func (s *ReadOnlyRetrySuite) TestQueryRowRetriesToMain() {
	ctx, cancel := context.WithTimeout(s.ctx, testTimeout)
	defer cancel()

	var label string
	err := s.pool.QueryRow(ctx,
		"-- rw: read\nINSERT INTO marker (label) VALUES ('qr_retry') RETURNING label").Scan(&label)
	s.Require().NoError(err)
	s.Assert().Equal("qr_retry", label)
}

func (s *ReadOnlyRetrySuite) TestQueryRetriesToMain() {
	ctx, cancel := context.WithTimeout(s.ctx, testTimeout)
	defer cancel()

	rows, err := s.pool.Query(ctx,
		"-- rw: read\nINSERT INTO marker (label) VALUES ('q_retry') RETURNING label")
	s.Require().NoError(err)
	defer rows.Close()

	s.Require().True(rows.Next())
	var label string
	s.Require().NoError(rows.Scan(&label))
	s.Require().NoError(rows.Err())
	s.Assert().Equal("q_retry", label)
}

func (s *ReadOnlyRetrySuite) TestCustomRetryHandler() {
	ctx, cancel := context.WithTimeout(s.ctx, testTimeout)
	defer cancel()

	var capturedErr error
	pool := New(s.mainDB.Pool, s.readDB.Pool, WithRetryOnError(func(err error) bool {
		capturedErr = err
		return true
	}))

	tag, err := pool.Exec(ctx, "-- rw: read\nINSERT INTO marker (label) VALUES ('custom_retry')")
	s.Require().NoError(err)
	s.Assert().Equal(int64(1), tag.RowsAffected())
	s.Assert().NotNil(capturedErr, "custom retry handler should have been called")
}

func TestNewRequiresTwoDistinctPools(t *testing.T) {
	p := &pgxpool.Pool{}
	cases := []struct {
		name       string
		main, read *pgxpool.Pool
	}{
		{"nil read", p, nil},
		{"nil main", nil, p},
		{"both nil", nil, nil},
		{"same pool", p, p},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected New to panic")
				}
			}()
			New(tc.main, tc.read)
		})
	}
}

func startContainer(ctx context.Context, dbName string) (*PostgresContainer, error) {
	ctr, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase(dbName),
		postgres.WithUsername(postgresUser),
		postgres.WithPassword(postgresPass),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, err
	}

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		return nil, err
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		return nil, err
	}

	return &PostgresContainer{
		Container: ctr,
		Pool:      pool,
		ConnStr:   connStr,
		DBName:    dbName,
	}, nil
}

func cleanupContainer(pc *PostgresContainer) {
	if pc == nil {
		return
	}
	if pc.Pool != nil {
		pc.Pool.Close()
	}
	if pc.Container != nil {
		_ = testcontainers.TerminateContainer(pc.Container)
	}
}

func setupMarkerTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label string) {
	t.Helper()

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS marker`)
	_, err := pool.Exec(ctx, `CREATE TABLE marker (label TEXT)`)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `INSERT INTO marker (label) VALUES ($1)`, label)
	require.NoError(t, err)
}

func makeReadOnly(t *testing.T, ctx context.Context, pc *PostgresContainer) {
	t.Helper()

	adminPool, err := newAdminPool(ctx, pc)
	require.NoError(t, err)
	defer adminPool.Close()

	_, err = adminPool.Exec(ctx,
		`ALTER DATABASE `+pc.DBName+` SET default_transaction_read_only = on`)
	require.NoError(t, err)

	pc.Pool.Reset()
}

func resetReadOnly(t *testing.T, ctx context.Context, pc *PostgresContainer) {
	t.Helper()
	adminPool, err := newAdminPool(ctx, pc)
	require.NoError(t, err)
	defer adminPool.Close()

	_, err = adminPool.Exec(ctx,
		`ALTER DATABASE `+pc.DBName+` SET default_transaction_read_only = off`)
	require.NoError(t, err)

	pc.Pool.Reset()
}

func newAdminPool(ctx context.Context, pc *PostgresContainer) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(pc.ConnStr)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Database = "postgres"
	return pgxpool.NewWithConfig(ctx, cfg)
}
