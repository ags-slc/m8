package cmd

import (
	"fmt"

	"github.com/ags-slc/m8/internal/engine"
	"github.com/spf13/cobra"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "One-time convergence of an existing database to match migration files",
	Long: `Diffs schema/ files against the live database and applies changes,
then applies all logic/ and permissions/ files. Ops/ files are baselined
(marked as applied without executing).

Use this for brownfield adoption: you have an existing database and want
m8 to bring it in line with your migration files. After sync, use 'apply'
for ongoing changes.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		_, eng, cleanup, err := connectAndBuildEngine(ctx, true)
		if err != nil {
			return err
		}
		defer cleanup()

		result, err := eng.Sync(ctx)
		if result != nil {
			fmt.Print(engine.FormatApplyOutput(result))
		}
		if err != nil {
			return err
		}

		fmt.Println("\nSync complete. Use 'm8 apply' for ongoing changes.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(syncCmd)
}
