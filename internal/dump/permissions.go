package dump

import (
	"context"
	"fmt"
	"strings"

	"github.com/ags-slc/m8/internal/pgident"
)

// publicRole is the sentinel Grantee standing for the PUBLIC pseudo-role.
// PostgreSQL reserves the name -- CREATE ROLE public is refused -- so no real
// role can produce it, and it is rendered unquoted because PUBLIC is a keyword.
const publicRole = "PUBLIC"

// granteeExpr renders a grantee from an aclexplode row: OID 0 is PUBLIC.
const granteeExpr = `CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END`

// quoteGrantee renders a grantee: PUBLIC is a keyword and must stay unquoted;
// every real role name is quoted like any other identifier.
func quoteGrantee(grantee string) string {
	if grantee == publicRole {
		return publicRole
	}
	return pgident.Quote(grantee)
}

// Grant represents a privilege on a relation or one of its columns.
//
// Column is empty for a relation-level grant. Kind is the pg_class.relkind of
// the target, because GRANT needs the object type spelled out: USAGE on a
// sequence is not a valid privilege for a table, so a sequence grant rendered
// as ON TABLE is rejected outright.
type Grant struct {
	Schema    string
	Table     string
	Column    string
	Kind      string // pg_class.relkind
	Grantee   string
	Privilege string // SELECT, INSERT, UPDATE, USAGE, ...
	Grantable bool   // WITH GRANT OPTION
}

// grantedRelkinds are the relation kinds that can carry privileges. 'S'
// (sequence) is included: a sequence behind a SERIAL column is granted
// separately from its table, and without it every application that inserts into
// that table loses nextval() on a rebuilt database.
const grantedRelkinds = `'r', 'p', 'v', 'm', 'f', 'S'`

// ListGrants returns all non-owner privileges on a schema's relations,
// including those granted to PUBLIC.
//
// Read from pg_class.relacl, NOT information_schema.role_table_grants: the
// information_schema views are filtered to grants the CURRENT role can see --
// ones where it is the grantor, the grantee, or a member of the grantee. Dumped
// by an ordinary application role, they silently omit every grant to a role
// that role is not a member of, and a permissions file that looks complete
// leaves whole services without access on a rebuilt database.
//
// DISTINCT because aclexplode yields one row per (grantor, grantee, privilege):
// the same privilege granted by two different grantors is one privilege, and
// without DISTINCT it produced duplicate GRANT lines.
func (d *Dumper) ListGrants(ctx context.Context, schema string) ([]Grant, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT DISTINCT
			n.nspname,
			c.relname,
			c.relkind::text,
			`+granteeExpr+`,
			a.privilege_type,
			a.is_grantable
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL aclexplode(c.relacl) a
		WHERE n.nspname = $1
		  AND c.relkind IN (`+grantedRelkinds+`)
		  AND c.relacl IS NOT NULL
		  AND a.grantee <> c.relowner              -- the owner's implicit grants
		  AND NOT (a.grantee <> 0 AND pg_catalog.pg_get_userbyid(a.grantee) LIKE 'pg\_%')
		  AND c.relname NOT LIKE '_m8%'
		ORDER BY 2, 4, 5
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list grants in %s: %w", schema, err)
	}
	defer rows.Close()

	var grants []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.Schema, &g.Table, &g.Kind, &g.Grantee, &g.Privilege, &g.Grantable); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// ListColumnGrants returns column-level privileges, which live in
// pg_attribute.attacl and are invisible to pg_class.relacl. A role granted
// SELECT on three columns of a table has no relation-level entry at all, so
// without this the grant vanishes entirely from the dump.
func (d *Dumper) ListColumnGrants(ctx context.Context, schema string) ([]Grant, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT DISTINCT
			n.nspname,
			c.relname,
			att.attname,
			c.relkind::text,
			`+granteeExpr+`,
			a.privilege_type,
			a.is_grantable
		FROM pg_attribute att
		JOIN pg_class c ON c.oid = att.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL aclexplode(att.attacl) a
		WHERE n.nspname = $1
		  AND c.relkind IN (`+grantedRelkinds+`)
		  AND att.attacl IS NOT NULL
		  AND att.attnum > 0
		  AND NOT att.attisdropped
		  AND a.grantee <> c.relowner
		  AND NOT (a.grantee <> 0 AND pg_catalog.pg_get_userbyid(a.grantee) LIKE 'pg\_%')
		  AND c.relname NOT LIKE '_m8%'
		ORDER BY 2, 3, 5, 6
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list column grants in %s: %w", schema, err)
	}
	defer rows.Close()

	var grants []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.Schema, &g.Table, &g.Column, &g.Kind, &g.Grantee, &g.Privilege, &g.Grantable); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// SchemaGrant represents a privilege on the schema itself (USAGE, CREATE).
type SchemaGrant struct {
	Schema    string
	Grantee   string
	Privilege string
	Grantable bool
}

// ListSchemaGrants returns privileges on the schema itself, from
// pg_namespace.nspacl.
//
// Without this every captured relation grant is INERT on a rebuilt database. A
// role needs USAGE on the schema before any privilege on a relation inside it
// means anything, and CREATE SCHEMA grants USAGE to nobody -- so a permissions
// file full of correct GRANT SELECT statements produces a database where the
// application still cannot see a single table.
func (d *Dumper) ListSchemaGrants(ctx context.Context, schema string) ([]SchemaGrant, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT DISTINCT
			n.nspname,
			`+granteeExpr+`,
			a.privilege_type,
			a.is_grantable
		FROM pg_namespace n
		CROSS JOIN LATERAL aclexplode(n.nspacl) a
		WHERE n.nspname = $1
		  AND n.nspacl IS NOT NULL
		  AND a.grantee <> n.nspowner
		  AND NOT (a.grantee <> 0 AND pg_catalog.pg_get_userbyid(a.grantee) LIKE 'pg\_%')
		ORDER BY 2, 3
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list schema grants for %s: %w", schema, err)
	}
	defer rows.Close()

	var grants []SchemaGrant
	for rows.Next() {
		var g SchemaGrant
		if err := rows.Scan(&g.Schema, &g.Grantee, &g.Privilege, &g.Grantable); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// RoutineGrant represents a non-default EXECUTE privilege on a function or
// procedure.
//
// Name and Args are kept apart because only Name is an identifier: it must be
// quoted, while Args is a rendered type list from
// pg_get_function_identity_arguments that must not be. Args is required --
// EXECUTE is granted per overload, so an unsignatured GRANT is ambiguous the
// moment a function is overloaded.
type RoutineGrant struct {
	Schema    string
	Name      string
	Args      string
	Grantee   string
	Privilege string
	Grantable bool
}

// notExtensionOwned excludes objects installed by an extension: they are
// recreated by CREATE EXTENSION, not by migration files.
const notExtensionOwned = `
		  AND NOT EXISTS (
			  SELECT 1 FROM pg_depend dep
			  WHERE dep.objid = p.oid AND dep.classid = 'pg_proc'::regclass AND dep.deptype = 'e'
		  )`

// ListRoutineGrants returns EXECUTE privileges on a schema's functions and
// procedures, excluding the owner's own.
//
// information_schema.role_table_grants covers relations only, so without this
// a dumped schema silently loses every routine grant -- and a procedure the
// application calls becomes uncallable on a rebuilt database.
//
// Reads p.proacl directly. It used to read coalesce(p.proacl, acldefault('f',
// p.proowner)), which was dead code: when proacl is NULL the default ACL holds
// exactly the owner's own privileges and PUBLIC's, and both were then filtered
// out again by the WHERE clause. A NULL proacl means "nothing has been changed",
// and there is nothing to capture.
func (d *Dumper) ListRoutineGrants(ctx context.Context, schema string) ([]RoutineGrant, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT DISTINCT
			n.nspname,
			p.proname,
			pg_catalog.pg_get_function_identity_arguments(p.oid),
			`+granteeExpr+`,
			a.privilege_type,
			a.is_grantable
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		CROSS JOIN LATERAL aclexplode(p.proacl) a
		WHERE n.nspname = $1
		  AND p.prokind IN ('f', 'p')
		  AND p.proacl IS NOT NULL
		  AND a.grantee <> 0                                    -- PUBLIC: see ListPublicRevokes
		  AND a.grantee <> p.proowner                           -- not the owner's own
		  AND pg_catalog.pg_get_userbyid(a.grantee) NOT LIKE 'pg\_%'`+notExtensionOwned+`
		ORDER BY 2, 3, 4, 5
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list routine grants in %s: %w", schema, err)
	}
	defer rows.Close()

	var grants []RoutineGrant
	for rows.Next() {
		var g RoutineGrant
		if err := rows.Scan(&g.Schema, &g.Name, &g.Args, &g.Grantee, &g.Privilege, &g.Grantable); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// Revoke is a privilege PostgreSQL hands to PUBLIC by default which the live
// database has taken away.
type Revoke struct {
	Schema    string
	Name      string // routine name; empty for a schema-level revoke
	Args      string
	Privilege string
}

// ListPublicRevokes returns the PUBLIC privileges the live database has revoked
// and a rebuilt one would silently hand back.
//
// A GRANT-only dump is not a faithful capture of an access-control state: it
// records what was added and loses what was taken away, and the direction of
// that loss is ESCALATION. Every function is created with EXECUTE granted to
// PUBLIC, so a SECURITY DEFINER function hardened with
//
//	REVOKE EXECUTE ON FUNCTION auth.user_secret(text) FROM PUBLIC
//
// is executable by every role again on a database rebuilt from a dump that only
// captured grants.
//
// The comparison is against acldefault() for the object's owner rather than a
// hardcoded list, so it follows whatever the server version considers default.
func (d *Dumper) ListPublicRevokes(ctx context.Context, schema string) ([]Revoke, error) {
	var revokes []Revoke

	rows, err := d.conn.Query(ctx, `
		SELECT DISTINCT
			n.nspname,
			p.proname,
			pg_catalog.pg_get_function_identity_arguments(p.oid),
			def.privilege_type
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		CROSS JOIN LATERAL aclexplode(acldefault('f', p.proowner)) def
		WHERE n.nspname = $1
		  AND p.prokind IN ('f', 'p')
		  AND p.proacl IS NOT NULL
		  AND def.grantee = 0
		  AND NOT EXISTS (
			  SELECT 1 FROM aclexplode(p.proacl) held
			  WHERE held.grantee = 0 AND held.privilege_type = def.privilege_type
		  )`+notExtensionOwned+`
		ORDER BY 2, 3, 4
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list public revokes in %s: %w", schema, err)
	}
	for rows.Next() {
		var r Revoke
		if err := rows.Scan(&r.Schema, &r.Name, &r.Args, &r.Privilege); err != nil {
			rows.Close()
			return nil, err
		}
		revokes = append(revokes, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The public schema is the one schema initdb grants to PUBLIC (USAGE, and
	// CREATE before PostgreSQL 15), so it is the one schema where a revoke can
	// be undone by a rebuild. Any other schema starts with no PUBLIC access at
	// all, and emitting a revoke for it would be noise.
	if schema == "public" {
		prows, err := d.conn.Query(ctx, `
			SELECT priv
			FROM unnest(ARRAY['USAGE', 'CREATE']) AS priv
			JOIN pg_namespace n ON n.nspname = 'public'
			WHERE n.nspacl IS NOT NULL
			  AND NOT EXISTS (
				  SELECT 1 FROM aclexplode(n.nspacl) held
				  WHERE held.grantee = 0 AND held.privilege_type = priv
			  )
			ORDER BY priv
		`)
		if err != nil {
			return nil, fmt.Errorf("failed to check public-schema revokes: %w", err)
		}
		defer prows.Close()
		for prows.Next() {
			var priv string
			if err := prows.Scan(&priv); err != nil {
				return nil, err
			}
			revokes = append(revokes, Revoke{Schema: schema, Privilege: priv})
		}
		if err := prows.Err(); err != nil {
			return nil, err
		}
	}

	return revokes, nil
}

// RenderPublicRevokes generates the REVOKE ... FROM PUBLIC statements.
//
// They are emitted before the grants: revoke the defaults, then grant what was
// actually intended.
func RenderPublicRevokes(revokes []Revoke) string {
	if len(revokes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("-- Privileges PostgreSQL grants to PUBLIC by default and this database revoked.\n")
	b.WriteString("-- Without these a rebuilt database silently hands them back.\n")
	for _, r := range revokes {
		if r.Name == "" {
			fmt.Fprintf(&b, "REVOKE %s ON SCHEMA %s FROM PUBLIC;\n", r.Privilege, pgident.Quote(r.Schema))
			continue
		}
		fmt.Fprintf(&b, "REVOKE %s ON ROUTINE %s.%s(%s) FROM PUBLIC;\n",
			r.Privilege, pgident.Quote(r.Schema), pgident.Quote(r.Name), r.Args)
	}
	return b.String()
}

// RenderSchemaGrants generates GRANT ... ON SCHEMA statements.
func RenderSchemaGrants(grants []SchemaGrant) string {
	if len(grants) == 0 {
		return ""
	}
	type key struct {
		grantee   string
		schema    string
		grantable bool
	}
	grouped := make(map[key][]string)
	var order []key
	for _, g := range grants {
		k := key{g.Grantee, g.Schema, g.Grantable}
		if _, exists := grouped[k]; !exists {
			order = append(order, k)
		}
		grouped[k] = append(grouped[k], g.Privilege)
	}

	var b strings.Builder
	for _, k := range order {
		fmt.Fprintf(&b, "GRANT %s ON SCHEMA %s TO %s%s;\n",
			strings.Join(grouped[k], ", "),
			pgident.Quote(k.schema),
			quoteGrantee(k.grantee),
			grantOption(k.grantable))
	}
	return b.String()
}

// RenderRoutineGrants generates GRANT ... ON ROUTINE statements.
//
// The signature is always included: EXECUTE is granted per overload, and an
// unqualified or unsignatured GRANT is ambiguous the moment a function is
// overloaded. "ON ROUTINE" covers both functions and procedures.
func RenderRoutineGrants(grants []RoutineGrant) string {
	if len(grants) == 0 {
		return ""
	}
	var b strings.Builder
	for _, g := range grants {
		fmt.Fprintf(&b, "GRANT %s ON ROUTINE %s.%s(%s) TO %s%s;\n",
			g.Privilege, pgident.Quote(g.Schema), pgident.Quote(g.Name), g.Args,
			quoteGrantee(g.Grantee), grantOption(g.Grantable))
	}
	return b.String()
}

// RenderGrants generates GRANT statements grouped by grantee, target, and
// whether the privilege carries WITH GRANT OPTION.
func RenderGrants(grants []Grant, schema string) string {
	if len(grants) == 0 {
		return ""
	}

	// Grouped by everything that has to match for two privileges to belong in
	// one statement. grantable is part of the key: WITH GRANT OPTION applies to
	// the whole statement, so folding a grantable privilege in with a
	// non-grantable one would hand out re-granting rights that were never given.
	type key struct {
		grantee   string
		schema    string
		table     string
		column    string
		kind      string
		grantable bool
	}
	grouped := make(map[key][]string)
	var order []key
	for _, g := range grants {
		k := key{g.Grantee, g.Schema, g.Table, g.Column, g.Kind, g.Grantable}
		if _, exists := grouped[k]; !exists {
			order = append(order, k)
		}
		grouped[k] = append(grouped[k], g.Privilege)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "-- Grants for schema %s\n\n", schema)

	// Schema-qualify every target. Permissions are replayed through a
	// connection pool, so a session-level SET search_path cannot be relied on
	// to reach the statement after it -- an unqualified GRANT either errors or
	// silently lands on a same-named relation in public.
	for _, k := range order {
		objectType := "TABLE"
		if k.kind == "S" {
			// USAGE is not a table privilege; a sequence grant rendered ON TABLE
			// is rejected outright.
			objectType = "SEQUENCE"
		}
		columns := ""
		if k.column != "" {
			columns = " (" + pgident.Quote(k.column) + ")"
		}
		fmt.Fprintf(&b, "GRANT %s%s ON %s %s TO %s%s;\n",
			strings.Join(grouped[k], ", "),
			columns,
			objectType,
			pgident.Qualify(k.schema, k.table),
			quoteGrantee(k.grantee),
			grantOption(k.grantable))
	}

	return b.String()
}

// grantOption renders the WITH GRANT OPTION suffix. Losing it turns a role that
// could delegate access into one that cannot, which breaks whatever downstream
// grant depended on it -- silently, at the moment the delegation is next needed.
func grantOption(grantable bool) string {
	if grantable {
		return " WITH GRANT OPTION"
	}
	return ""
}
