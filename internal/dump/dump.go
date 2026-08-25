package dump

import (
	"context"
	"fmt"
	"strings"

	"github.com/ags-slc/m8/internal/pgident"
	"github.com/jackc/pgx/v5"
)

// Table holds the metadata needed to generate a CREATE TABLE statement.
type Table struct {
	Schema  string
	Name    string
	Columns []Column
	PK      *PrimaryKey
	Uniques []UniqueConstraint
	Checks  []CheckConstraint
	FKs     []ForeignKey
	Indexes []Index
}

// QualifiedName returns the schema-qualified, quoted table name, e.g.
// `"materialized"."rpt_invoice_detail"`.
func (t *Table) QualifiedName() string {
	return pgident.Qualify(t.Schema, t.Name)
}

// Column represents a table column.
type Column struct {
	Name         string
	DataType     string
	Nullable     bool
	Default      *string
	IsIdentity   bool
	IdentityKind string // "ALWAYS" or "BY DEFAULT"
	// Generated holds the expression of a GENERATED ALWAYS AS (...) STORED
	// column. Postgres stores that expression in pg_attrdef alongside ordinary
	// defaults, but it is not a default: it references sibling columns, which a
	// DEFAULT may not do. Emitting one as a DEFAULT produces DDL Postgres
	// rejects with 0A000.
	Generated *string
}

// PrimaryKey represents a primary key constraint.
type PrimaryKey struct {
	Name    string
	Columns []string
}

// UniqueConstraint represents a UNIQUE constraint.
type UniqueConstraint struct {
	Name    string
	Columns []string
}

// CheckConstraint represents a CHECK constraint.
type CheckConstraint struct {
	Name       string
	Expression string
}

// ForeignKey represents a foreign key constraint.
type ForeignKey struct {
	Name       string
	Columns    []string
	RefSchema  string
	RefTable   string
	RefColumns []string
	OnDelete   string
	OnUpdate   string
}

// Index represents a non-constraint index.
type Index struct {
	Name       string
	Unique     bool
	Columns    string // raw column expression (may include expressions, ORDER BY, etc.)
	Definition string // full CREATE INDEX statement from pg_indexes
}

// Dumper generates CREATE TABLE DDL from a live database.
type Dumper struct {
	conn *pgx.Conn
	// AllowUnsupported downgrades a refusal to dump objects m8 cannot represent
	// -- materialized views -- into a silent skip. Off by default: a baseline
	// that quietly omits objects is worse than one that will not be produced,
	// because nothing downstream can tell the difference between "this database
	// has no materialized views" and "m8 did not look".
	AllowUnsupported bool
}

// NewDumper creates a new Dumper.
func NewDumper(conn *pgx.Conn) *Dumper {
	return &Dumper{conn: conn}
}

// ListSchemas returns all user-created schemas (excludes pg_*, information_schema, _m8, _peerdb_internal).
func (d *Dumper) ListSchemas(ctx context.Context) ([]string, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT nspname FROM pg_namespace
		WHERE nspname NOT LIKE 'pg_%'
		  AND nspname NOT IN ('information_schema', '_m8', '_peerdb_internal')
		ORDER BY nspname
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		schemas = append(schemas, s)
	}
	return schemas, rows.Err()
}

// ListTables returns all regular tables in a schema (excludes partitions, foreign tables, temp tables).
//
// Extension-owned tables are excluded, as ListFunctions and ListViews already
// exclude extension-owned routines and views. They are recreated by CREATE
// EXTENSION, not by a migration file -- and pg-schema-diff excludes them from
// its own introspection, so a schema/ file describing one is a table the differ
// cannot see on the live side. It reads as "must create" on every run and the
// generated CREATE TABLE fails on apply with 42P07 (relation already exists).
func (d *Dumper) ListTables(ctx context.Context, schema string) ([]string, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p')  -- regular tables and partitioned tables
		  AND NOT c.relispartition      -- exclude child partitions
		  AND NOT EXISTS (
			  SELECT 1 FROM pg_depend d
			  WHERE d.objid = c.oid
			    AND d.classid = 'pg_class'::regclass
			    AND d.deptype = 'e'     -- installed by an extension
		  )
		ORDER BY c.relname
	`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
}

// DumpTable generates a CREATE TABLE statement for a single table.
func (d *Dumper) DumpTable(ctx context.Context, schema, table string) (*Table, error) {
	t := &Table{Schema: schema, Name: table}

	if err := d.loadColumns(ctx, t); err != nil {
		return nil, fmt.Errorf("columns for %s.%s: %w", schema, table, err)
	}
	if err := d.loadPrimaryKey(ctx, t); err != nil {
		return nil, fmt.Errorf("pk for %s.%s: %w", schema, table, err)
	}
	if err := d.loadUniqueConstraints(ctx, t); err != nil {
		return nil, fmt.Errorf("uniques for %s.%s: %w", schema, table, err)
	}
	if err := d.loadCheckConstraints(ctx, t); err != nil {
		return nil, fmt.Errorf("checks for %s.%s: %w", schema, table, err)
	}
	if err := d.loadForeignKeys(ctx, t); err != nil {
		return nil, fmt.Errorf("fks for %s.%s: %w", schema, table, err)
	}
	if err := d.loadIndexes(ctx, t); err != nil {
		return nil, fmt.Errorf("indexes for %s.%s: %w", schema, table, err)
	}

	return t, nil
}

func (d *Dumper) loadColumns(ctx context.Context, t *Table) error {
	rows, err := d.conn.Query(ctx, `
		SELECT
			a.attname,
			pg_catalog.format_type(a.atttypid, a.atttypmod),
			NOT a.attnotnull,
			pg_get_expr(ad.adbin, ad.adrelid),
			a.attidentity::text,
			a.attgenerated::text
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		WHERE n.nspname = $1
		  AND c.relname = $2
		  AND a.attnum > 0
		  AND NOT a.attisdropped
		ORDER BY a.attnum
	`, t.Schema, t.Name)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var col Column
		var defaultVal *string
		var identity string
		var generated string
		if err := rows.Scan(&col.Name, &col.DataType, &col.Nullable, &defaultVal, &identity, &generated); err != nil {
			return err
		}
		// A generated column's expression lives in pg_attrdef too. Route it to
		// Generated so RenderDDL emits GENERATED ALWAYS AS (...) STORED rather
		// than a DEFAULT that references sibling columns.
		if generated == "s" {
			col.Generated = defaultVal
			defaultVal = nil
		}
		col.Default = defaultVal
		switch identity {
		case "a":
			col.IsIdentity = true
			col.IdentityKind = "ALWAYS"
		case "d":
			col.IsIdentity = true
			col.IdentityKind = "BY DEFAULT"
		}
		// Skip serial-style defaults (nextval) when they're part of SERIAL/BIGSERIAL
		if defaultVal != nil && strings.HasPrefix(*defaultVal, "nextval(") {
			// Check if the type is integer/bigint — if so, this is a SERIAL
			if col.DataType == "integer" || col.DataType == "bigint" || col.DataType == "smallint" {
				serialType := "SERIAL"
				switch col.DataType {
				case "bigint":
					serialType = "BIGSERIAL"
				case "smallint":
					serialType = "SMALLSERIAL"
				}
				col.DataType = serialType
				col.Default = nil
			}
		}
		t.Columns = append(t.Columns, col)
	}
	return rows.Err()
}

func (d *Dumper) loadPrimaryKey(ctx context.Context, t *Table) error {
	rows, err := d.conn.Query(ctx, `
		SELECT con.conname, array_agg(a.attname ORDER BY k.n)
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN unnest(con.conkey) WITH ORDINALITY AS k(attnum, n)
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
		WHERE n.nspname = $1
		  AND c.relname = $2
		  AND con.contype = 'p'
		GROUP BY con.conname
	`, t.Schema, t.Name)
	if err != nil {
		return err
	}
	defer rows.Close()

	if rows.Next() {
		var pk PrimaryKey
		if err := rows.Scan(&pk.Name, &pk.Columns); err != nil {
			return err
		}
		t.PK = &pk
	}
	return rows.Err()
}

func (d *Dumper) loadUniqueConstraints(ctx context.Context, t *Table) error {
	rows, err := d.conn.Query(ctx, `
		SELECT con.conname, array_agg(a.attname ORDER BY k.n)
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN unnest(con.conkey) WITH ORDINALITY AS k(attnum, n)
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
		WHERE n.nspname = $1
		  AND c.relname = $2
		  AND con.contype = 'u'
		GROUP BY con.conname
		ORDER BY con.conname
	`, t.Schema, t.Name)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var u UniqueConstraint
		if err := rows.Scan(&u.Name, &u.Columns); err != nil {
			return err
		}
		t.Uniques = append(t.Uniques, u)
	}
	return rows.Err()
}

func (d *Dumper) loadCheckConstraints(ctx context.Context, t *Table) error {
	rows, err := d.conn.Query(ctx, `
		SELECT con.conname, pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relname = $2
		  AND con.contype = 'c'
		ORDER BY con.conname
	`, t.Schema, t.Name)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var ck CheckConstraint
		if err := rows.Scan(&ck.Name, &ck.Expression); err != nil {
			return err
		}
		t.Checks = append(t.Checks, ck)
	}
	return rows.Err()
}

func (d *Dumper) loadForeignKeys(ctx context.Context, t *Table) error {
	rows, err := d.conn.Query(ctx, `
		SELECT
			con.conname,
			array_agg(a.attname ORDER BY k.n),
			nr.nspname,
			cr.relname,
			array_agg(ar.attname ORDER BY kr.n),
			CASE con.confdeltype
				WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT'
				WHEN 'c' THEN 'CASCADE' WHEN 'n' THEN 'SET NULL'
				WHEN 'd' THEN 'SET DEFAULT' ELSE 'NO ACTION'
			END,
			CASE con.confupdtype
				WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT'
				WHEN 'c' THEN 'CASCADE' WHEN 'n' THEN 'SET NULL'
				WHEN 'd' THEN 'SET DEFAULT' ELSE 'NO ACTION'
			END
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_class cr ON cr.oid = con.confrelid
		JOIN pg_namespace nr ON nr.oid = cr.relnamespace
		CROSS JOIN unnest(con.conkey) WITH ORDINALITY AS k(attnum, n)
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
		CROSS JOIN unnest(con.confkey) WITH ORDINALITY AS kr(attnum, n)
		JOIN pg_attribute ar ON ar.attrelid = con.confrelid AND ar.attnum = kr.attnum
		WHERE n.nspname = $1
		  AND c.relname = $2
		  AND con.contype = 'f'
		GROUP BY con.conname, nr.nspname, cr.relname, con.confdeltype, con.confupdtype
		ORDER BY con.conname
	`, t.Schema, t.Name)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var fk ForeignKey
		if err := rows.Scan(&fk.Name, &fk.Columns, &fk.RefSchema, &fk.RefTable, &fk.RefColumns, &fk.OnDelete, &fk.OnUpdate); err != nil {
			return err
		}
		t.FKs = append(t.FKs, fk)
	}
	return rows.Err()
}

func (d *Dumper) loadIndexes(ctx context.Context, t *Table) error {
	// Get indexes that are NOT backing constraints (PK, unique, FK are already captured)
	rows, err := d.conn.Query(ctx, `
		SELECT
			i.relname,
			ix.indisunique,
			pg_get_indexdef(ix.indexrelid)
		FROM pg_index ix
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_class c ON c.oid = ix.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relname = $2
		  AND NOT ix.indisprimary
		  AND NOT EXISTS (
			  SELECT 1 FROM pg_constraint con
			  WHERE con.conindid = ix.indexrelid
		  )
		ORDER BY i.relname
	`, t.Schema, t.Name)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var idx Index
		if err := rows.Scan(&idx.Name, &idx.Unique, &idx.Definition); err != nil {
			return err
		}
		t.Indexes = append(t.Indexes, idx)
	}
	return rows.Err()
}

// RenderDDL generates the CREATE TABLE + CREATE INDEX statements for a table.
func RenderDDL(t *Table) string {
	var b strings.Builder

	// Schema-qualify every object. The desired-state DDL is replayed into a
	// throwaway database through a *connection pool*, so a leading
	// "SET search_path" cannot be relied on to reach the statement that follows
	// it. Unqualified DDL therefore lands in public no matter which
	// schema/{pg_schema}/ folder it came from, and the diff — which reads the
	// live side scoped to that schema — sees an empty desired state and proposes
	// dropping every table in it.
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", t.QualifiedName())

	// Columns
	for i, col := range t.Columns {
		fmt.Fprintf(&b, "    %s %s", pgident.Quote(col.Name), col.DataType)
		if col.IsIdentity {
			fmt.Fprintf(&b, " GENERATED %s AS IDENTITY", col.IdentityKind)
		}
		if col.Generated != nil {
			// Rendered before NOT NULL, which is the order Postgres accepts,
			// and byte-for-byte as pg_get_expr wrote it.
			//
			// The expression used to be folded onto one line with
			// strings.Fields/Join. That collapses whitespace INSIDE string
			// literals too, so
			//
			//	(a || '
			//	  two spaces  here'::text)
			//
			// dumped as (a || ' two spaces here'::text) and the replayed column
			// computed different values -- the baseline in git stopped
			// describing the database. Re-indenting a pretty-printed expression
			// has the same defect: a literal containing a newline gets indented
			// along with the SQL. There is no cosmetic transformation of an
			// arbitrary SQL expression that is safe without parsing it, and
			// readability is not worth a wrong expression. The sibling DEFAULT
			// path a few lines down has always emitted verbatim.
			fmt.Fprintf(&b, " GENERATED ALWAYS AS (%s) STORED", *col.Generated)
		}
		if !col.Nullable {
			b.WriteString(" NOT NULL")
		}
		if col.Default != nil && !col.IsIdentity {
			fmt.Fprintf(&b, " DEFAULT %s", *col.Default)
		}

		// Trailing comma unless last item AND no constraints follow
		hasConstraints := t.PK != nil || len(t.Uniques) > 0 || len(t.Checks) > 0 || len(t.FKs) > 0
		if i < len(t.Columns)-1 || hasConstraints {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	// Primary key
	if t.PK != nil {
		remaining := len(t.Uniques) + len(t.Checks) + len(t.FKs)
		fmt.Fprintf(&b, "    CONSTRAINT %s PRIMARY KEY (%s)",
			pgident.Quote(t.PK.Name), strings.Join(pgident.QuoteAll(t.PK.Columns), ", "))
		if remaining > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	// Unique constraints
	for i, u := range t.Uniques {
		remaining := len(t.Checks) + len(t.FKs)
		fmt.Fprintf(&b, "    CONSTRAINT %s UNIQUE (%s)",
			pgident.Quote(u.Name), strings.Join(pgident.QuoteAll(u.Columns), ", "))
		if i < len(t.Uniques)-1 || remaining > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	// Check constraints
	for i, ck := range t.Checks {
		remaining := len(t.FKs)
		// The expression comes from pg_get_constraintdef, which already quotes
		// what needs quoting; only the constraint name is ours to render.
		fmt.Fprintf(&b, "    CONSTRAINT %s %s", pgident.Quote(ck.Name), ck.Expression)
		if i < len(t.Checks)-1 || remaining > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	// Foreign keys
	for i, fk := range t.FKs {
		// The FK target is the sharpest case for quoting: an unquoted,
		// wrongly-cased reference does not error, it resolves to a DIFFERENT
		// table -- the constraint is created, against the wrong object.
		fmt.Fprintf(&b, "    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
			pgident.Quote(fk.Name),
			strings.Join(pgident.QuoteAll(fk.Columns), ", "),
			pgident.Qualify(fk.RefSchema, fk.RefTable),
			strings.Join(pgident.QuoteAll(fk.RefColumns), ", "))
		if fk.OnDelete != "NO ACTION" {
			fmt.Fprintf(&b, " ON DELETE %s", fk.OnDelete)
		}
		if fk.OnUpdate != "NO ACTION" {
			fmt.Fprintf(&b, " ON UPDATE %s", fk.OnUpdate)
		}
		if i < len(t.FKs)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	b.WriteString(");\n")

	// Indexes (non-constraint). pg_get_indexdef() already emits a
	// schema-qualified target ("ON materialized.foo"); keep it, for the same
	// reason the CREATE TABLE above is qualified.
	for _, idx := range t.Indexes {
		fmt.Fprintf(&b, "\n%s;\n", idx.Definition)
	}

	return b.String()
}
