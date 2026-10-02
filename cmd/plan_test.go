package cmd

import (
	"errors"
	"testing"

	"github.com/ags-slc/m8/internal/engine"
	"github.com/ags-slc/m8/internal/partman"
)

// A pg_partman change is a pending change, and a refused declaration is a plan
// that cannot be computed: exit 2 for the one, an error for the other. Exit 0
// for a partman-only change would read in CI as "nothing to apply", and a
// refusal reported as anything but an error is found by apply, after merge.
func TestPlanExitCodesCountPartman(t *testing.T) {
	spec := partman.Spec{Schema: "public", Table: "event_log"}

	pending := &engine.ApplyResult{Partman: []engine.PartmanResult{{Action: partman.Action{Spec: spec, Statements: []string{"UPDATE ..."}}}}}
	if !hasPending(pending) {
		t.Error("a partman change is not reported as pending")
	}

	clean := &engine.ApplyResult{Partman: []engine.PartmanResult{{Action: partman.Action{Spec: spec}}}}
	if hasPending(clean) {
		t.Error("a matching partman declaration is reported as pending")
	}

	refused := &engine.ApplyResult{Partman: []engine.PartmanResult{{Action: partman.Action{Spec: spec}, Error: errors.New("cannot change the interval")}}}
	if names := undiffable(refused); len(names) != 1 || names[0] != "public.event_log (partman)" {
		t.Errorf("a refused declaration is not reported as undiffable: %v", names)
	}
}
