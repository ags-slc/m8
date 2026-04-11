package parser

import "time"

// ParseResult holds the output of parsing a migration SQL file.
type ParseResult struct {
	Directives Directive
	Statements []Statement
	AutoNoTx   bool     // true if CREATE INDEX CONCURRENTLY was detected
	PsqlWarns  []string // psql meta-commands that were skipped
}

// Statement represents a single SQL statement extracted from a migration file.
type Statement struct {
	SQL       string // The raw SQL text (trimmed).
	StartLine int    // 1-based line number where the statement begins.
	EndLine   int    // 1-based line number where the statement ends.
}

// Directive holds parsed m8-specific directives from SQL comments.
type Directive struct {
	NoTransaction bool
	LockTimeout   time.Duration
	Requires      []string // version strings this migration depends on
}

// Parse splits a SQL file into individual statements, extracting m8 directives
// and handling dollar-quoting, string literals, and comments correctly.
func Parse(content []byte) (*ParseResult, error) {
	// TODO: implement character-level state machine
	return &ParseResult{}, nil
}
