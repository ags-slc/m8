package cmd

import (
	"fmt"

	"github.com/ags-slc/m8/internal/engine"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show migration status (applied vs pending)",
	Long:  "Displays all applied and pending migrations, including checksum mismatches for versioned migrations.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		_, eng, cleanup, err := connectAndBuildEngine(ctx)
		if err != nil {
			return err
		}
		defer cleanup()

		result, err := eng.Status(ctx)
		if err != nil {
			return err
		}

		fmt.Print(engine.FormatStatusOutput(result))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
