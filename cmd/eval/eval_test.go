package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/worker"
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

// fakeBackfillDB implements backfillDB for unit tests without a live DB. The
// two selections emulate keyset paging over the scripted slices (ordered by
// slice position), recording each page request.
type fakeBackfillDB struct {
	transcripts    []*db.Transcript          // --backfill-unevaluated selection
	errTranscripts []*db.Transcript          // --backfill-eval-errors selection
	stored         map[string][]db.EvalChunk // transcript ID → stored chunk rows
	existing       map[string]map[db.FindingKey]bool
	findings       []db.Finding
	evalMetrics    []db.EvalMetrics
	findingsErr    error
	metricsErr     error
	transcriptErr  error
	pageLimits     []int // limit passed on each selection call
}

func (f *fakeBackfillDB) page(all []*db.Transcript, after db.TranscriptCursor, limit int) []*db.Transcript {
	f.pageLimits = append(f.pageLimits, limit)
	start := 0
	if after.ID != "" {
		for i, t := range all {
			if t.ID == after.ID {
				start = i + 1
			}
		}
	}
	end := min(start+limit, len(all))
	return all[start:end]
}

func (f *fakeBackfillDB) GetUnevaluatedJobTranscripts(_ context.Context, after db.TranscriptCursor, limit int) ([]*db.Transcript, error) {
	if f.transcriptErr != nil {
		return nil, f.transcriptErr
	}
	return f.page(f.transcripts, after, limit), nil
}

func (f *fakeBackfillDB) GetEvalErrorTranscripts(_ context.Context, after db.TranscriptCursor, limit int) ([]*db.Transcript, error) {
	if f.transcriptErr != nil {
		return nil, f.transcriptErr
	}
	return f.page(f.errTranscripts, after, limit), nil
}

func (f *fakeBackfillDB) GetFindingKeys(_ context.Context, transcriptID string) (map[db.FindingKey]bool, error) {
	return f.existing[transcriptID], nil
}

func (f *fakeBackfillDB) GetEvalChunksForTranscript(_ context.Context, transcriptID string) ([]db.EvalChunk, error) {
	return f.stored[transcriptID], nil
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
	cfg := &config.Config{ChunkSize: 32, EvalGatesEmbed: true}

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, cfg, backfillOptions{}); err != nil {
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
	cfg := &config.Config{ChunkSize: 32, EvalGatesEmbed: true}

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, cfg, backfillOptions{write: true}); err != nil {
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

// textGatedChat returns resp only for chunks whose prompt contains needle (an
// empty findings list otherwise), so a finding lands only on the chunk that
// really holds the flagged text.
type textGatedChat struct{ needle, resp string }

func (c textGatedChat) Complete(_ context.Context, _, user string) (string, error) {
	if strings.Contains(user, c.needle) {
		return c.resp, nil
	}
	return `{"findings":[]}`, nil
}
func (c textGatedChat) Model() string { return "fake-backfill-judge" }

// TestRunBackfill_ChunksMatchEmbedPass is the regression for the backfill
// chunk-ID mismatch: for a transcript WITH segments the embed worker chunks by
// segment, so the backfill must too. Raw-text token chunking under the same
// db.ChunkUUID(t.ID, i) judged different text than the embed pass stores there,
// so each finding's chunk ID, chunk hash and timestamps pointed at the wrong
// text. Every finding must match the embed pass's chunk under its ID.
func TestRunBackfill_ChunksMatchEmbedPass(t *testing.T) {
	tr := &db.Transcript{
		ID:       "t-seg",
		JobID:    "j-seg",
		FilePath: "/books/Dune/Ch3.m4b",
		RawText:  "Alpha one two three four. Bravo five six seven eight. Charlie nine ten eleven twelve.",
		Segments: []db.Segment{
			{ID: 0, Start: 0, End: 4, Text: "Alpha one two three four."},
			{ID: 1, Start: 4, End: 9, Text: "Bravo five six seven eight."},
			{ID: 2, Start: 9, End: 15, Text: "Charlie nine ten eleven twelve."},
		},
	}
	fdb := &fakeBackfillDB{transcripts: []*db.Transcript{tr}}
	judge := evalpkg.NewJudge(textGatedChat{
		needle: "Charlie",
		resp:   `{"findings":[{"original_text":"Charlie","issue_type":"misheard_proper_noun","suggested_correction":"Charley","confidence":0.9}]}`,
	})
	cfg := &config.Config{ChunkSize: 8, EvalGatesEmbed: true}

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, cfg, backfillOptions{write: true}); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}

	embedChunks, err := worker.PristineChunks(tr, cfg.ChunkSize)
	if err != nil {
		t.Fatalf("PristineChunks: %v", err)
	}
	byID := make(map[string]db.Chunk, len(embedChunks))
	for _, c := range embedChunks {
		byID[c.ID] = c
	}
	if len(fdb.findings) == 0 {
		t.Fatalf("expected a finding on the Charlie chunk:\n%s", out.String())
	}
	for _, f := range fdb.findings {
		if f.ChunkID == nil {
			t.Fatalf("finding has no chunk ID: %+v", f)
		}
		c, ok := byID[*f.ChunkID]
		if !ok {
			t.Fatalf("finding chunk ID %s is not an embed-pass chunk", *f.ChunkID)
		}
		if !strings.Contains(c.Text, f.OriginalText) {
			t.Errorf("chunk %d text %q does not contain finding %q", c.ChunkIndex, c.Text, f.OriginalText)
		}
		if f.ChunkTextSHA256 == nil || *f.ChunkTextSHA256 != patch.ChunkHash(c.Text) {
			t.Errorf("chunk hash does not match the embed pass's chunk %d text %q", c.ChunkIndex, c.Text)
		}
		if f.StartSec != c.StartSec || f.EndSec != c.EndSec {
			t.Errorf("finding span [%v,%v] != embed chunk span [%v,%v]", f.StartSec, f.EndSec, c.StartSec, c.EndSec)
		}
	}
}

// TestRunBackfill_EmbeddedTranscriptUsesStoredChunkIDs: an already-embedded
// transcript (ungated path → random chunk IDs) must be judged against its
// STORED rows, so findings reference chunk IDs that exist — regenerated UUIDv5
// IDs would point at nothing.
func TestRunBackfill_EmbeddedTranscriptUsesStoredChunkIDs(t *testing.T) {
	tr := &db.Transcript{ID: "t-emb", JobID: "j-emb", FilePath: "/books/Dune/Ch4.m4b",
		RawText: "Alpha one. Charlie two."}
	stored := []db.EvalChunk{
		{ChunkID: "11111111-1111-4111-8111-111111111111", TranscriptID: "t-emb", TranscriptionRunID: "j-emb",
			FilePath: tr.FilePath, ChunkIndex: 0, StartSec: 0, EndSec: 3, Text: "Alpha one."},
		{ChunkID: "22222222-2222-4222-8222-222222222222", TranscriptID: "t-emb", TranscriptionRunID: "j-emb",
			FilePath: tr.FilePath, ChunkIndex: 1, StartSec: 3, EndSec: 6, Text: "Charlie two."},
	}
	fdb := &fakeBackfillDB{
		transcripts: []*db.Transcript{tr},
		stored:      map[string][]db.EvalChunk{"t-emb": stored},
	}
	judge := evalpkg.NewJudge(textGatedChat{
		needle: "Charlie",
		resp:   `{"findings":[{"original_text":"Charlie","issue_type":"misheard_proper_noun","suggested_correction":"Charley","confidence":0.9}]}`,
	})

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, &config.Config{ChunkSize: 8}, backfillOptions{write: true}); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}
	if len(fdb.findings) != 1 {
		t.Fatalf("expected 1 finding, got %d:\n%s", len(fdb.findings), out.String())
	}
	f := fdb.findings[0]
	if f.ChunkID == nil || *f.ChunkID != stored[1].ChunkID {
		t.Errorf("finding chunk ID = %v, want the stored row's ID %s", f.ChunkID, stored[1].ChunkID)
	}
	if f.ChunkTextSHA256 == nil || *f.ChunkTextSHA256 != patch.ChunkHash(stored[1].Text) {
		t.Errorf("chunk hash must fingerprint the stored pristine text")
	}
}

// TestRunBackfill_EmptyQueueReportsNoWork verifies that when there are no
// unevaluated transcripts the backfill prints a "nothing to do" message and
// returns nil.
func TestRunBackfill_EmptyQueueReportsNoWork(t *testing.T) {
	fdb := &fakeBackfillDB{}
	judge := evalpkg.NewJudge(fakeBackfillChat{resp: `{"findings":[]}`})
	cfg := &config.Config{ChunkSize: 32, EvalGatesEmbed: true}

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, cfg, backfillOptions{}); err != nil {
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
	cfg := &config.Config{ChunkSize: 32, EvalGatesEmbed: true}

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, cfg, backfillOptions{write: true}); err != nil {
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

// errBackfillChat fails every judge call.
type errBackfillChat struct{}

func (errBackfillChat) Complete(context.Context, string, string) (string, error) {
	return "", errors.New("judge endpoint down")
}
func (errBackfillChat) Model() string { return "fake-backfill-judge" }

// TestRunBackfill_JudgeErrorDoesNotLatch: the backfill must not write the
// eval_finished_at latch for a transcript the judge failed on, or the next
// backfill would never pick it up again (Phase 0a item 2).
func TestRunBackfill_JudgeErrorDoesNotLatch(t *testing.T) {
	fdb := &fakeBackfillDB{transcripts: []*db.Transcript{{
		ID: "t-fail", JobID: "j-fail", FilePath: "/books/Dune/Ch9.m4b",
		RawText: "The spice must flow. Paul Atreides walked the sands of Arrakis.",
	}}}
	judge := evalpkg.NewJudge(errBackfillChat{})
	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, &config.Config{ChunkSize: 32, EvalGatesEmbed: true}, backfillOptions{write: true}); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}
	if len(fdb.evalMetrics) != 1 {
		t.Fatalf("want exactly one outcome record (the failure), got %d", len(fdb.evalMetrics))
	}
	for _, m := range fdb.evalMetrics {
		if !m.FinishedAt.IsZero() {
			t.Errorf("judge failed → eval_finished_at must not be latched (got %v, chunks=%d skipped=%d)",
				m.FinishedAt, m.Chunks, m.Skipped)
		}
	}
}

// flakyBackfillChat fails the first call and answers resp afterwards.
type flakyBackfillChat struct {
	resp  string
	calls *int
}

func (c flakyBackfillChat) Complete(context.Context, string, string) (string, error) {
	*c.calls++
	if *c.calls == 1 {
		return "", errors.New("transient glitch")
	}
	return c.resp, nil
}
func (flakyBackfillChat) Model() string { return "fake-backfill-judge" }

const longBackfillText = "The spice must flow. Paul Atreides walked the sands of Arrakis while the worm rose behind him and the Fremen watched from the rocks above the basin."

// A partial run records a failure (not the latch) and still stores the
// surviving findings.
func TestRunBackfill_PartialFailureRecordsFailure(t *testing.T) {
	fdb := &fakeBackfillDB{transcripts: []*db.Transcript{{
		ID: "t-part", JobID: "j-part", FilePath: "/books/Dune/Ch5.m4b", RawText: longBackfillText,
	}}}
	calls := 0
	judge := evalpkg.NewJudge(flakyBackfillChat{
		calls: &calls,
		resp:  `{"findings":[{"original_text":"spice","issue_type":"misheard_word","suggested_correction":"spies","confidence":0.9}]}`,
	})
	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, &config.Config{ChunkSize: 8, EvalGatesEmbed: true}, backfillOptions{write: true}); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}
	if len(fdb.evalMetrics) != 1 {
		t.Fatalf("want 1 outcome record, got %d", len(fdb.evalMetrics))
	}
	m := fdb.evalMetrics[0]
	if !m.Failed() || m.FailedChunks != 1 || !strings.Contains(m.Error, "transient glitch") {
		t.Errorf("want failure record (1 failed chunk, error kept), got %+v", m)
	}
	if !strings.Contains(out.String(), "left unlatched") {
		t.Errorf("report must say the transcript was left unlatched:\n%s", out.String())
	}
}

// --backfill-eval-errors re-judges the eval-error selection, skips findings an
// earlier partial run already recorded, and latches on success.
func TestRunBackfill_EvalErrorsReJudgesAndDedupes(t *testing.T) {
	tr := &db.Transcript{ID: "t-err", JobID: "j-err", FilePath: "/books/Dune/Ch6.m4b", RawText: "Fear is the mind killer."}
	chunkID := "11111111-1111-1111-1111-111111111111"
	fdb := &fakeBackfillDB{
		transcripts:    []*db.Transcript{{ID: "t-uneval", JobID: "j-uneval", FilePath: "/x.m4b", RawText: "unused"}},
		errTranscripts: []*db.Transcript{tr},
		stored: map[string][]db.EvalChunk{tr.ID: {{
			ChunkID: chunkID, TranscriptID: tr.ID, FilePath: tr.FilePath, Text: "Fear is the mind killer.",
		}}},
		existing: map[string]map[db.FindingKey]bool{tr.ID: {
			{ChunkID: chunkID, OriginalText: "Fear", IssueType: "misheard_word", SuggestedCorrection: "Fire"}: true,
		}},
	}
	judge := evalpkg.NewJudge(fakeBackfillChat{resp: `{"findings":[
		{"original_text":"Fear","issue_type":"misheard_word","suggested_correction":"Fire","confidence":0.9},
		{"original_text":"killer","issue_type":"misheard_word","suggested_correction":"filler","confidence":0.9}]}`})

	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, &config.Config{ChunkSize: 32},
		backfillOptions{mode: backfillEvalErrors, write: true}); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}
	if len(fdb.findings) != 1 || fdb.findings[0].OriginalText != "killer" {
		t.Fatalf("want only the new finding inserted, got %+v", fdb.findings)
	}
	if len(fdb.evalMetrics) != 1 || fdb.evalMetrics[0].Failed() || fdb.evalMetrics[0].JobID != "j-err" {
		t.Fatalf("want one latch for j-err (selection must be the eval-error one), got %+v", fdb.evalMetrics)
	}
	if !strings.Contains(out.String(), "1 already recorded") {
		t.Errorf("report should count the deduped finding:\n%s", out.String())
	}
	if got := fdb.evalMetrics[0].Findings; got != 1 {
		t.Errorf("eval_findings = %d, want 1 (only the findings this run recorded)", got)
	}
}

// The sweep walks the selection in keyset pages and honors --limit; a dry run
// (whose rows never leave the selection) still terminates.
func TestRunBackfill_PagesAndLimit(t *testing.T) {
	mk := func(i int) *db.Transcript {
		return &db.Transcript{ID: fmt.Sprintf("t%d", i), JobID: fmt.Sprintf("j%d", i), FilePath: fmt.Sprintf("/b/%d.m4b", i), RawText: "Fear is the mind killer."}
	}
	var all []*db.Transcript
	for i := range 5 {
		all = append(all, mk(i))
	}
	judge := evalpkg.NewJudge(fakeBackfillChat{resp: `{"findings":[]}`})

	t.Run("dry run walks every page", func(t *testing.T) {
		fdb := &fakeBackfillDB{transcripts: all}
		var out strings.Builder
		if err := runBackfill(context.Background(), &out, fdb, judge, &config.Config{ChunkSize: 32, EvalGatesEmbed: true},
			backfillOptions{pageSize: 2}); err != nil {
			t.Fatalf("runBackfill: %v", err)
		}
		if got := strings.Count(out.String(), "[dry-run]"); got != 5 {
			t.Errorf("judged %d transcripts in dry run, want 5:\n%s", got, out.String())
		}
		if fmt.Sprint(fdb.pageLimits) != "[2 2 2]" {
			t.Errorf("page limits = %v, want [2 2 2] (bounded pages)", fdb.pageLimits)
		}
		if len(fdb.evalMetrics) != 0 {
			t.Errorf("dry run wrote %d metrics", len(fdb.evalMetrics))
		}
	})

	t.Run("limit caps the run", func(t *testing.T) {
		fdb := &fakeBackfillDB{transcripts: all}
		var out strings.Builder
		if err := runBackfill(context.Background(), &out, fdb, judge, &config.Config{ChunkSize: 32, EvalGatesEmbed: true},
			backfillOptions{write: true, limit: 3, pageSize: 2}); err != nil {
			t.Fatalf("runBackfill: %v", err)
		}
		if len(fdb.evalMetrics) != 3 {
			t.Errorf("latched %d transcripts, want --limit 3", len(fdb.evalMetrics))
		}
		// Full pages: skipped/failed rows don't count toward --limit, so the
		// remaining limit can't size the page. The run stops mid-page.
		if fmt.Sprint(fdb.pageLimits) != "[2 2]" {
			t.Errorf("page limits = %v, want [2 2]", fdb.pageLimits)
		}
	})
}

// Decoupled mode (EVAL_IN_PIPELINE=false): the worker embeds without judging,
// so the transcript is embedded but never latched. --backfill-unevaluated is the
// pass that judges it — against its STORED chunk rows (their real IDs) — and
// latches it.
func TestRunBackfill_JudgesTranscriptsEmbeddedWithoutEval(t *testing.T) {
	tr := &db.Transcript{ID: "t-emb", JobID: "j-emb", FilePath: "/books/Dune/Ch7.m4b", RawText: "Fear is the mind killer."}
	storedID := "22222222-2222-2222-2222-222222222222" // random ID from the ungated embed
	fdb := &fakeBackfillDB{
		transcripts: []*db.Transcript{tr},
		stored: map[string][]db.EvalChunk{tr.ID: {{
			ChunkID: storedID, TranscriptID: tr.ID, TranscriptionRunID: tr.JobID,
			FilePath: tr.FilePath, Text: "Fear is the mind killer.",
		}}},
	}
	judge := evalpkg.NewJudge(fakeBackfillChat{resp: `{"findings":[{"original_text":"killer","issue_type":"misheard_word","suggested_correction":"filler","confidence":0.9}]}`})
	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, &config.Config{ChunkSize: 32}, backfillOptions{write: true}); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}
	if len(fdb.findings) != 1 || fdb.findings[0].ChunkID == nil || *fdb.findings[0].ChunkID != storedID {
		t.Fatalf("finding must reference the stored chunk row %s, got %+v", storedID, fdb.findings)
	}
	if len(fdb.evalMetrics) != 1 || fdb.evalMetrics[0].Failed() || fdb.evalMetrics[0].JobID != "j-emb" {
		t.Fatalf("want the transcript latched, got %+v", fdb.evalMetrics)
	}
}

// Ungated (EVAL_GATES_EMBED=false) deployments assign RANDOM chunk IDs when the
// embed worker inserts chunks, so a transcript with no stored chunks cannot be
// judged yet: regenerated UUIDv5 IDs would never exist (orphaned findings, no
// FK) and the latch would stop it from ever being judged against its real
// chunks. It must be skipped and left unlatched.
func TestRunBackfill_UngatedSkipsNotYetEmbedded(t *testing.T) {
	calls := 0
	fdb := &fakeBackfillDB{transcripts: []*db.Transcript{{
		ID: "t-pending", JobID: "j-pending", FilePath: "/books/Dune/Ch8.m4b", RawText: "Fear is the mind killer.",
	}}}
	judge := evalpkg.NewJudge(flakyBackfillChat{calls: &calls, resp: `{"findings":[]}`})
	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb, judge, &config.Config{ChunkSize: 32}, backfillOptions{write: true}); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}
	if calls != 0 {
		t.Errorf("judge called %d times; a not-yet-embedded transcript must not be judged when ungated", calls)
	}
	if len(fdb.findings) != 0 || len(fdb.evalMetrics) != 0 {
		t.Errorf("must write nothing (no orphan findings, no latch): findings=%d metrics=%d", len(fdb.findings), len(fdb.evalMetrics))
	}
	if !strings.Contains(out.String(), "not embedded yet") {
		t.Errorf("report should explain the skip:\n%s", out.String())
	}

	// Same transcript under the gate: the embed pass will insert the
	// deterministic IDs, so it is judged and latched.
	fdb = &fakeBackfillDB{transcripts: fdb.transcripts}
	if err := runBackfill(context.Background(), &out, fdb, evalpkg.NewJudge(fakeBackfillChat{resp: `{"findings":[]}`}),
		&config.Config{ChunkSize: 32, EvalGatesEmbed: true}, backfillOptions{write: true}); err != nil {
		t.Fatalf("runBackfill (gated): %v", err)
	}
	if len(fdb.evalMetrics) != 1 || fdb.evalMetrics[0].Failed() {
		t.Errorf("gated: want the transcript latched, got %+v", fdb.evalMetrics)
	}
}

// poisonChat fails every call whose prompt contains needle and answers an
// empty findings list otherwise — a transcript that always fails to judge.
type poisonChat struct{ needle string }

func (c poisonChat) Complete(_ context.Context, _, user string) (string, error) {
	if strings.Contains(user, c.needle) {
		return "", errors.New("poisoned chunk")
	}
	return `{"findings":[]}`, nil
}
func (poisonChat) Model() string { return "fake-backfill-judge" }

// stalledHead builds the selection that stalled every --limit run: a head of
// rows that can never latch (empty raw text, not embedded yet under the
// ungated config, a transcript whose judge call always fails) followed by
// judgeable ones.
func stalledHead() *fakeBackfillDB {
	embedded := func(id string) []db.EvalChunk {
		return []db.EvalChunk{{ChunkID: "c-" + id, TranscriptID: id, FilePath: "/b/" + id + ".m4b", Text: "text of " + id}}
	}
	fdb := &fakeBackfillDB{stored: map[string][]db.EvalChunk{}}
	for _, id := range []string{"t-empty", "t-unembedded", "t-poison", "t-ok1", "t-ok2", "t-ok3"} {
		tr := &db.Transcript{ID: id, JobID: "j-" + id, FilePath: "/b/" + id + ".m4b", RawText: "x"}
		switch id {
		case "t-empty":
			tr.RawText = ""
		case "t-unembedded":
			// no stored chunks + ungated config → skipped as not embedded
		default:
			fdb.stored[id] = embedded(id)
		}
		fdb.transcripts = append(fdb.transcripts, tr)
	}
	return fdb
}

// TestRunBackfill_LimitCountsOnlySuccesses is the stall regression: --limit
// used to count every visited row, so a head of rows that never latch used up
// the limit on every run and the backfill made no progress. The limit must
// count only transcripts judged successfully; skipped/failed rows are visited
// once (keyset cursor) and passed over.
func TestRunBackfill_LimitCountsOnlySuccesses(t *testing.T) {
	cfg := &config.Config{ChunkSize: 32} // ungated: t-unembedded is skipped
	judge := evalpkg.NewJudge(poisonChat{needle: "text of t-poison"})

	t.Run("write", func(t *testing.T) {
		fdb := stalledHead()
		var out strings.Builder
		if err := runBackfill(context.Background(), &out, fdb, judge, cfg,
			backfillOptions{write: true, limit: 2, pageSize: 2}); err != nil {
			t.Fatalf("runBackfill: %v", err)
		}
		var latched, failedJobs []string
		for _, m := range fdb.evalMetrics {
			if m.Failed() {
				failedJobs = append(failedJobs, m.JobID)
			} else {
				latched = append(latched, m.JobID)
			}
		}
		if fmt.Sprint(latched) != "[j-t-ok1 j-t-ok2]" {
			t.Errorf("latched %v, want the 2 judgeable transcripts past the stuck head", latched)
		}
		if fmt.Sprint(failedJobs) != "[j-t-poison]" {
			t.Errorf("failure records %v, want exactly one for t-poison (visited once)", failedJobs)
		}
		// Stops at the limit: t-ok3 is never judged.
		if strings.Contains(out.String(), "t-ok3") {
			t.Errorf("run went past --limit:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "Stopped at --limit 2") {
			t.Errorf("report should say the limit stopped the run:\n%s", out.String())
		}
	})

	t.Run("dry run counts would-latch", func(t *testing.T) {
		fdb := stalledHead()
		var out strings.Builder
		if err := runBackfill(context.Background(), &out, fdb, judge, cfg,
			backfillOptions{limit: 1, pageSize: 2}); err != nil {
			t.Fatalf("runBackfill: %v", err)
		}
		s := out.String()
		if !strings.Contains(s, "t-ok1.m4b") || strings.Contains(s, "t-ok2") {
			t.Errorf("dry run with --limit 1 must preview exactly the first judgeable transcript:\n%s", s)
		}
		if len(fdb.evalMetrics) != 0 || len(fdb.findings) != 0 {
			t.Errorf("dry run wrote: metrics=%d findings=%d", len(fdb.evalMetrics), len(fdb.findings))
		}
	})
}

// TestRunBackfill_LimitTerminatesWhenExhausted: when nothing in the selection
// can latch, a --limit run still ends — at the end of the selection, each row
// visited once — instead of re-selecting the same head.
func TestRunBackfill_LimitTerminatesWhenExhausted(t *testing.T) {
	fdb := stalledHead()
	fdb.transcripts = fdb.transcripts[:3] // only the never-latching head
	var out strings.Builder
	if err := runBackfill(context.Background(), &out, fdb,
		evalpkg.NewJudge(poisonChat{needle: "text of t-poison"}), &config.Config{ChunkSize: 32},
		backfillOptions{write: true, limit: 5, pageSize: 2}); err != nil {
		t.Fatalf("runBackfill: %v", err)
	}
	if fmt.Sprint(fdb.pageLimits) != "[2 2]" {
		t.Errorf("page requests = %v, want [2 2] (a full page, then the short last page)", fdb.pageLimits)
	}
	if len(fdb.evalMetrics) != 1 || !fdb.evalMetrics[0].Failed() {
		t.Errorf("want only t-poison's failure record, got %+v", fdb.evalMetrics)
	}
	if !strings.Contains(out.String(), "3 done transcript(s) with eval_finished_at IS NULL visited") {
		t.Errorf("each row must be visited exactly once:\n%s", out.String())
	}
}

// TestRunBackfill_OutageStopsLimitedRun: --limit no longer counts failures, so
// a judge outage must not turn a bounded run into a sweep of the whole
// selection. After maxConsecutiveJudgeOutages transcripts fail on EVERY chunk
// the run stops with an error.
func TestRunBackfill_OutageStopsLimitedRun(t *testing.T) {
	fdb := &fakeBackfillDB{stored: map[string][]db.EvalChunk{}}
	for i := range maxConsecutiveJudgeOutages + 3 {
		id := fmt.Sprintf("t%d", i)
		fdb.transcripts = append(fdb.transcripts, &db.Transcript{ID: id, JobID: "j" + id, FilePath: "/b/" + id + ".m4b", RawText: "x"})
		fdb.stored[id] = []db.EvalChunk{{ChunkID: "c" + id, TranscriptID: id, Text: "text " + id}}
	}
	var out strings.Builder
	err := runBackfill(context.Background(), &out, fdb, evalpkg.NewJudge(errBackfillChat{}),
		&config.Config{ChunkSize: 32}, backfillOptions{write: true, limit: 2, pageSize: 4})
	if err == nil || !strings.Contains(err.Error(), "judge outage") {
		t.Fatalf("want judge outage error, got %v", err)
	}
	if len(fdb.evalMetrics) != maxConsecutiveJudgeOutages {
		t.Errorf("judged %d transcripts, want to stop after %d consecutive outages", len(fdb.evalMetrics), maxConsecutiveJudgeOutages)
	}

	// Without --limit the operator asked for the whole selection: no breaker.
	fdb.evalMetrics = nil
	fdb.pageLimits = nil
	if err := runBackfill(context.Background(), &out, fdb, evalpkg.NewJudge(errBackfillChat{}),
		&config.Config{ChunkSize: 32}, backfillOptions{write: true, pageSize: 4}); err != nil {
		t.Fatalf("unlimited run: %v", err)
	}
	if len(fdb.evalMetrics) != len(fdb.transcripts) {
		t.Errorf("unlimited run judged %d of %d", len(fdb.evalMetrics), len(fdb.transcripts))
	}
}
