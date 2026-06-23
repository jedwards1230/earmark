// Package eval implements the `earmark eval` command: a read-only LLM-as-judge
// pass that records SUSPECTED transcription errors as advisory findings without
// ever editing the transcripts (CONTRACT §2.15, issue #49).
//
// It mirrors `requeue`'s ergonomics: dry-run by default (prints what it would
// record), persisting only with --write/--yes. Cost is operator-bounded — judge
// a single book or a random sample of N chunks, never the whole library at once.
//
// --backfill-unevaluated: a special mode that selects ALL done transcripts whose
// eval_finished_at IS NULL (regardless of embed state) and judges them. It is
// safe to run over live data — it only writes to transcript_findings and
// run_metrics (eval_finished_at). CONTRACT §2.15.
package eval

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/jedwards1230/earmark/internal/chunker"
	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
	"github.com/spf13/cobra"
)

// runner is the slice of internal/eval the command drives (kept small + an
// interface so the dry-run gating is unit-testable without a DB or LLM).
type runner interface {
	Run(ctx context.Context, opts evalpkg.RunOptions) ([]db.Finding, evalpkg.RunStats, error)
}

type options struct {
	sample              int  // judge a random sample of N chunks library-wide (instead of a book)
	limit               int  // cap chunks evaluated for a book (0 → package default)
	write               bool // persist findings; without it the command is a dry-run preview
	backfillUnevaluated bool // judge ALL done transcripts with eval_finished_at IS NULL
}

var opts options

var EvalCmd = &cobra.Command{
	Use:   "eval [flags] [book-substring]",
	Short: "Read-only LLM judge: flag suspected transcript errors (dry-run unless --write)",
	Long: `Run a read-only LLM-as-judge over transcript chunks and record SUSPECTED
errors as advisory findings. The transcripts are NEVER edited — findings are
advisory metadata you triage by confidence (CONTRACT §2.15).

Cost is bounded: evaluate one book, or a random --sample of N chunks. Nothing is
recorded unless you pass --write (alias --yes).

Endpoint: bind AI_ROLES.eval to a chat AI_ENDPOINTS entry (preferred), or set
EVAL_CHAT_BASE_URL and EVAL_CHAT_MODEL (OpenAI-compatible chat endpoint, e.g.
vLLM) as a fallback. EVAL_CHAT_API_KEY is optional.

--backfill-unevaluated judges ALL done transcripts whose eval_finished_at IS NULL
regardless of whether they have been embedded. It is safe to run over live data;
it only writes to transcript_findings and run_metrics (eval_finished_at). Use it
to retroactively cover transcripts that were processed before EVAL_GATES_EMBED was
enabled (CONTRACT §2.15, §1.5).

Examples:
  earmark eval "Project Hail Mary"              # preview findings for one book
  earmark eval "Project Hail Mary" --write      # record them
  earmark eval --sample 50                      # preview a 50-chunk library sample
  earmark eval --sample 50 --write              # record a 50-chunk sample
  earmark eval --backfill-unevaluated           # preview backfill (dry-run)
  earmark eval --backfill-unevaluated --write   # backfill all unevaluated transcripts`,
	Run: runEval,
}

func init() {
	EvalCmd.Flags().IntVar(&opts.sample, "sample", 0, "judge a random sample of N chunks library-wide")
	EvalCmd.Flags().IntVar(&opts.limit, "limit", 0, "max chunks to evaluate for a book (0 = default)")
	EvalCmd.Flags().BoolVar(&opts.write, "write", false, "persist findings (otherwise dry-run preview)")
	EvalCmd.Flags().BoolVar(&opts.write, "yes", false, "alias for --write")
	EvalCmd.Flags().BoolVar(&opts.backfillUnevaluated, "backfill-unevaluated", false,
		"judge ALL done transcripts with eval_finished_at IS NULL (regardless of embed state)")
}

func runEval(cmd *cobra.Command, args []string) {
	book := ""
	if len(args) > 0 {
		book = args[0]
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	database, err := db.New(cfg)
	if err != nil {
		fmt.Printf("Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	// Resolve the chat endpoint: AI_ROLES["eval"] from the registry when set,
	// else the standalone EVAL_CHAT_* env vars (#48 resolved).
	chat, err := evalpkg.ResolveChatClient(evalpkg.ConfigSource(cfg))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	judge := evalpkg.NewJudge(chat)

	// --backfill-unevaluated: a separate execution path that judges done transcripts
	// with eval_finished_at IS NULL (regardless of embed state). This is an offline
	// sweep over raw transcript text, not over transcript_chunks, so it uses a
	// different DB query and chunker path (CONTRACT §2.15).
	if opts.backfillUnevaluated {
		if err := runBackfill(context.Background(), os.Stdout, database, judge, cfg, opts.write); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	r := &dbRunner{reader: database, judge: judge, writer: database, events: database}
	if err := run(context.Background(), os.Stdout, r, book, opts); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

// backfillDB is the narrow slice of db.DB the backfill execution path needs.
// It is a proper interface so the backfill path is testable without a live DB.
type backfillDB interface {
	// GetUnevaluatedJobTranscripts returns done transcripts with eval_finished_at
	// IS NULL, regardless of embed state. Used by --backfill-unevaluated.
	GetUnevaluatedJobTranscripts(ctx context.Context) ([]*db.Transcript, error)
	// InsertFindings persists advisory judge findings (best-effort in backfill).
	InsertFindings(ctx context.Context, findings []db.Finding) error
	// UpsertEvalMetrics writes eval_finished_at (the eval-completion latch) plus
	// per-run counts. Writing it is what makes a transcript "covered" by the
	// backfill pass. Best-effort in backfill: a write failure is logged + skipped.
	UpsertEvalMetrics(ctx context.Context, m db.EvalMetrics) error
}

// dbRunner adapts the DB + judge to the runner interface.
type dbRunner struct {
	reader evalpkg.ChunkReader
	judge  *evalpkg.Judge
	writer evalpkg.FindingWriter
	// events records a book/sample-level eval pipeline_event (CONTRACT §1.7). The
	// standalone eval path covers many jobs (a whole book or a library sample), so
	// it emits ONE job_id=NULL eval event rather than the per-job run_metrics slice
	// (which only the in-pipeline worker writes). nil → no event (e.g. tests).
	events db.EventAppender
}

func (d *dbRunner) Run(ctx context.Context, o evalpkg.RunOptions) ([]db.Finding, evalpkg.RunStats, error) {
	start := time.Now()
	findings, stats, err := evalpkg.Run(ctx, d.reader, d.judge, d.writer, o)
	if err != nil {
		return findings, stats, err
	}
	// Best-effort audit event for the standalone eval run. job_id is NULL (the run
	// spans many jobs); file_path carries the book scope when scoped to one.
	if d.events != nil {
		ev := db.PipelineEvent{
			Stage:      db.StageEval,
			Event:      db.EventFinish,
			RunnerHost: db.HostGoMonitor,
			Model:      d.judge.Model(),
			DurationMS: db.Int64Ptr(time.Since(start).Milliseconds()),
			ItemCount:  db.IntPtr(stats.FindingsFound),
			Detail: map[string]any{
				"evaluated": stats.ChunksEvaluated,
				"skipped":   stats.ChunksSkipped,
				"scope":     "standalone",
				"sample":    o.Sample,
				"book":      o.Book,
				"persisted": stats.Persisted,
			},
		}
		if o.Book != "" {
			ev.FilePath = o.Book
		}
		if aerr := d.events.AppendEvent(ctx, ev); aerr != nil {
			fmt.Printf("warning: eval pipeline event write failed (continuing): %v\n", aerr)
		}
	}
	return findings, stats, err
}

// runBackfill judges every done transcript whose eval_finished_at IS NULL,
// regardless of embed state. It chunks transcripts from raw text in memory
// (not from transcript_chunks), so it works even when a transcript was processed
// before the gated flow was enabled. For each transcript it:
//
//  1. Chunks the raw text using deterministic UUIDv5 IDs (CONTRACT §1.5) so
//     findings reference the chunk rows that the embed pass will/did insert.
//  2. Runs the judge over the chunks.
//  3. Persists findings (best-effort — a write failure is logged and skipped).
//  4. Writes eval_finished_at via UpsertEvalMetrics (the latch for the gate).
//
// This is a sweep command — it is NOT bounded by book or sample. In dry-run mode
// (write=false) it prints what it would record but writes nothing.
func runBackfill(ctx context.Context, out io.Writer, bdb backfillDB, judge *evalpkg.Judge, cfg *config.Config, write bool) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	transcripts, err := bdb.GetUnevaluatedJobTranscripts(ctx)
	if err != nil {
		return fmt.Errorf("query unevaluated transcripts: %w", err)
	}

	if len(transcripts) == 0 {
		p("No unevaluated done transcripts found — nothing to backfill.\n")
		return nil
	}
	p("Backfill: %d done transcript(s) with eval_finished_at IS NULL.\n", len(transcripts))

	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 512
	}

	var totalChunks, totalFindings, totalSkipped int
	for _, t := range transcripts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if t.RawText == "" {
			p("  skip %s (empty raw text)\n", filepath.Base(t.FilePath))
			continue
		}

		// Build EvalChunks from raw text with deterministic UUIDs so findings
		// reference the same chunk IDs that the embed pass used/will use.
		var evalChunks []db.EvalChunk
		texts := chunker.Chunker(t.RawText, chunkSize, chunker.SplitTypeToken)
		for i, text := range texts {
			evalChunks = append(evalChunks, db.EvalChunk{
				ChunkID:            db.ChunkUUID(t.ID, i),
				TranscriptID:       t.ID,
				TranscriptionRunID: t.JobID,
				FilePath:           t.FilePath,
				ChunkIndex:         i,
				Text:               text,
			})
		}
		if len(evalChunks) == 0 {
			p("  skip %s (no chunks produced)\n", filepath.Base(t.FilePath))
			continue
		}

		started := time.Now()
		findings, stats, jerr := evalpkg.RunOnChunks(ctx, judge, nil, evalChunks, false)
		finished := time.Now()
		if jerr != nil {
			p("  warn %s: judge error (%v); writing eval_finished_at=0 findings\n",
				filepath.Base(t.FilePath), jerr)
		}
		totalChunks += stats.ChunksEvaluated
		totalFindings += stats.FindingsFound
		totalSkipped += stats.ChunksSkipped

		if !write {
			p("  [dry-run] %s: %d chunks, %d findings\n",
				filepath.Base(t.FilePath), stats.ChunksEvaluated, stats.FindingsFound)
			continue
		}

		// Persist findings before eval_finished_at (same ordering discipline as
		// the worker's evalTranscript: never set the latch before the evidence).
		if len(findings) > 0 {
			if ferr := bdb.InsertFindings(ctx, findings); ferr != nil {
				p("  warn %s: persist findings failed (%v); writing eval_finished_at anyway\n",
					filepath.Base(t.FilePath), ferr)
			}
		}

		// Write eval_finished_at — the latch that marks this transcript as covered.
		m := db.EvalMetrics{
			JobID:      t.JobID,
			StartedAt:  started,
			FinishedAt: finished,
			Model:      judge.Model(),
			Chunks:     stats.ChunksEvaluated,
			Skipped:    stats.ChunksSkipped,
			Findings:   stats.FindingsFound,
		}
		if merr := bdb.UpsertEvalMetrics(ctx, m); merr != nil {
			p("  warn %s: eval_finished_at write failed (%v); transcript will be re-judged on next backfill\n",
				filepath.Base(t.FilePath), merr)
			continue
		}
		p("  done %s: %d chunks, %d findings, eval_finished_at written\n",
			filepath.Base(t.FilePath), stats.ChunksEvaluated, stats.FindingsFound)
	}

	if !write {
		p("\n(dry-run) pass --write to record %d finding(s) across %d transcript(s).\n",
			totalFindings, len(transcripts))
		return nil
	}

	p("\nBackfill complete: %d chunk(s) evaluated, %d finding(s) recorded, %d chunk(s) skipped.\n",
		totalChunks, totalFindings, totalSkipped)
	return nil
}

// run holds the testable logic: validate flags, run the judge, and report.
// In dry-run (no --write) it prints what it would record and persists nothing.
func run(ctx context.Context, out io.Writer, r runner, book string, o options) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	if o.sample <= 0 && book == "" {
		return fmt.Errorf("provide a book substring, or use --sample N")
	}
	if o.sample > 0 && book != "" {
		return fmt.Errorf("--sample and a book substring cannot be combined")
	}

	runOpts := evalpkg.RunOptions{
		Book:   book,
		Sample: o.sample,
		Limit:  o.limit,
		Write:  o.write,
	}

	findings, stats, err := r.Run(ctx, runOpts)
	if err != nil {
		return err
	}

	scope := fmt.Sprintf("book %q", book)
	if o.sample > 0 {
		scope = fmt.Sprintf("a %d-chunk sample", o.sample)
	}
	p("Evaluated %d chunk(s) from %s — %d suspected error(s) found.\n",
		stats.ChunksEvaluated, scope, stats.FindingsFound)
	if stats.ChunksSkipped > 0 {
		p("(%d chunk(s) skipped due to transient judge errors — partial results below.)\n", stats.ChunksSkipped)
	}

	for _, f := range findings {
		conf := f.Confidence
		p("  [%.2f] %-22s %s — %q\n", conf, f.IssueType, filepath.Base(f.FilePath), truncate(f.OriginalText, 60))
	}

	if !o.write {
		p("\n(dry-run) pass --write to record these %d finding(s).\n", stats.FindingsFound)
		return nil
	}
	if stats.Persisted {
		p("\nRecorded %d finding(s).\n", stats.FindingsFound)
	} else {
		p("\nNo findings to record.\n")
	}
	return nil
}

// truncate shortens a string to n runes for the preview line, appending an
// ellipsis. It slices by rune (not byte) so a multi-byte codepoint — accented
// proper nouns, CJK, emoji — is never split mid-encoding.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
