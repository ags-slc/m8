package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inRepoDir writes a .m8.yaml (when body is non-empty) into a temp directory and
// makes it the working directory for the test.
func inRepoDir(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, ".m8.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
}

// TestUnparseableConfigIsFatal pins that a typo in .m8.yaml stops the run.
//
// loadConfig used to log slog.Warn and hand back an EMPTY config on any parse
// error, so a mis-indented line silently turned require_shadow back off and m8
// carried on against the target -- the exact degrade the setting exists to
// forbid, disarmed by the file that was supposed to forbid it.
func TestUnparseableConfigIsFatal(t *testing.T) {
	// Valid-looking, and require_shadow is plainly there -- but the list item
	// makes it invalid YAML for this struct.
	inRepoDir(t, "require_shadow: true\nmigrations_dir:\n  - migrations\n")

	t.Setenv("SHADOW_DATABASE_URL", "")
	flagShadowURL = ""
	t.Cleanup(func() { flagShadowURL = "" })

	// A reachable-looking target, so "we refused because of the config" and
	// "we could not connect" stay distinguishable.
	t.Setenv("PGHOST", "127.0.0.1")
	t.Setenv("PGPORT", "1")
	t.Setenv("PGDATABASE", "nonexistent")
	t.Setenv("PGUSER", "nonexistent")

	_, _, _, err := connectAndBuildEngine(context.Background(), true)
	if err == nil {
		t.Fatal("expected an unparseable .m8.yaml to be fatal, got no error")
	}
	if !strings.Contains(err.Error(), ".m8.yaml") {
		t.Errorf("error does not name the file that could not be read: %v", err)
	}
	if strings.Contains(err.Error(), "failed to connect") {
		t.Errorf("connected to the target with a config it could not read: %v", err)
	}
}

// A missing .m8.yaml is still fine: m8 has always worked from flags and
// environment alone.
func TestMissingConfigIsNotAnError(t *testing.T) {
	inRepoDir(t, "")

	st, err := resolveSettings()
	if err != nil {
		t.Fatalf("a missing .m8.yaml must not be an error: %v", err)
	}
	if st.RequireShadow || st.Strict {
		t.Errorf("defaults changed with no config present: %+v", st)
	}
}

// M8_REQUIRE_SHADOW lets CI enforce the refusal independently of the file --
// which matters precisely because the file is the thing an editing mistake can
// disarm.
func TestRequireShadowEnvOverride(t *testing.T) {
	inRepoDir(t, "migrations_dir: migrations\n")

	t.Setenv("SHADOW_DATABASE_URL", "")
	t.Setenv("M8_REQUIRE_SHADOW", "true")
	flagShadowURL = ""
	t.Cleanup(func() { flagShadowURL = "" })

	t.Setenv("PGHOST", "127.0.0.1")
	t.Setenv("PGPORT", "1")
	t.Setenv("PGDATABASE", "nonexistent")
	t.Setenv("PGUSER", "nonexistent")

	_, _, _, err := connectAndBuildEngine(context.Background(), true)
	if err == nil {
		t.Fatal("M8_REQUIRE_SHADOW=true did not enforce a shadow instance")
	}
	if !strings.Contains(err.Error(), "require_shadow") {
		t.Errorf("error does not explain why it refused: %v", err)
	}
	if strings.Contains(err.Error(), "failed to connect") {
		t.Errorf("connected to the target before refusing: %v", err)
	}
}

// A boolean environment override that does not parse is an error, not a silent
// false: "M8_REQUIRE_SHADOW=ture" in a pipeline would otherwise disarm the gate
// it was added to enforce.
func TestBooleanEnvOverrideMustParse(t *testing.T) {
	inRepoDir(t, "require_shadow: true\n")

	t.Setenv("M8_REQUIRE_SHADOW", "ture")

	if _, err := resolveSettings(); err == nil {
		t.Fatal("expected an unparseable M8_REQUIRE_SHADOW to be an error")
	} else if !strings.Contains(err.Error(), "M8_REQUIRE_SHADOW") {
		t.Errorf("error does not name the variable: %v", err)
	}
}
