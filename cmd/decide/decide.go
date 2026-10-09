// Package decide implements `earmark decide`: a dry run of the decide step
// (CONTRACT §2.19). It samples proposed judge findings, runs rung 0, text
// evidence and the should_apply model call on each, and reports what a full
// run would do. It decides nothing: no finding_events, no state changes. The
// model answers are cached in fn_calls, so the later full run over the same
// findings is served from the cache.
package decide

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	decidepkg "github.com/jedwards1230/earmark/internal/decide"
	"github.com/jedwards1230/earmark/internal/telemetry"
)

type options struct {
	sample      int
	sampleSet   bool // --sample given explicitly
	seed        string
	book        string
	issueType   string
	concurrency int
	json        bool
	calibrate   bool
	human       []string
	rung0Only   bool
	dump        string // --dump path ("" = none)
	// --yes run
	yes        bool
	limit      int
	shard      string // "i/N"
	batch      int
	maxAccepts int
}

var opts options

// DecideCmd is `earmark decide`.
var DecideCmd = &cobra.Command{
	Use:   "decide [flags]",
	Short: "Decide proposed findings: dry-run a sample, or --yes to decide and write",
	Long: `decide samples proposed, anchored judge findings and runs the decide step
on each (CONTRACT §2.19): rung 0's deterministic checks, text evidence from the
book's catalogue record, and the should_apply question to the pinned System One
model. It reports what a full run would do — outcome × issue type × evidence,
reasons, a p histogram, model errors, latency, tokens and cost — and projects
the sample's rates onto every finding in scope.

It is a dry run: nothing is decided. No finding_events are written and no
finding changes state. The model calls are real (and paid for), and are logged
and cached in fn_calls, so a later full run over the same findings is served
from the cache. Requires AI_ROLES.decide bound to a systemone endpoint asking
for the pinned model (CONTRACT §2.14).

--calibrate samples findings a person already decided through the review
surface (accepted/applied vs rejected) instead, and reports how often the
decide step agrees: apply precision and recall, reject precision, hold rate,
and how agreement moves with rung 0's phonetic_min_sim (re-run offline).

The sample is deterministic: the same --seed over the same backlog picks the
same findings.

--rung0-only replays the sample through rung 0 alone: the same sampling,
loading, rung-0 checks and dedupe, but no model call, no recipe registered and
nothing written — so it needs no AI_ROLES.decide. It reports rung-0 passes
and reject reasons; run two builds over the same --seed to compare rung-0
versions. Like every command that opens the database it still runs the
goose schema migrations first (a no-op when the schema is current). --dump
FILE writes one JSON line per sampled finding (id, issue type, judge model,
text, rung-0 verdict and evidence, and the decision when the model was
asked), sorted by finding id; it works with or without --rung0-only. Both
are dry-run only.

--yes decides every finding in scope and writes the decisions (CONTRACT
§2.19 "earmark decide --yes"): apply moves a finding to accepted (replayed by
the next rebuild), reject to rejected, and a hold records a decision and
leaves it proposed — every change a finding_events row with actor
jev:<recipe_id>. It also re-checks findings an OLDER decide recipe accepted:
a reject undoes the fix (accepted → rejected, applied → reverted), a hold
keeps it and lists it for review. Work is computed with no transaction held
and written in short transactions of --batch transcripts. A re-run skips
findings this recipe already decided, except holds where the model was
unavailable. Each accept re-embeds its whole transcript on the next rebuild:
bound a run with --limit and --max-accepts. Undo with 'earmark decide revert'.

Examples:
  earmark decide --sample 200                    # preview 200 random findings
  earmark decide --sample 200 --seed q4 --json   # the same 200, as JSON
  earmark decide --sample 500 --issue-type misheard_proper_noun
  earmark decide --sample 300 --calibrate        # agreement with human decisions
  earmark decide --sample 300 --calibrate --human mcp,cli
  earmark decide --sample 2000 --seed q4 --rung0-only --dump /tmp/r0.jsonl
  earmark decide --yes --limit 2000 --max-accepts 500
  earmark decide --yes --shard 0/4               # one of four parallel runners
  earmark decide revert --recipe <id>            # preview the undo
  earmark decide revert --recipe <id> --yes`,
	Run: runDecide,
}

func init() {
	f := DecideCmd.Flags()
	f.IntVar(&opts.sample, "sample", 100, fmt.Sprintf("findings to decide, chosen by --seed (1..%d)", db.MaxDecideSample))
	f.StringVar(&opts.seed, "seed", "earmark", "sample seed: the same seed picks the same findings")
	f.StringVar(&opts.book, "book", "", "only findings whose file path contains this (case-insensitive)")
	f.StringVar(&opts.issueType, "issue-type", "", "only findings of this issue type")
	f.IntVar(&opts.concurrency, "concurrency", 8, "concurrent System One calls")
	f.BoolVar(&opts.json, "json", false, "print the report as JSON")
	f.BoolVar(&opts.calibrate, "calibrate", false, "sample human-decided findings and report agreement")
	f.StringSliceVar(&opts.human, "human", nil, "with --calibrate: decided_by prefixes that count as a person (default mcp; e.g. mcp,cli)")
	f.BoolVar(&opts.rung0Only, "rung0-only", false, "dry run through rung 0 only: no model call, no decision or fn_calls write, no AI_ROLES.decide needed (still runs goose migrations at connect, a no-op on a current schema)")
	f.StringVar(&opts.dump, "dump", "", "dry run: write one JSON line per sampled finding to this file")
	f.BoolVar(&opts.yes, "yes", false, "decide every finding in scope and write the decisions")
	f.IntVar(&opts.limit, "limit", 0, "with --yes: decide at most N findings this run (0 = all in scope)")
	f.StringVar(&opts.shard, "shard", "", "with --yes: i/N — only transcripts with hashtext(transcript_id) mod N = i")
	f.IntVar(&opts.batch, "batch", decidepkg.DefaultWriteBatch, "with --yes: transcripts per write transaction")
	f.IntVar(&opts.maxAccepts, "max-accepts", 0, "with --yes: stop after N accepts (bounds the re-embed; 0 = no cap)")
	DecideCmd.AddCommand(RevertCmd)
}

// parseShard parses "i/N" (0 ≤ i < N); "" = unsharded.
func parseShard(s string) (shard, shards int, err error) {
	if s == "" {
		return 0, 0, nil
	}
	if _, err := fmt.Sscanf(s, "%d/%d", &shard, &shards); err != nil || fmt.Sprintf("%d/%d", shard, shards) != s ||
		shards < 1 || shard < 0 || shard >= shards {
		return 0, 0, fmt.Errorf("--shard %q must be i/N with 0 <= i < N", s)
	}
	return shard, shards, nil
}

func validate(o options) error {
	switch {
	case o.sample < 1 || o.sample > db.MaxDecideSample:
		return fmt.Errorf("--sample must be 1..%d", db.MaxDecideSample)
	case o.seed == "":
		return errors.New("--seed must not be empty")
	case o.concurrency < 1 || o.concurrency > 64:
		return errors.New("--concurrency must be 1..64")
	case o.issueType != "" && !knownIssueType(o.issueType):
		return fmt.Errorf("--issue-type %q is not an issue type", o.issueType)
	case o.yes && (o.sampleSet || o.calibrate):
		return errors.New("--yes decides the whole scope: use --limit instead of --sample, and --calibrate is dry-run only")
	case !o.yes && (o.limit != 0 || o.shard != "" || o.maxAccepts != 0):
		return errors.New("--limit, --shard and --max-accepts need --yes")
	case o.limit < 0 || o.maxAccepts < 0:
		return errors.New("--limit and --max-accepts must be >= 0")
	case o.batch < 1 || o.batch > 500:
		return errors.New("--batch must be 1..500")
	case len(o.human) > 0 && !o.calibrate:
		return errors.New("--human needs --calibrate")
	case o.yes && (o.rung0Only || o.dump != ""):
		return errors.New("--rung0-only and --dump are dry-run only")
	}
	_, _, err := parseShard(o.shard)
	return err
}

func knownIssueType(t string) bool {
	switch t {
	case decidepkg.IssueMisheardProperNoun, decidepkg.IssueMisheardWord, decidepkg.IssueHomophone,
		decidepkg.IssueNumberArtifact, decidepkg.IssueRepeatedText, decidepkg.IssueDroppedWord, decidepkg.IssueOther:
		return true
	}
	return false
}

func runDecide(cmd *cobra.Command, _ []string) {
	opts.sampleSet = cmd.Flags().Changed("sample")
	if err := validate(opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	// A rung-0 replay asks no model, so it needs no decide endpoint.
	var asker decidepkg.Asker
	if !opts.rung0Only {
		client, ok, err := decidepkg.ClientFromConfig(cfg)
		if !ok {
			fmt.Fprintln(os.Stderr, "Error: earmark decide needs AI_ROLES.decide bound to a systemone endpoint in AI_ENDPOINTS (CONTRACT §2.14)")
			os.Exit(2)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: decide endpoint: %v\n", err)
			os.Exit(2)
		}
		asker = client
	}
	database, err := db.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	tel, terr := telemetry.Setup(context.Background())
	if terr != nil {
		log.Printf("WARNING: OpenTelemetry disabled: %v", terr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, os.Stdout, database, asker, opts)
	stop()
	flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if serr := tel.Shutdown(flushCtx); serr != nil {
		log.Printf("OpenTelemetry shutdown: %v", serr)
	}
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable core.
func run(ctx context.Context, out io.Writer, store decidepkg.WriteStore, asker decidepkg.Asker, o options) error {
	if err := validate(o); err != nil {
		return err
	}
	var rep *decidepkg.Report
	var err error
	if o.yes {
		shard, shards, _ := parseShard(o.shard)
		rep, err = decidepkg.Apply(ctx, store, asker, decidepkg.ApplyOptions{
			Book: o.book, IssueType: o.issueType, Shard: shard, Shards: shards,
			Limit: o.limit, Batch: o.batch, MaxAccepts: o.maxAccepts,
			Concurrency: o.concurrency, Params: decidepkg.DefaultShouldApplyParams(),
		})
	} else {
		ro := decidepkg.RunOptions{
			Scope: db.DecideScope{
				Sample: o.sample, Seed: o.seed, Book: o.book, IssueType: o.issueType,
				Calibrate: o.calibrate, HumanActors: o.human,
			},
			Concurrency: o.concurrency,
			Params:      decidepkg.DefaultShouldApplyParams(),
			Rung0Only:   o.rung0Only,
		}
		rep, err = dryRun(ctx, store, asker, ro, o.dump)
	}
	if err != nil {
		return err
	}
	return rep.Print(out, o.json)
}

// dryRun runs decidepkg.DryRun, writing the --dump file when path is set.
// The file is created before the run so a bad path fails fast, and it is
// removed again if the run fails, so a partial dump is never mistaken for a
// measurement.
func dryRun(ctx context.Context, store decidepkg.RunStore, asker decidepkg.Asker, ro decidepkg.RunOptions, path string) (*decidepkg.Report, error) {
	if path == "" {
		return decidepkg.DryRun(ctx, store, asker, ro)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("--dump: %w", err)
	}
	ro.Dump = f
	rep, err := decidepkg.DryRun(ctx, store, asker, ro)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("--dump: %w", cerr)
	}
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return rep, nil
}
