// Package pgident renders PostgreSQL identifiers for generated SQL.
//
// It exists so that every identifier m8 emits -- in dumped DDL, in GRANT
// targets, in CREATE SCHEMA, in the DROP DATABASE statements the temp-database
// sweeps issue -- goes through one function, instead of each call site
// concatenating strings and getting it right or not.
package pgident

import "strings"

// Quote double-quotes an identifier, escaping any embedded double quote.
//
// It quotes unconditionally, not only when the name "looks like it needs it".
// An unquoted identifier is case-folded by the server, so a table created as
// "Orders" is looked up as orders; it is rejected outright when the name is a
// reserved word, so a column named order or select cannot be dumped at all; and
// on the FOREIGN KEY path an unquoted, wrongly-cased name silently resolves to a
// different object rather than erroring.
//
// Quoting only when necessary means carrying PostgreSQL's keyword table and
// keeping it current across server versions -- pg_dump does exactly that, and
// it is not worth reproducing for output nothing hand-edits. It also matches
// what pg-schema-diff emits on the other side of the same pipeline
// (CREATE TABLE "public"."probe").
func Quote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// QuoteAll quotes each name in a list, for rendering a column list.
func QuoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = Quote(n)
	}
	return out
}

// Qualify renders a dotted, fully quoted name: Qualify("public", "orders") is
// `"public"."orders"`. Empty parts are skipped, so an unqualified name still
// comes back quoted.
func Qualify(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, Quote(p))
	}
	return strings.Join(out, ".")
}
