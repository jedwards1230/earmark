package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/log"
)

// auditCall is one AuditRows call as the handler made it.
type auditCall struct {
	source  string
	cur     db.AuditCursor
	limit   int
	filters map[string]string
}

// auditFakeDB records the calls the handler makes and answers from page. With
// total > 0 it behaves like a table of ids 1..total: the rows after (ascending)
// or before (descending) the cursor, at most limit of them, next = the last
// row's id. With gate set, a call blocks until the gate closes or its context
// ends, announcing itself on entered first.
type auditFakeDB struct {
	SimpleMockDB
	mu    sync.Mutex
	calls []auditCall
	// timeLeft[i] is what remained of call i's context deadline when it was
	// made; zero means the context had none.
	timeLeft []time.Duration
	page     db.AuditPage
	err      error
	total    int64
	gate     chan struct{}
	entered  chan struct{}
}

func (f *auditFakeDB) AuditRows(ctx context.Context, source string, cur db.AuditCursor, limit int, filters map[string]string) (db.AuditPage, error) {
	var left time.Duration
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	f.mu.Lock()
	f.calls = append(f.calls, auditCall{source, cur, limit, filters})
	f.timeLeft = append(f.timeLeft, left)
	f.mu.Unlock()
	if f.gate != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
		select {
		case <-f.gate:
		case <-ctx.Done():
			return db.AuditPage{}, fmt.Errorf("audit %s: %w", source, ctx.Err())
		}
	}
	if f.err != nil {
		return db.AuditPage{}, f.err
	}
	if f.total > 0 {
		var rows []string
		var last int64
		if cur.Desc {
			for id := min(cur.ID-1, f.total); id >= 1 && len(rows) < limit; id-- {
				rows = append(rows, fmt.Sprintf(`{"id":%d}`, id))
				last = id
			}
		} else {
			for id := cur.ID + 1; id <= f.total && len(rows) < limit; id++ {
				rows = append(rows, fmt.Sprintf(`{"id":%d}`, id))
				last = id
			}
		}
		p := db.AuditPage{Source: source, Rows: json.RawMessage("[" + strings.Join(rows, ",") + "]")}
		if last > 0 {
			p.Next = &last
		}
		return p, nil
	}
	p := f.page
	p.Source = source
	return p, nil
}

func auditGET(h http.Handler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

// wantDeadline fails unless the handler gave the database call a context that
// ends within the audit query timeout — deleting context.WithTimeout from the
// handler must not go unnoticed.
func wantDeadline(t *testing.T, left time.Duration) {
	t.Helper()
	if left <= 0 || left > auditQueryTimeout {
		t.Errorf("the database call had %s left on its context, want 0 < left <= %s", left, auditQueryTimeout)
	}
}

func auditErrorBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error == "" {
		t.Fatalf("body %q is not an error envelope: %v", w.Body.String(), err)
	}
	return e.Error
}

func TestAPIAuditOK(t *testing.T) {
	for _, src := range db.AuditSources() {
		t.Run(src.Name, func(t *testing.T) {
			next := int64(42)
			f := &auditFakeDB{page: db.AuditPage{Rows: json.RawMessage(`[{"id":41},{"id":42}]`), Next: &next}}
			w := auditGET(buildTestMux(f), "/api/v1/audit/"+src.Name)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q", ct)
			}
			var got struct {
				Source string            `json:"source"`
				Rows   []json.RawMessage `json:"rows"`
				Next   *int64            `json:"next"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Source != src.Name || len(got.Rows) != 2 || got.Next == nil || *got.Next != 42 {
				t.Errorf("response = %+v", got)
			}
			// Defaults reach the database layer; no filters.
			if len(f.calls) != 1 || f.calls[0].source != src.Name || f.calls[0].cur != db.AuditAfter(0) ||
				f.calls[0].limit != db.AuditDefaultLimit || len(f.calls[0].filters) != 0 {
				t.Errorf("calls = %+v", f.calls)
			}
			wantDeadline(t, f.timeLeft[0])
		})
	}
}

// Reads are open: a configured control token is not asked for.
func TestAPIAuditNeedsNoToken(t *testing.T) {
	w := auditGET(muxWithToken(&auditFakeDB{}, testToken), "/api/v1/audit/step_runs")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 without a token", w.Code)
	}
}

func TestAPIAuditParamsReachTheDB(t *testing.T) {
	const id = "e530a6f7-c96a-4cd5-a910-f809a99e0449"
	hostile := "x'; DROP TABLE fn_calls;--"
	cases := []struct {
		name   string
		target string
		want   auditCall
	}{
		{"after and limit", "/api/v1/audit/fn_calls?after=117999&limit=7",
			auditCall{"fn_calls", db.AuditAfter(117999), 7, nil}},
		{"limit at the cap", "/api/v1/audit/fn_calls?limit=500",
			auditCall{"fn_calls", db.AuditAfter(0), 500, nil}},
		{"limit of one", "/api/v1/audit/fn_calls?limit=1",
			auditCall{"fn_calls", db.AuditAfter(0), 1, nil}},
		{"largest cursor", "/api/v1/audit/fn_calls?after=9223372036854775807",
			auditCall{"fn_calls", db.AuditAfter(9223372036854775807), 100, nil}},
		{"explicit zero cursor", "/api/v1/audit/fn_calls?after=0",
			auditCall{"fn_calls", db.AuditAfter(0), 100, nil}},
		{"one filter", "/api/v1/audit/step_runs?status=running",
			auditCall{"step_runs", db.AuditAfter(0), 100, map[string]string{"status": "running"}}},
		{"uuid filter", "/api/v1/audit/finding_events?finding_id=" + id,
			auditCall{"finding_events", db.AuditAfter(0), 100, map[string]string{"finding_id": id}}},
		{"every filter at once", "/api/v1/audit/finding_events?kind=decision&recipe_id=ab&transcript_id=" + id + "&finding_id=" + id + "&after=5&limit=3",
			auditCall{"finding_events", db.AuditAfter(5), 3, map[string]string{"kind": "decision", "recipe_id": "ab", "transcript_id": id, "finding_id": id}}},
		{"hostile text value is passed through as data", "/api/v1/audit/fn_calls?fn=" + url.QueryEscape(hostile),
			auditCall{"fn_calls", db.AuditAfter(0), 100, map[string]string{"fn": hostile}}},

		// order=asc is the default spelled out.
		{"explicit ascending", "/api/v1/audit/fn_calls?order=asc",
			auditCall{"fn_calls", db.AuditAfter(0), 100, nil}},
		{"explicit ascending with after", "/api/v1/audit/fn_calls?order=asc&after=40&limit=5",
			auditCall{"fn_calls", db.AuditAfter(40), 5, nil}},
		// order=desc: newest first, from the newest row unless before bounds it.
		{"descending from the newest row", "/api/v1/audit/fn_calls?order=desc",
			auditCall{"fn_calls", db.AuditNewest(), 100, nil}},
		{"descending with a limit", "/api/v1/audit/fn_calls?order=desc&limit=3",
			auditCall{"fn_calls", db.AuditNewest(), 3, nil}},
		{"descending before an id", "/api/v1/audit/fn_calls?order=desc&before=117999&limit=7",
			auditCall{"fn_calls", db.AuditBefore(117999), 7, nil}},
		{"before is read whichever side of order it is on", "/api/v1/audit/fn_calls?before=12&order=desc",
			auditCall{"fn_calls", db.AuditBefore(12), 100, nil}},
		{"descending before zero", "/api/v1/audit/fn_calls?order=desc&before=0",
			auditCall{"fn_calls", db.AuditBefore(0), 100, nil}},
		{"descending before the largest id", "/api/v1/audit/fn_calls?order=desc&before=9223372036854775807",
			auditCall{"fn_calls", db.AuditBefore(9223372036854775807), 100, nil}},
		{"descending with a filter", "/api/v1/audit/step_runs?order=desc&status=running",
			auditCall{"step_runs", db.AuditNewest(), 100, map[string]string{"status": "running"}}},
		{"descending with every filter", "/api/v1/audit/finding_events?order=desc&before=50&kind=decision&recipe_id=ab&transcript_id=" + id + "&finding_id=" + id + "&limit=3",
			auditCall{"finding_events", db.AuditBefore(50), 3, map[string]string{"kind": "decision", "recipe_id": "ab", "transcript_id": id, "finding_id": id}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &auditFakeDB{}
			w := auditGET(buildTestMux(f), c.target)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
			}
			if len(f.calls) != 1 {
				t.Fatalf("%d calls", len(f.calls))
			}
			got := f.calls[0]
			wantDeadline(t, f.timeLeft[0])
			if got.source != c.want.source || got.cur != c.want.cur || got.limit != c.want.limit ||
				fmt.Sprint(got.filters) != fmt.Sprint(c.want.filters) {
				t.Errorf("call = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestAPIAuditNotFound(t *testing.T) {
	for _, target := range []string{
		"/api/v1/audit/users",
		"/api/v1/audit/transcript_chunks",
		"/api/v1/audit/FN_CALLS",
		"/api/v1/audit/fn_calls%3Bdrop",
		"/api/v1/audit/fn_calls%20",
		"/api/v1/audit/..%2Ffn_calls",
		"/api/v1/audit/users?limit=abc", // the source is checked first
	} {
		f := &auditFakeDB{}
		w := auditGET(buildTestMux(f), target)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, w.Code)
			continue
		}
		if msg := auditErrorBody(t, w); !strings.Contains(msg, "finding_events, judge_passes, fn_calls, step_runs, pipeline_events") {
			t.Errorf("GET %s: %q does not list the valid sources", target, msg)
		}
		if len(f.calls) != 0 {
			t.Errorf("GET %s reached the database: %+v", target, f.calls)
		}
	}
	// With no source at all the route is not the audit handler.
	for _, target := range []string{"/api/v1/audit", "/api/v1/audit/"} {
		f := &auditFakeDB{}
		if w := auditGET(buildTestMux(f), target); w.Code != http.StatusNotFound || len(f.calls) != 0 {
			t.Errorf("GET %s = %d, %d calls, want 404 and none", target, w.Code, len(f.calls))
		}
	}
}

func TestAPIAuditBadRequest(t *testing.T) {
	const id = "e530a6f7-c96a-4cd5-a910-f809a99e0449"
	cases := []struct {
		name, target, wantMsg string
	}{
		{"unknown filter", "/api/v1/audit/step_runs?fn=judge_chunk", `unknown filter "fn" for source "step_runs"; filters: step, status, recipe_id`},
		{"unknown parameter", "/api/v1/audit/step_runs?sort=desc", `unknown filter "sort"`},
		{"a served column that is not a filter", "/api/v1/audit/finding_events?actor=system", `unknown filter "actor"`},
		{"the id column", "/api/v1/audit/fn_calls?id=5", `unknown filter "id"`},
		{"a secret column as filter", "/api/v1/audit/fn_calls?input=x", `unknown filter "input"`},
		{"negative after", "/api/v1/audit/fn_calls?after=-1", "after must be an integer >= 0"},
		{"non-numeric after", "/api/v1/audit/fn_calls?after=abc", "after must be an integer >= 0"},
		{"float after", "/api/v1/audit/fn_calls?after=1.5", "after must be an integer >= 0"},
		{"overflowing after", "/api/v1/audit/fn_calls?after=9223372036854775808", "after must be an integer >= 0"},
		{"after with a plus sign", "/api/v1/audit/fn_calls?after=%2B5", "after must be an integer >= 0 in plain digits"},
		{"after with leading zeros", "/api/v1/audit/fn_calls?after=007", "after must be an integer >= 0 in plain digits"},
		{"after negative zero", "/api/v1/audit/fn_calls?after=-0", "after must be an integer >= 0 in plain digits"},
		{"after in hex", "/api/v1/audit/fn_calls?after=0x10", "after must be an integer >= 0 in plain digits"},
		{"after in exponent form", "/api/v1/audit/fn_calls?after=1e3", "after must be an integer >= 0 in plain digits"},
		{"after with a digit separator", "/api/v1/audit/fn_calls?after=1_000", "after must be an integer >= 0 in plain digits"},
		{"after with a leading space", "/api/v1/audit/fn_calls?after=%205", "after must be an integer >= 0 in plain digits"},
		{"after with a trailing space", "/api/v1/audit/fn_calls?after=5%20", "after must be an integer >= 0 in plain digits"},
		{"after in non-ASCII digits", "/api/v1/audit/fn_calls?after=%D9%A3", "after must be an integer >= 0 in plain digits"},
		{"limit with a plus sign", "/api/v1/audit/fn_calls?limit=%2B5", "limit must be 1..500 in plain digits"},
		{"limit with leading zeros", "/api/v1/audit/fn_calls?limit=007", "limit must be 1..500 in plain digits"},
		{"limit with a leading zero at the cap", "/api/v1/audit/fn_calls?limit=0500", "limit must be 1..500 in plain digits"},
		{"limit in exponent form", "/api/v1/audit/fn_calls?limit=1e2", "limit must be 1..500 in plain digits"},
		{"limit as a decimal", "/api/v1/audit/fn_calls?limit=5.0", "limit must be 1..500 in plain digits"},
		{"limit with a leading space", "/api/v1/audit/fn_calls?limit=%205", "limit must be 1..500 in plain digits"},
		{"semicolon in a filter value", "/api/v1/audit/fn_calls?fn=x;y", "malformed query string"},
		{"bad percent escape before a good pair", "/api/v1/audit/fn_calls?after=%zz&limit=10", "malformed query string"},
		{"semicolon as a separator", "/api/v1/audit/fn_calls?limit=5;after=3", "malformed query string"},
		{"NUL in a text filter", "/api/v1/audit/finding_events?kind=%00", `"kind" must be valid text`},
		{"NUL inside a text filter", "/api/v1/audit/fn_calls?fn=a%00b", `"fn" must be valid text`},
		{"invalid UTF-8 in a text filter", "/api/v1/audit/fn_calls?fn=%ff", `"fn" must be valid text`},
		{"truncated UTF-8 in a text filter", "/api/v1/audit/step_runs?step=x%C3", `"step" must be valid text`},
		{"NUL in a UUID filter", "/api/v1/audit/pipeline_events?job_id=%00", `"job_id" must be valid text`},
		{"limit zero", "/api/v1/audit/fn_calls?limit=0", "limit must be 1..500"},
		{"negative limit", "/api/v1/audit/fn_calls?limit=-1", "limit must be 1..500"},
		{"limit over the cap", "/api/v1/audit/fn_calls?limit=501", "limit must be 1..500"},
		{"non-numeric limit", "/api/v1/audit/fn_calls?limit=abc", "limit must be 1..500"},
		{"overflowing limit", "/api/v1/audit/fn_calls?limit=99999999999999999999", "limit must be 1..500"},
		{"repeated limit", "/api/v1/audit/fn_calls?limit=5&limit=6", `parameter "limit" must be given once`},
		{"repeated after", "/api/v1/audit/fn_calls?after=1&after=2", `parameter "after" must be given once`},
		{"repeated filter", "/api/v1/audit/step_runs?status=running&status=done", `parameter "status" must be given once`},
		{"empty after", "/api/v1/audit/fn_calls?after=", `parameter "after" is empty`},
		{"empty limit", "/api/v1/audit/fn_calls?limit=", `parameter "limit" is empty`},
		{"empty filter", "/api/v1/audit/step_runs?status=", `parameter "status" is empty`},
		{"key without value", "/api/v1/audit/step_runs?status", `parameter "status" is empty`},
		// order and before.
		{"unknown order", "/api/v1/audit/fn_calls?order=newest", "order must be asc or desc"},
		{"order in capitals", "/api/v1/audit/fn_calls?order=DESC", "order must be asc or desc"},
		{"order with a trailing space", "/api/v1/audit/fn_calls?order=desc%20", "order must be asc or desc"},
		{"order given a number", "/api/v1/audit/fn_calls?order=1", "order must be asc or desc"},
		{"empty order", "/api/v1/audit/fn_calls?order=", `parameter "order" is empty`},
		{"repeated order", "/api/v1/audit/fn_calls?order=asc&order=desc", `parameter "order" must be given once`},
		{"descending together with after", "/api/v1/audit/fn_calls?order=desc&after=5", "order=desc pages with before, not after"},
		{"descending together with after zero", "/api/v1/audit/fn_calls?order=desc&after=0", "order=desc pages with before, not after"},
		{"descending with both cursors", "/api/v1/audit/fn_calls?order=desc&after=1&before=9", "order=desc pages with before, not after"},
		{"before without an order", "/api/v1/audit/fn_calls?before=5", "before needs order=desc"},
		{"before with an explicit ascending order", "/api/v1/audit/fn_calls?order=asc&before=5", "before needs order=desc"},
		{"before with after", "/api/v1/audit/fn_calls?after=1&before=9", "before needs order=desc"},
		{"empty before", "/api/v1/audit/fn_calls?order=desc&before=", `parameter "before" is empty`},
		{"repeated before", "/api/v1/audit/fn_calls?order=desc&before=1&before=2", `parameter "before" must be given once`},
		{"negative before", "/api/v1/audit/fn_calls?order=desc&before=-1", "before must be an integer >= 0 in plain digits"},
		{"non-numeric before", "/api/v1/audit/fn_calls?order=desc&before=abc", "before must be an integer >= 0 in plain digits"},
		{"float before", "/api/v1/audit/fn_calls?order=desc&before=1.5", "before must be an integer >= 0 in plain digits"},
		{"overflowing before", "/api/v1/audit/fn_calls?order=desc&before=9223372036854775808", "before must be an integer >= 0 in plain digits"},
		{"before with a plus sign", "/api/v1/audit/fn_calls?order=desc&before=%2B5", "before must be an integer >= 0 in plain digits"},
		{"before with leading zeros", "/api/v1/audit/fn_calls?order=desc&before=007", "before must be an integer >= 0 in plain digits"},
		{"before negative zero", "/api/v1/audit/fn_calls?order=desc&before=-0", "before must be an integer >= 0 in plain digits"},
		{"before in exponent form", "/api/v1/audit/fn_calls?order=desc&before=1e3", "before must be an integer >= 0 in plain digits"},
		{"before with a leading space", "/api/v1/audit/fn_calls?order=desc&before=%205", "before must be an integer >= 0 in plain digits"},
		{"bad before without an order", "/api/v1/audit/fn_calls?before=abc", "before must be an integer >= 0 in plain digits"},
		{"descending with a bad limit", "/api/v1/audit/fn_calls?order=desc&limit=501", "limit must be 1..500"},
		{"descending with an unknown filter", "/api/v1/audit/step_runs?order=desc&fn=judge_chunk", `unknown filter "fn"`},
		{"non-UUID on a UUID filter", "/api/v1/audit/finding_events?finding_id=123", `"finding_id" must be a UUID`},
		{"SQL on a UUID filter", "/api/v1/audit/pipeline_events?job_id=" + url.QueryEscape("x' OR '1'='1"), `"job_id" must be a UUID`},
		{"UUID filter given a UUID with braces", "/api/v1/audit/judge_passes?transcript_id=%7B" + id + "%7D", `"transcript_id" must be a UUID`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &auditFakeDB{}
			w := auditGET(buildTestMux(f), c.target)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
			}
			if msg := auditErrorBody(t, w); !strings.Contains(msg, c.wantMsg) {
				t.Errorf("error %q does not contain %q", msg, c.wantMsg)
			}
			if len(f.calls) != 0 {
				t.Errorf("a rejected request reached the database: %+v", f.calls)
			}
		})
	}
}

// A 400 never echoes a value the caller sent (a parameter NAME is echoed: that is
// how the message names the valid choices).
func TestAPIAuditErrorsDoNotEchoValues(t *testing.T) {
	for _, target := range []string{
		"/api/v1/audit/finding_events?finding_id=SECRETVALUE",
		"/api/v1/audit/fn_calls?limit=SECRETVALUE",
		"/api/v1/audit/fn_calls?after=SECRETVALUE",
		"/api/v1/audit/fn_calls?order=SECRETVALUE",
		"/api/v1/audit/fn_calls?order=desc&before=SECRETVALUE",
		"/api/v1/audit/fn_calls?before=SECRETVALUE",
		"/api/v1/audit/fn_calls?order=SECRETVALUE&after=1",
		"/api/v1/audit/fn_calls?fn=SECRETVALUE%ff",
		"/api/v1/audit/fn_calls?fn=SECRETVALUE%00",
		"/api/v1/audit/fn_calls?fn=SECRETVALUE;x",
	} {
		w := auditGET(buildTestMux(&auditFakeDB{}), target)
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "SECRETVALUE") {
			t.Errorf("GET %s = %d %s", target, w.Code, w.Body.String())
		}
	}
}

// The cursor protocol: next goes back as after, every row is seen exactly once,
// and the page past the end is empty with a null cursor.
func TestAPIAuditCursorPaging(t *testing.T) {
	f := &auditFakeDB{total: 25}
	h := buildTestMux(f)

	var seen []int64
	after := int64(0)
	for page := 1; page <= 10; page++ {
		w := auditGET(h, fmt.Sprintf("/api/v1/audit/fn_calls?limit=10&after=%d", after))
		if w.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, w.Code, w.Body.String())
		}
		var got struct {
			Rows []struct {
				ID int64 `json:"id"`
			} `json:"rows"`
			Next *int64 `json:"next"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Rows) == 0 {
			if got.Next != nil {
				t.Errorf("empty page has next = %d, want null", *got.Next)
			}
			break
		}
		for _, r := range got.Rows {
			seen = append(seen, r.ID)
		}
		if got.Next == nil || *got.Next != got.Rows[len(got.Rows)-1].ID {
			t.Fatalf("page %d: next = %v, want the last row's id %d", page, got.Next, got.Rows[len(got.Rows)-1].ID)
		}
		after = *got.Next
	}
	if len(seen) != 25 {
		t.Fatalf("saw %d rows, want 25: %v", len(seen), seen)
	}
	for i, id := range seen {
		if id != int64(i+1) {
			t.Fatalf("row %d has id %d; want ascending 1..25: %v", i, id, seen)
		}
	}
}

// auditPageOf decodes a 200 audit response into its row ids and cursor.
func auditPageOf(t *testing.T, w *httptest.ResponseRecorder) (ids []int64, next *int64) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var got struct {
		Rows []struct {
			ID int64 `json:"id"`
		} `json:"rows"`
		Next *int64 `json:"next"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	ids = []int64{}
	for _, r := range got.Rows {
		ids = append(ids, r.ID)
	}
	return ids, got.Next
}

// Newest first: the first page is the highest ids, descending; next is the last
// (lowest) row's id and goes back as before; every row is seen exactly once;
// the page ending at id 1 gives next=1, and before=1 is empty with a null
// cursor.
func TestAPIAuditCursorPagingDesc(t *testing.T) {
	h := buildTestMux(&auditFakeDB{total: 25})

	var seen []int64
	target := "/api/v1/audit/fn_calls?order=desc&limit=10"
	var nexts []int64
	for page := 1; ; page++ {
		if page > 10 {
			t.Fatal("paging does not terminate")
		}
		ids, next := auditPageOf(t, auditGET(h, target))
		if len(ids) == 0 {
			if next != nil {
				t.Errorf("empty page has next = %d, want null", *next)
			}
			break
		}
		if next == nil || *next != ids[len(ids)-1] {
			t.Fatalf("page %d: next = %v, want the last row's id %d", page, next, ids[len(ids)-1])
		}
		nexts = append(nexts, *next)
		seen = append(seen, ids...)
		target = fmt.Sprintf("/api/v1/audit/fn_calls?order=desc&limit=10&before=%d", *next)
	}
	if len(seen) != 25 {
		t.Fatalf("saw %d rows, want 25: %v", len(seen), seen)
	}
	for i, id := range seen {
		if want := int64(25 - i); id != want {
			t.Fatalf("row %d has id %d; want descending 25..1: %v", i, id, seen)
		}
	}
	if want := []int64{16, 6, 1}; fmt.Sprint(nexts) != fmt.Sprint(want) {
		t.Errorf("cursors = %v, want %v: the last page ends at id 1 and next is 1", nexts, want)
	}
}

// What a descending read returns on the wire for the edge cursors.
func TestAPIAuditDescEdges(t *testing.T) {
	cases := []struct {
		name, query string
		wantIDs     []int64
		wantNext    int64 // 0 = null
	}{
		{"newest rows first", "order=desc&limit=3", []int64{25, 24, 23}, 23},
		{"before an id", "order=desc&before=10&limit=2", []int64{9, 8}, 8},
		{"before is exclusive", "order=desc&before=25&limit=1", []int64{24}, 24},
		{"before above the newest row", "order=desc&before=9999&limit=2", []int64{25, 24}, 24},
		{"before the oldest row", "order=desc&before=2", []int64{1}, 1},
		{"before 1: nothing is older", "order=desc&before=1", []int64{}, 0},
		{"before 0 matches nothing", "order=desc&before=0", []int64{}, 0},
		{"a short page", "order=desc&before=4", []int64{3, 2, 1}, 1},
		{"explicit ascending", "order=asc&limit=2&after=3", []int64{4, 5}, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ids, next := auditPageOf(t, auditGET(buildTestMux(&auditFakeDB{total: 25}), "/api/v1/audit/fn_calls?"+c.query))
			if fmt.Sprint(ids) != fmt.Sprint(c.wantIDs) {
				t.Errorf("ids = %v, want %v", ids, c.wantIDs)
			}
			switch {
			case c.wantNext == 0 && next != nil:
				t.Errorf("next = %d, want null", *next)
			case c.wantNext != 0 && (next == nil || *next != c.wantNext):
				t.Errorf("next = %v, want %d", next, c.wantNext)
			}
		})
	}
}

// An empty page is `"rows":[]` and `"next":null` on the wire — never null rows,
// whether the layer below returned an empty array or nothing at all.
func TestAPIAuditEmptyPage(t *testing.T) {
	for name, page := range map[string]db.AuditPage{
		"empty array": {Rows: json.RawMessage("[]")},
		"nil rows":    {},
	} {
		t.Run(name, func(t *testing.T) {
			w := auditGET(buildTestMux(&auditFakeDB{page: page}), "/api/v1/audit/judge_passes?status=failed")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			if string(raw["rows"]) != "[]" || string(raw["next"]) != "null" || string(raw["source"]) != `"judge_passes"` {
				t.Errorf("body = %s", w.Body.String())
			}
		})
	}
}

func TestAPIAuditTimeout(t *testing.T) {
	for name, err := range map[string]error{
		"bare":    db.ErrAuditTimeout,
		"wrapped": fmt.Errorf("audit fn_calls: %w (canceling statement due to statement timeout)", db.ErrAuditTimeout),
	} {
		t.Run(name, func(t *testing.T) {
			w := auditGET(buildTestMux(&auditFakeDB{err: err}), "/api/v1/audit/fn_calls?fn=judge_chunk")
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			if msg := auditErrorBody(t, w); msg != "audit query timed out; add a filter or lower limit" {
				t.Errorf("error = %q", msg)
			}
		})
	}
}

// Anything else is a 500 that says nothing about the database.
func TestAPIAuditInternalErrorDoesNotLeak(t *testing.T) {
	secret := errors.New(`audit fn_calls: ERROR: relation "fn_calls" does not exist (SQLSTATE 42P01) host=earmark-pg-rw password=hunter2`)
	w := auditGET(buildTestMux(&auditFakeDB{err: secret}), "/api/v1/audit/fn_calls")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if msg := auditErrorBody(t, w); msg != "internal error" {
		t.Errorf("error = %q, want the generic message", msg)
	}
	for _, leak := range []string{"hunter2", "earmark-pg-rw", "fn_calls", "SQLSTATE"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("500 body leaks %q: %s", leak, w.Body.String())
		}
	}
}

// Only GET is routed: the audit trail has no write surface.
func TestAPIAuditIsReadOnly(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		f := &auditFakeDB{}
		w := httptest.NewRecorder()
		buildTestMux(f).ServeHTTP(w, httptest.NewRequest(m, "/api/v1/audit/fn_calls", strings.NewReader(`{}`)))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", m, w.Code)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s reached the database", m)
		}
	}
}

// The demo database answers every known source with an empty page and rejects
// the rest, like the real one.
func TestAPIAuditDemo(t *testing.T) {
	h := NewMCPServer(demoDB{}, &config.Config{}).buildMux()
	for _, src := range db.AuditSources() {
		w := auditGET(h, "/api/v1/audit/"+src.Name)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"rows":[]`) || !strings.Contains(w.Body.String(), `"next":null`) {
			t.Errorf("demo %s = %d %s", src.Name, w.Code, w.Body.String())
		}
	}
	if w := auditGET(h, "/api/v1/audit/users"); w.Code != http.StatusNotFound {
		t.Errorf("demo unknown source = %d, want 404", w.Code)
	}
}

// A client that goes away mid-query gets no response and no error log: the
// database layer reports context.Canceled, the handler stays quiet.
func TestAPIAuditClientGoneIsQuiet(t *testing.T) {
	logged := func(buf *bytes.Buffer, srv *MCPServer) {
		srv.logger = log.Logger{Logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	}

	t.Run("canceled error", func(t *testing.T) {
		var buf bytes.Buffer
		srv := NewMCPServer(&auditFakeDB{err: fmt.Errorf("audit fn_calls: %w", context.Canceled)}, &config.Config{})
		logged(&buf, srv)
		w := auditGET(srv.buildMux(), "/api/v1/audit/fn_calls")
		if w.Body.Len() != 0 || w.Code >= 400 {
			t.Errorf("wrote %d %q for a canceled read, want nothing", w.Code, w.Body.String())
		}
		if strings.Contains(buf.String(), "level=ERROR") || strings.Contains(buf.String(), "level=WARN") {
			t.Errorf("a client disconnect was logged above debug:\n%s", buf.String())
		}
	})

	t.Run("request context canceled while the query runs", func(t *testing.T) {
		var buf bytes.Buffer
		f := &auditFakeDB{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
		srv := NewMCPServer(f, &config.Config{})
		logged(&buf, srv)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/audit/fn_calls", nil).WithContext(ctx))
		}()
		<-f.entered
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the handler did not return after its request was canceled")
		}
		if w.Body.Len() != 0 || w.Code >= 400 {
			t.Errorf("wrote %d %q for a canceled request, want nothing", w.Code, w.Body.String())
		}
		if strings.Contains(buf.String(), "level=ERROR") || strings.Contains(buf.String(), "level=WARN") {
			t.Errorf("a client disconnect was logged above debug:\n%s", buf.String())
		}
	})
}

// At most auditMaxInflight audit reads run at once; the next is a 429 with
// Retry-After, nothing queues, requests that fail validation are not counted,
// and the slots come back.
func TestAPIAuditConcurrencyCap(t *testing.T) {
	f := &auditFakeDB{gate: make(chan struct{}), entered: make(chan struct{}, 2*auditMaxInflight)}
	h := buildTestMux(f) // one server, one counter

	var wg sync.WaitGroup
	codes := make(chan int, auditMaxInflight)
	for i := 0; i < auditMaxInflight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- auditGET(h, "/api/v1/audit/fn_calls").Code
		}()
	}
	for i := 0; i < auditMaxInflight; i++ {
		select {
		case <-f.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d reads reached the database", i, auditMaxInflight)
		}
	}

	w := auditGET(h, "/api/v1/audit/finding_events?kind=revoke")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("read #%d = %d, want 429", auditMaxInflight+1, w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if msg := auditErrorBody(t, w); msg != "too many concurrent audit reads; retry shortly" {
		t.Errorf("429 body = %q", msg)
	}
	if n := len(f.calls); n != auditMaxInflight {
		t.Errorf("%d database calls, want %d: the rejected read must not reach the database", n, auditMaxInflight)
	}

	// While saturated a bad request is still the client's error, not a 429.
	if w := auditGET(h, "/api/v1/audit/fn_calls?limit=0"); w.Code != http.StatusBadRequest {
		t.Errorf("invalid request while saturated = %d, want 400", w.Code)
	}
	if w := auditGET(h, "/api/v1/audit/users"); w.Code != http.StatusNotFound {
		t.Errorf("unknown source while saturated = %d, want 404", w.Code)
	}
	// Other routes are untouched by the cap.
	if w := auditGET(h, "/api/v1/status"); w.Code != http.StatusOK {
		t.Errorf("GET /api/v1/status while saturated = %d, want 200", w.Code)
	}

	close(f.gate)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Errorf("a held read finished with %d, want 200", c)
		}
	}
	for i := 0; i < 2*auditMaxInflight; i++ {
		if w := auditGET(h, "/api/v1/audit/fn_calls"); w.Code != http.StatusOK {
			t.Fatalf("read %d after the others finished = %d, want 200: a slot was not released", i+1, w.Code)
		}
	}
}

// A slot is released however the request ends: success, any error, a canceled
// client, and a request rejected before the database.
func TestAPIAuditSlotReleasedOnEveryPath(t *testing.T) {
	cases := map[string]struct {
		err    error
		target string
	}{
		"ok":          {nil, "/api/v1/audit/fn_calls"},
		"timeout":     {db.ErrAuditTimeout, "/api/v1/audit/fn_calls"},
		"internal":    {errors.New("boom"), "/api/v1/audit/fn_calls"},
		"canceled":    {context.Canceled, "/api/v1/audit/fn_calls"},
		"bad request": {nil, "/api/v1/audit/fn_calls?limit=0"},
		"not found":   {nil, "/api/v1/audit/users"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := buildTestMux(&auditFakeDB{err: c.err})
			for i := 0; i < 2*auditMaxInflight; i++ {
				if w := auditGET(h, c.target); w.Code == http.StatusTooManyRequests {
					t.Fatalf("request %d was refused: a slot leaked", i+1)
				}
			}
			if w := auditGET(h, "/api/v1/audit/fn_calls?fn=judge_chunk"); w.Code == http.StatusTooManyRequests {
				t.Fatal("the slots were never given back")
			}
		})
	}
}

// deadlineWriter records SetWriteDeadline, which http.ResponseController uses
// when the writer supports it (a real connection does; a recorder does not).
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	err      error
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.deadline = t
	return d.err
}

// The response has auditWriteTimeout to be written; a writer that cannot take a
// deadline (or refuses it) does not stop the read being served.
func TestAPIAuditWriteDeadline(t *testing.T) {
	h := buildTestMux(&auditFakeDB{})

	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	before := time.Now()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/audit/fn_calls", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.deadline.Sub(before); got < auditWriteTimeout-time.Second || got > auditWriteTimeout+time.Second {
		t.Errorf("write deadline is %s from the request start, want about %s", got, auditWriteTimeout)
	}

	refusing := &deadlineWriter{ResponseRecorder: httptest.NewRecorder(), err: errors.New("deadline refused")}
	h.ServeHTTP(refusing, httptest.NewRequest(http.MethodGet, "/api/v1/audit/fn_calls", nil))
	if refusing.Code != http.StatusOK {
		t.Errorf("a refused write deadline stopped the read: %d", refusing.Code)
	}
	// httptest.ResponseRecorder has no SetWriteDeadline (http.ErrNotSupported): TestAPIAuditOK covers it.
}
