// Package worker polls for completed transcripts (from the external ASR runner)
// and runs the chunk → embed → pgvector pipeline for each one.
package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jedwards1230/earmark/internal/chunker"
	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/metrics"
	"github.com/jedwards1230/earmark/internal/openai"
	"github.com/jedwards1230/earmark/internal/queue"
	"github.com/jedwards1230/earmark/internal/tokenizer"
)

// DBInterface is the subset of db.DB used by the worker.
type DBInterface interface {
	// GetCompletedTranscripts returns done transcripts not yet embedded
	// (ungated path: EVAL_GATES_EMBED=false).
	GetCompletedTranscripts(ctx context.Context) ([]*db.Transcript, error)
	// GetUnevaluatedTranscripts returns done, not-eval'd, not-embedded
	// transcripts — the eval-pass selection for EVAL_GATES_EMBED=true.
	GetUnevaluatedTranscripts(ctx context.Context) ([]*db.Transcript, error)
	// GetEvaluatedUnembeddedTranscripts returns done, eval'd, not-embedded
	// transcripts — the embed-pass selection for EVAL_GATES_EMBED=true.
	GetEvaluatedUnembeddedTranscripts(ctx context.Context) ([]*db.Transcript, error)
	InsertChunks(ctx context.Context, chunks []db.Chunk) error
	// EmbedDocuments embeds transcript chunks for STORAGE — the document side of
	// the pipeline (search_document: prefix for nomic-embed-text). Never the
	// query side; see db.EmbedQuery.
	EmbedDocuments(texts []string) ([][]float32, error)
	EmbedDocumentsWithUsage(texts []string) ([][]float32, openai.EmbeddingUsage, error)
	RecoverStaleJobs(ctx context.Context, timeout time.Duration) (int, error)
	UpsertEmbedMetrics(ctx context.Context, m db.EmbedMetrics) error
	// UpsertEvalMetrics records the eval slice of run_metrics (timing, judge
	// model, chunk/skip/finding counts) for the in-pipeline judge. Best-effort.
	UpsertEvalMetrics(ctx context.Context, m db.EvalMetrics) error
	// AppendEvent records one pipeline_events row (CONTRACT §1.7). Best-effort:
	// the worker logs-and-continues; an event write never fails the embed/eval.
	AppendEvent(ctx context.Context, e db.PipelineEvent) error
	// GetPipelinePhase reports the batched-pipeline phase (CONTRACT §1.4). The
	// embed worker idles during the "transcribe" phase (ASR owns the GPU) and
	// processes normally for "idle"/"analyze"/NULL.
	GetPipelinePhase(ctx context.Context) (string, error)
	// InsertFindings persists eval findings — used only by the in-pipeline eval
	// path (EvalInPipeline). *db.DB already satisfies this (it is the eval
	// FindingWriter).
	InsertFindings(ctx context.Context, findings []db.Finding) error
}

// Worker polls for completed transcripts and embeds them.
type Worker struct {
	queue  *queue.Queue
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	db     DBInterface
	log    log.Logger
	// judge is non-nil only when EvalInPipeline is set AND a chat endpoint
	// resolved; when nil the worker skips the in-pipeline eval step entirely.
	judge *eval.Judge
	// evalGatesEmbed mirrors cfg.EvalGatesEmbed: when true the worker runs the
	// strict two-pass gated flow (eval pass → embed pass) rather than the
	// combined single-pass flow. CONTRACT §2.4.
	evalGatesEmbed bool
	// metrics records Prometheus stage durations + counters (CONTRACT §2.16).
	// nil-safe: a nil Registry makes the Record* calls no-ops.
	metrics *metrics.Registry
}

// SetMetrics attaches a Prometheus registry so the worker records stage
// durations and the completed/failed counters (CONTRACT §2.16). Optional —
// the Record* calls are nil-safe when unset.
func (w *Worker) SetMetrics(m *metrics.Registry) { w.metrics = m }

// NewWorker creates a Worker. The queue parameter is accepted for API
// compatibility with the monitor wiring but is not used by the embed loop.
//
// When cfg.EvalInPipeline is set, the worker resolves the eval chat endpoint and
// builds a judge so each transcript is evaluated before embedding. A resolution
// failure is non-fatal unless cfg.EvalGatesEmbed is also true: the gate requires
// a judge (fail-closed contract validated in config.LoadConfig), so by the time
// NewWorker runs, a missing judge with EvalGatesEmbed=true is already an error;
// here we only need the non-fatal path for EvalInPipeline without the gate.
func NewWorker(q *queue.Queue, database DBInterface, cfg *config.Config) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{
		queue:          q,
		done:           make(chan struct{}),
		ctx:            ctx,
		cancel:         cancel,
		db:             database,
		log:            log.NewLogger("worker"),
		evalGatesEmbed: cfg.EvalGatesEmbed,
	}
	if cfg.EvalInPipeline {
		chat, err := eval.ResolveChatClient(eval.ConfigSource(cfg))
		if err != nil {
			w.log.Warn("EVAL_IN_PIPELINE set but no eval chat endpoint resolved — inline eval disabled",
				"error", err)
		} else {
			w.judge = eval.NewJudge(chat)
			w.log.Info("in-pipeline eval enabled", "judge_model", chat.Model(),
				"eval_gates_embed", cfg.EvalGatesEmbed)
		}
	}
	return w
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
			failed, err := w.db.RecoverStaleJobs(w.ctx, cfg.StaleJobTimeout)
			if err != nil {
				w.log.Error("stale job recovery failed", "error", err)
			} else if failed > 0 && w.metrics != nil {
				for i := 0; i < failed; i++ {
					w.metrics.RecordJobFailed()
				}
			}
			lastStale = time.Now()
		}

		// Phase gate (CONTRACT §1.4): during the ASR-only "transcribe" phase the
		// embed worker idles so eval/embed don't contend for the GPU the ASR runner
		// owns. For "idle"/"analyze"/NULL it processes normally (today's behavior).
		// A read error defaults to "idle" (process) + logs, so a DB hiccup never
		// wedges the worker.
		phase, err := w.db.GetPipelinePhase(w.ctx)
		if err != nil {
			w.log.Error("read pipeline phase failed; defaulting to idle (processing)", "error", err)
			phase = db.PhaseIdle
		}
		if phase == db.PhaseTranscribe {
			w.log.Debug("pipeline in transcribe phase; embed worker idling this cycle")
			w.sleep(pollInterval)
			continue
		}

		if w.evalGatesEmbed {
			// Gated two-pass flow (EVAL_GATES_EMBED=true, CONTRACT §2.4):
			//   Eval pass:  done, not-eval'd, not-embedded → judge → eval_finished_at
			//   Embed pass: done, eval'd, not-embedded     → embed → transcript_chunks
			// The two passes run sequentially in the same poll cycle; the
			// eval_finished_at latch is the hand-off. Both are crash-resumable DB
			// selections — restarting the worker re-enters the correct pass.

			// Eval pass.
			unevaluated, err := w.db.GetUnevaluatedTranscripts(w.ctx)
			if err != nil {
				w.log.Error("poll for unevaluated transcripts failed", "error", err)
				w.sleep(pollInterval)
				continue
			}
			for _, t := range unevaluated {
				if w.ctx.Err() != nil {
					return
				}
				if err := w.evalTranscript(cfg, t); err != nil {
					w.log.Error("failed to eval transcript",
						"transcript_id", t.ID, "file", t.FilePath, "error", err)
				}
			}

			// Embed pass (picks up transcripts the eval pass just finished, plus
			// any that were already eval'd from a previous cycle).
			evaluated, err := w.db.GetEvaluatedUnembeddedTranscripts(w.ctx)
			if err != nil {
				w.log.Error("poll for evaluated unembedded transcripts failed", "error", err)
				w.sleep(pollInterval)
				continue
			}
			for _, t := range evaluated {
				if w.ctx.Err() != nil {
					return
				}
				if err := w.embedTranscript(cfg, t); err != nil {
					w.log.Error("failed to embed transcript",
						"transcript_id", t.ID, "file", t.FilePath, "error", err)
				}
			}
			if len(unevaluated) == 0 && len(evaluated) == 0 {
				w.sleep(pollInterval)
			}
		} else {
			// Ungated single-pass flow (default): combined chunk→eval→embed.
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
}

// processTranscript chunks a transcript, obtains embeddings, and stores them
// as transcript_chunks rows.
//
// Chunking strategy:
//   - If the transcript has segments (NeMo Parakeet output), accumulate whole
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
	w.appendEvent(db.PipelineEvent{
		JobID:      t.JobID,
		FilePath:   t.FilePath,
		Stage:      db.StageEmbed,
		Event:      db.EventStart,
		RunnerHost: db.HostGoWorker,
	})

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

	// Pre-assign chunk UUIDs so the in-pipeline eval below can attribute findings
	// to chunks before they are inserted. When EvalGatesEmbed is on, use
	// deterministic UUIDs (UUIDv5 over transcript_id+chunk_index) so the eval
	// pass and the embed pass produce the same IDs without coordination —
	// findings written in a prior eval pass reference the same chunk rows the
	// embed pass will insert. CONTRACT §1.5.
	if w.evalGatesEmbed {
		for i := range chunks {
			chunks[i].ID = db.ChunkUUID(t.ID, chunks[i].ChunkIndex)
		}
	} else {
		for i := range chunks {
			chunks[i].ID = uuid.NewString()
		}
	}

	// In-pipeline eval (repositioned per the batched-pipeline design): judge the
	// chunks BEFORE embedding so findings are produced from the same text. Gated
	// on EvalInPipeline (judge is nil otherwise). The findings are COMPUTED here
	// but PERSISTED only after the chunks are inserted (below) — otherwise an
	// embedding failure would leave findings referencing chunk UUIDs that were
	// never inserted (orphans), and the retry would re-chunk with fresh UUIDs and
	// double up. Best-effort: a judge error yields no findings, never blocks embed.
	var inlineFindings []db.Finding
	if w.judge != nil {
		inlineFindings = w.judgeChunks(t, chunks)
	}

	// Collect texts for batch embedding.
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}

	embeddings, usage, err := w.db.EmbedDocumentsWithUsage(texts)
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
	finished := time.Now()
	w.appendEvent(db.PipelineEvent{
		JobID:      t.JobID,
		FilePath:   t.FilePath,
		Stage:      db.StageEmbed,
		Event:      db.EventFinish,
		RunnerHost: db.HostGoWorker,
		Model:      embedModel(cfg),
		DurationMS: db.Int64Ptr(finished.Sub(start).Milliseconds()),
		ItemCount:  db.IntPtr(len(chunks)),
	})
	w.metrics.RecordStageFinish(db.StageEmbed, finished.Sub(start))

	// Persist in-pipeline findings now that their chunks exist. Best-effort: a
	// findings-write failure leaves chunks searchable but un-flagged (advisory),
	// which is the safe direction — the reverse (findings without chunks) is what
	// the post-insert ordering exists to prevent.
	if len(inlineFindings) > 0 {
		if err := w.db.InsertFindings(w.ctx, inlineFindings); err != nil {
			w.log.Warn("persist in-pipeline findings failed (chunks inserted; findings dropped)",
				"transcript_id", t.ID, "file", t.FilePath, "findings", len(inlineFindings), "error", err)
		} else {
			w.log.Info("in-pipeline eval findings persisted",
				"transcript_id", t.ID, "findings", len(inlineFindings))
		}
	}

	w.log.Info("transcript embedded",
		"file", t.FilePath,
		"transcript_id", t.ID,
		"chunks", len(chunks),
		"duration", finished.Sub(start).Round(time.Millisecond))

	// Per-run observability: record embedding timing, model, chunk count, and
	// token counts. Best-effort — a metrics write must not fail the embed.
	w.recordEmbedMetrics(t, texts, usage, start, finished, len(chunks),
		embedModel(cfg))
	return nil
}

// evalTranscript is the eval-pass handler for EVAL_GATES_EMBED=true. It:
//  1. Chunks the transcript with deterministic UUIDs (so the embed pass later
//     produces the same IDs).
//  2. Runs the judge over the chunks.
//  3. Persists findings.
//  4. Writes eval_finished_at (the embed-gate latch, CONTRACT §1.5).
//
// It does NOT embed — that is the embed pass's job. If the judge is nil
// (EVAL_IN_PIPELINE not set alongside EVAL_GATES_EMBED), we still write
// eval_finished_at with zero findings so the embed pass can proceed; the
// startup warning in config.LoadConfig already flagged the misconfiguration.
func (w *Worker) evalTranscript(cfg *config.Config, t *db.Transcript) error {
	if t.RawText == "" {
		return fmt.Errorf("transcript %s has empty raw text, skipping", t.ID)
	}
	w.log.Info("eval pass: judging transcript", "file", t.FilePath, "transcript_id", t.ID)

	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 512
	}

	var chunks []db.Chunk
	if len(t.Segments) == 0 {
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
			}
		}
	} else {
		chunks = buildChunksFromSegments(t, chunkSize)
		if len(chunks) == 0 {
			return fmt.Errorf("no chunks produced from segments for transcript %s", t.ID)
		}
	}

	// Deterministic UUIDs: same as the embed pass will produce for the same
	// (transcript_id, chunk_index) — findings reference these IDs before insert.
	for i := range chunks {
		chunks[i].ID = db.ChunkUUID(t.ID, chunks[i].ChunkIndex)
	}

	evalStart := time.Now()
	var inlineFindings []db.Finding
	if w.judge != nil {
		inlineFindings = w.judgeChunks(t, chunks)
	} else {
		w.log.Warn("eval pass: no judge configured (EVAL_IN_PIPELINE not set); "+
			"writing eval_finished_at with zero findings so embed pass can proceed",
			"transcript_id", t.ID)
	}
	evalFinished := time.Now()

	// Persist findings before the eval_finished_at latch so the DB is never in
	// a state where the latch is set but the findings are absent.
	if len(inlineFindings) > 0 {
		if err := w.db.InsertFindings(w.ctx, inlineFindings); err != nil {
			w.log.Warn("eval pass: persist findings failed (writing eval_finished_at anyway — findings lost)",
				"transcript_id", t.ID, "findings", len(inlineFindings), "error", err)
		}
	}

	// Write eval_finished_at — the latch that allows the embed pass to pick this
	// transcript up. We write it even when findings is empty (a clean transcript)
	// or when the judge was nil (misconfiguration fallback) so the pipeline never
	// stalls on a latch that is never set.
	stats := eval.RunStats{ChunksEvaluated: len(chunks), FindingsFound: len(inlineFindings)}
	w.recordEvalMetrics(t, stats, evalStart, evalFinished)
	w.log.Info("eval pass: finished", "file", t.FilePath, "chunks", len(chunks),
		"findings", len(inlineFindings), "duration", evalFinished.Sub(evalStart).Round(time.Millisecond))
	return nil
}

// embedTranscript is the embed-pass handler for EVAL_GATES_EMBED=true. It
// chunks the transcript with the same deterministic UUIDs as the eval pass used,
// embeds the chunks, and inserts them. It does NOT run the judge (eval was
// already done in the eval pass). CONTRACT §1.5.
func (w *Worker) embedTranscript(cfg *config.Config, t *db.Transcript) error {
	if t.RawText == "" {
		return fmt.Errorf("transcript %s has empty raw text, skipping", t.ID)
	}
	w.log.Info("embed pass: embedding transcript", "file", t.FilePath, "transcript_id", t.ID)
	start := time.Now()
	w.appendEvent(db.PipelineEvent{
		JobID:      t.JobID,
		FilePath:   t.FilePath,
		Stage:      db.StageEmbed,
		Event:      db.EventStart,
		RunnerHost: db.HostGoWorker,
	})

	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 512
	}

	var chunks []db.Chunk
	if len(t.Segments) == 0 {
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
			}
		}
	} else {
		chunks = buildChunksFromSegments(t, chunkSize)
		if len(chunks) == 0 {
			return fmt.Errorf("no chunks produced from segments for transcript %s", t.ID)
		}
	}

	// Deterministic UUIDs — must match what the eval pass assigned to these
	// chunks so findings reference the correct chunk rows after insert.
	for i := range chunks {
		chunks[i].ID = db.ChunkUUID(t.ID, chunks[i].ChunkIndex)
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}

	embeddings, usage, err := w.db.EmbedDocumentsWithUsage(texts)
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
	finished := time.Now()
	w.appendEvent(db.PipelineEvent{
		JobID:      t.JobID,
		FilePath:   t.FilePath,
		Stage:      db.StageEmbed,
		Event:      db.EventFinish,
		RunnerHost: db.HostGoWorker,
		Model:      embedModel(cfg),
		DurationMS: db.Int64Ptr(finished.Sub(start).Milliseconds()),
		ItemCount:  db.IntPtr(len(chunks)),
	})
	w.metrics.RecordStageFinish(db.StageEmbed, finished.Sub(start))

	w.log.Info("embed pass: transcript embedded",
		"file", t.FilePath, "transcript_id", t.ID, "chunks", len(chunks),
		"duration", finished.Sub(start).Round(time.Millisecond))

	w.recordEmbedMetrics(t, texts, usage, start, finished, len(chunks), embedModel(cfg))
	return nil
}

// judgeChunks runs the judge over the transcript's chunks and RETURNS the
// findings WITHOUT persisting them (write=false) — the caller persists after the
// chunks are inserted, so a later embedding failure can't leave orphaned
// findings. Best-effort: any error is logged and yields nil findings so the
// embed step always proceeds (eval is advisory; the corpus must stay searchable
// even if the judge endpoint is down). Chunks must already have their IDs
// assigned so the returned findings reference the rows the worker will insert.
func (w *Worker) judgeChunks(t *db.Transcript, chunks []db.Chunk) []db.Finding {
	evalChunks := make([]db.EvalChunk, len(chunks))
	for i, c := range chunks {
		evalChunks[i] = db.EvalChunk{
			ChunkID:            c.ID,
			TranscriptID:       t.ID,
			TranscriptionRunID: t.JobID,
			FilePath:           c.FilePath,
			ChunkIndex:         c.ChunkIndex,
			StartSec:           c.StartSec,
			EndSec:             c.EndSec,
			Text:               c.Text,
		}
	}

	evalStart := time.Now()
	w.appendEvent(db.PipelineEvent{
		JobID:      t.JobID,
		FilePath:   t.FilePath,
		Stage:      db.StageEval,
		Event:      db.EventStart,
		RunnerHost: db.HostGoWorker,
		Model:      w.judge.Model(),
	})
	findings, stats, err := eval.RunOnChunks(w.ctx, w.judge, nil, evalChunks, false)
	evalFinished := time.Now()
	if err != nil {
		w.log.Warn("in-pipeline eval failed (continuing to embed)",
			"transcript_id", t.ID, "file", t.FilePath, "error", err)
		w.appendEvent(db.PipelineEvent{
			JobID:      t.JobID,
			FilePath:   t.FilePath,
			Stage:      db.StageEval,
			Event:      db.EventError,
			RunnerHost: db.HostGoWorker,
			Model:      w.judge.Model(),
			DurationMS: db.Int64Ptr(evalFinished.Sub(evalStart).Milliseconds()),
			Reason:     err.Error(),
		})
		return nil
	}
	w.log.Info("in-pipeline eval judged",
		"transcript_id", t.ID,
		"chunks", stats.ChunksEvaluated,
		"findings", stats.FindingsFound)

	// Per-run observability: record eval timing, judge model, and counts. The
	// chunk set maps cleanly to this one job (t.JobID), so the run_metrics eval
	// slice attribution is unambiguous. Best-effort — a metrics write must not
	// fail the embed step (eval is advisory).
	w.recordEvalMetrics(t, stats, evalStart, evalFinished)
	w.appendEvent(db.PipelineEvent{
		JobID:      t.JobID,
		FilePath:   t.FilePath,
		Stage:      db.StageEval,
		Event:      db.EventFinish,
		RunnerHost: db.HostGoWorker,
		Model:      w.judge.Model(),
		DurationMS: db.Int64Ptr(evalFinished.Sub(evalStart).Milliseconds()),
		ItemCount:  db.IntPtr(stats.FindingsFound),
		Detail: map[string]any{
			"evaluated": stats.ChunksEvaluated,
			"skipped":   stats.ChunksSkipped,
		},
	})
	w.metrics.RecordStageFinish(db.StageEval, evalFinished.Sub(evalStart))
	return findings
}

// appendEvent records one pipeline_events row, best-effort: a write failure is
// logged and swallowed so an audit-event failure never affects the pipeline.
func (w *Worker) appendEvent(e db.PipelineEvent) {
	if err := w.db.AppendEvent(w.ctx, e); err != nil {
		w.log.Warn("pipeline event write failed (continuing)",
			"stage", e.Stage, "event", e.Event, "job_id", e.JobID, "error", err)
	}
}

// recordEvalMetrics UPSERTs the eval worker's slice of run_metrics for a
// transcript's job (CONTRACT §1.5). eval_finished_at is the per-job eval-
// completion marker. Best-effort: a DB error is logged and swallowed.
func (w *Worker) recordEvalMetrics(t *db.Transcript, stats eval.RunStats, started, finished time.Time) {
	m := db.EvalMetrics{
		JobID:      t.JobID,
		StartedAt:  started,
		FinishedAt: finished,
		Model:      w.judge.Model(),
		Chunks:     stats.ChunksEvaluated,
		Skipped:    stats.ChunksSkipped,
		Findings:   stats.FindingsFound,
	}
	if err := w.db.UpsertEvalMetrics(w.ctx, m); err != nil {
		w.log.Warn("eval metrics write failed (continuing)",
			"transcript_id", t.ID, "job_id", t.JobID, "error", err)
	}
}

// recordEmbedMetrics UPSERTs the embed worker's slice of run_metrics for a
// transcript's job. embed_total_tokens is the authoritative local tokenizer
// count (Ollama frequently omits usage for embeddings); embed_prompt_tokens is
// the provider-reported value, stored only when non-zero (nullable otherwise).
// Best-effort: a tokenizer or DB error is logged and swallowed.
func (w *Worker) recordEmbedMetrics(t *db.Transcript, texts []string, usage openai.EmbeddingUsage, started, finished time.Time, chunkCount int, model string) {
	total, failed := localTokenCount(texts)

	// If any chunk failed to tokenize, total is a partial sum indistinguishable
	// from a complete count. Store NULL (unknown) rather than a misleading
	// partial, and warn so the failure is visible.
	var totalTokens *int
	if failed > 0 {
		w.log.Warn("embed_total_tokens unknown: chunk tokenization failed (storing NULL)",
			"transcript_id", t.ID, "job_id", t.JobID,
			"failed_chunks", failed, "total_chunks", len(texts))
	} else {
		totalTokens = &total
	}

	var promptTokens *int
	if usage.PromptTokens > 0 {
		p := usage.PromptTokens
		promptTokens = &p
	}

	m := db.EmbedMetrics{
		JobID:        t.JobID,
		StartedAt:    started,
		FinishedAt:   finished,
		Model:        model,
		ChunkCount:   chunkCount,
		PromptTokens: promptTokens,
		TotalTokens:  totalTokens,
	}
	if err := w.db.UpsertEmbedMetrics(w.ctx, m); err != nil {
		w.log.Warn("embed metrics write failed (continuing)",
			"transcript_id", t.ID, "job_id", t.JobID, "error", err)
	}
}

// localTokenCount sums the tokenizer's token count across the embedded chunk
// texts and reports how many chunks failed to tokenize. This is the
// authoritative embed_total_tokens — the same tokenizer the chunker uses, so the
// count reflects exactly what was embedded regardless of whether the provider
// reported usage.
//
// The returned count is only meaningful when failed == 0: if any chunk fails to
// tokenize, the sum is partial and indistinguishable from a complete count, so
// the caller MUST treat the total as unknown (store NULL) rather than persist a
// misleading partial. The tokenization errors themselves never fail the embed —
// the chunks are already embedded by this point; only the metric is degraded.
func localTokenCount(texts []string) (total, failed int) {
	for _, txt := range texts {
		n, err := tokenizer.CountTokens(txt)
		if err != nil {
			failed++
			continue
		}
		total += n
	}
	return total, failed
}

// embedModel returns the configured embeddings model, falling back to the
// CONTRACT default when unset.
func embedModel(cfg *config.Config) string {
	if cfg.EmbeddingsModel != "" {
		return cfg.EmbeddingsModel
	}
	return "nomic-embed-text"
}

// buildChunksFromSegments accumulates ASR segments into token-budgeted
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
