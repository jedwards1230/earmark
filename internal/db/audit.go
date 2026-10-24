package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The audit trail read path (CONTRACT §2.12.2): one generic, read-only query
// over the tables the pipeline already appends to — finding_events,
// judge_passes, fn_calls, step_runs, pipeline_events — served as stored.
//
// The registry below is the security boundary. A request names a source and
// equality filters; the table and column names in the SQL come only from the
// registry, every value is a bind parameter, and only the allowlisted columns
// are ever selected. The rule for what is on the list: no prompt or reply
// bodies, no error text (it can wrap an upstream response body), no command
// lines, no library paths or titles — what happened is still served through
// ids, statuses, error classes, counters and timestamps. Every served column is
// an id, uuid, hash, number, timestamp, boolean, closed-vocabulary token,
// code-defined name, or text cut to auditClip characters. A column added to one
// of these tables by a later migration is NOT served until it is added here
// (TestIntegrationAuditSchemaGuard fails until someone decides allow vs
// exclude).

const (
	// AuditDefaultLimit and AuditMaxLimit bound a page.
	AuditDefaultLimit = 100
	AuditMaxLimit     = 500

	// AuditStatementTimeout is the server-side statement_timeout of every audit
	// read. A caller's context should outlive it by a margin so the server's
	// cancellation, not a torn connection, is what ends a slow query.
	AuditStatementTimeout = 5 * time.Second

	// auditClip is the length externally-sourced text columns (model, host and
	// actor names, reason codes) are cut to.
	auditClip = 256

	// pgQueryCanceled is SQLSTATE 57014, raised by statement_timeout.
	pgQueryCanceled = "57014"
)

var (
	// ErrAuditUnknownSource: the source is not in the registry.
	ErrAuditUnknownSource = errors.New("unknown audit source")
	// ErrAuditUnknownFilter: the filter is not one of the source's.
	ErrAuditUnknownFilter = errors.New("unknown filter")
	// ErrAuditBadFilterValue: the filter exists but the value is empty or not a
	// UUID for a UUID filter.
	ErrAuditBadFilterValue = errors.New("invalid filter value")
	// ErrAuditTimeout: the read ran past AuditStatementTimeout (or its context
	// deadline).
	ErrAuditTimeout = errors.New("audit query timed out")
)

// AuditColumn is one served column. Clip > 0 cuts a free-text column to its
// first Clip characters (left(col, Clip)).
type AuditColumn struct {
	Name string
	Clip int
}

// AuditFilter is one equality filter: the query parameter Name filters Column.
// UUID filters must carry a canonical UUID.
type AuditFilter struct {
	Name   string
	Column string
	UUID   bool
}

// AuditSource is one servable table. Columns[0] is the id column — always
// "id" — which is the cursor and the sort key.
type AuditSource struct {
	Name    string
	Table   string
	Columns []AuditColumn
	Filters []AuditFilter
}

func plainCol(name string) AuditColumn   { return AuditColumn{Name: name} }
func clippedCol(name string) AuditColumn { return AuditColumn{Name: name, Clip: auditClip} }

// auditSources is the registry. Columns are allowlisted by name (migrations
// 00001, 00007, 00008, 00010, 00013, 00015); what each table leaves out, and
// why, is in CONTRACT §2.12.2:
//
//	judge_passes    file_path (a library path), error (error text)
//	fn_calls        input, output (the JSONB prompt and model reply)
//	step_runs       args (the command line: book names, endpoint overrides),
//	                error (error text), last_message (can name the audio file)
//	pipeline_events file_path (a library path), reason (failure text),
//	                detail (JSONB of unbounded, stage-specific shape)
var auditSources = []AuditSource{
	{
		Name: "finding_events", Table: "finding_events",
		Columns: []AuditColumn{
			plainCol("id"), plainCol("finding_id"), plainCol("transcript_id"), plainCol("kind"), plainCol("from_state"), plainCol("to_state"),
			plainCol("outcome"), clippedCol("reason"), clippedCol("actor"), plainCol("recipe_id"), plainCol("p"), plainCol("evidence"),
			plainCol("fn_call_id"), plainCol("chunk_text_sha256"), plainCol("issue_type"), plainCol("revokes_event_id"),
			plainCol("related_finding_id"), plainCol("anchor_offset"), plainCol("created_at"),
		},
		Filters: []AuditFilter{
			{Name: "finding_id", Column: "finding_id", UUID: true},
			{Name: "transcript_id", Column: "transcript_id", UUID: true},
			{Name: "recipe_id", Column: "recipe_id"},
			{Name: "kind", Column: "kind"},
		},
	},
	{
		Name: "judge_passes", Table: "judge_passes",
		Columns: []AuditColumn{
			plainCol("id"), plainCol("transcript_id"), plainCol("job_id"), plainCol("recipe_id"), plainCol("step_run_id"), plainCol("status"),
			plainCol("chunks_total"), plainCol("chunks_judged"), plainCol("chunks_failed"), plainCol("chunks_cached"),
			plainCol("findings_kept"), plainCol("dropped"), plainCol("input_tokens"), plainCol("output_tokens"), plainCol("cost_usd"),
			plainCol("latency_ms_p50"), plainCol("latency_ms_p95"), plainCol("book_context_sha256"), plainCol("started_at"),
			plainCol("finished_at"),
		},
		Filters: []AuditFilter{
			{Name: "transcript_id", Column: "transcript_id", UUID: true},
			{Name: "recipe_id", Column: "recipe_id"},
			{Name: "status", Column: "status"},
		},
	},
	{
		Name: "fn_calls", Table: "fn_calls",
		Columns: []AuditColumn{
			plainCol("id"), plainCol("fn"), plainCol("prompt_version"), plainCol("prompt_sha256"), clippedCol("model_alias"),
			clippedCol("model_resolved"), clippedCol("model_revision"), plainCol("recipe_id"), plainCol("input_sha256"),
			plainCol("error_class"), plainCol("latency_ms"), plainCol("input_tokens"), plainCol("output_tokens"), plainCol("cost_usd"),
			plainCol("cache_hit"), plainCol("cached_from"), plainCol("created_at"),
		},
		Filters: []AuditFilter{
			{Name: "fn", Column: "fn"},
			{Name: "recipe_id", Column: "recipe_id"},
			{Name: "error_class", Column: "error_class"},
		},
	},
	{
		Name: "step_runs", Table: "step_runs",
		Columns: []AuditColumn{
			plainCol("id"), plainCol("step"), plainCol("mode"), plainCol("recipe_id"), clippedCol("model"), clippedCol("host"), plainCol("started_at"),
			plainCol("heartbeat_at"), plainCol("finished_at"), plainCol("status"), plainCol("total"), plainCol("done"), plainCol("counters"),
			plainCol("cost_usd"),
		},
		Filters: []AuditFilter{
			{Name: "step", Column: "step"},
			{Name: "status", Column: "status"},
			{Name: "recipe_id", Column: "recipe_id"},
		},
	},
	{
		Name: "pipeline_events", Table: "pipeline_events",
		Columns: []AuditColumn{
			plainCol("id"), plainCol("job_id"), plainCol("stage"), plainCol("event"), clippedCol("runner_host"),
			clippedCol("model"), clippedCol("model_version"), plainCol("duration_ms"), plainCol("item_count"), plainCol("token_count"),
			plainCol("attempt"), plainCol("created_at"),
		},
		Filters: []AuditFilter{
			{Name: "job_id", Column: "job_id", UUID: true},
			{Name: "stage", Column: "stage"},
			{Name: "event", Column: "event"},
		},
	},
}

// AuditSources returns a copy of the registry, in the order sources are listed.
func AuditSources() []AuditSource {
	out := make([]AuditSource, len(auditSources))
	for i, s := range auditSources {
		out[i] = s.clone()
	}
	return out
}

// LookupAuditSource returns the named source, or an error wrapping
// ErrAuditUnknownSource that lists the valid names.
func LookupAuditSource(name string) (AuditSource, error) {
	for _, s := range auditSources {
		if s.Name == name {
			return s.clone(), nil
		}
	}
	names := make([]string, len(auditSources))
	for i, s := range auditSources {
		names[i] = s.Name
	}
	return AuditSource{}, fmt.Errorf("%w %q; sources: %s", ErrAuditUnknownSource, name, strings.Join(names, ", "))
}

func (s AuditSource) clone() AuditSource {
	s.Columns = slices.Clone(s.Columns)
	s.Filters = slices.Clone(s.Filters)
	return s
}

// FilterNames returns the source's filter parameter names in registry order.
func (s AuditSource) FilterNames() []string {
	names := make([]string, len(s.Filters))
	for i, f := range s.Filters {
		names[i] = f.Name
	}
	return names
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// CheckFilter validates one filter parameter against the source: its name must
// be one of the source's, its value non-empty and, for a UUID filter, a
// canonical UUID. The error never echoes the value.
func (s AuditSource) CheckFilter(name, value string) error {
	for _, f := range s.Filters {
		if f.Name != name {
			continue
		}
		switch {
		case value == "":
			return fmt.Errorf("%w: %q is empty", ErrAuditBadFilterValue, name)
		case strings.ContainsRune(value, 0) || !utf8.ValidString(value):
			// PostgreSQL text holds neither a NUL nor invalid UTF-8 (22021).
			return fmt.Errorf("%w: %q must be valid text", ErrAuditBadFilterValue, name)
		case f.UUID && !uuidPattern.MatchString(value):
			return fmt.Errorf("%w: %q must be a UUID", ErrAuditBadFilterValue, name)
		}
		return nil
	}
	return fmt.Errorf("%w %q for source %q; filters: %s", ErrAuditUnknownFilter, name, s.Name,
		strings.Join(s.FilterNames(), ", "))
}

// AuditCursor says where an audit read starts and which way it runs: ascending
// from the rows with id > ID, or, with Desc, newest first from the rows with
// id < ID. The zero value is the first page of an ascending read.
type AuditCursor struct {
	ID   int64
	Desc bool
}

// AuditAfter is the ascending cursor: rows with id > id, oldest first.
func AuditAfter(id int64) AuditCursor { return AuditCursor{ID: id} }

// AuditBefore is the descending cursor: rows with id < id, newest first.
func AuditBefore(id int64) AuditCursor { return AuditCursor{ID: id, Desc: true} }

// AuditNewest starts a descending read at the newest row. It is AuditBefore
// of the largest id, so the statement keeps its one `id < $1` bind predicate
// (a plain primary-key range scan); a row with id MaxInt64 cannot exist.
func AuditNewest() AuditCursor { return AuditBefore(math.MaxInt64) }

// selectExpr renders the column for the inner SELECT.
func (c AuditColumn) selectExpr() string {
	if c.Clip > 0 {
		return "left(" + c.Name + ", " + strconv.Itoa(c.Clip) + ") AS " + c.Name
	}
	return c.Name
}

// buildQuery renders the statement and its bind arguments for one request,
// ascending:
//
//	SELECT coalesce(json_agg(row_to_json(t) ORDER BY t.id), '[]'::json), max(t.id)
//	  FROM (SELECT <columns> FROM <table> WHERE id > $1 [AND <column> = $n …]
//	         ORDER BY id LIMIT $k) t
//
// and for a descending cursor with `id < $1`, `ORDER BY id DESC` (in the
// subquery and in the aggregate) and min(t.id): the page's last row either way.
//
// Identifiers come from the registry alone; the cursor, every filter value and
// the limit are bind parameters (a UUID filter is cast server-side from text).
// Filter clauses follow registry order, so the SQL is deterministic.
func (s AuditSource) buildQuery(cur AuditCursor, limit int, filters map[string]string) (string, []any, error) {
	if cur.ID < 0 {
		return "", nil, fmt.Errorf("audit cursor %d: must be >= 0", cur.ID)
	}
	if limit < 1 || limit > AuditMaxLimit {
		return "", nil, fmt.Errorf("audit limit %d: must be 1..%d", limit, AuditMaxLimit)
	}
	names := make([]string, 0, len(filters))
	for name := range filters {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := s.CheckFilter(name, filters[name]); err != nil {
			return "", nil, err
		}
	}

	cmp, dir, last := ">", "", "max"
	if cur.Desc {
		cmp, dir, last = "<", " DESC", "min"
	}
	args := []any{cur.ID}
	var b strings.Builder
	b.WriteString("SELECT coalesce(json_agg(row_to_json(t) ORDER BY t.id" + dir + "), '[]'::json), " + last + "(t.id) FROM (SELECT ")
	for i, c := range s.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c.selectExpr())
	}
	b.WriteString(" FROM ")
	b.WriteString(s.Table)
	b.WriteString(" WHERE id " + cmp + " $1")
	for _, f := range s.Filters {
		v, ok := filters[f.Name]
		if !ok {
			continue
		}
		args = append(args, v)
		b.WriteString(" AND " + f.Column + " = $" + strconv.Itoa(len(args)))
		if f.UUID {
			b.WriteString("::text::uuid")
		}
	}
	args = append(args, limit)
	b.WriteString(" ORDER BY id" + dir + " LIMIT $" + strconv.Itoa(len(args)) + ") t")
	return b.String(), args, nil
}

// AuditPage is one page of an audit source, in the shape of the API response.
// Rows is a JSON array of objects, one per row, exactly as stored, in the
// order read; Next is the last row's id — the highest of an ascending page,
// the lowest of a descending one — and nil when the page is empty.
type AuditPage struct {
	Source string          `json:"source"`
	Rows   json.RawMessage `json:"rows"`
	Next   *int64          `json:"next"`
}

// auditSessionSQL scopes the read transaction: JIT off (a planner estimate on a
// large log can cross jit_above_cost and compilation then dominates a
// millisecond query, as it did for the scorecards), the session time zone pinned
// to UTC (timestamps render with a +00:00 offset whatever the database is set
// to) and a hard statement timeout. $1 is the timeout in milliseconds.
const auditSessionSQL = `SELECT set_config('jit', 'off', true), set_config('TimeZone', 'UTC', true), ` +
	`set_config('statement_timeout', $1, true)`

// AuditRows returns up to limit rows of the named source from cur — ascending
// after an id, or newest first before one — narrowed by the equality filters
// (CONTRACT §2.12.2). It validates everything the HTTP layer does, so a caller
// cannot reach the SQL builder with an unlisted source, filter or
// out-of-range limit.
func (db *DB) AuditRows(ctx context.Context, source string, cur AuditCursor, limit int, filters map[string]string) (AuditPage, error) {
	s, err := LookupAuditSource(source)
	if err != nil {
		return AuditPage{}, err
	}
	return readAudit(ctx, db.pool, s, cur, limit, filters)
}

// readAudit runs one audit read in a read-only transaction with JIT off and the
// statement timeout set.
func readAudit(ctx context.Context, b txOptionsBeginner, s AuditSource, cur AuditCursor, limit int,
	filters map[string]string) (AuditPage, error) {
	query, args, err := s.buildQuery(cur, limit, filters)
	if err != nil {
		return AuditPage{}, err
	}
	tx, err := b.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return AuditPage{}, auditError("begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, auditSessionSQL, strconv.FormatInt(AuditStatementTimeout.Milliseconds(), 10)); err != nil {
		return AuditPage{}, auditError("session settings", err)
	}
	var rows []byte
	var next *int64
	if err := tx.QueryRow(ctx, query, args...).Scan(&rows, &next); err != nil {
		return AuditPage{}, auditError(s.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AuditPage{}, auditError("commit", err)
	}
	return AuditPage{Source: s.Name, Rows: json.RawMessage(rows), Next: next}, nil
}

// auditError wraps a failed audit read, mapping the statement timeout (and a
// context deadline) to ErrAuditTimeout. A canceled context stays itself: the
// caller went away, nothing timed out.
func auditError(stage string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("audit %s: %w", stage, err)
	}
	var pgErr *pgconn.PgError
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &pgErr) && pgErr.Code == pgQueryCanceled) {
		return fmt.Errorf("audit %s: %w (%v)", stage, ErrAuditTimeout, err)
	}
	return fmt.Errorf("audit %s: %w", stage, err)
}
