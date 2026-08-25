package dump

import (
	"context"
	"fmt"
	"strings"

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

// QualifiedName returns the schema-qualified table name, e.g. "materialized.rpt_invoice_detail".
func (t *Table) QualifiedName() string {
	return t.Schema + "." + t.Name
}

// Column represents a table column.
type Column struct {
	Name         string
	DataType     string
	Nullable     bool
	Default      *string
	IsIdentity   bool
	IdentityKind string // "ALWAYS" or "BY DEFAULT"
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
func (d *Dumper) ListTables(ctx context.Context, schema string) ([]string, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p')  -- regular tables and partitioned tables
		  AND NOT c.relispartition      -- exclude child partitions
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
			a.attidentity::text
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
		if err := rows.Scan(&col.Name, &col.DataType, &col.Nullable, &defaultVal, &identity); err != nil {
			return err
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
		fmt.Fprintf(&b, "    %s %s", col.Name, col.DataType)
		if col.IsIdentity {
			fmt.Fprintf(&b, " GENERATED %s AS IDENTITY", col.IdentityKind)
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
		fmt.Fprintf(&b, "    CONSTRAINT %s PRIMARY KEY (%s)", t.PK.Name, strings.Join(t.PK.Columns, ", "))
		if remaining > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	// Unique constraints
	for i, u := range t.Uniques {
		remaining := len(t.Checks) + len(t.FKs)
		fmt.Fprintf(&b, "    CONSTRAINT %s UNIQUE (%s)", u.Name, strings.Join(u.Columns, ", "))
		if i < len(t.Uniques)-1 || remaining > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	// Check constraints
	for i, ck := range t.Checks {
		remaining := len(t.FKs)
		fmt.Fprintf(&b, "    CONSTRAINT %s %s", ck.Name, ck.Expression)
		if i < len(t.Checks)-1 || remaining > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	// Foreign keys
	for i, fk := range t.FKs {
		refTable := fk.RefSchema + "." + fk.RefTable
		fmt.Fprintf(&b, "    CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
			fk.Name,
			strings.Join(fk.Columns, ", "),
			refTable,
			strings.Join(fk.RefColumns, ", "))
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
