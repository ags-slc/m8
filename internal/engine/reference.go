package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ags-slc/m8/internal/migration"
	"github.com/ags-slc/m8/internal/pgident"
	"github.com/ags-slc/m8/internal/reference"
	"github.com/jackc/pgx/v5"
)

// referenceLockTimeout bounds the wait for a lock when --lock-timeout is not
// set. Plan reads the target and apply writes rows an application may hold
// locked; left unbounded, either queues behind a long transaction instead of
// failing.
const referenceLockTimeout = 3 * time.Second

// discover finds every migration file and parses the reference/ ones, which
// needs no database. Every command calls it before its first phase, so a
// malformed reference/ file stops the run before anything has been changed.
func (e *Engine) discover() ([]*migration.Migration, []reference.Spec, error) {
	all, err := migration.Discover(e.config.MigrationsDir)
	if err != nil {
		return nil, nil, err
	}
	var specs []reference.Spec
	for _, m := range filterByType(all, migration.TypeReference) {
		s, err := reference.Parse(m.Filename, m.Content)
		if err != nil {
			return nil, nil, err
		}
		specs = append(specs, s)
	}
	return all, specs, nil
}

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
	tx, err := e.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	lockTimeout := referenceLockTimeout
	if e.config.LockTimeout > 0 {
		lockTimeout = e.config.LockTimeout
	}
	if err := setLocalTimeouts(ctx, tx, lockTimeout, e.config.StatementTimeout); err != nil {
		return nil, err
	}

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

// setLocalTimeouts bounds the rest of tx. A zero duration leaves that timeout
// as it is.
func setLocalTimeouts(ctx context.Context, tx pgx.Tx, lock, statement time.Duration) error {
	for name, d := range map[string]time.Duration{"lock_timeout": lock, "statement_timeout": statement} {
		if d <= 0 {
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL %s = '%dms'", name, d.Milliseconds())); err != nil {
			return fmt.Errorf("failed to set %s: %w", name, err)
		}
	}
	return nil
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
