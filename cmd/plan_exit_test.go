package cmd

import (
	"errors"
	"testing"

	"github.com/ags-slc/m8/internal/engine"
	"github.com/ags-slc/m8/internal/migration"
	"github.com/ags-slc/m8/internal/reference"
	"github.com/ags-slc/m8/internal/schema"
)

// A migration whose diff could not be generated must never be reported the way
// pending changes are. Exit code 2 means "there are changes to apply" and CI
// gates treat it as success, so classifying an undiffable migration as pending
// lets a broken change pass review and fail during apply, after merge.
func TestUndiffableIsNotPending(t *testing.T) {
	broken := &migration.Migration{Filename: "schema/materialized/rpt_thing.sql"}
	ok := &migration.Migration{Filename: "schema/materialized/ref_thing.sql"}

	r := &engine.ApplyResult{
		Schema: []engine.SchemaResult{
			{Migration: broken, Error: errors.New("failed to generate schema diff")},
			{Migration: ok, Skipped: true},
		},
	}

	names := undiffable(r)
	if len(names) != 1 || names[0] != "schema/materialized/rpt_thing.sql" {
		t.Errorf("undiffable() = %v, want the one broken file", names)
	}
}

func TestUndiffableEmptyWhenAllDiffsSucceed(t *testing.T) {
	r := &engine.ApplyResult{
		Schema: []engine.SchemaResult{
			{Migration: &migration.Migration{Filename: "a.sql"}, Skipped: true},
			{Migration: &migration.Migration{Filename: "b.sql"}, Diff: schemaDiffWithChanges()},
		},
	}
	if names := undiffable(r); len(names) != 0 {
		t.Errorf("undiffable() = %v, want none", names)
	}
	if !hasPending(r) {
		t.Error("a migration with a non-skipped diff should count as pending")
	}
}

func schemaDiffWithChanges() *schema.DiffResult {
	return &schema.DiffResult{
		Statements: []schema.DiffStatement{{DDL: "ALTER TABLE x ADD COLUMN y text"}},
		HasChanges: true,
	}
}

// A pending data/ migration must set exit code 2 like any other pending work.
// The gate is what tells a reviewer the merge will change the database; a phase
// invisible to it applies unannounced.
func TestPendingDataMigrationIsPending(t *testing.T) {
	r := &engine.ApplyResult{
		Data: []engine.MigrationResult{
			{Migration: &migration.Migration{Filename: "data/20260828_001__backfill.sql"}},
		},
	}
	if !hasPending(r) {
		t.Error("a non-skipped data/ migration should count as pending")
	}
}

func TestAppliedDataMigrationIsNotPending(t *testing.T) {
	r := &engine.ApplyResult{
		Data: []engine.MigrationResult{
			{Migration: &migration.Migration{Filename: "data/20260828_001__backfill.sql"}, Skipped: true},
		},
	}
	if hasPending(r) {
		t.Error("an already-applied data/ migration should not count as pending")
	}
}

// A reference/ file sets exit code 2 when it would write rows, and when plan
// cannot diff it yet because schema/ creates its table -- and only then: a
// converged file must not keep the gate permanently red.
func TestReferencePendingOnlyWhenItWouldWrite(t *testing.T) {
	converged := &reference.Diff{}
	if hasPending(&engine.ApplyResult{Reference: []*reference.Diff{converged}}) {
		t.Error("a converged reference/ file counted as pending")
	}
	for name, d := range map[string]*reference.Diff{
		"insert":  {Inserts: []reference.Row{{}}},
		"update":  {Updates: []reference.Change{{}}},
		"delete":  {Deletes: []reference.Row{{}}},
		"pending": {Pending: "table x does not exist yet"},
	} {
		if !hasPending(&engine.ApplyResult{Reference: []*reference.Diff{converged, d}}) {
			t.Errorf("%s: not counted as pending", name)
		}
	}
}
