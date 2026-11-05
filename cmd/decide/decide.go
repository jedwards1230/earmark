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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	decidepkg "github.com/jedwards1230/earmark/internal/decide"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/runs"
	"github.com/jedwards1230/earmark/internal/systemone"
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
	// --endpoint / --model: the decide step's model for this run only
	// (CONTRACT §2.20).
	overrideEndpoint string
	overrideModel    string
	// --policy key=value: the decide policy for this run only (CONTRACT
	// §2.19 "Decide policy").
	policyFlags []string
	// --audio-vote: the audio-vote rung (CONTRACT §2.19 "Audio-vote rung"),
	// configured by MODELS_FILE audio_vote or, all or nothing, by
	// --audio-rule with --audio-model <model_id>@<revision> (repeatable).
	audioVote   bool
	audioModels []string
	audioRule   string
	// model is the resolved decision model (not a flag; zero = default).
	model decidepkg.Model
	// policy is the resolved decide policy (not a flag; zero = default).
	policy decidepkg.Policy
}

var opts options

// defaultConcurrency is --concurrency's default: System One calls in flight.
// Measured against Clef (clef:27b on one Ollama host, MAX_LOADED_MODELS=1,
// through LiteLLM), which serializes the calls: 1 in flight gave 3.3 calls/s,
// 2 gave 4.5, 4 gave 4.64, and 8, 16 and 32 gave 4.63-4.66 — the same
// throughput with latency growing in step (p50 0.9 s at 4, 1.7 s at 8, 6.8 s at
// 32). Past 4 calls only queue.
const defaultConcurrency = 4

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
from the cache. Requires AI_ROLES.decide bound to a systemone endpoint
(CONTRACT §2.14), or a model_settings row for the decide step.

Model: should_apply asks the decide step's effective model (CONTRACT §2.20):
--endpoint/--model for this run, else the model_settings row ('earmark
models set decide ...'), else AI_ROLES.decide. Any pinned System One model
works; a different model is a different decide recipe, so its decisions are
never confused with another model's. Neither flag writes model_settings.

Calls: --concurrency (default 4) calls are in flight at once. A model served
locally answers them one at a time (Clef: about 4.6 calls/s however many are
in flight), so a higher value only lengthens the queue. A local model is also
unloaded after its keep-alive, and loading it takes longer than the 15 s call
timeout: until the first call has returned, calls get 2 minutes instead, so a
cold start is waited out rather than recorded as jev_unavailable. No extra
call is made for it and nothing about the decisions changes.

Policy: each decide model has its own apply/reject rule (CONTRACT §2.19
"Decide policy") — apply_p, reject_p, apply_evidence,
reject_requires_lowercase, reject_floor — resolved per key: --policy
key=value for this run, else the decide step's model_settings params
('earmark models set decide --param apply_p=0.9'), else MODELS_FILE
steps.decide.params, else the built-in preset for the model (jev-*, clef*),
else the default (apply p >= 0.85 with text evidence, reject p <= 0.20). The
policy is part of the decide recipe, so a dry run reports outcomes under
exactly the policy a --yes run with the same flags would use, and a new
policy re-decides from cached model answers. The report's "policy:" line
names it and its source.

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
unavailable.

Agreeing and conflicting findings (CONTRACT §2.19): findings of one chunk that
propose the same fix (same span and correction_key — typically two propose
recipes) form a group; one representative is decided (accepted/applied first,
then an existing representative, then one rung 0 can vet, then the current
propose recipe's, then the newest) and the others are recorded as duplicates
of it — never rejected, never applied. Different fixes to overlapping text are
all asked and ranked by the model's p; a loser is rejected overlap_dup when
the winner applies and held overlap_pending while the winner is held. A dry run lists each group and each ranked conflict. Each accept re-embeds its whole transcript on the next rebuild:
bound a run with --limit and --max-accepts. Undo with 'earmark decide revert'.

--audio-vote runs the audio-vote rung instead (CONTRACT §2.19 "Audio-vote
rung"): it asks no model. It takes every HELD finding (proposed, latest
decision a hold, by any decide recipe) with a current vote from the
configured second-ASR models (MODELS_FILE audio_vote, or --audio-rule with
two or more --audio-model <model_id>@<revision>, which then replace the file
block), re-runs rung 0 exactly as decide does — a rung-0 failure rejects —
and then: every model heard the original → reject asr_heard_original; every
model heard the replacement → apply asr_agree, unless the finding's type is
dropped_word/number_artifact, its hold carries no model p or a reason other
than uncertain/no_evidence/evidence_excluded, or it overlaps a different fix
not recorded as having lost to it; anything else holds (asr_split,
asr_vote_missing, asr_no_agreement, asr_agree_vetoed, asr_agree_conflict). A
dry run writes nothing at all — not even the recipe row — and previews what
--yes would write under the same --limit / --shard / --max-accepts; --sample
and --seed pick the listed findings. --yes writes only the applies and
rejects, as jev:<audio recipe id>; holds are never written, so the
should_apply recipe's work list is untouched. Undo with 'earmark decide
revert --recipe <audio recipe id>'. Not combinable with --calibrate,
--rung0-only, --dump, --human, --endpoint, --model or --policy.

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
  earmark decide revert --recipe <id> --yes
  earmark decide --sample 200 --seed q4 --endpoint litellm-s1 --model clef:27b
  earmark decide --sample 200 --seed q4 --policy apply_p=0.9 --dump /tmp/d.jsonl
  earmark decide --audio-vote --book "Children of Dune"   # preview the audio-vote rung
  earmark decide --audio-vote --yes --limit 2000 --max-accepts 500`,
	Run: runDecide,
}

func init() {
	f := DecideCmd.Flags()
	f.IntVar(&opts.sample, "sample", 100, fmt.Sprintf("findings to decide, chosen by --seed (1..%d)", db.MaxDecideSample))
	f.StringVar(&opts.seed, "seed", "earmark", "sample seed: the same seed picks the same findings")
	f.StringVar(&opts.book, "book", "", "only findings whose file path contains this (case-insensitive)")
	f.StringVar(&opts.issueType, "issue-type", "", "only findings of this issue type")
	f.IntVar(&opts.concurrency, "concurrency", defaultConcurrency,
		fmt.Sprintf("concurrent System One calls (a local model serves them one at a time: past %d they only queue)", defaultConcurrency))
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
	f.StringVar(&opts.overrideEndpoint, "endpoint", "", "this run only: the AI_ENDPOINTS id (a systemone endpoint) to ask (default: the decide step's effective endpoint)")
	f.StringVar(&opts.overrideModel, "model", "", "this run only: the pinned System One model id to ask (default: the decide step's effective model)")
	f.StringArrayVar(&opts.policyFlags, "policy", nil, "this run only: a decide policy key=value (apply_p, reject_p, apply_evidence, reject_requires_lowercase, reject_floor) over the model's policy; repeatable")
	f.BoolVar(&opts.audioVote, "audio-vote", false, "decide held findings from second-ASR votes (no model call; CONTRACT §2.19 \"Audio-vote rung\")")
	f.StringArrayVar(&opts.audioModels, "audio-model", nil, "with --audio-vote: a pilot model <model_id>@<revision> that must agree; repeatable, at least 2; with --audio-rule, replaces MODELS_FILE audio_vote")
	f.StringVar(&opts.audioRule, "audio-rule", "", "with --audio-vote: the vote rule counted (finding_asr_votes.detail.rule); with --audio-model, replaces MODELS_FILE audio_vote")
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
	case !o.audioVote && (len(o.audioModels) > 0 || o.audioRule != ""):
		return errors.New("--audio-model and --audio-rule need --audio-vote")
	case o.audioVote && (o.calibrate || o.rung0Only || o.dump != "" || len(o.human) > 0 ||
		o.overrideEndpoint != "" || o.overrideModel != "" || len(o.policyFlags) > 0):
		return errors.New("--audio-vote asks no model: it cannot be combined with --calibrate, --rung0-only, --dump, --human, --endpoint, --model or --policy")
	case o.audioVote && (len(o.audioModels) > 0) != (o.audioRule != ""):
		return errors.New("--audio-model and --audio-rule replace MODELS_FILE audio_vote together: give both, or neither")
	case !o.yes && !o.audioVote && (o.limit != 0 || o.shard != "" || o.maxAccepts != 0):
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
	var audio config.AudioVoteConfig
	if opts.audioVote {
		// Resolved before connecting: a missing configuration fails before
		// the database (and its migrations) is touched.
		if audio, err = audioConfig(cfg, opts); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(2)
		}
	}
	database, err := db.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()
	if opts.audioVote {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = runAudio(ctx, os.Stdout, database, audio, opts)
		stop()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	cfg, opts, err = resolve(context.Background(), database, cfg, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	log.Print(cfg.DescribeStep(recipe.StepDecide))
	log.Printf("decide policy: %s (source %s)", opts.policy.Params.Describe(), opts.policy.SourceLabel())
	if opts.policy.Preset == "" && len(opts.policy.Layers) == 0 {
		log.Printf("WARNING: decide model %s has no built-in policy preset and no policy keys set; it decides under "+
			"the default policy, which was measured on Jev — set one with 'earmark models set decide --param …' "+
			"or --policy before a --yes run", modelOrDefault(opts.model))
	}
	// A rung-0 replay asks no model, so it needs no decide endpoint.
	var asker decidepkg.Asker
	if !opts.rung0Only {
		a, ok, err := decidepkg.AskerFromConfig(cfg)
		if !ok {
			fmt.Fprintln(os.Stderr, "Error: earmark decide needs AI_ROLES.decide bound to a systemone endpoint in AI_ENDPOINTS (CONTRACT §2.14), or a decide model setting (earmark models set decide)")
			os.Exit(2)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: decide endpoint: %v\n", err)
			os.Exit(2)
		}
		asker = a
		log.Printf("decide: %d System One calls in flight; a call times out after %s, or %s until the first call has returned (a cold model load)",
			opts.concurrency, systemone.DefaultTimeout, decidepkg.ColdStartTimeout)
	}

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

// resolve is the decide step's effective model and policy for this run.
// Model (CONTRACT §2.20): --endpoint/--model, else its model_settings row,
// else AI_ROLES.decide; an invalid row fails the run (even a rung-0 replay,
// whose recipe id names the model). Policy (CONTRACT §2.19 "Decide policy"),
// for that model: --policy, else the step's pin params, else the model's
// preset, else the default; a bad value fails the run before anything is
// decided. Dry runs and --yes resolve alike, so a dry run reports outcomes
// under exactly the policy the write would use.
func resolve(ctx context.Context, settings db.SettingsReader, base *config.Config, o options) (*config.Config, options, error) {
	cfg, err := db.ResolveStep(ctx, settings, base, recipe.StepDecide, o.overrideEndpoint, o.overrideModel)
	if err != nil {
		return nil, o, err
	}
	o.model, _ = decidepkg.ModelFromConfig(cfg)
	override, err := decidepkg.ParsePolicyFlags(o.policyFlags)
	if err != nil {
		return nil, o, err
	}
	if o.policy, err = decidepkg.ResolvePolicy(cfg, override); err != nil {
		return nil, o, err
	}
	return cfg, o, nil
}

// run is the testable core. The run is recorded in step_runs (CONTRACT
// §1.10) when store is the real database; recording never fails the run.
func run(ctx context.Context, out io.Writer, store decidepkg.WriteStore, asker decidepkg.Asker, o options) (err error) {
	if err := validate(o); err != nil {
		return err
	}
	if o.policy.Source == "" {
		// Only resolve builds a policy: a run must never fall back to some
		// policy other than its model's (CONTRACT §2.19 "Decide policy").
		return errors.New("decide: the decide policy is not resolved")
	}
	mode := db.StepRunDryRun
	if o.yes {
		mode = db.StepRunWrite
	}
	rec := runs.StartIfStore(ctx, store, runs.Spec{
		Step: runs.StepDecide, Mode: mode, Model: modelOrDefault(o.model), Args: argsSummary(o),
	})
	defer func() { rec.Finish(err) }()
	var rep *decidepkg.Report
	if o.yes {
		shard, shards, _ := parseShard(o.shard)
		rep, err = decidepkg.Apply(ctx, store, asker, decidepkg.ApplyOptions{
			Book: o.book, IssueType: o.issueType, Shard: shard, Shards: shards,
			Limit: o.limit, Batch: o.batch, MaxAccepts: o.maxAccepts,
			Concurrency: o.concurrency, Params: o.policy.Params, PolicySource: o.policy.SourceLabel(),
			Progress: rec, Model: o.model,
		})
	} else {
		ro := decidepkg.RunOptions{
			Scope: db.DecideScope{
				Sample: o.sample, Seed: o.seed, Book: o.book, IssueType: o.issueType,
				Calibrate: o.calibrate, HumanActors: o.human,
			},
			Concurrency:  o.concurrency,
			Params:       o.policy.Params,
			PolicySource: o.policy.SourceLabel(),
			Rung0Only:    o.rung0Only,
			Progress:     rec,
			Model:        o.model,
		}
		rep, err = dryRun(ctx, store, asker, ro, o.dump)
	}
	if err != nil {
		return err
	}
	return rep.Print(out, o.json)
}

// audioConfig is the audio-vote rung's configuration: --audio-rule with
// --audio-model when given (all or nothing: they replace the file block),
// else MODELS_FILE audio_vote. Validated; absent is an error.
func audioConfig(cfg *config.Config, o options) (config.AudioVoteConfig, error) {
	var c config.AudioVoteConfig
	switch {
	case len(o.audioModels) > 0 || o.audioRule != "":
		c.Rule = o.audioRule
		for _, s := range o.audioModels {
			m, err := config.ParseAudioModelFlag(s)
			if err != nil {
				return c, err
			}
			c.Models = append(c.Models, m)
		}
	case cfg.AudioVote() != nil:
		c = *cfg.AudioVote()
	default:
		return c, errors.New("decide --audio-vote is not configured: set audio_vote (rule + models) in MODELS_FILE, " +
			"or pass --audio-rule with --audio-model <model_id>@<revision> for each model (CONTRACT §2.19 \"Audio-vote rung\")")
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("decide --audio-vote: %w", err)
	}
	return c, nil
}

// runAudio is the --audio-vote core: the rung over the scope, recorded in
// step_runs (CONTRACT §1.10) when store is the real database.
func runAudio(ctx context.Context, out io.Writer, store decidepkg.AudioVoteStore, cfg config.AudioVoteConfig, o options) (err error) {
	if err := validate(o); err != nil {
		return err
	}
	if !o.audioVote {
		return errors.New("decide: runAudio needs --audio-vote")
	}
	mode := db.StepRunDryRun
	if o.yes {
		mode = db.StepRunWrite
	}
	rec := runs.StartIfStore(ctx, store, runs.Spec{
		Step: runs.StepDecide, Mode: mode, Model: decidepkg.AudioVoteModelAlias, Args: argsSummary(o),
	})
	defer func() { rec.Finish(err) }()
	shard, shards, _ := parseShard(o.shard)
	ao := decidepkg.AudioVoteOptions{
		Config: cfg, Book: o.book, IssueType: o.issueType, Shard: shard, Shards: shards,
		Limit: o.limit, Batch: o.batch, MaxAccepts: o.maxAccepts, Yes: o.yes, Seed: o.seed, Progress: rec,
	}
	if o.sampleSet {
		ao.Sample = o.sample
	}
	rep, err := decidepkg.AudioVote(ctx, store, ao)
	if err != nil {
		return err
	}
	return rep.Print(out, o.json)
}

// modelOrDefault names the model a run asks.
func modelOrDefault(m decidepkg.Model) string {
	if m.Alias == "" {
		return decidepkg.ShouldApplyModel
	}
	return m.Alias
}

// argsSummary is the step_runs.args line: the flags that shape the run.
func argsSummary(o options) string {
	var b strings.Builder
	add := func(format string, a ...any) {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, format, a...)
	}
	if o.audioVote {
		add("--audio-vote")
	}
	if o.yes || o.audioVote {
		if o.yes {
			add("--yes")
		}
		if o.limit > 0 {
			add("--limit %d", o.limit)
		}
		if o.maxAccepts > 0 {
			add("--max-accepts %d", o.maxAccepts)
		}
		if o.shard != "" {
			add("--shard %s", o.shard)
		}
	} else {
		add("--sample %d --seed %s", o.sample, o.seed)
		if o.calibrate {
			add("--calibrate")
		}
		if o.rung0Only {
			add("--rung0-only")
		}
	}
	if o.book != "" {
		add("--book %q", o.book)
	}
	if o.issueType != "" {
		add("--issue-type %s", o.issueType)
	}
	if o.overrideEndpoint != "" {
		add("--endpoint %s", o.overrideEndpoint)
	}
	if o.overrideModel != "" {
		add("--model %s", o.overrideModel)
	}
	for _, kv := range o.policyFlags {
		add("--policy %s", kv)
	}
	return b.String()
}

// dryRun runs decidepkg.DryRun, writing the --dump file when path is set.
// The file is created before the run so a bad path fails fast, and it is
// removed again if the run fails, so a partial dump is never mistaken for a
// measurement.
func dryRun(ctx context.Context, store decidepkg.RunStore, asker decidepkg.Asker, ro decidepkg.RunOptions, path string) (*decidepkg.Report, error) {
	if path == "" {
		return decidepkg.DryRun(ctx, store, asker, ro)
	}
	f, err := os.Create(filepath.Clean(path)) // #nosec G304 -- operator-supplied --dump path
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
