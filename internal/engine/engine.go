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
	"github.com/ags-slc/m8/internal/pgident"
	"github.com/ags-slc/m8/internal/schema"
	"github.com/ags-slc/m8/internal/state"
	"github.com/jackc/pgx/v5"
)

// Advisory lock ID: first 8 bytes of SHA-256("m8_migration_lock")
const lockID int64 = 5739048866534836184

// resetTimeout bounds the RESET that clears a schema statement's timeouts. It
// runs in a defer on a context detached from the caller's, so without a deadline
// of its own an unresponsive server would hang the command after the statement
// has already finished.
const resetTimeout = 10 * time.Second

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
	// FailOnUnvalidated refuses a schema diff whose plan could not be validated
	// against a throwaway rebuild, instead of applying it with a warning. A CI
	// gate can be built on the resulting exit code; a warning printed to stdout
	// is not something a pipeline can fail on.
	FailOnUnvalidated bool
	// LockTimeout and StatementTimeout override the hazard-derived timeouts
	// pg-schema-diff attaches to each generated statement. Zero keeps the
	// plan's own value, which is what you want by default: 3s for ordinary DDL,
	// 20 minutes for a concurrent index build. The override exists because a
	// legitimate ALTER TABLE on a large table can exceed 3s -- and until these
	// timeouts were actually applied, it silently did.
	LockTimeout      time.Duration
	StatementTimeout time.Duration
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

	// Deliberately NOT EnsureSchema: plan is read-only, so on a database m8 has
	// never touched it reports everything as pending rather than bootstrapping
	// its own state as a side effect of being asked what it would do.
	stateReady, err := e.store.SchemaExists(ctx)
	if err != nil {
		return nil, err
	}

	all, err := migration.Discover(e.config.MigrationsDir)
	if err != nil {
		return nil, err
	}

	result := &ApplyResult{}

	// Phase A: Ops — find unapplied
	var applied []state.HistoryRow
	if stateReady {
		applied, err = e.store.GetAppliedOps(ctx)
		if err != nil {
			return nil, err
		}
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
				combinedDDL = append(combinedDDL, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s;", pgident.Quote(pgSchema)))
			}
			for _, m := range migrations {
				combinedDDL = append(combinedDDL, string(m.Content))
			}
			diffResult, err := e.differ.Diff(ctx, e.sqlDB, pgSchema, combinedDDL, e.config.Strict)
			if err != nil {
				// One diff covers the whole schema folder, so a failure is a
				// property of the folder, not of each file in it. Reporting it
				// against every file names dozens of innocent ones and buries
				// the real problem — attribute it once, like a successful diff.
				for i, m := range migrations {
					sr := SchemaResult{Migration: m}
					if i == 0 {
						sr.Error = err
					} else {
						sr.Skipped = true
					}
					result.Schema = append(result.Schema, sr)
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
	result.Logic, err = e.planIdempotent(ctx, filterByType(all, migration.TypeLogic), "logic", stateReady)
	if err != nil {
		return result, err
	}

	// Phase D: Permissions — find changed checksums
	result.Permissions, err = e.planIdempotent(ctx, filterByType(all, migration.TypePermissions), "permissions", stateReady)
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

// plannedSchema is one schema folder's diff, computed but not yet applied.
type plannedSchema struct {
	pgSchema   string
	migrations []*migration.Migration
	diff       *schema.DiffResult
}

func (e *Engine) applySchema(ctx context.Context, migrations []*migration.Migration) ([]SchemaResult, error) {
	if e.differ == nil {
		if len(migrations) > 0 {
			return nil, errDifferUnavailable(len(migrations))
		}
		return nil, nil
	}

	// Group by PG schema and diff combined DDL (FK references across files need
	// this). Sorted, because Go map iteration is randomised and two runs of the
	// same apply should not touch the schemas in different orders.
	grouped := groupByPGSchema(migrations)
	pgSchemas := make([]string, 0, len(grouped))
	for pgSchema := range grouped {
		pgSchemas = append(pgSchemas, pgSchema)
	}
	sort.Strings(pgSchemas)

	var results []SchemaResult

	// Pass 1: diff EVERY folder before applying any of them.
	//
	// A refusal has to be a refusal for the whole run. Deciding per folder,
	// inside the apply loop, means folder B can be refused after folder A's
	// statements have already landed -- which is not a refusal, it is a partial
	// apply with an error on the end. The same goes for a folder whose diff
	// cannot be computed at all.
	var planned []plannedSchema
	for _, pgSchema := range pgSchemas {
		pgMigrations := grouped[pgSchema]

		var combinedDDL []string
		if pgSchema != "public" {
			combinedDDL = append(combinedDDL, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s;", pgident.Quote(pgSchema)))
		}
		for _, m := range pgMigrations {
			combinedDDL = append(combinedDDL, string(m.Content))
		}

		diffResult, err := e.differ.Diff(ctx, e.sqlDB, pgSchema, combinedDDL, e.config.Strict)
		if err != nil {
			// Attribute once, for the same reason as in Plan: the diff covers
			// the whole schema folder, so repeating the error per file names
			// dozens of innocent ones.
			results = append(results, attributeSchemaError(pgMigrations, err, nil)...)
			return results, fmt.Errorf("schema diff failed for %s: %w", pgSchema, err)
		}
		diffResult.Name = pgSchema

		if diffResult.ValidationSkipped && e.config.FailOnUnvalidated {
			refusal := fmt.Errorf(
				"plan for schema %s was not validated (%s); refusing to apply it "+
					"(--fail-on-unvalidated / require_shadow is set -- configure a shadow instance "+
					"with --shadow-url / SHADOW_DATABASE_URL so the plan can be checked)",
				pgSchema, firstLine(diffResult.ValidationSkippedReason))
			results = append(results, attributeSchemaError(pgMigrations, refusal, diffResult)...)
			return results, refusal
		}

		planned = append(planned, plannedSchema{pgSchema: pgSchema, migrations: pgMigrations, diff: diffResult})
	}

	// Pass 2: apply.
	for _, p := range planned {
		if !p.diff.HasChanges {
			// Attach the diff even though there is nothing to do: "clean" and
			// "clean, and we could not verify it" are different answers, and the
			// second one has to survive as far as the output formatter.
			for i, m := range p.migrations {
				sr := SchemaResult{Migration: m, Skipped: true}
				if i == 0 {
					sr.Diff = p.diff
				}
				results = append(results, sr)
			}
			continue
		}

		// Apply the combined diff and record against each migration file
		start := time.Now()
		for i, stmt := range p.diff.Statements {
			if err := e.execSchemaStatement(ctx, stmt); err != nil {
				execMs := time.Since(start).Milliseconds()
				errMsg := fmt.Errorf("statement %d failed: %w\nDDL: %s", i+1, err, stmt.DDL)
				for _, m := range p.migrations {
					ps := p.pgSchema
					_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), &ps, m.Checksum, execMs, false)
					results = append(results, SchemaResult{Migration: m, ExecMs: execMs, Error: errMsg})
				}
				return results, fmt.Errorf("schema migration for %s failed: %w", p.pgSchema, errMsg)
			}
		}
		execMs := time.Since(start).Milliseconds()

		// Record success for all migrations in this PG schema group
		for i, m := range p.migrations {
			ps := p.pgSchema
			_ = e.store.RecordApplied(ctx, nil, m.Name, m.Type.String(), &ps, m.Checksum, execMs, true)
			sr := SchemaResult{Migration: m, ExecMs: execMs, Applied: true}
			if i == 0 {
				sr.Diff = p.diff
			}
			results = append(results, sr)
		}
		e.logger.Info("applied", "pg_schema", p.pgSchema, "type", "schema", "ms", execMs, "statements", len(p.diff.Statements))
	}
	return results, nil
}

// attributeSchemaError records a whole-folder failure against the first file in
// the folder and marks the rest skipped. One diff covers the whole folder, so
// repeating the error per file names dozens of innocent ones and buries the real
// problem.
func attributeSchemaError(migrations []*migration.Migration, err error, diff *schema.DiffResult) []SchemaResult {
	out := make([]SchemaResult, 0, len(migrations))
	for i, m := range migrations {
		sr := SchemaResult{Migration: m}
		if i == 0 {
			sr.Error = err
			sr.Diff = diff
		} else {
			sr.Skipped = true
		}
		out = append(out, sr)
	}
	return out
}

// execSchemaStatement runs one generated DDL statement with pg-schema-diff's
// hazard-derived timeouts actually in force.
//
// The timeouts are applied with a SESSION-level SET. They used to be issued as
// SET LOCAL on e.conn, which is not inside a transaction -- and SET LOCAL
// outside a transaction block does nothing at all: Postgres raises a WARNING
// and moves on. The errors were discarded (`_, _ =`) so even the warning went
// nowhere. Every lock timeout pg-schema-diff derived from a statement's hazards
// was therefore silently dropped, and schema DDL waited for its ACCESS
// EXCLUSIVE lock without bound against whatever it was pointed at.
//
// Wrapping the statement in an explicit transaction is not the fix: the plan
// contains CREATE INDEX CONCURRENTLY, which Postgres refuses to run inside one.
// pg-schema-diff says as much in Statement's own documentation ("be sure to set
// the session-level ... timeout"), and its CLI emits SET SESSION.
//
// A plain SET is safe here because e.conn is one dedicated connection: the same
// backend runs the SET and the statement. Both settings are reset afterwards,
// on a context detached from the caller's, so a cancelled or failed statement
// cannot leave its timeout on the connection the rest of the run keeps using.
func (e *Engine) execSchemaStatement(ctx context.Context, stmt schema.DiffStatement) error {
	lockTimeout := stmt.LockTimeout
	if e.config.LockTimeout > 0 {
		lockTimeout = e.config.LockTimeout
	}
	statementTimeout := stmt.Timeout
	if e.config.StatementTimeout > 0 {
		statementTimeout = e.config.StatementTimeout
	}

	reset := func(setting string) {
		// Detached from the caller's context so a cancelled or failed statement
		// still cleans up -- but with a deadline of its own, because
		// context.WithoutCancel drops the caller's deadline along with its
		// cancellation, and a RESET against an unresponsive server would then
		// block forever in a defer.
		resetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resetTimeout)
		defer cancel()
		if _, err := e.conn.Exec(resetCtx, "RESET "+setting); err != nil {
			e.logger.Warn("failed to reset session setting after a schema statement",
				"setting", setting, "error", err)
		}
	}

	if lockTimeout > 0 {
		if _, err := e.conn.Exec(ctx, fmt.Sprintf("SET lock_timeout = '%dms'", lockTimeout.Milliseconds())); err != nil {
			return fmt.Errorf("setting lock_timeout: %w", err)
		}
		defer reset("lock_timeout")
	}
	if statementTimeout > 0 {
		if _, err := e.conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = '%dms'", statementTimeout.Milliseconds())); err != nil {
			return fmt.Errorf("setting statement_timeout: %w", err)
		}
		defer reset("statement_timeout")
	}

	_, err := e.conn.Exec(ctx, stmt.DDL)
	return err
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

// planIdempotent classifies logic/permissions migrations as pending or already
// applied. stateReady is false on a database m8 has never touched, where there
// is no history to read and everything is therefore pending — plan must not
// create the state schema just to discover that.
func (e *Engine) planIdempotent(ctx context.Context, migrations []*migration.Migration, typ string, stateReady bool) ([]MigrationResult, error) {
	latest := map[string]state.HistoryRow{}
	if stateReady {
		var err error
		latest, err = e.store.GetLatestByType(ctx, typ)
		if err != nil {
			return nil, err
		}
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
		// Quoted: an unquoted schema/MySchema/ folder creates "myschema" while
		// the diff is scoped to "MySchema", which then reads as empty.
		_, err := e.conn.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", pgident.Quote(m.PGSchema)))
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
			if s.Diff.ValidationSkipped {
				fmt.Fprintf(&b, "    ⚠ PLAN_NOT_VALIDATED — the current schema could not be rebuilt in isolation\n")
				fmt.Fprintf(&b, "      (%s)\n", firstLine(s.Diff.ValidationSkippedReason))
			}
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

	// A diff that produced no statements is marked Skipped, so the loop above
	// never reaches it -- but "clean" and "clean, and we could not verify it"
	// are different answers. Surface the warning either way, without counting
	// it as pending: an unvalidated empty diff is still an empty diff.
	var w strings.Builder
	for _, s := range result.Schema {
		if s.Skipped && s.Diff != nil && s.Diff.ValidationSkipped {
			fmt.Fprintf(&w, "  ⚠ PLAN_NOT_VALIDATED %s (schema) — the current schema could not be rebuilt in isolation\n",
				s.Diff.Name)
			fmt.Fprintf(&w, "      (%s)\n", firstLine(s.Diff.ValidationSkippedReason))
		}
	}

	if pending == 0 {
		return w.String() + "No pending migrations. Database is up to date.\n"
	}

	header := fmt.Sprintf("Plan: %d migration(s) to apply.\n\n", pending)
	return header + w.String() + b.String()
}

// FormatApplyOutput returns a human-readable summary of an apply result.
//
// An unvalidated plan is reported here, not only in `plan`. apply runs the same
// Differ, so it can and does execute a degraded plan against the target; saying
// so only in the read-only command means the one operator who most needs to know
// -- the one holding the write connection -- is the one not told.
func FormatApplyOutput(result *ApplyResult) string {
	var b strings.Builder
	var applied, skipped, failed int

	// Warnings for results that get no line of their own, hoisted above the
	// per-file lines so they are not lost at the end of a long apply.
	var w strings.Builder
	warnUnvalidated := func(s SchemaResult, applied bool) {
		if s.Diff == nil || !s.Diff.ValidationSkipped {
			return
		}
		verb := "were applied"
		if !applied {
			verb = "would have been applied"
		}
		fmt.Fprintf(&w, "  ⚠ PLAN_NOT_VALIDATED %s (schema) — the statements %s without the check that they execute\n",
			s.Diff.Name, verb)
		fmt.Fprintf(&w, "      (%s)\n", firstLine(s.Diff.ValidationSkippedReason))
	}

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
			warnUnvalidated(s, true)
			applied++
		} else if s.Skipped {
			warnUnvalidated(s, false)
			skipped++
		}
	}
	writeResults(result.Logic)
	writeResults(result.Permissions)

	summary := fmt.Sprintf("\nApplied: %d, Skipped: %d, Failed: %d\n", applied, skipped, failed)
	return w.String() + b.String() + summary
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

// FailsOnUnvalidatedPlan reports whether this engine refuses a schema diff whose
// plan could not be validated. `plan` gates its exit code on the same setting
// `apply` gates its refusal on, read from the engine rather than resolved a
// second time, so the two commands cannot disagree within one run.
func (e *Engine) FailsOnUnvalidatedPlan() bool {
	return e.config.FailOnUnvalidated
}

// UnvalidatedSchemas returns the PostgreSQL schemas whose diff was produced
// without the plan-validation step. `plan` turns this into a non-zero exit when
// --fail-on-unvalidated is set: PLAN_NOT_VALIDATED printed to stdout is not
// something a CI pipeline can gate on.
func UnvalidatedSchemas(r *ApplyResult) []string {
	var names []string
	seen := make(map[string]bool)
	for _, s := range r.Schema {
		if s.Diff == nil || !s.Diff.ValidationSkipped || seen[s.Diff.Name] {
			continue
		}
		seen[s.Diff.Name] = true
		names = append(names, s.Diff.Name)
	}
	return names
}

// firstLine trims a multi-line error to its first line, so a plan stays
// readable when the underlying message embeds a whole view definition.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " ..."
	}
	return strings.TrimSpace(s)
}
