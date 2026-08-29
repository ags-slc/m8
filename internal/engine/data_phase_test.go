package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ags-slc/m8/internal/migration"
	"github.com/ags-slc/m8/internal/state"
	"github.com/jackc/pgx/v5"
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

// The CHECK upgrade is a probe followed by a mutation, so two m8 processes can
// both pass the guard: Status calls EnsureSchema without the advisory lock
// Apply takes, and the upgrade window is exactly when a team runs both against
// the same database. The loser blocks on the winner's ALTER, then finds the
// constraint already there. Without the exception handler in schema.sql that is
// a 42710 that aborts the whole bootstrap -- an upgrade to this release failing
// a deploy because a dashboard ran `m8 status` at the wrong moment.
//
// The race is driven deterministically rather than raced for: the winner holds
// its transaction open until the loser is provably parked on the lock.
func TestEnsureSchemaSurvivesAConcurrentTypeCheckUpgrade(t *testing.T) {
	conn, _, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

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

	// The winner: the same upgrade schema.sql performs, held open so it owns
	// ACCESS EXCLUSIVE on _m8.history and stays invisible to the loser's probe.
	winner, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("connecting the second session: %v", err)
	}
	defer func() { _ = winner.Close(ctx) }()

	tx, err := winner.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the winner's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
ALTER TABLE _m8.history DROP CONSTRAINT IF EXISTS history_type_check;
ALTER TABLE _m8.history ADD CONSTRAINT m8_history_type_check
    CHECK (type IN ('ops', 'schema', 'logic', 'permissions', 'data'));`); err != nil {
		t.Fatalf("the winner's upgrade: %v", err)
	}

	// The loser. conn is handed to the goroutine and must not be touched here
	// until it comes back -- a pgx.Conn is not safe for concurrent use.
	done := make(chan error, 1)
	go func() { done <- state.NewStore(conn).EnsureSchema(ctx) }()

	// Park until the loser is demonstrably inside the DO block's ALTER, waiting
	// on the winner's lock. Asserting the mode is what keeps this test honest:
	// AccessExclusiveLock on _m8.history is requested by nothing else in
	// schema.sql, so reaching it proves the guard was passed by both sessions
	// and the exception handler is about to be the thing under test.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		if err := tx.QueryRow(ctx, `
SELECT count(*) FROM pg_locks
WHERE relation = '_m8.history'::regclass
  AND mode = 'AccessExclusiveLock'
  AND NOT granted`).Scan(&waiting); err != nil {
			t.Fatalf("polling pg_locks: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second EnsureSchema never blocked on the upgrade; " +
				"this test is not exercising the race it exists for")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing the winner: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureSchema lost the upgrade race instead of tolerating it: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("EnsureSchema never returned after the winner committed")
	}

	// One constraint, not two, and the table now admits 'data'.
	var n int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM pg_constraint
WHERE conrelid = '_m8.history'::regclass
  AND conname = 'm8_history_type_check'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("_m8.history carries %d constraints named m8_history_type_check, want 1", n)
	}
	if _, err := conn.Exec(ctx,
		"INSERT INTO _m8.history (name, type, checksum) VALUES ('probe', 'data', 'x')"); err != nil {
		t.Errorf("the upgraded table still rejects type='data': %v", err)
	}
}
