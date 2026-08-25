package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/ags-slc/m8/internal/engine"
	"github.com/spf13/cobra"
)

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show pending migrations without applying them",
	Long:  "Displays what would be applied by 'm8 apply' without making changes. Exit code 2 if migrations are pending.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		_, eng, cleanup, err := connectAndBuildEngine(ctx, true)
		if err != nil {
			return err
		}
		defer cleanup()

		result, err := eng.Plan(ctx)
		if err != nil {
			return err
		}

		output := engine.FormatPlanOutput(result)
		fmt.Print(output)

		// A schema whose diff could not be computed is NOT "pending". Exit 2
		// means "there are changes to apply", and CI gates are built to treat
		// it as success — so reporting an undiffable migration that way lets a
		// broken change read as an ordinary pending one on a pull request and
		// fail later, during apply, after merge. Fail hard instead.
		if names := undiffable(result); len(names) > 0 {
			return fmt.Errorf("could not compute a plan for: %s", strings.Join(names, ", "))
		}

		// Exit code 2 if there are pending migrations (useful for CI gates)
		if hasPending(result) {
			os.Exit(2)
		}
		return nil
	},
}

// undiffable returns the migrations whose schema diff failed to generate.
func undiffable(r *engine.ApplyResult) []string {
	var names []string
	for _, s := range r.Schema {
		if s.Error != nil {
			names = append(names, s.Migration.Filename)
		}
	}
	return names
}

func hasPending(r *engine.ApplyResult) bool {
	if len(r.PendingPGSchemas) > 0 {
		return true
	}
	for _, v := range r.Ops {
		if !v.Skipped {
			return true
		}
	}
	for _, s := range r.Schema {
		if !s.Skipped {
			return true
		}
	}
	for _, v := range r.Logic {
		if !v.Skipped {
			return true
		}
	}
	for _, v := range r.Permissions {
		if !v.Skipped {
			return true
		}
	}
	return false
}

func init() {
	rootCmd.AddCommand(planCmd)
}
