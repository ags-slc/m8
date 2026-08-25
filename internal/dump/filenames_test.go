package dump

import "testing"

// Two overloads of one procedure must not share a filename. The naive
// schema+name scheme wrote both to logic/materialized_proc_lcb_backfill.sql,
// so the zero-argument overload vanished from the captured baseline without
// any error -- a silent hole in a tree whose whole purpose is completeness.
func TestResolveLogicFileNamesSeparatesOverloads(t *testing.T) {
	zeroArg := LogicObject{Schema: "materialized", Name: "proc_lcb_backfill"}
	oneArg := LogicObject{Schema: "materialized", Name: "proc_lcb_backfill", Identity: "IN p_start_date date"}

	names := ResolveLogicFileNames([]LogicObject{zeroArg, oneArg})

	if names[zeroArg] == names[oneArg] {
		t.Fatalf("overloads collided on %q", names[zeroArg])
	}
	if names[zeroArg] == "" || names[oneArg] == "" {
		t.Fatalf("missing filename: %q / %q", names[zeroArg], names[oneArg])
	}
	for _, n := range []string{names[zeroArg], names[oneArg]} {
		if n[len(n)-4:] != ".sql" {
			t.Errorf("filename %q is not a .sql file", n)
		}
	}
}

// A name that collides gets a suffix; a name that does not stays short and
// stable, so the common case keeps readable filenames.
func TestResolveLogicFileNamesKeepsUniqueNamesShort(t *testing.T) {
	view := LogicObject{Schema: "public", Name: "invoice_list"}
	fn := LogicObject{Schema: "materialized", Name: "proc_refresh_invoice_detail", Identity: "IN p_batch integer"}

	names := ResolveLogicFileNames([]LogicObject{view, fn})

	if got, want := names[view], "invoice_list.sql"; got != want {
		t.Errorf("public object: got %q, want %q", got, want)
	}
	if got, want := names[fn], "materialized_proc_refresh_invoice_detail.sql"; got != want {
		t.Errorf("schema-prefixed object: got %q, want %q", got, want)
	}
}

// A view and a function sharing a name in the same schema also collide, and a
// public object named "<schema>_<name>" can collide with a prefixed one.
func TestResolveLogicFileNamesSeparatesCrossKindCollisions(t *testing.T) {
	view := LogicObject{Schema: "radar", Name: "outcome_summary"}
	fn := LogicObject{Schema: "public", Name: "radar_outcome_summary", Identity: "p_id text"}

	names := ResolveLogicFileNames([]LogicObject{view, fn})

	if names[view] == names[fn] {
		t.Fatalf("cross-schema filename collision on %q", names[view])
	}
}

// Filenames must be reproducible: the same set of objects in a different order
// must yield the same names, or every re-dump churns the tree.
func TestResolveLogicFileNamesIsDeterministic(t *testing.T) {
	a := LogicObject{Schema: "materialized", Name: "f", Identity: "IN b integer"}
	b := LogicObject{Schema: "materialized", Name: "f", Identity: "IN a integer"}

	first := ResolveLogicFileNames([]LogicObject{a, b})
	second := ResolveLogicFileNames([]LogicObject{b, a})

	if first[a] != second[a] || first[b] != second[b] {
		t.Errorf("filenames are order-dependent: %v vs %v", first, second)
	}
}
