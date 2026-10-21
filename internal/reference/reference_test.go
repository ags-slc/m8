package reference

import (
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	s, err := Parse("reference/carrier.sql", []byte(`-- The carriers we rate.
-- m8:reference public.carrier key=code,region prune touch=updated_at,updated_on
SELECT * FROM (VALUES
    ('UPS', 'US', 'United Parcel Service') -- not a statement end; nor is this ;
) AS v(code, region, name);
`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Table != "public.carrier" || !s.Prune ||
		!slices.Equal(s.Key, []string{"code", "region"}) || !slices.Equal(s.Touch, []string{"updated_at", "updated_on"}) {
		t.Errorf("parsed %+v", s)
	}
	if strings.HasSuffix(s.Query, ";") || !strings.Contains(s.Query, "AS v(code, region, name)") {
		t.Errorf("query %q", s.Query)
	}

	s, err = Parse("reference/x.sql", []byte("-- m8:reference x\nVALUES (1)"))
	if err != nil || len(s.Key) != 0 || s.Prune || len(s.Touch) != 0 {
		t.Errorf("bare directive: %+v, %v", s, err)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"no directive", "SELECT 1", "missing -- m8:reference"},
		{"a longer directive is not this one", "-- m8:referenced x\nSELECT 1", "missing -- m8:reference"},
		{"no table", "-- m8:reference\nSELECT 1", "names no table"},
		{"twice", "-- m8:reference a\n-- m8:reference b\nSELECT 1", "more than one"},
		{"unknown option", "-- m8:reference a keys=id\nSELECT 1", `unknown -- m8:reference option "keys=id"`},
		{"empty key column", "-- m8:reference a key=id,\nSELECT 1", "empty column name"},
		{"empty touch", "-- m8:reference a touch=\nSELECT 1", "empty column name"},
		{"two statements", "-- m8:reference a\nSELECT 1;\nSELECT 2;", "found 2 statements"},
		{"no statement", "-- m8:reference a\n", "found 0 statements"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse("reference/a.sql", []byte(tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestLines(t *testing.T) {
	str := func(s string) *string { return &s }
	d := &Diff{
		Key:     []string{"code"},
		Columns: []string{"name", "code"},
		nonKey:  []string{"name"},
		Inserts: []Row{{str("OB"), str("O'Brien")}, {str("NN"), nil}},
		Updates: []Change{{Key: Row{str("UPS")}, Columns: []string{"name"}, Old: Row{nil}, New: Row{str("UPS Inc")}}},
		Deletes: []Row{{str("DHL")}},
	}
	got := strings.Join(d.Lines(1), "\n")
	want := strings.Join([]string{
		"+ ('OB') name='O''Brien'",
		"  … and 1 more",
		"~ ('UPS') name: NULL → 'UPS Inc'",
		"- ('DHL')",
	}, "\n")
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
