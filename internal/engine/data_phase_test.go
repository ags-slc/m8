package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ags-slc/m8/internal/migration"
)

// The migration this phase exists for: a one-time backfill that reads a column
// schema/ adds and CALLs a procedure logic/ creates, all in ONE apply.
//
// In ops/ this is impossible -- ops/ runs before both, so the file finds
// nothing. The two tests below are a matched pair: the same SQL in data/
// succeeds and in ops/ does not, so the ordering is pinned from both sides
// rather than asserted once and hoped for.
const (
	dataPhaseTable = `CREATE TABLE public.parcel (
    id      BIGINT PRIMARY KEY,
    country TEXT
);`
	dataPhaseProc = `CREATE OR REPLACE PROCEDURE public.backfill_country()
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE public.parcel SET country = 'US' WHERE country IS NULL;
END $$;`
	// Seeds a row and backfills it. Depends on schema/ (the table AND the
	// country column) and on logic/ (the procedure) at once.
	dataPhaseBackfill = `INSERT INTO public.parcel (id, country) VALUES (1, NULL);
CALL public.backfill_country();`
)

func TestDataPhaseRunsAfterSchemaAndLogic(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "parcel.sql"), []byte(dataPhaseTable), 0644)
	mustWriteFile(t, filepath.Join(dir, "logic", "backfill_country.sql"), []byte(dataPhaseProc), 0644)
	mustWriteFile(t, filepath.Join(dir, "data", "20260828_001__backfill_country.sql"), []byte(dataPhaseBackfill), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	result, err := eng.Apply(ctx)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Data) != 1 || !result.Data[0].Applied {
		t.Fatalf("expected the data/ migration to be applied, got %+v", result.Data)
	}

	// The backfill actually ran against the object the same apply created.
	var country string
	if err := conn.QueryRow(ctx, "SELECT country FROM public.parcel WHERE id = 1").Scan(&country); err != nil {
		t.Fatalf("reading back the backfilled row: %v", err)
	}
	if country != "US" {
		t.Errorf("country = %q, want \"US\" -- the data/ migration did not take effect", country)
	}
}

// The negative half. Identical SQL in ops/ must FAIL, because the table and the
// procedure do not exist yet. If this ever passes, ops/ has started running
// after schema/ and the whole reason for data/ has gone away -- so a green
// result here is a signal to delete the phase, not to relax the test.
func TestSameMigrationInOpsCannotSeeSchemaOrLogic(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "parcel.sql"), []byte(dataPhaseTable), 0644)
	mustWriteFile(t, filepath.Join(dir, "logic", "backfill_country.sql"), []byte(dataPhaseProc), 0644)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260828_001__backfill_country.sql"), []byte(dataPhaseBackfill), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	_, err := eng.Apply(ctx)
	if err == nil {
		t.Fatal("expected the ops/ migration to fail against objects schema/ and logic/ have not created yet")
	}
	if !strings.Contains(err.Error(), "parcel") {
		t.Errorf("expected the failure to name the missing relation, got: %v", err)
	}
}

// data/ is one-time and checksummed, exactly like ops/. A backfill that re-ran
// on every apply would be at best wasted work and at worst destructive.
func TestDataPhaseRunsOnlyOnce(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "parcel.sql"), []byte(dataPhaseTable), 0644)
	// Appends a row per execution, so a second run is visible as a count.
	mustWriteFile(t, filepath.Join(dir, "data", "20260828_001__seed.sql"),
		[]byte("INSERT INTO public.parcel (id, country) SELECT coalesce(max(id),0)+1, 'US' FROM public.parcel;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	if _, err := eng.Apply(ctx); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second, err := eng.Apply(ctx)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(second.Data) != 1 || !second.Data[0].Skipped {
		t.Fatalf("expected the data/ migration to be skipped on the second apply, got %+v", second.Data)
	}

	var rows int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM public.parcel").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("parcel has %d rows, want 1 -- the data/ migration ran more than once", rows)
	}
}

// Plan must report a pending data/ migration. `m8 plan` exits 2 on pending
// work, and a phase invisible to plan is a phase that applies unannounced.
func TestPlanReportsPendingDataMigration(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "data", "20260828_001__seed.sql"),
		[]byte("SELECT 1;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	plan, err := eng.Plan(ctx)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Data) != 1 || plan.Data[0].Skipped {
		t.Fatalf("expected 1 pending data/ migration in the plan, got %+v", plan.Data)
	}
	if out := FormatPlanOutput(plan); !strings.Contains(out, "20260828_001__seed.sql (data)") {
		t.Errorf("plan output does not mention the data/ migration:\n%s", out)
	}

	if _, err := eng.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}
	plan2, err := eng.Plan(ctx)
	if err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	if len(plan2.Data) != 1 || !plan2.Data[0].Skipped {
		t.Errorf("expected the applied data/ migration to plan as skipped, got %+v", plan2.Data)
	}
}

// Editing an applied data/ file is drift, not a re-apply -- the ops/ contract.
func TestDataPhaseChecksumDriftIsReported(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	dir := setupMigrationsDir(t)
	path := filepath.Join(dir, "data", "20260828_001__seed.sql")
	mustWriteFile(t, path, []byte("SELECT 1;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	mustWriteFile(t, path, []byte("SELECT 2;"), 0644)
	status, err := eng.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var found bool
	for _, d := range status.Drift {
		if d.Migration.Type == migration.TypeData {
			found = true
		}
	}
	if !found {
		t.Errorf("expected drift on the edited data/ file, got drift=%+v pending=%+v", status.Drift, status.Pending)
	}
}

// ops/ and data/ version numbers are separate namespaces. They are timestamps
// from the same clock, so a release that adds one of each on the same day can
// legitimately collide -- and the partial unique index on version would reject
// the second history row if both lived in one namespace.
func TestOpsAndDataShareAVersionWithoutColliding(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260828_001__before.sql"),
		[]byte("CREATE TABLE public.parcel (id BIGINT PRIMARY KEY, country TEXT);"), 0644)
	mustWriteFile(t, filepath.Join(dir, "data", "20260828_001__after.sql"),
		[]byte("INSERT INTO public.parcel (id, country) VALUES (1, 'US');"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}
	result, err := eng.Apply(ctx)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Ops) != 1 || !result.Ops[0].Applied {
		t.Errorf("ops/ migration not applied: %+v", result.Ops)
	}
	if len(result.Data) != 1 || !result.Data[0].Applied {
		t.Errorf("data/ migration not applied: %+v", result.Data)
	}
}

// An install created before data/ existed carries PostgreSQL's auto-generated
// `history_type_check`, which rejects type='data'. The migration would have run
// and then failed to record -- the worst possible order. EnsureSchema widens it.
func TestEnsureSchemaWidensTheLegacyTypeCheck(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	// Recreate the pre-data/ state table exactly as v0.2.0 shipped it.
	legacy := `
CREATE SCHEMA IF NOT EXISTS _m8;
CREATE TABLE IF NOT EXISTS _m8.history (
    id            BIGSERIAL PRIMARY KEY,
    version       TEXT,
    name          TEXT NOT NULL,
    type          TEXT NOT NULL CHECK (type IN ('ops', 'schema', 'logic', 'permissions')),
    pg_schema     TEXT,
    checksum      TEXT NOT NULL,
    applied_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    execution_ms  BIGINT NOT NULL DEFAULT 0,
    applied_by    TEXT NOT NULL DEFAULT current_user,
    success       BOOLEAN NOT NULL DEFAULT true
);`
	if _, err := conn.Exec(ctx, legacy); err != nil {
		t.Fatalf("seeding the legacy state table: %v", err)
	}
	// Prove the starting point really does reject 'data', or this test would
	// pass against a table that never had the problem.
	if _, err := conn.Exec(ctx,
		"INSERT INTO _m8.history (name, type, checksum) VALUES ('probe', 'data', 'x')"); err == nil {
		t.Fatal("the legacy table accepted type='data'; this test is not testing the upgrade")
	}

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "data", "20260828_001__seed.sql"), []byte("SELECT 1;"), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}
	result, err := eng.Apply(ctx)
	if err != nil {
		t.Fatalf("apply against an upgraded install: %v", err)
	}
	if len(result.Data) != 1 || !result.Data[0].Applied {
		t.Fatalf("expected the data/ migration to apply, got %+v", result.Data)
	}

	// And it was RECORDED -- the failure mode is running without recording.
	var n int
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM _m8.history WHERE type = 'data' AND success").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("history has %d successful data rows, want 1", n)
	}
}
