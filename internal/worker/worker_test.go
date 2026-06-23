package worker

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/openai"
	"github.com/jedwards1230/earmark/internal/queue"
	"github.com/stretchr/testify/require"
)

// ─── Worker construction ─────────────────────────────────────────────────────

func TestWorkerCreation(t *testing.T) {
	q := &queue.Queue{}
	cfg := &config.Config{ChunkSize: 512}

	fakeDB := &fakeDB{}
	w := NewWorker(q, fakeDB, cfg)

	require.NotNil(t, w)
	require.Equal(t, q, w.queue)
	require.NotNil(t, w.ctx)
	require.NotNil(t, w.cancel)
}

// ─── processTranscript tests ─────────────────────────────────────────────────

// fakeDB implements DBInterface for unit tests.
//
// mu guards the fields mutated by DBInterface methods (chunks,
// getCompletedCalls, …). The single-goroutine processTranscript tests touch
// these fields directly and never contend; only the Start-loop tests
// (TestStart_*) run the worker on a separate goroutine, so they read state
// through the locked snapshot helpers below to stay race-free.
type fakeDB struct {
	mu          sync.Mutex
	transcripts []*db.Transcript
	chunks      []db.Chunk
	embedErr    error
	insertErr   error
	usage       openai.EmbeddingUsage // returned by GetEmbeddingsWithUsage
	metrics     []db.EmbedMetrics     // captured by UpsertEmbedMetrics
	metricsErr  error
	evalMetrics []db.EvalMetrics   // captured by UpsertEvalMetrics
	evalMetErr  error              // error returned by UpsertEvalMetrics
	findings    []db.Finding       // captured by InsertFindings (in-pipeline eval)
	findingsErr error              // error returned by InsertFindings
	events      []db.PipelineEvent // captured by AppendEvent
	staleFailed int                // returned by RecoverStaleJobs (newly-failed count)
	// phase is returned by GetPipelinePhase. The zero value ("") normalizes to
	// "idle" so existing tests are unaffected. phaseErr forces a read error.
	phase    string
	phaseErr error
	// getCompletedCalls counts GetCompletedTranscripts invocations so the phase
	// gate can be observed: in the "transcribe" phase the worker idles BEFORE
	// polling, so the count stays 0.
	getCompletedCalls int
}

func (f *fakeDB) GetCompletedTranscripts(_ context.Context) ([]*db.Transcript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCompletedCalls++
	return f.transcripts, nil
}

// GetUnevaluatedTranscripts stubs the eval-pass selection: returns transcripts
// from the fakeDB's transcript slice that have no evaluatedIDs entry. For tests
// that don't exercise the gated flow, it returns the same slice as
// GetCompletedTranscripts (the ungated path is tested via GetCompletedTranscripts).
func (f *fakeDB) GetUnevaluatedTranscripts(_ context.Context) ([]*db.Transcript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transcripts, nil
}

// GetEvaluatedUnembeddedTranscripts stubs the embed-pass selection. In the gated
// flow the embed pass picks up transcripts after they have been eval'd; in tests
// we return an empty slice by default so callers that call this without a gated
// scenario don't accidentally embed a second time.
func (f *fakeDB) GetEvaluatedUnembeddedTranscripts(_ context.Context) ([]*db.Transcript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return nil, nil
}

func (f *fakeDB) InsertChunks(_ context.Context, chunks []db.Chunk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	f.chunks = append(f.chunks, chunks...)
	return nil
}

// chunkCount and completedCalls are locked snapshot helpers for the Start-loop
// tests, which observe fakeDB state from a goroutine other than the worker's.
func (f *fakeDB) chunkCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.chunks)
}

func (f *fakeDB) completedCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCompletedCalls
}

func (f *fakeDB) EmbedDocuments(texts []string) ([][]float32, error) {
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	// Return a zero 768-dim vector per text so the worker is satisfied.
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = make([]float32, 768)
	}
	return result, nil
}

func (f *fakeDB) EmbedDocumentsWithUsage(texts []string) ([][]float32, openai.EmbeddingUsage, error) {
	vecs, err := f.EmbedDocuments(texts)
	return vecs, f.usage, err
}

func (f *fakeDB) UpsertEmbedMetrics(_ context.Context, m db.EmbedMetrics) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.metricsErr != nil {
		return f.metricsErr
	}
	f.metrics = append(f.metrics, m)
	return nil
}

func (f *fakeDB) UpsertEvalMetrics(_ context.Context, m db.EvalMetrics) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.evalMetErr != nil {
		return f.evalMetErr
	}
	f.evalMetrics = append(f.evalMetrics, m)
	return nil
}

func (f *fakeDB) AppendEvent(_ context.Context, e db.PipelineEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakeDB) RecoverStaleJobs(_ context.Context, _ time.Duration) (int, error) {
	return f.staleFailed, nil
}

func (f *fakeDB) GetPipelinePhase(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.phaseErr != nil {
		return db.PhaseIdle, f.phaseErr
	}
	if f.phase == "" {
		return db.PhaseIdle, nil
	}
	return f.phase, nil
}

func (f *fakeDB) InsertFindings(_ context.Context, findings []db.Finding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findingsErr != nil {
		return f.findingsErr
	}
	f.findings = append(f.findings, findings...)
	return nil
}

// workerFakeChat implements eval.ChatClient so a test can inject a judge into
// the worker without a real endpoint.
type workerFakeChat struct{ resp string }

func (c workerFakeChat) Complete(_ context.Context, _, _ string) (string, error) {
	return c.resp, nil
}
func (c workerFakeChat) Model() string { return "fake-judge" }

// flakyChat errors on the first Complete call, then returns resp — used to
// exercise the transient per-chunk judge error (soft-fail) path.
type flakyChat struct {
	resp  string
	calls int
}

func (c *flakyChat) Complete(_ context.Context, _, _ string) (string, error) {
	c.calls++
	if c.calls == 1 {
		return "", fmt.Errorf("transient judge glitch")
	}
	return c.resp, nil
}
func (c *flakyChat) Model() string { return "flaky-judge" }

func TestProcessTranscript_Success(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{
		ctx: context.Background(),
		db:  fdb,
		log: log.NewLogger("worker-test"),
	}

	cfg := &config.Config{ChunkSize: 10}
	transcript := &db.Transcript{
		ID:       "tid-1",
		FilePath: "/books/author/title/ch1.mp3",
		RawText:  "Hello world this is a test transcript for chunking.",
	}

	err := w.processTranscript(cfg, transcript)
	require.NoError(t, err)
	require.NotEmpty(t, fdb.chunks)
	for _, c := range fdb.chunks {
		require.Equal(t, "tid-1", c.TranscriptID)
		require.Equal(t, "/books/author/title/ch1.mp3", c.FilePath)
		require.Len(t, c.Embedding, 768)
	}
}

func TestProcessTranscript_InlineEvalWritesFindingsLinkedToChunks(t *testing.T) {
	// With a judge set (EvalInPipeline path), processTranscript evaluates the
	// chunks before embedding, persists findings, AND still embeds. Each finding
	// must reference a chunk_id that matches an inserted chunk (the pre-assigned
	// UUID), proving eval-before-embed linkage.
	fdb := &fakeDB{}
	judge := eval.NewJudge(workerFakeChat{
		resp: `{"findings":[{"original_text":"hello world","issue_type":"misheard_word","suggested_correction":"hello word","confidence":0.9}]}`,
	})
	w := &Worker{
		ctx:   context.Background(),
		db:    fdb,
		log:   log.NewLogger("worker-test"),
		judge: judge,
	}

	cfg := &config.Config{ChunkSize: 10}
	transcript := &db.Transcript{
		ID:       "tid-eval",
		JobID:    "job-eval",
		FilePath: "/books/author/title/ch1.mp3",
		RawText:  "Hello world this is a test transcript for chunking.",
	}

	require.NoError(t, w.processTranscript(cfg, transcript))

	// Embedding still happened.
	require.NotEmpty(t, fdb.chunks)
	// Eval ran and persisted findings.
	require.NotEmpty(t, fdb.findings, "in-pipeline eval should have written findings")

	chunkIDs := map[string]bool{}
	for _, c := range fdb.chunks {
		require.NotEmpty(t, c.ID, "chunk should have a pre-assigned UUID")
		chunkIDs[c.ID] = true
	}
	for _, f := range fdb.findings {
		require.NotNil(t, f.ChunkID)
		require.True(t, chunkIDs[*f.ChunkID],
			"finding chunk_id %q must match an inserted chunk", *f.ChunkID)
		require.Equal(t, "job-eval", *f.TranscriptionRunID)
	}

	// Eval metrics recorded for the job (CONTRACT §1.5 eval slice).
	require.Len(t, fdb.evalMetrics, 1, "in-pipeline eval should record exactly one eval-metrics row")
	em := fdb.evalMetrics[0]
	require.Equal(t, "job-eval", em.JobID)
	require.Equal(t, "fake-judge", em.Model, "eval_model comes from the judge's chat model")
	require.Equal(t, len(fdb.findings), em.Findings)
	require.GreaterOrEqual(t, em.Chunks, 1, "at least one chunk evaluated")
	require.False(t, em.FinishedAt.IsZero(), "eval_finished_at is the completion marker — must be set")
	require.False(t, em.FinishedAt.Before(em.StartedAt), "finished must be >= started")

	// Audit events: embed start+finish AND eval start+finish (CONTRACT §1.7).
	stages := map[string]int{}
	for _, e := range fdb.events {
		stages[e.Stage+"/"+e.Event]++
		require.Equal(t, "job-eval", e.JobID, "events must carry the job id")
	}
	require.Equal(t, 1, stages["embed/start"], "expected one embed start event")
	require.Equal(t, 1, stages["embed/finish"], "expected one embed finish event")
	require.Equal(t, 1, stages["eval/start"], "expected one eval start event")
	require.Equal(t, 1, stages["eval/finish"], "expected one eval finish event")
}

func TestProcessTranscript_NoJudgeRecordsNoEvalMetrics(t *testing.T) {
	// Without a judge, no eval metrics row is written (the eval slice stays NULL).
	fdb := &fakeDB{}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}
	transcript := &db.Transcript{ID: "tid", JobID: "job", FilePath: "/b/a/t/ch.mp3", RawText: "Hello world test."}
	require.NoError(t, w.processTranscript(&config.Config{ChunkSize: 10}, transcript))
	require.Empty(t, fdb.evalMetrics, "no judge → no eval-metrics row")
}

func TestProcessTranscript_EvalMetricsWriteFailureStillEmbeds(t *testing.T) {
	// A best-effort eval-metrics write failure must not fail the embed.
	fdb := &fakeDB{evalMetErr: fmt.Errorf("run_metrics down")}
	judge := eval.NewJudge(workerFakeChat{
		resp: `{"findings":[]}`,
	})
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"), judge: judge}
	transcript := &db.Transcript{ID: "tid", JobID: "job", FilePath: "/b/a/t/ch.mp3", RawText: "Hello world this is a test."}
	require.NoError(t, w.processTranscript(&config.Config{ChunkSize: 10}, transcript),
		"an eval-metrics write failure must not fail the embed")
	require.NotEmpty(t, fdb.chunks, "chunks must still be inserted")
}

func TestProcessTranscript_NoJudgeSkipsEval(t *testing.T) {
	// Default (judge nil): no findings written, chunks still inserted — behavior
	// identical to before the in-pipeline eval was added.
	fdb := &fakeDB{}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}
	transcript := &db.Transcript{ID: "tid-noeval", FilePath: "/b/a/t/ch.mp3", RawText: "Hello world test."}
	require.NoError(t, w.processTranscript(&config.Config{ChunkSize: 10}, transcript))
	require.NotEmpty(t, fdb.chunks)
	require.Empty(t, fdb.findings, "no judge → no findings")
}

func TestProcessTranscript_EmbedFailureDoesNotPersistFindings(t *testing.T) {
	// The orphan-prevention guarantee: when embedding fails, the findings the
	// judge computed must NOT be persisted (they'd reference chunk UUIDs never
	// inserted). processTranscript returns the embed error and the findings table
	// stays empty.
	fdb := &fakeDB{embedErr: fmt.Errorf("ollama offline")}
	judge := eval.NewJudge(workerFakeChat{
		resp: `{"findings":[{"original_text":"hello world","issue_type":"misheard_word","suggested_correction":"hello word","confidence":0.9}]}`,
	})
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"), judge: judge}
	transcript := &db.Transcript{ID: "tid-embedfail", JobID: "job-embedfail", FilePath: "/b/a/t/ch.mp3", RawText: "Hello world this is a test."}

	require.Error(t, w.processTranscript(&config.Config{ChunkSize: 10}, transcript))
	require.Empty(t, fdb.chunks, "embed failed → no chunks inserted")
	require.Empty(t, fdb.findings, "embed failed → findings must not be persisted (no orphans)")
}

func TestProcessTranscript_FindingsWriteFailureStillEmbeds(t *testing.T) {
	// A findings-write failure is best-effort: chunks are already inserted and the
	// transcript is considered embedded (no error returned) — the safe direction
	// (searchable-but-unflagged), never a failed embed.
	fdb := &fakeDB{findingsErr: fmt.Errorf("findings table down")}
	judge := eval.NewJudge(workerFakeChat{
		resp: `{"findings":[{"original_text":"hello world","issue_type":"misheard_word","suggested_correction":"hello word","confidence":0.9}]}`,
	})
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"), judge: judge}
	transcript := &db.Transcript{ID: "tid-findfail", JobID: "job-findfail", FilePath: "/b/a/t/ch.mp3", RawText: "Hello world this is a test."}

	require.NoError(t, w.processTranscript(&config.Config{ChunkSize: 10}, transcript),
		"a findings-write failure must not fail the embed")
	require.NotEmpty(t, fdb.chunks, "chunks must still be inserted")
	require.Empty(t, fdb.findings, "findings write failed → none captured")
}

func TestNewWorker_EvalInPipelineNoEndpointLeavesJudgeNil(t *testing.T) {
	// EVAL_IN_PIPELINE on but no eval endpoint resolves → judge stays nil
	// (non-fatal fallback), and the worker still embeds normally.
	t.Setenv("EVAL_CHAT_BASE_URL", "")
	t.Setenv("EVAL_CHAT_MODEL", "")
	cfg := &config.Config{ChunkSize: 10, EvalInPipeline: true}
	w := NewWorker(&queue.Queue{}, &fakeDB{}, cfg)
	require.Nil(t, w.judge, "no eval endpoint → judge must be nil, not a panic/fatal")

	// And processing still works (no eval, chunks embedded).
	fdb := &fakeDB{}
	w.db = fdb
	transcript := &db.Transcript{ID: "tid-nojudge", FilePath: "/b/a/t/ch.mp3", RawText: "Hello world test transcript."}
	require.NoError(t, w.processTranscript(cfg, transcript))
	require.NotEmpty(t, fdb.chunks)
	require.Empty(t, fdb.findings)
}

func TestProcessTranscript_TransientJudgeErrorSkipsChunkAndPersistsRest(t *testing.T) {
	// A per-chunk judge error is soft-fail: that chunk is skipped, the run
	// continues, partial findings are persisted, and embedding is unaffected.
	fdb := &fakeDB{}
	judge := eval.NewJudge(&flakyChat{
		resp: `{"findings":[{"original_text":"hello world","issue_type":"misheard_word","suggested_correction":"hello word","confidence":0.9}]}`,
	})
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"), judge: judge}
	cfg := &config.Config{ChunkSize: 8}
	// Long enough to split into several chunks so the first (failing) chunk is a
	// strict subset — the survivors still yield findings.
	transcript := &db.Transcript{
		ID: "tid-flaky", JobID: "job-flaky", FilePath: "/b/a/t/ch.mp3",
		RawText: "Hello world this is a fairly long test transcript with plenty of words so the token chunker emits multiple chunks for the judge to evaluate one by one.",
	}

	require.NoError(t, w.processTranscript(cfg, transcript),
		"a transient judge error must not fail the transcript")
	require.Greater(t, len(fdb.chunks), 1, "expected multiple chunks for this test")
	require.NotEmpty(t, fdb.findings, "surviving chunks should still yield persisted findings")
	require.Less(t, len(fdb.findings), len(fdb.chunks),
		"the first chunk's judge error should have skipped its finding (partial result)")
}

func TestProcessTranscript_RecordsEmbedMetrics(t *testing.T) {
	// Ollama-style: no provider usage reported. embed_total_tokens must come from
	// the local tokenizer (non-zero), and embed_prompt_tokens must be nil.
	fdb := &fakeDB{usage: openai.EmbeddingUsage{}}
	w := &Worker{
		ctx: context.Background(),
		db:  fdb,
		log: log.NewLogger("worker-test"),
	}
	cfg := &config.Config{ChunkSize: 10, EmbeddingsModel: "nomic-embed-text"}
	transcript := &db.Transcript{
		ID:       "tid-metrics",
		JobID:    "job-metrics",
		FilePath: "/books/author/title/ch1.mp3",
		RawText:  "Hello world this is a test transcript for chunking.",
	}

	require.NoError(t, w.processTranscript(cfg, transcript))
	require.Len(t, fdb.metrics, 1)
	m := fdb.metrics[0]
	require.Equal(t, "job-metrics", m.JobID)
	require.Equal(t, "nomic-embed-text", m.Model)
	require.Equal(t, len(fdb.chunks), m.ChunkCount)
	require.NotNil(t, m.TotalTokens, "all chunks tokenized → total is known (non-nil)")
	require.Greater(t, *m.TotalTokens, 0, "local token count must be authoritative and non-zero")
	require.Nil(t, m.PromptTokens, "no provider usage → prompt tokens nil")
	require.False(t, m.FinishedAt.Before(m.StartedAt))
}

func TestProcessTranscript_EmbedMetricsPromptTokensFromProvider(t *testing.T) {
	// When the provider does report usage (non-Ollama), prompt tokens are stored.
	fdb := &fakeDB{usage: openai.EmbeddingUsage{PromptTokens: 42, TotalTokens: 42}}
	w := &Worker{
		ctx: context.Background(),
		db:  fdb,
		log: log.NewLogger("worker-test"),
	}
	cfg := &config.Config{ChunkSize: 10, EmbeddingsModel: "nomic-embed-text"}
	transcript := &db.Transcript{
		ID: "tid-mp", JobID: "job-mp",
		FilePath: "/books/a/t/ch.mp3",
		RawText:  "Hello world this is a test transcript for chunking.",
	}

	require.NoError(t, w.processTranscript(cfg, transcript))
	require.Len(t, fdb.metrics, 1)
	require.NotNil(t, fdb.metrics[0].PromptTokens)
	require.Equal(t, 42, *fdb.metrics[0].PromptTokens)
}

func TestProcessTranscript_WithSegments(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{
		ctx: context.Background(),
		db:  fdb,
		log: log.NewLogger("worker-test"),
	}

	sp := "SPEAKER_00"
	cfg := &config.Config{ChunkSize: 20}
	transcript := &db.Transcript{
		ID:       "tid-seg",
		FilePath: "/books/author/title/ch2.mp3",
		RawText:  "Hello world. Goodbye world.",
		Segments: []db.Segment{
			{ID: 0, Start: 0.0, End: 2.5, Text: "Hello world.", Speaker: &sp},
			{ID: 1, Start: 2.5, End: 5.0, Text: "Goodbye world.", Speaker: &sp},
		},
	}

	err := w.processTranscript(cfg, transcript)
	require.NoError(t, err)
	require.NotEmpty(t, fdb.chunks)
	// Each chunk must have start/end timestamps from segments.
	for _, c := range fdb.chunks {
		require.Equal(t, "tid-seg", c.TranscriptID)
		require.Equal(t, "/books/author/title/ch2.mp3", c.FilePath)
		require.GreaterOrEqual(t, c.EndSec, c.StartSec)
	}
}

func TestProcessTranscript_EmptyText(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{
		ctx: context.Background(),
		db:  fdb,
		log: log.NewLogger("worker-test"),
	}
	cfg := &config.Config{ChunkSize: 512}
	transcript := &db.Transcript{ID: "tid-2", RawText: ""}

	err := w.processTranscript(cfg, transcript)
	require.Error(t, err)
}

func TestProcessTranscript_EmbedError(t *testing.T) {
	fdb := &fakeDB{embedErr: fmt.Errorf("ollama down")}
	w := &Worker{
		ctx: context.Background(),
		db:  fdb,
		log: log.NewLogger("worker-test"),
	}
	cfg := &config.Config{ChunkSize: 512}
	transcript := &db.Transcript{ID: "tid-3", RawText: "some text"}

	err := w.processTranscript(cfg, transcript)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ollama down")
}

// M-6: test InsertChunks failure path.
func TestProcessTranscript_InsertError(t *testing.T) {
	fdb := &fakeDB{insertErr: fmt.Errorf("db write failed")}
	w := &Worker{
		ctx: context.Background(),
		db:  fdb,
		log: log.NewLogger("worker-test"),
	}
	cfg := &config.Config{ChunkSize: 512}
	transcript := &db.Transcript{ID: "tid-4", RawText: "some text to embed and then fail"}

	err := w.processTranscript(cfg, transcript)
	require.Error(t, err)
	require.Contains(t, err.Error(), "db write failed")
}

// TestProcessTranscript_MetricsWriteError verifies that a UpsertEmbedMetrics
// failure is best-effort: the transcript processing still SUCCEEDS (chunks are
// written) and the error does not propagate to the caller.
func TestProcessTranscript_MetricsWriteError(t *testing.T) {
	fdb := &fakeDB{metricsErr: fmt.Errorf("boom")}
	w := &Worker{
		ctx: context.Background(),
		db:  fdb,
		log: log.NewLogger("worker-test"),
	}
	cfg := &config.Config{ChunkSize: 10, EmbeddingsModel: "nomic-embed-text"}
	transcript := &db.Transcript{
		ID:       "tid-merr",
		JobID:    "job-merr",
		FilePath: "/books/author/title/ch1.mp3",
		RawText:  "Hello world this is a test transcript for chunking.",
	}

	// The call must succeed — metrics failure is swallowed.
	err := w.processTranscript(cfg, transcript)
	require.NoError(t, err, "metrics write error must not propagate")

	// Chunks must still be written.
	require.NotEmpty(t, fdb.chunks, "chunks must be inserted despite metrics error")
}

// M-6: embed error case with segments present.
func TestProcessTranscript_EmbedErrorWithSegments(t *testing.T) {
	fdb := &fakeDB{embedErr: fmt.Errorf("ollama timeout")}
	w := &Worker{
		ctx: context.Background(),
		db:  fdb,
		log: log.NewLogger("worker-test"),
	}
	sp := "SPEAKER_00"
	cfg := &config.Config{ChunkSize: 512}
	transcript := &db.Transcript{
		ID:      "tid-5",
		RawText: "segment text",
		Segments: []db.Segment{
			{ID: 0, Start: 0.0, End: 1.0, Text: "segment text", Speaker: &sp},
		},
	}

	err := w.processTranscript(cfg, transcript)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ollama timeout")
}

// ─── Phase gate (CONTRACT §1.4) ──────────────────────────────────────────────

// startWorkerWith spins up a Worker over the given fakeDB and runs its Start
// loop in a goroutine. The caller gets the worker (to Stop) and is responsible
// for stopping it. The fake's first cycle runs immediately (no initial sleep),
// so a short wait + Stop deterministically observes exactly one cycle's effect.
func startWorkerWith(fdb *fakeDB) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
		db:     fdb,
		log:    log.NewLogger("worker-test"),
	}
	go w.Start(&config.Config{ChunkSize: 10})
	return w
}

func TestStart_TranscribePhaseSkipsProcessing(t *testing.T) {
	// phase=="transcribe": the worker idles — it must NOT poll for completed
	// transcripts and must NOT insert any chunks, even though one is available.
	fdb := &fakeDB{
		phase: db.PhaseTranscribe,
		transcripts: []*db.Transcript{
			{ID: "tid-skip", FilePath: "/b/a/t/ch.mp3", RawText: "Hello world test transcript."},
		},
	}
	w := startWorkerWith(fdb)
	time.Sleep(150 * time.Millisecond) // let the first cycle run + hit the gate
	w.Stop()

	require.Zero(t, fdb.completedCalls(), "transcribe phase must idle before polling transcripts")
	require.Zero(t, fdb.chunkCount(), "transcribe phase must not process/insert chunks")
}

func TestStart_IdleAndAnalyzePhasesProcess(t *testing.T) {
	// For idle (default ""), explicit "idle", "analyze", the worker processes as
	// today: it polls and embeds the available transcript.
	for _, phase := range []string{"", db.PhaseIdle, db.PhaseAnalyze} {
		t.Run("phase="+phase, func(t *testing.T) {
			fdb := &fakeDB{
				phase: phase,
				transcripts: []*db.Transcript{
					{ID: "tid-go", FilePath: "/b/a/t/ch.mp3", RawText: "Hello world test transcript."},
				},
			}
			w := startWorkerWith(fdb)
			require.Eventually(t, func() bool {
				return fdb.chunkCount() > 0
			}, 2*time.Second, 10*time.Millisecond,
				"phase %q must process transcripts (chunks inserted)", phase)
			w.Stop()
			require.Positive(t, fdb.completedCalls(), "phase %q must poll for transcripts", phase)
		})
	}
}

func TestStart_PhaseReadErrorDefaultsToProcessing(t *testing.T) {
	// A phase-read DB error must default to idle (process) — never wedge the
	// worker. The transcript is still polled and embedded.
	fdb := &fakeDB{
		phaseErr: fmt.Errorf("phase column unavailable"),
		transcripts: []*db.Transcript{
			{ID: "tid-err", FilePath: "/b/a/t/ch.mp3", RawText: "Hello world test transcript."},
		},
	}
	w := startWorkerWith(fdb)
	require.Eventually(t, func() bool {
		return fdb.chunkCount() > 0
	}, 2*time.Second, 10*time.Millisecond,
		"a phase-read error must default to processing, not idle/wedge")
	w.Stop()
}
