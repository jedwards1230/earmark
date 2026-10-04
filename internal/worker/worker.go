// Package worker polls for completed transcripts (from the external ASR runner)
// and runs the chunk → embed → pgvector pipeline for each one.
package worker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jedwards1230/earmark/internal/chunker"
	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/metrics"
	"github.com/jedwards1230/earmark/internal/openai"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/queue"
	"github.com/jedwards1230/earmark/internal/tokenizer"
)

// DBInterface is the subset of db.DB used by the worker.
type DBInterface interface {
	// GetCompletedTranscripts returns one keyset page (≤ limit rows after the
	// cursor) of transcripts not yet embedded (ungated path:
	// EVAL_GATES_EMBED=false). The worker walks every page each cycle, so the
	// memory bound never costs coverage.
	GetCompletedTranscripts(ctx context.Context, after db.TranscriptCursor, limit int) ([]*db.Transcript, error)
	// GetUnevaluatedTranscripts returns done, not-eval'd, not-embedded
	// transcripts — the eval-pass selection for EVAL_GATES_EMBED=true. limit
	// bounds the rows loaded per call so a large backlog can't OOM the pod; the
	// worker drains across cycles (CONTRACT §2.4, EMBED_BATCH_SIZE).
	GetUnevaluatedTranscripts(ctx context.Context, limit int) ([]*db.Transcript, error)
	// GetEvaluatedUnembeddedTranscripts returns done, eval'd, not-embedded
	// transcripts — the embed-pass selection for EVAL_GATES_EMBED=true. limit
	// bounds the rows loaded per call (see GetUnevaluatedTranscripts).
	GetEvaluatedUnembeddedTranscripts(ctx context.Context, limit int) ([]*db.Transcript, error)
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
	// processes normally for "idle"/"analyze"/NULL. The worker ALSO idles when
	// GetPaused reports true — paused is a global stop that gates the embed
	// worker too, independent of phase (CONTRACT §1.4).
	GetPipelinePhase(ctx context.Context) (string, error)
	// GetPaused reports the global pause flag (CONTRACT §1.4). When true the embed
	// worker idles each cycle (global stop) — this is what makes the dashboard
	// PAUSED banner truthful. Independent of phase; checked before the phase gate.
	GetPaused(ctx context.Context) (bool, error)
	// InsertFindings persists eval findings — used only by the in-pipeline eval
	// path (EvalInPipeline). *db.DB already satisfies this (it is the eval
	// FindingWriter).
	InsertFindings(ctx context.Context, findings []db.Finding) error
	// GetCorrectionOverlay loads a transcript's accepted corrections so the
	// embed path can replay them onto the regenerated chunks (CONTRACT §2.17).
	// Read-only. It also returns the server-side watermark that read was taken
	// at, which ClearEmbeddingStale needs to avoid swallowing a concurrent
	// human decision.
	GetCorrectionOverlay(ctx context.Context, transcriptID string) ([]db.CorrectionRow, time.Time, error)
	// MarkFindingsApplied / MarkFindingsStale persist the outcome of a replay:
	// which corrections landed (with their span-level before/after) and which
	// could no longer be placed. Both write only transcript_findings.
	MarkFindingsApplied(ctx context.Context, recs []db.AppliedFinding) error
	MarkFindingsStale(ctx context.Context, ids []string, reason string) error
	// ClearEmbeddingStale clears the rebuild flag for chunks this rebuild
	// brought up to date, guarded by the overlay-read watermark.
	ClearEmbeddingStale(ctx context.Context, transcriptID string, watermark time.Time) error
	// GetTranscriptsWithStaleChunks selects transcripts with at least one chunk
	// flagged embedding_stale — the rebuild pass that closes the
	// accept → replay → re-embed loop (CONTRACT §2.17).
	GetTranscriptsWithStaleChunks(ctx context.Context, limit int) ([]*db.Transcript, error)
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

		// Pause gate (CONTRACT §1.4): paused is a global stop. When set (the
		// dashboard Pause button), the embed worker idles too — not just the ASR
		// runner — so the PAUSED banner is truthful (embeddings stop). Checked
		// before the phase gate and independent of it. A read error defaults to
		// NOT paused (process) + logs, so a DB hiccup never wedges the worker.
		paused, err := w.db.GetPaused(w.ctx)
		if err != nil {
			w.log.Error("read pause flag failed; defaulting to not paused (processing)", "error", err)
			paused = false
		}
		if paused {
			w.log.Debug("pipeline paused; embed worker idling this cycle")
			w.sleep(pollInterval)
			continue
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

		// Rebuild pass (CONTRACT §2.17): transcripts whose projection is out of
		// date because a human accepted or reverted a correction. It runs in
		// BOTH flows and before them, because the other selections only ever
		// look at transcripts with no chunks yet — an already-embedded
		// transcript would otherwise never be revisited and an accepted
		// correction would never reach the searchable text.
		staleRebuilt := w.rebuildStaleTranscripts(cfg)

		if w.evalGatesEmbed {
			// Gated two-pass flow (EVAL_GATES_EMBED=true, CONTRACT §2.4):
			//   Eval pass:  done, not-eval'd, not-embedded → judge → eval_finished_at
			//   Embed pass: done, eval'd, not-embedded     → embed → transcript_chunks
			// The two passes run sequentially in the same poll cycle; the
			// eval_finished_at latch is the hand-off. Both are crash-resumable DB
			// selections — restarting the worker re-enters the correct pass.
			//
			// Both selections are bounded by EMBED_BATCH_SIZE so a large backlog
			// (e.g. a full re-embed after re-segmentation) never loads every
			// transcript's segments JSONB into one slice and OOM-kills the pod.
			// Each pass processes at most batchSize transcripts; a transcript that
			// gets eval'd or embedded drops out of the next cycle's selection.
			batchSize := embedBatchSize(cfg)

			// Eval pass.
			unevaluated, err := w.db.GetUnevaluatedTranscripts(w.ctx, batchSize)
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
			evaluated, err := w.db.GetEvaluatedUnembeddedTranscripts(w.ctx, batchSize)
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

			// Drain-fast: when either pass returned a FULL batch, more work is
			// waiting — loop immediately (no pollInterval sleep) so a large backlog
			// drains across back-to-back cycles instead of one batch per poll
			// interval. Only sleep when both passes returned a short (or empty)
			// batch, which means the backlog is drained; this avoids a busy-loop on
			// an empty queue while keeping a 4000-item backlog from idling for hours.
			fullBatch := len(unevaluated) >= batchSize || len(evaluated) >= batchSize ||
				staleRebuilt >= batchSize
			if !fullBatch {
				w.sleep(pollInterval)
			}
		} else {
			// Ungated single-pass flow (default): combined chunk→eval→embed.
			embedded, ok := w.drainCompleted(cfg)
			if !ok {
				return // shutting down
			}
			// Only idle when nothing was embedded AND nothing was rebuilt — a
			// rebuild-only cycle must not be mistaken for an empty queue. The
			// drain already walked the whole selection, so a transcript that
			// keeps failing (and stays selected) waits out the poll interval
			// instead of being retried in a hot loop.
			if embedded == 0 && staleRebuilt == 0 {
				w.sleep(pollInterval)
			}
		}
	}
}

// drainCompleted is one ungated cycle: it walks the not-yet-embedded selection
// in keyset pages of EMBED_BATCH_SIZE, processing each page before loading the
// next, until a short page shows the selection is exhausted. Memory is bounded
// by one page of transcripts (each carrying its segments JSONB) instead of the
// whole backlog, and — unlike a plain "first N" LIMIT — transcripts that fail
// to embed (and so stay in the selection) can never starve the ones behind
// them, because the cursor moves past them.
//
// Returns how many transcripts it embedded successfully, and false when the
// worker is shutting down. A selection error is logged and ends the cycle.
func (w *Worker) drainCompleted(cfg *config.Config) (int, bool) {
	batchSize := embedBatchSize(cfg)
	var cursor db.TranscriptCursor
	embedded := 0
	for {
		page, err := w.db.GetCompletedTranscripts(w.ctx, cursor, batchSize)
		if err != nil {
			w.log.Error("poll for completed transcripts failed", "error", err)
			return embedded, true
		}
		for _, t := range page {
			if w.ctx.Err() != nil {
				return embedded, false
			}
			cursor = db.CursorAfter(t)
			if err := w.processTranscript(cfg, t); err != nil {
				w.log.Error("failed to process transcript",
					"transcript_id", t.ID,
					"file", t.FilePath,
					"error", err)
				continue
			}
			embedded++
		}
		if len(page) < batchSize {
			return embedded, true
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
	// ORIGINAL-text consumer (§2.17 reader audit): raw_text is immutable
	// provenance and is only used here as an "is there anything to chunk at all"
	// guard. Corrections are applied downstream, to the projection.
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

	// Single-sourced chunking. When EvalGatesEmbed is on, use deterministic
	// UUIDs (UUIDv5 over transcript_id+chunk_index) so the eval pass and the
	// embed pass produce the same IDs without coordination — findings written in
	// a prior eval pass reference the same chunk rows the embed pass will insert.
	// CONTRACT §1.5.
	pristine, err := w.chunkTranscript(t, cfg.ChunkSize, w.evalGatesEmbed)
	if err != nil {
		return err
	}

	// In-pipeline eval (repositioned per the batched-pipeline design): judge the
	// chunks BEFORE embedding so findings are produced from the same text. Gated
	// on EvalInPipeline (judge is nil otherwise). The findings are COMPUTED here
	// but PERSISTED only after the chunks are inserted (below) — otherwise an
	// embedding failure would leave findings referencing chunk UUIDs that were
	// never inserted (orphans), and the retry would re-chunk with fresh UUIDs and
	// double up. Best-effort: a judge error yields no findings, never blocks embed.
	//
	// JUDGE PATH — PRISTINE, NO OVERLAY (CONTRACT §2.17). The judge records rune
	// anchors and chunk_text_sha256 against exactly the text it is shown, and
	// replay always starts from the pristine regenerated text. Show it the
	// corrected surface and every new finding is born stale, because its hash
	// could never match the projection's input.
	var judged *judgeOutcome
	if w.judge != nil {
		judged = w.judgeChunks(t, pristine)
	}

	// EMBED PATH — replay the accepted corrections onto a COPY of the pristine
	// chunks. ONE chunking pass feeds both consumers: `pristine` is what the
	// judge saw, `chunks` is the corrected projection that gets embedded,
	// searched, and displayed.
	//
	// `replay` is COMPUTED here but PERSISTED only after InsertChunks succeeds,
	// for the same reason as inlineFindings above: a premature write would claim
	// corrections landed in chunks that were never inserted, and `stale` is
	// terminal.
	chunks, replay, err := w.correctedChunks(t, pristine)
	if err != nil {
		return err
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

	// Persist the replay outcome now that the projection it describes exists.
	w.persistReplay(t, replay)

	// Persist in-pipeline findings now that their chunks exist, then record the
	// judge outcome. Best-effort: a findings-write failure leaves chunks
	// searchable but un-flagged (advisory), which is the safe direction — the
	// reverse (findings without chunks) is what the post-insert ordering exists
	// to prevent. The outcome is recorded only here, after the findings are
	// durable, so eval_finished_at never latches a run whose findings were lost
	// (latch-only-on-success, CONTRACT §1.5).
	if judged != nil {
		w.persistJudged(t, judged)
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
//  3. Persists findings (including a partial run's).
//  4. Writes eval_finished_at (the embed-gate latch, CONTRACT §1.5) ONLY when
//     every chunk was judged and the findings were stored; otherwise records
//     the failure (eval_failed_at / eval_failed_chunks / eval_error), which
//     releases the embed pass without latching (latch-only-on-success).
//
// It does NOT embed — that is the embed pass's job. The gate's fail-closed
// startup validation (config.LoadConfig) guarantees a judge is built whenever
// EVAL_GATES_EMBED=true, so w.judge is non-nil here; the nil-guard below is
// belt-and-suspenders so a future caller that reaches this with judge=nil still
// writes the latch (with zero findings) instead of panicking.
func (w *Worker) evalTranscript(cfg *config.Config, t *db.Transcript) error {
	// ORIGINAL-text consumer (§2.17 reader audit): raw_text is immutable
	// provenance, read here only as an emptiness guard.
	if t.RawText == "" {
		return fmt.Errorf("transcript %s has empty raw text, skipping", t.ID)
	}
	w.log.Info("eval pass: judging transcript", "file", t.FilePath, "transcript_id", t.ID)

	// PRISTINE, NO OVERLAY (CONTRACT §2.17). This whole pass is the judge path,
	// so the correction overlay is deliberately NOT replayed here. The judge
	// records rune anchors and chunk_text_sha256 against the text it is shown,
	// and replay always starts from the pristine regenerated text — judging the
	// corrected surface would make every new finding stale on arrival.
	//
	// Deterministic UUIDs: same as the embed pass will produce for the same
	// (transcript_id, chunk_index) — findings reference these IDs before insert.
	chunks, err := w.chunkTranscript(t, cfg.ChunkSize, true)
	if err != nil {
		return err
	}

	if w.judge == nil {
		// Misconfiguration fallback (validated out at startup): latch with zero
		// findings so the gated embed pass is never stalled behind a judge that
		// does not exist. judgeModel() is nil-safe.
		w.log.Warn("eval pass: no judge configured (EVAL_IN_PIPELINE not set); "+
			"writing eval_finished_at with zero findings so embed pass can proceed",
			"transcript_id", t.ID)
		now := time.Now()
		w.recordEvalMetrics(t, eval.RunStats{}, now, now, w.judgeModel())
		return nil
	}

	judged := w.judgeChunks(t, chunks)
	if judged.stopped() {
		// Shutdown mid-judge: record nothing. The job stays unlatched AND
		// unfailed, so the eval pass simply re-selects it after restart.
		return judged.err
	}

	// Persist findings, then record the outcome: the latch on a complete run,
	// a failure record otherwise. A failure record also releases the embed pass
	// (fail-open — a judge outage never blocks search) without latching, so the
	// job stays visible to `earmark eval --backfill-unevaluated` and is NOT
	// re-judged in a hot loop by this pass (it no longer matches its selection).
	w.persistJudged(t, judged)
	w.log.Info("eval pass: finished", "file", t.FilePath, "chunks", len(chunks),
		"evaluated", judged.stats.ChunksEvaluated, "skipped", judged.stats.ChunksSkipped,
		"findings", len(judged.findings), "latched", judged.complete(),
		"duration", judged.finished.Sub(judged.started).Round(time.Millisecond))
	return nil
}

// embedTranscript is the embed-pass handler for EVAL_GATES_EMBED=true. It
// chunks the transcript with the same deterministic UUIDs as the eval pass used,
// embeds the chunks, and inserts them. It does NOT run the judge (eval was
// already done in the eval pass). CONTRACT §1.5.
func (w *Worker) embedTranscript(cfg *config.Config, t *db.Transcript) error {
	// ORIGINAL-text consumer (§2.17 reader audit): guard only. The text actually
	// embedded below is the CORRECTED projection — raw_text is never rewritten.
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

	// Deterministic UUIDs — must match what the eval pass assigned to these
	// chunks so findings reference the correct chunk rows after insert.
	pristine, err := w.chunkTranscript(t, cfg.ChunkSize, true)
	if err != nil {
		return err
	}

	// EMBED PATH — replay the accepted corrections onto the regenerated chunks
	// (CONTRACT §2.17). This is the only place corrected text is produced; the
	// eval pass above deliberately embeds nothing and sees only pristine text.
	// The outcome is persisted only after InsertChunks succeeds (below).
	chunks, replay, err := w.correctedChunks(t, pristine)
	if err != nil {
		return err
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

	// Persist the replay outcome now that the projection it describes exists.
	w.persistReplay(t, replay)

	w.log.Info("embed pass: transcript embedded",
		"file", t.FilePath, "transcript_id", t.ID, "chunks", len(chunks),
		"duration", finished.Sub(start).Round(time.Millisecond))

	w.recordEmbedMetrics(t, texts, usage, start, finished, len(chunks), embedModel(cfg))
	return nil
}

// judgeOutcome is the result of one in-pipeline judge run over a transcript.
// It is computed before embedding but persisted (findings + run_metrics) only
// after the chunks exist — see persistJudged.
type judgeOutcome struct {
	findings []db.Finding
	stats    eval.RunStats
	// err is non-nil only when the run was stopped by the worker's context
	// (shutdown); per-chunk judge errors are counted in stats instead.
	err        error
	chunks     int
	started    time.Time
	finished   time.Time
	persistErr error // set by persistJudged when InsertFindings fails
}

// stopped reports whether the run was aborted by shutdown (nothing to record).
func (o *judgeOutcome) stopped() bool { return o.err != nil }

// complete is the latch-only-on-success rule: every chunk judged AND the
// findings durably stored. Anything less leaves the job unlatched.
func (o *judgeOutcome) complete() bool {
	return o.err == nil && o.stats.Complete() && o.persistErr == nil
}

// failureReason summarizes why a run is not complete, for run_metrics.eval_error.
func (o *judgeOutcome) failureReason() string {
	var parts []string
	if o.stats.ChunksSkipped > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d chunks failed: %s",
			o.stats.ChunksSkipped, o.chunks, o.stats.FirstError))
	}
	if o.persistErr != nil {
		parts = append(parts, "persist findings: "+o.persistErr.Error())
	}
	return strings.Join(parts, "; ")
}

// judgeChunks runs the judge over the transcript's chunks and RETURNS the
// outcome WITHOUT persisting anything (write=false) — the caller persists after
// the chunks are inserted (persistJudged), so a later embedding failure can't
// leave orphaned findings. It never fails the embed: per-chunk judge errors are
// counted (stats.ChunksSkipped), and only a worker shutdown sets err. Chunks
// must already have their IDs assigned so the findings reference the rows the
// worker will insert.
//
// Events: eval/start, then eval/finish for a complete run, or eval/error when
// any chunk failed (or the run was stopped) — so a failed judge run is visible
// in the audit log, not only in run_metrics.
func (w *Worker) judgeChunks(t *db.Transcript, chunks []db.Chunk) *judgeOutcome {
	evalChunks := EvalChunksFor(t, chunks)

	o := &judgeOutcome{chunks: len(evalChunks), started: time.Now()}
	w.appendEvent(db.PipelineEvent{
		JobID:      t.JobID,
		FilePath:   t.FilePath,
		Stage:      db.StageEval,
		Event:      db.EventStart,
		RunnerHost: db.HostGoWorker,
		Model:      w.judge.Model(),
	})
	o.findings, o.stats, o.err = eval.RunOnChunks(w.ctx, w.judge, nil, evalChunks, false)
	o.finished = time.Now()
	detail := map[string]any{
		"evaluated": o.stats.ChunksEvaluated,
		"skipped":   o.stats.ChunksSkipped,
	}
	if o.stats.ResolvedModel != "" {
		// The event's model column is the REQUESTED judge model; record what the
		// endpoint actually served alongside it.
		detail["resolved_model"] = o.stats.ResolvedModel
	}

	if o.err != nil || !o.stats.Complete() {
		reason := o.failureReason()
		if o.err != nil {
			reason = o.err.Error()
		}
		w.log.Warn("in-pipeline eval incomplete (continuing to embed; not latched)",
			"transcript_id", t.ID, "file", t.FilePath,
			"evaluated", o.stats.ChunksEvaluated, "skipped", o.stats.ChunksSkipped, "reason", reason)
		w.appendEvent(db.PipelineEvent{
			JobID:      t.JobID,
			FilePath:   t.FilePath,
			Stage:      db.StageEval,
			Event:      db.EventError,
			RunnerHost: db.HostGoWorker,
			Model:      w.judge.Model(),
			DurationMS: db.Int64Ptr(o.finished.Sub(o.started).Milliseconds()),
			ItemCount:  db.IntPtr(len(o.findings)),
			Reason:     reason,
			Detail:     detail,
		})
		return o
	}

	w.log.Info("in-pipeline eval judged",
		"transcript_id", t.ID,
		"chunks", o.stats.ChunksEvaluated,
		"findings", o.stats.FindingsFound)
	w.appendEvent(db.PipelineEvent{
		JobID:      t.JobID,
		FilePath:   t.FilePath,
		Stage:      db.StageEval,
		Event:      db.EventFinish,
		RunnerHost: db.HostGoWorker,
		Model:      w.judge.Model(),
		DurationMS: db.Int64Ptr(o.finished.Sub(o.started).Milliseconds()),
		ItemCount:  db.IntPtr(o.stats.FindingsFound),
		Detail:     detail,
	})
	w.metrics.RecordStageFinish(db.StageEval, o.finished.Sub(o.started))
	return o
}

// persistJudged stores a judge run's findings and then its run_metrics outcome:
// the eval_finished_at latch when the run is complete, a failure record
// (eval_failed_at / eval_failed_chunks / eval_error) otherwise.
//
// Partial findings from an incomplete run ARE persisted — they are valid
// advisory signal, and dropping them would lose it if the job is never
// re-judged. The re-judge paths (`earmark eval --backfill-*`) skip findings
// already recorded for the transcript, so a retry never doubles them up.
//
// A stopped run (shutdown) records nothing. Best-effort throughout: failures
// are logged, never returned, because eval is advisory.
func (w *Worker) persistJudged(t *db.Transcript, o *judgeOutcome) {
	if o.stopped() {
		return
	}
	if len(o.findings) > 0 {
		if err := w.db.InsertFindings(w.ctx, o.findings); err != nil {
			o.persistErr = err
			w.log.Warn("persist in-pipeline findings failed (eval left unlatched for re-judge)",
				"transcript_id", t.ID, "file", t.FilePath, "findings", len(o.findings), "error", err)
		} else {
			w.log.Info("in-pipeline eval findings persisted",
				"transcript_id", t.ID, "findings", len(o.findings))
		}
	}
	if o.complete() {
		w.recordEvalMetrics(t, o.stats, o.started, o.finished, w.judgeModel())
		return
	}
	failedChunks := o.stats.ChunksSkipped
	if o.persistErr != nil {
		// The findings of every evaluated chunk were lost with the write.
		failedChunks = o.chunks
	}
	if err := w.db.UpsertEvalMetrics(w.ctx, db.EvalMetrics{
		JobID:        t.JobID,
		StartedAt:    o.started,
		Model:        w.judgeModel(),
		Chunks:       o.stats.ChunksEvaluated,
		Skipped:      o.stats.ChunksSkipped,
		Findings:     o.stats.FindingsFound,
		FailedAt:     o.finished,
		FailedChunks: failedChunks,
		Error:        o.failureReason(),
	}); err != nil {
		w.log.Warn("eval failure record write failed (continuing)",
			"transcript_id", t.ID, "job_id", t.JobID, "error", err)
	}
}

// appendEvent records one pipeline_events row, best-effort: a write failure is
// logged and swallowed so an audit-event failure never affects the pipeline.
func (w *Worker) appendEvent(e db.PipelineEvent) {
	if err := w.db.AppendEvent(w.ctx, e); err != nil {
		w.log.Warn("pipeline event write failed (continuing)",
			"stage", e.Stage, "event", e.Event, "job_id", e.JobID, "error", err)
	}
}

// judgeModel returns the eval judge's model id, or "" when no judge is
// configured. Nil-safe: callers can record eval metrics in the (validated-out,
// but defensively handled) judge=nil case without dereferencing a nil judge.
func (w *Worker) judgeModel() string {
	if w.judge == nil {
		return ""
	}
	return w.judge.Model()
}

// recordEvalMetrics UPSERTs the eval worker's slice of run_metrics for a
// transcript's job (CONTRACT §1.5). eval_finished_at is the per-job eval-
// completion marker. The model is passed in (computed nil-safely by the caller
// via judgeModel) so this never dereferences a nil judge. Best-effort: a DB
// error is logged and swallowed.
func (w *Worker) recordEvalMetrics(t *db.Transcript, stats eval.RunStats, started, finished time.Time, model string) {
	m := db.EvalMetrics{
		JobID:         t.JobID,
		StartedAt:     started,
		FinishedAt:    finished,
		Model:         model,
		ResolvedModel: stats.ResolvedModel,
		Chunks:        stats.ChunksEvaluated,
		Skipped:       stats.ChunksSkipped,
		Findings:      stats.FindingsFound,
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

// embedBatchSize returns the gated-flow per-cycle selection limit, falling back
// to the CONTRACT default (32) when cfg carries a non-positive value (e.g. a
// hand-constructed Config in a test). This is the OOM guard's load-bearing
// bound — it must never be zero/negative, which would round-trip to an
// unbounded selection.
func embedBatchSize(cfg *config.Config) int {
	if cfg.EmbedBatchSize > 0 {
		return cfg.EmbedBatchSize
	}
	return 32
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

// chunkTranscript is the single-source chunking contract shared by the ungated
// single-pass (processTranscript) and the gated two-pass (evalTranscript /
// embedTranscript) flows. Keeping all three on this one helper guarantees the
// eval pass and the embed pass produce byte-identical chunk text and indices
// (and, when deterministicIDs is true, identical chunk IDs) so findings written
// in the eval pass reference the exact chunk rows the embed pass inserts.
//
// Chunking strategy:
//   - With segments (NeMo Parakeet output): accumulate whole segments until the
//     token budget is reached, preserving start/end timestamps and dominant
//     speaker (buildChunksFromSegments).
//   - Without segments (legacy/missing diarization): raw-text token chunking with
//     zero timestamps and no speaker.
//
// IDs: when deterministicIDs is true, each chunk gets a UUIDv5 over
// (transcript_id, chunk_index) — stable across passes and worker restarts. When
// false, each chunk gets a fresh random UUID (ungated path, single insert).
//
// Returns an error when no chunks are produced (empty raw text or empty segment
// text) so the caller can skip the transcript rather than embed nothing.
//
// PURE and PRISTINE (CONTRACT §2.17): it queries nothing and replays nothing.
// It IS the regeneration step of "regenerate from source → replay corrections →
// embed", so it must keep producing exactly what the ASR recorded. Corrections
// are layered on afterwards by applyOverlay, which rewrites Text only. Reading
// the overlay in here would put corrected text in front of the judge and make
// every new finding stale on arrival.
func (w *Worker) chunkTranscript(t *db.Transcript, chunkSize int, deterministicIDs bool) ([]db.Chunk, error) {
	if len(t.Segments) == 0 {
		w.log.Warn("transcript has no segments; using raw-text chunking (timestamps will be zero)",
			"transcript_id", t.ID)
	}
	return pristineChunks(t, chunkSize, deterministicIDs)
}

// PristineChunks is chunkTranscript's chunking with deterministic IDs, for
// callers outside the worker that must address the exact chunk rows the embed
// pass inserts — the `earmark eval --backfill-unevaluated` sweep. Using any
// other chunker there (e.g. raw-text token chunking for a transcript that has
// segments) judges text that differs from what is stored under the same
// ChunkUUID, so every finding's anchors and chunk hash would be wrong.
func PristineChunks(t *db.Transcript, chunkSize int) ([]db.Chunk, error) {
	return pristineChunks(t, chunkSize, true)
}

// EvalChunksFor converts pristine chunks into the judge's input shape, carrying
// the transcript/run attribution every finding needs.
func EvalChunksFor(t *db.Transcript, chunks []db.Chunk) []db.EvalChunk {
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
	return evalChunks
}

func pristineChunks(t *db.Transcript, chunkSize int, deterministicIDs bool) ([]db.Chunk, error) {
	if chunkSize <= 0 {
		chunkSize = 512
	}

	// ORIGINAL-text consumers (§2.17 reader audit): segments (and the raw_text
	// fallback) are the immutable ASR record. This is the regeneration step, so
	// it wants exactly that — and it never writes either of them back.
	var chunks []db.Chunk
	if len(t.Segments) == 0 {
		texts := chunker.Chunker(t.RawText, chunkSize, chunker.SplitTypeToken)
		if len(texts) == 0 {
			return nil, fmt.Errorf("no chunks produced for transcript %s", t.ID)
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
		chunks = buildChunksFromSegments(t, chunkSize)
		if len(chunks) == 0 {
			return nil, fmt.Errorf("no chunks produced from segments for transcript %s", t.ID)
		}
	}

	for i := range chunks {
		if deterministicIDs {
			chunks[i].ID = db.ChunkUUID(t.ID, chunks[i].ChunkIndex)
		} else {
			chunks[i].ID = uuid.NewString()
		}
		// These chunks are PRISTINE: regenerated from the immutable transcript
		// source with no corrections replayed yet, so source_text == text.
		// applyOverlay rewrites Text only; SourceText stays the projection's
		// input and the text the judge is shown (CONTRACT §2.17).
		chunks[i].SourceText = chunks[i].Text
	}

	return chunks, nil
}

// ─── Correction overlay (CONTRACT §2.17) ─────────────────────────────────────
//
// Chunking is "regenerate from source → replay accepted corrections → embed".
// The regeneration step (chunkTranscript) is pristine and pure; this section is
// the replay step. Keeping them separate is what lets the judge see pristine
// text while search sees corrected text, from a single chunking pass.

// rebuildStaleTranscripts re-runs the embed path for transcripts whose
// projection is out of date — the trigger that closes the
// accept → replay → re-embed loop. Returns how many transcripts it attempted.
//
// A human accepting or reverting a correction sets embedding_stale on the one
// affected chunk (db.SetPatchState). Nothing else in the worker would ever look
// at that transcript again: the other three selections all require a transcript
// to have NO chunks, and findings only exist for transcripts that are already
// embedded. This pass is what makes an accepted correction actually reach the
// searchable text.
//
// It reuses embedTranscript deliberately — same regenerate → replay → embed
// path, no second chunking implementation to drift. That also means it NEVER
// feeds the judge: embedTranscript does not run eval, so the
// judge-sees-pristine invariant is untouched and, per §2.17, a stale-driven
// re-embed does not require re-judging the job (the eval gate latches on
// run_metrics.eval_finished_at, which this does not disturb).
//
// Bounded by the same EMBED_BATCH_SIZE as the other passes. A selection error
// is logged and skipped: a rebuild backlog is not urgent enough to wedge the
// embed of new transcripts.
func (w *Worker) rebuildStaleTranscripts(cfg *config.Config) int {
	batchSize := embedBatchSize(cfg)
	stale, err := w.db.GetTranscriptsWithStaleChunks(w.ctx, batchSize)
	if err != nil {
		w.log.Error("poll for transcripts with stale chunks failed", "error", err)
		return 0
	}
	if len(stale) == 0 {
		return 0
	}

	w.log.Info("rebuild pass: re-embedding transcripts with corrected chunks",
		"transcripts", len(stale))
	for _, t := range stale {
		if w.ctx.Err() != nil {
			return len(stale)
		}
		if err := w.embedTranscript(cfg, t); err != nil {
			// Left flagged, so the next cycle retries it.
			w.log.Error("failed to rebuild transcript with stale chunks",
				"transcript_id", t.ID, "file", t.FilePath, "error", err)
		}
	}
	return len(stale)
}

// replayOutcome is what a rebuild owes the database AFTER its projection has
// been written: which corrections landed, which were retired, and the watermark
// the overlay was read at.
//
// It is a return value rather than a side effect on purpose. Writing it before
// the chunks are inserted would claim corrections landed in a projection that
// an embed or insert failure then prevented from ever existing — and `stale` is
// TERMINAL, so a premature retirement cannot be walked back. This mirrors the
// rule processTranscript already applies to in-pipeline findings: computed
// early, persisted only after InsertChunks succeeds.
type replayOutcome struct {
	applied []patch.AppliedPatch
	stale   []patch.StaleRef
	// readAt is the server-side time the overlay was read. Always set on a
	// successful load, including the zero-corrections case — a revert leaves a
	// chunk flagged with nothing left to replay, and that flag still has to be
	// cleared safely.
	readAt time.Time
}

// correctedChunks loads the transcript's accepted corrections and replays them
// onto the regenerated chunks, returning the copy to embed plus the outcome the
// caller must persist after a successful insert.
//
// FAIL CLOSED on load: if the overlay cannot be read we do NOT fall back to
// embedding pristine text. That would silently publish uncorrected text as if
// it were corrected — indistinguishable, in the corpus, from "there were no
// corrections" — so the transcript is left alone and retried next cycle.
func (w *Worker) correctedChunks(t *db.Transcript, chunks []db.Chunk) ([]db.Chunk, replayOutcome, error) {
	rows, readAt, err := w.db.GetCorrectionOverlay(w.ctx, t.ID)
	if err != nil {
		w.log.Error("load correction overlay failed; refusing to embed pristine text as corrected",
			"transcript_id", t.ID, "file", t.FilePath, "error", err)
		return nil, replayOutcome{}, fmt.Errorf("load correction overlay for transcript %s: %w", t.ID, err)
	}
	outcome := replayOutcome{readAt: readAt}
	if len(rows) == 0 {
		return chunks, outcome, nil
	}

	overlay, unplaceable := db.BuildOverlay(rows)
	out, applied, stale := applyOverlay(chunks, overlay)

	// A finding with no chunk_index has no chunk to replay onto; retire it
	// rather than leaving it accepted-but-invisible forever.
	for _, id := range unplaceable {
		stale = append(stale, patch.StaleRef{ID: id, Reason: patch.StaleReasonChunkChanged})
	}

	w.log.Info("correction overlay replayed",
		"transcript_id", t.ID, "file", t.FilePath,
		"corrections", len(rows), "applied", len(applied), "stale", len(stale))

	outcome.applied, outcome.stale = applied, stale
	return out, outcome, nil
}

// persistReplay records a replay's outcome and clears the rebuild flag.
//
// MUST be called only after the rebuilt chunks are durably inserted — see
// replayOutcome.
//
// Best-effort by design: the projection is already correct, and every rebuild
// replays the whole overlay from scratch, so a failed bookkeeping write is
// re-attempted next cycle. The flag clear is guarded by the overlay watermark,
// so a decision that raced this rebuild leaves the chunk flagged and gets its
// own rebuild instead of being swallowed.
func (w *Worker) persistReplay(t *db.Transcript, o replayOutcome) {
	if len(o.applied) > 0 {
		recs := make([]db.AppliedFinding, len(o.applied))
		for i, a := range o.applied {
			recs[i] = db.AppliedFinding{ID: a.ID, Before: a.Before, After: a.After}
		}
		if err := w.db.MarkFindingsApplied(w.ctx, recs); err != nil {
			w.log.Warn("recording applied corrections failed (projection is still correct; retried next rebuild)",
				"transcript_id", t.ID, "applied", len(recs), "error", err)
		}
	}

	// Group by reason so each retired finding records WHY it stopped applying.
	// Reasons are visited in sorted order: map iteration order must not leak
	// into the DB call sequence or the logs.
	byReason := make(map[string][]string)
	for _, s := range o.stale {
		byReason[s.Reason] = append(byReason[s.Reason], s.ID)
	}
	for _, reason := range slices.Sorted(maps.Keys(byReason)) {
		ids := byReason[reason]
		if err := w.db.MarkFindingsStale(w.ctx, ids, reason); err != nil {
			w.log.Warn("marking corrections stale failed",
				"transcript_id", t.ID, "reason", reason, "findings", len(ids), "error", err)
		}
	}

	// Clear the rebuild flag last: it is the only record that a rebuild is
	// owed, so it must not be dropped before the outcome above is recorded.
	if o.readAt.IsZero() {
		return
	}
	if err := w.db.ClearEmbeddingStale(w.ctx, t.ID, o.readAt); err != nil {
		w.log.Warn("clearing embedding_stale failed (chunk stays flagged; rebuilt again next cycle)",
			"transcript_id", t.ID, "error", err)
	}
}

// applyOverlay replays an overlay onto regenerated chunks.
//
// Pure: the overlay is an argument, nothing is queried, and the input slice is
// not modified. It rewrites only Text and leaves SourceText alone — SourceText
// is the projection's pristine input and the ONLY thing the judge is ever shown,
// so a replay must never touch it.
//
// Replay always starts from the pristine text, never from a previously
// corrected chunk. That is what makes a rebuild byte-identical no matter how
// many times it runs.
func applyOverlay(chunks []db.Chunk, o patch.Overlay) (out []db.Chunk, applied []patch.AppliedPatch, stale []patch.StaleRef) {
	// Always copy, even with nothing to replay. Returning the caller's slice
	// would alias the PRISTINE chunks the judge was shown, and the embed path
	// writes Embedding into every element of what it gets back — which would
	// then be writing through to the judge's copy.
	out = make([]db.Chunk, len(chunks))
	copy(out, chunks)
	if len(o) == 0 {
		return out, nil, nil
	}

	// Corrections whose chunk_index no longer corresponds to any chunk (the
	// transcript re-chunked into fewer chunks) have nothing to replay onto.
	seen := make(map[int]bool, len(out))
	for i := range out {
		seen[out[i].ChunkIndex] = true
		patches := o[out[i].ChunkIndex]
		if len(patches) == 0 {
			continue
		}
		res := patch.Replay(pristineText(out[i]), patches)
		out[i].Text = res.Text
		applied = append(applied, res.Applied...)
		stale = append(stale, res.Stale...)
	}
	orphaned := make([]int, 0, len(o))
	for idx := range o {
		if !seen[idx] {
			orphaned = append(orphaned, idx)
		}
	}
	slices.Sort(orphaned) // map iteration order must not leak into the output
	for _, idx := range orphaned {
		for _, p := range o[idx] {
			stale = append(stale, patch.StaleRef{ID: p.ID, Reason: patch.StaleReasonChunkChanged})
		}
	}
	return out, applied, stale
}

// pristineText returns the chunk's projection input.
//
// SourceText is authoritative and chunkTranscript always sets it, so the
// fallback is unreachable on the worker's own path. It is kept as a defence for
// any future caller that builds a db.Chunk without it (nothing SELECTs
// source_text into a db.Chunk today): replaying onto an empty string would
// quietly blank the chunk, whereas falling back to Text replays onto text that
// — for a chunk with no SourceText — carries no corrections and so IS pristine.
func pristineText(c db.Chunk) string {
	if c.SourceText != "" {
		return c.SourceText
	}
	return c.Text
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
