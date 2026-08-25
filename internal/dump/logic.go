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
//
// Materialized views are NOT views for this purpose and are not returned. They
// have no CREATE OR REPLACE form, so they cannot be re-applied idempotently the
// way a logic/ file must be, and RenderView would emit them as
// CREATE OR REPLACE VIEW -- a plain view with the same name and none of the
// storage, refresh policy, or indexes.
//
// The previous filter (relkind = 'v') skipped them in silence, which is the
// worse failure: a baseline that omits objects without saying so cannot be
// distinguished from one taken against a database that has none. m8 refuses
// instead, and names them. Set Dumper.AllowUnsupported to take the skip
// deliberately.
func (d *Dumper) ListViews(ctx context.Context, schema string) ([]View, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT c.relname, c.relkind::text, pg_get_viewdef(c.oid, true), coalesce(c.reloptions, '{}')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relkind IN ('v', 'm')
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
	var materialized []string
	for rows.Next() {
		var v View
		var kind string
		v.Schema = schema
		if err := rows.Scan(&v.Name, &kind, &v.Definition, &v.Options); err != nil {
			return nil, err
		}
		if kind == "m" {
			materialized = append(materialized, v.Name)
			continue
		}
		views = append(views, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(materialized) > 0 && !d.AllowUnsupported {
		return nil, fmt.Errorf(
			"schema %s contains materialized view(s) m8 cannot represent as a logic/ file: %s -- "+
				"a materialized view has no CREATE OR REPLACE form, so it cannot be re-applied "+
				"idempotently, and emitting it as CREATE OR REPLACE VIEW would replace it with a "+
				"plain view. Re-run with --allow-unsupported to leave them out of the dump "+
				"deliberately, or scope the dump with --schema",
			schema, strings.Join(materialized, ", "))
	}
	return views, nil
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
