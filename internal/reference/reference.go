// Package reference reconciles reference/ files: tables whose rows are
// declared, the way schema/ declares their shape.
//
// A file names a table and holds one query whose result is the rows that table
// must contain:
//
//	-- m8:reference public.carrier key=code prune touch=updated_at
//	SELECT * FROM (VALUES
//	    ('UPS',   'United Parcel Service'),
//	    ('FEDEX', 'FedEx')
//	) AS v(code, name);
//
// Every apply loads the query into a temporary table typed like the target,
// compares it to the live rows by key, and writes only the difference: rows
// missing from the table are inserted, rows whose declared columns differ are
// updated, and -- with prune -- rows the file does not list are deleted.
// Columns the file does not list are never written, except the touch= columns,
// which are set to now() on every row m8 inserts or changes.
package reference

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ags-slc/m8/internal/parser"
	"github.com/ags-slc/m8/internal/pgident"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Spec is one reference/ file: the table, how its rows are matched, and the
// query that produces them.
type Spec struct {
	File  string
	Table string   // as written; resolved with to_regclass, so search_path applies
	Key   []string // empty means the primary key
	Prune bool
	Touch []string
	Query string
}

// Parse reads a reference/ file.
func Parse(file string, content []byte) (Spec, error) {
	spec := Spec{File: file}
	parsed, err := parser.Parse(content)
	if err != nil {
		return spec, fmt.Errorf("%s: %w", file, err)
	}
	switch len(parsed.Directives.Reference) {
	case 0:
		return spec, fmt.Errorf("%s: missing -- m8:reference <table> directive", file)
	case 1:
	default:
		return spec, fmt.Errorf("%s: more than one -- m8:reference directive", file)
	}
	fields := strings.Fields(parsed.Directives.Reference[0])
	if len(fields) == 0 {
		return spec, fmt.Errorf("%s: -- m8:reference names no table", file)
	}
	spec.Table = fields[0]
	for _, f := range fields[1:] {
		switch {
		case f == "prune":
			spec.Prune = true
		case strings.HasPrefix(f, "key="):
			spec.Key = strings.Split(strings.TrimPrefix(f, "key="), ",")
		case strings.HasPrefix(f, "touch="):
			spec.Touch = strings.Split(strings.TrimPrefix(f, "touch="), ",")
		default:
			return spec, fmt.Errorf("%s: unknown -- m8:reference option %q (expected key=, touch= or prune)", file, f)
		}
	}
	if slices.Contains(spec.Key, "") || slices.Contains(spec.Touch, "") {
		return spec, fmt.Errorf("%s: empty column name in -- m8:reference", file)
	}
	if len(parsed.Statements) != 1 {
		return spec, fmt.Errorf("%s: must hold exactly one SELECT or VALUES query, found %d statements", file, len(parsed.Statements))
	}
	spec.Query = parsed.Statements[0].SQL
	return spec, nil
}

// Row is one row rendered as text, in a fixed column order. nil is NULL.
type Row []*string

// Change is an update: the row's key, and the columns whose values differ.
type Change struct {
	Key      Row
	Columns  []string
	Old, New Row
}

// Diff is what reconciling one file finds.
type Diff struct {
	Spec    Spec
	Table   string   // the resolved table, as regclass renders it
	Key     []string // resolved: the primary key when the file names none
	Columns []string // the columns the query declares
	Inserts []Row    // the key columns, then the other declared columns
	Updates []Change
	Deletes []Row // key columns only
	// Pending is set when plan does not diff the file because this release's
	// schema/ changes its table: plan reads the database as it stands, ahead
	// of the schema phase, so the table it would check against is not the one
	// apply will write to. Apply never returns it.
	Pending string

	oid    uint32
	qname  string // the table, quoted for SQL
	temp   string // the temporary table holding the declared rows
	nonKey []string
	types  map[string]string // declared column -> type
}

// Empty reports whether applying the diff would write nothing.
func (d *Diff) Empty() bool {
	return d.Pending == "" && len(d.Inserts) == 0 && len(d.Updates) == 0 && len(d.Deletes) == 0
}

// Plan computes the diff for every spec inside tx, in the order the files must
// be applied in. It writes nothing but temporary tables.
//
// changing is set for m8 plan. It reports whether this release's schema/
// changes a table, by name; such a table is reported Pending instead of being
// diffed. Plan also runs the declared queries in a read-only transaction, so
// one that calls nextval() or setval() -- whose effects a rollback does not
// undo -- fails instead of moving a sequence.
func Plan(ctx context.Context, tx pgx.Tx, specs []Spec, changing func(rel string) bool) ([]*Diff, error) {
	diffs := make([]*Diff, 0, len(specs))
	seen := map[uint32]string{}
	for _, s := range specs {
		d := &Diff{Spec: s}
		if err := resolve(ctx, tx, d, changing); err != nil {
			return nil, fmt.Errorf("%s: %w", s.File, err)
		}
		if d.Pending == "" {
			if prev, ok := seen[d.oid]; ok {
				return nil, fmt.Errorf("%s: %s is already declared in %s", s.File, d.Table, prev)
			}
			seen[d.oid] = s.File
		}
		diffs = append(diffs, d)
	}
	diffs, err := fkOrder(ctx, tx, diffs)
	if err != nil {
		return nil, err
	}
	for i, d := range diffs {
		if d.Pending != "" {
			continue
		}
		d.temp = pgident.Qualify("pg_temp", fmt.Sprintf("m8_reference_%d", i))
		if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE TEMP TABLE %s ON COMMIT DROP AS SELECT %s FROM %s WITH NO DATA",
			d.temp, strings.Join(pgident.QuoteAll(d.Columns), ", "), d.qname)); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Spec.File, err)
		}
		if err := readsNoDeclaredTable(ctx, tx, d, i, seen); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Spec.File, err)
		}
	}
	// A transaction may turn read-only at any point; it is the other direction
	// PostgreSQL refuses. The temporary tables are created, and filling them
	// is a write a read-only transaction still allows.
	if changing != nil {
		if _, err := tx.Exec(ctx, "SET LOCAL transaction_read_only = on"); err != nil {
			return nil, err
		}
	}
	for _, d := range diffs {
		if d.Pending != "" {
			continue
		}
		if err := load(ctx, tx, d); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Spec.File, err)
		}
		if err := compute(ctx, tx, d); err != nil {
			return nil, fmt.Errorf("%s: %w", d.Spec.File, err)
		}
	}
	return diffs, nil
}

// Apply writes the diffs Plan computed, in the same transaction.
//
// No one order of writes suits every declaration. Re-keying a row whose other
// unique columns stay the same needs the old row deleted before the new one is
// inserted; moving child rows off a parent that is pruned needs the children
// updated before the parent is deleted. Apply writes deletes first, and if
// that violates a constraint, rolls back to a savepoint and writes them last.
//
// ponytail: two fixed orders; a declaration that needs both at once still
// fails, and would need the writes interleaved row by row.
func Apply(ctx context.Context, tx pgx.Tx, diffs []*Diff) error {
	if _, err := tx.Exec(ctx, "SAVEPOINT m8_reference"); err != nil {
		return err
	}
	first := write(ctx, tx, diffs, true)
	var pgErr *pgconn.PgError
	if !errors.As(first, &pgErr) || !strings.HasPrefix(pgErr.Code, "23") {
		return first
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT m8_reference"); err != nil {
		return err
	}
	if write(ctx, tx, diffs, false) != nil {
		return first
	}
	return nil
}

// write deletes pruned rows children first, and updates and inserts parents
// first, so neither direction leaves a foreign key pointing at nothing.
func write(ctx context.Context, tx pgx.Tx, diffs []*Diff, deletesFirst bool) error {
	deletes := func() error {
		for _, d := range slices.Backward(diffs) {
			if len(d.Deletes) == 0 {
				continue
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s t WHERE NOT EXISTS (SELECT 1 FROM %s r WHERE %s)",
				d.qname, d.temp, keyMatch("t", "r", d.Key))); err != nil {
				return fmt.Errorf("%s: %w", d.Spec.File, err)
			}
		}
		return nil
	}
	upserts := func() error {
		for _, d := range diffs {
			for _, sql := range upsertSQL(d) {
				if _, err := tx.Exec(ctx, sql); err != nil {
					return fmt.Errorf("%s: %w", d.Spec.File, err)
				}
			}
		}
		return nil
	}
	if deletesFirst {
		if err := deletes(); err != nil {
			return err
		}
		return upserts()
	}
	if err := upserts(); err != nil {
		return err
	}
	return deletes()
}

// upsertSQL updates the changed rows and inserts the missing ones, touching
// no other row: an unchanged row is neither locked nor written, so no trigger
// fires and no WAL or CDC event is produced for it.
//
// Changed rows get a plain UPDATE, never INSERT ... ON CONFLICT: the proposed
// insert row is checked before the conflict is found, so a NOT NULL column the
// file does not declare would fail the update of a row that already has it.
// Missing rows do go through ON CONFLICT on the key -- which resolve has
// checked is backed by a unique index -- so a row an application inserts
// between the diff and the write is updated rather than failing the apply.
func upsertSQL(d *Diff) []string {
	var out []string
	same := keyMatch("t", "r", d.Key)
	if len(d.Updates) > 0 {
		var set []string
		for _, c := range d.nonKey {
			set = append(set, fmt.Sprintf("%s = r.%s", pgident.Quote(c), pgident.Quote(c)))
		}
		for _, t := range d.Spec.Touch {
			set = append(set, pgident.Quote(t)+" = now()")
		}
		out = append(out, fmt.Sprintf("UPDATE %s AS t SET %s FROM %s r WHERE %s AND NOT %s",
			d.qname, strings.Join(set, ", "), d.temp, same, rowsEqual("t", "r", d.nonKey)))
	}
	if len(d.Inserts) > 0 {
		cols := pgident.QuoteAll(d.Columns)
		vals := qualified("r", d.Columns)
		var touch []string
		for _, t := range d.Spec.Touch {
			cols = append(cols, pgident.Quote(t))
			vals = append(vals, "now()")
			touch = append(touch, pgident.Quote(t)+" = now()")
		}
		conflict := "DO NOTHING"
		if len(d.nonKey) > 0 {
			var set []string
			for _, c := range d.nonKey {
				set = append(set, fmt.Sprintf("%s = excluded.%s", pgident.Quote(c), pgident.Quote(c)))
			}
			conflict = fmt.Sprintf("DO UPDATE SET %s WHERE NOT %s",
				strings.Join(append(set, touch...), ", "), rowsEqual("t", "excluded", d.nonKey))
		}
		out = append(out, fmt.Sprintf("INSERT INTO %s AS t (%s) SELECT %s FROM %s r WHERE NOT EXISTS (SELECT 1 FROM %s t WHERE %s) ON CONFLICT (%s) %s",
			d.qname, strings.Join(cols, ", "), strings.Join(vals, ", "), d.temp, d.qname, same,
			strings.Join(pgident.QuoteAll(d.Key), ", "), conflict))
	}
	return out
}

type column struct {
	typ            string
	generated      bool
	identityAlways bool
}

// resolve finds the table and checks the declaration against it: the columns
// exist and can be written, the key is backed by a unique index, and pruning
// cannot reach into another table.
func resolve(ctx context.Context, tx pgx.Tx, d *Diff, changing func(rel string) bool) error {
	var kind, nsp, rel string
	err := tx.QueryRow(ctx, `SELECT c.oid, c.oid::regclass::text, c.relkind::text, n.nspname::text, c.relname::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = to_regclass($1)`,
		d.Spec.Table).Scan(&d.oid, &d.Table, &kind, &nsp, &rel)
	found := err == nil
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	if !found {
		d.Table, rel = d.Spec.Table, relName(d.Spec.Table)
	}
	if changing != nil && changing(rel) {
		d.Pending = fmt.Sprintf("schema/ changes %s in this release", d.Table)
		return nil
	}
	if !found {
		return fmt.Errorf("table %s does not exist", d.Spec.Table)
	}
	if kind != "r" && kind != "p" {
		return fmt.Errorf("%s is not a table", d.Table)
	}
	d.qname = pgident.Qualify(nsp, rel)

	live, err := liveColumns(ctx, tx, d.oid)
	if err != nil {
		return err
	}
	if d.Columns, err = queryColumns(ctx, tx, d.Spec.Query); err != nil {
		return fmt.Errorf("the query failed: %w", err)
	}
	d.types = map[string]string{}
	for _, c := range d.Columns {
		lc, ok := live[c]
		switch {
		case !ok:
			return fmt.Errorf("column %s does not exist on %s", c, d.Table)
		case lc.generated:
			return fmt.Errorf("column %s is generated and cannot be declared", c)
		case lc.identityAlways:
			return fmt.Errorf("column %s is GENERATED ALWAYS AS IDENTITY, which refuses an explicit value; "+
				"leave it out of the query and match rows on another key=", c)
		case d.types[c] != "":
			return fmt.Errorf("the query names column %s twice", c)
		}
		d.types[c] = lc.typ
	}

	d.Key = d.Spec.Key
	if len(d.Key) == 0 {
		if d.Key, err = primaryKey(ctx, tx, d.oid); err != nil {
			return err
		}
		if len(d.Key) == 0 {
			return fmt.Errorf("%s has no primary key; name the key columns with key=", d.Table)
		}
	}
	for _, k := range d.Key {
		if d.types[k] == "" {
			return fmt.Errorf("key column %s is not one of the query's columns", k)
		}
	}
	for _, c := range d.Columns {
		if !slices.Contains(d.Key, c) {
			d.nonKey = append(d.nonKey, c)
		}
	}
	for _, t := range d.Spec.Touch {
		if _, ok := live[t]; !ok {
			return fmt.Errorf("touch column %s does not exist on %s", t, d.Table)
		}
		if d.types[t] != "" {
			return fmt.Errorf("touch column %s is also one of the query's columns", t)
		}
	}

	if err := keyIsUnique(ctx, tx, d); err != nil {
		return err
	}
	if d.Spec.Prune {
		return pruneIsContained(ctx, tx, d)
	}
	return nil
}

// liveColumns reads the table's columns, each with the type load casts its
// declared values to: the column's type without its modifier, which the
// assignment into the temporary table then applies. char and bit are the
// exceptions -- written without a length they mean length 1, and an explicit
// cast truncates to it -- so they are cast to the unbounded bpchar and varbit.
func liveColumns(ctx context.Context, tx pgx.Tx, oid uint32) (map[string]column, error) {
	rows, err := tx.Query(ctx, `SELECT a.attname::text,
		    CASE t.typname WHEN 'bpchar' THEN 'bpchar' WHEN '_bpchar' THEN 'bpchar[]'
		                   WHEN 'bit' THEN 'varbit' WHEN '_bit' THEN 'varbit[]'
		                   ELSE format_type(a.atttypid, NULL) END,
		    a.attgenerated <> '', a.attidentity = 'a'
		FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped`, oid)
	if err != nil {
		return nil, err
	}
	live := map[string]column{}
	for rows.Next() {
		var name string
		var c column
		if err := rows.Scan(&name, &c.typ, &c.generated, &c.identityAlways); err != nil {
			return nil, err
		}
		live[name] = c
	}
	return live, rows.Err()
}

// relName is the relation part of a table name as a file writes it, folded the
// way PostgreSQL folds an unquoted identifier.
//
// ponytail: splits on the last dot, so a quoted name containing one is misread.
func relName(table string) string {
	name := table[strings.LastIndex(table, ".")+1:]
	if unquoted, ok := strings.CutPrefix(name, `"`); ok {
		return strings.ReplaceAll(strings.TrimSuffix(unquoted, `"`), `""`, `"`)
	}
	return strings.ToLower(name)
}

// readsNoDeclaredTable refuses a query that reads a table another file -- or
// this one -- declares. Every query is loaded before anything is written, so
// such a read sees that table's rows from before this apply: a row resolved
// through a join to a parent the same release adds or re-keys is missing, and
// prune deletes it. A temporary view of the query lists, in pg_depend, the
// tables it reads.
func readsNoDeclaredTable(ctx context.Context, tx pgx.Tx, d *Diff, i int, declared map[uint32]string) error {
	view := pgident.Qualify("pg_temp", fmt.Sprintf("m8_reference_query_%d", i))
	if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE TEMP VIEW %s AS\n%s\n", view, d.Spec.Query)); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT dep.refobjid, dep.refobjid::regclass::text
		FROM pg_rewrite r JOIN pg_depend dep ON dep.classid = 'pg_rewrite'::regclass AND dep.objid = r.oid
		WHERE r.ev_class = $1::regclass AND dep.refclassid = 'pg_class'::regclass AND dep.refobjid <> r.ev_class`, view)
	if err != nil {
		return err
	}
	var oid uint32
	var name, file string
	for rows.Next() {
		if err := rows.Scan(&oid, &name); err != nil {
			return err
		}
		if f, ok := declared[oid]; ok {
			file = f
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// A view cannot be ON COMMIT DROP, and apply commits.
	if _, err := tx.Exec(ctx, "DROP VIEW "+view); err != nil {
		return err
	}
	if file != "" {
		return fmt.Errorf("the query reads %s, which %s declares; it would see that table's rows from before this apply. "+
			"Write the values themselves rather than reading them from another reference/ table", name, file)
	}
	return nil
}

// queryColumns reads the names of the columns the query returns, without
// fetching a row.
func queryColumns(ctx context.Context, tx pgx.Tx, query string) ([]string, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf("SELECT * FROM (\n%s\n) q LIMIT 0", query))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range rows.FieldDescriptions() {
		names = append(names, f.Name)
	}
	rows.Close()
	return names, rows.Err()
}

func primaryKey(ctx context.Context, tx pgx.Tx, oid uint32) ([]string, error) {
	// indkey also lists INCLUDE columns, after the first indnkeyatts.
	rows, err := tx.Query(ctx, `SELECT a.attname::text FROM pg_index i
		CROSS JOIN LATERAL unnest((i.indkey::int2[])[0:i.indnkeyatts - 1]) WITH ORDINALITY k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = $1 AND i.indisprimary
		ORDER BY k.ord`, oid)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// keyIsUnique requires an index ON CONFLICT can use as its arbiter: unique,
// valid, not deferrable, not partial, on exactly the key columns.
func keyIsUnique(ctx context.Context, tx pgx.Tx, d *Diff) error {
	var unique bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_index i
		WHERE i.indrelid = $1 AND i.indisunique AND i.indisvalid AND i.indimmediate
		  AND i.indpred IS NULL AND i.indexprs IS NULL
		  AND (SELECT array_agg(a.attname::text)
		       FROM unnest((i.indkey::int2[])[0:i.indnkeyatts - 1]) k(attnum)
		       JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum)
		      <@ $2::text[]
		  AND i.indnkeyatts = cardinality($2::text[]))`, d.oid, d.Key).Scan(&unique); err != nil {
		return err
	}
	if !unique {
		return fmt.Errorf("no unique index on %s covers exactly the key (%s); without one a key could match several rows",
			d.Table, strings.Join(d.Key, ", "))
	}
	return nil
}

// pruneIsContained refuses prune on a table another table's foreign key would
// carry the delete into.
func pruneIsContained(ctx context.Context, tx pgx.Tx, d *Diff) error {
	var con, from, action string
	err := tx.QueryRow(ctx, `SELECT conname::text, conrelid::regclass::text, confdeltype::text FROM pg_constraint
		WHERE contype = 'f' AND confrelid = $1 AND conparentid = 0 AND confdeltype IN ('c', 'n', 'd')
		ORDER BY conname LIMIT 1`, d.oid).Scan(&con, &from, &action)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	verb := map[string]string{"c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}[action]
	return fmt.Errorf("prune would delete through foreign key %s on %s (ON DELETE %s) and change rows in a table this file does not declare; "+
		"drop prune, or make the foreign key RESTRICT or NO ACTION", con, from, verb)
}

// fkOrder sorts diffs so a table comes after every other declared table it has
// a foreign key to, keeping file order otherwise. Tables still pending have no
// catalog entry to read, and keep their place.
func fkOrder(ctx context.Context, tx pgx.Tx, diffs []*Diff) ([]*Diff, error) {
	var oids []uint32
	for _, d := range diffs {
		if d.Pending == "" {
			oids = append(oids, d.oid)
		}
	}
	if len(oids) < 2 {
		return diffs, nil
	}
	rows, err := tx.Query(ctx, `SELECT conrelid, confrelid FROM pg_constraint
		WHERE contype = 'f' AND conparentid = 0 AND conrelid <> confrelid
		  AND conrelid = ANY ($1) AND confrelid = ANY ($1)`, oids)
	if err != nil {
		return nil, err
	}
	parents := map[uint32][]uint32{}
	for rows.Next() {
		var child, parent uint32
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, err
		}
		parents[child] = append(parents[child], parent)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	placed := map[uint32]bool{}
	remaining := slices.Clone(diffs)
	sorted := make([]*Diff, 0, len(diffs))
	for len(remaining) > 0 {
		before := len(remaining)
		remaining = slices.DeleteFunc(remaining, func(d *Diff) bool {
			if slices.ContainsFunc(parents[d.oid], func(p uint32) bool { return !placed[p] }) {
				return false
			}
			placed[d.oid] = true
			sorted = append(sorted, d)
			return true
		})
		if len(remaining) == before {
			var names []string
			for _, d := range remaining {
				names = append(names, d.Table)
			}
			return nil, fmt.Errorf("reference tables reference each other in a cycle (%s); m8 cannot order their writes",
				strings.Join(names, ", "))
		}
	}
	return sorted, nil
}

// load runs the query into the temporary table, which has the target's column
// types. Each value is cast to its column's type without the type modifier and
// then assigned, which is what an INSERT does: varchar(n) refuses a value too
// long rather than truncating it, and numeric(p,s) rounds the way the table
// would.
func load(ctx context.Context, tx pgx.Tx, d *Diff) error {
	cols := strings.Join(pgident.QuoteAll(d.Columns), ", ")
	var casts []string
	for _, c := range d.Columns {
		casts = append(casts, fmt.Sprintf("q.%s::%s", pgident.Quote(c), d.types[c]))
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM (\n%s\n) q",
		d.temp, cols, strings.Join(casts, ", "), d.Spec.Query)); err != nil {
		return fmt.Errorf("loading the declared rows: %w", err)
	}

	var nulls []string
	for _, k := range d.Key {
		nulls = append(nulls, pgident.Quote(k)+" IS NULL")
	}
	key, err := queryRows(ctx, tx, fmt.Sprintf("SELECT %s FROM %s r WHERE %s LIMIT 1",
		textCols("r", d.Key), d.temp, strings.Join(nulls, " OR ")))
	if err != nil {
		return err
	}
	if len(key) > 0 {
		return fmt.Errorf("a row declares a NULL key %s; a NULL key never matches a live row", formatRow(key[0]))
	}
	dups, err := queryRows(ctx, tx, fmt.Sprintf("SELECT %s FROM %s r GROUP BY %s HAVING count(*) > 1 ORDER BY 1 LIMIT 5",
		textCols("r", d.Key), d.temp, joined("r", d.Key)))
	if err != nil {
		return err
	}
	if len(dups) > 0 {
		var keys []string
		for _, k := range dups {
			keys = append(keys, formatRow(k))
		}
		return fmt.Errorf("key (%s) is declared more than once: %s", strings.Join(d.Key, ", "), strings.Join(keys, ", "))
	}
	return nil
}

// compute reads the difference between the declared rows and the live ones.
func compute(ctx context.Context, tx pgx.Tx, d *Diff) error {
	match := keyMatch("t", "r", d.Key)
	var err error
	d.Inserts, err = queryRows(ctx, tx, fmt.Sprintf("SELECT %s FROM %s r WHERE NOT EXISTS (SELECT 1 FROM %s t WHERE %s) ORDER BY %s",
		textCols("r", slices.Concat(d.Key, d.nonKey)), d.temp, d.qname, match, joined("r", d.Key)))
	if err != nil {
		return err
	}
	if len(d.nonKey) > 0 {
		rows, err := queryRows(ctx, tx, fmt.Sprintf("SELECT %s, %s, %s FROM %s r JOIN %s t ON %s WHERE NOT %s ORDER BY %s",
			textCols("r", d.Key), textCols("t", d.nonKey), textCols("r", d.nonKey), d.temp, d.qname, match,
			rowsEqual("t", "r", d.nonKey), joined("r", d.Key)))
		if err != nil {
			return err
		}
		k, n := len(d.Key), len(d.nonKey)
		for _, r := range rows {
			c := Change{Key: r[:k]}
			for i, col := range d.nonKey {
				o, nw := r[k+i], r[k+n+i]
				if (o == nil) != (nw == nil) || o != nil && *o != *nw {
					c.Columns = append(c.Columns, col)
					c.Old = append(c.Old, o)
					c.New = append(c.New, nw)
				}
			}
			d.Updates = append(d.Updates, c)
		}
	}
	if d.Spec.Prune {
		d.Deletes, err = queryRows(ctx, tx, fmt.Sprintf("SELECT %s FROM %s t WHERE NOT EXISTS (SELECT 1 FROM %s r WHERE %s) ORDER BY %s",
			textCols("t", d.Key), d.qname, d.temp, match, joined("t", d.Key)))
	}
	return err
}

func queryRows(ctx context.Context, tx pgx.Tx, sql string) ([]Row, error) {
	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Row, error) {
		row := make(Row, len(r.FieldDescriptions()))
		ptrs := make([]any, len(row))
		for i := range row {
			ptrs[i] = &row[i]
		}
		return row, r.Scan(ptrs...)
	})
}

func qualified(alias string, cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = pgident.Qualify(alias, c)
	}
	return out
}

func joined(alias string, cols []string) string { return strings.Join(qualified(alias, cols), ", ") }

func textCols(alias string, cols []string) string {
	out := qualified(alias, cols)
	for i := range out {
		out[i] += "::text"
	}
	return strings.Join(out, ", ")
}

func keyMatch(a, b string, key []string) string {
	var out []string
	for _, k := range key {
		out = append(out, pgident.Qualify(a, k)+" = "+pgident.Qualify(b, k))
	}
	return strings.Join(out, " AND ")
}

// rowsEqual compares columns NULL-safely: two NULLs are equal.
func rowsEqual(a, b string, cols []string) string {
	return fmt.Sprintf("ROW(%s) IS NOT DISTINCT FROM ROW(%s)", joined(a, cols), joined(b, cols))
}

// formatValue renders a value as a SQL literal, so a plan line reads unambiguously.
func formatValue(v *string) string {
	if v == nil {
		return "NULL"
	}
	return "'" + strings.ReplaceAll(*v, "'", "''") + "'"
}

func formatRow(r Row) string {
	out := make([]string, len(r))
	for i, v := range r {
		out[i] = formatValue(v)
	}
	return "(" + strings.Join(out, ", ") + ")"
}

// Lines renders the diff as plan output, at most limit lines per kind of change.
func (d *Diff) Lines(limit int) []string {
	var out []string
	add := func(n int, line func(i int) string) {
		for i := 0; i < n && i < limit; i++ {
			out = append(out, line(i))
		}
		if n > limit {
			out = append(out, fmt.Sprintf("  … and %d more", n-limit))
		}
	}
	k := len(d.Key)
	add(len(d.Inserts), func(i int) string {
		r := d.Inserts[i]
		line := "+ " + formatRow(r[:k])
		for j, c := range d.nonKey {
			line += " " + c + "=" + formatValue(r[k+j])
		}
		return line
	})
	add(len(d.Updates), func(i int) string {
		c := d.Updates[i]
		var parts []string
		for j, col := range c.Columns {
			parts = append(parts, fmt.Sprintf("%s: %s → %s", col, formatValue(c.Old[j]), formatValue(c.New[j])))
		}
		return "~ " + formatRow(c.Key) + " " + strings.Join(parts, ", ")
	})
	add(len(d.Deletes), func(i int) string { return "- " + formatRow(d.Deletes[i]) })
	return out
}
