package parser

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// ParseResult holds the output of parsing a migration SQL file.
type ParseResult struct {
	Directives Directive
	Statements []Statement
	AutoNoTx   bool     // true if CREATE INDEX CONCURRENTLY was detected
	AutoNoTxBE bool     // true if explicit BEGIN/COMMIT was detected
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

type parserState int

const (
	stateNormal parserState = iota
	stateSingleLineComment
	stateBlockComment
	stateStringLiteral
	stateDollarQuote
	stateIdentifierQuote
)

type stateMachine struct {
	input       []byte
	pos         int
	state       parserState
	dollarTag   string // current dollar-quote tag (e.g., "" for $$, "procedure" for $procedure$)
	dollarStack []string
	blockDepth  int
	current     strings.Builder
	startLine   int
	line        int
	lineStart   bool // true if we're at the start of a line (after newline or BOF)
	statements  []Statement
	psqlWarns   []string
}

// Parse splits a SQL file into individual statements, extracting m8 directives
// and handling dollar-quoting, string literals, and comments correctly.
func Parse(content []byte) (*ParseResult, error) {
	directives := extractDirectives(content)

	sm := &stateMachine{
		input:     content,
		pos:       0,
		state:     stateNormal,
		startLine: 1,
		line:      1,
		lineStart: true,
	}

	if err := sm.run(); err != nil {
		return nil, err
	}

	// Emit any trailing statement
	sm.emitStatement()

	result := &ParseResult{
		Directives: directives,
		Statements: sm.statements,
		PsqlWarns:  sm.psqlWarns,
	}

	// Post-pass: detect CREATE INDEX CONCURRENTLY
	result.AutoNoTx = detectConcurrently(result.Statements)

	// Post-pass: detect explicit transaction control (BEGIN/COMMIT/ROLLBACK)
	result.AutoNoTxBE = detectExplicitTxControl(result.Statements)

	return result, nil
}

func (sm *stateMachine) run() error {
	for sm.pos < len(sm.input) {
		switch sm.state {
		case stateNormal:
			if err := sm.scanNormal(); err != nil {
				return err
			}
		case stateSingleLineComment:
			sm.scanSingleLineComment()
		case stateBlockComment:
			if err := sm.scanBlockComment(); err != nil {
				return err
			}
		case stateStringLiteral:
			if err := sm.scanStringLiteral(); err != nil {
				return err
			}
		case stateDollarQuote:
			if err := sm.scanDollarQuote(); err != nil {
				return err
			}
		case stateIdentifierQuote:
			if err := sm.scanIdentifierQuote(); err != nil {
				return err
			}
		}
	}

	// Check for unterminated states
	switch sm.state {
	case stateBlockComment:
		return fmt.Errorf("parse error at line %d: unterminated block comment", sm.line)
	case stateStringLiteral:
		return fmt.Errorf("parse error at line %d: unterminated string literal", sm.line)
	case stateDollarQuote:
		return fmt.Errorf("parse error at line %d: unterminated dollar-quoted string $%s$", sm.line, sm.dollarTag)
	case stateIdentifierQuote:
		return fmt.Errorf("parse error at line %d: unterminated identifier quote", sm.line)
	}

	return nil
}

func (sm *stateMachine) peek() byte {
	if sm.pos+1 < len(sm.input) {
		return sm.input[sm.pos+1]
	}
	return 0
}

func (sm *stateMachine) advance() {
	if sm.pos < len(sm.input) {
		if sm.input[sm.pos] == '\n' {
			sm.line++
			sm.lineStart = true
		} else if sm.input[sm.pos] != '\r' {
			sm.lineStart = false
		}
		sm.pos++
	}
}

func (sm *stateMachine) emitStatement() {
	sql := strings.TrimSpace(sm.current.String())
	if sql != "" && hasNonCommentContent(sql) {
		sm.statements = append(sm.statements, Statement{
			SQL:       sql,
			StartLine: sm.startLine,
			EndLine:   sm.line,
		})
	}
	sm.current.Reset()
	sm.startLine = sm.line
}

// hasNonCommentContent returns true if the SQL string contains content
// beyond just comments and whitespace.
func hasNonCommentContent(sql string) bool {
	lines := strings.Split(sql, "\n")
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		return true
	}
	return false
}

func (sm *stateMachine) scanNormal() error {
	c := sm.input[sm.pos]

	// Check for psql meta-commands at start of line
	if sm.lineStart && c == '\\' {
		return sm.scanPsqlMetaCommand()
	}

	switch c {
	case ';':
		// Statement terminator
		sm.advance()
		sm.emitStatement()
		sm.startLine = sm.line
		return nil

	case '-':
		if sm.peek() == '-' {
			// Single-line comment
			sm.current.WriteByte(c)
			sm.advance()
			sm.current.WriteByte(sm.input[sm.pos])
			sm.advance()
			sm.state = stateSingleLineComment
			return nil
		}

	case '/':
		if sm.peek() == '*' {
			// Block comment
			sm.current.WriteByte(c)
			sm.advance()
			sm.current.WriteByte(sm.input[sm.pos])
			sm.advance()
			sm.blockDepth = 1
			sm.state = stateBlockComment
			return nil
		}

	case '\'':
		sm.current.WriteByte(c)
		sm.advance()
		sm.state = stateStringLiteral
		return nil

	case '"':
		sm.current.WriteByte(c)
		sm.advance()
		sm.state = stateIdentifierQuote
		return nil

	case '$':
		// Try to match a dollar-quote tag
		if tag, length, ok := sm.tryDollarQuoteTag(sm.pos); ok {
			// Write the full tag including delimiters
			for i := 0; i < length; i++ {
				sm.current.WriteByte(sm.input[sm.pos])
				sm.advance()
			}
			sm.dollarTag = tag
			sm.dollarStack = []string{tag}
			sm.state = stateDollarQuote
			return nil
		}
	}

	// Default: consume the character
	if sm.current.Len() == 0 && (c == ' ' || c == '\t' || c == '\r' || c == '\n') {
		// Skip leading whitespace between statements, track start line
		sm.advance()
		sm.startLine = sm.line
	} else {
		sm.current.WriteByte(c)
		sm.advance()
	}

	return nil
}

func (sm *stateMachine) scanPsqlMetaCommand() error {
	// Capture the psql command for the warning
	start := sm.pos
	for sm.pos < len(sm.input) && sm.input[sm.pos] != '\n' {
		sm.pos++
	}
	cmd := strings.TrimSpace(string(sm.input[start:sm.pos]))
	sm.psqlWarns = append(sm.psqlWarns, fmt.Sprintf("skipped psql meta-command at line %d: %s", sm.line, cmd))
	if sm.pos < len(sm.input) {
		sm.advance() // consume the newline
	}
	return nil
}

func (sm *stateMachine) scanSingleLineComment() {
	c := sm.input[sm.pos]
	sm.current.WriteByte(c)
	if c == '\n' {
		sm.advance()
		sm.state = stateNormal
	} else {
		sm.advance()
	}
}

func (sm *stateMachine) scanBlockComment() error {
	c := sm.input[sm.pos]

	if c == '/' && sm.peek() == '*' {
		// Nested block comment
		sm.current.WriteByte(c)
		sm.advance()
		sm.current.WriteByte(sm.input[sm.pos])
		sm.advance()
		sm.blockDepth++
		return nil
	}

	if c == '*' && sm.peek() == '/' {
		sm.current.WriteByte(c)
		sm.advance()
		sm.current.WriteByte(sm.input[sm.pos])
		sm.advance()
		sm.blockDepth--
		if sm.blockDepth == 0 {
			sm.state = stateNormal
		}
		return nil
	}

	sm.current.WriteByte(c)
	sm.advance()
	return nil
}

func (sm *stateMachine) scanStringLiteral() error {
	c := sm.input[sm.pos]

	if c == '\'' {
		sm.current.WriteByte(c)
		sm.advance()
		// Check for escaped quote ''
		if sm.pos < len(sm.input) && sm.input[sm.pos] == '\'' {
			sm.current.WriteByte(sm.input[sm.pos])
			sm.advance()
			return nil // Still in string literal
		}
		sm.state = stateNormal
		return nil
	}

	sm.current.WriteByte(c)
	sm.advance()
	return nil
}

func (sm *stateMachine) scanDollarQuote() error {
	c := sm.input[sm.pos]

	if c == '$' {
		// Check if this matches the closing tag for the current level
		currentTag := sm.dollarStack[len(sm.dollarStack)-1]
		if tag, length, ok := sm.tryDollarQuoteTag(sm.pos); ok {
			if tag == currentTag {
				// Closing tag - write it and pop the stack
				for i := 0; i < length; i++ {
					sm.current.WriteByte(sm.input[sm.pos])
					sm.advance()
				}
				sm.dollarStack = sm.dollarStack[:len(sm.dollarStack)-1]
				if len(sm.dollarStack) == 0 {
					sm.state = stateNormal
				} else {
					sm.dollarTag = sm.dollarStack[len(sm.dollarStack)-1]
				}
				return nil
			}
			// Different tag - nested dollar-quote, push onto stack
			for i := 0; i < length; i++ {
				sm.current.WriteByte(sm.input[sm.pos])
				sm.advance()
			}
			sm.dollarStack = append(sm.dollarStack, tag)
			sm.dollarTag = tag
			return nil
		}
	}

	sm.current.WriteByte(c)
	sm.advance()
	return nil
}

func (sm *stateMachine) scanIdentifierQuote() error {
	c := sm.input[sm.pos]

	if c == '"' {
		sm.current.WriteByte(c)
		sm.advance()
		// Check for escaped quote ""
		if sm.pos < len(sm.input) && sm.input[sm.pos] == '"' {
			sm.current.WriteByte(sm.input[sm.pos])
			sm.advance()
			return nil // Still in identifier quote
		}
		sm.state = stateNormal
		return nil
	}

	sm.current.WriteByte(c)
	sm.advance()
	return nil
}

// tryDollarQuoteTag attempts to match a dollar-quote tag starting at pos.
// Returns (tag, length, ok). Tag is the content between $ delimiters.
// Length is the total bytes consumed (including both $ signs).
// For $$, tag="" and length=2.
// For $proc$, tag="proc" and length=6.
func (sm *stateMachine) tryDollarQuoteTag(pos int) (string, int, bool) {
	if pos >= len(sm.input) || sm.input[pos] != '$' {
		return "", 0, false
	}

	// Scan for the closing $
	i := pos + 1

	// Empty tag ($$)
	if i < len(sm.input) && sm.input[i] == '$' {
		return "", 2, true
	}

	// Tag must start with a letter or underscore
	if i >= len(sm.input) {
		return "", 0, false
	}
	ch := rune(sm.input[i])
	if !unicode.IsLetter(ch) && ch != '_' {
		return "", 0, false
	}

	// Scan tag characters (letters, digits, underscore)
	tagStart := i
	for i < len(sm.input) {
		ch = rune(sm.input[i])
		if ch == '$' {
			// Found closing $
			tag := string(sm.input[tagStart:i])
			return tag, i - pos + 1, true
		}
		if !unicode.IsLetter(ch) && !unicode.IsDigit(ch) && ch != '_' {
			// Invalid tag character
			return "", 0, false
		}
		i++
	}

	return "", 0, false
}

// extractDirectives scans for -- m8: directive comments.
func extractDirectives(content []byte) Directive {
	var d Directive
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "-- m8:") {
			continue
		}
		directive := strings.TrimPrefix(trimmed, "-- m8:")
		directive = strings.TrimSpace(directive)

		switch {
		case directive == "no-transaction":
			d.NoTransaction = true
		case strings.HasPrefix(directive, "lock-timeout "):
			val := strings.TrimPrefix(directive, "lock-timeout ")
			val = strings.TrimSpace(val)
			if dur, err := time.ParseDuration(val); err == nil {
				d.LockTimeout = dur
			}
		case strings.HasPrefix(directive, "requires "):
			val := strings.TrimPrefix(directive, "requires ")
			val = strings.TrimSpace(val)
			d.Requires = append(d.Requires, strings.Fields(val)...)
		}
	}
	return d
}

var concurrentlyRegex = regexp.MustCompile(`(?i)^\s*CREATE\s+(UNIQUE\s+)?INDEX\s+CONCURRENTLY\b`)

// detectConcurrently checks if any statement contains CREATE INDEX CONCURRENTLY.
func detectConcurrently(stmts []Statement) bool {
	for _, s := range stmts {
		if concurrentlyRegex.MatchString(s.SQL) {
			return true
		}
	}
	return false
}

var beginRegex = regexp.MustCompile(`(?i)^\s*BEGIN\s*$`)
var commitRegex = regexp.MustCompile(`(?i)^\s*COMMIT\s*$`)
var rollbackRegex = regexp.MustCompile(`(?i)^\s*ROLLBACK\s*$`)

// detectExplicitTxControl checks if any statement is an explicit BEGIN, COMMIT, or ROLLBACK.
func detectExplicitTxControl(stmts []Statement) bool {
	for _, s := range stmts {
		if beginRegex.MatchString(s.SQL) || commitRegex.MatchString(s.SQL) || rollbackRegex.MatchString(s.SQL) {
			return true
		}
	}
	return false
}
