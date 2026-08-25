package schema

import (
	"strings"
	"testing"
)

func TestFindSequenceFixups(t *testing.T) {
	ddl := `CREATE TABLE "materialized"."_proclock_snapshots" (
	"id" bigint DEFAULT nextval('materialized._proclock_snapshots_id_seq'::regclass) NOT NULL,
	"captured_at" timestamptz NOT NULL,
	"note" text
)`
	got := findSequenceFixups(ddl)
	if len(got) != 1 {
		t.Fatalf("expected 1 fixup, got %d: %+v", len(got), got)
	}
	if got[0].Sequence != `"materialized"."_proclock_snapshots_id_seq"` {
		t.Errorf("sequence = %q", got[0].Sequence)
	}
	if got[0].Table != `"materialized"."_proclock_snapshots"` {
		t.Errorf("table = %q", got[0].Table)
	}
	if got[0].Column != `"id"` {
		t.Errorf("column = %q", got[0].Column)
	}

	before, after := sequenceStatements(got)
	if len(before) != 1 || !strings.Contains(before[0].DDL, "CREATE SEQUENCE IF NOT EXISTS") {
		t.Errorf("before = %+v", before)
	}
	if len(after) != 1 || !strings.Contains(after[0].DDL, "OWNED BY") {
		t.Errorf("after = %+v", after)
	}
}

func TestFindSequenceFixupsIgnoresOtherStatements(t *testing.T) {
	for _, ddl := range []string{
		`ALTER TABLE "public"."widget" ADD COLUMN "label" text`,
		`CREATE INDEX idx_widget_label ON public.widget (label)`,
		`CREATE TABLE "public"."plain" ("id" bigint NOT NULL)`,
		`CREATE TABLE "public"."defaulted" ("n" integer DEFAULT 7)`,
	} {
		if got := findSequenceFixups(ddl); got != nil {
			t.Errorf("expected no fixups for %q, got %+v", ddl, got)
		}
	}
}

func TestFindSequenceFixupsMultipleColumns(t *testing.T) {
	ddl := `CREATE TABLE "public"."two" (
	"a" bigint DEFAULT nextval('public.two_a_seq'::regclass) NOT NULL,
	"b" integer DEFAULT nextval('public.two_b_seq'::regclass) NOT NULL
)`
	got := findSequenceFixups(ddl)
	if len(got) != 2 {
		t.Fatalf("expected 2 fixups, got %d: %+v", len(got), got)
	}
	before, after := sequenceStatements(got)
	if len(before) != 2 || len(after) != 2 {
		t.Errorf("before=%d after=%d, want 2 and 2", len(before), len(after))
	}
}

// A sequence referenced twice must only be created once.
func TestSequenceStatementsDedupesCreate(t *testing.T) {
	fixups := []sequenceFixup{
		{Sequence: `"public"."s"`, Table: `"public"."t"`, Column: `"a"`},
		{Sequence: `"public"."s"`, Table: `"public"."t"`, Column: `"b"`},
	}
	before, after := sequenceStatements(fixups)
	if len(before) != 1 {
		t.Errorf("expected 1 CREATE SEQUENCE, got %d", len(before))
	}
	if len(after) != 2 {
		t.Errorf("expected 2 OWNED BY, got %d", len(after))
	}
}

func TestQuoteSequenceRef(t *testing.T) {
	tests := []struct{ in, want string }{
		{"public.widget_id_seq", `"public"."widget_id_seq"`},
		{"widget_id_seq", `"widget_id_seq"`},
		{`public."odd name_seq"`, `public."odd name_seq"`},
	}
	for _, tt := range tests {
		if got := quoteSequenceRef(tt.in); got != tt.want {
			t.Errorf("quoteSequenceRef(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
