package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var applyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply pending migrations to the database",
	Long:  "Discovers pending versioned, schema, and repeatable migrations, then applies them in order (V -> S -> R).",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("m8 apply: not yet implemented")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(applyCmd)
}
