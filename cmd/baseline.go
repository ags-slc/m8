package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	baselineVersion string
	baselineAll     bool
)

var baselineCmd = &cobra.Command{
	Use:   "baseline",
	Short: "Mark existing migrations as applied without running them",
	Long:  "Records migrations as already applied in _m8.history without executing them. Used for adopting m8 on an existing database.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if !baselineAll && baselineVersion == "" {
			return fmt.Errorf("either --all or --version is required")
		}

		ctx := cmd.Context()

		_, eng, cleanup, err := connectAndBuildEngine(ctx)
		if err != nil {
			return err
		}
		defer cleanup()

		if err := eng.Baseline(ctx, baselineVersion, baselineAll); err != nil {
			return err
		}

		fmt.Println("Baseline complete.")
		return nil
	},
}

func init() {
	baselineCmd.Flags().StringVar(&baselineVersion, "version", "", "Mark all versioned migrations up to this version as applied")
	baselineCmd.Flags().BoolVar(&baselineAll, "all", false, "Mark all discovered migrations as applied")
	rootCmd.AddCommand(baselineCmd)
}
