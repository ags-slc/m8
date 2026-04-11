package engine

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// Engine orchestrates migration discovery, planning, and execution.
type Engine struct {
	conn   *pgx.Conn
	config *Config
	logger *slog.Logger
}

// Config holds engine configuration.
type Config struct {
	MigrationsDir string
	DryRun        bool
	LockTimeout   string
}

// New creates a new Engine instance.
func New(conn *pgx.Conn, config *Config, logger *slog.Logger) *Engine {
	return &Engine{
		conn:   conn,
		config: config,
		logger: logger,
	}
}

// Apply discovers and executes pending migrations in order: V__ -> S__ -> R__.
func (e *Engine) Apply(ctx context.Context) error {
	// TODO: implement
	return nil
}

// Plan shows what would be applied without making changes.
func (e *Engine) Plan(ctx context.Context) error {
	// TODO: implement
	return nil
}

// Status shows applied vs pending migrations.
func (e *Engine) Status(ctx context.Context) error {
	// TODO: implement
	return nil
}

// Baseline marks migrations as applied without executing them.
func (e *Engine) Baseline(ctx context.Context, version string, all bool) error {
	// TODO: implement
	return nil
}
