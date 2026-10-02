package schema

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

// partitionedFixture is a partitioned table whose children were made by
// something other than the migration files -- in production, pg_partman's
// run_maintenance. The files declare the parent only.
const partitionedFixture = `
CREATE TABLE public.event_log (
    id text NOT NULL,
    created_at timestamptz NOT NULL,
    org text NOT NULL
) PARTITION BY RANGE (created_at);
CREATE INDEX event_log_org_idx ON public.event_log (org);
CREATE TABLE public.event_log_p202609 PARTITION OF public.event_log FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE public.event_log_p202610 PARTITION OF public.event_log FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE public.event_log_default PARTITION OF public.event_log DEFAULT;`

const declaredParent = `
CREATE TABLE public.event_log (
    id text NOT NULL,
    created_at timestamptz NOT NULL,
    org text NOT NULL
) PARTITION BY RANGE (created_at);
CREATE INDEX event_log_org_idx ON public.event_log (org);`

func adoptDiff(t *testing.T, fixture string, desired ...string) *DiffResult {
	t.Helper()
	db, connStr := depDB(t)
	mustExec(t, db, fixture)
	d, err := NewDiffer(context.Background(), connStr, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	res, err := d.Diff(context.Background(), db, "public", desired, false)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	return res
}

func mustExec(t *testing.T, db *sql.DB, sql string) {
	t.Helper()
	if _, err := db.Exec(sql); err != nil {
		t.Fatalf("exec: %v\n%s", err, sql)
	}
}

func joinDDL(r *DiffResult) string {
	var b strings.Builder
	for _, s := range r.Statements {
		b.WriteString(s.DDL + "\n")
	}
	return b.String()
}

// Without adoption this plan does not exist: pg-schema-diff fails with
// "deleting partitions without dropping parent table: not implemented".
func TestUndeclaredPartitionsAreAdopted(t *testing.T) {
	res := adoptDiff(t, partitionedFixture, declaredParent)
	if res.HasChanges {
		t.Fatalf("expected no changes for an unchanged parent, got:\n%s", joinDDL(res))
	}
	if res.ValidationSkipped {
		t.Fatalf("plan was not validated: %s", res.ValidationSkippedReason)
	}
}

// The reason to adopt children rather than hide them: with them visible, a new
// index on the parent is built on every child without blocking writes. Hidden,
// the plan would be a lone CREATE INDEX ... ON ONLY, left invalid and built on
// no existing child.
func TestIndexOnPartitionedParentIsBuiltPerChild(t *testing.T) {
	res := adoptDiff(t, partitionedFixture, declaredParent+`
CREATE INDEX event_log_id_idx ON public.event_log (id);`)
	ddl := joinDDL(res)
	if !strings.Contains(ddl, "CREATE INDEX event_log_id_idx ON ONLY public.event_log") {
		t.Errorf("expected the parent index ON ONLY, got:\n%s", ddl)
	}
	for _, child := range []string{"event_log_p202609", "event_log_p202610", "event_log_default"} {
		if !strings.Contains(ddl, "CREATE INDEX CONCURRENTLY "+child+"_id_idx ON public."+child) {
			t.Errorf("expected a concurrent build on %s, got:\n%s", child, ddl)
		}
		if !strings.Contains(ddl, `ATTACH PARTITION "public"."`+child+`_id_idx"`) {
			t.Errorf("expected %s's index attached to the parent's, got:\n%s", child, ddl)
		}
	}
}

// Partitions do not inherit their parent's grants, so GRANT ... ON ALL TABLES
// or default privileges leave every child with grants of its own. Adopting a
// child without them would read as a change pg-schema-diff refuses outright
// ("privileges on partitions: not implemented"); with them, the plan is clean
// and validated.
func TestAdoptedPartitionsKeepTheirGrants(t *testing.T) {
	res := adoptDiff(t, partitionedFixture+`
CREATE ROLE probe_app;
GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA public TO probe_app;`,
		declaredParent+`
GRANT SELECT, INSERT ON public.event_log TO probe_app;`)
	if res.HasChanges {
		t.Fatalf("expected no changes, got:\n%s", joinDDL(res))
	}
	if res.ValidationSkipped {
		t.Fatalf("plan was not validated: %s", res.ValidationSkippedReason)
	}
}

// A child that logical replication reads from carries REPLICA IDENTITY FULL.
// The adopted child must carry it too, and adopting it must not count as the
// folder declaring replica identity: that would unlock replica-identity
// statements for every undeclared table, here resetting plain_cdc's FULL.
func TestAdoptionKeepsReplicaIdentityAndDoesNotDeclareIt(t *testing.T) {
	res := adoptDiff(t, partitionedFixture+`
ALTER TABLE public.event_log_p202610 REPLICA IDENTITY FULL;
CREATE TABLE public.plain_cdc (id bigint PRIMARY KEY);
ALTER TABLE public.plain_cdc REPLICA IDENTITY FULL;`,
		declaredParent+`
CREATE TABLE public.plain_cdc (id bigint PRIMARY KEY);`)
	if ddl := joinDDL(res); strings.Contains(ddl, "REPLICA IDENTITY") {
		t.Fatalf("plan touches replica identity:\n%s", ddl)
	}
}

// A file that declares a partitioned table as a plain one describes a
// different table; pg-schema-diff plans DROP TABLE and CREATE TABLE. That is
// what converting from Flyway by hand, or an older m8 dump, produces.
func TestPartitionedTableDeclaredPlainIsRefused(t *testing.T) {
	db, connStr := depDB(t)
	mustExec(t, db, partitionedFixture)
	d, err := NewDiffer(context.Background(), connStr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	plain := strings.Replace(declaredParent, ") PARTITION BY RANGE (created_at);", ");", 1)
	_, err = d.Diff(context.Background(), db, "public", []string{plain}, false)
	if err == nil || !strings.Contains(err.Error(), "declared without PARTITION BY") {
		t.Fatalf("expected a refusal naming the missing PARTITION BY, got %v", err)
	}
}

// Under --strict nothing is filtered, so an adopted child that did not carry
// its own attributes would have them reset: REPLICA IDENTITY DEFAULT on a
// partition logical replication reads from, row-level security switched off.
func TestStrictModeLeavesAdoptedChildAttributesAlone(t *testing.T) {
	db, connStr := depDB(t)
	mustExec(t, db, partitionedFixture+`
ALTER TABLE public.event_log_p202610 REPLICA IDENTITY FULL;
ALTER TABLE public.event_log_p202609 ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.event_log_p202609 FORCE ROW LEVEL SECURITY;`)
	d, err := NewDiffer(context.Background(), connStr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	res, err := d.Diff(context.Background(), db, "public", []string{declaredParent}, true)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if res.HasChanges {
		t.Fatalf("strict plan changes adopted children:\n%s", joinDDL(res))
	}
}

// A partition built by hand -- created, indexed under names of its own
// choosing, then attached, which is how pg_partman 5 builds one too -- keeps
// those names. Rebuilt with CREATE TABLE ... PARTITION OF it would get the
// names Postgres generates, and validation could not attach them.
func TestHandBuiltPartitionsKeepTheirIndexNames(t *testing.T) {
	res := adoptDiff(t, `
CREATE TABLE public.event_log (
    id text NOT NULL,
    created_at timestamptz NOT NULL,
    org text NOT NULL,
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE INDEX event_log_org_idx ON public.event_log (org);
CREATE TABLE public.ev_2026_10 (LIKE public.event_log);
ALTER TABLE public.ev_2026_10 ADD CONSTRAINT ev_2026_10_pkey_custom PRIMARY KEY (id, created_at);
CREATE INDEX ev_2026_10_by_org ON public.ev_2026_10 (org);
ALTER TABLE public.event_log ATTACH PARTITION public.ev_2026_10 FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');`,
		`CREATE TABLE public.event_log (
    id text NOT NULL,
    created_at timestamptz NOT NULL,
    org text NOT NULL,
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE INDEX event_log_org_idx ON public.event_log (org);`)
	if res.HasChanges {
		t.Fatalf("expected no changes, got:\n%s", joinDDL(res))
	}
	if res.ValidationSkipped {
		t.Fatalf("plan was not validated: %s", res.ValidationSkippedReason)
	}
}

// What a partition carries beyond its parent survives --strict: an index of
// its own, and a NOT NULL its parent does not have.
func TestStrictModeLeavesChildOnlyIndexesAndConstraintsAlone(t *testing.T) {
	db, connStr := depDB(t)
	mustExec(t, db, partitionedFixture+`
ALTER TABLE public.event_log ADD COLUMN note text;
ALTER TABLE public.event_log_p202610 ALTER COLUMN note SET NOT NULL;
CREATE INDEX event_log_p202610_note_idx ON public.event_log_p202610 (note);`)
	d, err := NewDiffer(context.Background(), connStr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	parent := strings.Replace(declaredParent, "org text NOT NULL\n)", "org text NOT NULL,\n    note text\n)", 1)
	res, err := d.Diff(context.Background(), db, "public", []string{parent}, true)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if res.HasChanges {
		t.Fatalf("strict plan changes what the child carries:\n%s", joinDDL(res))
	}
}

func diffErr(t *testing.T, fixture string, desired ...string) error {
	t.Helper()
	db, connStr := depDB(t)
	mustExec(t, db, fixture)
	d, err := NewDiffer(context.Background(), connStr, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	_, err = d.Diff(context.Background(), db, "public", desired, false)
	return err
}

// The plain-declaration refusal must not depend on there being a child to
// adopt: an empty partitioned table declared plain is planned DROP and CREATE
// as well.
func TestEmptyPartitionedTableDeclaredPlainIsRefused(t *testing.T) {
	plain := strings.Replace(declaredParent, ") PARTITION BY RANGE (created_at);", ");", 1)
	err := diffErr(t, `CREATE TABLE public.event_log (id text NOT NULL, created_at timestamptz NOT NULL, org text NOT NULL)
		PARTITION BY RANGE (created_at);`, plain)
	if err == nil || !strings.Contains(err.Error(), "declared without PARTITION BY") {
		t.Fatalf("expected a refusal naming the missing PARTITION BY, got %v", err)
	}
}

// What adoption cannot rebuild is refused by name, not left to fail inside
// the diff with "not implemented".
func TestUnadoptablePartitionsAreRefusedByName(t *testing.T) {
	err := diffErr(t, partitionedFixture+`
ALTER TABLE public.event_log_p202610 ADD CONSTRAINT org_not_empty CHECK (org <> '');`, declaredParent)
	if err == nil || !strings.Contains(err.Error(), "event_log_p202610") || !strings.Contains(err.Error(), "CHECK constraint") {
		t.Errorf("expected the CHECK on event_log_p202610 named, got %v", err)
	}

	err = diffErr(t, `
CREATE TABLE public.event_log (id text NOT NULL, created_at timestamptz NOT NULL, org text NOT NULL) PARTITION BY RANGE (created_at);
CREATE TABLE public.event_log_2026 PARTITION OF public.event_log FOR VALUES FROM ('2026-01-01') TO ('2027-01-01') PARTITION BY LIST (org);
CREATE TABLE public.event_log_2026_a PARTITION OF public.event_log_2026 FOR VALUES IN ('a');`,
		`CREATE TABLE public.event_log (id text NOT NULL, created_at timestamptz NOT NULL, org text NOT NULL) PARTITION BY RANGE (created_at);`)
	if err == nil || !strings.Contains(err.Error(), "event_log_2026 is a partition") || !strings.Contains(err.Error(), "itself partitioned") {
		t.Errorf("expected the sub-partitioned event_log_2026 named, got %v", err)
	}
}

func TestDeclaredTables(t *testing.T) {
	got := declaredTables([]string{`
CREATE TABLE public."event-log" (id text) PARTITION BY RANGE (id);
CREATE TABLE "Events" (id text);
CREATE TABLE events (id text);
CREATE TABLE IF NOT EXISTS "public"."say ""hi""" (id text);
CREATE TABLE ev_def PARTITION OF ev DEFAULT PARTITION BY RANGE (id);
CREATE UNLOGGED TABLE Scratch (id text);
-- m8:partman audit_log control=created_at partition_interval='1 month'
/* the audit trail */
CREATE TABLE audit_log (id text, created_at timestamptz) PARTITION BY RANGE (created_at);`})
	want := map[string]bool{
		"event-log": true, "Events": false, "events": false, `say "hi"`: false, "ev_def": true, "scratch": false,
		"audit_log": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("declaredTables = %v, want %v", got, want)
	}
}

func TestRecreatedAdoptedChildIsCaught(t *testing.T) {
	a := adopted{children: map[string]bool{"event_log_p202610": true}}
	if got := a.recreated("public", []string{
		`CREATE INDEX CONCURRENTLY event_log_p202610_id_idx ON public.event_log_p202610 USING btree (id)`,
		`CREATE TABLE "public"."event_log_p2026100" (id text)`,
	}); got != "" {
		t.Errorf("flagged %q, which the plan does not create", got)
	}
	if got := a.recreated("public", []string{`CREATE TABLE "public"."event_log_p202610" (` + "\n\t\"id\" text\n)"}); got != "event_log_p202610" {
		t.Errorf("did not catch the recreated child, got %q", got)
	}
}
