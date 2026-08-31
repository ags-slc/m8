package schema

import (
	"context"
	"database/sql"
	"fmt"
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
		// CONCURRENTLY cannot run inside a transaction and buys nothing on an
		// empty database. The seeded copy only has to satisfy the dependent
		// object's definition, not reproduce its build strategy.
		out = append(out, strings.ReplaceAll(s.DDL, " CONCURRENTLY", ""))
	}
	return out, nil
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
func (d *Differ) applySeed(ctx context.Context, db *sql.DB) []string {
	d.seedMu.Lock()
	ddl := d.seedDDL
	d.seedMu.Unlock()

	var skipped []string
	for _, stmt := range ddl {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", firstLineOf(stmt), err))
		}
	}
	return skipped
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " ..."
	}
	return strings.TrimSpace(s)
}

// noteSkipped records objects the seed could not create, for the operator.
func (d *Differ) noteSkipped(skipped []string) {
	if len(skipped) == 0 {
		return
	}
	d.seedMu.Lock()
	d.seedSkipped = append(d.seedSkipped, skipped...)
	d.seedMu.Unlock()
}

// takeSkipped returns and clears the skipped-object list.
func (d *Differ) takeSkipped() []string {
	d.seedMu.Lock()
	defer d.seedMu.Unlock()
	out := d.seedSkipped
	d.seedSkipped = nil
	return out
}
