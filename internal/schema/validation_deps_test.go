package schema

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// depDB starts a throwaway PostgreSQL and returns a pool plus its conn string.
func depDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	c, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("m8test"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("testpwd"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	connStr, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = c.Terminate(ctx)
		t.Fatalf("connection string: %v", err)
	}
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		_ = c.Terminate(ctx)
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(); _ = c.Terminate(ctx) })
	return db, connStr
}

// crossSchemaFixture is the shape that breaks plan validation: a target schema
// holding a view that reads another schema's table. It is the reduced form of
// materialized.admin_revenue_orphans, a dbt-owned view over app_admin.contract.
const crossSchemaFixture = `
CREATE SCHEMA dep;
CREATE TABLE dep.contract (id bigint primary key, org text);
CREATE SCHEMA tgt;
CREATE TABLE tgt.rpt (id bigint primary key, note text);
CREATE VIEW tgt.orphans AS
    SELECT r.id, r.note, c.org FROM tgt.rpt r JOIN dep.contract c ON c.id = r.id;`

func desiredWithNewColumn() []string {
	return []string{
		`CREATE SCHEMA IF NOT EXISTS "tgt";`,
		`CREATE TABLE "tgt"."rpt" (id bigint primary key, note text, country text);`,
	}
}

func newDepDiffer(t *testing.T, connStr string) *Differ {
	t.Helper()
	d, err := NewDiffer(context.Background(), connStr, "")
	if err != nil {
		t.Fatalf("NewDiffer: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// The headline: a schema whose view reaches into another schema used to come
// back unvalidated, and --fail-on-unvalidated then refused the plan outright.
// The first real change to such a schema was unshippable.
func TestPlanValidatesDespiteACrossSchemaView(t *testing.T) {
	ctx := context.Background()
	db, connStr := depDB(t)
	if _, err := db.ExecContext(ctx, crossSchemaFixture); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	d := newDepDiffer(t, connStr)

	res, err := d.Diff(ctx, db, "tgt", desiredWithNewColumn(), false)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if res.ValidationSkipped {
		t.Fatalf("plan came back unvalidated: %s", res.ValidationSkippedReason)
	}
	if len(res.Statements) != 1 || !strings.Contains(res.Statements[0].DDL, "ADD COLUMN") {
		t.Fatalf("expected one ADD COLUMN, got %+v", res.Statements)
	}
	// A validated plan that needed an import must say what it imported.
	if len(res.SeededSchemas) != 1 || res.SeededSchemas[0] != "dep" {
		t.Errorf("SeededSchemas = %v, want [dep]", res.SeededSchemas)
	}
}

// Seeding must not become a silent tax on the ordinary case.
func TestNoDependenciesMeansNoSeeding(t *testing.T) {
	ctx := context.Background()
	db, connStr := depDB(t)
	if _, err := db.ExecContext(ctx, `
CREATE SCHEMA tgt;
CREATE TABLE tgt.rpt (id bigint primary key, note text);`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	d := newDepDiffer(t, connStr)

	res, err := d.Diff(ctx, db, "tgt", desiredWithNewColumn(), false)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if res.ValidationSkipped {
		t.Fatalf("a self-contained schema should validate normally: %s", res.ValidationSkippedReason)
	}
	if len(res.SeededSchemas) != 0 {
		t.Errorf("SeededSchemas = %v, want none -- nothing needed importing", res.SeededSchemas)
	}
}

// The seed must be armed ONLY around the validation retry. If it leaked, every
// temp database -- including the ones used to parse desired DDL -- would carry
// objects the desired state never mentions.
func TestSeedIsDisarmedAfterTheRetry(t *testing.T) {
	ctx := context.Background()
	db, connStr := depDB(t)
	if _, err := db.ExecContext(ctx, crossSchemaFixture); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	d := newDepDiffer(t, connStr)

	if _, err := d.Diff(ctx, db, "tgt", desiredWithNewColumn(), false); err != nil {
		t.Fatalf("diff: %v", err)
	}

	d.seedMu.Lock()
	leftArmed := len(d.seedDDL)
	d.seedMu.Unlock()
	if leftArmed != 0 {
		t.Errorf("seed left armed with %d statements after Diff returned", leftArmed)
	}
}

// A dependency too large to import is refused rather than paid for, and the
// plan degrades exactly as it did before this feature existed.
func TestOversizedDependencyDegradesInsteadOfImporting(t *testing.T) {
	ctx := context.Background()
	db, connStr := depDB(t)
	if _, err := db.ExecContext(ctx, crossSchemaFixture); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// Push "dep" past the cap.
	var b strings.Builder
	for i := 0; i < maxSeededRelations+1; i++ {
		b.WriteString("CREATE TABLE dep.filler_")
		b.WriteString(strings.Repeat("x", 1))
		b.WriteString(itoa(i))
		b.WriteString(" (id int);\n")
	}
	if _, err := db.ExecContext(ctx, b.String()); err != nil {
		t.Fatalf("filler: %v", err)
	}
	d := newDepDiffer(t, connStr)

	res, err := d.Diff(ctx, db, "tgt", desiredWithNewColumn(), false)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !res.ValidationSkipped {
		t.Fatal("expected the oversized dependency to be refused, leaving the plan unvalidated")
	}
	if len(res.SeededSchemas) != 0 {
		t.Errorf("SeededSchemas = %v, want none", res.SeededSchemas)
	}
	// The operator must be told m8 tried and why it stopped. This lives in
	// RecoveryNote, NOT appended to ValidationSkippedReason: that string is a
	// multi-line pg-schema-diff error and every caller renders only its first
	// line, so an appended explanation is invisible exactly when it is needed.
	// Found the hard way -- a production plan degraded with the reason silently
	// truncated away, and the cause took a database session to work out.
	if !strings.Contains(res.RecoveryNote, "over the") {
		t.Errorf("RecoveryNote does not explain the refusal: %q", res.RecoveryNote)
	}
	if strings.Contains(res.ValidationSkippedReason, "over the") {
		t.Error("the explanation was folded back into the multi-line reason, where it gets truncated")
	}
	// And the diff itself is unaffected.
	if len(res.Statements) != 1 {
		t.Errorf("expected the ADD COLUMN regardless, got %+v", res.Statements)
	}
}

// dependencySchemas must find the schema a view reads and ignore the noise.
func TestDependencySchemasFindsViewAndForeignKeyTargets(t *testing.T) {
	ctx := context.Background()
	db, _ := depDB(t)
	if _, err := db.ExecContext(ctx, crossSchemaFixture+`
CREATE SCHEMA fk;
CREATE TABLE fk.parent (id bigint primary key);
CREATE TABLE tgt.child (id bigint primary key, parent_id bigint REFERENCES fk.parent(id));
CREATE SCHEMA unrelated;
CREATE TABLE unrelated.nobody (id int);`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	got, err := dependencySchemas(ctx, db, "tgt")
	if err != nil {
		t.Fatalf("dependencySchemas: %v", err)
	}
	want := map[string]bool{"dep": true, "fk": true}
	if len(got) != len(want) {
		t.Fatalf("dependencySchemas = %v, want dep and fk only", got)
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("dependencySchemas returned %q, which nothing in tgt references", s)
		}
	}
}

// itoa avoids pulling strconv in for one call site in a test fixture.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// The production shape this was found by: the target's view reaches into
// app_admin, and app_admin holds a view of its own over a CDC schema. Importing
// app_admin therefore cannot fully succeed -- and must not have to. The target
// only needs app_admin's TABLE.
func TestSeedSkipsObjectsItCannotCreate(t *testing.T) {
	ctx := context.Background()
	db, connStr := depDB(t)
	if _, err := db.ExecContext(ctx, `
CREATE SCHEMA cdc;
CREATE TABLE cdc.usage_record (id bigint primary key, n int);
CREATE SCHEMA dep;
CREATE TABLE dep.contract (id bigint primary key, org text);
-- dep's own view reaches into a third schema. Rebuilding dep in isolation
-- cannot create this, and chasing the closure would mean importing cdc too.
CREATE VIEW dep.usage_summary AS SELECT id, n FROM cdc.usage_record;
CREATE SCHEMA tgt;
CREATE TABLE tgt.rpt (id bigint primary key, note text);
CREATE VIEW tgt.orphans AS
    SELECT r.id, r.note, c.org FROM tgt.rpt r JOIN dep.contract c ON c.id = r.id;`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	d := newDepDiffer(t, connStr)

	res, err := d.Diff(ctx, db, "tgt", desiredWithNewColumn(), false)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if res.ValidationSkipped {
		t.Fatalf("a transitive dependency should not defeat the recovery: %s\n%s",
			res.ValidationSkippedReason, res.RecoveryNote)
	}
	if len(res.SeededSchemas) != 1 || res.SeededSchemas[0] != "dep" {
		t.Errorf("SeededSchemas = %v, want [dep]", res.SeededSchemas)
	}
	// The skip must be reported, not swallowed.
	if !strings.Contains(res.RecoveryNote, "skipped") {
		t.Errorf("RecoveryNote does not mention the skipped object: %q", res.RecoveryNote)
	}
	if !strings.Contains(res.RecoveryNote, "usage_summary") {
		t.Errorf("RecoveryNote does not name what was skipped: %q", res.RecoveryNote)
	}
}
