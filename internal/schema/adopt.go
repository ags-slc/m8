package schema

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/ags-slc/m8/internal/parser"
	"github.com/ags-slc/m8/internal/pgident"
)

// Partitions that exist in the live database but are not declared are someone
// else's to create and drop -- pg_partman's run_maintenance, an application job,
// a hand-run script. A file that declares a partitioned table declares its
// shape, not the set of children that happens to exist this month.
//
// pg-schema-diff cannot be told that. It reads every child, finds none of them
// in the desired state, and gives up: dropping a partition while keeping its
// parent is not implemented. The plan fails before m8's undeclared-object filter
// ever sees a statement, so that filter cannot help.
//
// adoptPartitions closes the gap from the other side: it describes each live,
// undeclared child in the desired state exactly as it is, so the diff sees
// nothing to do for it. That is not the same as hiding it. Because the children
// are visible to both sides, an index added to the parent is planned the way
// pg-schema-diff plans one on a partitioned table -- an empty index ON ONLY the
// parent, CREATE INDEX CONCURRENTLY on each child, then ATTACH -- rather than as
// a bare ON ONLY index that would be left invalid, built on no existing child.
//
// The children are read from the live database on every run, never written to a
// file: a list captured once is wrong the first time the partition manager adds
// a month.
//
// Each child is rebuilt the way pg_partman 5 builds one: a table LIKE the
// parent, given the child's own NOT NULLs, constraints and indexes under their
// live names, then attached. (Not its column defaults: pg-schema-diff does not
// compare those on a partition.) Attaching reuses an index that matches
// one of the parent's instead of creating it, so the rebuilt child's indexes
// carry the names the live child's do -- a child built with CREATE TABLE ...
// PARTITION OF would get the names Postgres generates instead, and a partition
// whose indexes were named any other way could not be validated at all. After
// attaching, the child gets the attributes a partition does not inherit:
// replica identity, row-level-security flags and grants. Leaving any of these
// out would read as a change to the child -- and under --strict, which filters
// nothing, the plan would make it: drop a child-only index, reset REPLICA
// IDENTITY FULL on a partition logical replication reads from.
//
// What the rebuild cannot carry is refused rather than left to fail inside the
// diff: a child with a CHECK constraint of its own, and a child that is itself
// partitioned. pg-schema-diff implements neither on a partition.
func adoptPartitions(ctx context.Context, liveDB *sql.DB, targetSchema string, desiredDDL []string) (adopted, error) {
	declared := declaredTables(desiredDDL)
	q := partitionQueries{ctx: ctx, db: liveDB, schema: targetSchema}

	// Checked against every live partitioned table, not only those with
	// children: an empty one declared plain is planned DROP and CREATE too.
	parents, err := q.names(`
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'p' AND NOT c.relispartition`)
	if err != nil {
		return adopted{}, err
	}
	for _, p := range parents {
		if partitioned, ok := declared[p]; ok && !partitioned {
			return adopted{}, partitionedDeclaredPlain(targetSchema, p)
		}
	}

	kids, err := q.children()
	if err != nil {
		return adopted{}, err
	}
	var undeclared []child
	for _, k := range kids {
		if _, parentDeclared := declared[k.parent]; !parentDeclared {
			continue
		}
		if _, childDeclared := declared[k.name]; childDeclared {
			continue
		}
		if k.partitioned {
			return adopted{}, fmt.Errorf("%s.%s is a partition of %s.%s and is itself partitioned; "+
				"pg-schema-diff cannot diff a partitioned partition, so its partitions cannot be adopted",
				targetSchema, k.name, targetSchema, k.parent)
		}
		undeclared = append(undeclared, k)
	}
	if len(undeclared) == 0 {
		return adopted{}, nil
	}

	checks, err := q.pairs(`SELECT k.relname, con.conname FROM kids k
		JOIN pg_constraint con ON con.conrelid = k.oid
		WHERE con.contype = 'c' AND con.conislocal ORDER BY 1, 2`)
	if err != nil {
		return adopted{}, err
	}
	notNulls, err := q.pairs(`SELECT k.relname, a.attname FROM kids k
		JOIN pg_attribute a ON a.attrelid = k.oid AND a.attnum > 0 AND NOT a.attisdropped AND a.attnotnull
		JOIN pg_attribute pa ON pa.attrelid = k.parent_oid AND pa.attname = a.attname AND NOT pa.attnotnull
		ORDER BY 1, a.attnum`)
	if err != nil {
		return adopted{}, err
	}
	constraints, err := q.pairs(`SELECT k.relname, format('ADD CONSTRAINT %I %s', con.conname, pg_get_constraintdef(con.oid))
		FROM kids k JOIN pg_constraint con ON con.conrelid = k.oid
		WHERE con.contype IN ('p', 'u', 'x') ORDER BY 1, con.conname`)
	if err != nil {
		return adopted{}, err
	}
	indexes, err := q.pairs(`SELECT k.relname, pg_get_indexdef(x.indexrelid) FROM kids k
		JOIN pg_index x ON x.indrelid = k.oid
		JOIN pg_class ic ON ic.oid = x.indexrelid
		WHERE NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = x.indexrelid AND con.conrelid = k.oid)
		ORDER BY 1, ic.relname`)
	if err != nil {
		return adopted{}, err
	}
	grants, err := q.pairs(`SELECT k.relname,
		       format('%s ON %I.%I TO %s%s', a.privilege_type, $1::text, k.relname,
		              CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE '"' || replace(r.rolname, '"', '""') || '"' END,
		              CASE WHEN a.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END)
		FROM kids k JOIN pg_class c ON c.oid = k.oid
		CROSS JOIN LATERAL aclexplode(c.relacl) a
		LEFT JOIN pg_roles r ON r.oid = a.grantee
		WHERE a.grantee <> c.relowner
		ORDER BY 1, a.privilege_type, a.grantee`)
	if err != nil {
		return adopted{}, err
	}

	out := adopted{children: map[string]bool{}}
	var ddl []string
	for _, k := range undeclared {
		if c := checks[k.name]; len(c) > 0 {
			return adopted{}, fmt.Errorf("%s.%s, a partition of %s.%s, has a CHECK constraint of its own (%s); "+
				"pg-schema-diff cannot diff check constraints on a partition, so the partition cannot be adopted",
				targetSchema, k.name, targetSchema, k.parent, c[0])
		}
		out.children[k.name] = true
		qChild, qParent := pgident.Qualify(targetSchema, k.name), pgident.Qualify(targetSchema, k.parent)

		ddl = append(ddl, fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING GENERATED);", qChild, qParent))
		for _, col := range notNulls[k.name] {
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL;", qChild, pgident.Quote(col)))
		}
		for _, c := range constraints[k.name] {
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s %s;", qChild, c))
		}
		for _, idx := range indexes[k.name] {
			ddl = append(ddl, idx+";")
		}
		ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s ATTACH PARTITION %s %s;", qParent, qChild, k.bound))

		switch k.replIdent {
		case "f":
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s REPLICA IDENTITY FULL;", qChild))
		case "n":
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s REPLICA IDENTITY NOTHING;", qChild))
		case "i":
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s REPLICA IDENTITY USING INDEX %s;", qChild, pgident.Quote(k.replIndex)))
		}
		if k.rls {
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY;", qChild))
		}
		if k.forceRLS {
			ddl = append(ddl, fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY;", qChild))
		}
		for _, g := range grants[k.name] {
			ddl = append(ddl, "GRANT "+g+";")
		}
	}

	// One string: pg-schema-diff replays the desired state one element at a
	// time, and a folder with hundreds of children would otherwise cost a round
	// trip per statement, on every temp database a diff creates.
	out.ddl = strings.Join(ddl, "\n")
	return out, nil
}

// adopted is what adoptPartitions adds to the desired state: the DDL, and the
// names of the children it describes.
type adopted struct {
	ddl      string
	children map[string]bool
}

type child struct {
	parent, name, bound  string
	partitioned          bool
	replIdent, replIndex string
	rls, forceRLS        bool
}

// partitionQueries reads the live partitions of one schema's partitioned
// tables.
type partitionQueries struct {
	ctx    context.Context
	db     *sql.DB
	schema string
}

// kidsCTE is the plain-table children of the schema's top-level partitioned
// tables, the set the per-attribute queries in pairs run over.
const kidsCTE = `WITH kids AS (
	SELECT c.oid, c.relname, p.oid AS parent_oid
	FROM pg_inherits i
	JOIN pg_class c ON c.oid = i.inhrelid
	JOIN pg_class p ON p.oid = i.inhparent
	JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = $1 AND p.relnamespace = c.relnamespace
	  AND p.relkind = 'p' AND NOT p.relispartition AND c.relkind = 'r'
) `

func (q partitionQueries) children() ([]child, error) {
	rows, err := q.db.QueryContext(q.ctx, `
		SELECT p.relname, c.relname, pg_get_expr(c.relpartbound, c.oid), c.relkind = 'p',
		       c.relreplident::text, coalesce(ri.relname, ''), c.relrowsecurity, c.relforcerowsecurity
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_index x ON x.indrelid = c.oid AND x.indisreplident
		LEFT JOIN pg_class ri ON ri.oid = x.indexrelid
		WHERE n.nspname = $1 AND p.relnamespace = c.relnamespace
		  AND p.relkind = 'p' AND NOT p.relispartition
		ORDER BY p.relname, c.relname`, q.schema)
	if err != nil {
		return nil, fmt.Errorf("reading live partitions of %s: %w", q.schema, err)
	}
	defer func() { _ = rows.Close() }()
	var out []child
	for rows.Next() {
		var k child
		if err := rows.Scan(&k.parent, &k.name, &k.bound, &k.partitioned, &k.replIdent, &k.replIndex, &k.rls, &k.forceRLS); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (q partitionQueries) names(query string) ([]string, error) {
	rows, err := q.db.QueryContext(q.ctx, query, q.schema)
	if err != nil {
		return nil, fmt.Errorf("reading live partitioned tables of %s: %w", q.schema, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// pairs runs a (child name, value) query over kidsCTE and groups the values by
// child, in row order.
func (q partitionQueries) pairs(query string) (map[string][]string, error) {
	rows, err := q.db.QueryContext(q.ctx, kidsCTE+query, q.schema)
	if err != nil {
		return nil, fmt.Errorf("reading live partitions of %s: %w", q.schema, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = append(out[k], v)
	}
	return out, rows.Err()
}

var (
	// createTableNameRe captures the table a CREATE TABLE statement names: a
	// quoted identifier (group 1, "" escaping a quote) or a bare one (group 2),
	// after an optional schema. The parser keeps the comments that lead a
	// statement -- an m8:partman directive, typically -- so they are skipped.
	createTableNameRe = regexp.MustCompile(`(?is)^(?:\s*(?:--[^\n]*(?:\n|$)|/\*.*?\*/))*\s*CREATE\s+(?:(?:GLOBAL|LOCAL)\s+)?(?:(?:TEMP|TEMPORARY|UNLOGGED)\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?` +
		`(?:(?:"(?:[^"]|"")+"|[A-Za-z_][\w$]*)\s*\.\s*)?(?:"((?:[^"]|"")+)"|([A-Za-z_][\w$]*))`)
	partitionByRe = regexp.MustCompile(`(?is)\bPARTITION\s+BY\b`)
)

// declaredTables maps each table the files declare, by the name Postgres gives
// it -- a quoted identifier as written, a bare one folded to lower case -- to
// whether it is declared partitioned.
func declaredTables(desiredDDL []string) map[string]bool {
	tables := map[string]bool{}
	for _, file := range desiredDDL {
		res, err := parser.Parse([]byte(file))
		if err != nil {
			continue
		}
		for _, st := range res.Statements {
			m := createTableNameRe.FindStringSubmatch(st.SQL)
			if m == nil {
				continue
			}
			name := strings.ReplaceAll(m[1], `""`, `"`)
			if m[1] == "" {
				name = strings.ToLower(m[2])
			}
			tables[name] = partitionByRe.MatchString(st.SQL)
		}
	}
	return tables
}

// partitionedDeclaredPlain refuses a table that is partitioned in the database
// but declared as a plain one.
//
// That declaration does not describe a smaller change; it describes a
// different table, and pg-schema-diff plans it as one: DROP TABLE, then CREATE
// TABLE, flagged DELETES_DATA. It is also exactly what a hand conversion from
// Flyway, or an `m8 dump` from before dump rendered PARTITION BY, produces -- so
// the first plan run against a converted database must not be the one that
// finds out.
func partitionedDeclaredPlain(targetSchema, table string) error {
	return fmt.Errorf("%s.%s is partitioned in the database but declared without PARTITION BY; "+
		"as declared, the plan would drop and recreate it. Add its partition key to the CREATE TABLE "+
		"(SELECT pg_get_partkeydef('%s.%s'::regclass) prints it)", targetSchema, table, targetSchema, table)
}

// recreated returns the first adopted child a plan would create, or "".
//
// An adopted child exists by definition, so a plan that creates one was
// computed from two different moments: adoptPartitions read the child, and by
// the time pg-schema-diff read the live schema it was gone -- retired by
// partition maintenance in between. Applied, that plan would bring a dropped
// partition back. CREATE statements always pass the undeclared-object filter,
// so it has to be caught here.
func (a adopted) recreated(targetSchema string, ddls []string) string {
	for _, ddl := range ddls {
		for name := range a.children {
			prefix := "CREATE TABLE " + pgident.Qualify(targetSchema, name)
			if rest, ok := strings.CutPrefix(ddl, prefix); ok && (rest == "" || rest[0] == ' ' || rest[0] == '(') {
				return name
			}
		}
	}
	return ""
}
