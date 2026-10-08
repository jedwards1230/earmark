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
	seed        string
	book        string
	issueType   string
	concurrency int
	json        bool
	calibrate   bool
}

var opts options

// DecideCmd is `earmark decide`.
var DecideCmd = &cobra.Command{
	Use:   "decide [flags]",
	Short: "Dry-run the decide step on a sample of proposed findings (decides nothing)",
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

Examples:
  earmark decide --sample 200                    # preview 200 random findings
  earmark decide --sample 200 --seed q4 --json   # the same 200, as JSON
  earmark decide --sample 500 --issue-type misheard_proper_noun
  earmark decide --sample 300 --calibrate        # agreement with human decisions`,
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
	}
	return nil
}

func knownIssueType(t string) bool {
	switch t {
	case decidepkg.IssueMisheardProperNoun, decidepkg.IssueMisheardWord, decidepkg.IssueHomophone,
		decidepkg.IssueNumberArtifact, decidepkg.IssueRepeatedText, decidepkg.IssueDroppedWord, decidepkg.IssueOther:
		return true
	}
	return false
}

func runDecide(_ *cobra.Command, _ []string) {
	if err := validate(opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	client, ok, err := decidepkg.ClientFromConfig(cfg)
	if !ok {
		fmt.Fprintln(os.Stderr, "Error: earmark decide needs AI_ROLES.decide bound to a systemone endpoint in AI_ENDPOINTS (CONTRACT §2.14)")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: decide endpoint: %v\n", err)
		os.Exit(2)
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
	err = run(ctx, os.Stdout, database, client, opts)
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
func run(ctx context.Context, out io.Writer, store decidepkg.RunStore, asker decidepkg.Asker, o options) error {
	if err := validate(o); err != nil {
		return err
	}
	rep, err := decidepkg.DryRun(ctx, store, asker, decidepkg.RunOptions{
		Scope: db.DecideScope{
			Sample: o.sample, Seed: o.seed, Book: o.book, IssueType: o.issueType, Calibrate: o.calibrate,
		},
		Concurrency: o.concurrency,
		Params:      decidepkg.DefaultShouldApplyParams(),
	})
	if err != nil {
		return err
	}
	return rep.Print(out, o.json)
}
