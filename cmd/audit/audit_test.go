package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/db"
)

const (
	testUUID   = "21018bec-6af3-42d8-a8d2-73cc0e638fa4"
	testRecipe = "f1c32cfe5f8401528d84874ef74e7e853710f4438da2bbd573113e8376caef7a"
)

// fakeReader records the request and answers with a canned page.
type fakeReader struct {
	page db.AuditPage
	err  error

	source  string
	cur     db.AuditCursor
	limit   int
	filters map[string]string
	calls   int
}

func (f *fakeReader) AuditRows(_ context.Context, source string, cur db.AuditCursor, limit int, filters map[string]string) (db.AuditPage, error) {
	f.calls++
	f.source, f.cur, f.limit, f.filters = source, cur, limit, filters
	return f.page, f.err
}

func page(source string, next int64, rows ...string) db.AuditPage {
	p := db.AuditPage{Source: source, Rows: json.RawMessage("[" + strings.Join(rows, ",") + "]")}
	if len(rows) > 0 {
		p.Next = &next
	}
	return p
}

func opts(t *testing.T, source string, f Flags) Options {
	t.Helper()
	if f.Limit == 0 {
		f.Limit = DefaultLimit
	}
	o, err := ParseOptions(source, f)
	require.NoError(t, err)
	return o
}

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		flags    Flags
		wantCur  db.AuditCursor
		wantLim  int
		wantFilt map[string]string
		wantCols []string
		wantErr  string
	}{
		{name: "defaults read the newest rows, bounded", source: "step_runs", flags: Flags{Limit: DefaultLimit},
			wantCur: db.AuditNewest(), wantLim: DefaultLimit},
		{name: "before pages newest first", source: "fn_calls", flags: Flags{Limit: 5, Before: 900, HasBefore: true},
			wantCur: db.AuditBefore(900), wantLim: 5},
		{name: "before 0 is a valid (empty) cursor", source: "fn_calls", flags: Flags{Limit: 5, Before: 0, HasBefore: true},
			wantCur: db.AuditBefore(0), wantLim: 5},
		{name: "after pages oldest first", source: "judge_passes", flags: Flags{Limit: 7, After: 120, HasAfter: true},
			wantCur: db.AuditAfter(120), wantLim: 7},
		{name: "asc starts at the oldest row", source: "pipeline_events", flags: Flags{Limit: 7, Asc: true},
			wantCur: db.AuditAfter(0), wantLim: 7},
		{name: "asc with after is after", source: "pipeline_events", flags: Flags{Limit: 7, Asc: true, After: 3, HasAfter: true},
			wantCur: db.AuditAfter(3), wantLim: 7},
		{name: "max limit", source: "step_runs", flags: Flags{Limit: db.AuditMaxLimit},
			wantCur: db.AuditNewest(), wantLim: db.AuditMaxLimit},
		{name: "filters, a UUID one included", source: "finding_events",
			flags:   Flags{Limit: 3, Filters: []string{"transcript_id=" + testUUID, "kind=decision", "recipe_id=a=b"}},
			wantCur: db.AuditNewest(), wantLim: 3,
			wantFilt: map[string]string{"transcript_id": testUUID, "kind": "decision", "recipe_id": "a=b"}},
		{name: "columns keep the order asked", source: "step_runs", flags: Flags{Limit: 3, Columns: []string{"status", "id"}},
			wantCur: db.AuditNewest(), wantLim: 3, wantCols: []string{"status", "id"}},

		{name: "unknown source lists the valid ones", source: "transcripts", flags: Flags{Limit: 1},
			wantErr: "finding_events, judge_passes, fn_calls, step_runs, pipeline_events"},
		{name: "limit 0", source: "step_runs", flags: Flags{Limit: 0}, wantErr: "--limit 0: want 1..500"},
		{name: "negative limit", source: "step_runs", flags: Flags{Limit: -1}, wantErr: "--limit -1: want 1..500"},
		{name: "limit over the cap", source: "step_runs", flags: Flags{Limit: db.AuditMaxLimit + 1}, wantErr: "want 1..500"},
		{name: "after with before", source: "step_runs",
			flags: Flags{Limit: 1, After: 1, HasAfter: true, Before: 2, HasBefore: true}, wantErr: "give one"},
		{name: "asc with before", source: "step_runs",
			flags: Flags{Limit: 1, Asc: true, Before: 2, HasBefore: true}, wantErr: "drop --asc"},
		{name: "negative after", source: "step_runs", flags: Flags{Limit: 1, After: -1, HasAfter: true}, wantErr: "--after -1"},
		{name: "negative before", source: "step_runs", flags: Flags{Limit: 1, Before: -4, HasBefore: true}, wantErr: "--before -4"},
		{name: "filter without a value separator", source: "step_runs", flags: Flags{Limit: 1, Filters: []string{"status"}},
			wantErr: "want name=value; filters of step_runs: step, status, recipe_id"},
		{name: "filter without a name", source: "step_runs", flags: Flags{Limit: 1, Filters: []string{"=done"}}, wantErr: "want name=value"},
		{name: "filter with an empty value", source: "step_runs", flags: Flags{Limit: 1, Filters: []string{"status="}}, wantErr: "is empty"},
		{name: "unknown filter", source: "step_runs", flags: Flags{Limit: 1, Filters: []string{"host=x"}},
			wantErr: "step, status, recipe_id"},
		{name: "a filter of another source", source: "step_runs", flags: Flags{Limit: 1, Filters: []string{"transcript_id=" + testUUID}},
			wantErr: "unknown filter"},
		{name: "UUID filter must be a UUID", source: "finding_events", flags: Flags{Limit: 1, Filters: []string{"finding_id=12"}},
			wantErr: "must be a UUID"},
		{name: "filter given twice", source: "step_runs", flags: Flags{Limit: 1, Filters: []string{"status=done", "status=failed"}},
			wantErr: "given twice"},
		{name: "unknown column", source: "step_runs", flags: Flags{Limit: 1, Columns: []string{"args"}},
			wantErr: `"args" is not a column of step_runs`},
		{name: "column given twice", source: "step_runs", flags: Flags{Limit: 1, Columns: []string{"id", "id"}}, wantErr: "given twice"},
		{name: "columns do not apply to json", source: "step_runs", flags: Flags{Limit: 1, JSON: true, Columns: []string{"id"}},
			wantErr: "--json prints every column"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := ParseOptions(tt.source, tt.flags)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.source, o.Source.Name)
			assert.Equal(t, tt.wantCur, o.Cursor)
			assert.Equal(t, tt.wantLim, o.Limit)
			assert.Equal(t, tt.wantFilt, o.Filters)
			assert.Equal(t, tt.wantCols, o.Columns)
		})
	}
}

// The error for a filter value never echoes a bad value back, like the API's.
func TestParseOptionsDoesNotEchoBadFilterValues(t *testing.T) {
	_, err := ParseOptions("finding_events", Flags{Limit: 1, Filters: []string{"finding_id=hunter2"}})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}

func TestSourceArg(t *testing.T) {
	assert.NoError(t, sourceArg(nil, []string{"step_runs"}))
	for _, args := range [][]string{nil, {"a", "b"}} {
		err := sourceArg(nil, args)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sources: finding_events, judge_passes, fn_calls, step_runs, pipeline_events")
	}
}

// The help text is built from the registry, so a source or filter added there
// shows up without editing this package.
func TestLongHelpListsTheRegistry(t *testing.T) {
	help := longHelp()
	for _, s := range db.AuditSources() {
		assert.Contains(t, help, s.Name)
		assert.Contains(t, help, strings.Join(s.FilterNames(), ", "))
	}
}

func TestRunPassesTheRequestThrough(t *testing.T) {
	f := &fakeReader{page: page("fn_calls", 40, `{"id":41}`)}
	o := opts(t, "fn_calls", Flags{Limit: 9, Before: 50, HasBefore: true, Filters: []string{"fn=judge_chunk", "error_class=timeout"}})
	var out bytes.Buffer
	require.NoError(t, Run(context.Background(), &out, f, o))
	assert.Equal(t, 1, f.calls)
	assert.Equal(t, "fn_calls", f.source)
	assert.Equal(t, db.AuditBefore(50), f.cur)
	assert.Equal(t, 9, f.limit)
	assert.Equal(t, map[string]string{"fn": "judge_chunk", "error_class": "timeout"}, f.filters)
}

func TestRunJSONIsThePageAsTheAPIServesIt(t *testing.T) {
	f := &fakeReader{page: page("step_runs", 377,
		`{"id":378,"step":"eval_backfill","recipe_id":null,"counters":{"judged": 3},"cost_usd":0}`,
		`{"id":377,"step":"decide","recipe_id":"`+testRecipe+`","counters":{},"cost_usd":0.5}`)}
	var out bytes.Buffer
	require.NoError(t, Run(context.Background(), &out, f, opts(t, "step_runs", Flags{JSON: true})))

	assert.True(t, strings.HasSuffix(out.String(), "\n"))
	var got struct {
		Source string            `json:"source"`
		Rows   []json.RawMessage `json:"rows"`
		Next   *int64            `json:"next"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, "step_runs", got.Source)
	require.Len(t, got.Rows, 2)
	require.NotNil(t, got.Next)
	assert.EqualValues(t, 377, *got.Next)
	// Stdout is the page alone: one JSON document, nothing before or after it.
	dec := json.NewDecoder(&out)
	var v any
	require.NoError(t, dec.Decode(&v))
	assert.False(t, dec.More())
}

func TestRunJSONEmptyPage(t *testing.T) {
	f := &fakeReader{page: db.AuditPage{Source: "judge_passes"}} // nil Rows, nil Next
	var out bytes.Buffer
	require.NoError(t, Run(context.Background(), &out, f, opts(t, "judge_passes", Flags{JSON: true})))
	assert.JSONEq(t, `{"source":"judge_passes","rows":[],"next":null}`, out.String())
}

func TestRunTextEmptyPage(t *testing.T) {
	f := &fakeReader{page: db.AuditPage{Source: "judge_passes", Rows: json.RawMessage("[]")}}
	var out bytes.Buffer
	require.NoError(t, Run(context.Background(), &out, f, opts(t, "judge_passes", Flags{})))
	assert.Equal(t, "no judge_passes rows\n", out.String())
}

// The header is the registry's column list in the registry's order, for every
// source: the CLI keeps no column list of its own.
func TestRunTextHeaderIsTheRegistryColumns(t *testing.T) {
	for _, s := range db.AuditSources() {
		t.Run(s.Name, func(t *testing.T) {
			row := map[string]any{}
			var want []string
			for _, c := range s.Columns {
				row[c.Name] = 1
				want = append(want, strings.ToUpper(c.Name))
			}
			raw, err := json.Marshal(row)
			require.NoError(t, err)
			f := &fakeReader{page: page(s.Name, 1, string(raw))}
			var out bytes.Buffer
			require.NoError(t, Run(context.Background(), &out, f, opts(t, s.Name, Flags{})))
			lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
			require.Len(t, lines, 2, "a header and the row")
			assert.Equal(t, want, strings.Fields(lines[0]))
			assert.Equal(t, strings.Repeat("1 ", len(want)-1)+"1", strings.Join(strings.Fields(lines[1]), " "))
		})
	}
}

func TestRunTextCells(t *testing.T) {
	row := `{"id":8770,"step":"eval","recipe_id":"` + testRecipe + `","model":"` + strings.Repeat("m", 60) + `",` +
		`"host":"pod\u001b[31m-1\tx","total":null,"done":0,"counters":{"judged": 3,  "kept": 1},"ok":true}`
	f := &fakeReader{page: page("step_runs", 8770, row)}

	t.Run("default view abbreviates", func(t *testing.T) {
		var out bytes.Buffer
		require.NoError(t, Run(context.Background(), &out, f, Options{Source: mustSource(t, "step_runs"), Limit: 20, Cursor: db.AuditNewest(),
			Columns: nil}))
		text := out.String()
		assert.Contains(t, text, testRecipe[:digestShown]+"…")
		assert.NotContains(t, text, testRecipe)
		assert.Contains(t, text, strings.Repeat("m", cellMax-1)+"…")
		assert.NotContains(t, text, strings.Repeat("m", cellMax))
		assert.Contains(t, text, `{"judged":3,"kept":1}`)
		assert.NotContains(t, text, "\x1b", "control characters are not printed")
		assert.Contains(t, text, "pod?[31m-1?x")
	})

	t.Run("named columns print whole, in the order asked", func(t *testing.T) {
		var out bytes.Buffer
		o := opts(t, "step_runs", Flags{Columns: []string{"model", "id", "recipe_id", "total"}})
		require.NoError(t, Run(context.Background(), &out, f, o))
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		require.Len(t, lines, 2)
		assert.Equal(t, []string{"MODEL", "ID", "RECIPE_ID", "TOTAL"}, strings.Fields(lines[0]))
		assert.Equal(t, []string{strings.Repeat("m", 60), "8770", testRecipe, "-"}, strings.Fields(lines[1]))
	})
}

func mustSource(t *testing.T, name string) db.AuditSource {
	t.Helper()
	s, err := db.LookupAuditSource(name)
	require.NoError(t, err)
	return s
}

func TestRunTextNextPageHint(t *testing.T) {
	rows := []string{`{"id":9}`, `{"id":8}`}
	tests := []struct {
		name  string
		flags Flags
		page  db.AuditPage
		want  string // "" = no hint
	}{
		{name: "full descending page", flags: Flags{Limit: 2}, page: page("step_runs", 8, rows...),
			want: "2 rows · next page: --before 8"},
		{name: "full ascending page", flags: Flags{Limit: 2, Asc: true}, page: page("step_runs", 9, rows...),
			want: "2 rows · next page: --after 9"},
		{name: "short page is the end", flags: Flags{Limit: 3}, page: page("step_runs", 8, rows...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, Run(context.Background(), &out, &fakeReader{page: tt.page}, opts(t, "step_runs", tt.flags)))
			if tt.want == "" {
				assert.NotContains(t, out.String(), "next page")
				return
			}
			assert.Contains(t, out.String(), tt.want)
		})
	}
}

func TestRunErrors(t *testing.T) {
	t.Run("timeout says how to narrow the read", func(t *testing.T) {
		f := &fakeReader{err: fmt.Errorf("audit fn_calls: %w (boom)", db.ErrAuditTimeout)}
		err := Run(context.Background(), &bytes.Buffer{}, f, opts(t, "fn_calls", Flags{}))
		require.ErrorIs(t, err, db.ErrAuditTimeout)
		assert.Contains(t, err.Error(), "add a --filter or lower --limit")
	})
	t.Run("other errors pass through and print nothing", func(t *testing.T) {
		boom := errors.New("connection refused")
		var out bytes.Buffer
		err := Run(context.Background(), &out, &fakeReader{err: boom}, opts(t, "fn_calls", Flags{}))
		require.ErrorIs(t, err, boom)
		assert.Empty(t, out.String())
	})
}

func TestCell(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		whole bool
		want  string
	}{
		{"null", `null`, false, "-"},
		{"missing", ``, false, "-"},
		{"number", `12.5`, false, "12.5"},
		{"bool", `true`, false, "true"},
		{"string", `"complete"`, false, "complete"},
		{"uuid stays whole", `"` + testUUID + `"`, false, testUUID},
		{"digest cut", `"` + testRecipe + `"`, false, testRecipe[:digestShown] + "…"},
		{"digest whole when named", `"` + testRecipe + `"`, true, testRecipe},
		{"not a digest: 64 chars but not hex", `"` + strings.Repeat("z", 64) + `"`, true, strings.Repeat("z", 64)},
		{"object compacted", `{"a": 1, "b": {"c": 2}}`, false, `{"a":1,"b":{"c":2}}`},
		{"timestamp to whole seconds", `"2026-10-24T14:17:01.620081+00:00"`, false, "2026-10-24T14:17:01Z"},
		{"timestamp without a fraction", `"2026-10-24T14:17:01+00:00"`, false, "2026-10-24T14:17:01Z"},
		{"timestamp whole when named", `"2026-10-24T14:17:01.620081+00:00"`, true, "2026-10-24T14:17:01.620081+00:00"},
		{"other offsets are not rewritten", `"2026-10-24T14:17:01.5+02:00"`, false, "2026-10-24T14:17:01.5+02:00"},
		{"escape sequence", `"\u001b[2J"`, true, "?[2J"},
		{"newline", `"a\nb"`, true, "a?b"},
		{"bidi override", "\"a\u202eb\"", true, "a?b"},
		{"clipped by characters not bytes", `"` + strings.Repeat("é", 60) + `"`, false, strings.Repeat("é", cellMax-1) + "…"},
		{"exactly the width", `"` + strings.Repeat("x", cellMax) + `"`, false, strings.Repeat("x", cellMax)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cell(json.RawMessage(tt.raw), tt.whole))
		})
	}
}
