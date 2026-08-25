package dump

import (
	"strings"
	"testing"
)

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
	// The suffix disambiguates on arguments only; it must not repeat the name
	// that is already in the prefix.
	if got, want := names[oneArg], "materialized_proc_lcb_backfill__in_p_start_date_date.sql"; got != want {
		t.Errorf("overload filename: got %q, want %q", got, want)
	}
	if got, want := names[zeroArg], "materialized_proc_lcb_backfill__noargs.sql"; got != want {
		t.Errorf("zero-arg overload filename: got %q, want %q", got, want)
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
	a := LogicObject{Schema: "materialized", Name: "f", Identity: "f(IN b integer)"}
	b := LogicObject{Schema: "materialized", Name: "f", Identity: "f(IN a integer)"}

	first := ResolveLogicFileNames([]LogicObject{a, b})
	second := ResolveLogicFileNames([]LogicObject{b, a})

	if first[a] != second[a] || first[b] != second[b] {
		t.Errorf("filenames are order-dependent: %v vs %v", first, second)
	}
}

// argSlug is lossy: f(text) and f(text[]) both slug to "text", because
// nonFilenameChars collapses "[]" to "_" and the trailing "_" is trimmed. Three
// such overloads used to produce only two filenames -- the ordinal was
// incremented against the rewritten candidate, so __2 was handed out twice and
// one function was overwritten by another.
func TestResolveLogicFileNamesSeparatesSlugCollidingOverloads(t *testing.T) {
	objects := []LogicObject{
		{Schema: "public", Name: "f", Identity: "f(text)"},
		{Schema: "public", Name: "f", Identity: "f(text[])"},
		{Schema: "public", Name: "f", Identity: "f(text[][])"},
	}

	names := ResolveLogicFileNames(objects)

	seen := make(map[string]LogicObject, len(objects))
	for _, o := range objects {
		n := names[o]
		if n == "" {
			t.Fatalf("no filename for %+v", o)
		}
		if prev, dup := seen[n]; dup {
			t.Errorf("%+v and %+v both write to %q -- one is silently lost", prev, o, n)
		}
		seen[n] = o
	}
	if len(seen) != len(objects) {
		t.Errorf("got %d filenames for %d objects", len(seen), len(objects))
	}
}

// Removing one overload must never hand its path to a different overload. With
// an ordinal disambiguator, deleting f(text) renamed f(text[]) from f__text_2
// to f__text: the file f__text.sql stayed in the tree and silently changed
// which function it contained.
func TestResolveLogicFileNamesDoNotInheritARemovedSiblingsPath(t *testing.T) {
	integer := LogicObject{Schema: "public", Name: "f", Identity: "f(integer)"}
	text := LogicObject{Schema: "public", Name: "f", Identity: "f(text)"}
	textArray := LogicObject{Schema: "public", Name: "f", Identity: "f(text[])"}

	before := ResolveLogicFileNames([]LogicObject{integer, text, textArray})
	after := ResolveLogicFileNames([]LogicObject{integer, textArray})

	owner := make(map[string]LogicObject, len(before))
	for o, n := range before {
		owner[n] = o
	}
	for o, n := range after {
		if prev, ok := owner[n]; ok && prev != o {
			t.Errorf("%q used to hold %+v and now holds %+v -- same path, different object",
				n, prev, o)
		}
	}
}

// Two objects can collide on base filename while sharing an Identity: a view in
// schema "a" named "b_c" and a view in schema "a_b" named "c" both base to
// "a_b_c", and both have the empty Identity of a view. The old sort keyed on
// Identity alone and was not stable, so which one got the plain name and which
// got the ordinal depended on the order the caller happened to pass them in.
func TestResolveLogicFileNamesHandlesEqualIdentities(t *testing.T) {
	first := LogicObject{Schema: "a", Name: "b_c"}
	second := LogicObject{Schema: "a_b", Name: "c"}

	forward := ResolveLogicFileNames([]LogicObject{first, second})
	reverse := ResolveLogicFileNames([]LogicObject{second, first})

	if forward[first] == forward[second] {
		t.Fatalf("objects with equal Identity collided on %q", forward[first])
	}
	if forward[first] != reverse[first] || forward[second] != reverse[second] {
		t.Errorf("filenames flipped with input order:\n  forward: %v\n  reverse: %v", forward, reverse)
	}
}

// A pathological signature must not produce a path longer than a filesystem
// will take. Truncation is safe because a truncated slug that is no longer
// unique collides, and a collision pulls in the hash.
func TestResolveLogicFileNamesBoundsFilenameLength(t *testing.T) {
	long := strings.Repeat("p_very_long_argument_name text, ", 40)
	a := LogicObject{Schema: "public", Name: "f", Identity: "f(" + long + "a integer)"}
	b := LogicObject{Schema: "public", Name: "f", Identity: "f(" + long + "b integer)"}

	names := ResolveLogicFileNames([]LogicObject{a, b})

	if names[a] == names[b] {
		t.Fatalf("truncation collapsed two overloads onto %q", names[a])
	}
	for _, n := range []string{names[a], names[b]} {
		if len(n) > 255 {
			t.Errorf("filename is %d bytes, longer than a filesystem will take: %q", len(n), n)
		}
	}
}

// The per-group reasoning cannot see across groups. BaseFileName is
// Schema + "_" + Name, so a base can itself contain "__" and collide with
// another group's disambiguated name.
func TestResolveLogicFileNamesSeparatesCrossGroupCollisions(t *testing.T) {
	// rpt.totals(date) is disambiguated to rpt_totals__date.sql; rpt."totals__date"
	// bases to exactly that.
	byDate := LogicObject{Schema: "rpt", Name: "totals", Identity: "totals(date)"}
	byInt := LogicObject{Schema: "rpt", Name: "totals", Identity: "totals(integer)"}
	helper := LogicObject{Schema: "rpt", Name: "totals__date"}

	assertDistinctFilenames(t, []LogicObject{byDate, byInt, helper})
}

// A case-insensitive filesystem -- APFS, NTFS -- makes Foo.sql and foo.sql one
// file however distinct the map keys are, so one object overwrites the other on
// the machine most dumps are taken from.
func TestResolveLogicFileNamesSeparatesCaseOnlyCollisions(t *testing.T) {
	upper := LogicObject{Schema: "public", Name: "Report"}
	lower := LogicObject{Schema: "public", Name: "report"}

	names := ResolveLogicFileNames([]LogicObject{upper, lower})
	if strings.EqualFold(names[upper], names[lower]) {
		t.Errorf("case-only difference resolves to one file: %q and %q", names[upper], names[lower])
	}
}

// assertDistinctFilenames checks that every object gets a filename, that no two
// share one (compared case-folded), and that the mapping does not depend on the
// order the objects were passed in.
func assertDistinctFilenames(t *testing.T, objects []LogicObject) {
	t.Helper()
	names := ResolveLogicFileNames(objects)

	seen := make(map[string]LogicObject, len(objects))
	for _, o := range objects {
		n := names[o]
		if n == "" {
			t.Fatalf("no filename for %+v", o)
		}
		key := strings.ToLower(n)
		if prev, dup := seen[key]; dup {
			t.Errorf("%+v and %+v both write to %q -- one is silently lost", prev, o, n)
		}
		seen[key] = o
	}

	reversed := make([]LogicObject, len(objects))
	for i, o := range objects {
		reversed[len(objects)-1-i] = o
	}
	again := ResolveLogicFileNames(reversed)
	for _, o := range objects {
		if names[o] != again[o] {
			t.Errorf("filename for %+v depends on input order: %q vs %q", o, names[o], again[o])
		}
	}
}
