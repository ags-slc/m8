package schema

import (
	"errors"
	"fmt"
	"testing"
)

// wrapAsPgSchemaDiff reproduces how pg-schema-diff v1.0.5 reports a
// plan-validation failure: assertValidPlan's phase error, wrapped once by
// Generate with the plan pretty-printed after it.
//
//	return Plan{}, fmt.Errorf("validating migration plan: %w \n%# v", err, pretty.Formatter(plan))
func wrapAsPgSchemaDiff(inner error, prettyPlan string) error {
	return fmt.Errorf("validating migration plan: %w \n%s", inner, prettyPlan)
}

func TestIsValidationFailureDegradesOnlyOnTheShadowRebuild(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		want  bool
		guard string
	}{
		{
			name: "cross-schema rebuild of the current schema",
			err: wrapAsPgSchemaDiff(
				fmt.Errorf("inserting schema in temporary database: %w",
					errors.New(`executing statements: ERROR: relation "elsewhere.source_rows" does not exist`)),
				"diff.Plan{Statements: []diff.Statement{...}}"),
			want:  true,
			guard: "this is the only phase scoping the diff to one schema can break",
		},
		{
			name: "generated DDL does not execute",
			err: wrapAsPgSchemaDiff(
				fmt.Errorf("running migration plan: %w",
					errors.New(`ERROR: relation "widget_seq_seq" does not exist (SQLSTATE 42P01)`)),
				"diff.Plan{Statements: []diff.Statement{...}}"),
			want:  false,
			guard: "degrading here turns 'this plan dies halfway through your primary' into a printed warning",
		},
		{
			name: "plan does not converge to the desired state",
			err: wrapAsPgSchemaDiff(
				errors.New("validating plan failed. diff detected:\nALTER TABLE \"public\".\"probe\" ADD COLUMN \"note\" text"),
				"diff.Plan{Statements: []diff.Statement{...}}"),
			want:  false,
			guard: "degrading here hides that the plan does not reach the declared schema",
		},
		{
			name: "introspecting the rebuilt database failed",
			err: wrapAsPgSchemaDiff(
				fmt.Errorf("fetching schema from migrated database: %w", errors.New("connection reset by peer")),
				"diff.Plan{Statements: []diff.Statement{...}}"),
			want: false,
		},
		{
			name: "not a validation failure at all",
			err:  fmt.Errorf("getting current schema: %w", errors.New("permission denied for schema materialized")),
			want: false,
		},
		{
			// The wrap embeds pretty.Formatter(plan) -- every DDL statement in
			// the plan. A strings.Contains test on the phase name is therefore
			// satisfiable by DDL text, which is why the match is anchored.
			name: "DDL text that mentions the degradable phase must not fool the match",
			err: wrapAsPgSchemaDiff(
				fmt.Errorf("running migration plan: %w", errors.New("SQLSTATE 42P01")),
				`diff.Plan{Statements: []diff.Statement{{DDL: "COMMENT ON TABLE \"public\".\"t\" IS 'inserting schema in temporary database'"}}}`),
			want:  false,
			guard: "a Contains match reads the plan's own DDL as the failing phase",
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidationFailure(tt.err); got != tt.want {
				t.Errorf("isValidationFailure() = %v, want %v\n  err: %v\n  why: %s",
					got, tt.want, tt.err, tt.guard)
			}
		})
	}
}
