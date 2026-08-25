package dump

import (
	"context"
	"fmt"
	"strings"

	"github.com/ags-slc/m8/internal/pgident"
)

// Function represents a function or procedure.
type Function struct {
	Schema     string
	Name       string
	Identity   string // full signature for overloaded functions
	Kind       string // "f" = function, "p" = procedure
	Definition string // full CREATE OR REPLACE statement
}

// View represents a database view.
type View struct {
	Schema     string
	Name       string
	Definition string   // the SELECT query
	Options    []string // reloptions (security_barrier, security_invoker, check_option)
}

// QualifiedName returns the quoted schema.name, matching Table.QualifiedName.
func (v *View) QualifiedName() string {
	return pgident.Qualify(v.Schema, v.Name)
}

// ListFunctions returns all user-defined functions and procedures in a schema.
func (d *Dumper) ListFunctions(ctx context.Context, schema string) ([]Function, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT
			p.proname,
			pg_catalog.pg_get_function_identity_arguments(p.oid),
			CASE p.prokind WHEN 'p' THEN 'p' ELSE 'f' END,
			pg_catalog.pg_get_functiondef(p.oid)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1
		  AND p.prokind IN ('f', 'p')  -- functions and procedures
		  AND p.proname NOT LIKE 'pg_%'
		  AND NOT EXISTS (
			  SELECT 1 FROM pg_depend d
			  WHERE d.objid = p.oid AND d.deptype = 'e'  -- exclude extension-owned
		  )
		ORDER BY p.proname, pg_catalog.pg_get_function_identity_arguments(p.oid)
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list functions in %s: %w", schema, err)
	}
	defer rows.Close()

	var funcs []Function
	for rows.Next() {
		var f Function
		var identArgs string
		f.Schema = schema
		if err := rows.Scan(&f.Name, &identArgs, &f.Kind, &f.Definition); err != nil {
			return nil, err
		}
		f.Identity = fmt.Sprintf("%s(%s)", f.Name, identArgs)
		funcs = append(funcs, f)
	}
	return funcs, rows.Err()
}

// ListViews returns all user-defined views in a schema.
func (d *Dumper) ListViews(ctx context.Context, schema string) ([]View, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT c.relname, pg_get_viewdef(c.oid, true), coalesce(c.reloptions, '{}')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind = 'v'
		  AND NOT EXISTS (
			  SELECT 1 FROM pg_depend d
			  WHERE d.objid = c.oid AND d.deptype = 'e'
		  )
		ORDER BY c.relname
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list views in %s: %w", schema, err)
	}
	defer rows.Close()

	var views []View
	for rows.Next() {
		var v View
		v.Schema = schema
		if err := rows.Scan(&v.Name, &v.Definition, &v.Options); err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, rows.Err()
}

// RenderFunction generates a CREATE OR REPLACE statement.
// pg_get_functiondef already returns the full definition, so we just
// ensure it starts with CREATE OR REPLACE.
func RenderFunction(f *Function) string {
	def := f.Definition
	// pg_get_functiondef returns "CREATE OR REPLACE FUNCTION/PROCEDURE ..."
	// Just ensure it ends with a semicolon
	def = strings.TrimSpace(def)
	if !strings.HasSuffix(def, ";") {
		def += ";"
	}
	return def + "\n"
}

// RenderView generates a schema-qualified CREATE OR REPLACE VIEW statement.
//
// The name MUST be qualified: desired state is replayed over a connection pool,
// so a session-level SET search_path cannot be relied on and an unqualified view
// would be created in whatever schema happens to be first on the path (usually
// public) instead of its own.
//
// pg_get_viewdef() already terminates its output with a semicolon, so appending
// another one produces an empty trailing statement (";;").
func RenderView(v *View) string {
	def := strings.TrimSpace(v.Definition)
	def = strings.TrimRight(def, ";")

	var opts string
	if len(v.Options) > 0 {
		opts = fmt.Sprintf(" WITH (%s)", strings.Join(v.Options, ", "))
	}

	return fmt.Sprintf("CREATE OR REPLACE VIEW %s%s AS\n%s;\n", v.QualifiedName(), opts, def)
}
