package cmd

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

var (
	flagHost          string
	flagPort          int
	flagDatabase      string
	flagUser          string
	flagPassword      string
	flagSSLMode       string
	flagMigrationsDir string
	flagDatabaseURL   string
	flagJSON          bool
)

var rootCmd = &cobra.Command{
	Use:   "m8",
	Short: "PostgreSQL migration tool",
	Long:  "m8 (mate) -- a PostgreSQL-specific migration tool with versioned, repeatable, and schema migrations.",
}

func init() {
	cobra.OnInitialize(loadEnv)

	rootCmd.PersistentFlags().StringVar(&flagHost, "host", "", "PostgreSQL host (env: PGHOST, default: localhost)")
	rootCmd.PersistentFlags().IntVar(&flagPort, "port", 0, "PostgreSQL port (env: PGPORT, default: 5432)")
	rootCmd.PersistentFlags().StringVar(&flagDatabase, "database", "", "PostgreSQL database name (env: PGDATABASE)")
	rootCmd.PersistentFlags().StringVar(&flagUser, "user", "", "PostgreSQL user (env: PGUSER)")
	rootCmd.PersistentFlags().StringVar(&flagPassword, "password", "", "PostgreSQL password (env: PGPASSWORD)")
	rootCmd.PersistentFlags().StringVar(&flagSSLMode, "sslmode", "", "PostgreSQL SSL mode (env: PGSSLMODE, default: prefer)")
	rootCmd.PersistentFlags().StringVar(&flagDatabaseURL, "database-url", "", "PostgreSQL connection URL (overrides individual flags)")
	rootCmd.PersistentFlags().StringVar(&flagMigrationsDir, "migrations-dir", "migrations", "Path to migrations directory")
	rootCmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "Output in JSON format")
}

func loadEnv() {
	_ = godotenv.Load()
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
