// Package audit implements `earmark audit`: read the append-only audit trail
// (CONTRACT §2.12.2) from the command line. It is the same read as
// GET /api/v1/audit/{source} — one registry (internal/db/audit.go) names the
// sources, columns and filters, and db.AuditRows runs the query in a read-only
// transaction with a statement timeout — so what is served, and what is
// withheld, cannot differ between the API and the CLI.
package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	earmarklog "github.com/jedwards1230/earmark/internal/log"
)

const (
	// DefaultLimit is the rows printed when --limit is not given: a screenful,
	// well under the endpoint's page (db.AuditDefaultLimit) and its cap
	// (db.AuditMaxLimit).
	DefaultLimit = 20

	// cellMax is the width, in characters, the default view cuts a cell to.
	cellMax = 48
	// digestShown is how much of a 64-hex digest (recipe ids, prompt and input
	// hashes) the default view shows.
	digestShown = 12
)

// Reader is the slice of the DB the command needs (*db.DB implements it).
type Reader interface {
	AuditRows(ctx context.Context, source string, cur db.AuditCursor, limit int, filters map[string]string) (db.AuditPage, error)
}

// Flags are the raw command-line values, before validation.
type Flags struct {
	Limit   int
	Filters []string // name=value, repeatable
	Columns []string
	// After and Before are the cursors; HasAfter / HasBefore say whether the
	// flag was given (0 is a valid cursor).
	After, Before       int64
	HasAfter, HasBefore bool
	Asc                 bool
	JSON                bool
}

// Options is a validated request.
type Options struct {
	Source  db.AuditSource
	Cursor  db.AuditCursor
	Limit   int
	Filters map[string]string
	// Columns are the columns the text view prints, in the order asked; empty
	// means every column of the source, abbreviated (see render).
	Columns []string
	JSON    bool
}

var flags = Flags{Limit: DefaultLimit}

// AuditCmd is `earmark audit`.
var AuditCmd = &cobra.Command{
	Use:   "audit <source> [flags]",
	Short: "Read the audit trail (finding_events, judge_passes, fn_calls, step_runs, pipeline_events)",
	Long:  longHelp(),
	Args:  sourceArg,
	// Registry names, for shell completion.
	ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return sourceNames(), cobra.ShellCompDirectiveNoFileComp
	},
	Run: runAudit,
}

func init() {
	f := AuditCmd.Flags()
	f.IntVarP(&flags.Limit, "limit", "n", DefaultLimit, fmt.Sprintf("rows to print (1..%d)", db.AuditMaxLimit))
	f.StringArrayVarP(&flags.Filters, "filter", "f", nil, "equality filter name=value (the source's filters are listed in --help); repeatable, ANDed")
	f.StringSliceVarP(&flags.Columns, "columns", "c", nil, "text view: print only these columns, whole (comma-separated, in this order)")
	f.Int64Var(&flags.After, "after", 0, "page oldest first: rows with id greater than this (the previous page's next)")
	f.Int64Var(&flags.Before, "before", 0, "page newest first: rows with id less than this (the previous page's next)")
	f.BoolVar(&flags.Asc, "asc", false, "read oldest first, from the start of the table (default: newest first)")
	f.BoolVar(&flags.JSON, "json", false, "print the page as the API serves it: {\"source\":…,\"rows\":[…],\"next\":…}")
}

func longHelp() string {
	var b strings.Builder
	b.WriteString(`audit prints rows of the append-only audit trail (CONTRACT §2.12.2), newest
first, as stored. It is the read side of what the pipeline already writes: the
same registry, query and limits as GET /api/v1/audit/{source}, run straight
against the database (DATABASE_URL), read-only, with a 5 s statement timeout.
(Like every command it applies pending schema migrations on connect.)

Only the registry's slim columns are served — never prompt or reply bodies,
error text, command lines or library paths.

Sources and their filters:

`)
	for _, s := range db.AuditSources() {
		fmt.Fprintf(&b, "  %-16s %s\n", s.Name, strings.Join(s.FilterNames(), ", "))
	}
	fmt.Fprintf(&b, `
Paging: a page is --limit rows (default %d, at most %d). When it is full the
last line gives the cursor for the next one: --before N continues newest first,
--after N continues oldest first (start there with --asc). --before and --after
do not mix.

The text view shows every column, abbreviated: 64-hex digests cut to %d
characters, timestamps to whole seconds (UTC, Z), long values to %d; name
columns with --columns to see them whole. --json always carries every column,
whole, in the shape the API returns (--columns does not apply). Diagnostics go
to stderr, so stdout is the page alone.

Tailing: finding_events ids commit out of order, so a cursor can skip rows that
were still uncommitted when it was read; re-read an overlap rather than relying
on --after alone (CONTRACT §2.12.2, "Semantics a consumer must know").

Examples:
  earmark audit step_runs                          # the 20 newest step runs
  earmark audit step_runs -f status=running --json
  earmark audit fn_calls -f error_class=timeout -n 50 -c id,fn,model_alias,latency_ms,created_at
  earmark audit finding_events -f transcript_id=<uuid> --asc
  earmark audit judge_passes -f recipe_id=<recipe> --before 8700`, DefaultLimit, db.AuditMaxLimit, digestShown, cellMax)
	return b.String()
}

func sourceNames() []string {
	srcs := db.AuditSources()
	names := make([]string, len(srcs))
	for i, s := range srcs {
		names[i] = s.Name
	}
	return names
}

// sourceArg requires exactly one source and, when it is missing, lists the
// valid ones instead of cobra's generic arity message.
func sourceArg(_ *cobra.Command, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("want one source; sources: %s", strings.Join(sourceNames(), ", "))
	}
	return nil
}

// ParseOptions validates the flags against the registry. Every problem is
// reported before the database is touched.
func ParseOptions(source string, f Flags) (Options, error) {
	src, err := db.LookupAuditSource(source)
	if err != nil {
		return Options{}, err
	}
	o := Options{Source: src, Limit: f.Limit, JSON: f.JSON}
	if f.Limit < 1 || f.Limit > db.AuditMaxLimit {
		return Options{}, fmt.Errorf("--limit %d: want 1..%d", f.Limit, db.AuditMaxLimit)
	}

	switch {
	case f.HasAfter && f.HasBefore:
		return Options{}, errors.New("--after reads oldest first and --before newest first: give one")
	case f.HasBefore && f.Asc:
		return Options{}, errors.New("--before reads newest first: drop --asc, or use --after")
	case f.HasAfter && f.After < 0:
		return Options{}, fmt.Errorf("--after %d: want an id >= 0", f.After)
	case f.HasBefore && f.Before < 0:
		return Options{}, fmt.Errorf("--before %d: want an id >= 0", f.Before)
	case f.HasAfter:
		o.Cursor = db.AuditAfter(f.After)
	case f.Asc:
		o.Cursor = db.AuditAfter(0)
	case f.HasBefore:
		o.Cursor = db.AuditBefore(f.Before)
	default:
		o.Cursor = db.AuditNewest()
	}

	for _, kv := range f.Filters {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return Options{}, fmt.Errorf("--filter %q: want name=value; filters of %s: %s", kv, src.Name, strings.Join(src.FilterNames(), ", "))
		}
		if _, dup := o.Filters[name]; dup {
			return Options{}, fmt.Errorf("--filter %q given twice", name)
		}
		if err := src.CheckFilter(name, value); err != nil {
			return Options{}, err
		}
		if o.Filters == nil {
			o.Filters = map[string]string{}
		}
		o.Filters[name] = value
	}

	if len(f.Columns) > 0 {
		if f.JSON {
			return Options{}, errors.New("--columns narrows the text view; --json prints every column")
		}
		valid := columnNames(src)
		for _, c := range f.Columns {
			if !slices.Contains(valid, c) {
				return Options{}, fmt.Errorf("--columns: %q is not a column of %s; columns: %s", c, src.Name, strings.Join(valid, ", "))
			}
			if slices.Contains(o.Columns, c) {
				return Options{}, fmt.Errorf("--columns: %q given twice", c)
			}
			o.Columns = append(o.Columns, c)
		}
	}
	return o, nil
}

func columnNames(s db.AuditSource) []string {
	names := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		names[i] = c.Name
	}
	return names
}

func runAudit(cmd *cobra.Command, args []string) {
	f := flags
	f.HasAfter = cmd.Flags().Changed("after")
	f.HasBefore = cmd.Flags().Changed("before")
	o, err := ParseOptions(args[0], f)
	exitOn(err, 2)
	exitOn(read(cmd.Context(), o), 1)
}

func exitOn(err error, code int) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(code)
	}
}

// read opens the database, prints the page and closes the database before it
// returns. The package logger writes to stdout by default; it is pointed at
// stderr first so a --json page is not mixed with "database initialized".
func read(ctx context.Context, o Options) error {
	earmarklog.SetOutput(os.Stderr)
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	database, err := db.New(cfg)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer database.Close()
	return Run(ctx, os.Stdout, database, o)
}

// Run reads one page and prints it.
func Run(ctx context.Context, out io.Writer, store Reader, o Options) error {
	// The server-side statement timeout ends a slow read; the context outlives
	// it by a margin so that, not a torn connection, is what the caller sees.
	ctx, cancel := context.WithTimeout(ctx, db.AuditStatementTimeout+time.Second)
	defer cancel()
	page, err := store.AuditRows(ctx, o.Source.Name, o.Cursor, o.Limit, o.Filters)
	switch {
	case errors.Is(err, db.ErrAuditTimeout):
		return fmt.Errorf("the read of %s timed out; add a --filter or lower --limit: %w", o.Source.Name, err)
	case err != nil:
		return err
	}
	if len(page.Rows) == 0 {
		page.Rows = json.RawMessage("[]")
	}
	if o.JSON {
		return json.NewEncoder(out).Encode(page)
	}
	return render(out, o, page)
}

// render prints the page as a table in registry column order. The default view
// abbreviates; an explicit --columns list prints its cells whole.
func render(out io.Writer, o Options, page db.AuditPage) error {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(page.Rows, &rows); err != nil {
		return fmt.Errorf("decode %s rows: %w", page.Source, err)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintf(out, "no %s rows\n", o.Source.Name)
		return err
	}
	cols, whole := o.Columns, true
	if len(cols) == 0 {
		cols, whole = columnNames(o.Source), false
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, strings.ToUpper(strings.Join(cols, "\t")))
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = cell(r[c], whole)
		}
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// A full page may have more behind it; a short one is the end.
	if len(rows) >= o.Limit && page.Next != nil {
		flag := "--after"
		if o.Cursor.Desc {
			flag = "--before"
		}
		_, err := fmt.Fprintf(out, "\n%d rows · next page: %s %d\n", len(rows), flag, *page.Next)
		return err
	}
	return nil
}

var (
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// timestampPattern is a timestamp as the audit read renders it: UTC (the
	// read pins the session time zone), microseconds, a +00:00 offset.
	timestampPattern = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.\d+)?\+00:00$`)
)

// cell renders one JSON value for the table: NULL is "-", a string is itself,
// anything else (number, boolean, counter object) its compact JSON. Anything
// that is not printable never reaches the terminal — the text columns are
// model, host and actor names and reason codes from outside the process — and,
// unless whole, 64-hex digests are cut, timestamps drop their fraction (and
// show Z for the +00:00 every audit timestamp carries) and long values are
// clipped.
func cell(raw json.RawMessage, whole bool) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "-"
	}
	s := string(raw)
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &s); err != nil {
			s = string(raw)
		}
	} else {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err == nil {
			s = buf.String()
		}
	}
	s = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return '?'
		}
		return r
	}, s)
	if whole {
		return s
	}
	if digestPattern.MatchString(s) {
		return s[:digestShown] + "…"
	}
	if m := timestampPattern.FindStringSubmatch(s); m != nil {
		return m[1] + "Z"
	}
	if r := []rune(s); len(r) > cellMax {
		return string(r[:cellMax-1]) + "…"
	}
	return s
}
