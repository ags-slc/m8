package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A dump writes files whose paths come from the target database's catalog:
// schema, table, function and view names. Anyone holding CREATE on a schema
// chooses those names, and a quoted PostgreSQL identifier may contain "/" and
// "..", so every one of them is a path the attacker partly controls. These
// tests pin that no such name can place a file outside the migrations tree,
// and -- just as important -- that none can place one in a SIBLING directory
// inside the tree, because apply executes ops/ and logic/ verbatim.

func TestSafeComponent(t *testing.T) {
	safe := []string{"users", "public", "app_v2", "Foo.sql", "a-b", "_m8", "..hidden", "..."}
	for _, s := range safe {
		if !safeComponent(s) {
			t.Errorf("safeComponent(%q) = false, want true", s)
		}
	}

	unsafe := []string{
		"",             // empty
		".",            // self
		"..",           // parent
		"a/b",          // separator
		"../x",         // climbs
		"/etc/passwd",  // absolute
		`a\b`,          // windows separator
		"x\x00y",       // NUL
		"../../ops/1",  // climbs into an executed folder
		"sub/dir/file", // nested
	}
	for _, s := range unsafe {
		if safeComponent(s) {
			t.Errorf("safeComponent(%q) = true, want false", s)
		}
	}
}

// writeFile must refuse a hostile filename rather than following it.
func TestWriteFileRejectsUnsafeFilename(t *testing.T) {
	root := t.TempDir()

	cases := []struct{ name, filename string }{
		{"parent", "../escaped.sql"},
		{"deep parent", "../../../../escaped.sql"},
		{"separator", "sub/escaped.sql"},
		{"absolute", "/tmp/escaped.sql"},
		{"dot dot", ".."},
		{"empty", ""},
		// The grants_ and schema_ prefixes are ordinary path components that
		// the next ".." simply cancels -- they absorb one level, not the attack.
		{"prefixed", "grants_../../../escaped.sql"},
		{"sibling folder", "../ops/20260101_001__escaped.sql"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := writeFile(root, "logic", tc.filename, "SELECT 1;")
			if err == nil {
				t.Fatalf("writeFile(%q) succeeded, want refusal", tc.filename)
			}
			if !strings.Contains(err.Error(), "refusing to write") {
				t.Fatalf("writeFile(%q) error = %v, want a refusal", tc.filename, err)
			}
		})
	}

	assertNothingOutside(t, root)
}

// writeFile must refuse a relDir that climbs, and must not be fooled by one
// that merely *cleans* to something local: filepath.Join("schema", "../ops")
// is "ops", which is local, exists, and is executed verbatim by apply. That
// case is caught by the caller, so here we pin the escape half.
func TestWriteFileRejectsUnsafeDir(t *testing.T) {
	root := t.TempDir()

	for _, relDir := range []string{"../outside", "../../outside", "/tmp"} {
		if err := writeFile(root, relDir, "x.sql", "SELECT 1;"); err == nil {
			t.Errorf("writeFile(relDir=%q) succeeded, want refusal", relDir)
		}
	}

	assertNothingOutside(t, root)
}

// os.Root is used rather than a lexical filepath.Rel check precisely so that a
// symlink planted inside the tree cannot be used to write outside it.
func TestWriteFileRefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	if err := os.MkdirAll(filepath.Join(root, "logic"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "logic", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// The filename is a plain component, so the component checks pass; only
	// the root confinement stops this one.
	if err := writeFile(root, "logic/link", "escaped.sql", "SELECT 1;"); err == nil {
		t.Fatal("writeFile through a symlink succeeded, want refusal")
	}

	if _, err := os.Stat(filepath.Join(outside, "escaped.sql")); err == nil {
		t.Fatal("file written outside the root through a symlink")
	}
}

// The ordinary path still has to work.
func TestWriteFileWritesLegitimateNames(t *testing.T) {
	root := t.TempDir()

	if err := writeFile(root, filepath.Join("schema", "public"), "users.sql", "CREATE TABLE users ();"); err != nil {
		t.Fatalf("legitimate table write failed: %v", err)
	}
	if err := writeFile(root, "logic", "app_refresh.sql", "CREATE FUNCTION f();"); err != nil {
		t.Fatalf("legitimate logic write failed: %v", err)
	}
	if err := writeFile(root, "permissions", "grants_public.sql", "GRANT USAGE ON SCHEMA public TO r;"); err != nil {
		t.Fatalf("legitimate grants write failed: %v", err)
	}

	for _, want := range []string{
		filepath.Join("schema", "public", "users.sql"),
		filepath.Join("logic", "app_refresh.sql"),
		filepath.Join("permissions", "grants_public.sql"),
	} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Errorf("expected %s to exist: %v", want, err)
		}
	}
}

// assertNothingOutside fails if anything was created in root's parent, which is
// where a successful escape from these tests would land.
func assertNothingOutside(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			t.Errorf("file escaped the root: %s", filepath.Join(filepath.Dir(root), e.Name()))
		}
	}
}
