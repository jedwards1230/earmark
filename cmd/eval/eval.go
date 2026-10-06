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
// run_metrics (the eval slice). CONTRACT §2.15. It is also the judging pass for
// a deployment that runs with EVAL_IN_PIPELINE=false (eval decoupled from
// embed): schedule it and embedding never waits on a judge call.
//
// --backfill-eval-errors: re-judges done transcripts whose judging FAILED —
// including legacy ones the pre-fix pipeline latched as done anyway (found via
// eval_skipped > 0 and the pipeline_events eval error log). CONTRACT §2.15.
package eval

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/worker"
	"github.com/spf13/cobra"
)

// runner is the slice of internal/eval the command drives (kept small + an
// interface so the dry-run gating is unit-testable without a DB or LLM).
type runner interface {
	Run(ctx context.Context, opts evalpkg.RunOptions) ([]db.Finding, evalpkg.RunStats, error)
}

type options struct {
	sample              int  // judge a random sample of N chunks library-wide (instead of a book)
	limit               int  // cap chunks evaluated for a book / transcripts latched by a backfill (0 → default/all)
	maxAttempts         int  // backfill spend cap: transcripts judged, latched or not (0 → 3×limit, unbounded without --limit)
	write               bool // persist findings; without it the command is a dry-run preview
	backfillUnevaluated bool // judge ALL done transcripts with eval_finished_at IS NULL
	backfillEvalErrors  bool // re-judge done transcripts whose judging failed
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
it only writes to transcript_findings and run_metrics (the eval slice). Use it
to retroactively cover transcripts that were processed before EVAL_GATES_EMBED was
enabled, to retry judge runs that failed, and as the scheduled judging pass when
EVAL_IN_PIPELINE=false keeps the judge out of the embed path (CONTRACT §2.15, §1.5).

--backfill-eval-errors re-judges done transcripts whose judging failed: those
with a recorded failure (run_metrics.eval_failed_at), and LEGACY ones the old
pipeline latched as done despite a judge error (eval_skipped > 0, an eval error
event in pipeline_events, or a legacy run that judged fewer chunks than are
stored, with no chunk added since the run). Findings already recorded for a
transcript are not inserted twice.

In both backfill modes eval_finished_at is written only when EVERY chunk was
judged; otherwise the failure is recorded and the transcript stays eligible for
the next run. --limit N caps how many transcripts one run judges SUCCESSFULLY
(0 = all): skipped transcripts (empty text, not embedded yet) and failed ones do
not count, so a head of rows that never latch cannot stall every run.
--max-attempts N caps judge SPEND: every transcript the judge is called for,
latched or not (default 3×--limit; unbounded only when --limit is 0 too). A
--limit run also stops after 5 transcripts in a row fail on every chunk (judge
outage). A backfill that judged something but latched nothing exits non-zero,
so a dead endpoint or API key fails a scheduled Job instead of exiting 0.

Run 'earmark prune-chunks --yes' before a --backfill-* run: findings judged
against an orphan chunk tail that a later prune deletes are not retired.

Examples:
  earmark eval "Project Hail Mary"              # preview findings for one book
  earmark eval "Project Hail Mary" --write      # record them
  earmark eval --sample 50                      # preview a 50-chunk library sample
  earmark eval --sample 50 --write              # record a 50-chunk sample
  earmark eval --backfill-unevaluated           # preview backfill (dry-run)
  earmark eval --backfill-unevaluated --write   # backfill all unevaluated transcripts
  earmark eval --backfill-eval-errors --limit 10          # preview the first 10 re-judges
  earmark eval --backfill-eval-errors --limit 10 --write  # re-judge them`,
	Run: runEval,
}

func init() {
	EvalCmd.Flags().IntVar(&opts.sample, "sample", 0, "judge a random sample of N chunks library-wide")
	EvalCmd.Flags().IntVar(&opts.limit, "limit", 0, "max chunks to evaluate for a book (0 = default); with --backfill-*, max transcripts judged successfully (0 = all)")
	EvalCmd.Flags().IntVar(&opts.maxAttempts, "max-attempts", 0,
		"with --backfill-*, max transcripts the judge is called for, latched or not (0 = 3×--limit; unbounded only without --limit)")
	EvalCmd.Flags().BoolVar(&opts.write, "write", false, "persist findings (otherwise dry-run preview)")
	EvalCmd.Flags().BoolVar(&opts.write, "yes", false, "alias for --write")
	EvalCmd.Flags().BoolVar(&opts.backfillUnevaluated, "backfill-unevaluated", false,
		"judge ALL done transcripts with eval_finished_at IS NULL (regardless of embed state)")
	EvalCmd.Flags().BoolVar(&opts.backfillEvalErrors, "backfill-eval-errors", false,
		"re-judge done transcripts whose judging failed (incl. legacy runs latched despite an error)")
	EvalCmd.MarkFlagsMutuallyExclusive("backfill-unevaluated", "backfill-eval-errors")
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
	judge := evalpkg.NewJudgeForConfig(chat, cfg)

	// --backfill-unevaluated: a separate execution path that judges done transcripts
	// with eval_finished_at IS NULL (regardless of embed state). This is an offline
	// sweep over raw transcript text, not over transcript_chunks, so it uses a
	// different DB query and chunker path (CONTRACT §2.15).
	if opts.backfillUnevaluated || opts.backfillEvalErrors {
		mode := backfillUnevaluated
		if opts.backfillEvalErrors {
			mode = backfillEvalErrors
		}
		if opts.maxAttempts < 0 || opts.limit < 0 {
			fmt.Println("Error: --limit and --max-attempts must be >= 0")
			os.Exit(1)
		}
		bo := backfillOptions{mode: mode, write: opts.write, limit: opts.limit, maxAttempts: opts.maxAttempts}
		if err := runBackfill(context.Background(), os.Stdout, database, judge, cfg, bo); err != nil {
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
	// GetUnevaluatedJobTranscripts returns one keyset page of done transcripts
	// with eval_finished_at IS NULL, regardless of embed state.
	GetUnevaluatedJobTranscripts(ctx context.Context, after db.TranscriptCursor, limit int) ([]*db.Transcript, error)
	// GetEvalErrorTranscripts returns one keyset page of done transcripts whose
	// judging failed (incl. legacy latched-despite-error runs).
	GetEvalErrorTranscripts(ctx context.Context, after db.TranscriptCursor, limit int) ([]*db.Transcript, error)
	// GetEvalChunksForTranscript returns the transcript's stored chunk rows
	// (real IDs, pristine text); empty when it has not been embedded yet.
	GetEvalChunksForTranscript(ctx context.Context, transcriptID string) ([]db.EvalChunk, error)
	// GetFindingKeys returns the dedupe keys of the transcript's existing
	// findings, so a re-judge never inserts the same finding twice.
	GetFindingKeys(ctx context.Context, transcriptID string) (map[db.FindingKey]bool, error)
	// InsertFindings persists advisory judge findings.
	InsertFindings(ctx context.Context, findings []db.Finding) error
	// UpsertEvalMetrics records the attempt: the eval_finished_at latch on a
	// complete run, the failure record otherwise.
	UpsertEvalMetrics(ctx context.Context, m db.EvalMetrics) error
}

// backfillMode selects which transcripts a backfill run judges.
type backfillMode int

const (
	backfillUnevaluated backfillMode = iota // eval_finished_at IS NULL
	backfillEvalErrors                      // judging failed (incl. legacy latched errors)
)

// backfillOptions are the knobs for one runBackfill call.
type backfillOptions struct {
	mode  backfillMode
	write bool
	// limit is the progress target: transcripts judged successfully; 0 = all.
	limit int
	// maxAttempts caps the transcripts the judge is called for (latched or
	// not); 0 → 3×limit when limit > 0, unbounded when limit is 0.
	maxAttempts int
	// pageSize is the keyset page size (rows loaded per query); 0 → default.
	pageSize int
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
		if stats.ResolvedModel != "" {
			ev.Detail["resolved_model"] = stats.ResolvedModel
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

// defaultBackfillPageSize bounds the transcripts (each with its segments JSONB)
// loaded per selection query — mirroring EMBED_BATCH_SIZE's OOM guard.
const defaultBackfillPageSize = 32

// maxConsecutiveJudgeOutages is how many transcripts IN A ROW may fail on every
// chunk before a --limit run gives up. A transcript whose chunks ALL fail is
// the outage signature; one bad chunk among several (a poisoned transcript)
// is not, and never trips it. It is the fast exit for a dead endpoint; the
// spend cap that holds against EVERY failure mode is --max-attempts.
const maxConsecutiveJudgeOutages = 5

// defaultAttemptsPerLimit sizes the default --max-attempts: 3×--limit.
const defaultAttemptsPerLimit = 3

// effectiveMaxAttempts resolves --max-attempts: an explicit positive value
// wins; otherwise 3×limit when --limit is set, and unbounded (0) only when
// --limit is 0 too.
func effectiveMaxAttempts(limit, maxAttempts int) int {
	if maxAttempts > 0 {
		return maxAttempts
	}
	if limit > 0 {
		return defaultAttemptsPerLimit * limit
	}
	return 0
}

// backfillTally accumulates one run's outcome for the summary.
type backfillTally struct {
	seen, attempts, latched, failed, notEmbedded, skipped int
	totalChunks, totalFindings, totalSkipped, totalDupes  int
	limitReached, attemptsReached                         bool
}

// runBackfill judges every transcript the selected mode returns, walking the
// selection in keyset pages so memory stays bounded (and a dry run, whose rows
// never drop out of the selection, still terminates). For each transcript it:
//
//  1. Picks the chunks findings must reference: the stored transcript_chunks
//     rows when already embedded (their real IDs, which may be random from the
//     ungated path), else the embed worker's own chunking with deterministic
//     UUIDv5 IDs (CONTRACT §1.5) — the rows the gated embed pass will insert.
//  2. Runs the judge over the chunks.
//  3. Persists findings not already recorded for the transcript (a re-judge of
//     a partially successful earlier run would otherwise double them up).
//  4. Writes eval_finished_at ONLY if every chunk was judged and the findings
//     were stored; otherwise records the failure (eval_failed_at /
//     eval_failed_chunks / eval_error) so the next run picks it up again.
//
// Two bounds, two jobs:
//
//   - --limit (o.limit) is the PROGRESS target: transcripts judged
//     SUCCESSFULLY — latched, or in a dry run, that would latch. Skipped
//     transcripts (empty raw text, not embedded yet, a read error) and failed
//     ones do not count. A latched transcript leaves both selections for good,
//     so a --limit N run either latches N more or exhausts the candidate set.
//     Counting attempts instead lets a head of permanently failing or
//     unjudgeable rows — they never leave the selection, and keyset order puts
//     them first every run — use up the limit on every run and stall the
//     backfill forever.
//   - --max-attempts (o.maxAttempts, default 3×--limit) is the SPEND cap:
//     every transcript the judge was actually called for, latched or not.
//     Without it a flaky judge that fails some chunk of every transcript, or a
//     systematic write failure after judging, would turn --limit 25 into a
//     paid sweep of the whole selection.
//
// The cursor advances past EVERY row it visits, whatever its outcome, so
// within one run no row is selected twice and the run ends when a short page
// shows the selection is exhausted.
//
// The run returns an error — a non-zero exit, so a scheduled Job FAILS — when
// it judged something but latched nothing (failed > 0, latched == 0): a dead
// API key or endpoint must not hide behind exit 0.
//
// In dry-run mode (write=false) it prints what it would record but writes
// nothing. A cancelled context stops the sweep without recording anything for
// the in-flight transcript.
func runBackfill(ctx context.Context, out io.Writer, bdb backfillDB, judge *evalpkg.Judge, cfg *config.Config, o backfillOptions) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	selectPage := bdb.GetUnevaluatedJobTranscripts
	what := "done transcript(s) with eval_finished_at IS NULL"
	if o.mode == backfillEvalErrors {
		selectPage = bdb.GetEvalErrorTranscripts
		what = "done transcript(s) whose judging failed"
	}
	pageSize := o.pageSize
	if pageSize <= 0 {
		pageSize = defaultBackfillPageSize
	}
	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 512
	}
	maxAttempts := effectiveMaxAttempts(o.limit, o.maxAttempts)

	var tl backfillTally
	var outageRun int
	var cursor db.TranscriptCursor
walk:
	for {
		// Always load a full page: rows that are skipped or fail do not count
		// toward --limit, so the remaining limit says nothing about how many
		// rows this run still has to visit.
		page, err := selectPage(ctx, cursor, pageSize)
		if err != nil {
			return fmt.Errorf("query backfill transcripts: %w", err)
		}
		for _, t := range page {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			tl.seen++
			cursor = db.CursorAfter(t) // advance past every row, whatever its outcome
			res, err := backfillOne(ctx, p, bdb, judge, chunkSize, cfg.EvalGatesEmbed, t, o.write)
			if err != nil {
				return err
			}
			if res.judged {
				tl.attempts++
			}
			tl.totalChunks += res.stats.ChunksEvaluated
			tl.totalSkipped += res.stats.ChunksSkipped
			tl.totalFindings += res.newFindings
			tl.totalDupes += res.dupes
			switch {
			case res.latched:
				tl.latched++
			case res.failed:
				tl.failed++
			case res.notEmbedded:
				tl.notEmbedded++
			default:
				tl.skipped++
			}

			if res.stats.ChunksEvaluated == 0 && res.stats.ChunksSkipped > 0 {
				outageRun++
			} else if res.stats.ChunksEvaluated > 0 {
				outageRun = 0
			}
			if o.limit > 0 && outageRun >= maxConsecutiveJudgeOutages {
				p("\nStopping: the judge failed every chunk of %d transcripts in a row (endpoint down?) — last error: %s\n",
					outageRun, res.stats.FirstError)
				printBackfillSummary(p, what, o, maxAttempts, tl)
				return fmt.Errorf("judge outage: %d consecutive transcripts failed on every chunk", outageRun)
			}

			if o.limit > 0 && tl.latched >= o.limit {
				tl.limitReached = true
				break walk
			}
			if maxAttempts > 0 && tl.attempts >= maxAttempts {
				tl.attemptsReached = true
				break walk
			}
		}
		if len(page) < pageSize {
			break // selection exhausted
		}
	}

	if tl.seen == 0 {
		p("No %s — nothing to backfill.\n", what)
		return nil
	}
	printBackfillSummary(p, what, o, maxAttempts, tl)
	if tl.failed > 0 && tl.latched == 0 {
		return fmt.Errorf("no transcript latched: %d judged transcript(s) failed (judge endpoint, API key, or writes broken?)", tl.failed)
	}
	return nil
}

// printBackfillSummary prints the end-of-run report. In a dry run "latched"
// counts the transcripts that WOULD latch.
func printBackfillSummary(p func(string, ...any), what string, o backfillOptions, maxAttempts int, tl backfillTally) {
	p("\nBackfill: %d %s visited, %d judged.\n", tl.seen, what, tl.attempts)
	if tl.limitReached {
		p("Stopped at --limit %d (counts transcripts judged successfully; skipped and failed ones do not count).\n", o.limit)
	}
	if tl.attemptsReached {
		p("Stopped at --max-attempts %d (every transcript the judge was called for, latched or not).\n", maxAttempts)
	}
	if tl.notEmbedded > 0 {
		p("%d transcript(s) skipped: not embedded yet (EVAL_GATES_EMBED=false assigns chunk IDs at embed time); re-run after the embed worker catches up.\n", tl.notEmbedded)
	}
	if tl.skipped > 0 {
		p("%d transcript(s) skipped without a judge call (empty raw text, no chunks, or a read error) — left unlatched.\n", tl.skipped)
	}
	if !o.write {
		p("(dry-run) pass --write to record %d new finding(s); %d transcript(s) would latch, %d would stay unlatched (%d chunk(s) would be skipped by judge errors).\n",
			tl.totalFindings, tl.latched, tl.failed, tl.totalSkipped)
		return
	}
	p("Backfill complete: %d chunk(s) evaluated, %d new finding(s) recorded (%d already present), %d chunk(s) skipped; %d transcript(s) latched, %d left unlatched for retry.\n",
		tl.totalChunks, tl.totalFindings, tl.totalDupes, tl.totalSkipped, tl.latched, tl.failed)
}

// backfillResult is one transcript's backfill outcome.
type backfillResult struct {
	stats       evalpkg.RunStats
	newFindings int  // findings not already recorded for the transcript
	dupes       int  // findings skipped because they were already recorded
	judged      bool // the judge was called (counts toward --max-attempts)
	latched     bool // eval_finished_at written (dry run: would be written)
	failed      bool // judged but not latched: failure recorded, or a write failed
	notEmbedded bool // skipped: ungated and no stored chunks yet
	// None of the three set: skipped without a judge call (empty raw text, no
	// chunks produced, or a read error).
}

// backfillOne judges one transcript and (under write) records its outcome.
// It returns an error only when the context was cancelled.
//
// gated is cfg.EvalGatesEmbed. It decides what to do with a transcript that has
// no stored chunks yet: under the gate the embed pass will insert chunks with the
// deterministic UUIDv5 IDs, so judging regenerated chunks is safe; WITHOUT the
// gate the embed worker assigns RANDOM chunk IDs at insert time, so findings
// judged now would reference chunk IDs that will never exist (orphans — chunk_id
// has no FK) while the latch would stop the job from ever being judged again.
// Such a transcript is skipped and left unlatched; the next backfill run judges
// it once it has been embedded.
func backfillOne(ctx context.Context, p func(string, ...any), bdb backfillDB, judge *evalpkg.Judge,
	chunkSize int, gated bool, t *db.Transcript, write bool) (backfillResult, error) {
	var res backfillResult
	name := filepath.Base(t.FilePath)
	if t.RawText == "" {
		p("  skip %s (empty raw text)\n", name)
		return res, nil
	}

	// ORIGINAL-text consumer (CONTRACT §2.17 reader audit), and correct as-is:
	// the backfill judge must see PRISTINE text. raw_text is immutable
	// provenance that no Go code ever writes, and replay always starts from
	// pristine — so anchors and chunk_text_sha256 recorded here line up with
	// what the embed worker will replay onto. Feeding the judge the corrected
	// projection instead would make every backfilled finding stale on arrival.
	//
	// Every finding's chunk ID, anchors and chunk hash must refer to the exact
	// text stored under that ID:
	//   - Already embedded: judge the STORED rows (real IDs + pristine
	//     source text). Those rows may carry random IDs from the ungated
	//     worker path, so regenerated UUIDv5 IDs would not exist.
	//   - Not embedded yet: regenerate with the embed worker's own
	//     (segment-aware) chunking and deterministic UUIDs — the IDs the
	//     gated embed pass will insert, exactly as its own eval pass does.
	evalChunks, cerr := bdb.GetEvalChunksForTranscript(ctx, t.ID)
	if cerr != nil {
		p("  warn %s: read stored chunks failed (%v); skipping (will retry next backfill)\n", name, cerr)
		return res, nil
	}
	if len(evalChunks) == 0 {
		if !gated {
			res.notEmbedded = true
			p("  skip %s (not embedded yet; chunk IDs unknown until the embed worker inserts them — left unlatched for the next run)\n", name)
			return res, nil
		}
		chunks, perr := worker.PristineChunks(t, chunkSize)
		if perr != nil {
			p("  skip %s (no chunks produced)\n", name)
			return res, nil
		}
		evalChunks = worker.EvalChunksFor(t, chunks)
	}

	started := time.Now()
	findings, stats, jerr := evalpkg.RunOnChunks(ctx, judge, nil, evalChunks, false)
	finished := time.Now()
	res.judged = true
	res.stats = stats
	if jerr != nil {
		// RunOnChunks only errors when ctx was cancelled: stop, record nothing.
		return res, jerr
	}

	// Drop findings an earlier (partial) judge run already recorded.
	fresh := findings
	if len(findings) > 0 {
		existing, kerr := bdb.GetFindingKeys(ctx, t.ID)
		if kerr != nil {
			p("  warn %s: read existing findings failed (%v); skipping (will retry next backfill)\n", name, kerr)
			res.failed = true // judged, but nothing recorded
			return res, nil
		}
		fresh = make([]db.Finding, 0, len(findings))
		for _, f := range findings {
			if !existing[db.KeyOf(f)] {
				fresh = append(fresh, f)
			}
		}
	}
	res.newFindings = len(fresh)
	res.dupes = len(findings) - len(fresh)

	if !write {
		// A dry run reports would-latch / would-fail through the same flags,
		// so --limit bounds a preview exactly as it bounds the real run.
		state := "would latch"
		res.latched = true
		if !stats.Complete() {
			state = fmt.Sprintf("would stay unlatched: %d chunk(s) failed: %s", stats.ChunksSkipped, stats.FirstError)
			res.latched, res.failed = false, true
		}
		p("  [dry-run] %s: %d/%d chunks judged, %d new finding(s) (%d already recorded) — %s\n",
			name, stats.ChunksEvaluated, len(evalChunks), len(fresh), res.dupes, state)
		return res, nil
	}

	// Persist findings before the latch (same ordering discipline as the worker:
	// never set the latch before the evidence).
	var persistErr error
	if len(fresh) > 0 {
		if persistErr = bdb.InsertFindings(ctx, fresh); persistErr != nil {
			p("  warn %s: persist findings failed (%v)\n", name, persistErr)
		}
	}

	m := db.EvalMetrics{
		JobID:         t.JobID,
		StartedAt:     started,
		Model:         judge.Model(),
		ResolvedModel: stats.ResolvedModel,
		Chunks:        stats.ChunksEvaluated,
		Skipped:       stats.ChunksSkipped,
		// Findings recorded by THIS run — duplicates of an earlier partial
		// run's rows are not counted twice.
		Findings: len(fresh),
	}
	if stats.Complete() && persistErr == nil {
		m.FinishedAt = finished
	} else {
		m.FailedAt = finished
		m.FailedChunks = stats.ChunksSkipped
		var reasons []string
		if stats.ChunksSkipped > 0 {
			reasons = append(reasons, fmt.Sprintf("%d of %d chunks failed: %s",
				stats.ChunksSkipped, len(evalChunks), stats.FirstError))
		}
		if persistErr != nil {
			m.FailedChunks = len(evalChunks)
			reasons = append(reasons, "persist findings: "+persistErr.Error())
		}
		m.Error = strings.Join(reasons, "; ")
	}
	if merr := bdb.UpsertEvalMetrics(ctx, m); merr != nil {
		p("  warn %s: eval run_metrics write failed (%v); transcript will be re-judged on next backfill\n", name, merr)
		res.failed = true // judged, but the latch was not written
		return res, nil
	}
	if m.Failed() {
		res.failed = true
		p("  FAIL %s: %d/%d chunks judged, %d new finding(s) — left unlatched (%s)\n",
			name, stats.ChunksEvaluated, len(evalChunks), len(fresh), m.Error)
		return res, nil
	}
	res.latched = true
	p("  done %s: %d chunks, %d new finding(s) (%d already recorded), eval_finished_at written\n",
		name, stats.ChunksEvaluated, len(fresh), res.dupes)
	return res, nil
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
