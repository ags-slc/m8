package dump

import (
	"context"
	"fmt"
	"strings"
)

// Grant represents a table-level privilege.
type Grant struct {
	Schema    string
	Table     string
	Grantee   string
	Privilege string // SELECT, INSERT, UPDATE, DELETE, etc.
}

// ListGrants returns all non-default table grants in a schema.
// Excludes grants to the table owner and to pg_ system roles.
func (d *Dumper) ListGrants(ctx context.Context, schema string) ([]Grant, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT
			t.table_schema,
			t.table_name,
			t.grantee,
			t.privilege_type
		FROM information_schema.role_table_grants t
		WHERE t.table_schema = $1
		  AND t.grantee != t.grantor
		  AND t.grantee NOT LIKE 'pg_%'
		  AND t.grantee != 'PUBLIC'
		  AND t.table_name NOT LIKE '_m8%'
		ORDER BY t.table_name, t.grantee, t.privilege_type
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

// ListPublicGrants returns all grants to PUBLIC in a schema.
func (d *Dumper) ListPublicGrants(ctx context.Context, schema string) ([]Grant, error) {
	rows, err := d.conn.Query(ctx, `
		SELECT
			t.table_schema,
			t.table_name,
			'PUBLIC',
			t.privilege_type
		FROM information_schema.role_table_grants t
		WHERE t.table_schema = $1
		  AND t.grantee = 'PUBLIC'
		  AND t.table_name NOT LIKE '_m8%'
		ORDER BY t.table_name, t.privilege_type
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
		target := k.table
		if k.schema != "" {
			target = k.schema + "." + k.table
		}
		fmt.Fprintf(&b, "GRANT %s ON %s TO %s;\n",
			strings.Join(privs, ", "),
			target,
			k.grantee)
	}

	return b.String()
}
