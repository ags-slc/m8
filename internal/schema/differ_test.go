package schema

import "testing"

// TestTempConnComposition mirrors how the temp-DB factory builds a connection
// string: replace the db name first, then append statement_timeout. Appending
// options before replaceDBName would corrupt a key=value DSN (replaceDBName
// tokenizes on whitespace and splits the quoted options value).
func TestTempConnComposition(t *testing.T) {
	tests := []struct {
		name string
		in   string
		db   string
		want string
	}{
		{
			"url",
			"postgres://u:p@h:5432/mydb",
			"pgschemadiff_tmp_x",
			"postgres://u:p@h:5432/pgschemadiff_tmp_x?options=-c+statement_timeout%3D0",
		},
		{
			"keyvalue",
			"host=h port=5432 dbname=mydb user=u",
			"pgschemadiff_tmp_x",
			"host=h port=5432 dbname=pgschemadiff_tmp_x user=u options='-c statement_timeout=0'",
		},
	}
	for _, tt := range tests {
		got := appendConnOption(replaceDBName(tt.in, tt.db), "statement_timeout", "0")
		if got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestReplaceDBNameURL(t *testing.T) {
	tests := []struct {
		input string
		newDB string
		want  string
	}{
		{
			"postgres://user:pass@localhost:5432/mydb",
			"tempdb",
			"postgres://user:pass@localhost:5432/tempdb",
		},
		{
			"postgres://user:pass@localhost:5432/mydb?sslmode=disable",
			"tempdb",
			"postgres://user:pass@localhost:5432/tempdb?sslmode=disable",
		},
		{
			"postgresql://user@host/db?search_path=public",
			"newdb",
			"postgresql://user@host/newdb?search_path=public",
		},
	}
	for _, tt := range tests {
		got := replaceDBName(tt.input, tt.newDB)
		if got != tt.want {
			t.Errorf("replaceDBName(%q, %q) = %q, want %q", tt.input, tt.newDB, got, tt.want)
		}
	}
}

func TestReplaceDBNameKeyValue(t *testing.T) {
	tests := []struct {
		input string
		newDB string
		want  string
	}{
		{
			"host=localhost port=5432 dbname=mydb user=postgres",
			"tempdb",
			"host=localhost port=5432 dbname=tempdb user=postgres",
		},
		{
			"host=localhost user=postgres",
			"tempdb",
			"host=localhost user=postgres dbname=tempdb",
		},
	}
	for _, tt := range tests {
		got := replaceDBName(tt.input, tt.newDB)
		if got != tt.want {
			t.Errorf("replaceDBName(%q, %q) = %q, want %q", tt.input, tt.newDB, got, tt.want)
		}
	}
}
