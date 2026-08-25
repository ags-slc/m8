package dump

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func testDB(t *testing.T) (*pgx.Conn, func()) {
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

	cleanup := func() {
		_ = conn.Close(ctx)
		_ = container.Terminate(ctx)
	}
	return conn, cleanup
}

func TestDumpTable(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE TABLE users (
			id BIGSERIAL PRIMARY KEY,
			email TEXT NOT NULL,
			name TEXT,
			verified BOOLEAN NOT NULL DEFAULT false,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX idx_users_email ON users (email);
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	table, err := d.DumpTable(ctx, "public", "users")
	if err != nil {
		t.Fatal(err)
	}

	if len(table.Columns) != 5 {
		t.Errorf("expected 5 columns, got %d", len(table.Columns))
	}
	if table.PK == nil {
		t.Error("expected primary key")
	}
	if len(table.Indexes) != 1 {
		t.Errorf("expected 1 non-constraint index, got %d", len(table.Indexes))
	}

	ddl := RenderDDL(table)
	if !strings.Contains(ddl, "BIGSERIAL") {
		t.Error("expected BIGSERIAL in DDL")
	}
	if !strings.Contains(ddl, "NOT NULL") {
		t.Error("expected NOT NULL in DDL")
	}
	if !strings.Contains(ddl, "DEFAULT false") {
		t.Error("expected DEFAULT false in DDL")
	}
	if !strings.Contains(ddl, "idx_users_email") {
		t.Error("expected index in DDL")
	}
}

func TestDumpTableWithFK(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE TABLE users (id SERIAL PRIMARY KEY);
		CREATE TABLE orders (
			id SERIAL PRIMARY KEY,
			user_id INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			status TEXT NOT NULL DEFAULT 'pending',
			CONSTRAINT orders_status_check CHECK (status IN ('pending','paid','shipped'))
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	table, err := d.DumpTable(ctx, "public", "orders")
	if err != nil {
		t.Fatal(err)
	}

	if len(table.FKs) != 1 {
		t.Fatalf("expected 1 FK, got %d", len(table.FKs))
	}
	if table.FKs[0].OnDelete != "CASCADE" {
		t.Errorf("expected ON DELETE CASCADE, got %q", table.FKs[0].OnDelete)
	}
	if len(table.Checks) != 1 {
		t.Errorf("expected 1 check constraint, got %d", len(table.Checks))
	}

	ddl := RenderDDL(table)
	if !strings.Contains(ddl, "REFERENCES public.users") {
		t.Error("expected FK reference in DDL")
	}
	if !strings.Contains(ddl, "ON DELETE CASCADE") {
		t.Error("expected ON DELETE CASCADE in DDL")
	}
}

func TestDumpTableWithPartialIndex(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE TABLE events (id SERIAL PRIMARY KEY, status TEXT, name TEXT);
		CREATE INDEX idx_events_active ON events (name) WHERE status = 'active';
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	table, err := d.DumpTable(ctx, "public", "events")
	if err != nil {
		t.Fatal(err)
	}

	if len(table.Indexes) != 1 {
		t.Fatalf("expected 1 index, got %d", len(table.Indexes))
	}
	if !strings.Contains(table.Indexes[0].Definition, "WHERE") {
		t.Error("expected partial index with WHERE clause")
	}
}

func TestListSchemas(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, "CREATE SCHEMA app_admin;")
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	schemas, err := d.ListSchemas(ctx)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	for _, s := range schemas {
		found[s] = true
	}
	if !found["public"] {
		t.Error("expected public schema")
	}
	if !found["app_admin"] {
		t.Error("expected app_admin schema")
	}
}

func TestListTables(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE TABLE alpha (id INT);
		CREATE TABLE beta (id INT);
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	tables, err := d.ListTables(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}

	if len(tables) != 2 {
		t.Fatalf("expected 2 tables, got %d", len(tables))
	}
	if tables[0] != "alpha" || tables[1] != "beta" {
		t.Errorf("expected [alpha, beta], got %v", tables)
	}
}

func TestDumpFunction(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE OR REPLACE FUNCTION hello(name TEXT)
		RETURNS TEXT
		LANGUAGE sql
		AS $$ SELECT 'hello ' || name; $$;
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	funcs, err := d.ListFunctions(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}

	if len(funcs) != 1 {
		t.Fatalf("expected 1 function, got %d", len(funcs))
	}
	if funcs[0].Name != "hello" {
		t.Errorf("expected hello, got %q", funcs[0].Name)
	}
	if funcs[0].Kind != "f" {
		t.Errorf("expected kind=f, got %q", funcs[0].Kind)
	}

	rendered := RenderFunction(&funcs[0])
	if !strings.Contains(rendered, "CREATE OR REPLACE FUNCTION") {
		t.Error("expected CREATE OR REPLACE FUNCTION in rendered output")
	}
}

func TestDumpProcedure(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE OR REPLACE PROCEDURE do_nothing()
		LANGUAGE plpgsql AS $$
		BEGIN
			-- nothing
		END;
		$$;
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	funcs, err := d.ListFunctions(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}

	if len(funcs) != 1 {
		t.Fatalf("expected 1 procedure, got %d", len(funcs))
	}
	if funcs[0].Kind != "p" {
		t.Errorf("expected kind=p, got %q", funcs[0].Kind)
	}

	rendered := RenderFunction(&funcs[0])
	if !strings.Contains(rendered, "CREATE OR REPLACE PROCEDURE") {
		t.Error("expected CREATE OR REPLACE PROCEDURE in rendered output")
	}
}

func TestDumpView(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE TABLE users (id SERIAL PRIMARY KEY, name TEXT, active BOOLEAN DEFAULT true);
		CREATE OR REPLACE VIEW active_users AS
			SELECT id, name FROM users WHERE active = true;
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	views, err := d.ListViews(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}

	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	if views[0].Name != "active_users" {
		t.Errorf("expected active_users, got %q", views[0].Name)
	}

	rendered := RenderView(&views[0])
	if !strings.Contains(rendered, "CREATE OR REPLACE VIEW public.active_users") {
		t.Errorf("expected schema-qualified CREATE OR REPLACE VIEW, got:\n%s", rendered)
	}
}

// A dumped view must be schema-qualified, must not be double-terminated, and
// must carry its reloptions. Desired state is replayed through a connection
// pool, so an unqualified name silently lands in public; pg_get_viewdef()
// already ends in ";" so a second one emits an empty trailing statement; and a
// dropped security_barrier/security_invoker changes the view's access
// semantics.
func TestDumpViewIsQualifiedAndKeepsOptions(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE SCHEMA reporting;
		CREATE TABLE reporting.accounts (id INT, secret TEXT);
		CREATE VIEW reporting.safe_accounts
			WITH (security_barrier = true)
			AS SELECT id FROM reporting.accounts;
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	views, err := d.ListViews(ctx, "reporting")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}

	rendered := RenderView(&views[0])

	if !strings.HasPrefix(rendered, "CREATE OR REPLACE VIEW reporting.safe_accounts") {
		t.Errorf("view name is not schema-qualified:\n%s", rendered)
	}
	if strings.Contains(rendered, ";;") {
		t.Errorf("view DDL is double-terminated:\n%s", rendered)
	}
	if !strings.Contains(rendered, "security_barrier=true") &&
		!strings.Contains(rendered, "security_barrier = true") {
		t.Errorf("view DDL dropped its reloptions:\n%s", rendered)
	}

	// The rendered DDL must round-trip into a database that has no reporting
	// schema on its search_path.
	if _, err := conn.Exec(ctx, "DROP VIEW reporting.safe_accounts"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SET search_path TO public"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, rendered); err != nil {
		t.Fatalf("rendered view DDL failed to replay: %v\n%s", err, rendered)
	}
	var nsp string
	if err := conn.QueryRow(ctx, `
		SELECT n.nspname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = 'safe_accounts' AND c.relkind = 'v'
	`).Scan(&nsp); err != nil {
		t.Fatal(err)
	}
	if nsp != "reporting" {
		t.Errorf("replayed view landed in schema %q, want reporting", nsp)
	}
}

func TestDumpGrants(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE TABLE docs (id INT);
		CREATE ROLE reader;
		GRANT SELECT ON docs TO reader;
		GRANT SELECT ON docs TO PUBLIC;
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)

	grants, err := d.ListGrants(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Errorf("expected 1 role grant, got %d", len(grants))
	}

	publicGrants, err := d.ListPublicGrants(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	if len(publicGrants) != 1 {
		t.Errorf("expected 1 public grant, got %d", len(publicGrants))
	}

	allGrants := append(grants, publicGrants...)
	rendered := RenderGrants(allGrants, "public")
	if !strings.Contains(rendered, "GRANT SELECT ON public.docs TO reader") {
		t.Errorf("expected grant to reader in:\n%s", rendered)
	}
	if !strings.Contains(rendered, "GRANT SELECT ON public.docs TO PUBLIC") {
		t.Errorf("expected grant to PUBLIC in:\n%s", rendered)
	}
}

func TestRenderDDLRoundtrip(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE TABLE products (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			price NUMERIC(10,2) NOT NULL DEFAULT 0,
			active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX idx_products_name ON products (name);
		CREATE UNIQUE INDEX idx_products_active_name ON products (name) WHERE active = true;
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	table, err := d.DumpTable(ctx, "public", "products")
	if err != nil {
		t.Fatal(err)
	}

	ddl := RenderDDL(table)

	// The DDL should be valid SQL that recreates the table
	if !strings.Contains(ddl, "CREATE TABLE public.products") {
		t.Error("expected CREATE TABLE")
	}
	if !strings.Contains(ddl, "BIGSERIAL") {
		t.Error("expected BIGSERIAL")
	}
	if !strings.Contains(ddl, "idx_products_name") {
		t.Error("expected idx_products_name")
	}
	if !strings.Contains(ddl, "idx_products_active_name") {
		t.Error("expected idx_products_active_name")
	}
	if !strings.Contains(ddl, "WHERE") {
		t.Error("expected partial index WHERE clause")
	}
}

// TestRenderDDLQualifiesNonPublicSchema pins the contract that dumped DDL is
// schema-qualified. The desired state is replayed into a throwaway database
// through a connection pool, where a "SET search_path" does not reliably reach
// the next statement — so unqualified DDL from schema/materialized/ would be
// created in public, the schema-scoped diff would see an empty desired state,
// and the plan would propose dropping the real tables.
func TestRenderDDLQualifiesNonPublicSchema(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	_, err := conn.Exec(ctx, `
		CREATE SCHEMA materialized;
		CREATE TABLE materialized.parent (
			id BIGINT PRIMARY KEY
		);
		CREATE TABLE materialized.rpt_thing (
			id BIGINT PRIMARY KEY,
			parent_id BIGINT NOT NULL REFERENCES materialized.parent (id),
			loaded_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX idx_rpt_thing_loaded_at ON materialized.rpt_thing (loaded_at DESC);
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	table, err := d.DumpTable(ctx, "materialized", "rpt_thing")
	if err != nil {
		t.Fatal(err)
	}

	ddl := RenderDDL(table)

	if !strings.Contains(ddl, "CREATE TABLE materialized.rpt_thing") {
		t.Errorf("expected schema-qualified CREATE TABLE, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, "REFERENCES materialized.parent") {
		t.Errorf("expected schema-qualified FK target, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, "ON materialized.rpt_thing") {
		t.Errorf("expected schema-qualified index target, got:\n%s", ddl)
	}

	// The real proof: replaying the DDL into a database that has the schema but
	// not the table must recreate it *in that schema*, not in public.
	if _, err := conn.Exec(ctx, `DROP TABLE materialized.rpt_thing`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatalf("replaying dumped DDL failed: %v\n%s", err, ddl)
	}
	var landedIn string
	err = conn.QueryRow(ctx, `
		SELECT n.nspname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = 'rpt_thing' AND c.relkind = 'r'
	`).Scan(&landedIn)
	if err != nil {
		t.Fatal(err)
	}
	if landedIn != "materialized" {
		t.Errorf("replayed table landed in schema %q, want \"materialized\"", landedIn)
	}
}

// Grants must be schema-qualified for the same reason view DDL must be: they
// are replayed over a connection pool, where no session search_path survives
// to the next statement. An unqualified GRANT either errors out or lands on a
// same-named relation in public.
func TestDumpGrantsAreSchemaQualified(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE SCHEMA radar;
		CREATE TABLE radar.audit_log (id INT);
		CREATE ROLE viewport_readonly;
		GRANT SELECT ON radar.audit_log TO viewport_readonly;
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	grants, err := d.ListGrants(ctx, "radar")
	if err != nil {
		t.Fatal(err)
	}

	rendered := RenderGrants(grants, "radar")
	if !strings.Contains(rendered, "ON radar.audit_log TO viewport_readonly") {
		t.Errorf("grant target is not schema-qualified:\n%s", rendered)
	}

	if _, err := conn.Exec(ctx, "REVOKE ALL ON radar.audit_log FROM viewport_readonly"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SET search_path TO public"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, rendered); err != nil {
		t.Fatalf("rendered grants failed to replay without a search_path: %v\n%s", err, rendered)
	}
}

// TestRenderDDLGeneratedColumn pins that a GENERATED ALWAYS AS (...) STORED
// column round-trips. Postgres keeps the generation expression in pg_attrdef
// next to ordinary defaults, so a dump that reads pg_attrdef without checking
// attgenerated emits it as a DEFAULT — and a DEFAULT may not reference sibling
// columns, so Postgres rejects the DDL with 0A000. Because m8 diffs a whole
// schema folder as one batch, a single such table poisons every diff in it.
func TestRenderDDLGeneratedColumn(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	_, err := conn.Exec(ctx, `
		CREATE TABLE progress (
			id BIGINT PRIMARY KEY,
			started_at  TIMESTAMPTZ,
			finished_at TIMESTAMPTZ,
			duration_seconds INTEGER GENERATED ALWAYS AS (
				CASE WHEN finished_at IS NOT NULL AND started_at IS NOT NULL
				     THEN (EXTRACT(epoch FROM (finished_at - started_at)))::integer
				     ELSE NULL::integer
				END
			) STORED
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	table, err := d.DumpTable(ctx, "public", "progress")
	if err != nil {
		t.Fatal(err)
	}
	ddl := RenderDDL(table)

	if !strings.Contains(ddl, "GENERATED ALWAYS AS (") || !strings.Contains(ddl, ") STORED") {
		t.Errorf("expected a generated-column clause, got:\n%s", ddl)
	}
	if strings.Contains(ddl, "DEFAULT CASE") || strings.Contains(ddl, "DEFAULT\nCASE") {
		t.Errorf("generation expression was emitted as a DEFAULT:\n%s", ddl)
	}
	// The expression must be folded onto one line, not pretty-printed across
	// several, or the column definition is unreadable.
	for _, line := range strings.Split(ddl, "\n") {
		if strings.Contains(line, "GENERATED ALWAYS AS (") && !strings.Contains(line, ") STORED") {
			t.Errorf("generated-column clause spans multiple lines: %q", line)
		}
	}

	// The real proof: the dumped DDL must replay.
	if _, err := conn.Exec(ctx, `DROP TABLE progress`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatalf("replaying dumped DDL failed: %v\n%s", err, ddl)
	}
	var isGenerated string
	if err := conn.QueryRow(ctx, `
		SELECT a.attgenerated::text FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relname = 'progress' AND a.attname = 'duration_seconds'
	`).Scan(&isGenerated); err != nil {
		t.Fatal(err)
	}
	if isGenerated != "s" {
		t.Errorf("replayed column is not stored-generated (attgenerated=%q)", isGenerated)
	}
}

// EXECUTE privileges live in pg_proc.proacl, which
// information_schema.role_table_grants does not cover. Without capturing them
// a dumped schema silently loses every routine grant, and a procedure the
// application calls becomes uncallable on a rebuilt database.
func TestDumpRoutineGrants(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE SCHEMA radar;
		CREATE ROLE app;
		CREATE PROCEDURE radar.record_decision(p_id text, p_note text DEFAULT NULL)
			LANGUAGE sql AS $$ SELECT 1 $$;
		CREATE FUNCTION radar.score(p_id text) RETURNS int
			LANGUAGE sql AS $$ SELECT 1 $$;
		REVOKE ALL ON PROCEDURE radar.record_decision(text, text) FROM PUBLIC;
		REVOKE ALL ON FUNCTION radar.score(text) FROM PUBLIC;
		GRANT EXECUTE ON PROCEDURE radar.record_decision(text, text) TO app;
	`)
	if err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	grants, err := d.ListRoutineGrants(ctx, "radar")
	if err != nil {
		t.Fatal(err)
	}

	if len(grants) != 1 {
		t.Fatalf("expected exactly the one non-default grant, got %d: %+v", len(grants), grants)
	}
	if grants[0].Grantee != "app" || grants[0].Privilege != "EXECUTE" {
		t.Errorf("unexpected grant: %+v", grants[0])
	}

	rendered := RenderRoutineGrants(grants)
	if !strings.Contains(rendered, "radar.record_decision(") {
		t.Errorf("grant is not schema-qualified with a signature:\n%s", rendered)
	}
	if !strings.Contains(rendered, "TO app") {
		t.Errorf("grantee missing:\n%s", rendered)
	}

	// It must replay without a search_path, and restore the privilege exactly.
	if _, err := conn.Exec(ctx, "REVOKE ALL ON PROCEDURE radar.record_decision(text, text) FROM app"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SET search_path TO public"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, rendered); err != nil {
		t.Fatalf("rendered routine grants failed to replay: %v\n%s", err, rendered)
	}

	after, err := d.ListRoutineGrants(ctx, "radar")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Grantee != "app" {
		t.Errorf("replay did not restore the grant: %+v", after)
	}
}

// information_schema.role_table_grants only shows grants the CURRENT role can
// see -- ones where it is the grantor, the grantee, or a member of the grantee.
// Dumped by an ordinary application role, a grant to an unrelated service role
// disappears without a trace, and the permissions file looks complete while
// leaving that service without access on a rebuilt database.
func TestDumpGrantsSeesGrantsToUnrelatedRoles(t *testing.T) {
	conn, cleanup := testDB(t)
	defer cleanup()

	ctx := context.Background()
	_, err := conn.Exec(ctx, `
		CREATE SCHEMA radar;
		CREATE ROLE owner_role;
		CREATE ROLE app LOGIN;
		CREATE ROLE unrelated_service;
		GRANT owner_role TO postgres;
		GRANT CREATE, USAGE ON SCHEMA radar TO owner_role;
		SET ROLE owner_role;
		CREATE TABLE radar.outcome (id int);
		GRANT SELECT, INSERT ON radar.outcome TO app;
		GRANT SELECT ON radar.outcome TO unrelated_service;
		RESET ROLE;
		GRANT USAGE ON SCHEMA radar TO app;
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Dump as `app`: a role that is neither the owner nor a member of
	// unrelated_service. This is the shape of a real dump run by a service
	// account, and the case information_schema silently truncates.
	if _, err := conn.Exec(ctx, "SET ROLE app"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, "RESET ROLE") }()

	d := NewDumper(conn)
	grants, err := d.ListGrants(ctx, "radar")
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string][]string{}
	for _, g := range grants {
		seen[g.Grantee] = append(seen[g.Grantee], g.Privilege)
	}

	if len(seen["unrelated_service"]) == 0 {
		t.Errorf(
			"grant to a role the dumping user is unrelated to was lost; captured only %v",
			seen,
		)
	}
	if len(seen["app"]) != 2 {
		t.Errorf("expected SELECT+INSERT for app, got %v", seen["app"])
	}
	if _, ok := seen["owner_role"]; ok {
		t.Errorf("the owner's own implicit privileges should not be emitted: %v", seen)
	}
}
