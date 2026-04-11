package migration

import (
	"os"
	"path/filepath"
	"testing"
)

// --- Folder-based layout tests ---

func TestFolderOps(t *testing.T) {
	dir := t.TempDir()
	opsDir := filepath.Join(dir, "ops")
	os.Mkdir(opsDir, 0755)
	os.WriteFile(filepath.Join(opsDir, "20260411_001__create_ext.sql"), []byte("CREATE EXTENSION pg_trgm;"), 0644)
	os.WriteFile(filepath.Join(opsDir, "20260411_002__create_hypertable.sql"), []byte("SELECT create_hypertable('t','ts');"), 0644)
	os.WriteFile(filepath.Join(opsDir, "bad_name.sql"), []byte("skip me"), 0644) // no timestamp

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 2 {
		t.Fatalf("expected 2, got %d", len(migrations))
	}
	if migrations[0].Type != TypeOps {
		t.Errorf("expected TypeOps, got %v", migrations[0].Type)
	}
	if migrations[0].Version != "20260411_001" {
		t.Errorf("version: got %q", migrations[0].Version)
	}
	if migrations[0].Filename != "ops/20260411_001__create_ext.sql" {
		t.Errorf("filename: got %q", migrations[0].Filename)
	}
}

func TestFolderSchemaWithPGSchemas(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "schema", "public"), 0755)
	os.MkdirAll(filepath.Join(dir, "schema", "materialized"), 0755)

	os.WriteFile(filepath.Join(dir, "schema", "public", "users.sql"), []byte("CREATE TABLE users (id INT);"), 0644)
	os.WriteFile(filepath.Join(dir, "schema", "public", "orders.sql"), []byte("CREATE TABLE orders (id INT);"), 0644)
	os.WriteFile(filepath.Join(dir, "schema", "materialized", "rpt_invoice.sql"), []byte("CREATE TABLE rpt_invoice (id INT);"), 0644)

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 3 {
		t.Fatalf("expected 3, got %d", len(migrations))
	}
	// Sorted by PGSchema then Name: materialized/rpt_invoice, public/orders, public/users
	if migrations[0].PGSchema != "materialized" {
		t.Errorf("[0] pgschema: got %q", migrations[0].PGSchema)
	}
	if migrations[0].Name != "rpt_invoice" {
		t.Errorf("[0] name: got %q", migrations[0].Name)
	}
	if migrations[0].Filename != "schema/materialized/rpt_invoice.sql" {
		t.Errorf("[0] filename: got %q", migrations[0].Filename)
	}
	if migrations[1].PGSchema != "public" {
		t.Errorf("[1] pgschema: got %q", migrations[1].PGSchema)
	}
	if migrations[1].Name != "orders" {
		t.Errorf("[1] name: got %q", migrations[1].Name)
	}
	if migrations[2].Name != "users" {
		t.Errorf("[2] name: got %q", migrations[2].Name)
	}
}

func TestFolderLogic(t *testing.T) {
	dir := t.TempDir()
	logicDir := filepath.Join(dir, "logic")
	os.Mkdir(logicDir, 0755)
	os.WriteFile(filepath.Join(logicDir, "proc_refresh.sql"), []byte("CREATE OR REPLACE PROCEDURE p() LANGUAGE plpgsql AS $$ BEGIN END; $$;"), 0644)
	os.WriteFile(filepath.Join(logicDir, "view_summary.sql"), []byte("CREATE OR REPLACE VIEW v AS SELECT 1;"), 0644)

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 2 {
		t.Fatalf("expected 2, got %d", len(migrations))
	}
	if migrations[0].Type != TypeLogic {
		t.Errorf("expected TypeLogic, got %v", migrations[0].Type)
	}
	if migrations[0].Name != "proc_refresh" {
		t.Errorf("expected alphabetical: got %q", migrations[0].Name)
	}
}

func TestFolderPermissions(t *testing.T) {
	dir := t.TempDir()
	permDir := filepath.Join(dir, "permissions")
	os.Mkdir(permDir, 0755)
	os.WriteFile(filepath.Join(permDir, "grants_public.sql"), []byte("GRANT SELECT ON t TO PUBLIC;"), 0644)
	os.WriteFile(filepath.Join(permDir, "roles.sql"), []byte("-- role definitions"), 0644)

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 2 {
		t.Fatalf("expected 2, got %d", len(migrations))
	}
	if migrations[0].Type != TypePermissions {
		t.Errorf("expected TypePermissions, got %v", migrations[0].Type)
	}
}

func TestFolderMixedAllTypes(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "ops"), 0755)
	os.MkdirAll(filepath.Join(dir, "schema", "public"), 0755)
	os.Mkdir(filepath.Join(dir, "logic"), 0755)
	os.Mkdir(filepath.Join(dir, "permissions"), 0755)

	os.WriteFile(filepath.Join(dir, "ops", "20260411_001__ext.sql"), []byte("SELECT 1;"), 0644)
	os.WriteFile(filepath.Join(dir, "schema", "public", "users.sql"), []byte("CREATE TABLE users (id INT);"), 0644)
	os.WriteFile(filepath.Join(dir, "logic", "proc.sql"), []byte("SELECT 1;"), 0644)
	os.WriteFile(filepath.Join(dir, "permissions", "grants.sql"), []byte("GRANT SELECT ON t TO r;"), 0644)

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 4 {
		t.Fatalf("expected 4, got %d", len(migrations))
	}

	// Order: ops → schema → logic → permissions
	expectedTypes := []Type{TypeOps, TypeSchema, TypeLogic, TypePermissions}
	for i, exp := range expectedTypes {
		if migrations[i].Type != exp {
			t.Errorf("[%d] expected %v, got %v", i, exp, migrations[i].Type)
		}
	}
}

func TestFolderMissingSubdirs(t *testing.T) {
	dir := t.TempDir()
	// Only create schema/ — others don't exist
	os.MkdirAll(filepath.Join(dir, "schema", "public"), 0755)
	os.WriteFile(filepath.Join(dir, "schema", "public", "users.sql"), []byte("CREATE TABLE users (id INT);"), 0644)

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 1 {
		t.Fatalf("expected 1, got %d", len(migrations))
	}
}

func TestFolderSchemaSkipsFilesAtRoot(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "schema", "public"), 0755)
	// File at schema/ root (no PG schema subfolder) should be skipped
	os.WriteFile(filepath.Join(dir, "schema", "stray.sql"), []byte("SELECT 1;"), 0644)
	os.WriteFile(filepath.Join(dir, "schema", "public", "users.sql"), []byte("CREATE TABLE users (id INT);"), 0644)

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 1 {
		t.Fatalf("expected 1 (skip stray file), got %d", len(migrations))
	}
}

// --- Legacy flat layout tests (backward compat) ---

func TestLegacyFlatLayout(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"V20260411_002__second.sql": "SELECT 2;",
		"V20260411_001__first.sql":  "SELECT 1;",
		"R__grants.sql":             "GRANT SELECT ON t TO r;",
		"R__alpha_proc.sql":         "SELECT 3;",
		"S__users.sql":              "CREATE TABLE users (id INT);",
		"S__accounts.sql":           "CREATE TABLE accounts (id INT);",
		"not_a_migration.txt":       "ignore me",
	}
	for name, content := range files {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
	}

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 6 {
		t.Fatalf("expected 6, got %d", len(migrations))
	}

	// Order: ops (by version) → schema (by name) → logic (by name)
	expected := []struct {
		typ  Type
		name string
	}{
		{TypeOps, "first"},
		{TypeOps, "second"},
		{TypeSchema, "accounts"},
		{TypeSchema, "users"},
		{TypeLogic, "alpha_proc"},
		{TypeLogic, "grants"},
	}
	for i, exp := range expected {
		if migrations[i].Type != exp.typ {
			t.Errorf("[%d] type: got %v, want %v", i, migrations[i].Type, exp.typ)
		}
		if migrations[i].Name != exp.name {
			t.Errorf("[%d] name: got %q, want %q", i, migrations[i].Name, exp.name)
		}
	}

	// Legacy S__ should default to PGSchema="public"
	for _, m := range migrations {
		if m.Type == TypeSchema && m.PGSchema != "public" {
			t.Errorf("legacy schema %q should have PGSchema=public, got %q", m.Name, m.PGSchema)
		}
	}
}

// --- General tests ---

func TestComputeChecksum(t *testing.T) {
	content := []byte("SELECT 1;")
	cs1 := ComputeChecksum(content)
	cs2 := ComputeChecksum(content)
	if cs1 != cs2 {
		t.Error("checksums should be deterministic")
	}
	if len(cs1) != 64 {
		t.Errorf("SHA-256 hex should be 64 chars, got %d", len(cs1))
	}

	cs3 := ComputeChecksum([]byte("SELECT 2;"))
	if cs1 == cs3 {
		t.Error("different content should produce different checksums")
	}
}

func TestDiscoverEmptyDir(t *testing.T) {
	dir := t.TempDir()
	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 0 {
		t.Errorf("expected 0, got %d", len(migrations))
	}
}

func TestDiscoverNonexistentDir(t *testing.T) {
	_, err := Discover("/nonexistent/path")
	if err == nil {
		t.Error("expected error")
	}
}

func TestTypeString(t *testing.T) {
	tests := []struct {
		typ  Type
		want string
	}{
		{TypeOps, "ops"},
		{TypeSchema, "schema"},
		{TypeLogic, "logic"},
		{TypePermissions, "permissions"},
		{Type(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.typ.String(); got != tt.want {
			t.Errorf("Type(%d).String() = %q, want %q", tt.typ, got, tt.want)
		}
	}
}
