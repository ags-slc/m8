package cmd

import (
	"fmt"
	"os"

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

		// Exit code 2 if there are pending migrations (useful for CI gates)
		if hasPending(result) {
			os.Exit(2)
		}
		return nil
	},
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
