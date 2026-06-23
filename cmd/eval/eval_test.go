package eval

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
)

// fakeRunner records the RunOptions it was called with and returns canned data.
type fakeRunner struct {
	findings []db.Finding
	stats    evalpkg.RunStats
	gotOpts  evalpkg.RunOptions
	called   bool
}

func (f *fakeRunner) Run(_ context.Context, o evalpkg.RunOptions) ([]db.Finding, evalpkg.RunStats, error) {
	f.called = true
	f.gotOpts = o
	return f.findings, f.stats, nil
}

func sampleFinding() db.Finding {
	return db.Finding{FilePath: "/b/Dune/01.m4b", IssueType: "misheard_proper_noun", OriginalText: "Paul Atreides", Confidence: 0.8}
}

func TestRun_DryRunDoesNotWrite(t *testing.T) {
	f := &fakeRunner{
		findings: []db.Finding{sampleFinding()},
		stats:    evalpkg.RunStats{ChunksEvaluated: 1, FindingsFound: 1, Persisted: false},
	}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Dune", options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotOpts.Write {
		t.Error("dry-run must pass Write=false to the runner")
	}
	if s := out.String(); !strings.Contains(s, "(dry-run)") {
		t.Errorf("expected dry-run notice, got:\n%s", s)
	}
}

func TestRun_WritePassesWriteFlag(t *testing.T) {
	f := &fakeRunner{
		findings: []db.Finding{sampleFinding()},
		stats:    evalpkg.RunStats{ChunksEvaluated: 1, FindingsFound: 1, Persisted: true},
	}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Dune", options{write: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !f.gotOpts.Write {
		t.Error("--write must pass Write=true to the runner")
	}
	if s := out.String(); !strings.Contains(s, "Recorded 1 finding") {
		t.Errorf("expected recorded notice, got:\n%s", s)
	}
}

func TestRun_SamplePassesSampleSize(t *testing.T) {
	f := &fakeRunner{stats: evalpkg.RunStats{ChunksEvaluated: 0, FindingsFound: 0}}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "", options{sample: 25}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.gotOpts.Sample != 25 {
		t.Errorf("Sample = %d, want 25", f.gotOpts.Sample)
	}
}

func TestRun_RejectsNoScope(t *testing.T) {
	f := &fakeRunner{}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "", options{}); err == nil {
		t.Fatal("expected error with neither book nor --sample")
	}
	if f.called {
		t.Error("runner should not be called when scope is invalid")
	}
}

func TestRun_RejectsBookAndSample(t *testing.T) {
	f := &fakeRunner{}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Dune", options{sample: 10}); err == nil {
		t.Fatal("expected error when both book and --sample are given")
	}
	if f.called {
		t.Error("runner should not be called when scope is ambiguous")
	}
}

func TestTruncate_RuneSafe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"ascii under limit", "hello", 60, "hello"},
		{"ascii over limit", "abcdef", 3, "abc…"},
		// "Atréïdes Bʁöñ" is multi-byte; truncating at a rune boundary must not
		// split a codepoint (a byte slice at n=5 would corrupt the é/ï).
		{"multibyte not split", "Atréïdes Brön", 5, "Atréï…"},
		{"multibyte under limit", "café", 60, "café"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.n)
			if got != tc.want {
				t.Errorf("truncate(%q,%d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncate(%q,%d) produced invalid UTF-8: %q", tc.in, tc.n, got)
			}
		})
	}
}

// TestRunOutput_FormatsFindingsCorrectly verifies the operator-facing preview
// line: confidence as [0.XX] (%.2f), the issue type and filename, and long
// original text truncated to 60 runes + ellipsis.
func TestRunOutput_FormatsFindingsCorrectly(t *testing.T) {
	longText := "Thufir Hawat says the spice must flow across the whole of Arrakis tonight" // > 60 runes
	f := &fakeRunner{
		findings: []db.Finding{{
			FilePath:     "/books/Dune/01.m4b",
			IssueType:    "misheard_proper_noun",
			OriginalText: longText,
			Confidence:   0.8456,
		}},
		stats: evalpkg.RunStats{ChunksEvaluated: 1, FindingsFound: 1},
	}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Dune", options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	s := out.String()

	if !strings.Contains(s, "[0.85]") { // 0.8456 → %.2f → 0.85
		t.Errorf("output should contain confidence [0.85], got:\n%s", s)
	}
	if !strings.Contains(s, "misheard_proper_noun") {
		t.Errorf("output should contain issue_type, got:\n%s", s)
	}
	if !strings.Contains(s, "01.m4b") {
		t.Errorf("output should contain filename, got:\n%s", s)
	}
	if !strings.Contains(s, "…") {
		t.Errorf("long original text should be truncated with an ellipsis, got:\n%s", s)
	}
	// The full (untruncated) long text must NOT appear verbatim.
	if strings.Contains(s, longText) {
		t.Errorf("long original text should have been truncated, but appeared in full:\n%s", s)
	}
}

// TestRunOutput_ReportsSkippedChunks verifies the partial-results notice surfaces
// when transient judge errors skipped chunks (RunStats.ChunksSkipped > 0).
func TestRunOutput_ReportsSkippedChunks(t *testing.T) {
	f := &fakeRunner{
		findings: nil,
		stats:    evalpkg.RunStats{ChunksEvaluated: 0, ChunksSkipped: 3, FindingsFound: 0},
	}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Book", options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "3 chunk(s) skipped") {
		t.Errorf("output should report skipped chunks on transient errors, got:\n%s", out.String())
	}
}

// ─── Backfill tests (CONTRACT §2.15, §1.5) ───────────────────────────────────

// fakeBackfillDB implements backfillDB for unit tests without a live DB.
type fakeBackfillDB struct {
	transcripts   []*db.Transcript
	findings      []db.Finding
	evalMetrics   []db.EvalMetrics
	findingsErr   error
	metricsErr    error
	transcriptErr error
}

func (f *fakeBackfillDB) GetUnevaluatedJobTranscripts(_ context.Context) ([]*db.Transcript, error) {
	if f.transcriptErr != nil {
		return nil, f.transcriptErr
	}
	return f.transcripts, nil
}

func (f *fakeBackfillDB) InsertFindings(_ context.Context, findings []db.Finding) error {
	if f.findingsErr != nil {
		return f.findingsErr
	}
	f.findings = append(f.findings, findings...)
	return nil
}

func (f *fakeBackfillDB) UpsertEvalMetrics(_ context.Context, m db.EvalMetrics) error {
	if f.metricsErr != nil {
		return f.metricsErr
	}
	f.evalMetrics = append(f.evalMetrics, m)
	return nil
}

// fakeJudge implements the chat.Client interface used by evalpkg.NewJudge.
type fakeBackfillChat struct{ resp string }

func (c fakeBackfillChat) Complete(_ context.Context, _, _ string) (string, error) {
	return c.resp, nil
}
func (c fakeBackfillChat) Model() string { return "fake-backfill-judge" }

// TestRunBackfill_DryRunDoesNotWrite verifies that in dry-run mode (write=false)
// the backfill function prints what it would do but writes no findings and no
// eval_finished_at rows.
func TestRunBackfill_DryRunDoesNotWrite(t *testing.T) {
	fdb := &fakeBackfillDB{
		transcripts: []*db.Transcript{
			{
				ID:       "t1",
				JobID:    "j1",
				FilePath: "/books/Dune/Chapter1.m4b",
				RawText:  "The spice must flow. Paul Atreides walked the sands of Arrakis.",
			},
		},
	}
	judge := evalpkg.NewJudge(fakeBackfillChat{
		resp: `{"findings":[{"original_text":"Atreides","issue_type":"misheard_proper_noun","confidence":0.9}]}`,
	})
	cfg := &config.Config{ChunkSize: 32}

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, cfg, false); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}

	if len(fdb.findings) != 0 {
		t.Errorf("dry-run must not persist findings, got %d", len(fdb.findings))
	}
	if len(fdb.evalMetrics) != 0 {
		t.Errorf("dry-run must not persist eval_finished_at, got %d", len(fdb.evalMetrics))
	}
	if s := out.String(); !strings.Contains(s, "(dry-run)") {
		t.Errorf("expected dry-run notice, got:\n%s", s)
	}
}

// TestRunBackfill_WritePersistesFindingsAndEvalFinishedAt verifies that in write
// mode the backfill persists findings and writes eval_finished_at for each
// transcript (the embed-gate latch).
func TestRunBackfill_WritePersistesFindingsAndEvalFinishedAt(t *testing.T) {
	fdb := &fakeBackfillDB{
		transcripts: []*db.Transcript{
			{
				ID:       "t-write",
				JobID:    "j-write",
				FilePath: "/books/Dune/Ch2.m4b",
				RawText:  "Fear is the mind killer. I must not fear.",
			},
		},
	}
	judge := evalpkg.NewJudge(fakeBackfillChat{
		resp: `{"findings":[{"original_text":"fear","issue_type":"misheard_word","suggested_correction":"spice","confidence":0.85}]}`,
	})
	cfg := &config.Config{ChunkSize: 32}

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, cfg, true); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}

	if len(fdb.findings) == 0 {
		t.Error("write mode must persist findings")
	}
	if len(fdb.evalMetrics) != 1 {
		t.Errorf("expected exactly 1 eval_metrics row (eval_finished_at), got %d", len(fdb.evalMetrics))
	}
	em := fdb.evalMetrics[0]
	if em.JobID != "j-write" {
		t.Errorf("eval_metrics.JobID = %q, want %q", em.JobID, "j-write")
	}
	if em.FinishedAt.IsZero() {
		t.Error("eval_metrics.FinishedAt must be set (it is the embed-gate latch)")
	}
	if em.Model != "fake-backfill-judge" {
		t.Errorf("eval_metrics.Model = %q, want %q", em.Model, "fake-backfill-judge")
	}
}

// TestRunBackfill_EmptyQueueReportsNoWork verifies that when there are no
// unevaluated transcripts the backfill prints a "nothing to do" message and
// returns nil.
func TestRunBackfill_EmptyQueueReportsNoWork(t *testing.T) {
	fdb := &fakeBackfillDB{}
	judge := evalpkg.NewJudge(fakeBackfillChat{resp: `{"findings":[]}`})
	cfg := &config.Config{ChunkSize: 32}

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, cfg, false); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}
	if s := out.String(); !strings.Contains(s, "nothing to backfill") {
		t.Errorf("expected 'nothing to backfill' message, got:\n%s", s)
	}
}

// TestRunBackfill_MultipleTranscriptsEachGetEvalMetrics verifies that each
// transcript in the backfill batch receives its own eval_finished_at row, not
// a single aggregated one.
func TestRunBackfill_MultipleTranscriptsEachGetEvalMetrics(t *testing.T) {
	fdb := &fakeBackfillDB{
		transcripts: []*db.Transcript{
			{ID: "t1", JobID: "j1", FilePath: "/books/A/ch1.m4b", RawText: "First book text here."},
			{ID: "t2", JobID: "j2", FilePath: "/books/B/ch1.m4b", RawText: "Second book text here."},
		},
	}
	judge := evalpkg.NewJudge(fakeBackfillChat{resp: `{"findings":[]}`})
	cfg := &config.Config{ChunkSize: 32}

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, cfg, true); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}

	if len(fdb.evalMetrics) != 2 {
		t.Errorf("expected 2 eval_metrics rows (one per transcript), got %d", len(fdb.evalMetrics))
	}
	jobIDs := map[string]bool{fdb.evalMetrics[0].JobID: true, fdb.evalMetrics[1].JobID: true}
	if !jobIDs["j1"] || !jobIDs["j2"] {
		t.Errorf("eval_metrics should cover both job IDs, got %v", jobIDs)
	}
}
