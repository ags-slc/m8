package engine

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
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

// Engine orchestrates migration discovery, planning, and execution.
type Engine struct {
	conn    *pgx.Conn
	sqlDB   *sql.DB // database/sql connection for pg-schema-diff
	store   *state.Store
	differ  *schema.Differ
	config  *Config
	logger  *slog.Logger
}

// Config holds engine configuration.
type Config struct {
	MigrationsDir string
	TargetSchema  string // schema to diff S__ files against (default: "public")
	ConnStr       string // connection string for schema differ temp DBs
}

// ApplyResult holds the outcome of an apply or plan operation.
type ApplyResult struct {
	Versioned  []MigrationResult
	Schema     []SchemaResult
	Repeatable []MigrationResult
}

// MigrationResult holds the outcome of a single V__ or R__ migration.
type MigrationResult struct {
	Migration   *migration.Migration
	ExecutionMs int64
	Applied     bool // false for plan (dry-run)
	Skipped     bool // true if already applied / checksum unchanged
	Error       error
}

// SchemaResult holds the outcome of a single S__ migration.
type SchemaResult struct {
	Migration  *migration.Migration
	Diff       *schema.DiffResult
	ExecMs     int64
	Applied    bool
	Skipped    bool // true if no diff (already in desired state)
	Error      error
}

// StatusResult holds the output of a status query.
type StatusResult struct {
	Applied []state.HistoryRow
	Pending []*migration.Migration
	Changed []*migration.Migration // repeatable/schema with changed checksums
	Drift   []DriftEntry           // versioned with mismatched checksums
}

// DriftEntry represents a versioned migration whose file content changed after being applied.
type DriftEntry struct {
	Migration       *migration.Migration
	AppliedChecksum string
}

// New creates a new Engine instance.
func New(conn *pgx.Conn, sqlDB *sql.DB, differ *schema.Differ, config *Config, logger *slog.Logger) *Engine {
	if config.TargetSchema == "" {
		config.TargetSchema = "public"
	}
	return &Engine{
		conn:   conn,
		sqlDB:  sqlDB,
		store:  state.NewStore(conn),
		differ: differ,
		config: config,
		logger: logger,
	}
}

// Apply discovers and executes pending migrations in order: V__ → S__ → R__.
func (e *Engine) Apply(ctx context.Context) (*ApplyResult, error) {
	if err := e.acquireLock(ctx); err != nil {
		return nil, err
	}
	defer e.releaseLock(ctx)

	if err := e.store.EnsureSchema(ctx); err != nil {
		return nil, err
	}

	allMigrations, err := migration.Discover(e.config.MigrationsDir)
	if err != nil {
		return nil, err
	}

	result := &ApplyResult{}

	// Phase A: Versioned
	vResult, err := e.applyVersioned(ctx, filterByType(allMigrations, migration.TypeVersioned))
	if err != nil {
		result.Versioned = vResult
		return result, err
	}
	result.Versioned = vResult

	// Phase B: Schema
	sResult, err := e.applySchema(ctx, filterByType(allMigrations, migration.TypeSchema))
	if err != nil {
		result.Schema = sResult
		return result, err
	}
	result.Schema = sResult

	// Phase C: Repeatable
	rResult, err := e.applyRepeatable(ctx, filterByType(allMigrations, migration.TypeRepeatable))
	if err != nil {
		result.Repeatable = rResult
		return result, err
	}
	result.Repeatable = rResult

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

	allMigrations, err := migration.Discover(e.config.MigrationsDir)
	if err != nil {
		return nil, err
	}

	result := &ApplyResult{}

	// Phase A: Versioned — find unapplied
	applied, err := e.store.GetAppliedVersioned(ctx)
	if err != nil {
		return nil, err
	}
	appliedSet := make(map[string]bool)
	for _, h := range applied {
		if h.Version != nil {
			appliedSet[*h.Version] = true
		}
	}
	for _, m := range filterByType(allMigrations, migration.TypeVersioned) {
		if appliedSet[m.Version] {
			result.Versioned = append(result.Versioned, MigrationResult{Migration: m, Skipped: true})
		} else {
			result.Versioned = append(result.Versioned, MigrationResult{Migration: m, Applied: false})
		}
	}

	// Phase B: Schema — diff each against live DB
	for _, m := range filterByType(allMigrations, migration.TypeSchema) {
		sr := SchemaResult{Migration: m}
		if e.differ != nil {
			ddl := []string{string(m.Content)}
			diffResult, err := e.differ.Diff(ctx, e.sqlDB, e.config.TargetSchema, ddl)
			if err != nil {
				sr.Error = err
			} else {
				diffResult.Name = m.Name
				sr.Diff = diffResult
				sr.Skipped = !diffResult.HasChanges
			}
		}
		result.Schema = append(result.Schema, sr)
	}

	// Phase C: Repeatable — find changed checksums
	latestRepeatable, err := e.store.GetLatestRepeatable(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range filterByType(allMigrations, migration.TypeRepeatable) {
		if prev, ok := latestRepeatable[m.Name]; ok && prev.Checksum == m.Checksum {
			result.Repeatable = append(result.Repeatable, MigrationResult{Migration: m, Skipped: true})
		} else {
			result.Repeatable = append(result.Repeatable, MigrationResult{Migration: m, Applied: false})
		}
	}

	return result, nil
}

// Status shows applied vs pending migrations.
func (e *Engine) Status(ctx context.Context) (*StatusResult, error) {
	if err := e.store.EnsureSchema(ctx); err != nil {
		return nil, err
	}

	allMigrations, err := migration.Discover(e.config.MigrationsDir)
	if err != nil {
		return nil, err
	}

	history, err := e.store.GetAllHistory(ctx)
	if err != nil {
		return nil, err
	}

	appliedVersioned, err := e.store.GetAppliedVersioned(ctx)
	if err != nil {
		return nil, err
	}
	appliedVersionedMap := make(map[string]state.HistoryRow)
	for _, h := range appliedVersioned {
		if h.Version != nil {
			appliedVersionedMap[*h.Version] = h
		}
	}

	latestRepeatable, err := e.store.GetLatestRepeatable(ctx)
	if err != nil {
		return nil, err
	}

	latestSchema, err := e.store.GetLatestSchema(ctx)
	if err != nil {
		return nil, err
	}

	result := &StatusResult{Applied: history}

	for _, m := range allMigrations {
		switch m.Type {
		case migration.TypeVersioned:
			if h, ok := appliedVersionedMap[m.Version]; ok {
				if h.Checksum != m.Checksum {
					result.Drift = append(result.Drift, DriftEntry{
						Migration:       m,
						AppliedChecksum: h.Checksum,
					})
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
		case migration.TypeRepeatable:
			if _, ok := latestRepeatable[m.Name]; !ok {
				result.Pending = append(result.Pending, m)
			} else if latestRepeatable[m.Name].Checksum != m.Checksum {
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
		if !all && m.Type == migration.TypeVersioned && m.Version > version {
			continue
		}
		var ver *string
		if m.Type == migration.TypeVersioned {
			ver = &m.Version
		}
		if err := e.store.RecordBaseline(ctx, ver, m.Name, m.Type.String(), m.Checksum); err != nil {
			return fmt.Errorf("failed to baseline %s: %w", m.Filename, err)
		}
		e.logger.Info("baselined", "file", m.Filename, "type", m.Type.String())
	}

	return nil
}

// --- apply helpers ---

func (e *Engine) applyVersioned(ctx context.Context, migrations []*migration.Migration) ([]MigrationResult, error) {
	applied, err := e.store.GetAppliedVersioned(ctx)
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
			_ = e.store.RecordApplied(ctx, &m.Version, m.Name, m.Type.String(), m.Checksum, mr.ExecutionMs, false)
			results = append(results, mr)
			return results, fmt.Errorf("migration %s failed: %w", m.Filename, err)
		}
		mr.ExecutionMs = time.Since(start).Milliseconds()
		mr.Applied = true
		if err := e.store.RecordApplied(ctx, &m.Version, m.Name, m.Type.String(), m.Checksum, mr.ExecutionMs, true); err != nil {
			results = append(results, mr)
			return results, fmt.Errorf("failed to record %s: %w", m.Filename, err)
		}
		e.logger.Info("applied", "file", m.Filename, "type", "versioned", "ms", mr.ExecutionMs)
		results = append(results, mr)
	}
	return results, nil
}

func (e *Engine) applySchema(ctx context.Context, migrations []*migration.Migration) ([]SchemaResult, error) {
	if e.differ == nil {
		return nil, nil
	}

	var results []SchemaResult
	for _, m := range migrations {
		sr := SchemaResult{Migration: m}
		ddl := []string{string(m.Content)}
		diffResult, err := e.differ.Diff(ctx, e.sqlDB, e.config.TargetSchema, ddl)
		if err != nil {
			sr.Error = err
			results = append(results, sr)
			return results, fmt.Errorf("schema diff failed for %s: %w", m.Filename, err)
		}
		diffResult.Name = m.Name
		sr.Diff = diffResult

		if !diffResult.HasChanges {
			sr.Skipped = true
			results = append(results, sr)
			continue
		}

		start := time.Now()
		for i, stmt := range diffResult.Statements {
			if stmt.LockTimeout > 0 {
				_, _ = e.conn.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", stmt.LockTimeout.Milliseconds()))
			}
			if stmt.Timeout > 0 {
				_, _ = e.conn.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", stmt.Timeout.Milliseconds()))
			}
			if _, err := e.conn.Exec(ctx, stmt.DDL); err != nil {
				sr.ExecMs = time.Since(start).Milliseconds()
				sr.Error = fmt.Errorf("statement %d failed: %w\nDDL: %s", i+1, err, stmt.DDL)
				_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), m.Checksum, sr.ExecMs, false)
				results = append(results, sr)
				return results, fmt.Errorf("schema migration %s failed: %w", m.Filename, sr.Error)
			}
		}
		sr.ExecMs = time.Since(start).Milliseconds()
		sr.Applied = true
		if err := e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), m.Checksum, sr.ExecMs, true); err != nil {
			results = append(results, sr)
			return results, fmt.Errorf("failed to record %s: %w", m.Filename, err)
		}
		e.logger.Info("applied", "file", m.Filename, "type", "schema", "ms", sr.ExecMs, "statements", len(diffResult.Statements))
		results = append(results, sr)
	}
	return results, nil
}

func (e *Engine) applyRepeatable(ctx context.Context, migrations []*migration.Migration) ([]MigrationResult, error) {
	latest, err := e.store.GetLatestRepeatable(ctx)
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
			_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), m.Checksum, mr.ExecutionMs, false)
			results = append(results, mr)
			return results, fmt.Errorf("migration %s failed: %w", m.Filename, err)
		}
		mr.ExecutionMs = time.Since(start).Milliseconds()
		mr.Applied = true
		if err := e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), m.Checksum, mr.ExecutionMs, true); err != nil {
			results = append(results, mr)
			return results, fmt.Errorf("failed to record %s: %w", m.Filename, err)
		}
		e.logger.Info("applied", "file", m.Filename, "type", "repeatable", "ms", mr.ExecutionMs)
		results = append(results, mr)
	}
	return results, nil
}

// executeMigration parses and executes a V__ or R__ migration file.
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
	defer tx.Rollback(ctx)

	if parsed.Directives.LockTimeout > 0 {
		_, err = tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", parsed.Directives.LockTimeout.Milliseconds()))
		if err != nil {
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

	// Versioned
	for _, v := range result.Versioned {
		if !v.Skipped {
			fmt.Fprintf(&b, "  + %s (versioned)\n", v.Migration.Filename)
			pending++
		}
	}

	// Schema
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

	// Repeatable
	for _, r := range result.Repeatable {
		if !r.Skipped {
			fmt.Fprintf(&b, "  ~ %s (repeatable)\n", r.Migration.Filename)
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

	for _, v := range result.Versioned {
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

	for _, r := range result.Repeatable {
		if r.Error != nil {
			fmt.Fprintf(&b, "  ✗ %s (%dms) ERROR: %v\n", r.Migration.Filename, r.ExecutionMs, r.Error)
			failed++
		} else if r.Applied {
			fmt.Fprintf(&b, "  ✓ %s (%dms)\n", r.Migration.Filename, r.ExecutionMs)
			applied++
		} else if r.Skipped {
			skipped++
		}
	}

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
			fmt.Fprintf(&b, "  %s %-12s %-10s %s  %s\n", status, h.Type, ver, h.AppliedAt.Format("2006-01-02 15:04"), h.Name)
		}
		b.WriteString("\n")
	}

	if len(result.Pending) > 0 {
		fmt.Fprintf(&b, "Pending migrations (%d):\n", len(result.Pending))
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
