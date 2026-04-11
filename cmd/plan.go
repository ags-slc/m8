package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show pending migrations without applying them",
	Long:  "Displays what would be applied by 'm8 apply' without making changes. Exit code 2 if migrations are pending.",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("m8 plan: not yet implemented")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(planCmd)
}
