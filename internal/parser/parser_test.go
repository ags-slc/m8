package parser

import (
	"testing"
	"time"
)

func TestParseSimpleStatements(t *testing.T) {
	input := []byte("SELECT 1;\nSELECT 2;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(result.Statements))
	}
	if result.Statements[0].SQL != "SELECT 1" {
		t.Errorf("stmt 0: expected 'SELECT 1', got %q", result.Statements[0].SQL)
	}
	if result.Statements[1].SQL != "SELECT 2" {
		t.Errorf("stmt 1: expected 'SELECT 2', got %q", result.Statements[1].SQL)
	}
}

func TestParseTrailingNewline(t *testing.T) {
	input := []byte("SELECT 1;\n")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
}

func TestParseNoTrailingSemicolon(t *testing.T) {
	input := []byte("SELECT 1")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
	if result.Statements[0].SQL != "SELECT 1" {
		t.Errorf("expected 'SELECT 1', got %q", result.Statements[0].SQL)
	}
}

func TestParseEmptyInput(t *testing.T) {
	result, err := Parse([]byte(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 0 {
		t.Errorf("expected 0 statements, got %d", len(result.Statements))
	}
}

func TestParseWhitespaceOnly(t *testing.T) {
	result, err := Parse([]byte("   \n\t  \n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 0 {
		t.Errorf("expected 0 statements, got %d", len(result.Statements))
	}
}

func TestParseCommentsOnly(t *testing.T) {
	input := []byte("-- just a comment\n-- another comment\n")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 0 {
		t.Errorf("expected 0 statements, got %d", len(result.Statements))
	}
}

func TestParseSingleLineComment(t *testing.T) {
	input := []byte("SELECT 1; -- this is a comment\nSELECT 2;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(result.Statements))
	}
}

func TestParseBlockComment(t *testing.T) {
	input := []byte("SELECT /* a comment */ 1;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
}

func TestParseNestedBlockComment(t *testing.T) {
	input := []byte("SELECT /* outer /* inner */ still comment */ 1;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
}

func TestParseUnterminatedBlockComment(t *testing.T) {
	input := []byte("SELECT /* unterminated comment")
	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for unterminated block comment")
	}
}

func TestParseStringLiteral(t *testing.T) {
	input := []byte("SELECT 'hello;world';")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement (semicolon inside string), got %d", len(result.Statements))
	}
	if result.Statements[0].SQL != "SELECT 'hello;world'" {
		t.Errorf("expected SELECT 'hello;world', got %q", result.Statements[0].SQL)
	}
}

func TestParseEscapedQuote(t *testing.T) {
	input := []byte("SELECT 'it''s';")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
	if result.Statements[0].SQL != "SELECT 'it''s'" {
		t.Errorf("got %q", result.Statements[0].SQL)
	}
}

func TestParseUnterminatedString(t *testing.T) {
	input := []byte("SELECT 'unterminated")
	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for unterminated string literal")
	}
}

func TestParseIdentifierQuote(t *testing.T) {
	input := []byte(`SELECT "col;name" FROM t;`)
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement (semicolon inside identifier), got %d", len(result.Statements))
	}
}

func TestParseEscapedIdentifierQuote(t *testing.T) {
	input := []byte(`SELECT "col""name" FROM t;`)
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
}

// --- Dollar-quoting tests ---

func TestParseDollarQuoteSimple(t *testing.T) {
	input := []byte(`CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  RAISE NOTICE 'hello';
END;
$$;`)
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d: %+v", len(result.Statements), result.Statements)
	}
}

func TestParseDollarQuoteNamed(t *testing.T) {
	input := []byte(`CREATE PROCEDURE proc() LANGUAGE plpgsql AS $procedure$
BEGIN
  COMMIT;
  INSERT INTO t VALUES (1);
  COMMIT;
END;
$procedure$;`)
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement (COMMITs inside dollar-quote), got %d", len(result.Statements))
	}
}

func TestParseDollarQuoteNested(t *testing.T) {
	// cron.schedule_in_database pattern: $$ wrapping SQL with inner semicolons
	input := []byte(`SELECT cron.schedule_in_database(
    'my-job',
    '*/5 * * * *',
    $$CALL my_proc(p_batch := 100);$$,
    'viewport'
);`)
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
}

func TestParseDollarQuoteNestedDifferentTags(t *testing.T) {
	// Procedure body uses $procedure$, inner SQL uses $$
	input := []byte(`CREATE PROCEDURE p() LANGUAGE plpgsql AS $proc$
BEGIN
  EXECUTE $$SELECT * FROM t WHERE x = 'hello';$$;
END;
$proc$;`)
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
}

func TestParseDollarQuoteUnterminated(t *testing.T) {
	input := []byte(`CREATE FUNCTION f() AS $$ SELECT 1`)
	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for unterminated dollar-quote")
	}
}

func TestParseDollarSignNotATag(t *testing.T) {
	// $1 is a parameter reference, not a dollar-quote tag
	input := []byte("SELECT $1 + $2;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
}

// --- Multi-statement DDL ---

func TestParseMultiStatementDDL(t *testing.T) {
	input := []byte(`CREATE TABLE users (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE INDEX idx_users_name ON users (name);

GRANT SELECT ON users TO readonly;`)

	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 3 {
		t.Fatalf("expected 3 statements, got %d", len(result.Statements))
	}
}

// --- BEGIN/COMMIT detection ---

func TestParseExplicitTransactionControl(t *testing.T) {
	input := []byte("BEGIN;\nCREATE TABLE t (id INT);\nCOMMIT;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AutoNoTxBE {
		t.Error("expected AutoNoTxBE=true for explicit BEGIN/COMMIT")
	}
	if len(result.Statements) != 3 {
		t.Fatalf("expected 3 statements, got %d", len(result.Statements))
	}
}

func TestParseNoExplicitTxControl(t *testing.T) {
	input := []byte("CREATE TABLE t (id INT);")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.AutoNoTxBE {
		t.Error("expected AutoNoTxBE=false when no BEGIN/COMMIT")
	}
}

func TestParseBeginInsideDollarQuoteNotDetected(t *testing.T) {
	// BEGIN inside a procedure body should NOT trigger AutoNoTxBE
	input := []byte(`CREATE PROCEDURE p() LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO t VALUES (1);
END;
$$;`)
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	// The "BEGIN" inside $$ is part of the procedure body, not a standalone statement
	if result.AutoNoTxBE {
		t.Error("BEGIN inside dollar-quote should not trigger AutoNoTxBE")
	}
}

// --- CONCURRENTLY detection ---

func TestParseDetectConcurrently(t *testing.T) {
	input := []byte("CREATE INDEX CONCURRENTLY idx_foo ON bar (baz);")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AutoNoTx {
		t.Error("expected AutoNoTx=true for CREATE INDEX CONCURRENTLY")
	}
}

func TestParseDetectUniqueConcurrently(t *testing.T) {
	input := []byte("CREATE UNIQUE INDEX CONCURRENTLY idx_foo ON bar (baz);")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AutoNoTx {
		t.Error("expected AutoNoTx=true for CREATE UNIQUE INDEX CONCURRENTLY")
	}
}

func TestParseConcurrentlyInsideDollarQuoteNotDetected(t *testing.T) {
	// CONCURRENTLY inside a procedure body - the parsed statement is the whole
	// CREATE PROCEDURE, not CREATE INDEX CONCURRENTLY
	input := []byte(`CREATE PROCEDURE p() LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE 'CREATE INDEX CONCURRENTLY idx ON t (col)';
END;
$$;`)
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	// The regex matches against the full statement which starts with CREATE PROCEDURE
	if result.AutoNoTx {
		t.Error("CONCURRENTLY inside dollar-quote should not trigger AutoNoTx")
	}
}

func TestParseNoConcurrently(t *testing.T) {
	input := []byte("CREATE INDEX idx_foo ON bar (baz);")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.AutoNoTx {
		t.Error("expected AutoNoTx=false for regular CREATE INDEX")
	}
}

// --- Directive extraction ---

func TestParseDirectiveNoTransaction(t *testing.T) {
	input := []byte("-- m8:no-transaction\nCREATE INDEX CONCURRENTLY idx ON t (c);")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Directives.NoTransaction {
		t.Error("expected NoTransaction=true")
	}
}

func TestParseDirectiveLockTimeout(t *testing.T) {
	input := []byte("-- m8:lock-timeout 5s\nALTER TABLE t ADD COLUMN c TEXT;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Directives.LockTimeout != 5*time.Second {
		t.Errorf("expected LockTimeout=5s, got %v", result.Directives.LockTimeout)
	}
}

func TestParseDirectiveRequires(t *testing.T) {
	input := []byte("-- m8:requires V20260411_001 V20260411_002\nSELECT 1;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Directives.Requires) != 2 {
		t.Fatalf("expected 2 requires, got %d", len(result.Directives.Requires))
	}
	if result.Directives.Requires[0] != "V20260411_001" {
		t.Errorf("requires[0] = %q", result.Directives.Requires[0])
	}
	if result.Directives.Requires[1] != "V20260411_002" {
		t.Errorf("requires[1] = %q", result.Directives.Requires[1])
	}
}

func TestParseMultipleDirectives(t *testing.T) {
	input := []byte("-- m8:no-transaction\n-- m8:lock-timeout 30s\nSELECT 1;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Directives.NoTransaction {
		t.Error("expected NoTransaction=true")
	}
	if result.Directives.LockTimeout != 30*time.Second {
		t.Errorf("expected LockTimeout=30s, got %v", result.Directives.LockTimeout)
	}
}

func TestParseDirectiveInMiddleOfFile(t *testing.T) {
	input := []byte("SELECT 1;\n-- m8:no-transaction\nSELECT 2;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	// Directives are extracted from anywhere in the file
	if !result.Directives.NoTransaction {
		t.Error("expected NoTransaction=true even for directive in middle of file")
	}
}

// --- psql meta-commands ---

func TestParsePsqlSetCommand(t *testing.T) {
	input := []byte(`\set ON_ERROR_STOP on
SELECT 1;`)
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PsqlWarns) != 1 {
		t.Fatalf("expected 1 psql warning, got %d", len(result.PsqlWarns))
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
}

func TestParsePsqlTimingCommand(t *testing.T) {
	input := []byte(`\set ON_ERROR_STOP on
\timing on
SELECT 1;
SELECT 2;`)
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PsqlWarns) != 2 {
		t.Fatalf("expected 2 psql warnings, got %d", len(result.PsqlWarns))
	}
	if len(result.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(result.Statements))
	}
}

// --- Line number tracking ---

func TestParseLineNumbers(t *testing.T) {
	input := []byte("SELECT 1;\n\nSELECT\n  2;")
	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(result.Statements))
	}
	if result.Statements[0].StartLine != 1 {
		t.Errorf("stmt 0 start line: expected 1, got %d", result.Statements[0].StartLine)
	}
	if result.Statements[1].StartLine != 3 {
		t.Errorf("stmt 1 start line: expected 3, got %d", result.Statements[1].StartLine)
	}
	if result.Statements[1].EndLine != 4 {
		t.Errorf("stmt 1 end line: expected 4, got %d", result.Statements[1].EndLine)
	}
}

// --- Real-world patterns from data-hub ---

func TestParseRealWorldProcedure(t *testing.T) {
	// Simulates the proc_refresh_* pattern from data-hub
	input := []byte(`CREATE OR REPLACE PROCEDURE materialized.proc_refresh_example()
LANGUAGE plpgsql AS $procedure$
DECLARE
    v_batch_start TIMESTAMPTZ;
    v_count INTEGER;
BEGIN
    SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0;

    SELECT MAX(_loaded_at) INTO v_batch_start
    FROM materialized.rpt_example;

    INSERT INTO materialized.rpt_example (id, value, _loaded_at)
    SELECT id, value, now()
    FROM source_schema.source_table
    WHERE _peerdb_synced_at > v_batch_start
    ON CONFLICT (id) DO UPDATE SET
        value = EXCLUDED.value,
        _loaded_at = EXCLUDED._loaded_at;

    GET DIAGNOSTICS v_count = ROW_COUNT;
    RAISE NOTICE 'Inserted/updated % rows', v_count;

    COMMIT;

    -- Re-set after COMMIT (SET LOCAL is transaction-scoped)
    SET LOCAL timescaledb.max_tuples_decompressed_per_dml_transaction = 0;
END;
$procedure$;`)

	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement (entire procedure), got %d", len(result.Statements))
	}
	// The procedure body contains semicolons and COMMIT - none should split the statement
	if result.AutoNoTxBE {
		t.Error("COMMIT inside dollar-quote should not trigger AutoNoTxBE")
	}
}

func TestParseRealWorldCronSchedule(t *testing.T) {
	// Simulates the cron.schedule_in_database pattern
	input := []byte(`SELECT cron.schedule_in_database(
    'cleanup-peerdb-raw-tables',
    '45 */6 * * *',
    $$CALL _peerdb_internal.proc_cleanup_raw_tables(
        p_batch_size := 50000,
        p_max_iterations := 50
    );$$,
    'viewport'
);`)

	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(result.Statements))
	}
}

func TestParseRealWorldMultiStatementMigration(t *testing.T) {
	input := []byte(`BEGIN;

CREATE TABLE IF NOT EXISTS materialized.mc_daily_summary (
    source_date DATE NOT NULL,
    billing_company_id BIGINT NOT NULL,
    total_orders INTEGER NOT NULL DEFAULT 0,
    total_revenue NUMERIC(18,2) NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_dds_source_date
    ON materialized.mc_daily_summary (source_date);

CREATE OR REPLACE PROCEDURE materialized.proc_refresh_mc_daily_summary()
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO materialized.mc_daily_summary (source_date, billing_company_id)
    SELECT CURRENT_DATE, 1;
    COMMIT;
END;
$$;

COMMIT;`)

	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	// Should have: BEGIN, CREATE TABLE, CREATE INDEX, CREATE PROCEDURE, COMMIT
	if len(result.Statements) != 5 {
		t.Fatalf("expected 5 statements, got %d", len(result.Statements))
	}
	if result.AutoNoTxBE {
		t.Log("AutoNoTxBE correctly detected explicit BEGIN/COMMIT")
	}
	if !result.AutoNoTxBE {
		t.Error("expected AutoNoTxBE=true for file with BEGIN/COMMIT")
	}
}

func TestParseRealWorldIndexesConcurrently(t *testing.T) {
	input := []byte(`-- m8:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_invoice_billing_company
    ON billing_zonos.invoice (billing_company_id);

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_invoice_detail_invoice_id
    ON billing_zonos.invoice_detail (invoice_id);`)

	result, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(result.Statements))
	}
	if !result.Directives.NoTransaction {
		t.Error("expected NoTransaction directive")
	}
	if !result.AutoNoTx {
		t.Error("expected AutoNoTx from CONCURRENTLY detection")
	}
}
