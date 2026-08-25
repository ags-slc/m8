package engine

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/ags-slc/m8/internal/migration"
	"github.com/ags-slc/m8/internal/parser"
	"github.com/ags-slc/m8/internal/schema"
	"github.com/ags-slc/m8/internal/state"
	"github.com/jackc/pgx/v5"
)

// Advisory lock ID: first 8 bytes of SHA-256("m8_migration_lock")
const lockID int64 = 5739048866534836184

// errDifferUnavailable is returned when schema (S__) migrations exist but the
// schema differ could not be constructed. We refuse to silently skip schema
// changes — a no-op that exits 0 is far more dangerous than a hard failure.
func errDifferUnavailable(n int) error {
	return fmt.Errorf("%d schema (S__) migration(s) present but the schema differ is unavailable; "+
		"refusing to silently skip them — verify CREATE DATABASE privilege and the shadow connection "+
		"(--shadow-url / SHADOW_DATABASE_URL)", n)
}

// Engine orchestrates migration discovery, planning, and execution.
type Engine struct {
	conn   *pgx.Conn
	sqlDB  *sql.DB
	store  *state.Store
	differ *schema.Differ
	config *Config
	logger *slog.Logger
}

// Config holds engine configuration.
type Config struct {
	MigrationsDir string
	ConnStr       string
	Strict        bool // When true, schema diffs include DROPs for undeclared objects.
}

// ApplyResult holds the outcome of an apply or plan operation.
type ApplyResult struct {
	Ops         []MigrationResult
	Schema      []SchemaResult
	Logic       []MigrationResult
	Permissions []MigrationResult
	// PendingPGSchemas lists PostgreSQL schemas implied by schema/ subfolders
	// that do not exist yet on the target. Populated by Plan (which never
	// creates them); Apply creates them instead of reporting them.
	PendingPGSchemas []string
}

// MigrationResult holds the outcome of a single ops/logic/permissions migration.
type MigrationResult struct {
	Migration   *migration.Migration
	ExecutionMs int64
	Applied     bool
	Skipped     bool
	Error       error
}

// SchemaResult holds the outcome of a single schema migration.
type SchemaResult struct {
	Migration *migration.Migration
	Diff      *schema.DiffResult
	ExecMs    int64
	Applied   bool
	Skipped   bool
	Error     error
}

// StatusResult holds the output of a status query.
type StatusResult struct {
	Applied []state.HistoryRow
	Pending []*migration.Migration
	Changed []*migration.Migration
	Drift   []DriftEntry
}

// DriftEntry represents an ops migration whose file content changed after being applied.
type DriftEntry struct {
	Migration       *migration.Migration
	AppliedChecksum string
}

// New creates a new Engine instance.
func New(conn *pgx.Conn, sqlDB *sql.DB, differ *schema.Differ, config *Config, logger *slog.Logger) *Engine {
	return &Engine{
		conn:   conn,
		sqlDB:  sqlDB,
		store:  state.NewStore(conn),
		differ: differ,
		config: config,
		logger: logger,
	}
}

// Apply discovers and executes pending migrations: ops → schema → logic → permissions.
func (e *Engine) Apply(ctx context.Context) (*ApplyResult, error) {
	if err := e.acquireLock(ctx); err != nil {
		return nil, err
	}
	defer e.releaseLock(ctx)

	if err := e.store.EnsureSchema(ctx); err != nil {
		return nil, err
	}

	all, err := migration.Discover(e.config.MigrationsDir)
	if err != nil {
		return nil, err
	}

	result := &ApplyResult{}

	// Phase A: Ops
	r, err := e.applyOps(ctx, filterByType(all, migration.TypeOps))
	result.Ops = r
	if err != nil {
		return result, err
	}

	// Phase B: Schema — ensure PG schemas exist, then diff and apply
	schemaMigrations := filterByType(all, migration.TypeSchema)
	if err := e.ensurePGSchemas(ctx, schemaMigrations); err != nil {
		return result, err
	}
	s, err := e.applySchema(ctx, schemaMigrations)
	result.Schema = s
	if err != nil {
		return result, err
	}

	// Phase C: Logic
	r, err = e.applyIdempotent(ctx, filterByType(all, migration.TypeLogic), "logic")
	result.Logic = r
	if err != nil {
		return result, err
	}

	// Phase D: Permissions
	r, err = e.applyIdempotent(ctx, filterByType(all, migration.TypePermissions), "permissions")
	result.Permissions = r
	if err != nil {
		return result, err
	}

	return result, nil
}

// Plan shows what would be applied without making changes.
func (e *Engine) Plan(ctx context.Context) (*ApplyResult, error) {
	if err := e.acquireLock(ctx); err != nil {
		return nil, err
	}
	defer e.releaseLock(ctx)

	if err := e.store.EnsureSchema(ctx); err != nil {
		return nil, err
	}

	all, err := migration.Discover(e.config.MigrationsDir)
	if err != nil {
		return nil, err
	}

	result := &ApplyResult{}

	// Phase A: Ops — find unapplied
	applied, err := e.store.GetAppliedOps(ctx)
	if err != nil {
		return nil, err
	}
	appliedSet := make(map[string]bool)
	for _, h := range applied {
		if h.Version != nil {
			appliedSet[*h.Version] = true
		}
	}
	for _, m := range filterByType(all, migration.TypeOps) {
		if appliedSet[m.Version] {
			result.Ops = append(result.Ops, MigrationResult{Migration: m, Skipped: true})
		} else {
			result.Ops = append(result.Ops, MigrationResult{Migration: m})
		}
	}

	// Phase B: Schema — report (never create) missing PG schemas, then diff.
	// Plan is a read-only command: it must not mutate the target, so unlike
	// Apply it only records which schemas Apply would create.
	schemaMigrations := filterByType(all, migration.TypeSchema)
	pendingSchemas, err := e.missingPGSchemas(ctx, schemaMigrations)
	if err != nil {
		return nil, err
	}
	result.PendingPGSchemas = pendingSchemas
	if e.differ != nil {
		grouped := groupByPGSchema(schemaMigrations)
		for pgSchema, migrations := range grouped {
			var combinedDDL []string
			// Ensure the PG schema exists in the temp DB for DDL parsing
			if pgSchema != "public" {
				combinedDDL = append(combinedDDL, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s;", pgSchema))
			}
			for _, m := range migrations {
				combinedDDL = append(combinedDDL, string(m.Content))
			}
			diffResult, err := e.differ.Diff(ctx, e.sqlDB, pgSchema, combinedDDL, e.config.Strict)
			if err != nil {
				for _, m := range migrations {
					result.Schema = append(result.Schema, SchemaResult{Migration: m, Error: err})
				}
			} else {
				diffResult.Name = pgSchema
				// Assign the combined diff to the first migration, mark rest as skipped
				for i, m := range migrations {
					sr := SchemaResult{Migration: m}
					if i == 0 {
						sr.Diff = diffResult
						sr.Skipped = !diffResult.HasChanges
					} else {
						sr.Skipped = true
					}
					result.Schema = append(result.Schema, sr)
				}
			}
		}
	} else if len(schemaMigrations) > 0 {
		return nil, errDifferUnavailable(len(schemaMigrations))
	}

	// Phase C: Logic — find changed checksums
	result.Logic, err = e.planIdempotent(ctx, filterByType(all, migration.TypeLogic), "logic")
	if err != nil {
		return result, err
	}

	// Phase D: Permissions — find changed checksums
	result.Permissions, err = e.planIdempotent(ctx, filterByType(all, migration.TypePermissions), "permissions")
	if err != nil {
		return result, err
	}

	return result, nil
}

// Status shows applied vs pending migrations.
func (e *Engine) Status(ctx context.Context) (*StatusResult, error) {
	if err := e.store.EnsureSchema(ctx); err != nil {
		return nil, err
	}

	all, err := migration.Discover(e.config.MigrationsDir)
	if err != nil {
		return nil, err
	}

	history, err := e.store.GetAllHistory(ctx)
	if err != nil {
		return nil, err
	}

	appliedOps, err := e.store.GetAppliedOps(ctx)
	if err != nil {
		return nil, err
	}
	opsMap := make(map[string]state.HistoryRow)
	for _, h := range appliedOps {
		if h.Version != nil {
			opsMap[*h.Version] = h
		}
	}

	latestSchema, err := e.store.GetLatestByType(ctx, "schema")
	if err != nil {
		return nil, err
	}
	latestLogic, err := e.store.GetLatestByType(ctx, "logic")
	if err != nil {
		return nil, err
	}
	latestPerms, err := e.store.GetLatestByType(ctx, "permissions")
	if err != nil {
		return nil, err
	}

	result := &StatusResult{Applied: history}

	for _, m := range all {
		switch m.Type {
		case migration.TypeOps:
			if h, ok := opsMap[m.Version]; ok {
				if h.Checksum != m.Checksum {
					result.Drift = append(result.Drift, DriftEntry{Migration: m, AppliedChecksum: h.Checksum})
				}
			} else {
				result.Pending = append(result.Pending, m)
			}
		case migration.TypeSchema:
			if _, ok := latestSchema[m.Name]; !ok {
				result.Pending = append(result.Pending, m)
			} else if latestSchema[m.Name].Checksum != m.Checksum {
				result.Changed = append(result.Changed, m)
			}
		case migration.TypeLogic:
			if _, ok := latestLogic[m.Name]; !ok {
				result.Pending = append(result.Pending, m)
			} else if latestLogic[m.Name].Checksum != m.Checksum {
				result.Changed = append(result.Changed, m)
			}
		case migration.TypePermissions:
			if _, ok := latestPerms[m.Name]; !ok {
				result.Pending = append(result.Pending, m)
			} else if latestPerms[m.Name].Checksum != m.Checksum {
				result.Changed = append(result.Changed, m)
			}
		}
	}

	return result, nil
}

// Baseline marks migrations as applied without executing them.
func (e *Engine) Baseline(ctx context.Context, version string, all bool) error {
	if err := e.acquireLock(ctx); err != nil {
		return err
	}
	defer e.releaseLock(ctx)

	if err := e.store.EnsureSchema(ctx); err != nil {
		return err
	}

	allMigrations, err := migration.Discover(e.config.MigrationsDir)
	if err != nil {
		return err
	}

	for _, m := range allMigrations {
		if !all && m.Type == migration.TypeOps && m.Version > version {
			continue
		}
		var ver *string
		if m.Type == migration.TypeOps {
			ver = &m.Version
		}
		var pgSchema *string
		if m.PGSchema != "" {
			pgSchema = &m.PGSchema
		}
		if err := e.store.RecordBaseline(ctx, ver, m.Name, m.Type.String(), pgSchema, m.Checksum); err != nil {
			return fmt.Errorf("failed to baseline %s: %w", m.Filename, err)
		}
		e.logger.Info("baselined", "file", m.Filename, "type", m.Type.String())
	}

	return nil
}

// Sync performs a one-time convergence: diffs all schema/ files against the live
// database and applies changes, then applies all logic/ and permissions/ files.
// Ops/ files are skipped (they require explicit ordering via apply).
// This is the brownfield adoption command — run it once to bring an existing
// database in line with your migration files.
func (e *Engine) Sync(ctx context.Context) (*ApplyResult, error) {
	if err := e.acquireLock(ctx); err != nil {
		return nil, err
	}
	defer e.releaseLock(ctx)

	if err := e.store.EnsureSchema(ctx); err != nil {
		return nil, err
	}

	all, err := migration.Discover(e.config.MigrationsDir)
	if err != nil {
		return nil, err
	}

	result := &ApplyResult{}

	// Mark all ops as baselined (they're assumed already applied in an existing DB)
	for _, m := range filterByType(all, migration.TypeOps) {
		_ = e.store.RecordBaseline(ctx, &m.Version, m.Name, m.Type.String(), nil, m.Checksum)
		result.Ops = append(result.Ops, MigrationResult{Migration: m, Skipped: true})
		e.logger.Info("baselined (sync)", "file", m.Filename, "type", "ops")
	}

	// Schema — ensure PG schemas exist, then diff and apply
	syncSchemaMigrations := filterByType(all, migration.TypeSchema)
	if err := e.ensurePGSchemas(ctx, syncSchemaMigrations); err != nil {
		return result, err
	}
	s, err := e.applySchema(ctx, syncSchemaMigrations)
	result.Schema = s
	if err != nil {
		return result, err
	}

	// Logic — apply all (force re-apply regardless of checksum)
	for _, m := range filterByType(all, migration.TypeLogic) {
		mr := MigrationResult{Migration: m}
		start := time.Now()
		if err := e.executeMigration(ctx, m); err != nil {
			mr.ExecutionMs = time.Since(start).Milliseconds()
			mr.Error = err
			_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), nil, m.Checksum, mr.ExecutionMs, false)
			result.Logic = append(result.Logic, mr)
			return result, fmt.Errorf("sync: %s failed: %w", m.Filename, err)
		}
		mr.ExecutionMs = time.Since(start).Milliseconds()
		mr.Applied = true
		_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), nil, m.Checksum, mr.ExecutionMs, true)
		e.logger.Info("applied (sync)", "file", m.Filename, "type", "logic", "ms", mr.ExecutionMs)
		result.Logic = append(result.Logic, mr)
	}

	// Permissions — apply all (force re-apply regardless of checksum)
	for _, m := range filterByType(all, migration.TypePermissions) {
		mr := MigrationResult{Migration: m}
		start := time.Now()
		if err := e.executeMigration(ctx, m); err != nil {
			mr.ExecutionMs = time.Since(start).Milliseconds()
			mr.Error = err
			_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), nil, m.Checksum, mr.ExecutionMs, false)
			result.Permissions = append(result.Permissions, mr)
			return result, fmt.Errorf("sync: %s failed: %w", m.Filename, err)
		}
		mr.ExecutionMs = time.Since(start).Milliseconds()
		mr.Applied = true
		_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), nil, m.Checksum, mr.ExecutionMs, true)
		e.logger.Info("applied (sync)", "file", m.Filename, "type", "permissions", "ms", mr.ExecutionMs)
		result.Permissions = append(result.Permissions, mr)
	}

	return result, nil
}

// --- apply helpers ---

func (e *Engine) applyOps(ctx context.Context, migrations []*migration.Migration) ([]MigrationResult, error) {
	applied, err := e.store.GetAppliedOps(ctx)
	if err != nil {
		return nil, err
	}
	appliedSet := make(map[string]bool)
	for _, h := range applied {
		if h.Version != nil {
			appliedSet[*h.Version] = true
		}
	}

	var results []MigrationResult
	for _, m := range migrations {
		if appliedSet[m.Version] {
			results = append(results, MigrationResult{Migration: m, Skipped: true})
			continue
		}
		mr := MigrationResult{Migration: m}
		start := time.Now()
		if err := e.executeMigration(ctx, m); err != nil {
			mr.ExecutionMs = time.Since(start).Milliseconds()
			mr.Error = err
			_ = e.store.RecordApplied(ctx, &m.Version, m.Name, m.Type.String(), nil, m.Checksum, mr.ExecutionMs, false)
			results = append(results, mr)
			return results, fmt.Errorf("migration %s failed: %w", m.Filename, err)
		}
		mr.ExecutionMs = time.Since(start).Milliseconds()
		mr.Applied = true
		_ = e.store.RecordApplied(ctx, &m.Version, m.Name, m.Type.String(), nil, m.Checksum, mr.ExecutionMs, true)
		e.logger.Info("applied", "file", m.Filename, "type", "ops", "ms", mr.ExecutionMs)
		results = append(results, mr)
	}
	return results, nil
}

func (e *Engine) applySchema(ctx context.Context, migrations []*migration.Migration) ([]SchemaResult, error) {
	if e.differ == nil {
		if len(migrations) > 0 {
			return nil, errDifferUnavailable(len(migrations))
		}
		return nil, nil
	}

	var results []SchemaResult

	// Group by PG schema and diff combined DDL (FK references across files need this)
	grouped := groupByPGSchema(migrations)
	for pgSchema, pgMigrations := range grouped {
		var combinedDDL []string
		if pgSchema != "public" {
			combinedDDL = append(combinedDDL, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s;", pgSchema))
		}
		for _, m := range pgMigrations {
			combinedDDL = append(combinedDDL, string(m.Content))
		}

		diffResult, err := e.differ.Diff(ctx, e.sqlDB, pgSchema, combinedDDL, e.config.Strict)
		if err != nil {
			for _, m := range pgMigrations {
				results = append(results, SchemaResult{Migration: m, Error: err})
			}
			return results, fmt.Errorf("schema diff failed for %s: %w", pgSchema, err)
		}
		diffResult.Name = pgSchema

		if !diffResult.HasChanges {
			for _, m := range pgMigrations {
				results = append(results, SchemaResult{Migration: m, Skipped: true})
			}
			continue
		}

		// Apply the combined diff and record against each migration file
		start := time.Now()
		for i, stmt := range diffResult.Statements {
			if stmt.LockTimeout > 0 {
				_, _ = e.conn.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", stmt.LockTimeout.Milliseconds()))
			}
			if stmt.Timeout > 0 {
				_, _ = e.conn.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", stmt.Timeout.Milliseconds()))
			}
			if _, err := e.conn.Exec(ctx, stmt.DDL); err != nil {
				execMs := time.Since(start).Milliseconds()
				errMsg := fmt.Errorf("statement %d failed: %w\nDDL: %s", i+1, err, stmt.DDL)
				for _, m := range pgMigrations {
					ps := pgSchema
					_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), &ps, m.Checksum, execMs, false)
					results = append(results, SchemaResult{Migration: m, ExecMs: execMs, Error: errMsg})
				}
				return results, fmt.Errorf("schema migration for %s failed: %w", pgSchema, errMsg)
			}
		}
		execMs := time.Since(start).Milliseconds()

		// Record success for all migrations in this PG schema group
		for i, m := range pgMigrations {
			ps := pgSchema
			_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), &ps, m.Checksum, execMs, true)
			sr := SchemaResult{Migration: m, ExecMs: execMs, Applied: true}
			if i == 0 {
				sr.Diff = diffResult
			}
			results = append(results, sr)
		}
		e.logger.Info("applied", "pg_schema", pgSchema, "type", "schema", "ms", execMs, "statements", len(diffResult.Statements))
	}
	return results, nil
}

func (e *Engine) applyIdempotent(ctx context.Context, migrations []*migration.Migration, typ string) ([]MigrationResult, error) {
	latest, err := e.store.GetLatestByType(ctx, typ)
	if err != nil {
		return nil, err
	}

	var results []MigrationResult
	for _, m := range migrations {
		if prev, ok := latest[m.Name]; ok && prev.Checksum == m.Checksum {
			results = append(results, MigrationResult{Migration: m, Skipped: true})
			continue
		}
		mr := MigrationResult{Migration: m}
		start := time.Now()
		if err := e.executeMigration(ctx, m); err != nil {
			mr.ExecutionMs = time.Since(start).Milliseconds()
			mr.Error = err
			_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), nil, m.Checksum, mr.ExecutionMs, false)
			results = append(results, mr)
			return results, fmt.Errorf("migration %s failed: %w", m.Filename, err)
		}
		mr.ExecutionMs = time.Since(start).Milliseconds()
		mr.Applied = true
		_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), nil, m.Checksum, mr.ExecutionMs, true)
		e.logger.Info("applied", "file", m.Filename, "type", typ, "ms", mr.ExecutionMs)
		results = append(results, mr)
	}
	return results, nil
}

func (e *Engine) planIdempotent(ctx context.Context, migrations []*migration.Migration, typ string) ([]MigrationResult, error) {
	latest, err := e.store.GetLatestByType(ctx, typ)
	if err != nil {
		return nil, err
	}
	var results []MigrationResult
	for _, m := range migrations {
		if prev, ok := latest[m.Name]; ok && prev.Checksum == m.Checksum {
			results = append(results, MigrationResult{Migration: m, Skipped: true})
		} else {
			results = append(results, MigrationResult{Migration: m})
		}
	}
	return results, nil
}

// executeMigration parses and executes an ops, logic, or permissions file.
func (e *Engine) executeMigration(ctx context.Context, m *migration.Migration) error {
	parsed, err := parser.Parse(m.Content)
	if err != nil {
		return fmt.Errorf("parse error in %s: %w", m.Filename, err)
	}

	for _, w := range parsed.PsqlWarns {
		e.logger.Warn(w)
	}

	noTx := parsed.Directives.NoTransaction || parsed.AutoNoTx || parsed.AutoNoTxBE

	if noTx {
		for i, stmt := range parsed.Statements {
			if _, err := e.conn.Exec(ctx, stmt.SQL); err != nil {
				return fmt.Errorf("statement %d (line %d) failed: %w", i+1, stmt.StartLine, err)
			}
		}
		return nil
	}

	tx, err := e.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	// Rollback is a no-op once the transaction has been committed.
	defer func() { _ = tx.Rollback(ctx) }()

	if parsed.Directives.LockTimeout > 0 {
		if _, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", parsed.Directives.LockTimeout.Milliseconds())); err != nil {
			return fmt.Errorf("failed to set lock_timeout: %w", err)
		}
	}

	for i, stmt := range parsed.Statements {
		if _, err := tx.Exec(ctx, stmt.SQL); err != nil {
			return fmt.Errorf("statement %d (line %d) failed: %w", i+1, stmt.StartLine, err)
		}
	}

	return tx.Commit(ctx)
}

// --- locking ---

func (e *Engine) acquireLock(ctx context.Context) error {
	var acquired bool
	err := e.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockID).Scan(&acquired)
	if err != nil {
		return fmt.Errorf("failed to acquire advisory lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("another m8 instance is running against this database (advisory lock %d held)", lockID)
	}
	return nil
}

func (e *Engine) releaseLock(ctx context.Context) {
	_, _ = e.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockID)
}

// --- helpers ---

// ensurePGSchemas creates any PostgreSQL schemas referenced by schema/ subfolders
// that don't already exist. This means you don't need an ops/ migration just to
// CREATE SCHEMA — the folder structure implies it.
// missingPGSchemas returns the PostgreSQL schemas implied by schema/ subfolders
// that do not exist on the target, in declaration order and without duplicates.
// It only reads the catalog — Plan uses it so that planning a greenfield schema
// does not create it as a side effect.
func (e *Engine) missingPGSchemas(ctx context.Context, migrations []*migration.Migration) ([]string, error) {
	var missing []string
	seen := make(map[string]bool)
	for _, m := range migrations {
		if m.PGSchema == "" || m.PGSchema == "public" || seen[m.PGSchema] {
			continue
		}
		seen[m.PGSchema] = true
		var exists bool
		err := e.conn.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", m.PGSchema,
		).Scan(&exists)
		if err != nil {
			return nil, fmt.Errorf("failed to check schema %s: %w", m.PGSchema, err)
		}
		if !exists {
			missing = append(missing, m.PGSchema)
		}
	}
	return missing, nil
}

func (e *Engine) ensurePGSchemas(ctx context.Context, migrations []*migration.Migration) error {
	seen := make(map[string]bool)
	for _, m := range migrations {
		if m.PGSchema == "" || m.PGSchema == "public" || seen[m.PGSchema] {
			continue
		}
		seen[m.PGSchema] = true
		_, err := e.conn.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", m.PGSchema))
		if err != nil {
			return fmt.Errorf("failed to create schema %s: %w", m.PGSchema, err)
		}
		e.logger.Info("ensured schema exists", "pg_schema", m.PGSchema)
	}
	return nil
}

func groupByPGSchema(migrations []*migration.Migration) map[string][]*migration.Migration {
	grouped := make(map[string][]*migration.Migration)
	for _, m := range migrations {
		grouped[m.PGSchema] = append(grouped[m.PGSchema], m)
	}
	// Sort within each group: tables without REFERENCES before tables with REFERENCES
	// so FK targets exist when creating referencing tables in the temp DB.
	for _, group := range grouped {
		sort.SliceStable(group, func(i, j int) bool {
			iFK := strings.Contains(strings.ToLower(string(group[i].Content)), "references")
			jFK := strings.Contains(strings.ToLower(string(group[j].Content)), "references")
			if iFK != jFK {
				return !iFK
			}
			return false
		})
	}
	return grouped
}

func filterByType(migrations []*migration.Migration, typ migration.Type) []*migration.Migration {
	var result []*migration.Migration
	for _, m := range migrations {
		if m.Type == typ {
			result = append(result, m)
		}
	}
	return result
}

// FormatPlanOutput returns a human-readable summary of a plan result.
func FormatPlanOutput(result *ApplyResult) string {
	var b strings.Builder
	var pending int

	for _, sch := range result.PendingPGSchemas {
		fmt.Fprintf(&b, "  + CREATE SCHEMA %s (schema)\n", sch)
		pending++
	}

	for _, v := range result.Ops {
		if !v.Skipped {
			fmt.Fprintf(&b, "  + %s (ops)\n", v.Migration.Filename)
			pending++
		}
	}

	for _, s := range result.Schema {
		if s.Error != nil {
			fmt.Fprintf(&b, "  ! %s (schema) ERROR: %v\n", s.Migration.Filename, s.Error)
			pending++
		} else if !s.Skipped && s.Diff != nil {
			fmt.Fprintf(&b, "  ~ %s (schema)\n", s.Migration.Filename)
			for _, stmt := range s.Diff.Statements {
				fmt.Fprintf(&b, "    %s\n", strings.TrimSpace(stmt.DDL))
				for _, h := range stmt.Hazards {
					fmt.Fprintf(&b, "    ⚠ %s\n", h)
				}
			}
			pending++
		}
	}

	for _, r := range result.Logic {
		if !r.Skipped {
			fmt.Fprintf(&b, "  ~ %s (logic)\n", r.Migration.Filename)
			pending++
		}
	}

	for _, r := range result.Permissions {
		if !r.Skipped {
			fmt.Fprintf(&b, "  ~ %s (permissions)\n", r.Migration.Filename)
			pending++
		}
	}

	if pending == 0 {
		return "No pending migrations. Database is up to date.\n"
	}

	header := fmt.Sprintf("Plan: %d migration(s) to apply.\n\n", pending)
	return header + b.String()
}

// FormatApplyOutput returns a human-readable summary of an apply result.
func FormatApplyOutput(result *ApplyResult) string {
	var b strings.Builder
	var applied, skipped, failed int

	writeResults := func(results []MigrationResult) {
		for _, v := range results {
			if v.Error != nil {
				fmt.Fprintf(&b, "  ✗ %s (%dms) ERROR: %v\n", v.Migration.Filename, v.ExecutionMs, v.Error)
				failed++
			} else if v.Applied {
				fmt.Fprintf(&b, "  ✓ %s (%dms)\n", v.Migration.Filename, v.ExecutionMs)
				applied++
			} else if v.Skipped {
				skipped++
			}
		}
	}

	writeResults(result.Ops)
	for _, s := range result.Schema {
		if s.Error != nil {
			fmt.Fprintf(&b, "  ✗ %s (%dms) ERROR: %v\n", s.Migration.Filename, s.ExecMs, s.Error)
			failed++
		} else if s.Applied {
			stmtCount := 0
			if s.Diff != nil {
				stmtCount = len(s.Diff.Statements)
			}
			fmt.Fprintf(&b, "  ✓ %s (%dms, %d statements)\n", s.Migration.Filename, s.ExecMs, stmtCount)
			applied++
		} else if s.Skipped {
			skipped++
		}
	}
	writeResults(result.Logic)
	writeResults(result.Permissions)

	summary := fmt.Sprintf("\nApplied: %d, Skipped: %d, Failed: %d\n", applied, skipped, failed)
	return b.String() + summary
}

// FormatStatusOutput returns a human-readable summary of status.
func FormatStatusOutput(result *StatusResult) string {
	var b strings.Builder

	if len(result.Applied) > 0 {
		fmt.Fprintf(&b, "Applied migrations (%d):\n", len(result.Applied))
		for _, h := range result.Applied {
			ver := "(none)"
			if h.Version != nil {
				ver = *h.Version
			}
			status := "✓"
			if !h.Success {
				status = "✗"
			}
			fmt.Fprintf(&b, "  %s %-12s %-14s %s  %s\n", status, h.Type, ver, h.AppliedAt.Format("2006-01-02 15:04"), h.Name)
		}
		b.WriteString("\n")
	}

	if len(result.Pending) > 0 {
		fmt.Fprintf(&b, "Pending (%d):\n", len(result.Pending))
		for _, m := range result.Pending {
			fmt.Fprintf(&b, "  + %s\n", m.Filename)
		}
		b.WriteString("\n")
	}

	if len(result.Changed) > 0 {
		fmt.Fprintf(&b, "Changed (will re-apply) (%d):\n", len(result.Changed))
		for _, m := range result.Changed {
			fmt.Fprintf(&b, "  ~ %s\n", m.Filename)
		}
		b.WriteString("\n")
	}

	if len(result.Drift) > 0 {
		fmt.Fprintf(&b, "Drift detected (%d):\n", len(result.Drift))
		for _, d := range result.Drift {
			fmt.Fprintf(&b, "  ⚠ %s (file checksum differs from applied)\n", d.Migration.Filename)
		}
		b.WriteString("\n")
	}

	if len(result.Pending) == 0 && len(result.Changed) == 0 && len(result.Drift) == 0 {
		b.WriteString("Database is up to date.\n")
	}

	return b.String()
}
