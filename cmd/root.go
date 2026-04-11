package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/ags-slc/m8/internal/engine"
	"github.com/ags-slc/m8/internal/schema"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // register pgx as database/sql driver
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
	flagStrict        bool
	flagJSON          bool
)

var rootCmd = &cobra.Command{
	Use:   "m8",
	Short: "PostgreSQL migration tool",
	Long:  "m8 (mate) -- a PostgreSQL-specific migration tool with schema, logic, permissions, and ops migrations.",
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
	rootCmd.PersistentFlags().BoolVar(&flagStrict, "strict", false, "Include DROP statements for DB objects not declared in migration files")
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

// resolveConnStr builds a PostgreSQL connection string from flags, env vars, and defaults.
func resolveConnStr() string {
	// --database-url or DATABASE_URL takes highest priority
	if flagDatabaseURL != "" {
		return flagDatabaseURL
	}
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}

	host := coalesce(flagHost, os.Getenv("PGHOST"), "localhost")
	port := flagPort
	if port == 0 {
		if p := os.Getenv("PGPORT"); p != "" {
			fmt.Sscanf(p, "%d", &port)
		}
		if port == 0 {
			port = 5432
		}
	}
	database := coalesce(flagDatabase, os.Getenv("PGDATABASE"), "")
	user := coalesce(flagUser, os.Getenv("PGUSER"), "")
	password := coalesce(flagPassword, os.Getenv("PGPASSWORD"), "")
	sslmode := coalesce(flagSSLMode, os.Getenv("PGSSLMODE"), "prefer")

	connStr := fmt.Sprintf("host=%s port=%d sslmode=%s", host, port, sslmode)
	if database != "" {
		connStr += fmt.Sprintf(" dbname=%s", database)
	}
	if user != "" {
		connStr += fmt.Sprintf(" user=%s", user)
	}
	if password != "" {
		connStr += fmt.Sprintf(" password=%s", password)
	}
	return connStr
}

// connectAndBuildEngine creates a pgx connection, sql.DB, schema differ, and engine.
func connectAndBuildEngine(ctx context.Context) (*pgx.Conn, *engine.Engine, func(), error) {
	connStr := resolveConnStr()

	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// database/sql connection for pg-schema-diff
	sqlDB, err := sql.Open("pgx", connStr)
	if err != nil {
		conn.Close(ctx)
		return nil, nil, nil, fmt.Errorf("failed to open sql.DB: %w", err)
	}

	// Schema differ (may fail if we lack CREATE DATABASE privilege — non-fatal)
	var differ *schema.Differ
	d, err := schema.NewDiffer(ctx, connStr)
	if err != nil {
		// Log but don't fail — S__ migrations just won't be diffed
		slog.Warn("schema differ unavailable (S__ migrations will be skipped)", "error", err)
	} else {
		differ = d
	}

	logger := slog.Default()
	eng := engine.New(conn, sqlDB, differ, &engine.Config{
		MigrationsDir: flagMigrationsDir,
		ConnStr:       connStr,
		Strict:        flagStrict,
	}, logger)

	cleanup := func() {
		if differ != nil {
			differ.Close()
		}
		sqlDB.Close()
		conn.Close(ctx)
	}

	return conn, eng, cleanup, nil
}

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
