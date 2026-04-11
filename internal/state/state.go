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
	Version     *string // NULL for repeatable and schema migrations.
	Name        string
	Type        string
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
