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
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/runs"
	"github.com/jedwards1230/earmark/internal/telemetry"
	"github.com/jedwards1230/earmark/internal/worker"
	"github.com/spf13/cobra"
)

// runner is the slice of internal/eval the command drives (kept small + an
// interface so the dry-run gating is unit-testable without a DB or LLM).
type runner interface {
	Run(ctx context.Context, opts evalpkg.RunOptions) ([]db.Finding, evalpkg.RunStats, error)
}

type options struct {
	sample              int    // judge a random sample of N chunks library-wide (instead of a book)
	limit               int    // cap chunks evaluated for a book / transcripts latched by a backfill (0 → default/all)
	maxAttempts         int    // backfill spend cap: transcripts judged, latched or not (0 → 3×limit, unbounded without --limit)
	write               bool   // persist findings; without it the command is a dry-run preview
	backfillUnevaluated bool   // judge ALL done transcripts with eval_finished_at IS NULL
	backfillEvalErrors  bool   // re-judge done transcripts whose judging failed
	seed                string // with --sample: fix which chunks the sample picks
	dump                string // dry run only: write one JSONL line per judged chunk here ("-" = stdout)
	// observe is the per-chunk hook --dump installs (not a flag).
	observe func(evalpkg.Result, error)
	// steps records the run's progress in step_runs (CONTRACT §1.10); model
	// is the judge model it names. nil steps (tests) records nothing.
	steps runs.Store
	model string
	// overrideEndpoint / overrideModel are --endpoint / --model: the
	// propose step's model for this run only (CONTRACT §2.20).
	overrideEndpoint string
	overrideModel    string
	// judgePrompt is --judge-prompt: the judge prompt version (v2 | v3;
	// "" = the propose pin's judge_prompt param, else the default v2).
	// CONTRACT §2.15 "Book context".
	judgePrompt string
	// callTimeout / retries are the judge's call policy, every mode
	// (CONTRACT §2.15 "Judge calls").
	callTimeout time.Duration
	retries     int
	// rejudge mode (CONTRACT §2.15 "Re-judge").
	rejudge      bool
	expectRecipe string
	concurrency  int
	book         string // --book: same as the positional book substring
	shard        string // "i/N"
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
stored, with no chunk added since the run). A finding the same judge recipe
already recorded for the transcript is not inserted twice; an identical finding
from another recipe is (that agreement is recorded). Under --write each
transcript is recorded as a judge pass (judge_passes).

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
  earmark eval --backfill-eval-errors --limit 10 --write  # re-judge them

Measuring a prompt change (dry run only; CONTRACT §2.15):
  earmark eval --sample 200 --seed q4 --dump /tmp/judge.jsonl
--seed fixes which chunks --sample picks (same seed, same library → same
chunks), so two builds judge identical input. --dump writes one JSON line per
chunk: tokens and cost as the endpoint reports them, and every finding the
judge returned — kept, or dropped with its reason. "--dump -" writes the lines
to stdout and the report to stderr.

Model: the judge runs on the propose step's effective model (CONTRACT §2.20):
--endpoint/--model for this run, else the model_settings row ('earmark
models set propose ...'), else AI_ROLES.eval. --model alone keeps the
endpoint; --endpoint alone uses that endpoint's own model. A different model
is a different propose recipe. Neither flag writes model_settings.
  earmark eval --sample 200 --seed q4 --dump /tmp/qwen.jsonl --model ollama/qwen3.8:27b-q8_0

Re-judge (CONTRACT §2.15 "Re-judge"): --rejudge judges every done transcript
that has stored chunks and no 'complete' judge pass under the CURRENT judge
recipe (or one equivalent to it), in keyset pages, with --concurrency judge
calls in flight across all transcripts (default 16). --limit N caps the
transcripts dispatched; --book narrows by path; --shard i/N runs one of N
disjoint partitions (decide --shard's) — concurrent runs need disjoint shards. --expect-recipe <id> refuses to start
unless the current judge recipe is <id> — pin it so a resumed run cannot
silently judge under a changed model or prompt. Each chunk is looked up in
fn_calls first, so a killed run resumes without paying again. With --write each
transcript gets a judge_passes row (opened 'running', closed complete/partial/
failed with its findings and the run_metrics latch in one transaction).
WITHOUT --write it still makes PAID model calls: nothing but fn_calls is
written (the replies are cached there, so the --write run that follows serves
them without paying again). Stop with Ctrl-C: in-flight passes are closed as
partial with what was judged. Runs from a local binary against the database;
memory holds at most --concurrency transcripts' chunks.
  earmark eval --rejudge --limit 5                         # preview 5 transcripts (paid, cached)
  earmark eval --rejudge --write --expect-recipe <id>      # the full re-judge
  earmark eval --rejudge --write --expect-recipe <id> --shard 0/2

Judge calls (every mode): --call-timeout bounds each model call (default 3m),
--retries retries a call that failed transiently — 429/5xx (honouring
Retry-After), a timeout, a transport error — with exponential backoff (default
4). A reply that is not valid findings JSON is never retried: that chunk FAILS
(error_class invalid_reply) and the transcript is not latched. Every call is
logged in fn_calls (fn judge_chunk) and a chunk this judge already answered is
served from there.

Prompt: --judge-prompt selects the judge prompt version for this run (CONTRACT
§2.15 "Book context"); without it the propose pin's judge_prompt param decides
('earmark models set propose ... --param judge_prompt=v3'), else v2. v2 (the
default) is the path-only prompt. v3 (opt-in until a labelled A/B shows it is
better) adds the book's catalogue record — title, author, series, narrator and
up to 40 names — to each chunk's message when the book's identity is resolved
(an ASIN); other books get the path-only message. --dump records
prompt_version, book_context_source and book_context_sha256 per chunk:
  earmark eval --seed q4 --sample 300 --dump /tmp/v2.jsonl --judge-prompt v2
  earmark eval --seed q4 --sample 300 --dump /tmp/v3.jsonl --judge-prompt v3`,
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
	EvalCmd.Flags().StringVar(&opts.seed, "seed", "",
		"with --sample: pick the sample in an order fixed by this seed (same seed → same chunks)")
	EvalCmd.Flags().StringVar(&opts.dump, "dump", "",
		"dry run only: write one JSON line per judged chunk (tokens, cost, kept and dropped findings) to this path; - = stdout")
	EvalCmd.Flags().StringVar(&opts.overrideEndpoint, "endpoint", "",
		"this run only: the AI_ENDPOINTS id (a chat endpoint) the judge calls (default: the propose step's effective endpoint)")
	EvalCmd.Flags().StringVar(&opts.overrideModel, "model", "",
		"this run only: the model id the judge requests (default: the propose step's effective model)")
	EvalCmd.Flags().StringVar(&opts.judgePrompt, "judge-prompt", "",
		"judge prompt version for this run: v2 (default; path only) or v3 (opt-in; adds the book's catalogue record). Default: the propose pin's judge_prompt param, else v2")
	EvalCmd.Flags().DurationVar(&opts.callTimeout, "call-timeout", evalpkg.DefaultCallTimeout,
		"deadline of one judge model call (every mode)")
	EvalCmd.Flags().IntVar(&opts.retries, "retries", evalpkg.DefaultRetries,
		"retries of a judge call that failed transiently (429/5xx honouring Retry-After, timeout, transport error); never an invalid reply (every mode)")
	EvalCmd.Flags().BoolVar(&opts.rejudge, "rejudge", false,
		"judge every done transcript with chunks that has no complete judge pass under the current judge recipe (PAID model calls even without --write)")
	EvalCmd.Flags().StringVar(&opts.expectRecipe, "expect-recipe", "",
		"with --rejudge: refuse to start unless the current judge recipe is this id (or equivalent to it)")
	EvalCmd.Flags().IntVar(&opts.concurrency, "concurrency", 16,
		"with --rejudge: max judge calls in flight, across every transcript of the run")
	EvalCmd.Flags().StringVar(&opts.book, "book", "",
		"only transcripts/chunks whose path contains this (case-insensitive); same as the positional book substring")
	EvalCmd.Flags().StringVar(&opts.shard, "shard", "",
		"with --rejudge: i/N — only transcripts with hashtext(transcript_id) mod N = i (decide --shard's partition)")
	EvalCmd.MarkFlagsMutuallyExclusive("backfill-unevaluated", "backfill-eval-errors", "rejudge")
	EvalCmd.MarkFlagsMutuallyExclusive("rejudge", "sample")
	EvalCmd.MarkFlagsMutuallyExclusive("rejudge", "dump")
	EvalCmd.MarkFlagsMutuallyExclusive("rejudge", "seed")
	EvalCmd.MarkFlagsMutuallyExclusive("rejudge", "max-attempts")
}

func runEval(cmd *cobra.Command, args []string) {
	book := opts.book
	if len(args) > 0 {
		if book != "" && book != args[0] {
			fmt.Fprintln(os.Stderr, "Error: pass the book either as --book or as the argument, not both")
			os.Exit(2)
		}
		book = args[0]
	}
	if err := validateCallFlags(opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
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

	// The propose step's effective model (CONTRACT §2.20): --endpoint/--model,
	// else its model_settings row, else the env registry. An invalid row fails
	// the run rather than judging with another model.
	cfg, err = db.ResolveStep(context.Background(), database, cfg, recipe.StepPropose,
		opts.overrideEndpoint, opts.overrideModel)
	if err != nil {
		// stderr: with --dump - stdout is the JSONL stream.
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	log.Print(cfg.DescribeStep(recipe.StepPropose))

	// Resolve the chat endpoint: AI_ROLES["eval"] from the registry when set,
	// else the standalone EVAL_CHAT_* env vars (#48 resolved).
	chat, err := evalpkg.ResolveChatClient(evalpkg.ConfigSource(cfg))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	// The prompt version: --judge-prompt for this run, else the propose pin's
	// judge_prompt param (applied by NewJudgeForConfig), else the default v2.
	judge := evalpkg.NewJudgeForConfig(chat, cfg)
	if opts.judgePrompt != "" {
		prompt, perr := evalpkg.ParseJudgePrompt(opts.judgePrompt)
		if perr == nil {
			perr = judge.SetPrompt(prompt)
		}
		if perr != nil {
			fmt.Fprintf(os.Stderr, "Error: --judge-prompt: %v\n", perr)
			os.Exit(2)
		}
	}
	// judge@v3 reads each book's catalogue record (book_metadata) for its
	// book context; a read error fails the run rather than judging path-only.
	judge.SetBookRecords(database)
	// Every judge call is logged in fn_calls (fn judge_chunk) and an answered
	// chunk is served from there, in every mode (CONTRACT §2.15 "Judge calls").
	judge.SetCallLog(database)
	judge.SetCallPolicy(evalpkg.CallPolicy{Timeout: opts.callTimeout, Retries: opts.retries})
	log.Printf("judge prompt %s", judge.Prompt().Version())

	// OpenTelemetry (CONTRACT §2.16): judge calls emit gen_ai spans when an
	// OTLP endpoint is configured. Flushed before the process exits.
	tel, terr := telemetry.Setup(context.Background())
	if terr != nil {
		log.Printf("WARNING: OpenTelemetry disabled: %v", terr)
	}
	code := runJudge(database, judge, cfg, book)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := tel.Shutdown(ctx); err != nil {
		log.Printf("OpenTelemetry shutdown: %v", err)
	}
	cancel()
	if code != 0 {
		database.Close()
		os.Exit(code)
	}
}

// runJudge runs the selected eval mode and returns the process exit code.
func runJudge(database *db.DB, judge *evalpkg.Judge, cfg *config.Config, book string) int {
	if opts.rejudge {
		ro, err := rejudgeOptionsFrom(opts, book)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := runRejudge(ctx, os.Stdout, database, judge, ro); err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		return 0
	}
	if opts.expectRecipe != "" || opts.shard != "" {
		fmt.Fprintln(os.Stderr, "Error: --expect-recipe and --shard apply to --rejudge only")
		return 2
	}
	// --backfill-unevaluated: a separate execution path that judges done transcripts
	// with eval_finished_at IS NULL (regardless of embed state). This is an offline
	// sweep over raw transcript text, not over transcript_chunks, so it uses a
	// different DB query and chunker path (CONTRACT §2.15).
	if opts.backfillUnevaluated || opts.backfillEvalErrors {
		mode := backfillUnevaluated
		if opts.backfillEvalErrors {
			mode = backfillEvalErrors
		}
		if opts.seed != "" || opts.dump != "" {
			fmt.Println("Error: --seed and --dump apply to --sample/book runs, not --backfill-*")
			os.Exit(1)
		}
		if opts.maxAttempts < 0 || opts.limit < 0 {
			fmt.Println("Error: --limit and --max-attempts must be >= 0")
			os.Exit(1)
		}
		bo := backfillOptions{mode: mode, write: opts.write, limit: opts.limit, maxAttempts: opts.maxAttempts}
		if err := runBackfill(context.Background(), os.Stdout, database, judge, cfg, bo); err != nil {
			fmt.Printf("Error: %v\n", err)
			return 1
		}
		return 0
	}

	return runSelected(database, judge, book, opts)
}

// runSelected runs a book or sample judge pass, with the optional --dump
// harness, and returns the process exit code.
func runSelected(database *db.DB, judge *evalpkg.Judge, book string, o options) int {
	out := io.Writer(os.Stdout)
	var dump *dumper
	if o.dump != "" {
		if o.write {
			fmt.Println("Error: --dump is a dry-run measurement; it cannot be combined with --write")
			return 1
		}
		w := io.Writer(os.Stdout)
		if o.dump == "-" {
			out = os.Stderr // stdout carries the JSON lines
		} else {
			f, err := os.Create(o.dump)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				return 1
			}
			defer func() {
				if cerr := f.Close(); cerr != nil {
					fmt.Fprintf(os.Stderr, "Error: close dump: %v\n", cerr)
				}
			}()
			w = f
		}
		dump = newDumper(w, judge.Model(), judge.Recipe())
		o.observe = dump.observe
	}

	r := &dbRunner{reader: database, judge: judge, writer: database, events: database}
	o.steps, o.model = database, judge.Model()
	if err := run(context.Background(), out, r, book, o); err != nil {
		_, _ = fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}
	if dump != nil && dump.err != nil {
		_, _ = fmt.Fprintf(out, "Error: %v\n", dump.err)
		return 1
	}
	return 0
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
				"evaluated":      stats.ChunksEvaluated,
				"skipped":        stats.ChunksSkipped,
				"scope":          "standalone",
				"sample":         o.Sample,
				"book":           o.Book,
				"persisted":      stats.Persisted,
				"prompt_version": stats.PromptVersion,
			},
		}
		if stats.ResolvedModel != "" {
			ev.Detail["resolved_model"] = stats.ResolvedModel
		}
		if o.Seed != "" {
			ev.Detail["seed"] = o.Seed
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
func runBackfill(ctx context.Context, out io.Writer, bdb backfillDB, judge *evalpkg.Judge, cfg *config.Config, o backfillOptions) (err error) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	// Recorded in step_runs (CONTRACT §1.10) when bdb is the real database;
	// recording never fails the backfill. No total: --limit counts latched
	// transcripts, not visited ones, so a ratio would mislead.
	mode := db.StepRunDryRun
	if o.write {
		mode = db.StepRunWrite
	}
	rec := runs.StartIfStore(ctx, bdb, runs.Spec{
		Step: runs.StepEvalBackfill, Mode: mode, Model: judge.Model(), Args: o.summary(),
	})
	defer func() { rec.Finish(err) }()

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
			rec.Phase(fmt.Sprintf("judging %s · asking %s", filepath.Base(t.FilePath), judge.Model()))
			res, err := backfillOne(ctx, p, bdb, judge, chunkSize, cfg.EvalGatesEmbed, t, o.write, rec.ID())
			if err != nil {
				return err
			}
			tickBackfill(rec, res)
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

// summary is the step_runs.args line of a backfill.
func (o backfillOptions) summary() string {
	s := "--backfill-unevaluated"
	if o.mode == backfillEvalErrors {
		s = "--backfill-eval-errors"
	}
	if o.limit > 0 {
		s += fmt.Sprintf(" --limit %d", o.limit)
	}
	if o.maxAttempts > 0 {
		s += fmt.Sprintf(" --max-attempts %d", o.maxAttempts)
	}
	if o.write {
		s += " --write"
	}
	return s
}

// tickBackfill records one visited transcript: its outcome (latched, failed,
// not_embedded, skipped), the chunks judged and failed, and new findings.
func tickBackfill(rec *runs.Run, res backfillResult) {
	if rec == nil {
		return
	}
	outcome := "skipped"
	switch {
	case res.latched:
		outcome = "latched"
	case res.failed:
		outcome = "failed"
	case res.notEmbedded:
		outcome = "not_embedded"
	}
	rec.Tick(outcome, 0)
	rec.Count("chunks", int64(res.stats.ChunksEvaluated))
	rec.Count("chunk_errors", int64(res.stats.ChunksSkipped))
	rec.Count("findings", int64(res.newFindings))
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
	chunkSize int, gated bool, t *db.Transcript, write bool, stepRunID int64) (backfillResult, error) {
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

	// Under --write against a store that records judge passes (the real
	// database), the pass is opened as 'running' before the first call and
	// closed with the findings and the latch in one transaction
	// (backfillCommit) — CONTRACT §2.15 "Lineage".
	passes, _ := bdb.(evalpkg.PassStore)
	var passID int64
	if write && passes != nil {
		var perr error
		passID, perr = judge.OpenPass(ctx, passes, evalpkg.PassTarget{TranscriptID: t.ID, JobID: t.JobID,
			FilePath: t.FilePath}, len(evalChunks), stepRunID)
		if perr != nil {
			p("  warn %s: open judge pass failed (%v); skipping (will retry next backfill)\n", name, perr)
			return res, nil
		}
	}

	started := time.Now()
	findings, stats, jerr := evalpkg.RunOnChunks(ctx, judge, nil, evalChunks, false)
	finished := time.Now()
	res.judged = true
	res.stats = stats
	if jerr != nil {
		// RunOnChunks errors when ctx was cancelled, or when the book record
		// read behind judge@v3's book context failed (before any judge call):
		// stop, record nothing — never judge path-only on a broken read. An
		// open pass is closed as failed (detached: the run may be stopping).
		if passID != 0 {
			closeStoppedPass(ctx, p, passes, passID, t.ID, name, findings, stats, len(evalChunks), jerr)
		}
		return res, jerr
	}
	if passID != 0 {
		return backfillCommit(ctx, p, passes, judge, t, passID, findings, stats, len(evalChunks), started, finished, res)
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
		p("  [dry-run] %s: %d/%d chunks judged, %d new finding(s) (%d already recorded), %s — %s\n",
			name, stats.ChunksEvaluated, len(evalChunks), len(fresh), res.dupes, bookContextLabel(stats, t.ID), state)
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

// backfillCommit closes a backfill transcript's judge pass: the findings not
// already recorded by this recipe, the pass's closing record and the
// run_metrics latch (or failure record), in one transaction.
func backfillCommit(ctx context.Context, p func(string, ...any), passes evalpkg.PassStore, judge *evalpkg.Judge,
	t *db.Transcript, passID int64, findings []db.Finding, stats evalpkg.RunStats, total int,
	started, finished time.Time, res backfillResult) (backfillResult, error) {
	name := filepath.Base(t.FilePath)
	cl := evalpkg.PassClose(stats, total, nil)
	m := evalMetricsFor(t.JobID, judge.Model(), stats, cl, total, started, finished)
	stored, err := passes.CommitJudgePass(ctx, db.JudgePassCommit{PassID: passID, TranscriptID: t.ID,
		Findings: findings, Close: cl, Metrics: &m})
	if err != nil {
		p("  warn %s: recording the judge pass failed (%v); pass %d left running, transcript will be re-judged on next backfill\n",
			name, err, passID)
		res.failed = true
		return res, nil
	}
	res.newFindings, res.dupes = stored.Inserted, stored.Duplicates
	if cl.Status != db.JudgePassComplete {
		res.failed = true
		p("  FAIL %s: %d/%d chunks judged, %d new finding(s) — pass %d %s, left unlatched (%s)\n",
			name, stats.ChunksEvaluated, total, stored.Inserted, passID, cl.Status, cl.Error)
		return res, nil
	}
	res.latched = true
	p("  done %s: %d chunks (%d cached), %d new finding(s) (%d already recorded), pass %d complete, eval_finished_at written\n",
		name, stats.ChunksEvaluated, stats.ChunksCached, stored.Inserted, stored.Duplicates, passID)
	return res, nil
}

// closeStoppedPass closes a pass whose judging stopped early (a cancelled run,
// an unreadable book record): partial or failed, with the findings of the
// chunks judged so far, the latch untouched. Detached from ctx so a stopping run still records
// it.
func closeStoppedPass(ctx context.Context, p func(string, ...any), passes evalpkg.PassStore, passID int64,
	transcriptID, name string, findings []db.Finding, stats evalpkg.RunStats, total int, stop error) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	if _, err := passes.CommitJudgePass(cctx, db.JudgePassCommit{PassID: passID, TranscriptID: transcriptID,
		Findings: findings, Close: evalpkg.PassClose(stats, total, stop)}); err != nil {
		p("  warn %s: closing judge pass %d failed (%v)\n", name, passID, err)
	}
}

// bookContextLabel describes the prompt and book context a transcript was
// judged with: "judge@v3 asin_record 1a2b3c4d" (sha prefix) or
// "judge@v3 path_only".
func bookContextLabel(stats evalpkg.RunStats, transcriptID string) string {
	bc, ok := stats.BookContexts[transcriptID]
	if !ok {
		bc.Source = evalpkg.BookContextPathOnly
	}
	label := strings.TrimSpace(stats.PromptVersion + " " + bc.Source)
	if len(bc.SHA256) >= 8 {
		label += " " + bc.SHA256[:8]
	}
	return label
}

// run holds the testable logic: validate flags, run the judge, and report.
// In dry-run (no --write) it prints what it would record and persists nothing.
// With o.steps set the run is recorded in step_runs (CONTRACT §1.10);
// recording never fails the run.
func run(ctx context.Context, out io.Writer, r runner, book string, o options) (err error) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }

	if o.sample <= 0 && book == "" {
		return fmt.Errorf("provide a book substring, or use --sample N")
	}
	if o.sample > 0 && book != "" {
		return fmt.Errorf("--sample and a book substring cannot be combined")
	}
	if o.seed != "" && o.sample <= 0 {
		return fmt.Errorf("--seed needs --sample N")
	}
	if o.dump != "" && o.write {
		return fmt.Errorf("--dump is a dry-run measurement; it cannot be combined with --write")
	}

	rec := startSelectedRun(ctx, book, o)
	defer func() { rec.Finish(err) }()
	observe := o.observe
	if rec != nil {
		observe = func(res evalpkg.Result, jerr error) {
			if o.observe != nil {
				o.observe(res, jerr)
			}
			tickJudged(rec, res, jerr)
		}
	}
	runOpts := evalpkg.RunOptions{
		Book:    book,
		Sample:  o.sample,
		Seed:    o.seed,
		Limit:   o.limit,
		Write:   o.write,
		Observe: observe,
	}

	findings, stats, err := r.Run(ctx, runOpts)
	if err != nil {
		return err
	}

	scope := fmt.Sprintf("book %q", book)
	if o.sample > 0 {
		scope = fmt.Sprintf("a %d-chunk sample", o.sample)
		if o.seed != "" {
			scope += fmt.Sprintf(" (seed %q)", o.seed)
		}
	}
	p("Evaluated %d chunk(s) from %s — %d suspected error(s) found.\n",
		stats.ChunksEvaluated, scope, stats.FindingsFound)
	if stats.ChunksSkipped > 0 {
		p("(%d chunk(s) skipped due to transient judge errors — partial results below.)\n", stats.ChunksSkipped)
	}
	if line := droppedLine(stats.Dropped); line != "" {
		p("Dropped before recording: %s.\n", line)
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

// startSelectedRun records a book or --sample run (nil when o.steps is nil).
func startSelectedRun(ctx context.Context, book string, o options) *runs.Run {
	if o.steps == nil {
		return nil
	}
	mode := db.StepRunDryRun
	if o.write {
		mode = db.StepRunWrite
	}
	spec := runs.Spec{Step: runs.StepEvalBook, Mode: mode, Model: o.model, Args: fmt.Sprintf("book %q", book)}
	if o.sample > 0 {
		n := int64(o.sample)
		spec.Step, spec.Total = runs.StepEvalSample, &n
		spec.Args = fmt.Sprintf("--sample %d", o.sample)
		if o.seed != "" {
			spec.Args += " --seed " + o.seed
		}
	}
	if o.judgePrompt != "" {
		spec.Args += " --judge-prompt " + o.judgePrompt
	}
	if o.write {
		spec.Args += " --write"
	}
	rec := runs.Start(ctx, o.steps, spec)
	rec.Phase("asking " + o.model)
	return rec
}

// tickJudged records one judged chunk: evaluated or errors, the findings it
// produced, and the gateway-reported cost.
func tickJudged(rec *runs.Run, res evalpkg.Result, jerr error) {
	if jerr != nil {
		rec.Tick("errors", res.Usage.CostUSD)
		return
	}
	rec.Tick("evaluated", res.Usage.CostUSD)
	rec.Count("findings", int64(len(res.Findings)))
}

// droppedLine renders per-reason drop counts, largest first then by reason
// ("cosmetic_only 4, below_min_confidence 2"); "" when nothing was dropped.
func droppedLine(dropped map[string]int) string {
	reasons := make([]string, 0, len(dropped))
	for r := range dropped {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool {
		if dropped[reasons[i]] != dropped[reasons[j]] {
			return dropped[reasons[i]] > dropped[reasons[j]]
		}
		return reasons[i] < reasons[j]
	})
	parts := make([]string, len(reasons))
	for i, r := range reasons {
		parts[i] = fmt.Sprintf("%s %d", r, dropped[r])
	}
	return strings.Join(parts, ", ")
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
