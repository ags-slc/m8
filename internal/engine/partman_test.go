package engine

import (
	"context"
	"path/filepath"
	"slices"
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
	// The template create_parent made is pg_partman's, not the files'.
	if templates, err := d.ListTables(ctx, "partman"); err != nil || len(templates) != 0 {
		t.Fatalf("partman schema tables listed for dump: %v, %v", templates, err)
	}
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

// The settings that fix a partition set's boundaries are declarable, and
// create_parent receives them; afterwards the ones pg_partman can change are
// reconciled and the ones it cannot are refused.
func TestPartmanDeclaresBoundaryAndConstraintSettings(t *testing.T) {
	conn, engine := partmanDB(t)
	ctx := context.Background()
	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260101_000000__partman.sql"), []byte(installPartman), 0644)
	file := func(directive string) {
		mustWriteFile(t, filepath.Join(dir, "schema", "public", "evt.sql"), []byte(directive+`
CREATE TABLE evt (
    id text NOT NULL,
    org text NOT NULL,
    created_at timestamptz NOT NULL
) PARTITION BY RANGE (created_at);
`), 0644)
	}
	const base = "-- m8:partman evt control=created_at partition_interval='1 week' date_trunc_interval=week constraint_cols=org ignore_default_data=false"
	file(base + " optimize_constraint=5")
	eng := engine(t, dir, false)
	// The boundary settings go to create_parent itself: the children it makes
	// in that call are laid out by them, and an UPDATE afterwards is too late.
	plan, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Partman) != 1 || len(plan.Partman[0].Action.Statements) == 0 ||
		!strings.Contains(plan.Partman[0].Action.Statements[0], "p_date_trunc_interval := 'week'") ||
		!strings.Contains(plan.Partman[0].Action.Statements[0], "p_constraint_cols := '{org}'::text[]") {
		t.Fatalf("create_parent call: %+v", plan.Partman)
	}
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	var row string
	if err := conn.QueryRow(ctx, `SELECT date_trunc_interval || ' ' || constraint_cols::text || ' ' || ignore_default_data || ' ' || optimize_constraint
		FROM partman.part_config WHERE parent_table = 'public.evt'`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if row != "week {org} false 5" {
		t.Errorf("part_config: %s", row)
	}
	mustPlanClean(t, eng)

	file(base + " optimize_constraint=10")
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	mustPlanClean(t, eng)

	// The same array spelled as PostgreSQL renders it is no change.
	file(strings.Replace(base, "constraint_cols=org", `constraint_cols='{"org"}'`, 1) + " optimize_constraint=10")
	mustPlanClean(t, eng)

	file(strings.Replace(base, "date_trunc_interval=week", "date_trunc_interval=day", 1) + " optimize_constraint=10")
	r, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Partman) != 1 || r.Partman[0].Error == nil || !strings.Contains(r.Partman[0].Error.Error(), "date_trunc_interval is week in the database but declared day") {
		t.Errorf("changing a fixed setting: %+v", r.Partman)
	}
}

// A malformed directive is a property of the files, so it stops the run
// before ops/ has changed anything.
func TestPartmanDirectiveErrorStopsBeforeOps(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	dir := setupMigrationsDir(t)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260101_000000__mark.sql"), []byte("CREATE TABLE ops_ran (id int)"), 0644)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "event_log.sql"), eventLogFile("-- m8:partman event_log control=created_at"), 0644)
	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}
	if _, err := eng.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "control and partition_interval are required") {
		t.Fatalf("got %v", err)
	}
	var ran bool
	if err := conn.QueryRow(context.Background(), "SELECT to_regclass('ops_ran') IS NOT NULL").Scan(&ran); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Error("ops/ ran before the malformed directive was refused")
	}
}

// What a directive cannot carry is written beside it, so a fresh environment
// built from the files does not differ silently: a custom template table, or
// a pg_partman too old to declare at all.
func TestDumpNotesWhatTheDirectiveCannotCarry(t *testing.T) {
	conn, _ := partmanDB(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, installPartman+`
		CREATE TABLE audit_log (id text NOT NULL, created_at timestamptz NOT NULL) PARTITION BY RANGE (created_at);
		CREATE TABLE audit_log_tpl (id text NOT NULL, created_at timestamptz NOT NULL);
		SELECT partman.create_parent('public.audit_log', 'created_at', '1 month', p_template_table := 'public.audit_log_tpl');`); err != nil {
		t.Fatal(err)
	}
	// A custom template is the user's own table, so it is dumped.
	if tables, err := dump.NewDumper(conn).ListTables(ctx, "public"); err != nil || !slices.Contains(tables, "audit_log_tpl") {
		t.Fatalf("custom template not listed for dump: %v, %v", tables, err)
	}
	table, err := dump.NewDumper(conn).DumpTable(ctx, "public", "audit_log")
	if err != nil {
		t.Fatal(err)
	}
	if ddl := dump.RenderDDL(table); !strings.Contains(ddl, "-- pg_partman uses the template table public.audit_log_tpl, which m8:partman cannot declare") ||
		!strings.Contains(ddl, "-- m8:partman audit_log") {
		t.Errorf("custom template table:\n%s", ddl)
	}

	if _, err := conn.Exec(ctx, "UPDATE pg_extension SET extversion = '4.7.4' WHERE extname = 'pg_partman'"); err != nil {
		t.Fatal(err)
	}
	table, err = dump.NewDumper(conn).DumpTable(ctx, "public", "audit_log")
	if err != nil {
		t.Fatalf("dump on pg_partman 4.x: %v", err)
	}
	ddl := dump.RenderDDL(table)
	if !strings.Contains(ddl, "pg_partman 4.7.4 is installed") || strings.Contains(ddl, "-- m8:partman audit_log") ||
		!strings.Contains(ddl, "PARTITION BY RANGE (created_at)") {
		t.Errorf("pg_partman 4.x:\n%s", ddl)
	}
}

// create_parent cuts a long template name to 63 bytes, and dump still knows
// it for pg_partman's own; a role that cannot read part_config still dumps.
func TestDumpTemplateNamesAndPermissions(t *testing.T) {
	conn, _ := partmanDB(t)
	ctx := context.Background()
	const long = "audit_log_with_a_rather_long_descriptive_table_name_x"
	if _, err := conn.Exec(ctx, installPartman+`
		CREATE TABLE `+long+` (id text NOT NULL, created_at timestamptz NOT NULL) PARTITION BY RANGE (created_at);
		SELECT partman.create_parent('public.`+long+`', 'created_at', '1 month');
		CREATE ROLE dumper LOGIN;
		GRANT USAGE ON SCHEMA partman TO dumper;`); err != nil {
		t.Fatal(err)
	}
	if tables, err := dump.NewDumper(conn).ListTables(ctx, "partman"); err != nil || len(tables) != 0 {
		t.Fatalf("truncated template listed for dump: %v, %v", tables, err)
	}
	table, err := dump.NewDumper(conn).DumpTable(ctx, "public", long)
	if err != nil {
		t.Fatal(err)
	}
	if ddl := dump.RenderDDL(table); strings.Contains(ddl, "template table") {
		t.Errorf("pg_partman's own template noted as custom:\n%s", ddl)
	}

	if _, err := conn.Exec(ctx, "SET ROLE dumper"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, "RESET ROLE") }()
	if _, err := dump.NewDumper(conn).ListTables(ctx, "public"); err != nil {
		t.Errorf("ListTables without SELECT on part_config: %v", err)
	}
}
