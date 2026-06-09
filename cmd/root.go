package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/ags-slc/m8/internal/config"
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
	flagShadowURL     string
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
	rootCmd.PersistentFlags().StringVar(&flagShadowURL, "shadow-url", "", "PostgreSQL connection URL for an isolated instance to host schema-diff temp databases (env: SHADOW_DATABASE_URL). Strongly recommended against production; if unset, temp DBs are created on the target.")
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

// loadConfig loads .m8.yaml from the current directory (if it exists).
func loadConfig() *config.Config {
	cfg, err := config.Load(".m8.yaml")
	if err != nil {
		slog.Warn("failed to load .m8.yaml", "error", err)
		return &config.Config{}
	}
	return cfg
}

// resolveConnStr builds a PostgreSQL connection string.
// Priority: flag > env > .m8.yaml > default
func resolveConnStr() string {
	cfg := loadConfig()

	// --database-url or DATABASE_URL or config takes highest priority
	if flagDatabaseURL != "" {
		return flagDatabaseURL
	}
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	if cfg.DatabaseURL != "" {
		return cfg.DatabaseURL
	}

	host := coalesce(flagHost, os.Getenv("PGHOST"), cfg.Host, "localhost")
	port := flagPort
	if port == 0 {
		if p := os.Getenv("PGPORT"); p != "" {
			fmt.Sscanf(p, "%d", &port)
		}
		if port == 0 && cfg.Port > 0 {
			port = cfg.Port
		}
		if port == 0 {
			port = 5432
		}
	}
	database := coalesce(flagDatabase, os.Getenv("PGDATABASE"), cfg.Database, "")
	user := coalesce(flagUser, os.Getenv("PGUSER"), cfg.User, "")
	password := coalesce(flagPassword, os.Getenv("PGPASSWORD"), cfg.Password, "")
	sslmode := coalesce(flagSSLMode, os.Getenv("PGSSLMODE"), cfg.SSLMode, "prefer")

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

// resolveShadowConnStr returns the connection string for the instance that
// hosts schema-diff temp databases. Priority: flag > SHADOW_DATABASE_URL env >
// .m8.yaml shadow_url. Returns "" when none is configured (temp DBs then land
// on the target instance).
func resolveShadowConnStr() string {
	if flagShadowURL != "" {
		return flagShadowURL
	}
	if url := os.Getenv("SHADOW_DATABASE_URL"); url != "" {
		return url
	}
	if cfg := loadConfig(); cfg.ShadowURL != "" {
		return cfg.ShadowURL
	}
	return ""
}

// resolveMigrationsDir returns the migrations directory from flag or config.
func resolveMigrationsDir() string {
	cfg := loadConfig()
	if flagMigrationsDir != "migrations" {
		return flagMigrationsDir // explicit flag overrides
	}
	if cfg.MigrationsDir != "" {
		return cfg.MigrationsDir
	}
	return flagMigrationsDir
}

// resolveStrict returns the strict setting from flag or config.
func resolveStrict() bool {
	if flagStrict {
		return true
	}
	cfg := loadConfig()
	return cfg.Strict
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
	// Disable statement_timeout for schema diffing operations (CREATE/DROP temp DBs)
	sqlDB.ExecContext(ctx, "SET statement_timeout = 0")

	// Schema differ (may fail if we lack CREATE DATABASE privilege — non-fatal)
	var differ *schema.Differ
	shadowConnStr := resolveShadowConnStr()
	if shadowConnStr == "" {
		slog.Warn("no shadow instance configured: schema-diff temp databases will be created on the TARGET instance " +
			"(set --shadow-url / SHADOW_DATABASE_URL to an isolated non-production instance to avoid CREATE/DROP DATABASE churn on the target)")
	} else {
		slog.Info("schema-diff temp databases will be created on the configured shadow instance")
	}
	d, err := schema.NewDiffer(ctx, connStr, shadowConnStr)
	if err != nil {
		// Log but don't fail — S__ migrations just won't be diffed
		slog.Warn("schema differ unavailable (S__ migrations will be skipped)", "error", err)
	} else {
		differ = d
		// Best-effort: remove any invalid temp databases left behind by a
		// previously interrupted diff (datconnlimit = -2). Safe to run anytime —
		// invalid databases cannot be in use.
		if n, serr := d.SweepInvalidTempDBs(ctx); serr != nil {
			slog.Warn("failed to sweep orphaned schema-diff temp databases", "error", serr)
		} else if n > 0 {
			slog.Info("swept orphaned schema-diff temp databases", "dropped", n)
		}
	}

	logger := slog.Default()
	eng := engine.New(conn, sqlDB, differ, &engine.Config{
		MigrationsDir: resolveMigrationsDir(),
		ConnStr:       connStr,
		Strict:        resolveStrict(),
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
