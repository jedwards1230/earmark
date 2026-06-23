// Package batch implements the `earmark batch` command: a coordinator that runs
// the pipeline in transcribe-then-analyze batches so the ASR model and the
// eval-judge LLM time-share a single GPU instead of contending for it
// (CONTRACT §1.4, the batched two-phase pipeline).
//
// It mirrors `cmd/eval`'s shape: a thin cobra command that wires real
// dependencies, plus a small, dependency-injected `run` core behind interfaces
// so the phase-transition logic is unit-testable without a DB, an ASR runner,
// or gpu-arbiter.
//
// The coordinator only flips runner_control.phase + run_limit and reads status;
// it knows nothing about CUDA and never POSTs to gpu-arbiter (the arbiter read
// is strictly GET /status, read-only). All durable state lives in the DB, so a
// restart mid-batch reconciles from runner_control + the job counts.
package batch

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jedwards1230/earmark/internal/batch"
	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/spf13/cobra"
)

type options struct {
	batchSize      int           // jobs transcribed per batch (run_limit per Phase A)
	maxBatches     int           // stop after N batches (0 = until the queue drains)
	pollInterval   time.Duration // how often to poll DB state for phase completion
	arbiterPoll    time.Duration // how often to re-check gpu-arbiter while gaming
	gpuArbiterURL  string        // gpu-arbiter /status URL (flag overrides GPU_ARBITER_URL)
	arbiterTimeout time.Duration // per-request timeout for the arbiter probe
}

var opts options

// BatchCmd is the `earmark batch` cobra command.
var BatchCmd = &cobra.Command{
	Use:   "batch [flags]",
	Short: "Coordinate the pipeline in transcribe→analyze batches (GPU time-sharing)",
	Long: `Run the pipeline in batches so the ASR model and the eval-judge LLM
time-share one GPU instead of contending for it (CONTRACT §1.4).

Each batch:
  1. Yield to games — if gpu-arbiter reports state="gaming", wait until it
     doesn't before doing GPU work (read-only GET /status; never POSTs).
  2. Phase A (transcribe) — set phase=transcribe + run_limit=N; the ASR runner
     transcribes up to N jobs while the embed worker idles. Wait until drained.
  3. Phase B (analyze) — set phase=analyze; the ASR runner parks its model and
     the embed worker drains the just-transcribed transcripts (chunk→embed,
     eval inline when EVAL_IN_PIPELINE=true). Wait until the embed backlog is 0.
  4. Repeat until no pending jobs remain, --max-batches is hit, or interrupted.

On exit (normal, error, or SIGINT/SIGTERM) the phase is ALWAYS restored to idle
and the run budget cleared, so the pipeline returns to normal continuous mode.

The coordinator is DB-driven and resumable: it keeps no critical state in
memory. If it restarts mid-batch and finds phase=analyze, it finishes Phase B
before starting a new Phase A.

gpu-arbiter URL comes from --gpu-arbiter-url or $GPU_ARBITER_URL. If unset or
unreachable, the coordinator logs it and proceeds (degrades gracefully — it
never wedges on arbiter absence).

Examples:
  earmark batch                          # batches of 10 until the queue drains
  earmark batch --batch-size 25          # larger batches
  earmark batch --max-batches 3          # stop after 3 batches
  earmark batch --gpu-arbiter-url http://gpu-host:48750/status`,
	Run: runBatch,
}

func init() {
	BatchCmd.Flags().IntVar(&opts.batchSize, "batch-size", 10, "jobs to transcribe per batch (Phase A run_limit)")
	BatchCmd.Flags().IntVar(&opts.maxBatches, "max-batches", 0, "stop after N batches (0 = until queue drains)")
	BatchCmd.Flags().DurationVar(&opts.pollInterval, "poll-interval", 10*time.Second, "how often to poll DB state for phase completion")
	BatchCmd.Flags().DurationVar(&opts.arbiterPoll, "arbiter-poll", 15*time.Second, "how often to re-check gpu-arbiter while gaming")
	BatchCmd.Flags().StringVar(&opts.gpuArbiterURL, "gpu-arbiter-url", "", "gpu-arbiter /status URL (overrides $GPU_ARBITER_URL)")
	BatchCmd.Flags().DurationVar(&opts.arbiterTimeout, "arbiter-timeout", 3*time.Second, "per-request timeout for the gpu-arbiter probe")
}

func runBatch(_ *cobra.Command, _ []string) {
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

	// gpu-arbiter URL: flag wins over env. Empty → no arbiter (degrade).
	arbiterURL := opts.gpuArbiterURL
	if arbiterURL == "" {
		arbiterURL = os.Getenv("GPU_ARBITER_URL")
	}

	cfgOpts := batch.Options{
		BatchSize:    opts.batchSize,
		MaxBatches:   opts.maxBatches,
		PollInterval: opts.pollInterval,
		ArbiterPoll:  opts.arbiterPoll,
		// Record runner_availability transitions to pipeline_events (CONTRACT §1.7)
		// so the availability windows feed the calendar ETA model. Best-effort.
		Sink: database,
		// EvalGatesEmbed: when true the batch coordinator waits for BOTH the eval
		// backlog and the embed backlog to reach 0 before advancing to the next
		// batch (Phase B completion, CONTRACT §1.4, §2.4).
		EvalGatesEmbed: cfg.EvalGatesEmbed,
	}
	arbiter := batch.NewHTTPArbiter(arbiterURL, opts.arbiterTimeout)

	// SIGINT/SIGTERM handling lives inside batch.Run so the signal-aware scope
	// strictly encloses its restore-idle defer (no signal-at-return-time race).
	if err := batch.Run(context.Background(), os.Stdout, database, arbiter, cfgOpts); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}
