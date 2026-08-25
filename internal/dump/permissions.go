package dump

import (
	"context"
	"fmt"
	"strings"

	"github.com/ags-slc/m8/internal/pgident"
)

// Grant represents a table-level privilege.
//
// Grantee is a role name, EXCEPT for the sentinel "PUBLIC", which stands for
// the pseudo-role and is rendered unquoted. PostgreSQL reserves the name
// (CREATE ROLE public is refused), so no real role can produce the sentinel.
type Grant struct {
	Schema    string
	Table     string
	Grantee   string
	Privilege string // SELECT, INSERT, UPDATE, DELETE, etc.
}

// publicRole is the sentinel Grantee for PUBLIC. See Grant.
const publicRole = "PUBLIC"

// quoteGrantee renders a grantee: PUBLIC is a keyword and must stay unquoted;
// every real role name is quoted like any other identifier.
func quoteGrantee(grantee string) string {
	if grantee == publicRole {
		return publicRole
	}
	return pgident.Quote(grantee)
}

// ListGrants returns all non-owner privileges on a schema's relations.
//
// Read from pg_class.relacl, NOT information_schema.role_table_grants: the
// information_schema views are filtered to grants the CURRENT role can see --
// ones where it is the grantor, the grantee, or a member of the grantee. Dumped
// by an ordinary application role, they silently omit every grant to a role
// that role is not a member of, and a permissions file that looks complete
// leaves whole services without access on a rebuilt database.
func (d *Dumper) ListGrants(ctx context.Context, schema string) ([]Grant, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT
			n.nspname,
			c.relname,
			pg_catalog.pg_get_userbyid(a.grantee),
			a.privilege_type
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL aclexplode(c.relacl) a
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		  AND c.relacl IS NOT NULL
		  AND a.grantee <> 0                       -- PUBLIC, handled separately
		  AND a.grantee <> c.relowner              -- the owner's implicit grants
		  AND pg_catalog.pg_get_userbyid(a.grantee) NOT LIKE 'pg\_%'
		  AND c.relname NOT LIKE '_m8%'
		ORDER BY c.relname,
		         pg_catalog.pg_get_userbyid(a.grantee),
		         a.privilege_type
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list grants in %s: %w", schema, err)
	}
	defer rows.Close()

	var grants []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.Schema, &g.Table, &g.Grantee, &g.Privilege); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// ListPublicGrants returns privileges granted to PUBLIC on a schema's
// relations. Read from pg_class.relacl for the same reason as ListGrants.
func (d *Dumper) ListPublicGrants(ctx context.Context, schema string) ([]Grant, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT n.nspname, c.relname, 'PUBLIC', a.privilege_type
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL aclexplode(c.relacl) a
		WHERE n.nspname = $1
		  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		  AND c.relacl IS NOT NULL
		  AND a.grantee = 0
		  AND c.relname NOT LIKE '_m8%'
		ORDER BY c.relname, a.privilege_type
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list public grants in %s: %w", schema, err)
	}
	defer rows.Close()

	var grants []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.Schema, &g.Table, &g.Grantee, &g.Privilege); err != nil {
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
}

// ListRoutineGrants returns EXECUTE privileges on a schema's functions and
// procedures, excluding the owner's own and the implicit grant to PUBLIC.
//
// information_schema.role_table_grants covers relations only, so without this
// a dumped schema silently loses every routine grant -- and a procedure the
// application calls becomes uncallable on a rebuilt database.
func (d *Dumper) ListRoutineGrants(ctx context.Context, schema string) ([]RoutineGrant, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT
			n.nspname,
			p.proname,
			pg_catalog.pg_get_function_identity_arguments(p.oid),
			pg_catalog.pg_get_userbyid(a.grantee),
			a.privilege_type
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		CROSS JOIN LATERAL aclexplode(
			coalesce(p.proacl, acldefault('f', p.proowner))
		) a
		WHERE n.nspname = $1
		  AND p.prokind IN ('f', 'p')
		  AND a.grantee <> 0                                    -- not PUBLIC
		  AND a.grantee <> p.proowner                           -- not the owner's own
		  AND pg_catalog.pg_get_userbyid(a.grantee) NOT LIKE 'pg\_%'
		  AND NOT EXISTS (
			  SELECT 1 FROM pg_depend d
			  WHERE d.objid = p.oid AND d.deptype = 'e'
		  )
		ORDER BY 2, 3, 4, 5
	`, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list routine grants in %s: %w", schema, err)
	}
	defer rows.Close()

	var grants []RoutineGrant
	for rows.Next() {
		var g RoutineGrant
		if err := rows.Scan(&g.Schema, &g.Name, &g.Args, &g.Grantee, &g.Privilege); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// RenderRoutineGrants generates GRANT ... ON FUNCTION/PROCEDURE statements.
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
		fmt.Fprintf(&b, "GRANT %s ON ROUTINE %s.%s(%s) TO %s;\n",
			g.Privilege, pgident.Quote(g.Schema), pgident.Quote(g.Name), g.Args, quoteGrantee(g.Grantee))
	}
	return b.String()
}

// RenderGrants generates GRANT statements grouped by grantee and table.
func RenderGrants(grants []Grant, schema string) string {
	if len(grants) == 0 {
		return ""
	}

	// Group by (grantee, schema, table) → privileges
	type key struct {
		grantee string
		schema  string
		table   string
	}
	grouped := make(map[key][]string)
	var order []key
	for _, g := range grants {
		k := key{g.Grantee, g.Schema, g.Table}
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
		privs := grouped[k]
		fmt.Fprintf(&b, "GRANT %s ON %s TO %s;\n",
			strings.Join(privs, ", "),
			pgident.Qualify(k.schema, k.table),
			quoteGrantee(k.grantee))
	}

	return b.String()
}
