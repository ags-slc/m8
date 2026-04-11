package schema

import "testing"

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
