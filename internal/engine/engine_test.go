package engine

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ags-slc/m8/internal/dump"
	"github.com/ags-slc/m8/internal/migration"
	"github.com/ags-slc/m8/internal/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// testDB spins up a PostgreSQL container and returns a connection + cleanup func.
func testDB(t *testing.T) (*pgx.Conn, *sql.DB, string, func()) {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("m8test"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("testpwd"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second)),
	)
	if err != nil {
		t.Fatalf("failed to start postgres: %v", err)
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("failed to get connection string: %v", err)
	}

	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("failed to connect: %v", err)
	}

	sqlDB, err := sql.Open("pgx", connStr)
	if err != nil {
		_ = conn.Close(ctx)
		_ = container.Terminate(ctx)
		t.Fatalf("failed to open sql.DB: %v", err)
	}

	cleanup := func() {
		_ = sqlDB.Close()
		_ = conn.Close(ctx)
		_ = container.Terminate(ctx)
	}

	return conn, sqlDB, connStr, cleanup
}

func setupMigrationsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"ops", "schema/public", "logic", "permissions"} {
		mustMkdirAll(t, filepath.Join(dir, sub), 0755)
	}
	return dir
}

func newEngine(conn *pgx.Conn, sqlDB *sql.DB, connStr, migrationsDir string, strict bool) (*Engine, *schema.Differ) {
	ctx := context.Background()
	differ, _ := schema.NewDiffer(ctx, connStr, "")
	eng := New(conn, sqlDB, differ, &Config{
		MigrationsDir: migrationsDir,
		ConnStr:       connStr,
		Strict:        strict,
	}, slog.Default())
	return eng, differ
}

func TestApplyOps(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260411_001__create_ext.sql"),
		[]byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	ctx := context.Background()
	result, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ops) != 1 || !result.Ops[0].Applied {
		t.Errorf("expected 1 applied op, got %+v", result.Ops)
	}

	// Apply again — should skip
	result2, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result2.Ops) != 1 || !result2.Ops[0].Skipped {
		t.Errorf("expected op to be skipped on second run")
	}
}

func TestApplyLogicAndPermissions(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)

	// Create a table first via ops
	mustWriteFile(t, filepath.Join(dir, "ops", "20260411_001__setup.sql"),
		[]byte("CREATE TABLE users (id INT PRIMARY KEY, name TEXT);"), 0644)

	mustWriteFile(t, filepath.Join(dir, "logic", "hello_func.sql"),
		[]byte("CREATE OR REPLACE FUNCTION hello() RETURNS TEXT LANGUAGE sql AS $$ SELECT 'hello'; $$;"), 0644)

	mustWriteFile(t, filepath.Join(dir, "permissions", "grants.sql"),
		[]byte("GRANT SELECT ON users TO PUBLIC;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	ctx := context.Background()
	result, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Ops) != 1 || !result.Ops[0].Applied {
		t.Error("ops not applied")
	}
	if len(result.Logic) != 1 || !result.Logic[0].Applied {
		t.Error("logic not applied")
	}
	if len(result.Permissions) != 1 || !result.Permissions[0].Applied {
		t.Error("permissions not applied")
	}

	// Second run: all should skip (checksums unchanged)
	result2, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range result2.Logic {
		if !r.Skipped {
			t.Error("logic should be skipped on second run")
		}
	}
	for _, r := range result2.Permissions {
		if !r.Skipped {
			t.Error("permissions should be skipped on second run")
		}
	}
}

func TestApplyLogicChangedChecksum(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	logicFile := filepath.Join(dir, "logic", "hello.sql")
	mustWriteFile(t, logicFile,
		[]byte("CREATE OR REPLACE FUNCTION hello() RETURNS TEXT LANGUAGE sql AS $$ SELECT 'v1'; $$;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	ctx := context.Background()
	_, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Verify v1
	var val string
	if err := conn.QueryRow(ctx, "SELECT hello()").Scan(&val); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if val != "v1" {
		t.Errorf("expected v1, got %q", val)
	}

	// Change the file
	mustWriteFile(t, logicFile,
		[]byte("CREATE OR REPLACE FUNCTION hello() RETURNS TEXT LANGUAGE sql AS $$ SELECT 'v2'; $$;"), 0644)

	result2, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result2.Logic) != 1 || !result2.Logic[0].Applied {
		t.Error("expected logic to be re-applied on checksum change")
	}

	if err := conn.QueryRow(ctx, "SELECT hello()").Scan(&val); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if val != "v2" {
		t.Errorf("expected v2, got %q", val)
	}
}

func TestPlan(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260411_001__ext.sql"),
		[]byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)
	mustWriteFile(t, filepath.Join(dir, "logic", "func.sql"),
		[]byte("CREATE OR REPLACE FUNCTION f() RETURNS void LANGUAGE sql AS $$ SELECT; $$;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	ctx := context.Background()
	result, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Both should be pending
	pending := 0
	for _, o := range result.Ops {
		if !o.Skipped {
			pending++
		}
	}
	for _, l := range result.Logic {
		if !l.Skipped {
			pending++
		}
	}
	if pending != 2 {
		t.Errorf("expected 2 pending, got %d", pending)
	}

	// Plan should NOT have applied anything — and on a database m8 has never
	// touched, it should not have bootstrapped its own state schema either.
	var stateExists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = '_m8')`,
	).Scan(&stateExists); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if stateExists {
		t.Error("plan should not create the _m8 state schema")
	}
}

func TestStatus(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260411_001__ext.sql"),
		[]byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260411_002__pending.sql"),
		[]byte("SELECT 1;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	ctx := context.Background()
	// Apply first migration only by applying then adding the second
	_ = os.Remove(filepath.Join(dir, "ops", "20260411_002__pending.sql"))
	_, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Re-add the second migration
	mustWriteFile(t, filepath.Join(dir, "ops", "20260411_002__pending.sql"),
		[]byte("SELECT 1;"), 0644)

	status, err := eng.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Applied) != 1 {
		t.Errorf("expected 1 applied, got %d", len(status.Applied))
	}
	if len(status.Pending) != 1 {
		t.Errorf("expected 1 pending, got %d", len(status.Pending))
	}
}

func TestBaseline(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260411_001__ext.sql"),
		[]byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)
	mustWriteFile(t, filepath.Join(dir, "logic", "func.sql"),
		[]byte("CREATE OR REPLACE FUNCTION f() RETURNS void LANGUAGE sql AS $$ SELECT; $$;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	ctx := context.Background()
	err := eng.Baseline(ctx, "", true)
	if err != nil {
		t.Fatal(err)
	}

	// Everything should be marked as applied
	status, err := eng.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Pending) != 0 {
		t.Errorf("expected 0 pending after baseline, got %d", len(status.Pending))
	}
}

func TestAdvisoryLock(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	ctx := context.Background()

	// Manually acquire the lock from a separate connection
	conn2, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn2.Close(ctx) }()

	var acquired bool
	if err := conn2.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockID).Scan(&acquired); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if !acquired {
		t.Fatal("failed to acquire lock from second connection")
	}

	// Now try to apply — should fail with lock error
	_, err = eng.Apply(ctx)
	if err == nil {
		t.Error("expected advisory lock error")
	}

	// Release and retry
	_, _ = conn2.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockID)
	_, err = eng.Apply(ctx)
	if err != nil {
		t.Errorf("expected success after lock release: %v", err)
	}
}

func TestSync(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	// Pre-create a table (simulating existing database)
	_, err := conn.Exec(ctx, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT);")
	if err != nil {
		t.Fatal(err)
	}

	dir := setupMigrationsDir(t)
	// Schema file adds a column
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "users.sql"),
		[]byte("CREATE TABLE users (id INT PRIMARY KEY, name TEXT, email TEXT);"), 0644)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260411_001__ext.sql"),
		[]byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)
	mustWriteFile(t, filepath.Join(dir, "logic", "hello.sql"),
		[]byte("CREATE OR REPLACE FUNCTION hello() RETURNS TEXT LANGUAGE sql AS $$ SELECT 'hi'; $$;"), 0644)
	mustWriteFile(t, filepath.Join(dir, "permissions", "grants.sql"),
		[]byte("GRANT SELECT ON users TO PUBLIC;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	result, err := eng.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Ops should be baselined (skipped)
	if len(result.Ops) != 1 || !result.Ops[0].Skipped {
		t.Error("ops should be baselined during sync")
	}

	// Logic and permissions should be applied
	if len(result.Logic) != 1 || !result.Logic[0].Applied {
		t.Error("logic should be applied during sync")
	}
	if len(result.Permissions) != 1 || !result.Permissions[0].Applied {
		t.Error("permissions should be applied during sync")
	}

	// Verify the email column was added
	var colExists bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'users' AND column_name = 'email'
		)
	`).Scan(&colExists); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if !colExists {
		t.Error("sync should have added the email column")
	}

	// Verify function works
	var val string
	if err := conn.QueryRow(ctx, "SELECT hello()").Scan(&val); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if val != "hi" {
		t.Errorf("expected 'hi', got %q", val)
	}
}

func TestApplyPhaseOrdering(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)

	// Ops creates the table
	mustWriteFile(t, filepath.Join(dir, "ops", "20260411_001__create_table.sql"),
		[]byte("CREATE TABLE events (id SERIAL PRIMARY KEY, name TEXT);"), 0644)

	// Logic creates a function that references the table
	mustWriteFile(t, filepath.Join(dir, "logic", "count_events.sql"),
		[]byte("CREATE OR REPLACE FUNCTION count_events() RETURNS BIGINT LANGUAGE sql AS $$ SELECT count(*) FROM events; $$;"), 0644)

	// Permissions grants on the table
	mustWriteFile(t, filepath.Join(dir, "permissions", "grants.sql"),
		[]byte("GRANT SELECT ON events TO PUBLIC;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	ctx := context.Background()
	result, err := eng.Apply(ctx)
	if err != nil {
		t.Fatalf("apply failed (phase ordering issue?): %v", err)
	}

	// All should have applied successfully
	if !result.Ops[0].Applied {
		t.Error("ops not applied")
	}
	if !result.Logic[0].Applied {
		t.Error("logic not applied")
	}
	if !result.Permissions[0].Applied {
		t.Error("permissions not applied")
	}

	// Verify the function works (depends on table existing)
	var count int64
	if err := conn.QueryRow(ctx, "SELECT count_events()").Scan(&count); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 events, got %d", count)
	}
}

func TestFailureRecording(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260411_001__bad.sql"),
		[]byte("CREATE TABLE nonexistent_schema.fail (id INT);"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	ctx := context.Background()
	_, err := eng.Apply(ctx)
	if err == nil {
		t.Fatal("expected error from bad migration")
	}

	// Failure should be recorded in history
	var success bool
	err = conn.QueryRow(ctx, "SELECT success FROM _m8.history WHERE name = 'bad'").Scan(&success)
	if err != nil {
		t.Fatalf("failed to query history: %v", err)
	}
	if success {
		t.Error("expected success=false in history for failed migration")
	}
}

func TestDriftDetection(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	opsFile := filepath.Join(dir, "ops", "20260411_001__ext.sql")
	mustWriteFile(t, opsFile, []byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	ctx := context.Background()
	_, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Modify the file after it was applied (drift)
	mustWriteFile(t, opsFile, []byte("CREATE EXTENSION IF NOT EXISTS pg_trgm; -- modified"), 0644)

	status, err := eng.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Drift) != 1 {
		t.Errorf("expected 1 drift entry, got %d", len(status.Drift))
	}
}

func TestSchemaApply(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	// Pre-create table
	_, err := conn.Exec(ctx, "CREATE TABLE items (id SERIAL PRIMARY KEY);")
	if err != nil {
		t.Fatal(err)
	}

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "items.sql"),
		[]byte("CREATE TABLE items (id SERIAL PRIMARY KEY, name TEXT NOT NULL DEFAULT '');"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	result, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Schema migration should have applied (added name column)
	schemaApplied := false
	for _, s := range result.Schema {
		if s.Applied {
			schemaApplied = true
			t.Logf("schema diff applied %d statements", len(s.Diff.Statements))
			for _, stmt := range s.Diff.Statements {
				t.Logf("  %s", stmt.DDL)
			}
		}
	}
	if !schemaApplied {
		t.Error("expected schema migration to apply (add name column)")
	}

	// Verify column exists
	var colExists bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'items' AND column_name = 'name'
		)
	`).Scan(&colExists); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if !colExists {
		t.Error("name column should exist after schema apply")
	}

	// Second apply should be a no-op
	result2, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range result2.Schema {
		if !s.Skipped {
			t.Error("expected schema to be skipped on second apply (no diff)")
		}
	}
}

func TestSweepInvalidTempDBs(t *testing.T) {
	conn, _, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	differ, err := schema.NewDiffer(ctx, connStr, "")
	if err != nil {
		t.Fatalf("NewDiffer: %v", err)
	}
	defer func() { _ = differ.Close() }()

	mustExec := func(sqlStr string) {
		if _, err := conn.Exec(ctx, sqlStr); err != nil {
			t.Fatalf("exec %q: %v", sqlStr, err)
		}
	}
	// An orphaned temp DB marked invalid (residue of an interrupted DROP), a
	// healthy temp DB that must survive, and an unrelated DB that must survive.
	mustExec(`CREATE DATABASE pgschemadiff_tmp_invalid TEMPLATE template0`)
	mustExec(`CREATE DATABASE pgschemadiff_tmp_valid TEMPLATE template0`)
	mustExec(`CREATE DATABASE unrelated_db TEMPLATE template0`)
	// Simulate what an interrupted DROP DATABASE leaves behind (datconnlimit = -2).
	mustExec(`UPDATE pg_database SET datconnlimit = -2 WHERE datname = 'pgschemadiff_tmp_invalid'`)

	n, err := differ.SweepInvalidTempDBs(ctx)
	if err != nil {
		t.Fatalf("SweepInvalidTempDBs: %v", err)
	}
	if n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}

	exists := func(name string) bool {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&ok); err != nil {
			t.Fatalf("exists(%s): %v", name, err)
		}
		return ok
	}
	if exists("pgschemadiff_tmp_invalid") {
		t.Error("invalid temp database was not dropped")
	}
	if !exists("pgschemadiff_tmp_valid") {
		t.Error("healthy temp database was incorrectly dropped")
	}
	if !exists("unrelated_db") {
		t.Error("unrelated database was incorrectly dropped")
	}

	// Idempotent: a second sweep finds nothing.
	if n, err = differ.SweepInvalidTempDBs(ctx); err != nil {
		t.Fatalf("second SweepInvalidTempDBs: %v", err)
	} else if n != 0 {
		t.Fatalf("second sweep dropped = %d, want 0", n)
	}
}

func TestSchemaMigrationsRequireDiffer(t *testing.T) {
	conn, sqlDB, _, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	dir := setupMigrationsDir(t)
	if err := os.WriteFile(filepath.Join(dir, "schema", "public", "users.sql"),
		[]byte("CREATE TABLE users (id bigint PRIMARY KEY);"), 0644); err != nil {
		t.Fatal(err)
	}

	// differ == nil simulates an unavailable schema differ (e.g. bad shadow creds).
	eng := New(conn, sqlDB, nil, &Config{MigrationsDir: dir}, slog.Default())

	if _, err := eng.Apply(ctx); err == nil {
		t.Error("Apply: expected error when S__ migrations exist but differ is nil, got nil")
	}
	if _, err := eng.Plan(ctx); err == nil {
		t.Error("Plan: expected error when S__ migrations exist but differ is nil, got nil")
	}
}

func TestSweepStaleTempDBs(t *testing.T) {
	conn, _, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	differ, err := schema.NewDiffer(ctx, connStr, "")
	if err != nil {
		t.Fatalf("NewDiffer: %v", err)
	}
	defer func() { _ = differ.Close() }()

	mustExec := func(c *pgx.Conn, sqlStr string) {
		if _, err := c.Exec(ctx, sqlStr); err != nil {
			t.Fatalf("exec %q: %v", sqlStr, err)
		}
	}
	mustExec(conn, `CREATE DATABASE pgschemadiff_tmp_stale TEMPLATE template0`)
	mustExec(conn, `CREATE DATABASE pgschemadiff_tmp_fresh TEMPLATE template0`)
	mustExec(conn, `CREATE DATABASE pgschemadiff_tmp_nometa TEMPLATE template0`)

	// Seed pg-schema-diff's metadata table inside the temp DBs with a chosen age.
	seedTempDBMetadata(t, connStr, "pgschemadiff_tmp_stale", "now() - interval '2 hours'")
	seedTempDBMetadata(t, connStr, "pgschemadiff_tmp_fresh", "now()")
	// pgschemadiff_tmp_nometa intentionally has no metadata table.

	n, err := differ.SweepStaleTempDBs(ctx, time.Hour)
	if err != nil {
		t.Fatalf("SweepStaleTempDBs: %v", err)
	}
	if n != 1 {
		t.Fatalf("dropped = %d, want 1 (only the stale DB)", n)
	}

	exists := func(name string) bool {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&ok); err != nil {
			t.Fatalf("exists(%s): %v", name, err)
		}
		return ok
	}
	if exists("pgschemadiff_tmp_stale") {
		t.Error("stale temp database was not dropped")
	}
	if !exists("pgschemadiff_tmp_fresh") {
		t.Error("fresh temp database was incorrectly dropped")
	}
	if !exists("pgschemadiff_tmp_nometa") {
		t.Error("temp database without metadata was incorrectly dropped")
	}
}

// tempDBConnStr rewrites a test container connection string to point at another
// database on the same instance.
func tempDBConnStr(connStr, dbName string) string {
	return strings.Replace(connStr, "/m8test?", "/"+dbName+"?", 1)
}

// seedTempDBMetadata writes pg-schema-diff's metadata table into dbName with a
// chosen creation time, so the TTL-based stale sweep has something to vet.
// createdAt is a SQL expression, e.g. "now() - interval '2 hours'".
func seedTempDBMetadata(t *testing.T, connStr, dbName, createdAt string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, tempDBConnStr(connStr, dbName))
	if err != nil {
		t.Fatalf("connect %s: %v", dbName, err)
	}
	defer func() { _ = c.Close(ctx) }()
	for _, stmt := range []string{
		`CREATE SCHEMA pgschemadiff_tmp_metadata`,
		`CREATE TABLE pgschemadiff_tmp_metadata.metadata (db_created_at timestamptz NOT NULL DEFAULT current_timestamp)`,
		`INSERT INTO pgschemadiff_tmp_metadata.metadata (db_created_at) VALUES (` + createdAt + `)`,
	} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			t.Fatalf("seeding metadata in %s (%s): %v", dbName, stmt, err)
		}
	}
}

// dbExists reports whether a database exists on the instance behind conn.
func dbExists(t *testing.T, conn *pgx.Conn, name string) bool {
	t.Helper()
	var ok bool
	if err := conn.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&ok); err != nil {
		t.Fatalf("exists(%s): %v", name, err)
	}
	return ok
}

// countTempDBs returns the number of pgschemadiff_tmp_* databases on the
// instance behind conn.
func countTempDBs(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_database WHERE datname LIKE 'pgschemadiff\_tmp\_%'`).Scan(&n); err != nil {
		t.Fatalf("counting temp databases: %v", err)
	}
	return n
}

// TestShadowInstanceRouting is the cross-instance counterpart to the same-instance
// sweep tests: the shadow is a *different* PostgreSQL server from the target, and
// nothing — temp databases or sweeps — may touch the target.
//
// The target is reached through a role that lacks CREATEDB. That is what makes the
// assertions real rather than vacuous: if a temp database were hosted on the target
// the diff would fail outright, which the final subtest demonstrates by taking the
// shadow away and watching the identical call fail.
func TestShadowInstanceRouting(t *testing.T) {
	targetConn, _, targetSuperConnStr, targetCleanup := testDB(t)
	defer targetCleanup()
	shadowConn, _, shadowConnStr, shadowCleanup := testDB(t)
	defer shadowCleanup()

	ctx := context.Background()

	mustExec := func(c *pgx.Conn, stmt string) {
		t.Helper()
		if _, err := c.Exec(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	mustExec(targetConn, `CREATE ROLE m8_nocreatedb LOGIN PASSWORD 'm8pwd' NOSUPERUSER NOCREATEDB`)
	mustExec(targetConn, `GRANT ALL ON SCHEMA public TO m8_nocreatedb`)

	targetConnStr := strings.Replace(targetSuperConnStr, "postgres:testpwd@", "m8_nocreatedb:m8pwd@", 1)
	if targetConnStr == targetSuperConnStr {
		t.Fatalf("could not derive a restricted connection string from %q", targetSuperConnStr)
	}
	targetDB, err := sql.Open("pgx", targetConnStr)
	if err != nil {
		t.Fatalf("open restricted target: %v", err)
	}
	defer func() { _ = targetDB.Close() }()

	differ, err := schema.NewDiffer(ctx, targetConnStr, shadowConnStr)
	if err != nil {
		t.Fatalf("NewDiffer with separate shadow: %v", err)
	}
	defer func() { _ = differ.Close() }()

	t.Run("diff hosts temp databases on the shadow", func(t *testing.T) {
		res, err := differ.Diff(ctx, targetDB, "public",
			[]string{"CREATE TABLE users (id bigint PRIMARY KEY, email text NOT NULL);"}, false)
		if err != nil {
			t.Fatalf("Diff across a separate shadow instance: %v", err)
		}
		if !res.HasChanges {
			t.Fatal("expected the undeclared table to produce diff statements")
		}
		var mentionsUsers bool
		for _, st := range res.Statements {
			if strings.Contains(strings.ToLower(st.DDL), "users") {
				mentionsUsers = true
			}
		}
		if !mentionsUsers {
			t.Errorf("no statement mentions the users table: %+v", res.Statements)
		}
		if n := countTempDBs(t, targetConn); n != 0 {
			t.Errorf("%d temp database(s) on the TARGET instance; they belong on the shadow", n)
		}
		if n := countTempDBs(t, shadowConn); n != 0 {
			t.Errorf("%d temp database(s) left on the shadow; the factory should have dropped them", n)
		}
	})

	t.Run("sweeps reclaim on the shadow and leave the target alone", func(t *testing.T) {
		// Identically named orphans on both instances: only the shadow's may go.
		for _, c := range []*pgx.Conn{targetConn, shadowConn} {
			mustExec(c, `CREATE DATABASE pgschemadiff_tmp_invalid TEMPLATE template0`)
			mustExec(c, `UPDATE pg_database SET datconnlimit = -2 WHERE datname = 'pgschemadiff_tmp_invalid'`)
			mustExec(c, `CREATE DATABASE pgschemadiff_tmp_stale TEMPLATE template0`)
		}
		seedTempDBMetadata(t, targetSuperConnStr, "pgschemadiff_tmp_stale", "now() - interval '2 hours'")
		seedTempDBMetadata(t, shadowConnStr, "pgschemadiff_tmp_stale", "now() - interval '2 hours'")

		n, err := differ.SweepInvalidTempDBs(ctx)
		if err != nil {
			t.Fatalf("SweepInvalidTempDBs: %v", err)
		}
		if n != 1 {
			t.Errorf("invalid sweep dropped = %d, want 1 (the shadow's only)", n)
		}
		if dbExists(t, shadowConn, "pgschemadiff_tmp_invalid") {
			t.Error("invalid orphan on the shadow was not dropped")
		}
		if !dbExists(t, targetConn, "pgschemadiff_tmp_invalid") {
			t.Error("a database on the TARGET instance was dropped by the invalid sweep")
		}

		n, err = differ.SweepStaleTempDBs(ctx, time.Hour)
		if err != nil {
			t.Fatalf("SweepStaleTempDBs: %v", err)
		}
		if n != 1 {
			t.Errorf("stale sweep dropped = %d, want 1 (the shadow's only)", n)
		}
		if dbExists(t, shadowConn, "pgschemadiff_tmp_stale") {
			t.Error("stale orphan on the shadow was not dropped")
		}
		if !dbExists(t, targetConn, "pgschemadiff_tmp_stale") {
			t.Error("a database on the TARGET instance was dropped by the stale sweep")
		}
	})

	t.Run("without a shadow the same diff is refused by the target", func(t *testing.T) {
		// Negative control: proves the subtests above pass because temp databases
		// went to the shadow, not because the target would have accepted them.
		noShadow, err := schema.NewDiffer(ctx, targetConnStr, "")
		if err == nil {
			defer func() { _ = noShadow.Close() }()
			_, err = noShadow.Diff(ctx, targetDB, "public",
				[]string{"CREATE TABLE users (id bigint PRIMARY KEY);"}, false)
		}
		if err == nil {
			t.Fatal("expected temp database creation on the target to be denied, got no error")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
			t.Fatalf("expected a permission error creating a temp database on the target, got: %v", err)
		}
	})
}

// TestShadowInstanceWithoutPostgresDatabase pins down what a shadow host must
// actually provide. pg-schema-diff's factory defaults its root connection to a
// database named "postgres" and asserts at construction that it landed there;
// NewDiffer overrides that with the database the shadow connection string names.
// Without the override this test fails at NewDiffer, and the documented promise
// that no separate "postgres" database is required would be false.
func TestShadowInstanceWithoutPostgresDatabase(t *testing.T) {
	_, targetDB, targetConnStr, targetCleanup := testDB(t)
	defer targetCleanup()
	shadowConn, _, shadowConnStr, shadowCleanup := testDB(t)
	defer shadowCleanup()

	ctx := context.Background()

	// The shadow connection string names m8test; nothing may depend on "postgres".
	if _, err := shadowConn.Exec(ctx, `DROP DATABASE postgres`); err != nil {
		t.Fatalf("dropping the postgres database on the shadow: %v", err)
	}

	differ, err := schema.NewDiffer(ctx, targetConnStr, shadowConnStr)
	if err != nil {
		t.Fatalf("NewDiffer against a shadow with no postgres database: %v", err)
	}
	defer func() { _ = differ.Close() }()

	if _, err := differ.Diff(ctx, targetDB, "public",
		[]string{"CREATE TABLE users (id bigint PRIMARY KEY);"}, false); err != nil {
		t.Fatalf("Diff against a shadow with no postgres database: %v", err)
	}
	if n := countTempDBs(t, shadowConn); n != 0 {
		t.Errorf("%d temp database(s) left on the shadow after the diff", n)
	}
	if _, err := differ.SweepInvalidTempDBs(ctx); err != nil {
		t.Errorf("SweepInvalidTempDBs against a shadow with no postgres database: %v", err)
	}
	if _, err := differ.SweepStaleTempDBs(ctx, time.Hour); err != nil {
		t.Errorf("SweepStaleTempDBs against a shadow with no postgres database: %v", err)
	}
}

// TestDropCreatedTempDBs covers the residue an interrupted run leaves behind: a
// temp database whose drop never ran is perfectly valid, so neither the invalid
// sweep nor the one-hour TTL sweep reaches it. Reclaiming strictly by the names
// this process created must get it, and must not touch anyone else's.
func TestDropCreatedTempDBs(t *testing.T) {
	_, targetDB, targetConnStr, targetCleanup := testDB(t)
	defer targetCleanup()
	shadowConn, _, shadowConnStr, shadowCleanup := testDB(t)
	defer shadowCleanup()

	ctx := context.Background()

	differ, err := schema.NewDiffer(ctx, targetConnStr, shadowConnStr)
	if err != nil {
		t.Fatalf("NewDiffer: %v", err)
	}
	defer func() { _ = differ.Close() }()

	if _, err := differ.Diff(ctx, targetDB, "public",
		[]string{"CREATE TABLE users (id bigint PRIMARY KEY);"}, false); err != nil {
		t.Fatalf("Diff: %v", err)
	}

	created := differ.CreatedTempDBs()
	if len(created) == 0 {
		t.Fatal("the diff reported creating no temp databases")
	}
	// pg-schema-diff dropped them itself, so there is nothing left to reclaim.
	if n, err := differ.DropCreatedTempDBs(ctx); err != nil {
		t.Fatalf("DropCreatedTempDBs after a clean diff: %v", err)
	} else if n != 0 {
		t.Errorf("reclaimed %d database(s) after a clean diff, want 0", n)
	}

	// Now stage what a cancelled run leaves: one of this run's temp databases
	// still present and valid, alongside another process's, which is off limits.
	leaked := created[0]
	if _, err := shadowConn.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{leaked}.Sanitize()+` TEMPLATE template0`); err != nil {
		t.Fatalf("recreating %s: %v", leaked, err)
	}
	if _, err := shadowConn.Exec(ctx, `CREATE DATABASE pgschemadiff_tmp_someone_else TEMPLATE template0`); err != nil {
		t.Fatalf("creating another process's temp database: %v", err)
	}

	n, err := differ.DropCreatedTempDBs(ctx)
	if err != nil {
		t.Fatalf("DropCreatedTempDBs: %v", err)
	}
	if n != 1 {
		t.Errorf("reclaimed = %d, want 1 (only this run's leftover)", n)
	}
	if dbExists(t, shadowConn, leaked) {
		t.Errorf("%s was not reclaimed", leaked)
	}
	if !dbExists(t, shadowConn, "pgschemadiff_tmp_someone_else") {
		t.Error("another process's temp database was dropped")
	}
}

// mustWriteFile writes a test fixture, failing the test if it cannot.
func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// mustMkdirAll creates a directory tree, failing the test if it cannot.
func mustMkdirAll(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
}

// TestPlanDoesNotCreateSchemas pins that `m8 plan` is read-only. Apply creates
// the PostgreSQL schemas implied by schema/{pg_schema}/ folders; Plan must only
// report them, or a CI "plan" gate silently mutates production.
func TestPlanDoesNotCreateSchemas(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	dir := setupMigrationsDir(t)
	mustMkdirAll(t, filepath.Join(dir, "schema", "warehouse"), 0755)
	mustWriteFile(t, filepath.Join(dir, "schema", "warehouse", "widget.sql"),
		[]byte("CREATE TABLE warehouse.widget (id BIGINT PRIMARY KEY);"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	result, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'warehouse')`,
	).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("plan created schema \"warehouse\" on the target; plan must not write")
	}

	found := false
	for _, s := range result.PendingPGSchemas {
		if s == "warehouse" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected \"warehouse\" in PendingPGSchemas, got %v", result.PendingPGSchemas)
	}

	// And exactly once. A schema/{pg_schema}/ folder implies its schema and the
	// engine owns creating it, so the diff must not also carry a CREATE SCHEMA
	// -- which would double-report it here and, if the engine's ordering ever
	// changed, run a second CREATE against a schema that already exists (42P06).
	for _, s := range result.Schema {
		if s.Diff == nil {
			continue
		}
		for _, stmt := range s.Diff.Statements {
			if strings.Contains(strings.ToLower(stmt.DDL), "create schema") {
				t.Errorf("the diff carries schema creation the engine already owns: %s", stmt.DDL)
			}
		}
	}

	// Apply, by contrast, does create it.
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'warehouse')`,
	).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("apply should have created schema \"warehouse\"")
	}
}

// TestSchemaDiffIsScopedToItsSchema pins that a diff for one schema never
// reaches into another. The desired state only ever describes the schema folder
// being diffed, so an unscoped introspection reads every other schema in the
// database as undeclared — harmless-looking in default mode, which filters
// statements down to declared objects afterwards, but in --strict mode it emits
// DROPs across schemas the migration files never mentioned.
func TestSchemaDiffIsScopedToItsSchema(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	_, err := conn.Exec(ctx, `
		CREATE SCHEMA declared;
		CREATE SCHEMA untouched;
		CREATE TABLE declared.thing (id BIGINT PRIMARY KEY);
		CREATE TABLE untouched.bystander (id BIGINT PRIMARY KEY);
	`)
	if err != nil {
		t.Fatal(err)
	}

	dir := setupMigrationsDir(t)
	mustMkdirAll(t, filepath.Join(dir, "schema", "declared"), 0755)
	// Declares a new column, so the diff is non-empty and we can inspect it.
	mustWriteFile(t, filepath.Join(dir, "schema", "declared", "thing.sql"),
		[]byte("CREATE TABLE declared.thing (id BIGINT PRIMARY KEY, label TEXT);"), 0644)

	// --strict is the sharp case: no post-filter hides out-of-scope statements.
	eng, differ := newEngine(conn, sqlDB, connStr, dir, true)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	result, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}

	sawExpectedChange := false
	for _, s := range result.Schema {
		if s.Error != nil {
			t.Fatalf("diff error: %v", s.Error)
		}
		if s.Diff == nil {
			continue
		}
		for _, stmt := range s.Diff.Statements {
			if strings.Contains(strings.ToLower(stmt.DDL), "untouched") {
				t.Errorf("diff for schema \"declared\" reached into schema \"untouched\": %s", stmt.DDL)
			}
			if strings.Contains(strings.ToLower(stmt.DDL), "label") {
				sawExpectedChange = true
			}
		}
	}
	if !sawExpectedChange {
		t.Error("expected the diff to add the declared \"label\" column")
	}

	// The bystander must still be there.
	var exists bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		               WHERE n.nspname = 'untouched' AND c.relname = 'bystander')
	`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("untouched.bystander disappeared")
	}
}

// TestSchemaApplyCreatesSerialTable pins that a table declared with a SERIAL
// column can actually be created.
//
// pg-schema-diff does manage sequences: it emits its own, correctly qualified,
// CREATE SEQUENCE "public"."probe_id_seq". What broke this was m8's own
// non-strict filter, which kept a statement only when it named a declared
// object -- and that CREATE SEQUENCE contains neither `"probe"` nor a
// word-bounded `probe`, so m8 dropped the library's correct statement and the
// CREATE TABLE then failed on apply with 42P01.
func TestSchemaApplyCreatesSerialTable(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "probe.sql"),
		[]byte("CREATE TABLE public.probe (\n    id BIGSERIAL,\n    note TEXT,\n    CONSTRAINT probe_pkey PRIMARY KEY (id)\n);"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	if _, err := eng.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// The table exists and the sequence actually drives it.
	var id1, id2 int64
	if err := conn.QueryRow(ctx, `INSERT INTO public.probe (note) VALUES ('a') RETURNING id`).Scan(&id1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO public.probe (note) VALUES ('b') RETURNING id`).Scan(&id2); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id2 <= id1 {
		t.Errorf("sequence did not advance: %d then %d", id1, id2)
	}

	// The sequence is owned by the column, so it is dropped with the table —
	// the ownership SERIAL would have established.
	var owned bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_depend d
			JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
			JOIN pg_class t ON t.oid = d.refobjid
			WHERE t.relname = 'probe' AND d.deptype = 'a'
		)
	`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Error("sequence is not OWNED BY the probe.id column")
	}

	// And the baseline is stable: a second plan has nothing to do.
	result, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range result.Schema {
		if s.Error != nil {
			t.Fatalf("second plan errored: %v", s.Error)
		}
		if !s.Skipped {
			for _, stmt := range s.Diff.Statements {
				t.Errorf("second plan is not clean: %s", stmt.DDL)
			}
		}
	}
}

// TestPlanLeavesNoPersistentTraceOnTheTarget pins what `m8 plan` may and may
// not write to a database it has never touched. A CI plan gate runs on every
// pull request; it must be safe to point at production.
//
// "Plan writes nothing at all" would be the wrong claim, and the old version of
// this test only appeared to prove it by using a fixture with no S__ files at
// all, so the differ never ran. With a schema migration present -- and with an
// empty shadow, which is the default -- planning CREATES AND DROPS databases on
// the target instance. That is the churn --shadow-url exists to move elsewhere,
// and it is asserted here rather than left implicit.
//
// What plan must not do is leave anything behind: no _m8 state schema, no
// declared object, and no surviving temp database.
func TestPlanLeavesNoPersistentTraceOnTheTarget(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260101_001__seed.sql"),
		[]byte("SELECT 1;"), 0644)
	mustWriteFile(t, filepath.Join(dir, "logic", "helper.sql"),
		[]byte("CREATE OR REPLACE FUNCTION helper() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$;"), 0644)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "widget.sql"),
		[]byte("CREATE TABLE public.widget (id BIGINT PRIMARY KEY);"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ == nil {
		t.Fatal("differ unavailable; this test is about what the differ does to the target")
	}
	defer func() { _ = differ.Close() }()

	result, err := eng.Plan(ctx)
	if err != nil {
		t.Fatalf("plan on a virgin database: %v", err)
	}

	// Planning a schema migration with no shadow instance configured DOES write
	// to the target: pg-schema-diff creates temp databases there.
	created := differ.CreatedTempDBs()
	if len(created) == 0 {
		t.Error("expected plan to create temp databases on the target with no shadow configured; " +
			"if that is no longer true, the warning in connectAndBuildEngine is stale")
	}

	// None may survive.
	var surviving int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_database WHERE datname LIKE 'pgschemadiff\_tmp\_%'`,
	).Scan(&surviving); err != nil {
		t.Fatal(err)
	}
	if surviving != 0 {
		t.Errorf("plan left %d temp database(s) behind on the target", surviving)
	}

	// Nothing persistent: not m8's own state schema...
	var stateExists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = '_m8')`,
	).Scan(&stateExists); err != nil {
		t.Fatal(err)
	}
	if stateExists {
		t.Error("plan bootstrapped the _m8 state schema; plan must not write")
	}

	// ...not the function it planned...
	var fnExists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'helper')`,
	).Scan(&fnExists); err != nil {
		t.Fatal(err)
	}
	if fnExists {
		t.Error("plan created the helper function")
	}

	// ...and not the table.
	var tableExists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		                WHERE n.nspname = 'public' AND c.relname = 'widget')`,
	).Scan(&tableExists); err != nil {
		t.Fatal(err)
	}
	if tableExists {
		t.Error("plan created the declared table")
	}

	// With no history to read, everything is pending.
	if len(result.Ops) != 1 || result.Ops[0].Skipped {
		t.Errorf("expected the ops migration to be pending, got %+v", result.Ops)
	}
	if len(result.Logic) != 1 || result.Logic[0].Skipped {
		t.Errorf("expected the logic migration to be pending, got %+v", result.Logic)
	}
}

// TestSchemaDiffDegradesWhenPlanCannotBeValidated pins the fallback for a
// cross-schema dependency. pg-schema-diff validates a plan by rebuilding the
// *current* schema in a throwaway database and replaying the generated
// statements against it. Because the diff is scoped to one schema — which is
// what makes it correct and affordable — that rebuild cannot resolve an object
// that reaches outside the schema, e.g. a view over another schema's table.
// m8 must still produce the diff, flagged as unvalidated, rather than refuse.
func TestSchemaDiffDegradesWhenPlanCannotBeValidated(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	_, err := conn.Exec(ctx, `
		CREATE SCHEMA elsewhere;
		CREATE TABLE elsewhere.source_rows (id BIGINT PRIMARY KEY, label TEXT);
		CREATE SCHEMA managed;
		CREATE TABLE managed.thing (id BIGINT PRIMARY KEY);
		-- A view in the managed schema whose definition reaches outside it.
		CREATE VIEW managed.reaching_out AS SELECT id, label FROM elsewhere.source_rows;
	`)
	if err != nil {
		t.Fatal(err)
	}

	dir := setupMigrationsDir(t)
	mustMkdirAll(t, filepath.Join(dir, "schema", "managed"), 0755)
	mustWriteFile(t, filepath.Join(dir, "schema", "managed", "thing.sql"),
		[]byte("CREATE TABLE managed.thing (id BIGINT PRIMARY KEY, note TEXT);"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	result, err := eng.Plan(ctx)
	if err != nil {
		t.Fatalf("plan should degrade, not fail: %v", err)
	}

	var diff *schema.DiffResult
	for _, s := range result.Schema {
		if s.Error != nil {
			t.Fatalf("plan reported an error instead of degrading: %v", s.Error)
		}
		if s.Diff != nil {
			diff = s.Diff
		}
	}
	if diff == nil {
		t.Fatal("no diff produced")
	}
	if !diff.ValidationSkipped {
		t.Error("expected the plan to be flagged as unvalidated")
	}
	if diff.ValidationSkippedReason == "" {
		t.Error("expected a reason for skipping validation")
	}

	// The diff itself must still be correct.
	sawExpected := false
	for _, stmt := range diff.Statements {
		if strings.Contains(strings.ToLower(stmt.DDL), "note") {
			sawExpected = true
		}
		if strings.Contains(strings.ToLower(stmt.DDL), "elsewhere") {
			t.Errorf("diff reached into the other schema: %s", stmt.DDL)
		}
	}
	if !sawExpected {
		t.Error("expected the diff to add the declared \"note\" column")
	}

	// And the warning must reach the operator, not just the struct.
	if out := FormatPlanOutput(result); !strings.Contains(out, "PLAN_NOT_VALIDATED") {
		t.Errorf("plan output does not warn that validation was skipped:\n%s", out)
	}
}

// A plan that produced no statements is marked Skipped, so the pending loop in
// FormatPlanOutput never reaches it. If the warning is only emitted from that
// loop, a clean-but-unvalidated plan prints "Database is up to date." with no
// hint that the check did not run -- which is exactly the reading an operator
// must not be allowed to take away.
func TestFormatPlanOutputWarnsOnCleanUnvalidatedPlan(t *testing.T) {
	result := &ApplyResult{
		Schema: []SchemaResult{{
			Migration: &migration.Migration{Filename: "schema/materialized/x.sql"},
			Skipped:   true, // no statements => Skipped
			Diff: &schema.DiffResult{
				Name:                    "materialized",
				HasChanges:              false,
				ValidationSkipped:       true,
				ValidationSkippedReason: "view materialized.admin_revenue_orphans reaches outside the schema",
			},
		}},
	}

	out := FormatPlanOutput(result)

	if !strings.Contains(out, "PLAN_NOT_VALIDATED") {
		t.Errorf("clean unvalidated plan does not warn:\n%s", out)
	}
	if !strings.Contains(out, "materialized") {
		t.Errorf("warning does not name the schema it applies to:\n%s", out)
	}
	if !strings.Contains(out, "No pending migrations") {
		t.Errorf("warning replaced the up-to-date verdict instead of accompanying it:\n%s", out)
	}
}

// TestSchemaApplyAddsSerialColumnToExistingTable pins the case a CREATE TABLE
// fixup can never reach: the table already exists and the declared SERIAL
// arrives as
//
//	CREATE SEQUENCE "public"."widget_seq_seq"
//	ALTER TABLE "public"."widget" ADD COLUMN "seq" bigint DEFAULT nextval('widget_seq_seq'::regclass) NOT NULL
//	ALTER SEQUENCE "public"."widget_seq_seq" OWNED BY "public"."widget"."seq"
//
// The middle statement names the declared table and survived the non-strict
// filter; the CREATE SEQUENCE names only the derived sequence and did not, so
// apply died with 42P01. m8's old synthesized fixup was anchored on CREATE
// TABLE and never fired here at all.
func TestSchemaApplyAddsSerialColumnToExistingTable(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	if _, err := conn.Exec(ctx, `CREATE TABLE public.widget (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "widget.sql"),
		[]byte("CREATE TABLE public.widget (\n    id BIGINT PRIMARY KEY,\n    seq BIGSERIAL\n);"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	if _, err := eng.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var seq1, seq2 int64
	if err := conn.QueryRow(ctx, `INSERT INTO public.widget (id) VALUES (1) RETURNING seq`).Scan(&seq1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO public.widget (id) VALUES (2) RETURNING seq`).Scan(&seq2); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if seq2 <= seq1 {
		t.Errorf("sequence did not advance: %d then %d", seq1, seq2)
	}

	var owned bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_depend d
			JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
			JOIN pg_class t ON t.oid = d.refobjid
			WHERE t.relname = 'widget' AND d.deptype = 'a'
		)
	`).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if !owned {
		t.Error("sequence is not OWNED BY the widget.seq column")
	}
}

// TestSchemaApplySerialSequenceMatchesColumnType pins that the sequence behind a
// SERIAL (int4) column is an int4 sequence. m8 used to synthesize every fixup
// sequence as "AS bigint" regardless of the column, so a SERIAL column got a
// bigint sequence whose upper range the column cannot hold.
func TestSchemaApplySerialSequenceMatchesColumnType(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "counter.sql"),
		[]byte("CREATE TABLE public.counter (\n    id SERIAL,\n    CONSTRAINT counter_pkey PRIMARY KEY (id)\n);"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	if _, err := eng.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var schemaName, dataType string
	if err := conn.QueryRow(ctx, `
		SELECT schemaname, data_type::text FROM pg_sequences WHERE sequencename = 'counter_id_seq'
	`).Scan(&schemaName, &dataType); err != nil {
		t.Fatalf("counter_id_seq: %v", err)
	}
	if dataType != "integer" {
		t.Errorf("sequence for a SERIAL column is %s, want integer", dataType)
	}
	if schemaName != "public" {
		t.Errorf("sequence landed in schema %q, want public", schemaName)
	}
}

// TestSchemaDiffReclaimsOwnedSequencesOnly pins how the non-strict filter decides
// a sequence is m8's to touch.
//
// A sequence behind a SERIAL column is never named in the DDL that declares it,
// so the declared-object set cannot contain it. Resolving it from pg_depend --
// the column each sequence actually hangs off -- adopts exactly the sequences of
// declared tables. Guessing by name prefix instead would adopt probe_history's
// sequence the moment "probe" is declared, and then drop it.
func TestSchemaDiffReclaimsOwnedSequencesOnly(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE public.probe (id BIGSERIAL PRIMARY KEY, note TEXT);
		CREATE TABLE public.probe_history (hid BIGSERIAL PRIMARY KEY);
	`); err != nil {
		t.Fatal(err)
	}

	dir := setupMigrationsDir(t)
	// probe is declared, and no longer SERIAL. probe_history is not declared at
	// all -- its name merely starts with "probe_".
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "probe.sql"),
		[]byte("CREATE TABLE public.probe (\n    id BIGINT NOT NULL,\n    note TEXT,\n    CONSTRAINT probe_pkey PRIMARY KEY (id)\n);"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	result, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}

	sawDrop := false
	for _, s := range result.Schema {
		if s.Error != nil {
			t.Fatalf("diff error: %v", s.Error)
		}
		if s.Diff == nil {
			continue
		}
		for _, stmt := range s.Diff.Statements {
			lower := strings.ToLower(stmt.DDL)
			if strings.Contains(lower, "probe_history") {
				t.Errorf("plan reached an undeclared table's sequence: %s", stmt.DDL)
			}
			if strings.Contains(lower, "drop sequence") && strings.Contains(lower, "probe_id_seq") {
				sawDrop = true
			}
		}
	}
	if !sawDrop {
		t.Error("plan left probe_id_seq behind: a declared table that stops being SERIAL must reclaim its own sequence")
	}

	if _, err := eng.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var probeSeq, historySeq bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_sequences WHERE sequencename = 'probe_id_seq'),
		        EXISTS (SELECT 1 FROM pg_sequences WHERE sequencename = 'probe_history_hid_seq')`,
	).Scan(&probeSeq, &historySeq); err != nil {
		t.Fatal(err)
	}
	if probeSeq {
		t.Error("probe_id_seq survived: the declared table no longer uses it")
	}
	if !historySeq {
		t.Error("probe_history_hid_seq was dropped: probe_history is not declared and must not be touched")
	}
}

// TestFormatApplyOutputWarnsOnUnvalidatedPlan pins that the warning reaches the
// operator who is actually writing to the database.
//
// The warning used to exist only in FormatPlanOutput, so `m8 apply` ran a
// degraded, unvalidated plan against the target and printed nothing but
// "✓ file (Nms, K statements)".
func TestFormatApplyOutputWarnsOnUnvalidatedPlan(t *testing.T) {
	result := &ApplyResult{
		Schema: []SchemaResult{{
			Migration: &migration.Migration{Filename: "schema/materialized/x.sql"},
			Applied:   true,
			ExecMs:    12,
			Diff: &schema.DiffResult{
				Name:                    "materialized",
				HasChanges:              true,
				Statements:              []schema.DiffStatement{{DDL: "ALTER TABLE x ADD COLUMN y text"}},
				ValidationSkipped:       true,
				ValidationSkippedReason: "view materialized.admin_revenue_orphans reaches outside the schema",
			},
		}},
	}

	out := FormatApplyOutput(result)

	if !strings.Contains(out, "PLAN_NOT_VALIDATED") {
		t.Errorf("apply output does not warn that the plan was never validated:\n%s", out)
	}
	if !strings.Contains(out, "materialized") {
		t.Errorf("warning does not name the schema it applies to:\n%s", out)
	}
	if !strings.Contains(out, "Applied: 1") {
		t.Errorf("warning replaced the summary instead of accompanying it:\n%s", out)
	}
}

// A clean-but-unvalidated apply is Skipped and would otherwise carry no line at
// all, exactly as in FormatPlanOutput.
func TestFormatApplyOutputWarnsOnCleanUnvalidatedPlan(t *testing.T) {
	result := &ApplyResult{
		Schema: []SchemaResult{{
			Migration: &migration.Migration{Filename: "schema/materialized/x.sql"},
			Skipped:   true,
			Diff: &schema.DiffResult{
				Name:                    "materialized",
				ValidationSkipped:       true,
				ValidationSkippedReason: "view materialized.admin_revenue_orphans reaches outside the schema",
			},
		}},
	}

	if out := FormatApplyOutput(result); !strings.Contains(out, "PLAN_NOT_VALIDATED") {
		t.Errorf("clean unvalidated apply does not warn:\n%s", out)
	}
}

// TestApplyRefusesUnvalidatedPlanWhenConfigured pins that a printed warning is
// not the only defence available. A repository whose target is a production
// primary sets --fail-on-unvalidated (or require_shadow, which implies it) and
// m8 then refuses the plan instead of running it.
func TestApplyRefusesUnvalidatedPlanWhenConfigured(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	if _, err := conn.Exec(ctx, `
		CREATE SCHEMA elsewhere;
		CREATE TABLE elsewhere.source_rows (id BIGINT PRIMARY KEY, label TEXT);
		CREATE SCHEMA managed;
		CREATE TABLE managed.thing (id BIGINT PRIMARY KEY);
		CREATE VIEW managed.reaching_out AS SELECT id, label FROM elsewhere.source_rows;
	`); err != nil {
		t.Fatal(err)
	}

	dir := setupMigrationsDir(t)
	mustMkdirAll(t, filepath.Join(dir, "schema", "managed"), 0755)
	mustWriteFile(t, filepath.Join(dir, "schema", "managed", "thing.sql"),
		[]byte("CREATE TABLE managed.thing (id BIGINT PRIMARY KEY, note TEXT);"), 0644)

	differ, err := schema.NewDiffer(ctx, connStr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = differ.Close() }()

	eng := New(conn, sqlDB, differ, &Config{
		MigrationsDir:     dir,
		ConnStr:           connStr,
		FailOnUnvalidated: true,
	}, slog.Default())

	result, err := eng.Apply(ctx)
	if err == nil {
		t.Fatal("apply ran an unvalidated plan even though it was configured to refuse")
	}
	if !strings.Contains(err.Error(), "not validated") {
		t.Errorf("refusal does not say why it refused: %v", err)
	}
	if !strings.Contains(err.Error(), "shadow") {
		t.Errorf("refusal does not say how to fix it: %v", err)
	}

	// Nothing was applied.
	var hasNote bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'managed' AND table_name = 'thing' AND column_name = 'note'
		)`).Scan(&hasNote); err != nil {
		t.Fatal(err)
	}
	if hasNote {
		t.Error("the refused plan was applied anyway")
	}

	// And the refusal is legible in the rendered output, not just in the error.
	if result == nil {
		t.Fatal("no result to format")
	}
	if out := FormatApplyOutput(result); !strings.Contains(out, "not validated") {
		t.Errorf("apply output does not carry the refusal:\n%s", out)
	}

	// Without the setting, the same plan applies -- the refusal is opt-in.
	permissive := New(conn, sqlDB, differ, &Config{MigrationsDir: dir, ConnStr: connStr}, slog.Default())
	if _, err := permissive.Apply(ctx); err != nil {
		t.Fatalf("apply without --fail-on-unvalidated: %v", err)
	}
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'managed' AND table_name = 'thing' AND column_name = 'note'
		)`).Scan(&hasNote); err != nil {
		t.Fatal(err)
	}
	if !hasNote {
		t.Error("the permissive apply did not apply the plan either")
	}
}

// TestSchemaStatementLockTimeoutIsActuallyApplied pins that pg-schema-diff's
// hazard-derived timeouts reach the server.
//
// They were issued as SET LOCAL on e.conn, which is not inside a transaction --
// and SET LOCAL outside a transaction block does nothing at all. Postgres
// raises a WARNING and moves on; the errors were discarded with `_, _ =`, so
// nothing surfaced. Schema DDL therefore waited for its ACCESS EXCLUSIVE lock
// without bound.
//
// With the fix the ALTER aborts with 55P03 (lock_not_available). Without it, it
// blocks until the context deadline below, which is what the test distinguishes.
func TestSchemaStatementLockTimeoutIsActuallyApplied(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	if _, err := conn.Exec(ctx, `CREATE TABLE public.blocked (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	// A second session sitting on the lock the ALTER needs.
	blocker, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close(ctx) }()
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE public.blocked IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	eng := New(conn, sqlDB, nil, &Config{ConnStr: connStr}, slog.Default())

	// Bounded so an unbounded wait is a failure rather than a hang.
	stmtCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	start := time.Now()
	err = eng.execSchemaStatement(stmtCtx, schema.DiffStatement{
		DDL:         `ALTER TABLE public.blocked ADD COLUMN note TEXT`,
		LockTimeout: 2 * time.Second,
		Timeout:     10 * time.Second,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the ALTER succeeded while another session held ACCESS EXCLUSIVE")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("statement did not abort on lock_timeout after %s: %v", elapsed, err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("lock_timeout of 2s took %s to fire", elapsed)
	}

	// Release the lock; the connection must be usable and clean afterwards.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	for setting, want := range map[string]string{"lock_timeout": "0", "statement_timeout": "0"} {
		var got string
		if err := conn.QueryRow(ctx, "SHOW "+setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s leaked onto the connection after the statement: %q, want %q", setting, got, want)
		}
	}
}

// TestSchemaStatementTimeoutOverride pins the escape hatch. pg-schema-diff's
// derived statement_timeout is 3s for ordinary DDL, which a legitimate ALTER on
// a large table can exceed -- and did, unnoticed, for as long as the timeouts
// were no-ops.
func TestSchemaStatementTimeoutOverride(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	eng := New(conn, sqlDB, nil, &Config{
		ConnStr:          connStr,
		StatementTimeout: 30 * time.Second,
	}, slog.Default())

	// The plan says 100ms; the override says 30s. pg_sleep(1) settles it.
	err := eng.execSchemaStatement(ctx, schema.DiffStatement{
		DDL:     `SELECT pg_sleep(1)`,
		Timeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("override did not replace the derived statement_timeout: %v", err)
	}

	// Without the override the derived value is what applies.
	strict := New(conn, sqlDB, nil, &Config{ConnStr: connStr}, slog.Default())
	err = strict.execSchemaStatement(ctx, schema.DiffStatement{
		DDL:     `SELECT pg_sleep(1)`,
		Timeout: 100 * time.Millisecond,
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("derived statement_timeout was not applied: %v", err)
	}
}

// TestDumpedSchemaPlansClean pins the seam between the two halves of m8: what
// `m8 dump` writes must be what `m8 plan` reads as already-applied. The README
// tells operators to bootstrap by dumping and then checking that the plan is
// empty, so a mismatch here means m8 proposes rewriting a database to the state
// it is already in.
//
// It is a real regression risk for anything that changes how DDL is rendered.
// The dumper now quotes every identifier, and the differ's non-strict filter
// decides what to keep by looking for declared object names in the generated
// statements -- one is written against the other, and only an end-to-end check
// notices when they drift apart.
func TestDumpedSchemaPlansClean(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE public.parent (id BIGINT PRIMARY KEY, name TEXT);
		CREATE TABLE public.child (
			id        BIGSERIAL PRIMARY KEY,
			parent_id BIGINT NOT NULL,
			label     TEXT NOT NULL DEFAULT 'x',
			CONSTRAINT child_parent_fk FOREIGN KEY (parent_id)
				REFERENCES public.parent (id) ON DELETE CASCADE,
			CONSTRAINT child_label_ck   CHECK (label <> ''),
			CONSTRAINT child_label_uniq UNIQUE (label)
		);
		CREATE INDEX child_parent_idx ON public.child (parent_id) WHERE parent_id > 0;
	`); err != nil {
		t.Fatal(err)
	}

	d := dump.NewDumper(conn)
	dir := setupMigrationsDir(t)
	for _, name := range []string{"parent", "child"} {
		tbl, err := d.DumpTable(ctx, "public", name)
		if err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(dir, "schema", "public", name+".sql"),
			[]byte(dump.RenderDDL(tbl)), 0644)
	}

	// --strict is the sharper half: with no post-filter, anything the dump
	// failed to describe comes back as a DROP.
	for _, strict := range []bool{false, true} {
		eng, differ := newEngine(conn, sqlDB, connStr, dir, strict)
		result, err := eng.Plan(ctx)
		if err != nil {
			t.Fatalf("strict=%v: plan: %v", strict, err)
		}
		for _, s := range result.Schema {
			if s.Error != nil {
				t.Fatalf("strict=%v: diff error: %v", strict, s.Error)
			}
			if s.Diff == nil {
				continue
			}
			for _, stmt := range s.Diff.Statements {
				t.Errorf("strict=%v: dumped schema does not plan clean: %s", strict, stmt.DDL)
			}
		}
		if differ != nil {
			_ = differ.Close()
		}
	}
}

// TestNonStrictPlanLeavesUndeclaredAttachedObjects pins that a dumped baseline
// does not propose destroying the things `m8 dump` cannot capture.
//
// pg-schema-diff introspects triggers, row-level security policies, the RLS flag
// and replica identity. m8's dumper captures none of them, so on a database
// bootstrapped by `m8 dump` the desired state never mentions them and the diff
// proposes removing them -- and the declared-object filter keeps those
// statements, because each one NAMES the declared table it hangs off. Applied
// against a production primary that means audit triggers gone, RLS disabled,
// the tenant-isolation policy dropped, and REPLICA IDENTITY FULL reset out from
// under a logical-replication consumer. The plan validates cleanly, so
// --fail-on-unvalidated catches none of it.
func TestNonStrictPlanLeavesUndeclaredAttachedObjects(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE public.t (id BIGINT PRIMARY KEY, tenant TEXT NOT NULL);
		CREATE FUNCTION public.t_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
		CREATE TRIGGER t_audit_trg BEFORE INSERT ON public.t FOR EACH ROW EXECUTE FUNCTION public.t_audit();
		ALTER TABLE public.t ENABLE ROW LEVEL SECURITY;
		CREATE POLICY t_tenant_isolation ON public.t USING (true);
		ALTER TABLE public.t REPLICA IDENTITY FULL;
	`); err != nil {
		t.Fatal(err)
	}

	// The baseline a `m8 dump` would produce: the table, and nothing attached.
	dir := setupMigrationsDir(t)
	tbl, err := dump.NewDumper(conn).DumpTable(ctx, "public", "t")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "t.sql"),
		[]byte(dump.RenderDDL(tbl)), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	result, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range result.Schema {
		if s.Error != nil {
			t.Fatalf("diff error: %v", s.Error)
		}
		if s.Diff == nil {
			continue
		}
		for _, stmt := range s.Diff.Statements {
			t.Errorf("plan proposes acting on an object the migration files never declared: %s", stmt.DDL)
		}
	}

	// And apply must actually leave them in place.
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var trigger, policy, rls bool
	var replicaIdentity string
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 't_audit_trg' AND NOT tgisinternal),
		       EXISTS (SELECT 1 FROM pg_policy WHERE polname = 't_tenant_isolation'),
		       c.relrowsecurity,
		       c.relreplident::text
		FROM pg_class c WHERE c.oid = 'public.t'::regclass
	`).Scan(&trigger, &policy, &rls, &replicaIdentity); err != nil {
		t.Fatal(err)
	}
	if !trigger {
		t.Error("apply dropped an undeclared trigger")
	}
	if !policy {
		t.Error("apply dropped an undeclared row-level security policy")
	}
	if !rls {
		t.Error("apply disabled row-level security")
	}
	if replicaIdentity != "f" {
		t.Errorf("apply reset REPLICA IDENTITY to %q, breaking logical replication", replicaIdentity)
	}
}

// The protection is per class, not blanket: a schema folder that DOES declare
// triggers keeps the old behaviour, because then a trigger the files omit really
// is one the desired state says should not exist.
func TestNonStrictPlanStillManagesDeclaredAttachedClasses(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE public.t (id BIGINT PRIMARY KEY);
		CREATE FUNCTION public.t_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
		CREATE TRIGGER t_old_trg BEFORE INSERT ON public.t FOR EACH ROW EXECUTE FUNCTION public.t_audit();
	`); err != nil {
		t.Fatal(err)
	}

	// The folder declares the table AND a different trigger, so triggers are a
	// class this schema manages.
	dir := setupMigrationsDir(t)
	// The trigger function has to be declared too: the desired state is parsed
	// by replaying this DDL into an empty database.
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "t.sql"), []byte(`
CREATE TABLE public.t (id BIGINT PRIMARY KEY);
CREATE FUNCTION public.t_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER t_new_trg BEFORE UPDATE ON public.t FOR EACH ROW EXECUTE FUNCTION public.t_audit();
`), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	result, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sawDrop, sawCreate bool
	for _, s := range result.Schema {
		if s.Error != nil {
			t.Fatalf("diff error: %v", s.Error)
		}
		if s.Diff == nil {
			continue
		}
		for _, stmt := range s.Diff.Statements {
			lower := strings.ToLower(stmt.DDL)
			if strings.Contains(lower, "drop trigger") && strings.Contains(lower, "t_old_trg") {
				sawDrop = true
			}
			if strings.Contains(lower, "create trigger") && strings.Contains(lower, "t_new_trg") {
				sawCreate = true
			}
		}
	}
	if !sawCreate {
		t.Error("a declared trigger was not created")
	}
	if !sawDrop {
		t.Error("a folder that declares triggers must still reconcile the ones it omits")
	}
}

// TestApplyRefusesBeforeAnyFolderIsApplied pins that a refusal is a refusal for
// the whole run.
//
// The check used to sit inside the loop that applies each schema/ folder, so a
// folder refused late was refused AFTER an earlier folder's statements had
// already landed -- a partial apply with an error on the end, which is the
// outcome --fail-on-unvalidated exists to prevent. Here "alpha" is clean and
// sorts first; "omega" cannot be validated.
func TestApplyRefusesBeforeAnyFolderIsApplied(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()

	if _, err := conn.Exec(ctx, `
		CREATE SCHEMA elsewhere;
		CREATE TABLE elsewhere.source_rows (id BIGINT PRIMARY KEY, label TEXT);
		CREATE SCHEMA alpha;
		CREATE TABLE alpha.thing (id BIGINT PRIMARY KEY);
		CREATE SCHEMA omega;
		CREATE TABLE omega.thing (id BIGINT PRIMARY KEY);
		-- Reaches outside its own schema, so the plan for omega cannot be
		-- validated against a single-schema rebuild.
		CREATE VIEW omega.reaching_out AS SELECT id, label FROM elsewhere.source_rows;
	`); err != nil {
		t.Fatal(err)
	}

	dir := setupMigrationsDir(t)
	for _, pgSchema := range []string{"alpha", "omega"} {
		mustMkdirAll(t, filepath.Join(dir, "schema", pgSchema), 0755)
		mustWriteFile(t, filepath.Join(dir, "schema", pgSchema, "thing.sql"),
			[]byte("CREATE TABLE "+pgSchema+".thing (id BIGINT PRIMARY KEY, note TEXT);"), 0644)
	}

	differ, err := schema.NewDiffer(ctx, connStr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = differ.Close() }()

	eng := New(conn, sqlDB, differ, &Config{
		MigrationsDir:     dir,
		ConnStr:           connStr,
		FailOnUnvalidated: true,
	}, slog.Default())

	if _, err := eng.Apply(ctx); err == nil {
		t.Fatal("apply ran an unvalidated plan even though it was configured to refuse")
	}

	for _, pgSchema := range []string{"alpha", "omega"} {
		var hasNote bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'thing' AND column_name = 'note'
			)`, pgSchema).Scan(&hasNote); err != nil {
			t.Fatal(err)
		}
		if hasNote {
			t.Errorf("schema %q was applied before the run was refused", pgSchema)
		}
	}
}

// An unvalidated diff that proposes NOTHING must not be refused. Validation
// replays the generated statements against a rebuilt schema; with no
// statements there is nothing to replay, so refusing protects nothing -- and
// because m8 scopes each diff to one schema, the rebuild fails on any object
// defined in terms of another schema. A database with one cross-schema view
// would otherwise refuse every clean plan forever, and a gate that is always
// red is a gate somebody turns off.
func TestUnvalidatedSchemasIgnoresAnEmptyDiff(t *testing.T) {
	result := &ApplyResult{
		Schema: []SchemaResult{{
			Migration: &migration.Migration{Filename: "schema/materialized/x.sql"},
			Skipped:   true,
			Diff: &schema.DiffResult{
				Name:                    "materialized",
				HasChanges:              false,
				ValidationSkipped:       true,
				ValidationSkippedReason: "view materialized.admin_revenue_orphans reaches outside the schema",
			},
		}},
	}

	if names := UnvalidatedSchemas(result); len(names) != 0 {
		t.Errorf("empty unvalidated diff was reported as refusable: %v", names)
	}

	// The warning must still be printed -- ignoring it and hiding it are
	// different things.
	if out := FormatPlanOutput(result); !strings.Contains(out, "PLAN_NOT_VALIDATED") {
		t.Errorf("the warning was suppressed along with the refusal:\n%s", out)
	}
}

// The other half: an unvalidated diff that DOES propose DDL is exactly the
// case --fail-on-unvalidated exists for, and must still be refused.
func TestUnvalidatedSchemasReportsADiffWithStatements(t *testing.T) {
	result := &ApplyResult{
		Schema: []SchemaResult{{
			Migration: &migration.Migration{Filename: "schema/materialized/x.sql"},
			Diff: &schema.DiffResult{
				Name:                    "materialized",
				HasChanges:              true,
				Statements:              []schema.DiffStatement{{DDL: "ALTER TABLE materialized.x ADD COLUMN y int"}},
				ValidationSkipped:       true,
				ValidationSkippedReason: "view materialized.admin_revenue_orphans reaches outside the schema",
			},
		}},
	}

	names := UnvalidatedSchemas(result)
	if len(names) != 1 || names[0] != "materialized" {
		t.Errorf("unvalidated diff with statements was not reported: %v", names)
	}
}
