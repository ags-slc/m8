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
		fmt.Println("m8 baseline: not yet implemented")
		return nil
	},
}

func init() {
	baselineCmd.Flags().StringVar(&baselineVersion, "version", "", "Mark all migrations up to this version as applied")
	baselineCmd.Flags().BoolVar(&baselineAll, "all", false, "Mark all discovered migrations as applied")
	rootCmd.AddCommand(baselineCmd)
}
