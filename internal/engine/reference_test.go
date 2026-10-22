package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The shape reference/ exists for, taken from a real seed: a surrogate id
// nobody matches on, a natural key with a unique constraint, audit columns.
const refMappingTable = `CREATE TABLE public.vendor_mapping (
    id         TEXT PRIMARY KEY,
    match_type TEXT NOT NULL,
    keyword    TEXT NOT NULL,
    carrier    TEXT,
    fee        NUMERIC(10,2),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT vendor_mapping_uk UNIQUE (match_type, keyword)
);`

func refMappingFile(options, rows string) []byte {
	return []byte("-- m8:reference public.vendor_mapping key=match_type,keyword touch=updated_at " + options + `
SELECT * FROM (VALUES
` + rows + `
) AS v(id, match_type, keyword, carrier, fee);
`)
}

const refMappingRows = `    ('vm_1', 'VENDOR_CONTAINS', 'landmark', 'LANDMARK', 1.234),
    ('vm_2', 'VENDOR_CONTAINS', 'funko', 'DHL_ECOM', NULL),
    ('vm_3', 'MERCHANT_EXACT', 'seko', 'SEKO_US', 2)`

func refQuery(t *testing.T, conn *pgx.Conn, sql string) string {
	t.Helper()
	var out string
	if err := conn.QueryRow(context.Background(), sql).Scan(&out); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}

func TestReferenceConvergesEveryApply(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	dir := setupMigrationsDir(t)
	mustMkdirAll(t, filepath.Join(dir, "reference"), 0755)
	mustWriteFile(t, filepath.Join(dir, "schema", "public", "vendor_mapping.sql"), []byte(refMappingTable), 0644)
	refFile := filepath.Join(dir, "reference", "vendor_mapping.sql")
	mustWriteFile(t, refFile, refMappingFile("", refMappingRows), 0644)

	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	// Before schema/ has created the table, plan cannot diff it -- but must
	// still say the file has work to do.
	plan, err := eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatPlanOutput(plan); !strings.Contains(out, "diffed at apply — schema/ changes public.vendor_mapping in this release") {
		t.Errorf("plan on an empty database:\n%s", out)
	}

	// One apply creates the table and fills it.
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if got := refQuery(t, conn, "SELECT string_agg(id || '=' || coalesce(carrier, '-') || '/' || coalesce(fee::text, '-'), ',' ORDER BY id) FROM vendor_mapping"); got != "vm_1=LANDMARK/1.23,vm_2=DHL_ECOM/-,vm_3=SEKO_US/2.00" {
		t.Errorf("rows after the first apply: %s", got)
	}

	// Converged: plan is clean (1.234 rounds to the stored 1.23, so it is not
	// a perpetual change), and an apply rewrites nothing -- xmin is the
	// transaction that last wrote each row.
	plan, err = eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatPlanOutput(plan); !strings.Contains(out, "No pending migrations") {
		t.Errorf("plan after converging:\n%s", out)
	}
	xmin := refQuery(t, conn, "SELECT string_agg(xmin::text, ',' ORDER BY id) FROM vendor_mapping")
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if got := refQuery(t, conn, "SELECT string_agg(xmin::text, ',' ORDER BY id) FROM vendor_mapping"); got != xmin {
		t.Errorf("an apply with nothing to change rewrote rows: xmin %s -> %s", xmin, got)
	}

	// Drift: a hand edit and a row the file does not list. Plan names the
	// change; apply reverts it, touches only that row, and keeps the
	// unlisted row because the file does not prune.
	mustExec(t, conn, `UPDATE vendor_mapping SET carrier = 'HAND_EDIT', fee = 9, updated_at = '2000-01-01' WHERE id = 'vm_2';
		UPDATE vendor_mapping SET updated_at = '2000-01-01' WHERE id = 'vm_3';
		INSERT INTO vendor_mapping (id, match_type, keyword, carrier) VALUES ('app_1', 'VENDOR_CONTAINS', 'app', 'APP')`)
	plan, err = eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := FormatPlanOutput(plan)
	if !strings.Contains(out, "reference/vendor_mapping.sql (reference vendor_mapping: 0 insert, 1 update, 0 delete)") ||
		!strings.Contains(out, "~ ('VENDOR_CONTAINS', 'funko') carrier: 'HAND_EDIT' → 'DHL_ECOM', fee: '9.00' → NULL") {
		t.Errorf("plan after drift:\n%s", out)
	}
	if !hasPendingReference(plan) {
		t.Error("drift should count as pending")
	}
	// An application holds an unchanged row. The apply changes a different
	// one, so it must not wait on that lock: ON CONFLICT DO UPDATE locks every
	// row it is handed, and the upsert is handed only the rows that changed.
	app, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close(ctx) }()
	appTx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appTx.Exec(ctx, "SELECT 1 FROM vendor_mapping WHERE id = 'vm_1' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatalf("apply waited on a row it did not change: %v", err)
	}
	_ = appTx.Rollback(ctx)
	if got := refQuery(t, conn, "SELECT carrier || ',' || (updated_at > '2001-01-01') FROM vendor_mapping WHERE id = 'vm_2'"); got != "DHL_ECOM,true" {
		t.Errorf("drifted row after apply: %s", got)
	}
	if got := refQuery(t, conn, "SELECT (updated_at = '2000-01-01')::text FROM vendor_mapping WHERE id = 'vm_3'"); got != "true" {
		t.Error("touch= was applied to a row m8 did not change")
	}
	if got := refQuery(t, conn, "SELECT count(*)::text FROM vendor_mapping WHERE id = 'app_1'"); got != "1" {
		t.Error("a row the file does not list was deleted without prune")
	}

	// prune deletes it; a changed declaration inserts and updates.
	mustWriteFile(t, refFile, refMappingFile("prune", refMappingRows+`,
    ('vm_4', 'VENDOR_CONTAINS', 'royal mail', 'ROYAL_MAIL', NULL)`), 0644)
	plan, err = eng.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out = FormatPlanOutput(plan)
	if !strings.Contains(out, "1 insert, 0 update, 1 delete") ||
		!strings.Contains(out, "+ ('VENDOR_CONTAINS', 'royal mail') id='vm_4' carrier='ROYAL_MAIL' fee=NULL") ||
		!strings.Contains(out, "- ('VENDOR_CONTAINS', 'app')") {
		t.Errorf("plan with prune:\n%s", out)
	}
	res, err := eng.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatApplyOutput(res); !strings.Contains(out, "✓ reference/vendor_mapping.sql (1 inserted, 0 updated, 1 deleted)") {
		t.Errorf("apply output:\n%s", out)
	}
	if got := refQuery(t, conn, "SELECT string_agg(id, ',' ORDER BY id) FROM vendor_mapping"); got != "vm_1,vm_2,vm_3,vm_4" {
		t.Errorf("rows after prune: %s", got)
	}
}

// baseline has no history to record for reference/ -- _m8.history's type
// CHECK would refuse the row -- and sync converges it like schema/.
func TestReferenceBaselineAndSync(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	mustExec(t, conn, "CREATE TABLE carrier (code TEXT PRIMARY KEY, name TEXT)")
	dir := setupMigrationsDir(t)
	mustMkdirAll(t, filepath.Join(dir, "reference"), 0755)
	mustWriteFile(t, filepath.Join(dir, "reference", "carrier.sql"),
		[]byte("-- m8:reference carrier\nSELECT * FROM (VALUES ('UPS', 'United Parcel Service')) v(code, name)"), 0644)
	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	if err := eng.Baseline(ctx, "", true); err != nil {
		t.Fatal(err)
	}
	if got := refQuery(t, conn, "SELECT count(*)::text FROM carrier"); got != "0" {
		t.Error("baseline wrote reference rows")
	}
	if _, err := eng.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := eng.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatStatusOutput(st); !strings.Contains(out, "  = reference/carrier.sql") || strings.Contains(out, "Database is up to date") {
		t.Errorf("status claims what it cannot check:\n%s", out)
	}
	if got := refQuery(t, conn, "SELECT name FROM carrier"); got != "United Parcel Service" {
		t.Errorf("after sync: %s", got)
	}
}

// A malformed reference/ file is caught before ops/ runs, so it cannot fail a
// release halfway through.
func TestReferenceParseErrorStopsBeforeOps(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()

	dir := setupMigrationsDir(t)
	mustMkdirAll(t, filepath.Join(dir, "reference"), 0755)
	mustWriteFile(t, filepath.Join(dir, "ops", "20260101_000000__mark.sql"), []byte("CREATE TABLE ops_ran (id INT)"), 0644)
	mustWriteFile(t, filepath.Join(dir, "reference", "carrier.sql"), []byte("-- m8:reference carrier keys=code\nSELECT 1"), 0644)
	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}
	if _, err := eng.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "unknown -- m8:reference option") {
		t.Fatalf("got %v", err)
	}
	if got := refQuery(t, conn, "SELECT (to_regclass('ops_ran') IS NULL)::text"); got != "true" {
		t.Error("ops/ ran before the malformed reference/ file was refused")
	}
}

// Declarations that need more than the simplest write: each was a failing
// apply, or a plan with a side effect, before the write path accounted for it.
func TestReferenceWriteShapes(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	apply := func(t *testing.T, files map[string]string) error {
		t.Helper()
		dir := t.TempDir()
		mustMkdirAll(t, filepath.Join(dir, "reference"), 0755)
		for name, content := range files {
			mustWriteFile(t, filepath.Join(dir, "reference", name), []byte(content), 0644)
		}
		eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
		if differ != nil {
			defer func() { _ = differ.Close() }()
		}
		_, err := eng.Apply(ctx)
		return err
	}

	t.Run("plan cannot move a sequence", func(t *testing.T) {
		mustExec(t, conn, "CREATE SEQUENCE seq; CREATE TABLE seqd (code TEXT PRIMARY KEY)")
		dir := t.TempDir()
		mustMkdirAll(t, filepath.Join(dir, "reference"), 0755)
		mustWriteFile(t, filepath.Join(dir, "reference", "seqd.sql"),
			[]byte("-- m8:reference seqd\nSELECT nextval('seq')::text AS code"), 0644)
		eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
		if differ != nil {
			defer func() { _ = differ.Close() }()
		}
		if _, err := eng.Plan(ctx); err == nil || !strings.Contains(err.Error(), "read-only transaction") {
			t.Errorf("plan: got %v", err)
		}
		if got := refQuery(t, conn, "SELECT is_called::text FROM seq"); got != "false" {
			t.Error("plan advanced a sequence")
		}
	})

	t.Run("plan refuses a column schema/ does not add", func(t *testing.T) {
		mustExec(t, conn, "CREATE TABLE typo (code TEXT PRIMARY KEY, name TEXT)")
		dir := t.TempDir()
		mustMkdirAll(t, filepath.Join(dir, "reference"), 0755)
		mustWriteFile(t, filepath.Join(dir, "reference", "typo.sql"),
			[]byte("-- m8:reference typo\nSELECT * FROM (VALUES ('a', 'b')) v(code, nmae)"), 0644)
		eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
		if differ != nil {
			defer func() { _ = differ.Close() }()
		}
		if _, err := eng.Plan(ctx); err == nil || !strings.Contains(err.Error(), "column nmae does not exist on typo") {
			t.Errorf("plan: got %v", err)
		}
	})

	t.Run("declared ids advance the sequence, never backwards", func(t *testing.T) {
		mustExec(t, conn, `CREATE TABLE serials (id SERIAL PRIMARY KEY, code TEXT);
			CREATE TABLE idents (id INT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, code TEXT)`)
		files := func(rows string) map[string]string {
			return map[string]string{
				"serials.sql": "-- m8:reference serials\nSELECT * FROM (VALUES " + rows + ") v(id, code)",
				"idents.sql":  "-- m8:reference idents\nSELECT * FROM (VALUES " + rows + ") v(id, code)",
			}
		}
		if err := apply(t, files("(1, 'UPS'), (2, 'FEDEX')")); err != nil {
			t.Fatal(err)
		}
		mustExec(t, conn, "INSERT INTO serials (code) VALUES ('app'); INSERT INTO idents (code) VALUES ('app')")
		if got := refQuery(t, conn, "SELECT (SELECT id FROM serials WHERE code = 'app') || ',' || (SELECT id FROM idents WHERE code = 'app')"); got != "3,3" {
			t.Errorf("application inserts got ids %s, want 3,3", got)
		}
		mustExec(t, conn, "SELECT setval('serials_id_seq', 100)")
		if err := apply(t, files("(1, 'UPS'), (2, 'FEDEX'), (5, 'DHL')")); err != nil {
			t.Fatal(err)
		}
		if got := refQuery(t, conn, "SELECT nextval('serials_id_seq')::text"); got != "101" {
			t.Errorf("sequence moved backwards: next value %s", got)
		}
	})

	t.Run("types without an equality operator converge", func(t *testing.T) {
		mustExec(t, conn, "CREATE TABLE shapes (code TEXT PRIMARY KEY, meta JSON, loc POINT, doc XML)")
		file := map[string]string{"shapes.sql": `-- m8:reference shapes
SELECT * FROM (VALUES ('a', '{"x": 1, "y": [1, 2]}', '(1,2)', '<a>1</a>')) v(code, meta, loc, doc)`}
		if err := apply(t, file); err != nil {
			t.Fatal(err)
		}
		xmin := refQuery(t, conn, "SELECT xmin::text FROM shapes")
		if err := apply(t, file); err != nil {
			t.Fatal(err)
		}
		if got := refQuery(t, conn, "SELECT xmin::text FROM shapes"); got != xmin {
			t.Error("an unchanged json/point/xml row was rewritten")
		}
	})

	t.Run("plan diffs a table when schema/ only adds a same-named column elsewhere", func(t *testing.T) {
		mustExec(t, conn, `CREATE TABLE courier (code TEXT PRIMARY KEY, name TEXT);
			INSERT INTO courier VALUES ('UPS', 'hand edit');
			CREATE TABLE parcel (id INT PRIMARY KEY)`)
		dir := t.TempDir()
		for _, sub := range []string{"reference", "schema/public"} {
			mustMkdirAll(t, filepath.Join(dir, sub), 0755)
		}
		mustWriteFile(t, filepath.Join(dir, "schema", "public", "courier.sql"), []byte("CREATE TABLE courier (code TEXT PRIMARY KEY, name TEXT);"), 0644)
		mustWriteFile(t, filepath.Join(dir, "schema", "public", "parcel.sql"), []byte("CREATE TABLE parcel (id INT PRIMARY KEY, courier TEXT);"), 0644)
		mustWriteFile(t, filepath.Join(dir, "reference", "courier.sql"),
			[]byte("-- m8:reference courier\nSELECT * FROM (VALUES ('UPS', 'United Parcel Service')) v(code, name)"), 0644)
		eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
		if differ != nil {
			defer func() { _ = differ.Close() }()
		}
		plan, err := eng.Plan(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := FormatPlanOutput(plan)
		if !strings.Contains(out, `ADD COLUMN "courier"`) || !strings.Contains(out, "~ ('UPS') name: 'hand edit' → 'United Parcel Service'") {
			t.Errorf("plan:\n%s", out)
		}
	})

	t.Run("char(n) keeps its length", func(t *testing.T) {
		mustExec(t, conn, "CREATE TABLE iso (code CHAR(2) PRIMARY KEY, iso3 CHAR(3), bits BIT(3))")
		if err := apply(t, map[string]string{"iso.sql": "-- m8:reference iso\nSELECT * FROM (VALUES ('US', 'USA', '101'), ('UK', 'GBR', '011')) v(code, iso3, bits)"}); err != nil {
			t.Fatal(err)
		}
		if got := refQuery(t, conn, "SELECT string_agg(code || iso3 || bits::text, ',' ORDER BY code) FROM iso"); got != "UKGBR011,USUSA101" {
			t.Errorf("rows: %s", got)
		}
	})

	t.Run("an undeclared NOT NULL column does not block an update", func(t *testing.T) {
		mustExec(t, conn, `CREATE TABLE owned (id INT PRIMARY KEY, name TEXT, owner TEXT NOT NULL);
			INSERT INTO owned VALUES (1, 'a', 'ops')`)
		if err := apply(t, map[string]string{"owned.sql": "-- m8:reference owned\nSELECT * FROM (VALUES (1, 'b')) v(id, name)"}); err != nil {
			t.Fatal(err)
		}
		if got := refQuery(t, conn, "SELECT name || '/' || owner FROM owned"); got != "b/ops" {
			t.Errorf("row: %s", got)
		}
	})

	t.Run("a primary key's INCLUDE columns are not key columns", func(t *testing.T) {
		mustExec(t, conn, "CREATE TABLE incl (id INT, name TEXT, PRIMARY KEY (id) INCLUDE (name))")
		if err := apply(t, map[string]string{"incl.sql": "-- m8:reference incl\nSELECT * FROM (VALUES (1, 'a')) v(id, name)"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("re-keying a row that keeps its other unique values", func(t *testing.T) {
		mustExec(t, conn, `CREATE TABLE rekey (id TEXT NOT NULL UNIQUE, keyword TEXT PRIMARY KEY);
			INSERT INTO rekey VALUES ('vm_1', 'dhl ecomm au')`)
		if err := apply(t, map[string]string{"rekey.sql": "-- m8:reference rekey prune\nSELECT * FROM (VALUES ('vm_1', 'dhl ecom au')) v(id, keyword)"}); err != nil {
			t.Fatal(err)
		}
		if got := refQuery(t, conn, "SELECT string_agg(keyword, ',') FROM rekey"); got != "dhl ecom au" {
			t.Errorf("rows: %s", got)
		}
	})

	t.Run("a child moves off a parent that is pruned", func(t *testing.T) {
		mustExec(t, conn, `CREATE TABLE nation (code TEXT PRIMARY KEY);
			CREATE TABLE province (code TEXT PRIMARY KEY, nation TEXT NOT NULL REFERENCES nation);
			INSERT INTO nation VALUES ('US'), ('CA');
			INSERT INTO province VALUES ('R', 'CA')`)
		if err := apply(t, map[string]string{
			"a_province.sql": "-- m8:reference province prune\nSELECT * FROM (VALUES ('R', 'US')) v(code, nation)",
			"b_nation.sql":   "-- m8:reference nation prune\nSELECT * FROM (VALUES ('US')) v(code)",
		}); err != nil {
			t.Fatal(err)
		}
		if got := refQuery(t, conn, "SELECT (SELECT string_agg(code, ',') FROM nation) || '|' || (SELECT nation FROM province)"); got != "US|US" {
			t.Errorf("rows: %s", got)
		}
	})
}

func hasPendingReference(r *ApplyResult) bool {
	for _, d := range r.Reference {
		if !d.Empty() {
			return true
		}
	}
	return false
}

// Every refusal is a declaration m8 cannot apply exactly as written. The last
// file in each case is the bad one; a.sql before it is valid, so each case
// also proves the phase is one transaction -- a.sql's rows must not land.
func TestReferenceRefusals(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	mustExec(t, conn, `CREATE TABLE witness (code TEXT PRIMARY KEY, name TEXT);
		CREATE TABLE ok (code TEXT PRIMARY KEY, name TEXT);
		CREATE TABLE nokey (code TEXT, name TEXT);
		CREATE TABLE partial (code TEXT, name TEXT);
		CREATE UNIQUE INDEX ON partial (code) WHERE name IS NOT NULL;
		CREATE TABLE short (code TEXT PRIMARY KEY, name VARCHAR(3));
		CREATE TABLE gen (code TEXT PRIMARY KEY, name TEXT, upper_name TEXT GENERATED ALWAYS AS (upper(name)) STORED);
		CREATE TABLE typed (code INT PRIMARY KEY);
		CREATE TABLE cshort (code CHAR(2) PRIMARY KEY);
		CREATE TABLE ident (id INT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name TEXT UNIQUE);
		CREATE TABLE parent (code TEXT PRIMARY KEY);
		CREATE TABLE child (code TEXT PRIMARY KEY, parent TEXT REFERENCES parent ON DELETE CASCADE);
		CREATE VIEW v AS SELECT 1 AS code`)

	for _, tc := range []struct{ name, file, want string }{
		{"no primary key", "-- m8:reference nokey\nSELECT * FROM (VALUES ('a', 'b')) v(code, name)", "nokey has no primary key; name the key columns with key="},
		{"key without a unique index", "-- m8:reference nokey key=code\nSELECT * FROM (VALUES ('a', 'b')) v(code, name)", "no unique index on nokey covers exactly the key (code)"},
		{"partial unique index", "-- m8:reference partial key=code\nSELECT * FROM (VALUES ('a', 'b')) v(code, name)", "no unique index on partial"},
		{"unique index on more than the key", "-- m8:reference ok key=code,name\nSELECT * FROM (VALUES ('a', 'b')) v(code, name)", "no unique index on ok covers exactly the key (code, name)"},
		{"key not declared", "-- m8:reference ok\nSELECT * FROM (VALUES ('b')) v(name)", "key column code is not one of the query's columns"},
		{"unknown column", "-- m8:reference ok\nSELECT * FROM (VALUES ('a', 'b')) v(code, nmae)", "column nmae does not exist on ok"},
		{"missing table", "-- m8:reference nope\nSELECT 1 AS code", "table nope does not exist"},
		{"not a table", "-- m8:reference v\nSELECT 1 AS code", "v is not a table"},
		{"generated column", "-- m8:reference gen\nSELECT * FROM (VALUES ('a', 'A')) v(code, upper_name)", "column upper_name is generated"},
		{"column twice", "-- m8:reference ok\nSELECT 'a' AS code, 'b' AS name, 'c' AS name", "names column name twice"},
		{"touch is declared", "-- m8:reference ok touch=name\nSELECT * FROM (VALUES ('a', 'b')) v(code, name)", "touch column name is also one of the query's columns"},
		{"touch does not exist", "-- m8:reference ok touch=updated_at\nSELECT * FROM (VALUES ('a', 'b')) v(code, name)", "touch column updated_at does not exist on ok"},
		{"duplicate key", "-- m8:reference ok\nSELECT * FROM (VALUES ('x', 'b'), ('x', 'c'), ('y', 'd')) v(code, name)", "key (code) is declared more than once: ('x')"},
		{"NULL key", "-- m8:reference ok\nSELECT * FROM (VALUES (NULL, 'b')) v(code, name)", "a row declares a NULL key (NULL)"},
		{"too long is refused, not truncated", "-- m8:reference short\nSELECT * FROM (VALUES ('a', 'abcd')) v(code, name)", "value too long for type character varying(3)"},
		{"char(n) too long is refused, not truncated", "-- m8:reference cshort\nSELECT 'USA' AS code", "value too long for type character(2)"},
		{"identity always", "-- m8:reference ident key=name\nSELECT * FROM (VALUES (1, 'a')) v(id, name)", "column id is GENERATED ALWAYS AS IDENTITY"},
		{"reads another reference table", "-- m8:reference ok\nSELECT code, name FROM witness", "the query reads witness, which reference/a.sql declares"},
		{"reads its own table", "-- m8:reference ok\nSELECT code, name FROM ok", "the query reads ok, which reference/z.sql declares"},
		{"value of the wrong type", "-- m8:reference typed\nSELECT 'abc' AS code", "invalid input syntax for type integer"},
		{"prune through a cascade", "-- m8:reference parent prune\nSELECT * FROM (VALUES ('a')) v(code)", "prune would delete through foreign key child_parent_fkey on child (ON DELETE CASCADE)"},
		{"query error", "-- m8:reference ok\nSELECT * FROM missing_table", "the query failed"},
		{"same table twice", "-- m8:reference public.witness\nSELECT 'z' AS code", "z.sql: witness is already declared in reference/a.sql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mustMkdirAll(t, filepath.Join(dir, "reference"), 0755)
			mustWriteFile(t, filepath.Join(dir, "reference", "a.sql"),
				[]byte("-- m8:reference witness\nSELECT * FROM (VALUES ('kept_out', 'x')) v(code, name)"), 0644)
			mustWriteFile(t, filepath.Join(dir, "reference", "z.sql"), []byte(tc.file), 0644)
			eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
			if differ != nil {
				defer func() { _ = differ.Close() }()
			}
			_, err := eng.Apply(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "reference/") {
				t.Errorf("error does not name the file: %v", err)
			}
			if got := refQuery(t, conn, "SELECT count(*)::text FROM witness"); got != "0" {
				t.Errorf("a refused run left %s row(s) from reference/a.sql", got)
			}
		})
	}
}

// Writes between declared tables follow their foreign keys, not their file
// names: the child sorts first here, so file order would insert a row whose
// parent does not exist yet, and prune would delete a parent still referenced.
func TestReferenceForeignKeyOrder(t *testing.T) {
	conn, sqlDB, connStr, cleanup := testDB(t)
	defer cleanup()
	ctx := context.Background()

	mustExec(t, conn, `CREATE TABLE country (code TEXT PRIMARY KEY);
		CREATE TABLE region (code TEXT PRIMARY KEY, country TEXT NOT NULL REFERENCES country)`)
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "reference"), 0755)
	write := func(countries, regions string) {
		mustWriteFile(t, filepath.Join(dir, "reference", "a_region.sql"),
			[]byte("-- m8:reference region prune\nSELECT * FROM (VALUES "+regions+") v(code, country)"), 0644)
		mustWriteFile(t, filepath.Join(dir, "reference", "b_country.sql"),
			[]byte("-- m8:reference country prune\nSELECT * FROM (VALUES "+countries+") v(code)"), 0644)
	}
	eng, differ := newEngine(conn, sqlDB, connStr, dir, false)
	if differ != nil {
		defer func() { _ = differ.Close() }()
	}

	write("('US'), ('CA')", "('US-UT', 'US'), ('CA-ON', 'CA')")
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	write("('US')", "('US-UT', 'US')")
	if _, err := eng.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if got := refQuery(t, conn, "SELECT (SELECT string_agg(code, ',') FROM country) || '|' || (SELECT string_agg(code, ',') FROM region)"); got != "US|US-UT" {
		t.Errorf("rows: %s", got)
	}

	mustExec(t, conn, "ALTER TABLE country ADD COLUMN parent TEXT REFERENCES region")
	if _, err := eng.Apply(ctx); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("a foreign key cycle: got %v", err)
	}
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
