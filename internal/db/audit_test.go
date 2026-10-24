package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v5"
)

var wantAuditSources = []string{"finding_events", "judge_passes", "fn_calls", "step_runs", "pipeline_events"}

// auditExcluded is, per source, what the allowlist deliberately leaves out
// (CONTRACT §2.12.2). The integration schema guard checks the same sets against
// information_schema; here they must simply never be served.
var auditExcluded = map[string][]string{
	"judge_passes":    {"file_path", "error"},
	"fn_calls":        {"input", "output"},
	"step_runs":       {"args", "error", "last_message"},
	"pipeline_events": {"file_path", "reason", "detail"},
}

// auditClippedColumns are the externally-sourced text columns served, per source:
// names of models, hosts and actors, and reason codes. Each is cut to
// auditClip (256) characters so what a model, a host or an upstream says cannot
// make a page large.
var auditClippedColumns = map[string][]string{
	"finding_events":  {"reason", "actor"},
	"fn_calls":        {"model_alias", "model_resolved", "model_revision"},
	"step_runs":       {"model", "host"},
	"pipeline_events": {"runner_host", "model", "model_version"},
}

// auditFixedText are the text columns served WITHOUT a clip, per source. Each is
// an id, a hash, a closed-vocabulary token (a CHECK constraint, or the step's
// own vocabulary) or a name fixed in code, so its length is bounded by what
// writes it, not by anything external. The integration schema guard fails if a
// served `text` column is on neither this list nor auditClippedColumns.
var auditFixedText = map[string][]string{
	"finding_events":  {"kind", "from_state", "to_state", "outcome", "recipe_id", "evidence", "chunk_text_sha256", "issue_type"},
	"judge_passes":    {"recipe_id", "status", "book_context_sha256"},
	"fn_calls":        {"fn", "prompt_version", "prompt_sha256", "recipe_id", "input_sha256", "error_class"},
	"step_runs":       {"step", "mode", "recipe_id", "status"},
	"pipeline_events": {"stage", "event"},
}

var identPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// forbiddenAuditColumns are names of secret-bearing or large columns that must
// never be served: prompt/reply bodies, command lines, blobs, embeddings, text.
var forbiddenAuditColumns = []string{
	"input", "output", "args", "detail", "details", "prompt", "response", "reply", "embedding",
	"raw_text", "source_text", "text", "segments", "body", "payload", "content", "token", "key",
}

// forbiddenAuditFragments are substrings that mark a secret-bearing column
// (token_count, input_tokens, prompt_sha256 … are fine; these are not).
var forbiddenAuditFragments = []string{"secret", "password", "passwd", "credential", "api_key", "apikey", "bearer"}

func TestAuditRegistry(t *testing.T) {
	sources := AuditSources()
	var names []string
	for _, s := range sources {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, wantAuditSources) {
		t.Fatalf("sources = %v, want %v", names, wantAuditSources)
	}

	for _, s := range sources {
		t.Run(s.Name, func(t *testing.T) {
			if s.Name != s.Table || !identPattern.MatchString(s.Table) {
				t.Errorf("name %q / table %q: want one lowercase identifier", s.Name, s.Table)
			}
			if len(s.Columns) == 0 || s.Columns[0].Name != "id" || s.Columns[0].Clip != 0 {
				t.Fatalf("Columns[0] = %+v, want the plain id column", s.Columns)
			}

			cols := map[string]AuditColumn{}
			for _, c := range s.Columns {
				if !identPattern.MatchString(c.Name) {
					t.Errorf("column %q is not a plain identifier", c.Name)
				}
				if _, dup := cols[c.Name]; dup {
					t.Errorf("column %q listed twice", c.Name)
				}
				cols[c.Name] = c
				if slices.Contains(forbiddenAuditColumns, c.Name) {
					t.Errorf("column %q is secret-bearing or large and must not be served", c.Name)
				}
				for _, frag := range forbiddenAuditFragments {
					if strings.Contains(c.Name, frag) {
						t.Errorf("column %q looks secret-bearing (%q)", c.Name, frag)
					}
				}
				if c.Clip != 0 && c.Clip != 256 {
					t.Errorf("column %q clip = %d, want 0 or 256", c.Name, c.Clip)
				}
			}
			for _, name := range auditExcluded[s.Name] {
				if _, ok := cols[name]; ok {
					t.Errorf("excluded column %q is served", name)
				}
			}
			var clipped []string
			for _, c := range s.Columns {
				if c.Clip > 0 {
					clipped = append(clipped, c.Name)
				}
			}
			if !slices.Equal(clipped, auditClippedColumns[s.Name]) {
				t.Errorf("clipped columns = %v, want %v", clipped, auditClippedColumns[s.Name])
			}
			for _, name := range auditFixedText[s.Name] {
				c, ok := cols[name]
				switch {
				case !ok:
					t.Errorf("fixed-text column %q is not served", name)
				case c.Clip != 0:
					t.Errorf("fixed-text column %q is also clipped; list it in one place", name)
				}
			}

			if n := len(s.Filters); n < 3 || n > 5 {
				t.Errorf("%d filters, want 3..5", n)
			}
			seen := map[string]bool{}
			for _, f := range s.Filters {
				if !identPattern.MatchString(f.Name) {
					t.Errorf("filter %q is not a plain identifier", f.Name)
				}
				if seen[f.Name] {
					t.Errorf("filter %q listed twice", f.Name)
				}
				seen[f.Name] = true
				if _, ok := cols[f.Column]; !ok {
					t.Errorf("filter %q filters column %q, which is not served", f.Name, f.Column)
				}
				if f.Name == "after" || f.Name == "limit" {
					t.Errorf("filter %q collides with a paging parameter", f.Name)
				}
			}
		})
	}
}

func TestLookupAuditSource(t *testing.T) {
	for _, name := range wantAuditSources {
		s, err := LookupAuditSource(name)
		if err != nil || s.Name != name {
			t.Errorf("Lookup(%q) = %+v, %v", name, s, err)
		}
	}
	for _, name := range []string{"", "users", "FN_CALLS", "fn_calls ", "fn_calls;", "../fn_calls", "transcript_chunks"} {
		_, err := LookupAuditSource(name)
		if !errors.Is(err, ErrAuditUnknownSource) {
			t.Errorf("Lookup(%q) err = %v, want ErrAuditUnknownSource", name, err)
		} else if !strings.Contains(err.Error(), strings.Join(wantAuditSources, ", ")) {
			t.Errorf("Lookup(%q) error %q does not list the valid sources", name, err)
		}
	}
}

// AuditSources and LookupAuditSource hand out copies: a caller cannot widen the
// allowlist by editing what it was given.
func TestAuditRegistryIsNotMutableFromOutside(t *testing.T) {
	got := AuditSources()
	got[0].Columns[0].Name = "args"
	got[0].Filters[0].Column = "args"
	got[0].Name = "x"
	s, err := LookupAuditSource("finding_events")
	if err != nil {
		t.Fatal(err)
	}
	s.Columns[1].Name = "input"
	again, err := LookupAuditSource("finding_events")
	if err != nil || again.Columns[0].Name != "id" || again.Columns[1].Name != "finding_id" || again.Filters[0].Column != "finding_id" {
		t.Errorf("registry changed through a returned copy: %+v, %v", again, err)
	}
}

func TestAuditCheckFilter(t *testing.T) {
	const goodUUID = "e530a6f7-c96a-4cd5-a910-f809a99e0449"
	fe, err := LookupAuditSource("finding_events")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, value string
		want        error
	}{
		{"finding_id", goodUUID, nil},
		{"finding_id", strings.ToUpper(goodUUID), nil},
		{"transcript_id", goodUUID, nil},
		{"recipe_id", "398b8afe", nil},
		{"kind", "revoke", nil},
		{"kind", "x'; DROP TABLE fn_calls;--", nil}, // text: bound, never interpreted
		{"kind", "café", nil},                       // multi-byte UTF-8 is text
		{"kind", "a\x00b", ErrAuditBadFilterValue},
		{"kind", "\x00", ErrAuditBadFilterValue},
		{"kind", "\xff", ErrAuditBadFilterValue},
		{"kind", "ok\xc3\x28", ErrAuditBadFilterValue}, // a truncated multi-byte sequence
		{"recipe_id", "a\x00b", ErrAuditBadFilterValue},
		{"finding_id", "\x00", ErrAuditBadFilterValue},
		{"finding_id", "not-a-uuid", ErrAuditBadFilterValue},
		{"finding_id", goodUUID + "0", ErrAuditBadFilterValue},
		{"finding_id", "{" + goodUUID + "}", ErrAuditBadFilterValue},
		{"finding_id", "urn:uuid:" + goodUUID, ErrAuditBadFilterValue},
		{"finding_id", strings.ReplaceAll(goodUUID, "-", ""), ErrAuditBadFilterValue},
		{"transcript_id", "'; DROP TABLE x;--", ErrAuditBadFilterValue},
		{"kind", "", ErrAuditBadFilterValue},
		{"fn", "judge_chunk", ErrAuditUnknownFilter}, // a fn_calls filter
		{"after", "1", ErrAuditUnknownFilter},
		{"id", "1", ErrAuditUnknownFilter},
		{"actor", "system", ErrAuditUnknownFilter}, // a served column that is not a filter
		{"", "x", ErrAuditUnknownFilter},
	}
	for _, c := range cases {
		err := fe.CheckFilter(c.name, c.value)
		if !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
			t.Errorf("CheckFilter(%q, %q) = %v, want %v", c.name, c.value, err, c.want)
		}
		if err != nil && c.value != "" && strings.Contains(err.Error(), c.value) {
			t.Errorf("CheckFilter(%q, %q) echoes the value: %v", c.name, c.value, err)
		}
	}
	err = fe.CheckFilter("fn", "x")
	if err == nil || !strings.Contains(err.Error(), "finding_id, transcript_id, recipe_id, kind") {
		t.Errorf("unknown-filter error %v does not list the valid filters", err)
	}
}

// sqlWords are the keywords, functions and types the builder emits itself; every
// other word in a statement must be a registry table or column.
var sqlWords = map[string]bool{
	"select": true, "coalesce": true, "json_agg": true, "row_to_json": true, "order": true, "by": true,
	"t": true, "as": true, "json": true, "max": true, "from": true, "where": true, "and": true, "limit": true,
	"left": true, "text": true, "uuid": true,
}

var wordPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

func TestAuditBuildQuery(t *testing.T) {
	const hostile = "x'; DROP TABLE fn_calls;--"
	const goodUUID = "e530a6f7-c96a-4cd5-a910-f809a99e0449"

	for _, s := range AuditSources() {
		t.Run(s.Name, func(t *testing.T) {
			allowed := map[string]bool{s.Table: true}
			var sel []string
			for _, c := range s.Columns {
				allowed[c.Name] = true
				sel = append(sel, c.selectExpr())
			}
			wantSelect := "SELECT coalesce(json_agg(row_to_json(t) ORDER BY t.id), '[]'::json), max(t.id) FROM (SELECT " +
				strings.Join(sel, ", ") + " FROM " + s.Table + " WHERE id > $1"

			// No filters: after, limit and nothing else.
			q, args, err := s.buildQuery(0, AuditDefaultLimit, nil)
			if err != nil {
				t.Fatal(err)
			}
			if want := wantSelect + " ORDER BY id LIMIT $2) t"; q != want {
				t.Errorf("default query\n got: %s\nwant: %s", q, want)
			}
			if !slices.Equal(args, []any{int64(0), AuditDefaultLimit}) {
				t.Errorf("default args = %v", args)
			}

			// Every filter at once: clauses in registry order, placeholders
			// consecutive, values only in args, UUID filters cast from text.
			filters := map[string]string{}
			for _, f := range s.Filters {
				if f.UUID {
					filters[f.Name] = goodUUID
				} else {
					filters[f.Name] = hostile
				}
			}
			q, args, err = s.buildQuery(41, 7, filters)
			if err != nil {
				t.Fatal(err)
			}
			want := wantSelect
			wantArgs := []any{int64(41)}
			for i, f := range s.Filters {
				want += fmt.Sprintf(" AND %s = $%d", f.Column, i+2)
				if f.UUID {
					want += "::text::uuid"
				}
				wantArgs = append(wantArgs, filters[f.Name])
			}
			want += fmt.Sprintf(" ORDER BY id LIMIT $%d) t", len(s.Filters)+2)
			wantArgs = append(wantArgs, 7)
			if q != want {
				t.Errorf("filtered query\n got: %s\nwant: %s", q, want)
			}
			if !slices.Equal(args, wantArgs) {
				t.Errorf("filtered args = %v, want %v", args, wantArgs)
			}
			for _, bad := range []string{hostile, goodUUID, "DROP", ";", "--"} {
				if strings.Contains(q, bad) {
					t.Errorf("statement contains the value %q:\n%s", bad, q)
				}
			}

			// Every word in the statement is the builder's own or a registry name.
			for _, w := range wordPattern.FindAllString(strings.ReplaceAll(q, "'[]'", ""), -1) {
				if lw := strings.ToLower(w); !sqlWords[lw] && !allowed[w] {
					t.Errorf("statement uses %q, which is neither builder SQL nor a registry name:\n%s", w, q)
				}
			}
		})
	}
}

// The filter clauses follow registry order whichever filters are set and
// however the caller's map happens to iterate.
func TestAuditBuildQueryFilterOrderIsStable(t *testing.T) {
	s, err := LookupAuditSource("finding_events")
	if err != nil {
		t.Fatal(err)
	}
	const id = "e530a6f7-c96a-4cd5-a910-f809a99e0449"
	filters := map[string]string{"kind": "decision", "recipe_id": "r", "transcript_id": id, "finding_id": id}
	first, _, err := s.buildQuery(0, 10, filters)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "AND finding_id = $2::text::uuid AND transcript_id = $3::text::uuid AND recipe_id = $4 AND kind = $5 ORDER BY") {
		t.Errorf("filter clauses out of registry order: %s", first)
	}
	for i := 0; i < 20; i++ {
		again, _, _ := s.buildQuery(0, 10, filters)
		if again != first {
			t.Fatalf("statement changed between calls:\n%s\n%s", first, again)
		}
	}
	sub, args, err := s.buildQuery(0, 10, map[string]string{"kind": "revoke"})
	if err != nil || !strings.Contains(sub, "AND kind = $2 ORDER BY id LIMIT $3)") || len(args) != 3 {
		t.Errorf("single filter: %s %v %v", sub, args, err)
	}
}

func TestAuditBuildQueryRejects(t *testing.T) {
	s, err := LookupAuditSource("fn_calls")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		after   int64
		limit   int
		filters map[string]string
		want    error
	}{
		{"unknown filter", 0, 10, map[string]string{"input": "x"}, ErrAuditUnknownFilter},
		{"a column that is not a filter", 0, 10, map[string]string{"input_sha256": "x"}, ErrAuditUnknownFilter},
		{"filter named like SQL", 0, 10, map[string]string{"fn = 1 OR 1": "x"}, ErrAuditUnknownFilter},
		{"empty value", 0, 10, map[string]string{"fn": ""}, ErrAuditBadFilterValue},
		{"negative after", -1, 10, nil, nil},
		{"limit zero", 0, 0, nil, nil},
		{"limit over cap", 0, AuditMaxLimit + 1, nil, nil},
		{"negative limit", 0, -5, nil, nil},
	}
	for _, c := range cases {
		q, args, err := s.buildQuery(c.after, c.limit, c.filters)
		if err == nil || q != "" || args != nil {
			t.Errorf("%s: accepted (%q, %v, %v)", c.name, q, args, err)
		} else if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// The cap itself is valid.
	if _, _, err := s.buildQuery(0, AuditMaxLimit, nil); err != nil {
		t.Errorf("limit %d rejected: %v", AuditMaxLimit, err)
	}
}

func TestAuditRowsUnknownSource(t *testing.T) {
	d := &DB{} // never reaches the pool
	_, err := d.AuditRows(context.Background(), "transcript_chunks", 0, 10, nil)
	if !errors.Is(err, ErrAuditUnknownSource) {
		t.Errorf("err = %v, want ErrAuditUnknownSource", err)
	}
}

func TestReadAudit(t *testing.T) {
	ctx := context.Background()
	s, err := LookupAuditSource("step_runs")
	if err != nil {
		t.Fatal(err)
	}
	roTx := pgx.TxOptions{AccessMode: pgx.ReadOnly}
	rowsJSON := `[{"id":365,"step":"decide"},{"id":366,"step":"scan"}]`

	t.Run("a page", func(t *testing.T) {
		mock := newMockPool(t)
		last := int64(366)
		mock.ExpectBeginTx(roTx)
		mock.ExpectExec(`set_config\('jit', 'off', true\), set_config\('TimeZone', 'UTC', true\), set_config\('statement_timeout', \$1, true\)`).
			WithArgs("5000").WillReturnResult(pgxmock.NewResult("SELECT", 1))
		mock.ExpectQuery(`FROM step_runs WHERE id > \$1 AND status = \$2 ORDER BY id LIMIT \$3\) t`).
			WithArgs(int64(364), "running", 100).
			WillReturnRows(pgxmock.NewRows([]string{"rows", "next"}).AddRow([]byte(rowsJSON), &last))
		mock.ExpectCommit()
		page, err := readAudit(ctx, mock, s, 364, 100, map[string]string{"status": "running"})
		if err != nil {
			t.Fatal(err)
		}
		if page.Source != "step_runs" || string(page.Rows) != rowsJSON || page.Next == nil || *page.Next != 366 {
			t.Errorf("page = %+v", page)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("empty page has no cursor", func(t *testing.T) {
		mock := newMockPool(t)
		mock.ExpectBeginTx(roTx)
		mock.ExpectExec(`set_config`).WithArgs("5000").WillReturnResult(pgxmock.NewResult("SELECT", 1))
		mock.ExpectQuery(`FROM step_runs`).WithArgs(int64(0), 100).
			WillReturnRows(pgxmock.NewRows([]string{"rows", "next"}).AddRow([]byte("[]"), (*int64)(nil)))
		mock.ExpectCommit()
		page, err := readAudit(ctx, mock, s, 0, 100, nil)
		if err != nil || string(page.Rows) != "[]" || page.Next != nil {
			t.Errorf("page = %+v, err = %v", page, err)
		}
	})

	// A query that is rejected before the pool is touched never opens a transaction.
	t.Run("invalid request never reaches the pool", func(t *testing.T) {
		mock := newMockPool(t)
		if _, err := readAudit(ctx, mock, s, 0, 100, map[string]string{"fn": "x"}); !errors.Is(err, ErrAuditUnknownFilter) {
			t.Errorf("err = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	failures := []struct {
		name    string
		err     error
		timeout bool
	}{
		{"statement timeout", &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}, true},
		{"context deadline", context.DeadlineExceeded, true},
		{"client went away", context.Canceled, false},
		{"other database error", &pgconn.PgError{Code: "42P01", Message: `relation "step_runs" does not exist`}, false},
		{"plain error", errors.New("conn reset"), false},
	}
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			mock := newMockPool(t)
			mock.ExpectBeginTx(roTx)
			mock.ExpectExec(`set_config`).WithArgs("5000").WillReturnResult(pgxmock.NewResult("SELECT", 1))
			mock.ExpectQuery(`FROM step_runs`).WithArgs(int64(0), 100).WillReturnError(f.err)
			mock.ExpectRollback()
			_, err := readAudit(ctx, mock, s, 0, 100, nil)
			if err == nil {
				t.Fatal("no error")
			}
			if got := errors.Is(err, ErrAuditTimeout); got != f.timeout {
				t.Errorf("errors.Is(ErrAuditTimeout) = %v, want %v (%v)", got, f.timeout, err)
			}
			if !errors.Is(err, ErrAuditTimeout) && !errors.Is(err, f.err) {
				t.Errorf("error %v does not wrap %v", err, f.err)
			}
		})
	}

	t.Run("begin failure", func(t *testing.T) {
		mock := newMockPool(t)
		mock.ExpectBeginTx(roTx).WillReturnError(context.DeadlineExceeded)
		if _, err := readAudit(ctx, mock, s, 0, 100, nil); !errors.Is(err, ErrAuditTimeout) {
			t.Errorf("err = %v", err)
		}
	})
}
