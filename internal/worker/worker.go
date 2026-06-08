// Package worker polls for completed transcripts (from the external WhisperX
// runner) and runs the chunk → embed → pgvector pipeline for each one.
package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/chunker"
	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/queue"
)

// DBInterface is the subset of db.DB used by the worker.
type DBInterface interface {
	GetCompletedTranscripts(ctx context.Context) ([]*db.Transcript, error)
	InsertChunks(ctx context.Context, chunks []db.Chunk) error
	GetEmbeddings(texts []string) ([][]float32, error)
	RecoverStaleJobs(ctx context.Context, timeout time.Duration) error
}

// Worker polls for completed transcripts and embeds them.
type Worker struct {
	queue  *queue.Queue
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	db     DBInterface
	log    log.Logger
}

// NewWorker creates a Worker. The queue parameter is accepted for API
// compatibility with the monitor wiring but is not used by the embed loop.
func NewWorker(q *queue.Queue, database DBInterface, cfg *config.Config) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{
		queue:  q,
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
		db:     database,
		log:    log.NewLogger("worker"),
	}
}

// Start runs the embed loop until Stop is called.
func (w *Worker) Start(cfg *config.Config) {
	w.log.Info("worker started")
	defer close(w.done)

	const pollInterval = 30 * time.Second
	const staleRecoveryInterval = 5 * time.Minute

	lastStale := time.Now()

	for {
		select {
		case <-w.ctx.Done():
			w.log.Info("worker shutting down")
			return
		default:
		}

		// Recover stale jobs periodically.
		if time.Since(lastStale) >= staleRecoveryInterval {
			if err := w.db.RecoverStaleJobs(w.ctx, cfg.StaleJobTimeout); err != nil {
				w.log.Error("stale job recovery failed", "error", err)
			}
			lastStale = time.Now()
		}

		transcripts, err := w.db.GetCompletedTranscripts(w.ctx)
		if err != nil {
			w.log.Error("poll for completed transcripts failed", "error", err)
			w.sleep(pollInterval)
			continue
		}

		if len(transcripts) == 0 {
			w.sleep(pollInterval)
			continue
		}

		for _, t := range transcripts {
			if w.ctx.Err() != nil {
				return
			}
			if err := w.processTranscript(cfg, t); err != nil {
				w.log.Error("failed to process transcript",
					"transcript_id", t.ID,
					"file", t.FilePath,
					"error", err)
			}
		}
	}
}

// processTranscript chunks a transcript, obtains embeddings, and stores them
// as transcript_chunks rows.
//
// Chunking strategy:
//   - If the transcript has segments (WhisperX output), accumulate whole
//     segments until the token budget (cfg.ChunkSize) is reached. The chunk
//     gets Chunk.StartSec/EndSec from the first/last segment in the window
//     and Speaker set to the dominant speaker across those segments.
//   - If Segments is empty (legacy or missing diarization data), fall back
//     to raw-text token chunking with zero timestamps and no speaker.
func (w *Worker) processTranscript(cfg *config.Config, t *db.Transcript) error {
	if t.RawText == "" {
		return fmt.Errorf("transcript %s has empty raw text, skipping", t.ID)
	}

	w.log.Info("embedding transcript", "file", t.FilePath, "transcript_id", t.ID)
	start := time.Now()

	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 512
	}

	var chunks []db.Chunk

	if len(t.Segments) == 0 {
		// Fallback: raw-text chunking with zero timestamps.
		w.log.Warn("transcript has no segments; using raw-text chunking (timestamps will be zero)",
			"transcript_id", t.ID)
		texts := chunker.Chunker(t.RawText, chunkSize, chunker.SplitTypeToken)
		if len(texts) == 0 {
			return fmt.Errorf("no chunks produced for transcript %s", t.ID)
		}
		chunks = make([]db.Chunk, len(texts))
		for i, text := range texts {
			chunks[i] = db.Chunk{
				TranscriptID: t.ID,
				FilePath:     t.FilePath,
				ChunkIndex:   i,
				StartSec:     0,
				EndSec:       0,
				Text:         text,
				// Speaker remains nil
			}
		}
	} else {
		// Preferred path: accumulate whole segments until the token budget is
		// exhausted, then emit a chunk with accurate timestamps and speaker.
		chunks = buildChunksFromSegments(t, chunkSize)
		if len(chunks) == 0 {
			return fmt.Errorf("no chunks produced from segments for transcript %s", t.ID)
		}
	}

	// Collect texts for batch embedding.
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}

	embeddings, err := w.db.GetEmbeddings(texts)
	if err != nil {
		return fmt.Errorf("get embeddings: %w", err)
	}
	if len(embeddings) != len(texts) {
		return fmt.Errorf("embedding count mismatch: got %d for %d chunks", len(embeddings), len(texts))
	}

	for i := range chunks {
		chunks[i].Embedding = embeddings[i]
	}

	if err := w.db.InsertChunks(w.ctx, chunks); err != nil {
		return fmt.Errorf("insert chunks: %w", err)
	}

	w.log.Info("transcript embedded",
		"file", t.FilePath,
		"transcript_id", t.ID,
		"chunks", len(chunks),
		"duration", time.Since(start).Round(time.Millisecond))
	return nil
}

// buildChunksFromSegments accumulates WhisperX segments into token-budgeted
// chunks, preserving start/end timestamps and dominant speaker.
func buildChunksFromSegments(t *db.Transcript, chunkSize int) []db.Chunk {
	var chunks []db.Chunk
	chunkIdx := 0

	// We accumulate segment texts until the estimated token count reaches chunkSize.
	// We use a rough word-based approximation (≈ 1.3 tokens/word) to avoid importing
	// the full tiktoken library inside the segment loop.
	var (
		accText      string
		accStart     float64
		accEnd       float64
		speakerCount map[string]int
	)

	resetAcc := func(seg db.Segment) {
		accText = seg.Text
		accStart = seg.Start
		accEnd = seg.End
		speakerCount = make(map[string]int)
		if seg.Speaker != nil {
			speakerCount[*seg.Speaker]++
		}
	}

	flushChunk := func() {
		if accText == "" {
			return
		}
		var dominant *string
		if len(speakerCount) > 0 {
			best := ""
			bestN := 0
			for sp, n := range speakerCount {
				if n > bestN || (n == bestN && sp > best) {
					best = sp
					bestN = n
				}
			}
			cp := best
			dominant = &cp
		}
		chunks = append(chunks, db.Chunk{
			TranscriptID: t.ID,
			FilePath:     t.FilePath,
			ChunkIndex:   chunkIdx,
			StartSec:     accStart,
			EndSec:       accEnd,
			Text:         accText,
			Speaker:      dominant,
		})
		chunkIdx++
	}

	first := true
	for _, seg := range t.Segments {
		if first {
			resetAcc(seg)
			first = false
			continue
		}

		// Check whether the combined text would exceed the token budget.
		// If the chunker splits the combined text into >1 chunk it is over budget.
		combined := accText + " " + seg.Text
		overBudget := len(chunker.Chunker(combined, chunkSize, chunker.SplitTypeToken)) > 1

		if overBudget {
			flushChunk()
			resetAcc(seg)
			continue
		}

		// Accumulate.
		accText = combined
		accEnd = seg.End
		if seg.Speaker != nil {
			speakerCount[*seg.Speaker]++
		}
	}
	flushChunk()

	return chunks
}

// Stop signals the worker to shut down and waits for it to finish.
func (w *Worker) Stop() {
	w.cancel()
	<-w.done
}

// sleep sleeps for d while respecting ctx cancellation.
func (w *Worker) sleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-w.ctx.Done():
	}
}
