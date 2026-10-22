package engine

import (
	"context"
	"strings"

	"github.com/ags-slc/m8/internal/pgident"
	"github.com/ags-slc/m8/internal/reference"
)

// reconcileReference diffs the declared rows against the live tables and, when
// changing is nil, writes the difference. A plan passes changing: whether this
// release's schema/ changes a table, which plan cannot diff against yet. Every
// file runs in one transaction, so a failure in one leaves every table as it
// was; a plan rolls it back, and the only thing it wrote is the temporary
// tables the diff is read from.
func (e *Engine) reconcileReference(ctx context.Context, specs []reference.Spec, changing func(nsp, rel string) bool) ([]*reference.Diff, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	tx, err := e.beginBounded(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	diffs, err := reference.Plan(ctx, tx, specs, changing)
	if changing != nil {
		return diffs, err
	}
	// An apply returns no diffs with its error: the transaction rolls back, so
	// nothing was written, and FormatApplyOutput would report every non-empty
	// diff as applied.
	if err != nil {
		return nil, err
	}
	if err := reference.Apply(ctx, tx, diffs); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	for _, d := range diffs {
		if !d.Empty() {
			e.logger.Info("applied", "file", d.Spec.File, "type", "reference", "table", d.Table,
				"inserted", len(d.Inserts), "updated", len(d.Updates), "deleted", len(d.Deletes))
		}
	}
	return diffs, nil
}

// schemaChanges reports whether a planned schema diff touches a table.
// pg-schema-diff schema-qualifies and quotes every table it emits, so a table
// appears in a statement as "schema"."name" -- which a column or a same-named
// table in another schema does not match.
func schemaChanges(r *ApplyResult) func(nsp, rel string) bool {
	var ddl strings.Builder
	for _, s := range r.Schema {
		if s.Diff != nil {
			for _, st := range s.Diff.Statements {
				ddl.WriteString(st.DDL)
			}
		}
	}
	text := ddl.String()
	return func(nsp, rel string) bool { return strings.Contains(text, pgident.Qualify(nsp, rel)) }
}
