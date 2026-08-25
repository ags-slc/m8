package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A warning is not enough when the target is a production primary: falling back
// to creating schema-diff temp databases there means CREATE DATABASE / DROP
// DATABASE churn on the primary. A repository that can never accept that sets
// require_shadow in .m8.yaml, and m8 must then refuse rather than degrade.
func TestRequireShadowRefusesWhenNoShadowIsConfigured(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, ".m8.yaml"),
		[]byte("migrations_dir: migrations\nrequire_shadow: true\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// No shadow anywhere: flag, env, or config.
	t.Setenv("SHADOW_DATABASE_URL", "")
	flagShadowURL = ""
	t.Cleanup(func() { flagShadowURL = "" })

	// Point at a port nothing listens on: the refusal must come from the
	// missing shadow, BEFORE any session is opened on the target.
	t.Setenv("PGHOST", "127.0.0.1")
	t.Setenv("PGPORT", "1")
	t.Setenv("PGDATABASE", "nonexistent")
	t.Setenv("PGUSER", "nonexistent")

	_, _, _, err = connectAndBuildEngine(context.Background(), true)
	if err == nil {
		t.Fatal("expected a refusal, got none")
	}
	if strings.Contains(err.Error(), "failed to connect") {
		t.Errorf("connected to the target before refusing: %v", err)
	}
	if !strings.Contains(err.Error(), "require_shadow") {
		t.Errorf("error does not explain why it refused: %v", err)
	}
	if !strings.Contains(err.Error(), "SHADOW_DATABASE_URL") {
		t.Errorf("error does not say how to fix it: %v", err)
	}
}

// Without require_shadow the historical behaviour is unchanged: m8 warns and
// carries on, so existing users are not broken by the new option.
func TestNoShadowStillDegradesWhenRequireShadowIsUnset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, ".m8.yaml"),
		[]byte("migrations_dir: migrations\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	t.Setenv("SHADOW_DATABASE_URL", "")
	flagShadowURL = ""
	t.Cleanup(func() { flagShadowURL = "" })

	_, _, _, err = connectAndBuildEngine(context.Background(), true)
	// There is no database to connect to here, so this fails -- but it must
	// fail on the connection, never on the missing shadow.
	if err != nil && strings.Contains(err.Error(), "require_shadow") {
		t.Errorf("refused for a missing shadow that was never required: %v", err)
	}
}
