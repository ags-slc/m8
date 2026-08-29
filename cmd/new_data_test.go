package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// `m8 new data` must scaffold into data/, with the same timestamp filename
// shape ops/ uses -- Discover skips a file in a versioned folder that has no
// timestamp prefix, so a wrong shape here produces a file nothing ever runs.
func TestNewDataScaffoldsAVersionedFileInDataFolder(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	rootCmd.SetArgs([]string{"new", "data", "backfill destination country"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("m8 new data: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "migrations", "data"))
	if err != nil {
		t.Fatalf("reading migrations/data: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 scaffolded file, got %d", len(entries))
	}

	name := entries[0].Name()
	// The same pattern migration.Discover requires of a versioned folder.
	if !regexp.MustCompile(`^\d{8}_\d{6}__backfill_destination_country\.sql$`).MatchString(name) {
		t.Errorf("scaffolded %q, which does not match the versioned filename shape", name)
	}

	body, err := os.ReadFile(filepath.Join(dir, "migrations", "data", name))
	if err != nil {
		t.Fatal(err)
	}
	// The template's job is to say which end of the apply this runs at; that is
	// the single fact someone reaching for ops/ by habit needs.
	if !strings.Contains(string(body), "AFTER") {
		t.Errorf("template does not say when data/ runs:\n%s", body)
	}
}

func TestNewRejectsAnUnknownType(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	rootCmd.SetArgs([]string{"new", "backfill", "something"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected an unknown migration type to be rejected")
	}
	// The error lists the valid types; it must name data/ now that it exists.
	if !strings.Contains(err.Error(), "data") {
		t.Errorf("error does not offer data/ as a valid type: %v", err)
	}
}
