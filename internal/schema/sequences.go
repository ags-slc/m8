package schema

import (
	"fmt"
	"regexp"
	"strings"
)

// pg-schema-diff does not manage sequences. When the desired state declares a
// SERIAL/BIGSERIAL column, the temp database it builds to parse that DDL ends up
// with a real sequence — but introspection reports the column only as
// "bigint ... DEFAULT nextval('t_id_seq'::regclass)", and the CREATE TABLE it
// generates for the target carries that default with no sequence behind it. The
// statement then fails on apply with 42P01 (relation does not exist).
//
// Rather than drop SERIAL support, m8 restores the sequences the generated DDL
// depends on: one CREATE SEQUENCE before the table, and an ALTER SEQUENCE ...
// OWNED BY after it so the sequence is dropped with the column, exactly as
// SERIAL would have arranged.

var (
	// Captures the target of CREATE TABLE, e.g. "public"."widget" or widget.
	createTableTargetRe = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?((?:"[^"]+"|[\w$]+)(?:\s*\.\s*(?:"[^"]+"|[\w$]+))?)`)
	// Captures a column definition that defaults to nextval on a sequence:
	// group 1 = column name, group 2 = sequence literal as written in the default.
	// Anchored on the "(" that opens the column list or a "," between columns,
	// so the CREATE TABLE keywords themselves can never be read as a column name.
	nextvalColumnRe = regexp.MustCompile(`(?is)[(,]\s*("[^"]+"|[\w$]+)\s+[^,()]*(?:\([^)]*\))?[^,()]*?\bDEFAULT\s+nextval\(\s*'([^']+)'(?:::regclass)?\s*\)`)
)

// sequenceFixup is one column that needs a sequence created for it.
type sequenceFixup struct {
	Sequence string // sequence name exactly as the DEFAULT references it
	Table    string // table the column belongs to, as written in CREATE TABLE
	Column   string // column name, quoted as written
}

// findSequenceFixups returns the sequences a CREATE TABLE statement depends on
// but does not create. It returns nil for any other statement.
func findSequenceFixups(ddl string) []sequenceFixup {
	m := createTableTargetRe.FindStringSubmatch(ddl)
	if m == nil {
		return nil
	}
	table := strings.Join(strings.Fields(m[1]), "")

	var out []sequenceFixup
	for _, c := range nextvalColumnRe.FindAllStringSubmatch(ddl, -1) {
		out = append(out, sequenceFixup{
			Sequence: quoteSequenceRef(c[2]),
			Table:    table,
			Column:   c[1],
		})
	}
	return out
}

// quoteSequenceRef renders a sequence reference taken from a nextval() literal
// as an identifier safe to use in DDL. Postgres writes the literal already
// quoted where quoting is needed (e.g. public."odd name_seq"), so a reference
// that carries a quote is passed through untouched; a bare one is requoted
// part-by-part.
func quoteSequenceRef(ref string) string {
	if strings.Contains(ref, `"`) {
		return ref
	}
	parts := strings.Split(ref, ".")
	for i, p := range parts {
		parts[i] = quoteIdent(p)
	}
	return strings.Join(parts, ".")
}

// sequenceStatements renders the DDL that must bracket a CREATE TABLE carrying
// nextval defaults: the CREATE SEQUENCEs that precede it, and the ownership
// links that follow it.
func sequenceStatements(fixups []sequenceFixup) (before, after []DiffStatement) {
	seen := make(map[string]bool)
	for _, f := range fixups {
		if !seen[f.Sequence] {
			seen[f.Sequence] = true
			before = append(before, DiffStatement{
				DDL: fmt.Sprintf("CREATE SEQUENCE IF NOT EXISTS %s AS bigint", f.Sequence),
			})
		}
		after = append(after, DiffStatement{
			DDL: fmt.Sprintf("ALTER SEQUENCE %s OWNED BY %s.%s", f.Sequence, f.Table, f.Column),
		})
	}
	return before, after
}
