package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
)

func norm(sql string) string { return strings.Join(strings.Fields(sql), " ") }

// A failed attempt goes through recordEvalFailureSQL, which must never write the
// eval_finished_at latch (or the success columns of an earlier latched run).
func TestRecordEvalFailureSQL_NeverLatches(t *testing.T) {
	sql := norm(recordEvalFailureSQL)
	for _, forbidden := range []string{"eval_finished_at", "eval_started_at", "eval_chunks", "eval_findings", "eval_skipped"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("recordEvalFailureSQL must not touch %s:\n%s", forbidden, sql)
		}
	}
	for _, c := range []string{"eval_failed_at", "eval_failed_chunks", "eval_error"} {
		if !strings.Contains(sql, c+" = EXCLUDED."+c) {
			t.Errorf("recordEvalFailureSQL must assign %s", c)
		}
	}
	if !strings.Contains(sql, "ON CONFLICT (job_id) DO UPDATE") {
		t.Error("recordEvalFailureSQL must UPSERT on job_id")
	}
}

// A successful run clears any earlier failure record so the job leaves the
// eval-error backfill selection.
func TestUpsertEvalMetricsSQL_ClearsFailure(t *testing.T) {
	sql := norm(upsertEvalMetricsSQL)
	for _, c := range []string{"eval_failed_at = NULL", "eval_failed_chunks = NULL", "eval_error = NULL"} {
		if !strings.Contains(sql, c) {
			t.Errorf("upsertEvalMetricsSQL must clear the failure record (%s):\n%s", c, sql)
		}
	}
}

func TestUpsertEvalMetrics_RoutesByOutcome(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	t.Run("success latches", func(t *testing.T) {
		ex := &captureExecer{}
		m := EvalMetrics{JobID: "j", StartedAt: now, FinishedAt: now.Add(time.Second), Model: "m", Chunks: 3}
		if err := upsertEvalMetrics(context.Background(), ex, m); err != nil {
			t.Fatal(err)
		}
		if ex.sql[0] != upsertEvalMetricsSQL {
			t.Fatalf("success must use upsertEvalMetricsSQL")
		}
		if got := ex.args[0][2]; got != m.FinishedAt {
			t.Errorf("$3 eval_finished_at = %v, want %v", got, m.FinishedAt)
		}
	})

	t.Run("failure records without latch, truncates error", func(t *testing.T) {
		ex := &captureExecer{}
		m := EvalMetrics{JobID: "j", FailedAt: now, FailedChunks: 2, Error: strings.Repeat("é", maxEvalErrorLen+50)}
		if !m.Failed() {
			t.Fatal("zero FinishedAt must be Failed()")
		}
		if err := upsertEvalMetrics(context.Background(), ex, m); err != nil {
			t.Fatal(err)
		}
		if ex.sql[0] != recordEvalFailureSQL {
			t.Fatalf("failure must use recordEvalFailureSQL")
		}
		args := ex.args[0]
		if args[0] != "j" || args[1] != now || args[2] != 2 {
			t.Errorf("args = %v, want [j %v 2 …]", args[:3], now)
		}
		reason, _ := args[3].(*string)
		if reason == nil || len([]rune(*reason)) != maxEvalErrorLen {
			t.Errorf("eval_error must be truncated to %d runes", maxEvalErrorLen)
		}
	})
}

// The gated passes treat a recorded failure as "attempted": the eval pass skips
// it (no hot re-judge loop) and the embed pass takes it (fail-open).
func TestGatedSelections_FailedEvalReleasesEmbedNotReJudged(t *testing.T) {
	evalSQL := norm(unevaluatedTranscriptsSQL)
	if !strings.Contains(evalSQL, "NOT EXISTS ( SELECT 1 FROM run_metrics rm WHERE rm.job_id = j.id AND (rm.eval_finished_at IS NOT NULL OR rm.eval_failed_at IS NOT NULL) )") {
		t.Errorf("eval pass must skip latched AND failed jobs:\n%s", evalSQL)
	}
	embedSQL := norm(evaluatedUnembeddedTranscriptsSQL)
	if !strings.Contains(embedSQL, "AND (rm.eval_finished_at IS NOT NULL OR rm.eval_failed_at IS NOT NULL)") {
		t.Errorf("embed pass must accept latched OR failed jobs (fail-open):\n%s", embedSQL)
	}
}

func TestEvalErrorTranscriptsSQL_Shape(t *testing.T) {
	sql := norm(evalErrorTranscriptsSQL)
	for _, want := range []string{
		"j.status = 'done'",
		"rm.eval_failed_at IS NOT NULL",
		"COALESCE(rm.eval_skipped, 0) > 0",
		"pe.stage = 'eval'",
		"pe.event = 'error'",
		"jsonb_typeof(pe.detail->'skipped') = 'number'",
		"THEN (pe.detail->>'skipped')::numeric > 0 ELSE false END",
		// Legacy CLI backfill that stopped on a client timeout: latched with
		// eval_skipped = 0 and no event, but fewer chunks judged than stored.
		"OR COALESCE(rm.eval_chunks, 0) < ( SELECT count(*) FROM transcript_chunks c WHERE c.transcript_id = t.id )",
		"pe.created_at >= COALESCE(rm.eval_started_at, '-infinity'::timestamptz)",
		"ORDER BY t.created_at ASC, t.id ASC",
		"LIMIT $3",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("eval-error selection missing %q:\n%s", want, sql)
		}
	}
	for _, verb := range []string{"UPDATE ", "DELETE ", "INSERT "} {
		if strings.Contains(sql, verb) {
			t.Errorf("selection must be read-only, found %q", verb)
		}
	}
}

// Keyset paging: the zero cursor binds NULLs (from the start); a real cursor
// binds (created_at, id); the limit is bounded.
func TestGetTranscriptPage_KeysetArgs(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	ts := time.Unix(1_700_000_000, 0)
	rows := pgxmock.NewRows(transcriptScanColumns)
	addTranscriptRow(rows, "t1", "j1")
	mock.ExpectQuery(unevaluatedJobTranscriptsSQL).WithArgs(nil, nil, defaultSelectLimit).WillReturnRows(rows)
	mock.ExpectQuery(evalErrorTranscriptsSQL).WithArgs(ts, "t1", 5).WillReturnRows(pgxmock.NewRows(transcriptScanColumns))

	got, err := getTranscriptPage(context.Background(), mock, unevaluatedJobTranscriptsSQL, TranscriptCursor{}, 0, "x")
	if err != nil || len(got) != 1 {
		t.Fatalf("first page: %v (%d rows)", err, len(got))
	}
	if _, err := getTranscriptPage(context.Background(), mock, evalErrorTranscriptsSQL,
		TranscriptCursor{CreatedAt: ts, ID: "t1"}, 5, "x"); err != nil {
		t.Fatalf("next page: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetFindingKeys_Scans(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	mock.ExpectQuery(findingKeysSQL).WithArgs("tr").WillReturnRows(
		pgxmock.NewRows([]string{"chunk_id", "original_text", "issue_type", "suggested_correction"}).
			AddRow("c1", "Fear", "misheard_word", "Fire"))
	keys, err := getFindingKeys(context.Background(), mock, "tr")
	if err != nil {
		t.Fatal(err)
	}
	chunk := "c1"
	fix := "Fire"
	if !keys[KeyOf(Finding{ChunkID: &chunk, OriginalText: "Fear", IssueType: "misheard_word", SuggestedCorrection: &fix})] {
		t.Errorf("KeyOf must match the stored key; got %v", keys)
	}
}

// The ungated selection is bounded and keyset-paged (Phase 0a item 1), and the
// backfill's unevaluated selection deliberately does NOT filter on embed state
// (it is how transcripts embedded with EVAL_IN_PIPELINE=false get judged).
func TestPagedSelections_Shape(t *testing.T) {
	completed := norm(completedTranscriptsSQL)
	for _, want := range []string{
		"NOT EXISTS ( SELECT 1 FROM transcript_chunks c WHERE c.transcript_id = t.id )",
		"(t.created_at, t.id) > ($1::timestamptz, $2::uuid)",
		"ORDER BY t.created_at ASC, t.id ASC",
		"LIMIT $3",
	} {
		if !strings.Contains(completed, want) {
			t.Errorf("completedTranscriptsSQL missing %q:\n%s", want, completed)
		}
	}
	uneval := norm(unevaluatedJobTranscriptsSQL)
	if strings.Contains(uneval, "transcript_chunks") {
		t.Errorf("backfill selection must cover embedded transcripts (no transcript_chunks filter):\n%s", uneval)
	}
	if !strings.Contains(uneval, "rm.eval_finished_at IS NOT NULL") || strings.Contains(uneval, "eval_failed_at") {
		t.Errorf("backfill selection must key only on the latch, so recorded failures are retried:\n%s", uneval)
	}
}

func TestGetCompletedTranscripts_Paged(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	rows := pgxmock.NewRows(transcriptScanColumns)
	addTranscriptRow(rows, "t1", "j1")
	addTranscriptRow(rows, "t2", "j2")
	mock.ExpectQuery(completedTranscriptsSQL).WithArgs(nil, nil, 2).WillReturnRows(rows)
	got, err := getTranscriptPage(context.Background(), mock, completedTranscriptsSQL, TranscriptCursor{}, 2, "completed transcripts")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d rows, err %v", len(got), err)
	}
	if c := CursorAfter(got[1]); c.ID != "t2" || !c.CreatedAt.Equal(got[1].CreatedAt) {
		t.Errorf("CursorAfter = %+v, want t2 @ its created_at", c)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The resolved judge model is written next to the requested one: on each
// finding (resolved_model, $16) and on the run's eval slice
// (eval_resolved_model, $8).
func TestResolvedModelColumns(t *testing.T) {
	ins := norm(insertFindingSQL)
	if !strings.Contains(ins, "chunk_text_sha256, resolved_model)") || !strings.Contains(ins, "$15, $16)") {
		t.Errorf("insertFindingSQL must write resolved_model as $16:\n%s", ins)
	}
	up := norm(upsertEvalMetricsSQL)
	if !strings.Contains(up, "eval_findings, eval_resolved_model)") || !strings.Contains(up, "$7, $8)") ||
		!strings.Contains(up, "eval_resolved_model = EXCLUDED.eval_resolved_model") {
		t.Errorf("upsertEvalMetricsSQL must write eval_resolved_model as $8:\n%s", up)
	}

	ex := &captureExecer{}
	m := EvalMetrics{JobID: "j", FinishedAt: time.Now(), Model: "anthropic/claude-sonnet-4-5", ResolvedModel: "claude-sonnet-4-5-20250929"}
	if err := upsertEvalMetrics(context.Background(), ex, m); err != nil {
		t.Fatal(err)
	}
	if got, _ := ex.args[0][7].(*string); got == nil || *got != m.ResolvedModel {
		t.Errorf("$8 = %v, want %q", ex.args[0][7], m.ResolvedModel)
	}
	ex = &captureExecer{}
	m.ResolvedModel = ""
	if err := upsertEvalMetrics(context.Background(), ex, m); err != nil {
		t.Fatal(err)
	}
	if got, _ := ex.args[0][7].(*string); got != nil {
		t.Errorf("empty resolved model must bind NULL, got %q", *got)
	}
}

// The skipped-count cast must only run on numeric JSON, and the short-run
// predicate must sit inside the latched branch (an unlatched job is the
// --backfill-unevaluated selection's business).
func TestEvalErrorTranscriptsSQL_GuardsAndPlacement(t *testing.T) {
	sql := norm(evalErrorTranscriptsSQL)
	if strings.Contains(sql, "(pe.detail->>'skipped')::int") {
		t.Errorf("unguarded ::int cast on detail.skipped:\n%s", sql)
	}
	latched := strings.Index(sql, "OR (rm.eval_finished_at IS NOT NULL AND (")
	short := strings.Index(sql, "COALESCE(rm.eval_chunks, 0) <")
	keyset := strings.Index(sql, "AND ($1::timestamptz IS NULL")
	if latched < 0 || short < latched || short > keyset {
		t.Errorf("short-run predicate must be inside the latched branch:\n%s", sql)
	}
}
