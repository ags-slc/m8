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
	if !strings.Contains(ddl, `REFERENCES "public"."users"`) {
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
	if !strings.Contains(rendered, `CREATE OR REPLACE VIEW "public"."active_users"`) {
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

	if !strings.HasPrefix(rendered, `CREATE OR REPLACE VIEW "reporting"."safe_accounts"`) {
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

	// One query now covers both: PUBLIC is grantee OID 0 in the same relacl.
	grants, err := d.ListGrants(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Errorf("expected the role grant and the PUBLIC grant, got %d: %+v", len(grants), grants)
	}

	rendered := RenderGrants(grants, "public")
	if !strings.Contains(rendered, `GRANT SELECT ON TABLE "public"."docs" TO "reader"`) {
		t.Errorf("expected grant to reader in:\n%s", rendered)
	}
	if !strings.Contains(rendered, `GRANT SELECT ON TABLE "public"."docs" TO PUBLIC`) {
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
	if !strings.Contains(ddl, `CREATE TABLE "public"."products"`) {
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

	if !strings.Contains(ddl, `CREATE TABLE "materialized"."rpt_thing"`) {
		t.Errorf("expected schema-qualified CREATE TABLE, got:\n%s", ddl)
	}
	if !strings.Contains(ddl, `REFERENCES "materialized"."parent"`) {
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
	if !strings.Contains(rendered, `ON TABLE "radar"."audit_log" TO "viewport_readonly"`) {
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
	// pg_get_expr pretty-prints a CASE across several lines. The dumper emits
	// it byte-for-byte anyway -- reflowing it would mean rewriting arbitrary
	// SQL without parsing it, which is how whitespace inside string literals
	// used to get eaten (see
	// TestRenderDDLGeneratedColumnPreservesStringLiterals). pg_dump makes the
	// same call and the same choice.
	var catalogExpr string
	if err := conn.QueryRow(ctx, `
		SELECT pg_get_expr(ad.adbin, ad.adrelid)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		WHERE c.relname = 'progress' AND a.attname = 'duration_seconds'
	`).Scan(&catalogExpr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "GENERATED ALWAYS AS ("+catalogExpr+") STORED") {
		t.Errorf("generation expression was rewritten on the way out.\ncatalog:\n%s\n\ndumped:\n%s",
			catalogExpr, ddl)
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
	if !strings.Contains(rendered, `"radar"."record_decision"(`) {
		t.Errorf("grant is not schema-qualified with a signature:\n%s", rendered)
	}
	if !strings.Contains(rendered, `TO "app"`) {
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

// TestRenderDDLGeneratedColumnPreservesStringLiterals pins that the dumper does
// not rewrite the expression it captured.
//
// The generated-column path used to fold the expression onto one line with
// strings.Fields/Join, which collapses whitespace inside STRING LITERALS as
// well as between SQL tokens. A column generated from
//
//	a || E'\n  two spaces  here'
//
// dumped as (a || ' two spaces here'::text): valid SQL, replays cleanly, and
// computes different values from the column it claims to describe. The sibling
// DEFAULT path was never collapsed, so the two halves of the same CREATE TABLE
// disagreed about whether literals were safe to touch.
func TestRenderDDLGeneratedColumnPreservesStringLiterals(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE literals (
			id BIGINT PRIMARY KEY,
			a  TEXT NOT NULL,
			padded TEXT NOT NULL DEFAULT 'two  spaces',
			g  TEXT GENERATED ALWAYS AS (a || E'\n  two spaces  here') STORED
		);
	`); err != nil {
		t.Fatal(err)
	}

	// What the live column actually computes, and how the catalog spells it.
	if _, err := conn.Exec(ctx, `INSERT INTO literals (id, a) VALUES (1, 'x')`); err != nil {
		t.Fatal(err)
	}
	var wantValue, wantExpr, wantDefault string
	if err := conn.QueryRow(ctx, `SELECT g FROM literals WHERE id = 1`).Scan(&wantValue); err != nil {
		t.Fatal(err)
	}
	const exprQuery = `
		SELECT pg_get_expr(ad.adbin, ad.adrelid)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		WHERE c.relname = 'literals' AND a.attname = $1`
	if err := conn.QueryRow(ctx, exprQuery, "g").Scan(&wantExpr); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, exprQuery, "padded").Scan(&wantDefault); err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	table, err := d.DumpTable(ctx, "public", "literals")
	if err != nil {
		t.Fatal(err)
	}
	ddl := RenderDDL(table)

	if _, err := conn.Exec(ctx, `DROP TABLE literals`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatalf("replaying dumped DDL failed: %v\n%s", err, ddl)
	}

	var gotExpr, gotDefault string
	if err := conn.QueryRow(ctx, exprQuery, "g").Scan(&gotExpr); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, exprQuery, "padded").Scan(&gotDefault); err != nil {
		t.Fatal(err)
	}
	if gotExpr != wantExpr {
		t.Errorf("generation expression changed across the round trip:\n  live:    %q\n  replayed: %q\n\n%s",
			wantExpr, gotExpr, ddl)
	}
	if gotDefault != wantDefault {
		t.Errorf("column default changed across the round trip:\n  live:    %q\n  replayed: %q",
			wantDefault, gotDefault)
	}

	// The point of the expression is the value it computes.
	if _, err := conn.Exec(ctx, `INSERT INTO literals (id, a) VALUES (1, 'x')`); err != nil {
		t.Fatal(err)
	}
	var gotValue string
	if err := conn.QueryRow(ctx, `SELECT g FROM literals WHERE id = 1`).Scan(&gotValue); err != nil {
		t.Fatal(err)
	}
	if gotValue != wantValue {
		t.Errorf("replayed column computes a different value:\n  live:     %q\n  replayed: %q",
			wantValue, gotValue)
	}
}

// TestDumpQuotesEveryIdentifier pins that dumped DDL survives identifiers that
// are not all-lowercase-simple: mixed case, spaces, and reserved words.
//
// None of it was quoted. QualifiedName was plain concatenation, column and
// constraint names went out bare, and internal/schema's quoteIdent was never
// reachable from internal/dump. On an all-lowercase database that is invisible;
// on anything else the dump either fails to replay or -- see
// TestDumpQuotesForeignKeyTargets -- replays against the wrong object.
func TestDumpQuotesEveryIdentifier(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	if _, err := conn.Exec(ctx, `
		CREATE SCHEMA "My Reports";
		CREATE TABLE "My Reports"."Users" (
			"Id" BIGINT NOT NULL,
			CONSTRAINT "Users pkey" PRIMARY KEY ("Id")
		);
		CREATE TABLE "My Reports"."Order" (
			"Id"       BIGINT NOT NULL,
			"select"   TEXT NOT NULL,
			"Group Id" BIGINT,
			"limit"    INTEGER NOT NULL DEFAULT 0,
			CONSTRAINT "Order pkey" PRIMARY KEY ("Id"),
			CONSTRAINT "Order uniq" UNIQUE ("select"),
			CONSTRAINT "Order limit ck" CHECK ("limit" >= 0),
			CONSTRAINT "Order user fk" FOREIGN KEY ("Group Id")
				REFERENCES "My Reports"."Users" ("Id") ON DELETE CASCADE
		);
		CREATE INDEX "Order select idx" ON "My Reports"."Order" ("select");
	`); err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	table, err := d.DumpTable(ctx, "My Reports", "Order")
	if err != nil {
		t.Fatal(err)
	}
	ddl := RenderDDL(table)

	// Replaying is the whole test: unquoted, this is a syntax error at CREATE
	// TABLE My Reports.Order, and if it got past that, at the column named
	// select.
	if _, err := conn.Exec(ctx, `DROP TABLE "My Reports"."Order"`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatalf("dumped DDL does not replay: %v\n%s", err, ddl)
	}

	// Every name must come back exactly as it went in -- not case-folded.
	var cols []string
	rows, err := conn.Query(ctx, `
		SELECT a.attname FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'My Reports' AND c.relname = 'Order'
		  AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"Id", "select", "Group Id", "limit"}
	if strings.Join(cols, "|") != strings.Join(want, "|") {
		t.Errorf("columns came back as %v, want %v", cols, want)
	}

	for _, con := range []string{"Order pkey", "Order uniq", "Order limit ck", "Order user fk"} {
		var exists bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_constraint con
				JOIN pg_class c ON c.oid = con.conrelid
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = 'My Reports' AND c.relname = 'Order' AND con.conname = $1
			)`, con).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("constraint %q did not survive the round trip:\n%s", con, ddl)
		}
	}
}

// TestDumpQuotesForeignKeyTargets pins the case where missing quotes do not
// error at all.
//
// Everything here except one table name is lowercase-simple, so the unquoted
// DDL parses and replays without complaint. But an unquoted REFERENCES
// public.Users is folded to users by the server, and users also exists: the
// constraint is created against a DIFFERENT table. Nothing surfaces at dump
// time, at replay time, or in any error -- only later, in whichever row the
// wrong key does or does not reject.
func TestDumpQuotesForeignKeyTargets(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE public."Users" (id BIGINT PRIMARY KEY);
		CREATE TABLE public.users     (id BIGINT PRIMARY KEY);
		CREATE TABLE public.membership (
			id      BIGINT PRIMARY KEY,
			user_id BIGINT,
			CONSTRAINT membership_user_fk FOREIGN KEY (user_id) REFERENCES public."Users" (id)
		);
	`); err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	table, err := d.DumpTable(ctx, "public", "membership")
	if err != nil {
		t.Fatal(err)
	}
	ddl := RenderDDL(table)

	if _, err := conn.Exec(ctx, `DROP TABLE public.membership`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, ddl); err != nil {
		t.Fatalf("dumped DDL does not replay: %v\n%s", err, ddl)
	}

	var target string
	if err := conn.QueryRow(ctx, `
		SELECT cr.relname
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_class cr ON cr.oid = con.confrelid
		WHERE n.nspname = 'public' AND c.relname = 'membership' AND con.contype = 'f'
	`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != "Users" {
		t.Errorf("the replayed foreign key points at %q, not the \"Users\" it was captured from\n%s",
			target, ddl)
	}
}

// A view and its grants carry identifiers too.
func TestDumpQuotesViewAndGrantIdentifiers(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	if _, err := conn.Exec(ctx, `
		CREATE ROLE "Read Only";
		CREATE SCHEMA "My Reports";
		CREATE TABLE "My Reports"."Order" (id BIGINT PRIMARY KEY);
		CREATE VIEW "My Reports"."Order List" AS SELECT id FROM "My Reports"."Order";
		GRANT SELECT ON "My Reports"."Order List" TO "Read Only";
	`); err != nil {
		t.Fatal(err)
	}

	d := NewDumper(conn)
	views, err := d.ListViews(ctx, "My Reports")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(views))
	}
	rendered := RenderView(&views[0])

	grants, err := d.ListGrants(ctx, "My Reports")
	if err != nil {
		t.Fatal(err)
	}
	renderedGrants := RenderGrants(grants, "My Reports")

	if _, err := conn.Exec(ctx, `DROP VIEW "My Reports"."Order List"`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SET search_path TO public"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, rendered); err != nil {
		t.Fatalf("dumped view does not replay: %v\n%s", err, rendered)
	}
	if _, err := conn.Exec(ctx, renderedGrants); err != nil {
		t.Fatalf("dumped grants do not replay: %v\n%s", err, renderedGrants)
	}

	var canSelect bool
	if err := conn.QueryRow(ctx,
		`SELECT has_table_privilege('Read Only', '"My Reports"."Order List"', 'SELECT')`,
	).Scan(&canSelect); err != nil {
		t.Fatal(err)
	}
	if !canSelect {
		t.Errorf("the replayed grant did not restore SELECT for \"Read Only\":\n%s", renderedGrants)
	}
}

// renderAllPermissions is what `m8 dump` writes to permissions/grants_{schema}.sql:
// schema grants, then the PUBLIC revokes, then relation, column, and routine
// grants.
func renderAllPermissions(t *testing.T, ctx context.Context, d *Dumper, schema string) string {
	t.Helper()
	schemaGrants, err := d.ListSchemaGrants(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := d.ListGrants(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	columnGrants, err := d.ListColumnGrants(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	routineGrants, err := d.ListRoutineGrants(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	revokes, err := d.ListPublicRevokes(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	return RenderSchemaGrants(schemaGrants) + "\n" +
		RenderPublicRevokes(revokes) + "\n" +
		RenderGrants(append(grants, columnGrants...), schema) + "\n" +
		RenderRoutineGrants(routineGrants)
}

// TestDumpCapturesSchemaUsageGrants pins the grant without which every other
// grant is inert.
//
// pg_namespace.nspacl was never read. CREATE SCHEMA grants USAGE to nobody, so a
// permissions file full of correct GRANT SELECT statements produces a rebuilt
// database where the application still cannot see a single table -- which is the
// exact failure the relacl work was written to prevent, one level up.
func TestDumpCapturesSchemaUsageGrants(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	if _, err := conn.Exec(ctx, `
		CREATE ROLE reporting_reader LOGIN;
		CREATE SCHEMA reporting;
		CREATE TABLE reporting.metrics (id BIGINT PRIMARY KEY);
		GRANT USAGE ON SCHEMA reporting TO reporting_reader;
		GRANT SELECT ON reporting.metrics TO reporting_reader;
	`); err != nil {
		t.Fatal(err)
	}

	rendered := renderAllPermissions(t, ctx, NewDumper(conn), "reporting")

	// Rebuild: the schema exists with no privileges on it, as CREATE SCHEMA
	// leaves it.
	if _, err := conn.Exec(ctx, `
		REVOKE ALL ON SCHEMA reporting FROM reporting_reader;
		REVOKE ALL ON reporting.metrics FROM reporting_reader;
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, rendered); err != nil {
		t.Fatalf("rendered permissions failed to replay: %v\n%s", err, rendered)
	}

	// The end-to-end property: the role can actually read the table. Without
	// USAGE on the schema this fails with 42501 even though the table grant is
	// in place.
	if _, err := conn.Exec(ctx, `SET ROLE reporting_reader`); err != nil {
		t.Fatal(err)
	}
	_, selErr := conn.Exec(ctx, `SELECT 1 FROM reporting.metrics`)
	if _, err := conn.Exec(ctx, `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	if selErr != nil {
		t.Errorf("the role cannot read the table it was granted SELECT on: %v\n%s", selErr, rendered)
	}
}

// TestDumpCapturesPublicRevokeOnRoutine pins that the dump records what was
// taken away, not only what was added.
//
// Every function is created with EXECUTE granted to PUBLIC. A GRANT-only dump
// therefore loses every hardening REVOKE, and the direction of that loss is
// ESCALATION: a SECURITY DEFINER function that returns secrets becomes callable
// by every role again on a rebuilt database.
func TestDumpCapturesPublicRevokeOnRoutine(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	if _, err := conn.Exec(ctx, `
		CREATE ROLE auth_service;
		CREATE SCHEMA auth;
		CREATE FUNCTION auth.user_secret(p_user text) RETURNS text
			LANGUAGE sql SECURITY DEFINER AS $$ SELECT 'hunter2'::text $$;
		REVOKE EXECUTE ON FUNCTION auth.user_secret(text) FROM PUBLIC;
		GRANT EXECUTE ON FUNCTION auth.user_secret(text) TO auth_service;
	`); err != nil {
		t.Fatal(err)
	}

	rendered := renderAllPermissions(t, ctx, NewDumper(conn), "auth")

	// Rebuild the function from its logic/ file: the fresh object carries
	// PostgreSQL's default ACL, which grants EXECUTE to PUBLIC.
	if _, err := conn.Exec(ctx, `
		DROP FUNCTION auth.user_secret(text);
		CREATE FUNCTION auth.user_secret(p_user text) RETURNS text
			LANGUAGE sql SECURITY DEFINER AS $$ SELECT 'hunter2'::text $$;
	`); err != nil {
		t.Fatal(err)
	}
	var publicBefore bool
	if err := conn.QueryRow(ctx,
		`SELECT has_function_privilege('public', 'auth.user_secret(text)', 'EXECUTE')`,
	).Scan(&publicBefore); err != nil {
		t.Fatal(err)
	}
	if !publicBefore {
		t.Fatal("premise broken: a freshly created function should be EXECUTE-able by PUBLIC")
	}

	if _, err := conn.Exec(ctx, rendered); err != nil {
		t.Fatalf("rendered permissions failed to replay: %v\n%s", err, rendered)
	}

	var publicAfter, serviceAfter bool
	if err := conn.QueryRow(ctx, `
		SELECT has_function_privilege('public', 'auth.user_secret(text)', 'EXECUTE'),
		       has_function_privilege('auth_service', 'auth.user_secret(text)', 'EXECUTE')
	`).Scan(&publicAfter, &serviceAfter); err != nil {
		t.Fatal(err)
	}
	if publicAfter {
		t.Errorf("PUBLIC can execute a SECURITY DEFINER function the live database revoked it from:\n%s", rendered)
	}
	if !serviceAfter {
		t.Errorf("the explicit grant to auth_service was lost:\n%s", rendered)
	}
}

// Column-level privileges live in pg_attribute.attacl and leave no trace in
// pg_class.relacl, so a role granted SELECT on three columns had no
// relation-level entry at all and vanished from the dump entirely.
func TestDumpCapturesColumnGrants(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	if _, err := conn.Exec(ctx, `
		CREATE ROLE analyst;
		CREATE TABLE public.people (id BIGINT PRIMARY KEY, name TEXT, ssn TEXT);
		GRANT SELECT (id, name) ON public.people TO analyst;
	`); err != nil {
		t.Fatal(err)
	}

	rendered := renderAllPermissions(t, ctx, NewDumper(conn), "public")

	if _, err := conn.Exec(ctx, `REVOKE ALL (id, name, ssn) ON public.people FROM analyst`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, rendered); err != nil {
		t.Fatalf("rendered permissions failed to replay: %v\n%s", err, rendered)
	}

	var id, name, ssn bool
	if err := conn.QueryRow(ctx, `
		SELECT has_column_privilege('analyst', 'public.people', 'id',   'SELECT'),
		       has_column_privilege('analyst', 'public.people', 'name', 'SELECT'),
		       has_column_privilege('analyst', 'public.people', 'ssn',  'SELECT')
	`).Scan(&id, &name, &ssn); err != nil {
		t.Fatal(err)
	}
	if !id || !name {
		t.Errorf("column grants were lost (id=%v name=%v):\n%s", id, name, rendered)
	}
	if ssn {
		t.Errorf("the replay widened the grant to a column that never had it:\n%s", rendered)
	}
}

// A sequence carries its own privileges, and relkind 'S' was excluded. Without
// USAGE on the sequence behind a SERIAL column, every INSERT by the application
// role fails on a rebuilt database even though the table grant is intact.
func TestDumpCapturesSequenceGrants(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	if _, err := conn.Exec(ctx, `
		CREATE ROLE writer;
		CREATE TABLE public.events (id BIGSERIAL PRIMARY KEY, note TEXT);
		GRANT INSERT ON public.events TO writer;
		GRANT USAGE ON SEQUENCE public.events_id_seq TO writer;
	`); err != nil {
		t.Fatal(err)
	}

	rendered := renderAllPermissions(t, ctx, NewDumper(conn), "public")

	if _, err := conn.Exec(ctx, `
		REVOKE ALL ON public.events FROM writer;
		REVOKE ALL ON SEQUENCE public.events_id_seq FROM writer;
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, rendered); err != nil {
		t.Fatalf("rendered permissions failed to replay: %v\n%s", err, rendered)
	}

	var usage bool
	if err := conn.QueryRow(ctx,
		`SELECT has_sequence_privilege('writer', 'public.events_id_seq', 'USAGE')`,
	).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if !usage {
		t.Errorf("the sequence grant was lost, so the role cannot INSERT:\n%s", rendered)
	}
}

// WITH GRANT OPTION is a separate bit in the ACL. Dropping it turns a role that
// could delegate access into one that cannot, and every downstream grant that
// depended on the delegation fails the next time it is needed.
func TestDumpCapturesGrantOption(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	if _, err := conn.Exec(ctx, `
		CREATE ROLE broker;
		CREATE TABLE public.ledger (id BIGINT PRIMARY KEY);
		GRANT SELECT ON public.ledger TO broker WITH GRANT OPTION;
		GRANT INSERT ON public.ledger TO broker;
	`); err != nil {
		t.Fatal(err)
	}

	rendered := renderAllPermissions(t, ctx, NewDumper(conn), "public")

	if _, err := conn.Exec(ctx, `REVOKE ALL ON public.ledger FROM broker`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, rendered); err != nil {
		t.Fatalf("rendered permissions failed to replay: %v\n%s", err, rendered)
	}

	var selectGrantable, insertGrantable bool
	if err := conn.QueryRow(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM aclexplode((SELECT relacl FROM pg_class WHERE oid = 'public.ledger'::regclass)) a
			        WHERE pg_get_userbyid(a.grantee) = 'broker' AND a.privilege_type = 'SELECT' AND a.is_grantable),
			EXISTS (SELECT 1 FROM aclexplode((SELECT relacl FROM pg_class WHERE oid = 'public.ledger'::regclass)) a
			        WHERE pg_get_userbyid(a.grantee) = 'broker' AND a.privilege_type = 'INSERT' AND a.is_grantable)
	`).Scan(&selectGrantable, &insertGrantable); err != nil {
		t.Fatal(err)
	}
	if !selectGrantable {
		t.Errorf("WITH GRANT OPTION was lost on SELECT:\n%s", rendered)
	}
	if insertGrantable {
		t.Errorf("WITH GRANT OPTION was widened to INSERT, which never had it:\n%s", rendered)
	}
}

// aclexplode yields one row per (grantor, grantee, privilege). The same
// privilege granted by two grantors is one privilege; without DISTINCT it
// produced the same GRANT line twice.
func TestDumpDoesNotDuplicateGrantsFromMultipleGrantors(t *testing.T) {
	ctx := context.Background()
	conn, cleanup := testDB(t)
	defer cleanup()

	if _, err := conn.Exec(ctx, `
		CREATE ROLE owner_role;
		CREATE ROLE middle_role;
		CREATE ROLE consumer_role;
		CREATE TABLE public.shared (id BIGINT PRIMARY KEY);
		ALTER TABLE public.shared OWNER TO owner_role;
		GRANT owner_role TO CURRENT_USER;
		SET ROLE owner_role;
		GRANT SELECT ON public.shared TO middle_role WITH GRANT OPTION;
		GRANT SELECT ON public.shared TO consumer_role;
		RESET ROLE;
		GRANT middle_role TO CURRENT_USER;
		SET ROLE middle_role;
		GRANT SELECT ON public.shared TO consumer_role;
		RESET ROLE;
	`); err != nil {
		t.Fatal(err)
	}

	// Premise: two grantors really do produce two ACL entries --
	// consumer_role=r/owner_role and consumer_role=r/middle_role.
	var aclRows int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM aclexplode((SELECT relacl FROM pg_class WHERE oid = 'public.shared'::regclass)) a
		WHERE pg_get_userbyid(a.grantee) = 'consumer_role' AND a.privilege_type = 'SELECT'
	`).Scan(&aclRows); err != nil {
		t.Fatal(err)
	}
	if aclRows < 2 {
		t.Skipf("this server collapsed the two grantors into %d ACL entries", aclRows)
	}

	grants := mustListGrants(t, ctx, NewDumper(conn), "public")
	rendered := RenderGrants(grants, "public")

	// The duplicate lands inside one statement -- "GRANT SELECT, SELECT ON ..." --
	// because the render groups by grantee and target.
	if strings.Contains(rendered, "SELECT, SELECT") {
		t.Errorf("the same privilege from two grantors was emitted twice:\n%s", rendered)
	}
	if want := "GRANT SELECT ON TABLE \"public\".\"shared\" TO \"consumer_role\";"; !strings.Contains(rendered, want) {
		t.Errorf("expected exactly %q in:\n%s", want, rendered)
	}
	// While we are here: middle_role's SELECT is grantable and must say so.
	if want := "TO \"middle_role\" WITH GRANT OPTION;"; !strings.Contains(rendered, want) {
		t.Errorf("expected %q in:\n%s", want, rendered)
	}
}

func mustListGrants(t *testing.T, ctx context.Context, d *Dumper, schema string) []Grant {
	t.Helper()
	g, err := d.ListGrants(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
