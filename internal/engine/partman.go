package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ags-slc/m8/internal/migration"
	"github.com/ags-slc/m8/internal/partman"
)

// PartmanResult holds the outcome of reconciling one table's declared
// pg_partman configuration.
type PartmanResult struct {
	Action  partman.Action
	ExecMs  int64
	Applied bool
	Error   error
}

// collectPartmanSpecs reads the m8:partman directives out of every schema/
// file. A table may be declared once: two directives for it would each be
// reconciled in turn, and the run would end on whichever came second.
func collectPartmanSpecs(migrations []*migration.Migration) ([]partman.Spec, error) {
	var specs []partman.Spec
	seen := map[string]string{}
	for _, m := range migrations {
		found, err := partman.Parse(m.Content, m.PGSchema)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Filename, err)
		}
		for _, s := range found {
			if prev, ok := seen[s.QualifiedName()]; ok {
				return nil, fmt.Errorf("%s: m8:partman %s is already declared in %s", m.Filename, s.QualifiedName(), prev)
			}
			seen[s.QualifiedName()] = m.Filename
			specs = append(specs, s)
		}
	}
	return specs, nil
}

// planPartman reports what applying the declared pg_partman configuration
// would do. It runs after the schema diff in the phase order, and reads the
// database as it stands: a table the schema phase has yet to create is
// reported as one create_parent will register.
func (e *Engine) planPartman(ctx context.Context, specs []partman.Spec) ([]PartmanResult, error) {
	actions, err := partman.Plan(ctx, e.conn, specs)
	if err != nil {
		return nil, err
	}
	results := make([]PartmanResult, len(actions))
	for i, a := range actions {
		results[i] = PartmanResult{Action: a, Error: a.Err}
	}
	return results, nil
}

// refusePartman fails the run, before anything in schema/ is applied, if a
// declaration would be refused. applyPartman checks again after the schema
// phase; this is the check that keeps a refusal from landing as a partial
// apply -- the schema half committed, the pg_partman half refused.
func (e *Engine) refusePartman(ctx context.Context, specs []partman.Spec) error {
	results, err := e.planPartman(ctx, specs)
	if err != nil {
		return err
	}
	for _, r := range results {
		if r.Error != nil {
			return fmt.Errorf("pg_partman: %w", r.Error)
		}
	}
	return nil
}

// applyPartman registers declared tables with pg_partman and brings part_config
// in line with the declarations. It runs after the schema phase, so the parent
// table and its indexes exist before create_parent makes the first children --
// which then inherit those indexes rather than needing them built afterwards.
//
// Every table is planned before any is touched: a refusal on one table refuses
// the run, for the same reason a schema diff that cannot be computed does.
func (e *Engine) applyPartman(ctx context.Context, specs []partman.Spec) ([]PartmanResult, error) {
	results, err := e.planPartman(ctx, specs)
	if err != nil || len(results) == 0 {
		return results, err
	}
	for _, r := range results {
		if r.Error != nil {
			return results, fmt.Errorf("pg_partman: %w", r.Error)
		}
		if !r.Action.Installed {
			return results, errors.New("pg_partman: schema/ declares m8:partman tables, but the pg_partman extension is not installed; " +
				"create it in an ops/ migration (CREATE EXTENSION pg_partman SCHEMA partman), which runs before schema/")
		}
	}

	for i := range results {
		r := &results[i]
		if len(r.Action.Statements) == 0 {
			continue
		}
		start := time.Now()
		r.Error = e.execInTx(ctx, r.Action.Statements)
		r.ExecMs = time.Since(start).Milliseconds()
		if r.Error != nil {
			return results, fmt.Errorf("pg_partman %s: %w", r.Action.Spec.QualifiedName(), r.Error)
		}
		r.Applied = true
		e.logger.Info("applied", "type", "partman", "table", r.Action.Spec.QualifiedName(), "ms", r.ExecMs, "statements", len(r.Action.Statements))
	}
	return results, nil
}

// execInTx runs statements as one transaction: create_parent and the UPDATE
// that follows it land together or not at all, so a failure never leaves a
// table registered with pg_partman's defaults instead of its declaration.
func (e *Engine) execInTx(ctx context.Context, stmts []string) error {
	tx, err := e.beginBounded(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("%w\nSQL: %s", err, s)
		}
	}
	return tx.Commit(ctx)
}
