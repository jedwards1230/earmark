package db

// Postgres integration tests for the audit trail read path (CONTRACT §2.12.2).
// Skipped unless EARMARK_TEST_DATABASE_URL is set (see migrate_integration_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedwards1230/earmark/internal/patch"
)

const (
	auJobB        = "00000000-0000-0000-0000-00000000a0b0"
	auTranscriptA = "00000000-0000-0000-0000-00000000a0a1"
	auTranscriptB = "00000000-0000-0000-0000-00000000a0b1"
	auHostile     = "x'; DROP TABLE fn_calls;--"
)

var (
	auRecipe1 = strings.Repeat("a1", 32)
	auRecipe2 = strings.Repeat("b2", 32)
)

type auditFixture struct {
	d        *DB
	url      string
	findings []string // finding ids; [0] and [1] carry decisions, [2] a transition
}

// seedAudit fills all five sources. Every column the endpoint must not serve
// carries a marker it must never return (PROMPT-BODY, REPLY-BODY, ARGS-SECRET,
// DETAIL-BODY, ERRTEXT-MARKER, LASTMSG-MARKER, REASON-MARKER, hunter2, the
// /b/ library paths), and every clipped column a 700-character value.
func seedAudit(t *testing.T) auditFixture {
	t.Helper()
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)

	ids := seedFindingEvents(t, conn, 3)
	registerTestRecipe(t, conn, auRecipe1, 1)
	registerTestRecipe(t, conn, auRecipe2, 2)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}

	// finding_events: 12 decisions on finding 0 (recipe 1), 6 on finding 1
	// (recipe 2), one with a 700-character reason, 3 revokes of the first
	// decisions, and the transition rows the triggers write for finding 2.
	exec(`INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id, p, evidence)
	      SELECT $1::uuid, $3::uuid, 'decision', 'apply', 'rung0', 'jev:' || $2::text, $2, 0.9, 'asin_verbatim'
	        FROM generate_series(1, 12)`, ids[0], auRecipe1, feTranscript)
	exec(`INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id, p, evidence)
	      SELECT $1::uuid, $3::uuid, 'decision', 'hold', 'model', 'jev:' || $2::text, $2, 0.4, 'none'
	        FROM generate_series(1, 6)`, ids[1], auRecipe2, feTranscript)
	exec(`INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id, p, evidence)
	      VALUES ($1::uuid, $3::uuid, 'decision', 'reject', repeat('x', 700), 'jev:' || $2::text, $2, 0.05, 'none')`,
		ids[0], auRecipe1, feTranscript)
	exec(`INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id, p, evidence)
	      VALUES ($1::uuid, $3::uuid, 'decision', 'apply', 'long actor', repeat('a', 700), $2, 0.8, 'none')`,
		ids[1], auRecipe2, feTranscript)
	exec(`INSERT INTO finding_events (finding_id, transcript_id, kind, reason, actor, recipe_id, revokes_event_id)
	      SELECT finding_id, transcript_id, 'revoke', 'undo', 'cli:test', recipe_id, id
	        FROM finding_events WHERE kind = 'decision' ORDER BY id LIMIT 3`)
	if err := d.SetPatchState(ctx, ids[2], patch.StateProposed, patch.StateAccepted, "mcp:justin"); err != nil {
		t.Fatal(err)
	}

	// judge_passes: one of every status; the failed one has a 700-character error.
	exec(`INSERT INTO judge_passes (transcript_id, job_id, file_path, recipe_id, status, chunks_total, chunks_judged,
	                                chunks_failed, chunks_cached, findings_kept, dropped, input_tokens, output_tokens,
	                                cost_usd, started_at, finished_at, error)
	      VALUES ($1, $3, '/b/AU/a.m4b', $4, 'complete', 10, 10, 0, 2, 3, '{"empty": 2}', 900, 120, 0.00123400, now(), now(), NULL),
	             ($1, $3, '/b/AU/a.m4b', $5, 'complete', 10, 10, 0, 0, 1, NULL, 900, 120, 0.5, now(), now(), NULL),
	             ($2, $3, '/b/AU/b.m4b', $4, 'failed', 10, 3, 7, 0, 0, NULL, 90, 12, 0.1, now(), now(), 'ERRTEXT-MARKER ' || repeat('e', 700)),
	             ($2, $3, '/b/AU/b.m4b', $5, 'partial', 10, 8, 2, 0, 1, NULL, 90, 12, 0.1, now(), now(), 'ERRTEXT-MARKER two chunks failed'),
	             ($1, $3, '/b/AU/a.m4b', $4, 'running', 10, 1, 0, 0, 0, NULL, NULL, NULL, NULL, now(), NULL, NULL),
	             ($2, $3, '/b/AU/b.m4b', NULL, 'legacy', NULL, NULL, NULL, NULL, 4, NULL, NULL, NULL, NULL, now(), now(), NULL)`,
		auTranscriptA, auTranscriptB, auJobB, auRecipe1, auRecipe2)

	// fn_calls: 8 rows, alternating fn; the last one is a failed 503 with no reply.
	exec(`INSERT INTO fn_calls (fn, prompt_version, prompt_sha256, model_alias, model_resolved, recipe_id, input_sha256,
	                            input, output, error_class, latency_ms, input_tokens, output_tokens, cost_usd, cache_hit)
	      SELECT CASE WHEN g % 2 = 0 THEN 'judge_chunk' ELSE 'should_apply' END, 'v1', 'ff', 'alias', 'resolved',
	             CASE WHEN g <= 5 THEN $1 ELSE $2 END, lpad(to_hex(g), 64, '0'),
	             '{"prompt": "PROMPT-BODY"}'::jsonb,
	             CASE WHEN g = 8 THEN NULL ELSE '{"reply": "REPLY-BODY"}'::jsonb END,
	             CASE WHEN g = 8 THEN '503' END, 120, 100, 20, 0.00123400, false
	        FROM generate_series(1, 8) g`, auRecipe1, auRecipe2)
	exec(`INSERT INTO fn_calls (fn, prompt_version, prompt_sha256, model_alias, model_resolved, model_revision, input_sha256, input)
	      VALUES ('judge_chunk', 'v1', 'ff', repeat('m', 700), repeat('r', 700), repeat('v', 700), lpad('99', 64, '0'), '{}')`)

	// step_runs: one per status, args carrying a secret, the failed one 700-character text.
	exec(`INSERT INTO step_runs (step, mode, recipe_id, model, args, host, status, finished_at, total, done, counters,
	                             cost_usd, error, last_message)
	      VALUES ('decide', 'write',   $1, 'clef:27b', '--yes --api-key=hunter2 ARGS-SECRET', 'pod-1', 'running',   NULL, 100, 40,  '{"apply": 30}', 0.5, NULL, 'LASTMSG-MARKER deciding'),
	             ('decide', 'dry_run', $1, 'clef:27b', 'ARGS-SECRET', 'pod-1', 'done',      now(), 100, 100, '{}', 0.1, NULL, 'LASTMSG-MARKER done'),
	             ('scan',   'write',   $2, repeat('M', 700), 'ARGS-SECRET', repeat('H', 700), 'failed',    now(), NULL, NULL, '{}', NULL, 'ERRTEXT-MARKER ' || repeat('E', 700), repeat('L', 700)),
	             ('eval',   'write',   NULL, 'm',      'ARGS-SECRET', 'pod-2', 'cancelled', now(), 10, 3,   '{}', 0, 'ERRTEXT-MARKER cancelled', 'LASTMSG-MARKER bye')`,
		auRecipe1, auRecipe2)
	exec(`INSERT INTO step_runs (step, mode, status, finished_at) SELECT 'scan', 'dry_run', 'done', now() FROM generate_series(1, 4)`)

	// pipeline_events: a job's timeline (with a 700-character reason and a detail
	// blob) and two job-less availability events.
	one := 3
	for _, e := range []PipelineEvent{
		{JobID: feJob, FilePath: "/b/FE/01.m4b", Stage: "enqueue", Event: "finish", RunnerHost: HostGoMonitor},
		{JobID: feJob, FilePath: "/b/FE/01.m4b", Stage: "embed", Event: "start", RunnerHost: HostGoWorker, Model: "nomic-embed-text"},
		{JobID: feJob, FilePath: "/b/FE/01.m4b", Stage: "embed", Event: "skip", RunnerHost: strings.Repeat("h", 700),
			Model: strings.Repeat("m", 700), ModelVersion: strings.Repeat("v", 700)},
		{JobID: feJob, FilePath: "/b/FE/01.m4b", Stage: "embed", Event: "finish", RunnerHost: HostGoWorker, ItemCount: &one},
		{JobID: feJob, FilePath: "/b/FE/01.m4b", Stage: "eval", Event: "error", RunnerHost: HostGoWorker,
			Reason: "REASON-MARKER " + strings.Repeat("r", 700), Detail: map[string]any{"x": "DETAIL-BODY"}},
		{Stage: "runner_availability", Event: "state", RunnerHost: HostGoWorker, Reason: "REASON-MARKER busy"},
		{Stage: "heartbeat", Event: "state", RunnerHost: HostGoWorker},
	} {
		if err := d.AppendEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return auditFixture{d: d, url: dbURL, findings: ids}
}

// auditRows decodes a page's rows.
func auditRows(t *testing.T, p AuditPage) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(p.Rows, &rows); err != nil {
		t.Fatalf("rows are not a JSON array of objects: %v\n%s", err, p.Rows)
	}
	return rows
}

func rowID(t *testing.T, r map[string]any) int64 {
	t.Helper()
	f, ok := r["id"].(float64)
	if !ok {
		t.Fatalf("row has no numeric id: %v", r)
	}
	return int64(f)
}

// auditAll pages through a source with the given page size and returns every row,
// failing if a page repeats or skips an id, is not ascending, or has the wrong cursor.
func (f auditFixture) auditAll(t *testing.T, source string, pageSize int, filters map[string]string) []map[string]any {
	t.Helper()
	var all []map[string]any
	after := int64(0)
	for pages := 0; ; pages++ {
		if pages > 1000 {
			t.Fatal("paging does not terminate")
		}
		p, err := f.d.AuditRows(context.Background(), source, AuditAfter(after), pageSize, filters)
		if err != nil {
			t.Fatalf("AuditRows(%s, after %d): %v", source, after, err)
		}
		if p.Source != source {
			t.Fatalf("page source = %q, want %q", p.Source, source)
		}
		rows := auditRows(t, p)
		if len(rows) == 0 {
			if p.Next != nil || string(p.Rows) != "[]" {
				t.Fatalf("empty page: next = %v rows = %s, want null and []", p.Next, p.Rows)
			}
			return all
		}
		if len(rows) > pageSize {
			t.Fatalf("page of %d rows exceeds limit %d", len(rows), pageSize)
		}
		for _, r := range rows {
			if id := rowID(t, r); id <= after {
				t.Fatalf("row id %d is not after the cursor %d", id, after)
			} else {
				after = id
			}
		}
		if p.Next == nil || *p.Next != after {
			t.Fatalf("next = %v, want the last row's id %d", p.Next, after)
		}
		all = append(all, rows...)
	}
}

func (f auditFixture) ids(t *testing.T, sql string, args ...any) []int64 {
	t.Helper()
	rows, err := f.d.pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func idsOf(t *testing.T, rows []map[string]any) []int64 {
	t.Helper()
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = rowID(t, r)
	}
	return out
}

// TestIntegrationAuditPaging: for every source, paging by next with a small
// page reaches every row exactly once in ascending id order, whatever the page
// size, and the page past the end is empty with a null cursor.
func TestIntegrationAuditPaging(t *testing.T) {
	f := seedAudit(t)
	for _, src := range AuditSources() {
		t.Run(src.Name, func(t *testing.T) {
			want := f.ids(t, "SELECT id FROM "+src.Table+" ORDER BY id")
			if len(want) < 6 {
				t.Fatalf("only %d seeded rows", len(want))
			}
			for _, size := range []int{1, 4, 5, len(want), AuditMaxLimit} {
				got := idsOf(t, f.auditAll(t, src.Name, size, nil))
				if !slices.Equal(got, want) {
					t.Errorf("page size %d: ids = %v, want %v", size, got, want)
				}
			}

			// A cursor in the middle returns exactly the rows after it.
			mid := want[len(want)/2]
			p, err := f.d.AuditRows(context.Background(), src.Name, AuditAfter(mid), AuditMaxLimit, nil)
			if err != nil {
				t.Fatal(err)
			}
			var wantAfter []int64
			for _, id := range want {
				if id > mid {
					wantAfter = append(wantAfter, id)
				}
			}
			if got := idsOf(t, auditRows(t, p)); !slices.Equal(got, wantAfter) {
				t.Errorf("after %d: ids = %v, want %v", mid, got, wantAfter)
			}

			// limit bounds a page.
			p, err = f.d.AuditRows(context.Background(), src.Name, AuditAfter(0), 2, nil)
			if err != nil || len(auditRows(t, p)) != 2 || p.Next == nil || *p.Next != want[1] {
				t.Errorf("limit 2: %s next %v err %v", p.Rows, p.Next, err)
			}

			// Past the end.
			p, err = f.d.AuditRows(context.Background(), src.Name, AuditAfter(want[len(want)-1]), 10, nil)
			if err != nil || string(p.Rows) != "[]" || p.Next != nil {
				t.Errorf("past the end: rows %s next %v err %v", p.Rows, p.Next, err)
			}
		})
	}
}

// auditAllDesc pages through a source newest first — AuditNewest, then
// AuditBefore(next) — and returns every row, failing if a page repeats or skips
// an id, is not strictly descending below its cursor, or has the wrong next.
func (f auditFixture) auditAllDesc(t *testing.T, source string, pageSize int, filters map[string]string) []map[string]any {
	t.Helper()
	var all []map[string]any
	cur := AuditNewest()
	for pages := 0; ; pages++ {
		if pages > 1000 {
			t.Fatal("paging does not terminate")
		}
		p, err := f.d.AuditRows(context.Background(), source, cur, pageSize, filters)
		if err != nil {
			t.Fatalf("AuditRows(%s, %+v): %v", source, cur, err)
		}
		if p.Source != source {
			t.Fatalf("page source = %q, want %q", p.Source, source)
		}
		rows := auditRows(t, p)
		if len(rows) == 0 {
			if p.Next != nil || string(p.Rows) != "[]" {
				t.Fatalf("empty page: next = %v rows = %s, want null and []", p.Next, p.Rows)
			}
			return all
		}
		if len(rows) > pageSize {
			t.Fatalf("page of %d rows exceeds limit %d", len(rows), pageSize)
		}
		prev := cur.ID
		for _, r := range rows {
			id := rowID(t, r)
			if id >= prev {
				t.Fatalf("row id %d is not below %d: rows must descend, strictly under the cursor", id, prev)
			}
			prev = id
		}
		if p.Next == nil || *p.Next != prev {
			t.Fatalf("next = %v, want the last (lowest) row's id %d", p.Next, prev)
		}
		all = append(all, rows...)
		cur = AuditBefore(prev)
	}
}

// TestIntegrationAuditNewestFirst: for every source, the newest-first read
// starts at the highest ids, paging by next → before walks down to the lowest
// row exactly once whatever the page size and ends with an empty page, returns
// the very rows the ascending read does, and before bounds a page strictly.
func TestIntegrationAuditNewestFirst(t *testing.T) {
	f := seedAudit(t)
	ctx := context.Background()
	for _, src := range AuditSources() {
		t.Run(src.Name, func(t *testing.T) {
			asc := f.ids(t, "SELECT id FROM "+src.Table+" ORDER BY id")
			want := f.ids(t, "SELECT id FROM "+src.Table+" ORDER BY id DESC")
			if len(want) < 6 {
				t.Fatalf("only %d seeded rows", len(want))
			}
			for _, size := range []int{1, 4, 5, len(want), AuditMaxLimit} {
				if got := idsOf(t, f.auditAllDesc(t, src.Name, size, nil)); !slices.Equal(got, want) {
					t.Errorf("page size %d: ids = %v, want %v", size, got, want)
				}
			}

			// The first page is the newest rows, with a small limit.
			p, err := f.d.AuditRows(ctx, src.Name, AuditNewest(), 3, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := idsOf(t, auditRows(t, p)); !slices.Equal(got, want[:3]) || p.Next == nil || *p.Next != want[2] {
				t.Errorf("newest 3: ids %v next %v, want %v next %d", got, p.Next, want[:3], want[2])
			}

			// Same rows as the ascending read, same content, opposite order.
			ascRows := auditRows(t, mustPage(t, f, src.Name, nil))
			descRows := f.auditAllDesc(t, src.Name, AuditMaxLimit, nil)
			slices.Reverse(descRows)
			if !reflect.DeepEqual(ascRows, descRows) {
				t.Errorf("descending rows differ from the ascending ones:\nasc  %v\ndesc %v", ascRows, descRows)
			}

			// before is exclusive: a cursor in the middle returns exactly the rows below it.
			mid := asc[len(asc)/2]
			p, err = f.d.AuditRows(ctx, src.Name, AuditBefore(mid), AuditMaxLimit, nil)
			if err != nil {
				t.Fatal(err)
			}
			var below []int64
			for _, id := range want {
				if id < mid {
					below = append(below, id)
				}
			}
			if got := idsOf(t, auditRows(t, p)); !slices.Equal(got, below) {
				t.Errorf("before %d: ids = %v, want %v", mid, got, below)
			}

			// A cursor above the newest row is the newest row's page; the lowest
			// row's id, and zero, are past the end.
			p, err = f.d.AuditRows(ctx, src.Name, AuditBefore(want[0]+1000), 2, nil)
			if err != nil || !slices.Equal(idsOf(t, auditRows(t, p)), want[:2]) {
				t.Errorf("before above the newest: %s err %v", p.Rows, err)
			}
			for _, id := range []int64{asc[0], 0} {
				p, err = f.d.AuditRows(ctx, src.Name, AuditBefore(id), 10, nil)
				if err != nil || string(p.Rows) != "[]" || p.Next != nil {
					t.Errorf("before %d (nothing is older): rows %s next %v err %v", id, p.Rows, p.Next, err)
				}
			}
		})
	}
}

// TestIntegrationAuditNewestFirstFilters: the equality filters hold in the
// descending read, across the cursor, alone and combined.
func TestIntegrationAuditNewestFirstFilters(t *testing.T) {
	f := seedAudit(t)
	cases := []struct {
		source  string
		filters map[string]string
		where   string
		args    []any
	}{
		{"finding_events", map[string]string{"kind": "decision"}, "kind = 'decision'", nil},
		{"finding_events", map[string]string{"kind": "revoke"}, "kind = 'revoke'", nil},
		{"finding_events", map[string]string{"recipe_id": auRecipe1, "kind": "decision"}, "recipe_id = $1 AND kind = 'decision'", []any{auRecipe1}},
		{"finding_events", map[string]string{"transcript_id": feTranscript, "kind": "decision", "recipe_id": auRecipe2}, "transcript_id = $1::uuid AND kind = 'decision' AND recipe_id = $2", []any{feTranscript, auRecipe2}},
		{"judge_passes", map[string]string{"transcript_id": auTranscriptA}, "transcript_id = $1::uuid", []any{auTranscriptA}},
		{"fn_calls", map[string]string{"fn": "judge_chunk"}, "fn = 'judge_chunk'", nil},
		{"fn_calls", map[string]string{"fn": "judge_chunk", "recipe_id": auRecipe1}, "fn = 'judge_chunk' AND recipe_id = $1", []any{auRecipe1}},
		{"step_runs", map[string]string{"step": "scan", "status": "done"}, "step = 'scan' AND status = 'done'", nil},
		{"pipeline_events", map[string]string{"stage": "embed"}, "stage = 'embed'", nil},
		{"pipeline_events", map[string]string{"job_id": feJob}, "job_id = $1::uuid", []any{feJob}},
		{"fn_calls", map[string]string{"error_class": "nope"}, "error_class = 'nope'", nil},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/%v", c.source, c.filters), func(t *testing.T) {
			src, _ := LookupAuditSource(c.source)
			want := f.ids(t, "SELECT id FROM "+src.Table+" WHERE "+c.where+" ORDER BY id DESC", c.args...)
			for _, size := range []int{1, 2, AuditMaxLimit} {
				if got := idsOf(t, f.auditAllDesc(t, c.source, size, c.filters)); !slices.Equal(got, want) {
					t.Errorf("page size %d: ids = %v, want %v", size, got, want)
				}
			}
		})
	}
}

// TestIntegrationAuditNewestFirstEnds: paging down ends at the oldest row with
// a page whose next is its id, and the page below it is empty with a null
// cursor. A fresh table makes the ids 1..5 exactly.
func TestIntegrationAuditNewestFirstEnds(t *testing.T) {
	dbURL, d := migratedTestDB(t)
	ctx := context.Background()
	conn := connect(t, dbURL)
	if _, err := conn.Exec(ctx, `INSERT INTO step_runs (step, mode, status, finished_at) SELECT 'scan', 'dry_run', 'done', now() FROM generate_series(1, 5)`); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		cur      AuditCursor
		wantIDs  []int64
		wantNext int64 // 0 = null
	}{
		{AuditNewest(), []int64{5, 4}, 4},
		{AuditBefore(4), []int64{3, 2}, 2},
		{AuditBefore(2), []int64{1}, 1},
		{AuditBefore(1), []int64{}, 0},
		{AuditBefore(0), []int64{}, 0},
		{AuditBefore(6), []int64{5, 4}, 4},
	}
	for _, c := range steps {
		p, err := d.AuditRows(ctx, "step_runs", c.cur, 2, nil)
		if err != nil {
			t.Fatalf("%+v: %v", c.cur, err)
		}
		if got := idsOf(t, auditRows(t, p)); !slices.Equal(got, c.wantIDs) {
			t.Errorf("%+v: ids = %v, want %v", c.cur, got, c.wantIDs)
		}
		switch {
		case c.wantNext == 0 && (p.Next != nil || string(p.Rows) != "[]"):
			t.Errorf("%+v: next = %v rows = %s, want null and []", c.cur, p.Next, p.Rows)
		case c.wantNext != 0 && (p.Next == nil || *p.Next != c.wantNext):
			t.Errorf("%+v: next = %v, want %d", c.cur, p.Next, c.wantNext)
		}
	}
}

// TestIntegrationAuditNewestFirstEmptyTable: a newest-first read of an empty
// table is an empty page with a null cursor, for every source.
func TestIntegrationAuditNewestFirstEmptyTable(t *testing.T) {
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	for _, src := range AuditSources() {
		if n := countRows(t, conn, "SELECT count(*) FROM "+src.Table); n != 0 {
			t.Fatalf("a freshly migrated database has %d rows in %s; the test needs it empty", n, src.Table)
		}
		for _, cur := range []AuditCursor{AuditNewest(), AuditBefore(10), AuditBefore(0)} {
			p, err := d.AuditRows(context.Background(), src.Name, cur, 10, nil)
			if err != nil || string(p.Rows) != "[]" || p.Next != nil {
				t.Errorf("%s %+v: rows %s next %v err %v, want an empty page", src.Name, cur, p.Rows, p.Next, err)
			}
		}
	}
}

// TestIntegrationAuditFilters: each filter returns exactly the rows its SQL
// predicate selects, alone and combined.
func TestIntegrationAuditFilters(t *testing.T) {
	f := seedAudit(t)
	cases := []struct {
		source  string
		filters map[string]string
		where   string // the test's own predicate, over the same table
		args    []any
		count   int // -1 = only compare with the predicate
	}{
		{"finding_events", map[string]string{"kind": "revoke"}, "kind = 'revoke'", nil, 3},
		{"finding_events", map[string]string{"kind": "decision"}, "kind = 'decision'", nil, 20},
		{"finding_events", map[string]string{"kind": "transition"}, "kind = 'transition'", nil, -1},
		{"finding_events", map[string]string{"finding_id": "$F1"}, "finding_id = $1::uuid", []any{"$F1"}, 7},
		{"finding_events", map[string]string{"transcript_id": feTranscript}, "transcript_id = $1::uuid", []any{feTranscript}, -1},
		{"finding_events", map[string]string{"recipe_id": auRecipe2}, "recipe_id = $1", []any{auRecipe2}, 7},
		{"finding_events", map[string]string{"recipe_id": auRecipe1, "kind": "decision"}, "recipe_id = $1 AND kind = 'decision'", []any{auRecipe1}, 13},
		{"finding_events", map[string]string{"recipe_id": auRecipe1, "kind": "decision", "finding_id": "$F0", "transcript_id": feTranscript},
			"recipe_id = $1 AND kind = 'decision' AND finding_id = $2::uuid", []any{auRecipe1, "$F0"}, 13},
		{"finding_events", map[string]string{"kind": "nope"}, "kind = 'nope'", nil, 0},
		{"judge_passes", map[string]string{"transcript_id": auTranscriptA}, "transcript_id = $1::uuid", []any{auTranscriptA}, 3},
		{"judge_passes", map[string]string{"transcript_id": auTranscriptB}, "transcript_id = $1::uuid", []any{auTranscriptB}, 3},
		{"judge_passes", map[string]string{"recipe_id": auRecipe1}, "recipe_id = $1", []any{auRecipe1}, 3},
		{"judge_passes", map[string]string{"status": "failed"}, "status = 'failed'", nil, 1},
		{"judge_passes", map[string]string{"status": "complete", "recipe_id": auRecipe2}, "status = 'complete' AND recipe_id = $1", []any{auRecipe2}, 1},
		{"fn_calls", map[string]string{"fn": "judge_chunk"}, "fn = 'judge_chunk'", nil, 5},
		{"fn_calls", map[string]string{"fn": "should_apply"}, "fn = 'should_apply'", nil, 4},
		{"fn_calls", map[string]string{"recipe_id": auRecipe2}, "recipe_id = $1", []any{auRecipe2}, 3},
		{"fn_calls", map[string]string{"error_class": "503"}, "error_class = '503'", nil, 1},
		{"fn_calls", map[string]string{"fn": "judge_chunk", "recipe_id": auRecipe1}, "fn = 'judge_chunk' AND recipe_id = $1", []any{auRecipe1}, 2},
		{"step_runs", map[string]string{"step": "decide"}, "step = 'decide'", nil, 2},
		{"step_runs", map[string]string{"status": "running"}, "status = 'running'", nil, 1},
		{"step_runs", map[string]string{"recipe_id": auRecipe1}, "recipe_id = $1", []any{auRecipe1}, 2},
		{"step_runs", map[string]string{"step": "decide", "status": "done"}, "step = 'decide' AND status = 'done'", nil, 1},
		{"pipeline_events", map[string]string{"job_id": feJob}, "job_id = $1::uuid", []any{feJob}, 5},
		{"pipeline_events", map[string]string{"stage": "embed"}, "stage = 'embed'", nil, 3},
		{"pipeline_events", map[string]string{"event": "error"}, "event = 'error'", nil, 1},
		{"pipeline_events", map[string]string{"stage": "heartbeat", "event": "state"}, "stage = 'heartbeat' AND event = 'state'", nil, 1},
		{"pipeline_events", map[string]string{"job_id": feJob, "stage": "runner_availability"}, "job_id = $1::uuid AND stage = 'runner_availability'", []any{feJob}, 0},
	}
	sub := func(s string) string {
		switch s {
		case "$F0":
			return f.findings[0]
		case "$F1":
			return f.findings[1]
		}
		return s
	}
	for _, c := range cases {
		filters := map[string]string{}
		for k, v := range c.filters {
			filters[k] = sub(v)
		}
		args := make([]any, len(c.args))
		for i, a := range c.args {
			if s, ok := a.(string); ok {
				args[i] = sub(s)
			} else {
				args[i] = a
			}
		}
		name := fmt.Sprintf("%s/%v", c.source, c.filters)
		t.Run(name, func(t *testing.T) {
			src, _ := LookupAuditSource(c.source)
			want := f.ids(t, "SELECT id FROM "+src.Table+" WHERE "+c.where+" ORDER BY id", args...)
			if c.count >= 0 && len(want) != c.count {
				t.Fatalf("the test's own predicate matched %d rows, expected %d: fix the fixture", len(want), c.count)
			}
			// Small pages: the filter must hold across the cursor, not just on page one.
			got := idsOf(t, f.auditAll(t, c.source, 2, filters))
			if !slices.Equal(got, want) {
				t.Errorf("ids = %v, want %v", got, want)
			}
		})
	}
}

// TestIntegrationAuditColumns: a row carries exactly the registry's columns in
// registry order and nothing else, bodies and secrets never appear anywhere in a
// page, and values keep their stored types.
func TestIntegrationAuditColumns(t *testing.T) {
	f := seedAudit(t)
	// Nothing here may appear anywhere in a page: bodies, error text, command
	// lines, library paths, and the names of the columns that hold them.
	mustNot := map[string][]string{
		"finding_events":  nil,
		"judge_passes":    {"/b/AU/", `"file_path"`, `"error"`, "ERRTEXT-MARKER"},
		"fn_calls":        {"PROMPT-BODY", "REPLY-BODY", `"input"`, `"output"`},
		"step_runs":       {"ARGS-SECRET", "hunter2", `"args"`, `"error"`, `"last_message"`, "ERRTEXT-MARKER", "LASTMSG-MARKER"},
		"pipeline_events": {"DETAIL-BODY", `"detail"`, `"reason"`, `"file_path"`, "REASON-MARKER", "/b/FE/"},
	}
	for _, src := range AuditSources() {
		t.Run(src.Name, func(t *testing.T) {
			p, err := f.d.AuditRows(context.Background(), src.Name, AuditAfter(0), AuditMaxLimit, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range mustNot[src.Name] {
				if strings.Contains(string(p.Rows), bad) {
					t.Errorf("page contains %q", bad)
				}
			}
			// Keys of every row, in order.
			dec := json.NewDecoder(strings.NewReader(string(p.Rows)))
			var rows []json.RawMessage
			if err := dec.Decode(&rows); err != nil || len(rows) == 0 {
				t.Fatalf("rows: %v, %d", err, len(rows))
			}
			var want []string
			for _, c := range src.Columns {
				want = append(want, c.Name)
			}
			for i, raw := range rows {
				if got := jsonKeys(t, raw); !slices.Equal(got, want) {
					t.Fatalf("row %d keys = %v, want %v", i, got, want)
				}
			}
		})
	}

	// Types as stored: numbers, strings, nulls, embedded objects.
	fe := auditRows(t, mustPage(t, f, "finding_events", map[string]string{"kind": "decision", "recipe_id": auRecipe1}))
	first := fe[0]
	if _, ok := first["p"].(float64); !ok {
		t.Errorf("p = %#v, want a JSON number", first["p"])
	}
	if s, ok := first["finding_id"].(string); !ok || s != f.findings[0] {
		t.Errorf("finding_id = %#v, want the UUID string %s", first["finding_id"], f.findings[0])
	}
	if s, ok := first["created_at"].(string); !ok {
		t.Errorf("created_at = %#v, want a string", first["created_at"])
	} else if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		t.Errorf("created_at %q is not ISO-8601 with an offset: %v", s, err)
	}
	if v, present := first["revokes_event_id"]; !present || v != nil {
		t.Errorf("revokes_event_id = %#v, want null", v)
	}
	rv := auditRows(t, mustPage(t, f, "finding_events", map[string]string{"kind": "revoke"}))
	if _, ok := rv[0]["revokes_event_id"].(float64); !ok {
		t.Errorf("a revoke's revokes_event_id = %#v, want a number", rv[0]["revokes_event_id"])
	}

	jp := auditRows(t, mustPage(t, f, "judge_passes", map[string]string{"status": "complete", "recipe_id": auRecipe1}))
	if d, ok := jp[0]["dropped"].(map[string]any); !ok || d["empty"] != float64(2) {
		t.Errorf("dropped = %#v, want the embedded object {empty: 2}", jp[0]["dropped"])
	}
	if c, ok := jp[0]["cost_usd"].(float64); !ok || c != 0.001234 {
		t.Errorf("cost_usd = %#v, want the number 0.001234", jp[0]["cost_usd"])
	}

	sr := auditRows(t, mustPage(t, f, "step_runs", map[string]string{"status": "running"}))
	if c, ok := sr[0]["counters"].(map[string]any); !ok || c["apply"] != float64(30) {
		t.Errorf("counters = %#v, want the embedded object {apply: 30}", sr[0]["counters"])
	}
	if v, present := sr[0]["finished_at"]; !present || v != nil {
		t.Errorf("a running step's finished_at = %#v, want null", v)
	}

	fn := auditRows(t, mustPage(t, f, "fn_calls", map[string]string{"error_class": "503"}))
	if b, ok := fn[0]["cache_hit"].(bool); !ok || b {
		t.Errorf("cache_hit = %#v, want false", fn[0]["cache_hit"])
	}
}

func mustPage(t *testing.T, f auditFixture, source string, filters map[string]string) AuditPage {
	t.Helper()
	p, err := f.d.AuditRows(context.Background(), source, AuditAfter(0), AuditMaxLimit, filters)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// jsonKeys returns an object's keys in document order.
func jsonKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// TestIntegrationAuditClip: externally-sourced text (model, host and actor names,
// reason codes) is cut to 256 characters, and a value under the limit is
// returned untouched.
func TestIntegrationAuditClip(t *testing.T) {
	f := seedAudit(t)
	cases := []struct {
		source, column string
		filters        map[string]string
		short          string // a value in the fixture that is under the limit
	}{
		{"finding_events", "reason", map[string]string{"kind": "decision", "recipe_id": auRecipe1}, "rung0"},
		{"finding_events", "actor", map[string]string{"kind": "decision", "recipe_id": auRecipe2}, "jev:" + auRecipe2},
		{"fn_calls", "model_alias", nil, "alias"},
		{"fn_calls", "model_resolved", nil, "resolved"},
		{"fn_calls", "model_revision", nil, ""},
		{"step_runs", "model", nil, "clef:27b"},
		{"step_runs", "host", nil, "pod-1"},
		{"pipeline_events", "runner_host", nil, HostGoWorker},
		{"pipeline_events", "model", nil, "nomic-embed-text"},
		{"pipeline_events", "model_version", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.source+"."+c.column, func(t *testing.T) {
			var lens []int
			var shortSeen bool
			for _, r := range auditRows(t, mustPage(t, f, c.source, c.filters)) {
				if s, ok := r[c.column].(string); ok {
					lens = append(lens, len(s))
					shortSeen = shortSeen || s == c.short
				}
			}
			if !slices.Contains(lens, 256) {
				t.Errorf("no %s value was cut to 256 characters: lengths %v", c.column, lens)
			}
			if slices.ContainsFunc(lens, func(n int) bool { return n > 256 }) {
				t.Errorf("%s value longer than 256 characters: lengths %v", c.column, lens)
			}
			if c.short != "" && !shortSeen {
				t.Errorf("a short %s value was altered: want %q among the rows", c.column, c.short)
			}
		})
	}
	// The clip is a prefix: the stored value starts the served one.
	for _, r := range auditRows(t, mustPage(t, f, "step_runs", map[string]string{"status": "failed"})) {
		if s, _ := r["host"].(string); strings.Trim(s, "H") != "" {
			t.Errorf("clipped host is not a prefix of the stored one: %.20q", s)
		}
	}
}

// TestIntegrationAuditInjection: a hostile filter value is data. Text filters
// return nothing and error nothing, a value PostgreSQL text cannot hold (NUL,
// invalid UTF-8) and any non-UUID for a UUID filter are refused before the
// database, and the tables are all still there.
func TestIntegrationAuditInjection(t *testing.T) {
	f := seedAudit(t)
	ctx := context.Background()
	before := f.ids(t, "SELECT id FROM fn_calls ORDER BY id")
	for _, src := range AuditSources() {
		for _, flt := range src.Filters {
			for _, hostile := range []string{auHostile, "' OR '1'='1", `\'; --`, "%", "a\x00b", "\xff", "\x00"} {
				p, err := f.d.AuditRows(ctx, src.Name, AuditAfter(0), 10, map[string]string{flt.Name: hostile})
				invalidText := strings.ContainsRune(hostile, 0) || hostile == "\xff"
				switch {
				case flt.UUID || invalidText:
					if !errors.Is(err, ErrAuditBadFilterValue) {
						t.Errorf("%s.%s=%q: err = %v, want ErrAuditBadFilterValue", src.Name, flt.Name, hostile, err)
					}
				case err != nil || string(p.Rows) != "[]" || p.Next != nil:
					t.Errorf("%s.%s=%q: rows %s next %v err %v, want an empty page", src.Name, flt.Name, hostile, p.Rows, p.Next, err)
				}
			}
		}
	}
	if after := f.ids(t, "SELECT id FROM fn_calls ORDER BY id"); !slices.Equal(before, after) {
		t.Errorf("fn_calls changed: %v -> %v", before, after)
	}
	// Unlisted names never reach SQL at all.
	for name := range map[string]string{"input": "", "fn = 1 OR 1": "", "id": ""} {
		if _, err := f.d.AuditRows(ctx, "fn_calls", AuditAfter(0), 10, map[string]string{name: "x"}); !errors.Is(err, ErrAuditUnknownFilter) {
			t.Errorf("filter %q: err = %v", name, err)
		}
	}
}

// TestIntegrationAuditSchemaGuard: the registry plus the documented exclusions
// are exactly the table's columns, and every served `text` column is either cut
// (auditClippedColumns) or a fixed-vocabulary one (auditFixedText). A migration
// that adds a column to one of these tables, or widens one, fails here until
// someone decides whether the endpoint serves it and how it is bounded.
func TestIntegrationAuditSchemaGuard(t *testing.T) {
	f := seedAudit(t)
	for _, src := range AuditSources() {
		t.Run(src.Name, func(t *testing.T) {
			rows, err := f.d.pool.Query(context.Background(),
				`SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1`, src.Table)
			if err != nil {
				t.Fatal(err)
			}
			type colType struct{ name, typ string }
			cols, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (colType, error) {
				var c colType
				return c, r.Scan(&c.name, &c.typ)
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(cols) == 0 {
				t.Fatalf("table %q does not exist", src.Table)
			}
			var actual, known []string
			types := map[string]string{}
			for _, c := range cols {
				actual = append(actual, c.name)
				types[c.name] = c.typ
			}
			for _, c := range src.Columns {
				known = append(known, c.Name)
			}
			known = append(known, auditExcluded[src.Name]...)
			slices.Sort(actual)
			slices.Sort(known)
			if !slices.Equal(actual, known) {
				t.Errorf("table %s columns != registry + documented exclusions\n  table:    %v\n  registry: %v\n"+
					"A new column must be added to the registry (internal/db/audit.go) or to auditExcluded, and to CONTRACT §2.12.2.",
					src.Table, actual, known)
			}

			for _, c := range src.Columns {
				if types[c.Name] != "text" && types[c.Name] != "character varying" {
					continue
				}
				switch {
				case c.Clip > 0 && slices.Contains(auditClippedColumns[src.Name], c.Name):
				case c.Clip == 0 && slices.Contains(auditFixedText[src.Name], c.Name):
				default:
					t.Errorf("served text column %s.%s is neither cut to %d characters (auditClippedColumns) nor a documented fixed-vocabulary one (auditFixedText)",
						src.Table, c.Name, auditClip)
				}
			}
			for _, name := range auditFixedText[src.Name] {
				if types[name] != "text" {
					t.Errorf("auditFixedText lists %s.%s, which is %q, not text", src.Table, name, types[name])
				}
			}
		})
	}
}

// TestIntegrationAuditSessionSettings: the read runs with JIT off, UTC and a 5 s
// statement timeout, all scoped to its transaction.
func TestIntegrationAuditSessionSettings(t *testing.T) {
	f := seedAudit(t)
	ctx := context.Background()
	show := func(q pgx.Tx, name string) string {
		t.Helper()
		var v string
		if err := q.QueryRow(ctx, "SHOW "+name).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	tx, err := f.d.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, auditSessionSQL, "5000"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"statement_timeout": "5s", "jit": "off", "TimeZone": "UTC"} {
		if got := show(tx, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// is_local: nothing leaks to the connection's next user.
	if got := f.pooledShow(t, "statement_timeout"); got == "5s" {
		t.Errorf("statement_timeout stayed %q after the transaction", got)
	}
}

func (f auditFixture) pooledShow(t *testing.T, name string) string {
	t.Helper()
	var v string
	if err := f.d.pool.QueryRow(context.Background(), "SHOW "+name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestIntegrationAuditTimestampsAreUTC: whatever time zone the database session
// is in, timestamps come out with a +00:00 offset, as the contract says.
func TestIntegrationAuditTimestampsAreUTC(t *testing.T) {
	f := seedAudit(t)
	ctx := context.Background()

	u, err := neturl.Parse(f.url)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("timezone", "Asia/Tokyo")
	u.RawQuery = q.Encode()
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tokyo := &DB{pool: pool, log: testLog()}

	// Control: this session really renders +09:00 when left alone.
	var raw string
	if err := pool.QueryRow(ctx, `SELECT row_to_json(t)::text FROM (SELECT created_at FROM finding_events ORDER BY id LIMIT 1) t`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "+09:00") {
		t.Fatalf("control query rendered %s; the test session is not in Asia/Tokyo", raw)
	}

	for _, c := range []struct{ source, column string }{
		{"finding_events", "created_at"}, {"judge_passes", "started_at"}, {"fn_calls", "created_at"},
		{"step_runs", "heartbeat_at"}, {"pipeline_events", "created_at"},
	} {
		p, err := tokyo.AuditRows(ctx, c.source, AuditAfter(0), 5, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range auditRows(t, p) {
			s, _ := r[c.column].(string)
			if !strings.HasSuffix(s, "+00:00") {
				t.Errorf("%s.%s = %q, want a +00:00 offset", c.source, c.column, s)
			}
		}
	}
}

// TestIntegrationAuditDeadline: a read that outlives its context is cut short and
// reported as ErrAuditTimeout, a client that cancels is not, and the pool is
// usable afterwards.
func TestIntegrationAuditDeadline(t *testing.T) {
	f := seedAudit(t)
	slow := AuditSource{
		Name: "slow", Table: "(SELECT 1 AS id, pg_sleep(30)) slow",
		Columns: []AuditColumn{plainCol("id")},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := readAudit(ctx, f.d.pool, slow, AuditAfter(0), 10, nil)
	if !errors.Is(err, ErrAuditTimeout) {
		t.Fatalf("err = %v, want ErrAuditTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s to give up", elapsed)
	}

	// The client going away is a cancellation, not a timeout.
	cctx, ccancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, ccancel)
	_, err = readAudit(cctx, f.d.pool, slow, AuditAfter(0), 10, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrAuditTimeout) {
		t.Errorf("canceled read: err = %v, want context.Canceled and not ErrAuditTimeout", err)
	}

	if p, err := f.d.AuditRows(context.Background(), "step_runs", AuditAfter(0), 1, nil); err != nil || len(auditRows(t, p)) != 1 {
		t.Errorf("pool unusable after a timeout: %v", err)
	}
}

// TestIntegrationAuditStatementTimeout: the server's own statement_timeout, the
// one the 5 s setting arms, raises SQLSTATE 57014 and auditError reports it as
// ErrAuditTimeout — against a real server, not a hand-built error.
func TestIntegrationAuditStatementTimeout(t *testing.T) {
	f := seedAudit(t)
	ctx := context.Background()
	tx, err := f.d.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, auditSessionSQL, "5"); err != nil { // 5 ms
		t.Fatal(err)
	}
	var v any
	err = tx.QueryRow(ctx, `SELECT pg_sleep(1)`).Scan(&v)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgQueryCanceled {
		t.Fatalf("pg_sleep under a 5 ms statement_timeout: err = %v, want SQLSTATE 57014", err)
	}
	if got := auditError("test", err); !errors.Is(got, ErrAuditTimeout) {
		t.Errorf("auditError(57014) = %v, want ErrAuditTimeout", got)
	}
}
