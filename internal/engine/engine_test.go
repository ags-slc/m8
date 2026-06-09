package engine

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ags-slc/m8/internal/schema"
	"github.com/jackc/pgx/v5"
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
		container.Terminate(ctx)
		t.Fatalf("failed to get connection string: %v", err)
	}

	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		container.Terminate(ctx)
		t.Fatalf("failed to connect: %v", err)
	}

	sqlDB, err := sql.Open("pgx", connStr)
	if err != nil {
		conn.Close(ctx)
		container.Terminate(ctx)
		t.Fatalf("failed to open sql.DB: %v", err)
	}

	cleanup := func() {
		sqlDB.Close()
		conn.Close(ctx)
		container.Terminate(ctx)
	}

	return conn, sqlDB, connStr, cleanup
}

func setupMigrationsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"ops", "schema/public", "logic", "permissions"} {
		os.MkdirAll(filepath.Join(dir, sub), 0755)
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
	os.WriteFile(filepath.Join(dir, "ops", "20260411_001__create_ext.sql"),
		[]byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
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
	os.WriteFile(filepath.Join(dir, "ops", "20260411_001__setup.sql"),
		[]byte("CREATE TABLE users (id INT PRIMARY KEY, name TEXT);"), 0644)

	os.WriteFile(filepath.Join(dir, "logic", "hello_func.sql"),
		[]byte("CREATE OR REPLACE FUNCTION hello() RETURNS TEXT LANGUAGE sql AS $$ SELECT 'hello'; $$;"), 0644)

	os.WriteFile(filepath.Join(dir, "permissions", "grants.sql"),
		[]byte("GRANT SELECT ON users TO PUBLIC;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
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
	os.WriteFile(logicFile,
		[]byte("CREATE OR REPLACE FUNCTION hello() RETURNS TEXT LANGUAGE sql AS $$ SELECT 'v1'; $$;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
	}

	ctx := context.Background()
	_, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Verify v1
	var val string
	conn.QueryRow(ctx, "SELECT hello()").Scan(&val)
	if val != "v1" {
		t.Errorf("expected v1, got %q", val)
	}

	// Change the file
	os.WriteFile(logicFile,
		[]byte("CREATE OR REPLACE FUNCTION hello() RETURNS TEXT LANGUAGE sql AS $$ SELECT 'v2'; $$;"), 0644)

	result2, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result2.Logic) != 1 || !result2.Logic[0].Applied {
		t.Error("expected logic to be re-applied on checksum change")
	}

	conn.QueryRow(ctx, "SELECT hello()").Scan(&val)
	if val != "v2" {
		t.Errorf("expected v2, got %q", val)
	}
}

func TestPlan(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	os.WriteFile(filepath.Join(dir, "ops", "20260411_001__ext.sql"),
		[]byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)
	os.WriteFile(filepath.Join(dir, "logic", "func.sql"),
		[]byte("CREATE OR REPLACE FUNCTION f() RETURNS void LANGUAGE sql AS $$ SELECT; $$;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
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

	// Plan should NOT have applied anything
	var count int
	conn.QueryRow(ctx, "SELECT count(*) FROM _m8.history").Scan(&count)
	if count != 0 {
		t.Errorf("plan should not write to history, got %d rows", count)
	}
}

func TestStatus(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	os.WriteFile(filepath.Join(dir, "ops", "20260411_001__ext.sql"),
		[]byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)
	os.WriteFile(filepath.Join(dir, "ops", "20260411_002__pending.sql"),
		[]byte("SELECT 1;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
	}

	ctx := context.Background()
	// Apply first migration only by applying then adding the second
	os.Remove(filepath.Join(dir, "ops", "20260411_002__pending.sql"))
	_, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Re-add the second migration
	os.WriteFile(filepath.Join(dir, "ops", "20260411_002__pending.sql"),
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
	os.WriteFile(filepath.Join(dir, "ops", "20260411_001__ext.sql"),
		[]byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)
	os.WriteFile(filepath.Join(dir, "logic", "func.sql"),
		[]byte("CREATE OR REPLACE FUNCTION f() RETURNS void LANGUAGE sql AS $$ SELECT; $$;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
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
		defer differ.Close()
	}

	ctx := context.Background()

	// Manually acquire the lock from a separate connection
	conn2, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close(ctx)

	var acquired bool
	conn2.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockID).Scan(&acquired)
	if !acquired {
		t.Fatal("failed to acquire lock from second connection")
	}

	// Now try to apply — should fail with lock error
	_, err = eng.Apply(ctx)
	if err == nil {
		t.Error("expected advisory lock error")
	}

	// Release and retry
	conn2.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockID)
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
	os.WriteFile(filepath.Join(dir, "schema", "public", "users.sql"),
		[]byte("CREATE TABLE users (id INT PRIMARY KEY, name TEXT, email TEXT);"), 0644)
	os.WriteFile(filepath.Join(dir, "ops", "20260411_001__ext.sql"),
		[]byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)
	os.WriteFile(filepath.Join(dir, "logic", "hello.sql"),
		[]byte("CREATE OR REPLACE FUNCTION hello() RETURNS TEXT LANGUAGE sql AS $$ SELECT 'hi'; $$;"), 0644)
	os.WriteFile(filepath.Join(dir, "permissions", "grants.sql"),
		[]byte("GRANT SELECT ON users TO PUBLIC;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
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
	conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'users' AND column_name = 'email'
		)
	`).Scan(&colExists)
	if !colExists {
		t.Error("sync should have added the email column")
	}

	// Verify function works
	var val string
	conn.QueryRow(ctx, "SELECT hello()").Scan(&val)
	if val != "hi" {
		t.Errorf("expected 'hi', got %q", val)
	}
}

func TestApplyPhaseOrdering(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)

	// Ops creates the table
	os.WriteFile(filepath.Join(dir, "ops", "20260411_001__create_table.sql"),
		[]byte("CREATE TABLE events (id SERIAL PRIMARY KEY, name TEXT);"), 0644)

	// Logic creates a function that references the table
	os.WriteFile(filepath.Join(dir, "logic", "count_events.sql"),
		[]byte("CREATE OR REPLACE FUNCTION count_events() RETURNS BIGINT LANGUAGE sql AS $$ SELECT count(*) FROM events; $$;"), 0644)

	// Permissions grants on the table
	os.WriteFile(filepath.Join(dir, "permissions", "grants.sql"),
		[]byte("GRANT SELECT ON events TO PUBLIC;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
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
	conn.QueryRow(ctx, "SELECT count_events()").Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 events, got %d", count)
	}
}

func TestFailureRecording(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	os.WriteFile(filepath.Join(dir, "ops", "20260411_001__bad.sql"),
		[]byte("CREATE TABLE nonexistent_schema.fail (id INT);"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
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
	os.WriteFile(opsFile, []byte("CREATE EXTENSION IF NOT EXISTS pg_trgm;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
	}

	ctx := context.Background()
	_, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Modify the file after it was applied (drift)
	os.WriteFile(opsFile, []byte("CREATE EXTENSION IF NOT EXISTS pg_trgm; -- modified"), 0644)

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
	os.WriteFile(filepath.Join(dir, "schema", "public", "items.sql"),
		[]byte("CREATE TABLE items (id SERIAL PRIMARY KEY, name TEXT NOT NULL DEFAULT '');"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer differ.Close()
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
	conn.QueryRow(ctx, fmt.Sprintf(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'items' AND column_name = 'name'
		)
	`)).Scan(&colExists)
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
	defer differ.Close()

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
