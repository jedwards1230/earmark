package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/queue"
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
