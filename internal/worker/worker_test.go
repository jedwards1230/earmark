package worker

import (
	"context"
	"fmt"
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
type fakeDB struct {
	transcripts []*db.Transcript
	chunks      []db.Chunk
	embedErr    error
	insertErr   error
	usage       openai.EmbeddingUsage // returned by GetEmbeddingsWithUsage
	metrics     []db.EmbedMetrics     // captured by UpsertEmbedMetrics
	metricsErr  error
	findings    []db.Finding // captured by InsertFindings (in-pipeline eval)
	findingsErr error        // error returned by InsertFindings
}

func (f *fakeDB) GetCompletedTranscripts(_ context.Context) ([]*db.Transcript, error) {
	return f.transcripts, nil
}

func (f *fakeDB) InsertChunks(_ context.Context, chunks []db.Chunk) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.chunks = append(f.chunks, chunks...)
	return nil
}

func (f *fakeDB) GetEmbeddings(texts []string) ([][]float32, error) {
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

func (f *fakeDB) GetEmbeddingsWithUsage(texts []string) ([][]float32, openai.EmbeddingUsage, error) {
	vecs, err := f.GetEmbeddings(texts)
	return vecs, f.usage, err
}

func (f *fakeDB) UpsertEmbedMetrics(_ context.Context, m db.EmbedMetrics) error {
	if f.metricsErr != nil {
		return f.metricsErr
	}
	f.metrics = append(f.metrics, m)
	return nil
}

func (f *fakeDB) RecoverStaleJobs(_ context.Context, _ time.Duration) error { return nil }

func (f *fakeDB) InsertFindings(_ context.Context, findings []db.Finding) error {
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
