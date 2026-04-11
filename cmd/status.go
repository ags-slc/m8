package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show migration status (applied vs pending)",
	Long:  "Displays all applied and pending migrations, including checksum mismatches for versioned migrations.",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("m8 status: not yet implemented")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
