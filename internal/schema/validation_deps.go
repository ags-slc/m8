package schema

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/stripe/pg-schema-diff/pkg/diff"
)

// maxSeededRelations caps how much of a dependency schema m8 will import into a
// validation rebuild.
//
// The cost is real: seeding runs once per temp database, and this database
// carries CDC-replicated schemas with tens of thousands of relations. A view
// reaching into one of those would turn every plan into a bulk schema copy. A
// dependency that large is also a sign the boundary is wrong, not that the cap
// is too low -- so exceed it and m8 gives up on seeding and degrades to the
// unvalidated plan it would have produced anyway.
const maxSeededRelations = 200

// maxSeededStatements caps the harvest itself.
//
// maxSeededRelations bounds the wrong unit: it counts pg_class entries, while
// the cost is the statements applySeed executes once per temp database. The two
// are not proportional. A schema of 84 relations harvested 5268 statements
// against the database this was built for -- comfortably under the relation cap
// and two orders of magnitude over its intent -- because indexes, constraints
// and (until they were filtered) grants all multiply per relation.
//
// The relation count stays as a cheap pre-filter: it is one catalog query and
// rules out a CDC schema before the expensive empty-to-live diff runs at all.
// This is the backstop on what that diff actually produced.
const maxSeededStatements = 2000

// dependencySchemas returns the schemas that objects in targetSchema reach into.
//
// Plan validation rebuilds targetSchema alone in a throwaway database, so any
// object defined in terms of another schema -- a view over another schema's
// tables is the common one -- cannot be recreated there and the whole
// validation fails. Knowing which schemas those are is what lets m8 import them
// first.
//
// Scoped to RELATION dependencies: those recorded through pg_rewrite (views,
// materialized views) and through pg_constraint (foreign keys).
//
// One known gap, which degrades rather than misleads -- the plan comes back
// unvalidated exactly as it did before this existed:
//
//   - A view whose ONLY reach into another schema is a function call. That
//     dependency is recorded against pg_proc, not pg_class, so the schema
//     holding the function is never discovered. Note the "only": when the view
//     also reads a relation there, the schema IS discovered, and the import
//     brings the function with it -- pg-schema-diff emits routines as well as
//     tables. (A function's own body is never the problem; PostgreSQL does not
//     resolve it at creation time.)
//
// Cycles are NOT a gap. A dependency schema that reaches back into the target
// cannot be fully created here, and does not need to be: applySeed skips what
// it cannot build.
func dependencySchemas(ctx context.Context, db *sql.DB, targetSchema string) ([]string, error) {
	const q = `
WITH refs AS (
    -- Views and materialized views: the rewrite rule records what they read.
    SELECT DISTINCT dep_ns.nspname AS schema_name
    FROM pg_depend d
    JOIN pg_rewrite r     ON r.oid = d.objid AND d.classid = 'pg_rewrite'::regclass
    JOIN pg_class  v      ON v.oid = r.ev_class
    JOIN pg_namespace v_ns ON v_ns.oid = v.relnamespace
    JOIN pg_class  dep    ON dep.oid = d.refobjid AND d.refclassid = 'pg_class'::regclass
    JOIN pg_namespace dep_ns ON dep_ns.oid = dep.relnamespace
    WHERE v_ns.nspname = $1

    UNION

    -- Foreign keys pointing out of the schema.
    SELECT DISTINCT ref_ns.nspname
    FROM pg_constraint c
    JOIN pg_namespace c_ns   ON c_ns.oid = c.connamespace
    JOIN pg_class     ref    ON ref.oid = c.confrelid
    JOIN pg_namespace ref_ns ON ref_ns.oid = ref.relnamespace
    WHERE c.contype = 'f' AND c_ns.nspname = $1
)
SELECT schema_name FROM refs
WHERE schema_name <> $1
  AND schema_name NOT IN ('pg_catalog', 'information_schema')
  AND schema_name NOT LIKE 'pg_toast%'
  AND schema_name NOT LIKE 'pg_temp%'
ORDER BY schema_name`

	rows, err := db.QueryContext(ctx, q, targetSchema)
	if err != nil {
		return nil, fmt.Errorf("resolving cross-schema dependencies of %q: %w", targetSchema, err)
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

// countRelations reports how many relations the given schemas hold, so seeding
// can refuse a dependency too large to import cheaply.
func countRelations(ctx context.Context, db *sql.DB, schemas []string) (int, error) {
	if len(schemas) == 0 {
		return 0, nil
	}
	const q = `
SELECT count(*)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ANY($1) AND c.relkind IN ('r', 'p', 'v', 'm', 'f')`
	var n int
	if err := db.QueryRowContext(ctx, q, schemas).Scan(&n); err != nil {
		return 0, fmt.Errorf("sizing dependency schemas: %w", err)
	}
	return n, nil
}

// harvestSchemaDDL returns the statements that rebuild the given schemas from
// nothing.
//
// There is no public "render this live schema as DDL" call in pg-schema-diff,
// but a diff FROM an empty database TO the live one is exactly that: the plan it
// generates is the list of CREATEs. The empty side is a temp database from our
// own factory, so it is cleaned up with everything else.
func (d *Differ) harvestSchemaDDL(ctx context.Context, liveDB *sql.DB, schemas []string) ([]string, error) {
	if len(schemas) == 0 {
		return nil, nil
	}

	emptyDB, err := d.tempFactory.Create(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating an empty database to harvest %v from: %w", schemas, err)
	}
	defer func() {
		if cerr := emptyDB.Close(ctx); cerr != nil && err == nil {
			err = cerr
		}
	}()

	plan, err := diff.Generate(ctx,
		diff.DBSchemaSource(emptyDB.ConnPool),
		diff.DBSchemaSource(liveDB),
		diff.WithTempDbFactory(d.tempFactory),
		diff.WithIncludeSchemas(schemas...),
		// Nothing to validate: we want the statements, not a promise about them.
		diff.WithDoNotValidatePlan(),
	)
	if err != nil {
		return nil, fmt.Errorf("harvesting DDL for %v: %w", schemas, err)
	}

	out := make([]string, 0, len(plan.Statements))
	for _, s := range plan.Statements {
		// Permissions are not shape. No generated statement's success depends
		// on a grant existing, and a throwaway database holds none of the
		// roles, so every one of these fails with 42704 and is skipped --
		// pure waste that also buries the skips that mean something. The
		// harvest expands them per privilege per grantee, so they dominate:
		// a 2-relation fixture emits 7 grants among 15 statements, and the
		// 84-relation schema that prompted this emitted 5268 statements whose
		// skip list opened with a grant to a role that does not exist.
		if isGrantOrRevoke(s.DDL) {
			continue
		}
		// CONCURRENTLY cannot run inside a transaction and buys nothing on an
		// empty database. The seeded copy only has to satisfy the dependent
		// object's definition, not reproduce its build strategy.
		out = append(out, strings.ReplaceAll(s.DDL, " CONCURRENTLY", ""))
	}
	return out, nil
}

// isGrantOrRevoke reports whether a harvested statement only moves privileges.
func isGrantOrRevoke(ddl string) bool {
	t := strings.ToUpper(strings.TrimLeft(ddl, " \t\n\r"))
	return strings.HasPrefix(t, "GRANT ") || strings.HasPrefix(t, "REVOKE ")
}

// seedTempDatabases arms the factory so every temp database it creates from now
// on is pre-populated with ddl. Returns a function that disarms it.
//
// Seeding is armed only around the validation retry. The factory also hands out
// temp databases for parsing desired DDL, and seeding those would pay the import
// cost several times per plan for no benefit -- and would put objects in the
// parse database that the desired DDL never mentions.
func (d *Differ) seedTempDatabases(ddl []string) func() {
	d.seedMu.Lock()
	d.seedDDL = ddl
	d.seedMu.Unlock()
	return func() {
		d.seedMu.Lock()
		d.seedDDL = nil
		d.seedMu.Unlock()
	}
}

// applySeed runs the armed seed DDL against a freshly created temp database.
//
// Statements that fail are SKIPPED, not fatal. A dependency schema routinely
// contains objects of its own that reach into a third schema -- app_admin holds
// a view over a CDC schema in the database this was built for -- and those
// cannot be created here. Chasing the full transitive closure instead would
// mean importing whole CDC schemas to satisfy a view the plan never touches.
//
// Skipping is safe because the seed is scaffolding, not the thing under test:
// what has to hold is that the TARGET schema rebuilds and the plan converges
// against it. If a skipped object was one the target actually needed, that
// rebuild fails and the plan degrades exactly as it did before any of this
// existed. A skipped object cannot make a bad plan look good.
func (d *Differ) applySeed(ctx context.Context, db *sql.DB) []seedSkip {
	d.seedMu.Lock()
	ddl := d.seedDDL
	d.seedMu.Unlock()

	var skipped []seedSkip
	for _, stmt := range ddl {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			skipped = append(skipped, seedSkip{
				Kind:   statementKind(stmt),
				Detail: fmt.Sprintf("%s (%v)", firstLineOf(stmt), err),
			})
		}
	}
	return skipped
}

// seedSkip is one statement the seed could not run.
//
// Kind is carried separately so the operator gets a breakdown rather than a
// total and one arbitrary example. The total alone was actively misleading: it
// was dominated by grants, and "first" was therefore always a grant, so a
// single table that genuinely failed to build was invisible behind it.
type seedSkip struct {
	Kind   string
	Detail string
}

// statementKind labels a DDL statement for grouping: "CREATE VIEW",
// "ALTER TABLE", "CREATE MATERIALIZED VIEW". Falls back to the first word.
func statementKind(ddl string) string {
	fields := strings.Fields(strings.ToUpper(strings.TrimSpace(ddl)))
	if len(fields) == 0 {
		return "UNKNOWN"
	}
	switch fields[0] {
	case "CREATE", "ALTER", "DROP":
		// Skip the noise words some statements carry between the verb and the
		// object type, so CREATE UNIQUE INDEX groups with CREATE INDEX.
		for _, f := range fields[1:] {
			switch f {
			case "UNIQUE", "OR", "REPLACE", "IF", "NOT", "EXISTS", "TEMP", "TEMPORARY":
				continue
			case "MATERIALIZED":
				return fields[0] + " MATERIALIZED VIEW"
			default:
				return fields[0] + " " + strings.TrimSuffix(f, "(")
			}
		}
		return fields[0]
	default:
		return fields[0]
	}
}

// summarizeSkips renders a skip list as a per-kind breakdown plus one example.
func summarizeSkips(skipped []seedSkip) string {
	counts := map[string]int{}
	var order []string
	for _, s := range skipped {
		if _, seen := counts[s.Kind]; !seen {
			order = append(order, s.Kind)
		}
		counts[s.Kind]++
	}
	sort.Slice(order, func(i, j int) bool {
		if counts[order[i]] != counts[order[j]] {
			return counts[order[i]] > counts[order[j]]
		}
		return order[i] < order[j]
	})
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return fmt.Sprintf("%s (first: %s)", strings.Join(parts, ", "), skipped[0].Detail)
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " ..."
	}
	return strings.TrimSpace(s)
}

// noteSkipped records objects the seed could not create, for the operator.
func (d *Differ) noteSkipped(skipped []seedSkip) {
	if len(skipped) == 0 {
		return
	}
	d.seedMu.Lock()
	d.seedSkipped = append(d.seedSkipped, skipped...)
	d.seedMu.Unlock()
}

// takeSkipped returns and clears the skipped-object list, deduplicated.
//
// The seed is applied to EVERY temp database the retry creates, and a validation
// run creates more than one, so an object that cannot be built is recorded once
// per database. Reporting that raw would tell the operator two views failed when
// one did -- the count is meant to be "objects that could not be built", not
// "failures observed". Identical statement and identical error is the same
// object hitting the same wall in another copy of the same empty database.
func (d *Differ) takeSkipped() []seedSkip {
	d.seedMu.Lock()
	defer d.seedMu.Unlock()
	all := d.seedSkipped
	d.seedSkipped = nil

	seen := make(map[string]struct{}, len(all))
	out := make([]seedSkip, 0, len(all))
	for _, s := range all {
		if _, dup := seen[s.Detail]; dup {
			continue
		}
		seen[s.Detail] = struct{}{}
		out = append(out, s)
	}
	return out
}
