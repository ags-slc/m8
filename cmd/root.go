package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ags-slc/m8/internal/config"
	"github.com/ags-slc/m8/internal/engine"
	"github.com/ags-slc/m8/internal/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
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
	// flagFailOnUnvalidated turns an unvalidated plan into a refusal rather
	// than a warning, so a CI gate can be built on the exit code.
	flagFailOnUnvalidated bool
	// Overrides for the per-statement timeouts pg-schema-diff derives from each
	// generated statement's hazards. Zero keeps the derived value.
	flagLockTimeout      time.Duration
	flagStatementTimeout time.Duration
)

var rootCmd = &cobra.Command{
	Use:   "m8",
	Short: "PostgreSQL migration tool",
	Long:  "m8 (mate) -- a PostgreSQL-specific migration tool with schema, logic, permissions, and ops migrations.",
	// A failed connection or migration is not a usage error: don't answer it
	// with the full help text. Execute() is the single place errors are printed.
	SilenceUsage:  true,
	SilenceErrors: true,
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
	rootCmd.PersistentFlags().BoolVar(&flagFailOnUnvalidated, "fail-on-unvalidated", false,
		"Refuse to plan or apply a schema diff whose plan could not be validated (env: M8_FAIL_ON_UNVALIDATED; implied by require_shadow)")
	rootCmd.PersistentFlags().DurationVar(&flagLockTimeout, "lock-timeout", 0,
		"Override the lock_timeout applied to every generated schema statement (default: pg-schema-diff's hazard-derived value, 3s for ordinary DDL)")
	rootCmd.PersistentFlags().DurationVar(&flagStatementTimeout, "statement-timeout", 0,
		"Override the statement_timeout applied to every generated schema statement (default: pg-schema-diff's hazard-derived value, 3s for ordinary DDL / 20m for a concurrent index build)")
}

func loadEnv() {
	_ = godotenv.Load()
}

// Execute runs the root command.
func Execute() {
	// Cancel the command context on SIGINT/SIGTERM instead of dying where we
	// stand: in-flight statements are cancelled and, more importantly, deferred
	// cleanup still runs — including the temp database sweep, which is what
	// keeps an interrupted schema diff from orphaning a database. A second
	// signal restores the default disposition and kills the process outright.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "interrupted")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// loadConfig reads .m8.yaml from the current directory.
//
// A parse failure is FATAL. It used to log slog.Warn and hand back an empty
// config, so a YAML typo silently turned require_shadow -- the one setting whose
// entire job is to refuse a degrade -- back off, and the run carried on against
// the target as if the file had never existed. A missing file is still fine; a
// file that exists and cannot be read or parsed is not.
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(".m8.yaml")
	if err != nil {
		return nil, fmt.Errorf("reading .m8.yaml: %w", err)
	}
	return cfg, nil
}

// settings is the effective configuration for one command invocation: flags,
// environment, and .m8.yaml collapsed in that priority order.
//
// It is resolved once per command. The previous shape re-read .m8.yaml from
// four separate resolvers, which meant the same run could read the file four
// times and, on a parse error, disagree with itself about what it said.
type settings struct {
	ConnStr           string
	ShadowConnStr     string
	MigrationsDir     string
	Strict            bool
	RequireShadow     bool
	FailOnUnvalidated bool
	LockTimeout       time.Duration
	StatementTimeout  time.Duration
}

func resolveSettings() (*settings, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}

	// M8_REQUIRE_SHADOW lets CI enforce the refusal independently of the file,
	// which matters precisely because the file is the thing an editing mistake
	// can disarm.
	requireShadow, err := envBool("M8_REQUIRE_SHADOW", cfg.RequireShadow)
	if err != nil {
		return nil, err
	}
	failOnUnvalidated, err := envBool("M8_FAIL_ON_UNVALIDATED", cfg.FailOnUnvalidated)
	if err != nil {
		return nil, err
	}

	lockTimeout, err := resolveDuration("lock_timeout", flagLockTimeout, cfg.LockTimeout)
	if err != nil {
		return nil, err
	}
	statementTimeout, err := resolveDuration("statement_timeout", flagStatementTimeout, cfg.StatementTimeout)
	if err != nil {
		return nil, err
	}

	return &settings{
		ConnStr:       resolveConnStr(cfg),
		ShadowConnStr: resolveShadowConnStr(cfg),
		MigrationsDir: resolveMigrationsDir(cfg),
		Strict:        flagStrict || cfg.Strict,
		RequireShadow: requireShadow,
		// An unvalidated plan is the same degrade require_shadow exists to
		// forbid, one layer in: the statements were never proved to execute.
		// A repository that refuses to plan without a shadow instance cannot
		// coherently accept applying a plan the shadow never checked.
		FailOnUnvalidated: flagFailOnUnvalidated || failOnUnvalidated || requireShadow,
		LockTimeout:       lockTimeout,
		StatementTimeout:  statementTimeout,
	}, nil
}

// resolveDuration takes the flag when set, otherwise the config value. A config
// value that does not parse is an error rather than a silent zero -- zero here
// means "keep pg-schema-diff's derived timeout", which is not what someone who
// wrote lock_timeout into the file was asking for.
func resolveDuration(key string, flag time.Duration, configured string) (time.Duration, error) {
	if flag > 0 {
		return flag, nil
	}
	if configured == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(configured)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration", key, configured)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: %q is negative", key, configured)
	}
	return d, nil
}

// envBool reads a boolean override from the environment. Unset or empty leaves
// the file's value alone; anything else must parse, so "M8_REQUIRE_SHADOW=ture"
// in a CI job is an error rather than a silent false.
func envBool(name string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(name)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a boolean", name, raw)
	}
	return v, nil
}

// resolveConnStr builds a PostgreSQL connection string.
// Priority: flag > env > .m8.yaml > default
func resolveConnStr(cfg *config.Config) string {
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
			parsed, err := strconv.Atoi(p)
			if err != nil || parsed <= 0 {
				slog.Warn("ignoring unparseable PGPORT", "value", p)
			} else {
				port = parsed
			}
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

// teardownTimeout bounds the cleanup that runs after a command finishes,
// including after an interrupted one. Short enough that a Ctrl-C still returns
// promptly, long enough for a DROP DATABASE to land.
const teardownTimeout = 30 * time.Second

// resolveShadowConnStr returns the connection string for the instance that
// hosts schema-diff temp databases. Priority: flag > SHADOW_DATABASE_URL env >
// .m8.yaml shadow_url. Returns "" when none is configured (temp DBs then land
// on the target instance).
func resolveShadowConnStr(cfg *config.Config) string {
	if flagShadowURL != "" {
		return flagShadowURL
	}
	if url := os.Getenv("SHADOW_DATABASE_URL"); url != "" {
		return url
	}
	return cfg.ShadowURL
}

// resolveMigrationsDir returns the migrations directory from flag or config.
func resolveMigrationsDir(cfg *config.Config) string {
	if flagMigrationsDir != "migrations" {
		return flagMigrationsDir // explicit flag overrides
	}
	if cfg.MigrationsDir != "" {
		return cfg.MigrationsDir
	}
	return flagMigrationsDir
}

// connectAndBuildEngine creates a pgx connection, sql.DB, and engine. When
// needDiffer is true it also constructs the schema differ — which connects to
// the shadow instance and sweeps orphaned temp databases. Commands that never
// diff (status, baseline) pass false so they don't open a shadow connection or
// perform DROP DATABASE side effects.
func connectAndBuildEngine(ctx context.Context, needDiffer bool) (*pgx.Conn, *engine.Engine, func(), error) {
	st, err := resolveSettings()
	if err != nil {
		return nil, nil, nil, err
	}
	connStr := st.ConnStr

	// Checked before connecting: a missing shadow is a configuration error, and
	// there is no reason to open a session on the target -- possibly a
	// production primary -- only to refuse a moment later.
	if needDiffer && st.ShadowConnStr == "" && st.RequireShadow {
		// A warning is not enough when the target is a production primary:
		// falling back means CREATE DATABASE / DROP DATABASE churn on it.
		// Repositories that can never accept that set require_shadow in
		// .m8.yaml, and this turns the degrade into a refusal.
		return nil, nil, nil, fmt.Errorf(
			"require_shadow is set in .m8.yaml but no shadow instance is configured: " +
				"set --shadow-url or SHADOW_DATABASE_URL to an isolated non-production instance")
	}

	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// database/sql handle pg-schema-diff introspects the live target through.
	sqlDB, err := openTargetPool(connStr)
	if err != nil {
		_ = conn.Close(ctx)
		return nil, nil, nil, err
	}

	var differ *schema.Differ
	if needDiffer {
		shadowConnStr := st.ShadowConnStr
		if shadowConnStr == "" {
			slog.Warn("no shadow instance configured: schema-diff temp databases will be created on the TARGET instance " +
				"(set --shadow-url / SHADOW_DATABASE_URL to an isolated non-production instance to avoid CREATE/DROP DATABASE churn on the target)")
		} else {
			slog.Info("schema-diff temp databases will be created on the configured shadow instance")
		}
		d, derr := schema.NewDiffer(ctx, connStr, shadowConnStr)
		if derr != nil {
			if shadowConnStr != "" {
				// An explicitly configured shadow that fails must not silently
				// disable schema diffing — fail loudly rather than skip S__ work.
				_ = sqlDB.Close()
				_ = conn.Close(ctx)
				return nil, nil, nil, fmt.Errorf("schema differ unavailable with configured shadow instance: %w", derr)
			}
			// No shadow configured: degrade. The engine still refuses to skip
			// schema migrations silently (see errDifferUnavailable).
			slog.Warn("schema differ unavailable (schema migrations cannot be diffed)", "error", derr)
		} else {
			differ = d
			// Best-effort: remove any invalid temp databases left behind by a
			// previously interrupted diff (datconnlimit = -2). Safe to run anytime —
			// invalid databases cannot be in use.
			if n, serr := d.SweepInvalidTempDBs(ctx); serr != nil {
				slog.Warn("failed to sweep orphaned schema-diff temp databases", "error", serr)
			} else if n > 0 {
				slog.Info("swept orphaned invalid schema-diff temp databases", "dropped", n)
			}
			// On a dedicated shadow instance, also reclaim valid temp databases
			// abandoned by a process that died before its drop. Restricted to an
			// explicit shadow so we never auto-drop valid databases on the target.
			if shadowConnStr != "" {
				if n, serr := d.SweepStaleTempDBs(ctx, schema.StaleTempDBTTL); serr != nil {
					slog.Warn("failed to sweep stale schema-diff temp databases", "error", serr)
				} else if n > 0 {
					slog.Info("swept stale schema-diff temp databases", "dropped", n)
				}
			}
		}
	}

	logger := slog.Default()
	eng := engine.New(conn, sqlDB, differ, &engine.Config{
		MigrationsDir:     st.MigrationsDir,
		ConnStr:           connStr,
		Strict:            st.Strict,
		FailOnUnvalidated: st.FailOnUnvalidated,
		LockTimeout:       st.LockTimeout,
		StatementTimeout:  st.StatementTimeout,
	}, logger)

	cleanup := func() {
		// Teardown runs on a context detached from the command's, so an
		// interrupted run still cleans up after itself.
		teardownCtx, cancelTeardown := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
		defer cancelTeardown()

		if differ != nil {
			_ = differ.Close()
			// pg-schema-diff issues its own DROP DATABASE on the caller's context, so
			// a cancelled run leaves the temp database behind — invalid if the drop
			// was interrupted mid-flight, perfectly valid if it never got sent at all.
			// Reclaim this run's own databases by name, which covers both and cannot
			// touch a database belonging to another m8 process.
			if n, serr := differ.DropCreatedTempDBs(teardownCtx); serr != nil {
				slog.Warn("failed to reclaim schema-diff temp databases left by this run", "error", serr)
			} else if n > 0 {
				slog.Info("reclaimed schema-diff temp databases left by this run", "dropped", n)
			}
			// Then anything an earlier run left invalid, which is always safe to drop.
			if n, serr := differ.SweepInvalidTempDBs(teardownCtx); serr != nil {
				slog.Warn("post-run sweep of invalid schema-diff temp databases failed", "error", serr)
			} else if n > 0 {
				slog.Info("swept orphaned invalid schema-diff temp databases", "dropped", n)
			}
		}
		_ = sqlDB.Close()
		_ = conn.Close(teardownCtx)
	}

	return conn, eng, cleanup, nil
}

// openTargetPool opens the pool pg-schema-diff reads the live schema through,
// with statement_timeout disabled.
//
// The timeout is disabled in the connection configuration rather than by running
// "SET statement_timeout = 0" against the pool. A SET executes on one arbitrary
// pooled connection, which database/sql may never hand out again — every other
// connection the pool opens keeps whatever the role or database default says. A
// runtime parameter is sent in each connection's startup packet, so it holds for
// all of them. (The differ's own connections do the same thing through their
// connection strings; only introspection runs through this pool, since temp
// database DDL happens on the shadow instance.)
func openTargetPool(connStr string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["statement_timeout"] = "0"
	db := stdlib.OpenDB(*cfg)
	// pg-schema-diff introspects the target through this pool and fans out one
	// query per relation. database/sql defaults to an UNBOUNDED number of open
	// connections, so on a database with thousands of relations that fan-out
	// opens connections as fast as the introspection loop asks for them —
	// enough, against a pooler behind a load balancer, to exhaust the resolver
	// or the pooler's client slots and fail mid-diff. pg-schema-diff's own
	// documentation asks for a bounded pool; give it one.
	db.SetMaxOpenConns(targetPoolMaxConns)
	db.SetMaxIdleConns(targetPoolMaxConns)
	db.SetConnMaxLifetime(targetPoolConnMaxLifetime)
	return db, nil
}

// Bounds for the introspection pool opened by openTargetPool. Small enough to
// stay polite to a shared pooler, large enough that per-relation introspection
// still overlaps.
const (
	targetPoolMaxConns        = 8
	targetPoolConnMaxLifetime = 5 * time.Minute
)

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
