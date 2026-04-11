package cmd

import (
	"fmt"

	"github.com/ags-slc/m8/internal/engine"
	"github.com/spf13/cobra"
)

var applyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply pending migrations to the database",
	Long:  "Discovers pending versioned, schema, and repeatable migrations, then applies them in order (V -> S -> R).",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		_, eng, cleanup, err := connectAndBuildEngine(ctx)
		if err != nil {
			return err
		}
		defer cleanup()

		result, err := eng.Apply(ctx)
		if result != nil {
			fmt.Print(engine.FormatApplyOutput(result))
		}
		return err
	},
}

func init() {
	rootCmd.AddCommand(applyCmd)
}
