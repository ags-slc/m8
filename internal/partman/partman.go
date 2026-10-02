// Package partman makes pg_partman's configuration of a partitioned table part
// of the desired state a schema/ file declares.
//
// A partitioned table has two halves. Its shape -- columns, indexes, the
// partition key -- is ordinary DDL, and the schema diff owns it. Which children
// exist, and how many months ahead and behind of them there should be, is
// pg_partman's: create_parent registers the table in part_config, and
// run_maintenance keeps creating and retiring children from what that row says.
// A file that declared only the shape left the other half to a hand-written
// ops/ step that nothing ever compared again, which is how a table ends up with
// infinite_time_partitions unset and every new row in its default partition.
//
// The directive declares that row:
//
//	-- m8:partman audit_log control=created_at partition_interval='1 month' premake=4
//
// Keys are part_config's own column names, plus create_parent's default_table
// and start_partition. A key left out is not managed: m8 leaves whatever value
// the database holds, as it does for any object a file does not declare.
package partman

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ags-slc/m8/internal/pgident"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Spec is one table's declared pg_partman configuration.
type Spec struct {
	Schema string
	Table  string

	// Control and Interval are fixed when create_parent runs. A declaration
	// that disagrees with the live row is refused, not reconciled: neither can
	// be changed in place without rebuilding the partition set.
	Control  string
	Interval string

	// DefaultTable and StartPartition are create_parent arguments with no
	// part_config column behind them. They apply when the table is registered
	// and are never compared afterwards.
	DefaultTable   *bool
	StartPartition *string

	// Settings are part_config columns that can be changed in place, keyed by
	// column name. Only declared keys are present.
	Settings map[string]string
}

// QualifiedName renders the table as part_config stores it: schema.table,
// unquoted.
func (s Spec) QualifiedName() string {
	return s.Schema + "." + s.Table
}

type kind int

const (
	kindBool kind = iota
	kindInt
	kindNullableText // "none" declares NULL
	kindMaintenance  // on | off | none
)

// column is a part_config column a directive may manage in place.
type column struct {
	kind kind
	// createArg: create_parent also takes it, as p_<column>, and it belongs
	// there rather than in the UPDATE after -- premake decides how many
	// children the first call creates, and by then they already exist.
	createArg bool
}

var settable = map[string]column{
	"premake":                  {kindInt, true},
	"retention":                {kindNullableText, false},
	"retention_keep_table":     {kindBool, false},
	"retention_keep_index":     {kindBool, false},
	"infinite_time_partitions": {kindBool, false},
	"inherit_privileges":       {kindBool, false},
	"automatic_maintenance":    {kindMaintenance, true},
	"jobmon":                   {kindBool, true},
}

const directivePrefix = "-- m8:partman"

// Parse extracts the partman directives from one schema/ file. pgSchema is the
// file's schema/{pg_schema}/ folder, which every table it names belongs to.
func Parse(content []byte, pgSchema string) ([]Spec, error) {
	var specs []Spec
	for i, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(trimmed, directivePrefix)
		if !ok {
			continue
		}
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
			continue // -- m8:partmanfoo is some other directive, not this one
		}
		spec, err := parseDirective(rest, pgSchema)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func parseDirective(body, pgSchema string) (Spec, error) {
	tokens, err := tokenize(body)
	if err != nil {
		return Spec{}, err
	}
	if len(tokens) == 0 || strings.Contains(tokens[0], "=") {
		return Spec{}, errors.New("m8:partman needs a table name first, as in: -- m8:partman <table> control=<column> partition_interval=<interval>")
	}
	spec := Spec{Schema: pgSchema, Table: tokens[0], Settings: map[string]string{}}
	if strings.Contains(spec.Table, ".") {
		return Spec{}, fmt.Errorf("m8:partman table %q must be unqualified: its schema is the schema/%s/ folder", spec.Table, pgSchema)
	}

	seen := map[string]bool{}
	for _, tok := range tokens[1:] {
		key, val, ok := strings.Cut(tok, "=")
		if !ok || key == "" {
			return Spec{}, fmt.Errorf("m8:partman %s: expected key=value, got %q", spec.Table, tok)
		}
		if seen[key] {
			return Spec{}, fmt.Errorf("m8:partman %s: %s given twice", spec.Table, key)
		}
		seen[key] = true

		switch key {
		case "control":
			spec.Control = val
		case "partition_interval":
			spec.Interval = val
		case "default_table":
			b, err := parseBool(val)
			if err != nil {
				return Spec{}, fmt.Errorf("m8:partman %s: default_table: %w", spec.Table, err)
			}
			spec.DefaultTable = &b
		case "start_partition":
			v := val
			spec.StartPartition = &v
		default:
			col, ok := settable[key]
			if !ok {
				return Spec{}, fmt.Errorf("m8:partman %s: unknown key %q (keys are part_config columns: %s; plus control, partition_interval, default_table, start_partition)",
					spec.Table, key, strings.Join(slices.Sorted(maps.Keys(settable)), ", "))
			}
			norm, err := normalize(col.kind, val)
			if err != nil {
				return Spec{}, fmt.Errorf("m8:partman %s: %s: %w", spec.Table, key, err)
			}
			spec.Settings[key] = norm
		}
	}
	if spec.Control == "" || spec.Interval == "" {
		return Spec{}, fmt.Errorf("m8:partman %s: control and partition_interval are required", spec.Table)
	}
	return spec, nil
}

// tokenize splits on whitespace, keeping a single-quoted value -- '1 month' --
// as one token, with ” as an escaped quote.
func tokenize(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote && c == '\'' && i+1 < len(s) && s[i+1] == '\'':
			cur.WriteByte('\'')
			i++
		case c == '\'':
			inQuote = !inQuote
			have = true
		case !inQuote && (c == ' ' || c == '\t'):
			if have {
				tokens = append(tokens, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if inQuote {
		return nil, errors.New("unterminated quote")
	}
	if have {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}

func atoiOr(s string, fallback int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return fallback
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "true", "on", "yes":
		return true, nil
	case "false", "off", "no":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a boolean", v)
}

// normalize validates a declared value and returns it in the form compare and
// render expect: "true"/"false", a decimal integer, or text.
func normalize(k kind, v string) (string, error) {
	switch k {
	case kindBool:
		b, err := parseBool(v)
		if err != nil {
			return "", err
		}
		return strconv.FormatBool(b), nil
	case kindInt:
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return "", fmt.Errorf("%q is not a non-negative integer", v)
		}
		return strconv.Itoa(n), nil
	case kindMaintenance:
		switch v {
		case "on", "off", "none":
			return v, nil
		}
		return "", fmt.Errorf("%q must be on, off or none", v)
	}
	return v, nil
}

// Directive renders a spec as the directive line Parse reads back.
func (s Spec) Directive() string {
	var b strings.Builder
	b.WriteString(directivePrefix + " " + s.Table)
	writeKV(&b, "control", s.Control)
	writeKV(&b, "partition_interval", s.Interval)
	if s.DefaultTable != nil {
		writeKV(&b, "default_table", strconv.FormatBool(*s.DefaultTable))
	}
	if s.StartPartition != nil {
		writeKV(&b, "start_partition", *s.StartPartition)
	}
	for _, k := range slices.Sorted(maps.Keys(s.Settings)) {
		writeKV(&b, k, s.Settings[k])
	}
	return b.String()
}

func writeKV(b *strings.Builder, key, val string) {
	b.WriteString(" " + key + "=")
	if val == "" || strings.ContainsAny(val, " \t'") {
		b.WriteString(pgident.Literal(val))
		return
	}
	b.WriteString(val)
}

// ExtensionSchema returns the schema pg_partman is installed in, or "" if it is
// not installed. It is not always "partman": CREATE EXTENSION without WITH
// SCHEMA puts it wherever search_path points.
//
// pg_partman before 5.0 is an error, not a missing extension: create_parent's
// signature and part_config's columns changed in 5.0, so statements written for
// 5.x would plan cleanly and fail on apply.
func ExtensionSchema(ctx context.Context, conn *pgx.Conn) (string, error) {
	var nsp, version string
	err := conn.QueryRow(ctx, `
		SELECT n.nspname, e.extversion FROM pg_extension e
		JOIN pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname = 'pg_partman'`).Scan(&nsp, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if major, _, _ := strings.Cut(version, "."); atoiOr(major, 0) < 5 {
		return "", fmt.Errorf("pg_partman %s is installed; m8:partman supports pg_partman 5.0 and later", version)
	}
	return nsp, nil
}

// Action is what bringing one table's pg_partman configuration in line with its
// declaration takes. No statements means it already matches.
type Action struct {
	Spec       Spec
	Statements []string
	// Err is a declaration m8 will not reconcile: one that disagrees with the
	// live row on something pg_partman cannot change in place.
	Err error
	// Installed is false when pg_partman is not installed in the database; the
	// statements then assume it will be in AssumedSchema.
	Installed bool
}

// AssumedSchema stands in for pg_partman's schema when planning against a
// database that does not have it yet -- typically because the ops/ migration
// that creates it is itself still pending.
const AssumedSchema = "partman"

// Plan compares each spec with the live part_config row and returns the
// statements that would reconcile them. A disagreement it cannot reconcile --
// a different control column or interval -- is recorded on that table's
// Action; the returned error is reserved for failing to read the database.
func Plan(ctx context.Context, conn *pgx.Conn, specs []Spec) ([]Action, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	nsp, err := ExtensionSchema(ctx, conn)
	if err != nil {
		return nil, err
	}
	installed := nsp != ""
	if !installed {
		nsp = AssumedSchema
	}

	actions := make([]Action, 0, len(specs))
	for _, s := range specs {
		a := Action{Spec: s, Installed: installed}
		var live *liveConfig
		if installed {
			if live, err = loadLive(ctx, conn, nsp, s); err != nil {
				return nil, err
			}
		}
		if live == nil {
			a.Statements = createStatements(nsp, s)
		} else {
			a.Statements, a.Err = reconcile(ctx, conn, nsp, s, live)
		}
		actions = append(actions, a)
	}
	return actions, nil
}

// liveConfig is the subset of a part_config row a spec can declare, every
// value rendered as text ("" for NULL), plus when maintenance last ran.
type liveConfig struct {
	control, interval, lastRun string
	settings                   map[string]string
}

func loadLive(ctx context.Context, conn *pgx.Conn, nsp string, s Spec) (*liveConfig, error) {
	cols := slices.Sorted(maps.Keys(settable))
	sel := make([]string, len(cols))
	for i, c := range cols {
		sel[i] = "coalesce(" + pgident.Quote(c) + "::text, '')"
	}
	q := fmt.Sprintf(`SELECT control, partition_interval, coalesce(maintenance_last_run::text, ''), %s
		FROM %s.part_config WHERE parent_table = $1`, strings.Join(sel, ", "), pgident.Quote(nsp))

	live := &liveConfig{settings: map[string]string{}}
	vals := make([]string, len(cols))
	dest := []any{&live.control, &live.interval, &live.lastRun}
	for i := range vals {
		dest = append(dest, &vals[i])
	}
	if err := conn.QueryRow(ctx, q, s.QualifiedName()).Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s.part_config for %s: %w", nsp, s.QualifiedName(), err)
	}
	for i, c := range cols {
		live.settings[c] = vals[i]
	}
	return live, nil
}

// Load reads one table's live pg_partman configuration as a Spec, with every
// settable key present -- the complete declaration `m8 dump` writes. nsp is
// pg_partman's schema (ExtensionSchema). It returns nil if pg_partman does not
// manage the table.
func Load(ctx context.Context, conn *pgx.Conn, nsp, pgSchema, table string) (*Spec, error) {
	s := Spec{Schema: pgSchema, Table: table}
	live, err := loadLive(ctx, conn, nsp, s)
	if err != nil || live == nil {
		return nil, err
	}
	s.Control, s.Interval, s.Settings = live.control, live.interval, live.settings
	if s.Settings["retention"] == "" {
		s.Settings["retention"] = "none"
	}

	// default_table has no part_config column; whether the table has one is the
	// answer. pg_partman 5 names it <parent>_default, but the bound is what makes
	// a partition the default, so ask for that rather than the name.
	var hasDefault bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			WHERE i.inhparent = to_regclass($1) AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT')`,
		pgident.Qualify(pgSchema, table)).Scan(&hasDefault); err != nil {
		return nil, err
	}
	s.DefaultTable = &hasDefault
	return &s, nil
}

func createStatements(nsp string, s Spec) []string {
	args := []string{
		"p_parent_table := " + pgident.Literal(s.QualifiedName()),
		"p_control := " + pgident.Literal(s.Control),
		"p_interval := " + pgident.Literal(s.Interval),
	}
	if s.DefaultTable != nil {
		args = append(args, "p_default_table := "+strconv.FormatBool(*s.DefaultTable))
	}
	if s.StartPartition != nil {
		args = append(args, "p_start_partition := "+pgident.Literal(*s.StartPartition))
	}
	var rest []string
	for _, k := range slices.Sorted(maps.Keys(s.Settings)) {
		if settable[k].createArg {
			args = append(args, "p_"+k+" := "+renderValue(settable[k].kind, s.Settings[k]))
		} else {
			rest = append(rest, k)
		}
	}
	stmts := []string{fmt.Sprintf("SELECT %s.create_parent(%s)", pgident.Quote(nsp), strings.Join(args, ", "))}
	if len(rest) > 0 {
		stmts = append(stmts, updateStatement(nsp, s, rest))
	}
	return stmts
}

// reconcile returns the UPDATE that brings the live row in line, or a refusal.
// A failure to compare is returned as a refusal too: either way the table is
// not reconciled, and the message says why.
func reconcile(ctx context.Context, conn *pgx.Conn, nsp string, s Spec, live *liveConfig) ([]string, error) {
	if live.control != s.Control {
		return nil, fmt.Errorf("m8:partman %s: control is %s in the database but declared %s; pg_partman cannot change the control column of an existing partition set",
			s.QualifiedName(), live.control, s.Control)
	}
	same, err := sameInterval(ctx, conn, live.interval, s.Interval)
	if err != nil {
		return nil, err
	}
	if !same {
		return nil, fmt.Errorf("m8:partman %s: partition_interval is %s in the database but declared %s; pg_partman cannot change the interval of an existing partition set",
			s.QualifiedName(), live.interval, s.Interval)
	}

	var changed []string
	for _, k := range slices.Sorted(maps.Keys(s.Settings)) {
		eq, err := sameSetting(ctx, conn, k, live.settings[k], s.Settings[k])
		if err != nil {
			return nil, err
		}
		if !eq {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	return []string{updateStatement(nsp, s, changed)}, nil
}

func sameSetting(ctx context.Context, conn *pgx.Conn, key, live, want string) (bool, error) {
	if settable[key].kind != kindNullableText {
		return live == want, nil
	}
	if want == "none" {
		return live == "", nil
	}
	if live == "" {
		return false, nil
	}
	return sameInterval(ctx, conn, live, want)
}

// sameInterval compares two interval spellings by the server's canonical
// rendering of each: part_config stores '1 month' as '1 mon', and both render
// as "1 mon". Not by interval equality, which treats '1 mon' and '30 days' as
// equal -- they are different partition intervals to pg_partman, which steps
// by calendar month in one and by 30 days in the other. A value the server
// will not read as an interval compares as text. Only called outside a
// transaction, so a failed cast costs nothing but the round trip.
func sameInterval(ctx context.Context, conn *pgx.Conn, a, b string) (bool, error) {
	if a == b {
		return true, nil
	}
	var eq bool
	err := conn.QueryRow(ctx, `SELECT $1::interval::text = $2::interval::text`, a, b).Scan(&eq)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22") { // data exception: not an interval
		return false, nil
	}
	return eq, err
}

func updateStatement(nsp string, s Spec, keys []string) string {
	sets := make([]string, len(keys))
	for i, k := range keys {
		sets[i] = pgident.Quote(k) + " = " + renderValue(settable[k].kind, s.Settings[k])
	}
	return fmt.Sprintf("UPDATE %s.part_config SET %s WHERE parent_table = %s",
		pgident.Quote(nsp), strings.Join(sets, ", "), pgident.Literal(s.QualifiedName()))
}

func renderValue(k kind, v string) string {
	switch k {
	case kindBool, kindInt:
		return v
	case kindNullableText:
		if v == "none" {
			return "NULL"
		}
	}
	return pgident.Literal(v)
}

// Status is what `m8 status` reports for a declared table: whether pg_partman
// knows about it, and when maintenance last ran over it.
type Status struct {
	Spec       Spec
	Registered bool
	// LastRun is part_config.maintenance_last_run rendered as text; "" means
	// maintenance has never run for this table, which is the state a table is
	// in when nothing schedules run_maintenance at all.
	LastRun string
}

// Statuses reports each spec's registration and last maintenance run. Without
// pg_partman installed, every declared table is unregistered.
func Statuses(ctx context.Context, conn *pgx.Conn, specs []Spec) ([]Status, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	nsp, err := ExtensionSchema(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(specs))
	for _, s := range specs {
		st := Status{Spec: s}
		if nsp != "" {
			live, err := loadLive(ctx, conn, nsp, s)
			if err != nil {
				return nil, err
			}
			if live != nil {
				st.Registered, st.LastRun = true, live.lastRun
			}
		}
		out = append(out, st)
	}
	return out, nil
}
