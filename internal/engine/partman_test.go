package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ags-slc/m8/internal/dump"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
)

// partmanDB is testDB on an image with pg_partman installed.
func partmanDB(t *testing.T) (*pgx.Conn, func(*testing.T, string, bool) *Engine) {
	t.Helper()
	conn, sqlDB, connStr, cleanup := startPostgres(t, "",
		testcontainers.WithDockerfile(testcontainers.FromDockerfile{
			Context:   filepath.Join("..", "..", "testdata", "pg-partman"),
			Repo:      "m8-test-pg-partman",
			Tag:       "16",
			KeepImage: true,
		}))
	t.Cleanup(cleanup)
	return conn, func(t *testing.T, dir string, strict bool) *Engine {
		eng, differ := newEngine(conn, sqlDB, connStr, dir, strict)
		if differ != nil {
			t.Cleanup(func() { _ = differ.Close() })
		}
		return eng
	}
}

const installPartman = `CREATE SCHEMA IF NOT EXISTS partman;
CREATE EXTENSION IF NOT EXISTS pg_partman SCHEMA partman;`

func eventLogFile(directive string) []byte {
	return []byte(directive + `
CREATE TABLE event_log (
    id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
) PARTITION BY RANGE (created_at);
CREATE INDEX event_log_created_at_idx ON event_log (created_at);
`)
}

func childCount(t *testing.T, conn *pgx.Conn, table string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_inherits WHERE inhparent = to_regclass($1)`, table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustPlanClean(t *testing.T, eng *Engine) {
	t.Helper()
	r, err := eng.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatPlanOutput(r); !strings.Contains(out, "No pending migrations") {
		t.Fatalf("expected a clean plan, got:\n%s", out)
	}
}

// A declared table is registered with pg_partman after the schema phase
// creates it, with the declared premake applied by create_parent itself -- so
// the first set of children is the declared size, not pg_partman's default --
// and a second plan is clean, which needs '1 month' and the '1 mon' that
// part_config stores to compare equal.
func TestPartmanRegistersDeclaredTable(t *testing.T) {
	conn, engine := partmanDB(t)
	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20261002_001__pg_partman.sql"), []byte(installPartman), 0644)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "event_log.sql"), eventLogFile(
		`-- m8:partman event_log control=created_at partition_interval='1 month' premake=2 infinite_time_partitions=true`), 0644)
	eng := engine(t, dir, false)

	if _, err := eng.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	// premake 2: two months back, the current one, two ahead, and the default.
	if n := childCount(t, conn, "public.event_log"); n != 6 {
		t.Errorf("expected 6 children, got %d", n)
	}
	var infinite bool
	if err := conn.QueryRow(context.Background(),
		`SELECT infinite_time_partitions FROM partman.part_config WHERE parent_table = 'public.event_log'`).Scan(&infinite); err != nil {
		t.Fatal(err)
	}
	if !infinite {
		t.Error("infinite_time_partitions was not applied")
	}
	mustPlanClean(t, eng)
}

// A changed setting is reconciled in place; a changed interval is refused, and
// refused before anything runs -- the premake change declared alongside it is
// not applied either.
func TestPartmanReconcilesSettingsAndRefusesTheInterval(t *testing.T) {
	conn, engine := partmanDB(t)
	dir := setupMigrationsDir(t)
	file := filepath.Join(dir, "schema", "public", "event_log.sql")
	mustWriteFile(t, filepath.Join(dir, "ops", "20261002_001__pg_partman.sql"), []byte(installPartman), 0644)
	mustWriteFile(t, file, eventLogFile(`-- m8:partman event_log control=created_at partition_interval='1 month' premake=2`), 0644)
	eng := engine(t, dir, false)
	ctx := context.Background()
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatal(err)
	}

	mustWriteFile(t, file, eventLogFile(`-- m8:partman event_log control=created_at partition_interval='1 month' premake=5 retention='3 months'`), 0644)
	r, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatPlanOutput(r); !strings.Contains(out, `UPDATE "partman".part_config SET "premake" = 5, "retention" = '3 months'`) {
		t.Fatalf("expected the settings reconciled in place, got:\n%s", out)
	}
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	mustPlanClean(t, eng)

	// The refusal comes before the schema phase: the column declared alongside
	// it must not land either, or the run is a partial apply with an error on
	// the end.
	withColumn := strings.Replace(string(eventLogFile(`-- m8:partman event_log control=created_at partition_interval='1 week' premake=7`)),
		"created_at timestamptz NOT NULL DEFAULT now()", "created_at timestamptz NOT NULL DEFAULT now(),\n    note text", 1)
	mustWriteFile(t, file, []byte(withColumn), 0644)
	if _, err := eng.Apply(ctx); err == nil || !strings.Contains(err.Error(), "cannot change the interval") {
		t.Fatalf("expected the interval change refused, got %v", err)
	}
	var premake int
	var noteExists bool
	if err := conn.QueryRow(ctx, `SELECT premake, EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'event_log' AND column_name = 'note')
		FROM partman.part_config WHERE parent_table = 'public.event_log'`).Scan(&premake, &noteExists); err != nil {
		t.Fatal(err)
	}
	if premake != 5 || noteExists {
		t.Errorf("a refused run changed the database: premake=%d, note column added=%v", premake, noteExists)
	}

	// Interval equality treats '1 mon' and '30 days' as the same; pg_partman,
	// stepping by calendar month in one and by 30 days in the other, does not.
	mustWriteFile(t, file, eventLogFile(`-- m8:partman event_log control=created_at partition_interval='30 days' premake=5`), 0644)
	r, err = eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatPlanOutput(r); !strings.Contains(out, "cannot change the interval") {
		t.Errorf("expected '30 days' refused against '1 mon', got:\n%s", out)
	}
}

func TestPartmanNeedsTheExtension(t *testing.T) {
	_, engine := partmanDB(t)
	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "event_log.sql"), eventLogFile(
		`-- m8:partman event_log control=created_at partition_interval='1 month'`), 0644)
	eng := engine(t, dir, false)

	r, err := eng.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatPlanOutput(r); !strings.Contains(out, "pg_partman is not installed yet") {
		t.Errorf("plan should say pg_partman is missing, got:\n%s", out)
	}
	if _, err := eng.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "ops/") {
		t.Fatalf("expected apply to point at ops/, got %v", err)
	}
}

// The failure this support exists for, end to end: a table created the way a
// Flyway migration creates it, dumped, and planned against the database it came
// from. Before, the dump lost PARTITION BY and the plan was DROP TABLE.
func TestDumpedPartmanTablePlansClean(t *testing.T) {
	conn, engine := partmanDB(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, installPartman+`
		CREATE ROLE app_user;
		CREATE TABLE audit_log (id text NOT NULL, org text NOT NULL, created_at timestamptz NOT NULL)
			PARTITION BY RANGE (created_at);
		CREATE INDEX audit_log_org_idx ON audit_log (org, created_at);
		SELECT partman.create_parent('public.audit_log', 'created_at', '1 month');
		UPDATE partman.part_config SET infinite_time_partitions = true WHERE parent_table = 'public.audit_log';
		GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA public TO app_user;`); err != nil {
		t.Fatal(err)
	}

	d := dump.NewDumper(conn)
	table, err := d.DumpTable(ctx, "public", "audit_log")
	if err != nil {
		t.Fatal(err)
	}
	ddl := dump.RenderDDL(table)
	if !strings.HasPrefix(ddl, "-- m8:partman audit_log control=created_at partition_interval='1 mon' default_table=true") ||
		!strings.Contains(ddl, "infinite_time_partitions=true") {
		t.Fatalf("expected the live configuration as a directive, got:\n%s", ddl)
	}

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "audit_log.sql"),
		[]byte(ddl+"\nGRANT SELECT, INSERT ON public.audit_log TO app_user;\n"), 0644)
	mustPlanClean(t, engine(t, dir, false))
}

// Maintenance that never runs is invisible to every other check: the table is
// registered, nothing is pending, and new rows go to the default partition
// once the premade months run out. status says so.
func TestStatusReportsPartmanMaintenance(t *testing.T) {
	conn, engine := partmanDB(t)
	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20261002_001__pg_partman.sql"), []byte(installPartman), 0644)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "event_log.sql"), eventLogFile(
		`-- m8:partman event_log control=created_at partition_interval='1 month'`), 0644)
	eng := engine(t, dir, false)
	ctx := context.Background()

	st, err := eng.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatStatusOutput(st); !strings.Contains(out, "public.event_log not registered yet") {
		t.Errorf("expected the table reported unregistered, got:\n%s", out)
	}

	if _, err := eng.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err = eng.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if out := FormatStatusOutput(st); !strings.Contains(out, "public.event_log maintenance has never run") {
		t.Errorf("expected never-run maintenance flagged, got:\n%s", out)
	}

	if _, err := conn.Exec(ctx, `SELECT partman.run_maintenance('public.event_log')`); err != nil {
		t.Fatal(err)
	}
	if st, err = eng.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if out := FormatStatusOutput(st); !strings.Contains(out, "public.event_log maintenance last ran") {
		t.Errorf("expected the last run reported, got:\n%s", out)
	}
}

// create_parent takes an ACCESS EXCLUSIVE lock on the parent. Behind a
// long-running reader that lock would be waited for without bound -- and every
// query against the table would queue behind the wait.
func TestPartmanCreateParentDoesNotWaitForeverOnALock(t *testing.T) {
	conn, engine := partmanDB(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, installPartman+`
		CREATE TABLE event_log (id text NOT NULL, created_at timestamptz NOT NULL DEFAULT now())
			PARTITION BY RANGE (created_at);
		CREATE INDEX event_log_created_at_idx ON event_log (created_at);`); err != nil {
		t.Fatal(err)
	}

	reader, err := pgx.Connect(ctx, conn.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(ctx) }()
	if _, err := reader.Exec(ctx, `BEGIN; LOCK TABLE event_log IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}

	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "event_log.sql"), eventLogFile(
		`-- m8:partman event_log control=created_at partition_interval='1 month'`), 0644)
	eng := engine(t, dir, false)
	eng.config.LockTimeout = 200 * time.Millisecond

	applyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = eng.Apply(applyCtx)
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("expected apply to give up on the lock, got %v", err)
	}
}
