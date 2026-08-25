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
	if !strings.Contains(rendered, "CREATE OR REPLACE VIEW active_users") {
		t.Error("expected CREATE OR REPLACE VIEW in rendered output")
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
	if !strings.Contains(rendered, "GRANT SELECT ON docs TO reader") {
		t.Errorf("expected grant to reader in:\n%s", rendered)
	}
	if !strings.Contains(rendered, "GRANT SELECT ON docs TO PUBLIC") {
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
