package migration

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseVersionedFilename(t *testing.T) {
	tests := []struct {
		filename string
		wantVer  string
		wantName string
	}{
		{"V20260411_001__create_users_table.sql", "20260411_001", "create users table"},
		{"V1__initial.sql", "1", "initial"},
		{"V20260101_999__complex_migration_name.sql", "20260101_999", "complex migration name"},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tt.filename), []byte("SELECT 1;"), 0644); err != nil {
				t.Fatal(err)
			}

			m, err := parseFile(dir, tt.filename)
			if err != nil {
				t.Fatal(err)
			}
			if m == nil {
				t.Fatal("expected migration, got nil")
			}
			if m.Type != TypeVersioned {
				t.Errorf("type: got %v, want %v", m.Type, TypeVersioned)
			}
			if m.Version != tt.wantVer {
				t.Errorf("version: got %q, want %q", m.Version, tt.wantVer)
			}
			if m.Name != tt.wantName {
				t.Errorf("name: got %q, want %q", m.Name, tt.wantName)
			}
		})
	}
}

func TestParseRepeatableFilename(t *testing.T) {
	dir := t.TempDir()
	filename := "R__proc_refresh_invoice_detail.sql"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte("SELECT 1;"), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := parseFile(dir, filename)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected migration, got nil")
	}
	if m.Type != TypeRepeatable {
		t.Errorf("type: got %v, want %v", m.Type, TypeRepeatable)
	}
	if m.Name != "proc_refresh_invoice_detail" {
		t.Errorf("name: got %q", m.Name)
	}
	if m.Version != "" {
		t.Errorf("version should be empty for repeatable, got %q", m.Version)
	}
}

func TestParseSchemaFilename(t *testing.T) {
	dir := t.TempDir()
	filename := "S__users.sql"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte("CREATE TABLE users (id INT);"), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := parseFile(dir, filename)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected migration, got nil")
	}
	if m.Type != TypeSchema {
		t.Errorf("type: got %v, want %v", m.Type, TypeSchema)
	}
	if m.Name != "users" {
		t.Errorf("name: got %q", m.Name)
	}
}

func TestParseUnrecognizedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "not_a_migration.sql"), []byte("SELECT 1;"), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := parseFile(dir, "not_a_migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Error("expected nil for unrecognized filename")
	}
}

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

func TestDiscoverSortOrder(t *testing.T) {
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
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(migrations) != 6 {
		t.Fatalf("expected 6 migrations, got %d", len(migrations))
	}

	// Expected order: versioned (by version), schema (by name), repeatable (by name)
	expected := []struct {
		typ  Type
		name string
	}{
		{TypeVersioned, "first"},
		{TypeVersioned, "second"},
		{TypeSchema, "accounts"},
		{TypeSchema, "users"},
		{TypeRepeatable, "alpha_proc"},
		{TypeRepeatable, "grants"},
	}

	for i, exp := range expected {
		if migrations[i].Type != exp.typ {
			t.Errorf("migration[%d] type: got %v, want %v", i, migrations[i].Type, exp.typ)
		}
		if migrations[i].Name != exp.name {
			t.Errorf("migration[%d] name: got %q, want %q", i, migrations[i].Name, exp.name)
		}
	}
}

func TestDiscoverEmptyDir(t *testing.T) {
	dir := t.TempDir()
	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 0 {
		t.Errorf("expected 0 migrations, got %d", len(migrations))
	}
}

func TestDiscoverNonexistentDir(t *testing.T) {
	_, err := Discover("/nonexistent/path")
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

func TestDiscoverSkipsNonSQL(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("not sql"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "V1__real.sql"), []byte("SELECT 1;"), 0644); err != nil {
		t.Fatal(err)
	}

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 1 {
		t.Errorf("expected 1 migration, got %d", len(migrations))
	}
}

func TestDiscoverSkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "V1__real.sql"), []byte("SELECT 1;"), 0644); err != nil {
		t.Fatal(err)
	}

	migrations, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 1 {
		t.Errorf("expected 1 migration, got %d", len(migrations))
	}
}

func TestTypeString(t *testing.T) {
	tests := []struct {
		typ  Type
		want string
	}{
		{TypeVersioned, "versioned"},
		{TypeRepeatable, "repeatable"},
		{TypeSchema, "schema"},
		{Type(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.typ.String(); got != tt.want {
			t.Errorf("Type(%d).String() = %q, want %q", tt.typ, got, tt.want)
		}
	}
}

func TestMigrationChecksum(t *testing.T) {
	dir := t.TempDir()
	content := "CREATE TABLE users (id INT);"
	if err := os.WriteFile(filepath.Join(dir, "S__users.sql"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := parseFile(dir, "S__users.sql")
	if err != nil {
		t.Fatal(err)
	}

	expected := ComputeChecksum([]byte(content))
	if m.Checksum != expected {
		t.Errorf("checksum mismatch: got %q, want %q", m.Checksum, expected)
	}
}
