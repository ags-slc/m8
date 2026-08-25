package cmd

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestOpenTargetPoolDisablesStatementTimeout checks the property a pool-level
// "SET statement_timeout = 0" cannot provide: that *every* connection the pool
// opens has the timeout disabled, not just whichever one happened to run the SET.
func TestOpenTargetPoolDisablesStatementTimeout(t *testing.T) {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("m8test"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("testpwd"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second)),
	)
	if err != nil {
		t.Fatalf("starting postgres: %v", err)
	}
	defer func() { _ = container.Terminate(ctx) }()

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	// A database-level default every new connection inherits.
	admin, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(ctx, `ALTER DATABASE m8test SET statement_timeout = '250ms'`); err != nil {
		t.Fatalf("setting the database default: %v", err)
	}

	// Control: without the fix a fresh connection inherits the 250ms default. If
	// this ever reads "0" the assertion below proves nothing.
	var baseline string
	plain, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open plain pool: %v", err)
	}
	defer func() { _ = plain.Close() }()
	if err := plain.QueryRowContext(ctx, "SHOW statement_timeout").Scan(&baseline); err != nil {
		t.Fatalf("reading the inherited timeout: %v", err)
	}
	if baseline != "250ms" {
		t.Fatalf("inherited statement_timeout = %q, want 250ms — the control is not in effect", baseline)
	}

	pool, err := openTargetPool(connStr)
	if err != nil {
		t.Fatalf("openTargetPool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	// Hold several connections open at once so the pool is forced to open more
	// than one: a SET would only ever have reached the first.
	const want = 4
	conns := make([]*sql.Conn, 0, want)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < want; i++ {
		c, err := pool.Conn(ctx)
		if err != nil {
			t.Fatalf("acquiring connection %d: %v", i, err)
		}
		conns = append(conns, c)

		var timeout string
		if err := c.QueryRowContext(ctx, "SHOW statement_timeout").Scan(&timeout); err != nil {
			t.Fatalf("reading statement_timeout on connection %d: %v", i, err)
		}
		if timeout != "0" {
			t.Errorf("connection %d: statement_timeout = %q, want 0", i, timeout)
		}
	}
}
