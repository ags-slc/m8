// Package pgtest gives tests a PostgreSQL database without a container each.
//
// Starting a container takes seconds; creating a database on a running one
// takes milliseconds. So a test binary that calls Main starts one container,
// the first time a test asks for it, and DB hands every test its own fresh
// database on it, dropped when the test ends.
//
// A database isolates almost everything a test does, but not what PostgreSQL
// keeps per server. Roles are the common case, so DB's cleanup drops every
// role a test created. A test that reads or changes other databases -- the
// pgschemadiff_tmp_* sweeps, counting temp databases, dropping postgres --
// cannot share a server at all; Server gives it a container of its own.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver tests open
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Database is the database a container starts with. Server's connection
// string names it; DB's name their own.
const Database = "m8test"

var (
	shared struct {
		once      sync.Once
		container *postgres.PostgresContainer
		connStr   string
		err       error
	}
	seq atomic.Int64
)

// Main runs the package's tests, then stops the shared container if any test
// started it. Call it from TestMain:
//
//	func TestMain(m *testing.M) { pgtest.Main(m) }
func Main(m *testing.M) {
	code := m.Run()
	if shared.container != nil {
		_ = shared.container.Terminate(context.Background())
	}
	os.Exit(code)
}

// DB creates a fresh database on the shared server and returns a superuser
// connection string for it. When the test ends the database is dropped, with
// any connection the test left open, and so is every role the test created.
func DB(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	shared.once.Do(func() {
		shared.container, shared.connStr, shared.err = start(ctx)
	})
	if shared.err != nil {
		t.Fatalf("starting the shared postgres: %v", shared.err)
	}

	admin, err := pgx.Connect(ctx, shared.connStr)
	if err != nil {
		t.Fatalf("connecting to the shared postgres: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	name := fmt.Sprintf("t%d", seq.Add(1))
	roles, err := userRoles(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating database %s: %v", name, err)
	}
	t.Cleanup(func() { drop(t, name, roles) })
	return WithDatabase(shared.connStr, name)
}

// drop removes a test's database, then the roles it created. A role can only
// be dropped once nothing depends on it, and the database held everything
// that did.
func drop(t *testing.T, name string, before map[string]bool) {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, shared.connStr)
	if err != nil {
		t.Errorf("connecting to drop %s: %v", name, err)
		return
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
		t.Errorf("dropping database %s: %v", name, err)
		return
	}
	after, err := userRoles(ctx, admin)
	if err != nil {
		t.Error(err)
		return
	}
	for role := range after {
		if before[role] {
			continue
		}
		if _, err := admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Errorf("dropping role %s that the test created: %v", role, err)
		}
	}
}

// userRoles lists the roles a test may have created: every role but the
// built-in pg_* ones and the bootstrap superuser.
func userRoles(ctx context.Context, conn *pgx.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, `SELECT rolname::text FROM pg_roles WHERE rolname !~ '^pg_' AND oid <> 10`)
	if err != nil {
		return nil, fmt.Errorf("listing roles: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("listing roles: %w", err)
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set, nil
}

// Server starts a container of the test's own and returns a superuser
// connection string for its Database. It is for the tests DB cannot isolate;
// the container stops when the test ends.
func Server(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	container, connStr, err := start(ctx)
	if err != nil {
		t.Fatalf("starting postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })
	return connStr
}

// WithDatabase points a connection string at another database on the same
// server.
func WithDatabase(connStr, name string) string {
	u, err := url.Parse(connStr)
	if err != nil {
		panic(fmt.Sprintf("pgtest: connection string %q: %v", connStr, err))
	}
	u.Path = "/" + name
	return u.String()
}

func start(ctx context.Context) (*postgres.PostgresContainer, string, error) {
	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase(Database),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("testpwd"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		return nil, "", err
	}
	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, "", err
	}
	return container, connStr, nil
}
