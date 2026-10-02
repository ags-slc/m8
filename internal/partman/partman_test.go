package partman

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDirective(t *testing.T) {
	specs, err := Parse([]byte(`
-- m8:partman audit_log control=created_at partition_interval='1 month' premake=4 default_table=true infinite_time_partitions=on retention=none
CREATE TABLE audit_log (id text, created_at timestamptz NOT NULL) PARTITION BY RANGE (created_at);
-- m8:partmanage is someone else's directive
-- m8:no-transaction
`), "public")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	s := specs[0]
	if s.QualifiedName() != "public.audit_log" || s.Control != "created_at" || s.Interval != "1 month" {
		t.Errorf("unexpected spec: %+v", s)
	}
	if s.DefaultTable == nil || !*s.DefaultTable {
		t.Errorf("default_table not parsed: %+v", s.DefaultTable)
	}
	want := map[string]string{"premake": "4", "infinite_time_partitions": "true", "retention": "none"}
	if !reflect.DeepEqual(s.Settings, want) {
		t.Errorf("settings = %v, want %v", s.Settings, want)
	}
}

// A typo must not be a silent no-op: an unrecognised key that m8 ignored would
// read, in review, like configuration it was enforcing.
func TestParseRejects(t *testing.T) {
	for name, line := range map[string]string{
		"unknown key":         `-- m8:partman t control=c partition_interval=1 premak=4`,
		"missing interval":    `-- m8:partman t control=c`,
		"no table":            `-- m8:partman control=c partition_interval=1`,
		"qualified table":     `-- m8:partman public.t control=c partition_interval=1`,
		"duplicate key":       `-- m8:partman t control=c partition_interval=1 premake=1 premake=2`,
		"bad bool":            `-- m8:partman t control=c partition_interval=1 jobmon=maybe`,
		"bad int":             `-- m8:partman t control=c partition_interval=1 premake=-1`,
		"bad maintenance":     `-- m8:partman t control=c partition_interval=1 automatic_maintenance=true`,
		"unterminated quote":  `-- m8:partman t control=c partition_interval='1 month`,
		"value without a key": `-- m8:partman t control=c partition_interval=1 =4`,
	} {
		if _, err := Parse([]byte(line), "public"); err == nil {
			t.Errorf("%s: expected an error for %q", name, line)
		}
	}
}

// Directive is what `m8 dump` writes; Parse must read back the same spec, or a
// dumped baseline plans a change against the database it was dumped from.
func TestDirectiveRoundTrip(t *testing.T) {
	yes := true
	start := "2026-01-01"
	in := Spec{
		Schema: "public", Table: "event_log", Control: "created_at", Interval: "1 mon",
		DefaultTable: &yes, StartPartition: &start,
		Settings: map[string]string{"premake": "4", "retention": "6 months", "jobmon": "false", "automatic_maintenance": "on"},
	}
	line := in.Directive()
	if !strings.Contains(line, "partition_interval='1 mon'") || !strings.Contains(line, "retention='6 months'") {
		t.Errorf("values with spaces must be quoted: %s", line)
	}
	out, err := Parse([]byte(line), "public")
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	if !reflect.DeepEqual(out[0], in) {
		t.Errorf("round trip changed the spec:\n in: %+v\nout: %+v", in, out[0])
	}
}

func TestCreateStatementsPassCreateParentArguments(t *testing.T) {
	no := false
	stmts := createStatements("partman", Spec{
		Schema: "public", Table: "t", Control: "created_at", Interval: "1 month", DefaultTable: &no,
		Settings: map[string]string{"premake": "2", "infinite_time_partitions": "true", "retention": "none"},
	})
	if len(stmts) != 2 {
		t.Fatalf("expected create_parent + UPDATE, got %v", stmts)
	}
	// premake decides how many children the first call creates; applied
	// afterwards by UPDATE it would be too late.
	if !strings.Contains(stmts[0], "p_premake := 2") || !strings.Contains(stmts[0], "p_default_table := false") {
		t.Errorf("create_parent missing its arguments: %s", stmts[0])
	}
	if strings.Contains(stmts[1], "premake") ||
		!strings.Contains(stmts[1], `"infinite_time_partitions" = true`) || !strings.Contains(stmts[1], `"retention" = NULL`) {
		t.Errorf("unexpected UPDATE: %s", stmts[1])
	}
}
