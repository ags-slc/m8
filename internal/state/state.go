package state

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var schemaSQL string

// HistoryRow represents a row in _m8.history.
type HistoryRow struct {
	ID          int64
	Version     *string // NULL for logic, permissions, and schema migrations.
	Name        string
	Type        string
	PGSchema    *string // Target PG schema for schema migrations.
	Checksum    string
	AppliedAt   time.Time
	ExecutionMs int64
	AppliedBy   string
	Success     bool
}

// Store manages migration state in the target database.
type Store struct {
	conn *pgx.Conn
}

// NewStore creates a new state store.
func NewStore(conn *pgx.Conn) *Store {
	return &Store{conn: conn}
}

// EnsureSchema creates the _m8 schema and history table if they don't exist.
func (s *Store) EnsureSchema(ctx context.Context) error {
	_, err := s.conn.Exec(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("failed to bootstrap _m8 schema: %w", err)
	}
	return nil
}

// GetAppliedOps returns all successfully applied ops migrations, sorted by version.
func (s *Store) GetAppliedOps(ctx context.Context) ([]HistoryRow, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT id, version, name, type, pg_schema, checksum, applied_at, execution_ms, applied_by, success
		FROM _m8.history
		WHERE type = 'ops' AND success = true
		ORDER BY version ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query ops history: %w", err)
	}
	defer rows.Close()
	return scanHistoryRows(rows)
}

// GetLatestByType returns the most recent successful apply for each
// migration of the given type, keyed by name.
func (s *Store) GetLatestByType(ctx context.Context, typ string) (map[string]HistoryRow, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT DISTINCT ON (name)
			id, version, name, type, pg_schema, checksum, applied_at, execution_ms, applied_by, success
		FROM _m8.history
		WHERE type = $1 AND success = true
		ORDER BY name, applied_at DESC
	`, typ)
	if err != nil {
		return nil, fmt.Errorf("failed to query %s history: %w", typ, err)
	}
	defer rows.Close()

	result := make(map[string]HistoryRow)
	for rows.Next() {
		var h HistoryRow
		if err := scanOneRow(rows, &h); err != nil {
			return nil, err
		}
		result[h.Name] = h
	}
	return result, rows.Err()
}

// GetAllHistory returns every row in _m8.history ordered by applied_at.
func (s *Store) GetAllHistory(ctx context.Context) ([]HistoryRow, error) {
	rows, err := s.conn.Query(ctx, `
		SELECT id, version, name, type, pg_schema, checksum, applied_at, execution_ms, applied_by, success
		FROM _m8.history
		ORDER BY applied_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to query history: %w", err)
	}
	defer rows.Close()
	return scanHistoryRows(rows)
}

// RecordApplied inserts a history row for a migration that was applied (or failed).
func (s *Store) RecordApplied(ctx context.Context, version *string, name string, typ string, pgSchema *string, checksum string, executionMs int64, success bool) error {
	_, err := s.conn.Exec(ctx, `
		INSERT INTO _m8.history (version, name, type, pg_schema, checksum, execution_ms, success)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, version, name, typ, pgSchema, checksum, executionMs, success)
	if err != nil {
		return fmt.Errorf("failed to record migration %q: %w", name, err)
	}
	return nil
}

// RecordBaseline records a migration as applied without actually executing it.
func (s *Store) RecordBaseline(ctx context.Context, version *string, name string, typ string, pgSchema *string, checksum string) error {
	_, err := s.conn.Exec(ctx, `
		INSERT INTO _m8.history (version, name, type, pg_schema, checksum, execution_ms, success)
		VALUES ($1, $2, $3, $4, $5, 0, true)
	`, version, name, typ, pgSchema, checksum)
	if err != nil {
		return fmt.Errorf("failed to baseline migration %q: %w", name, err)
	}
	return nil
}

func scanHistoryRows(rows pgx.Rows) ([]HistoryRow, error) {
	var result []HistoryRow
	for rows.Next() {
		var h HistoryRow
		if err := scanOneRow(rows, &h); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

func scanOneRow(rows pgx.Rows, h *HistoryRow) error {
	return rows.Scan(
		&h.ID, &h.Version, &h.Name, &h.Type, &h.PGSchema, &h.Checksum,
		&h.AppliedAt, &h.ExecutionMs, &h.AppliedBy, &h.Success,
	)
}
