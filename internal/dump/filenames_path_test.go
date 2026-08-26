package dump

import (
	"path/filepath"
	"strings"
	"testing"
)

// A logic filename is built from a function or view name read out of the target
// catalog. PostgreSQL allows "/" and ".." inside a quoted identifier, so these
// tests pin the invariant the write path depends on: whatever the catalog says,
// a resolved logic filename is exactly one path component.

func TestSanitizeComponent(t *testing.T) {
	cases := map[string]string{
		"users":         "users",
		"refresh_daily": "refresh_daily",
		"Foo.Bar":       "Foo.Bar",
		"a-b":           "a-b",
		"a/b":           "a_b",
		"../etc/passwd": ".._etc_passwd",
		"..":            "_",
		".":             "_",
		"":              "_",
		"a\\b":          "a_b",
		"sp ace":        "sp_ace",
	}
	for in, want := range cases {
		if got := SanitizeComponent(in); got != want {
			t.Errorf("SanitizeComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

// The invariant that matters: no resolved name is ever more than one component.
func TestResolveLogicFileNamesAreSingleComponents(t *testing.T) {
	objects := []LogicObject{
		{Schema: "public", Name: "../../../../etc/cron.d/pwn"},
		{Schema: "../ops", Name: "20260101_001__pwn"},
		{Schema: "public", Name: ".."},
		{Schema: "public", Name: "."},
		{Schema: "app", Name: "a/b"},
		{Schema: "app", Name: "a_b"}, // collides with the above once sanitized
		{Schema: "public", Name: "normal_view"},
		{Schema: "app", Name: "overloaded", Identity: "a integer"},
		{Schema: "app", Name: "overloaded", Identity: "a text"},
	}

	names := ResolveLogicFileNames(objects)

	if len(names) != len(objects) {
		t.Fatalf("got %d names for %d objects; objects were silently merged", len(names), len(objects))
	}

	seen := make(map[string]LogicObject, len(names))
	for o, n := range names {
		if strings.ContainsAny(n, `/\`) {
			t.Errorf("name for %+v contains a path separator: %q", o, n)
		}
		if n != filepath.Base(n) {
			t.Errorf("name for %+v is not a single component: %q", o, n)
		}
		if !filepath.IsLocal(n) {
			t.Errorf("name for %+v is not local: %q", o, n)
		}
		if strings.HasPrefix(n, "..") && !strings.HasPrefix(n, "..hidden") {
			// ".." as a leading literal is fine inside a longer component, but
			// the name must never BE ".." or "../something".
			if n == ".." || n == "..sql" {
				t.Errorf("name for %+v is a parent reference: %q", o, n)
			}
		}
		// Case-folded, because APFS and NTFS collapse case.
		key := strings.ToLower(n)
		if prev, dup := seen[key]; dup {
			t.Errorf("collision on %q between %+v and %+v", n, prev, o)
		}
		seen[key] = o
	}
}

// Sanitizing must not reintroduce the silent-overwrite bug the dedup pass
// exists to prevent: two names that differ only in characters the sanitizer
// destroys must still resolve to distinct files.
func TestSanitizedCollisionsStillDeduplicate(t *testing.T) {
	objects := []LogicObject{
		{Schema: "public", Name: "a/b"},
		{Schema: "public", Name: "a_b"},
		{Schema: "public", Name: "a-b"},
	}
	names := ResolveLogicFileNames(objects)

	distinct := make(map[string]bool, len(names))
	for _, n := range names {
		distinct[strings.ToLower(n)] = true
	}
	if len(distinct) != len(objects) {
		t.Fatalf("got %d distinct filenames for %d objects: %v", len(distinct), len(objects), names)
	}
}
