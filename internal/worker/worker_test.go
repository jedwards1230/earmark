package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
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
