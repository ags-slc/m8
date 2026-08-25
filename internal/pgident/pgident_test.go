package pgident

import "testing"

func TestQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		why  string
	}{
		{"lowercase", "orders", `"orders"`, ""},
		{"mixed case", "Orders", `"Orders"`,
			"an unquoted Orders is folded to orders and refers to a different table"},
		{"reserved word", "order", `"order"`,
			"an unquoted ORDER is a syntax error, so the table could not be dumped at all"},
		{"reserved word select", "select", `"select"`, ""},
		{"space", "my table", `"my table"`, ""},
		{"embedded quote", `we"ird`, `"we""ird"`,
			"an unescaped quote closes the identifier and the rest becomes syntax"},
		{"dot", "a.b", `"a.b"`,
			"an unquoted a.b parses as schema a, table b"},
		{"empty", "", `""`, ""},
		{"temp database", "pgschemadiff_tmp_abc", `"pgschemadiff_tmp_abc"`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Quote(tt.in); got != tt.want {
				t.Errorf("Quote(%q) = %q, want %q\n  %s", tt.in, got, tt.want, tt.why)
			}
		})
	}
}

func TestQualify(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{[]string{"public", "orders"}, `"public"."orders"`},
		{[]string{"", "orders"}, `"orders"`},
		{[]string{"My Schema", "My Table"}, `"My Schema"."My Table"`},
		{[]string{"public", "t", "col"}, `"public"."t"."col"`},
	}
	for _, tt := range tests {
		if got := Qualify(tt.in...); got != tt.want {
			t.Errorf("Qualify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestQuoteAll(t *testing.T) {
	got := QuoteAll([]string{"a", "Order"})
	if len(got) != 2 || got[0] != `"a"` || got[1] != `"Order"` {
		t.Errorf("QuoteAll = %q", got)
	}
}
