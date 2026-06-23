package mcp

import (
	"github.com/jedwards1230/earmark/internal/db"
)

// pipelineActivity is the operator-facing activity label for the pipeline as
// a whole. It summarises which stage currently owns the GPU/work.
type pipelineActivity string

const (
	activityTranscribing pipelineActivity = "transcribing"
	activityEvaluating   pipelineActivity = "evaluating"
	activityEmbedding    pipelineActivity = "embedding"
	activityWindingDown  pipelineActivity = "winding-down"
	activityIdle         pipelineActivity = "idle"
	activityPaused       pipelineActivity = "paused"
)

// pipelineLifecycle is the derived view of the 3-stage pipeline that the
// dashboard and /api/v1/status both expose.  It is computed at read time from
// existing signals (QueueStats + arbiterStatus + phase) — no schema change.
//
// Stages in order: Transcribe → Eval → Embed.
// The "winding-down" state captures the gap the original dashboard missed:
// the transcribe queue is drained but the GPU is still busy (eval / embed catch-up).
type pipelineLifecycle struct {
	// Activity is the operator-facing label.
	Activity pipelineActivity `json:"activity"`
	// Phase is the raw coordinator phase ("idle"|"transcribe"|"analyze").
	Phase string `json:"phase"`

	// Transcribe stage.
	TranscribeDone  int `json:"transcribeDone"`
	TranscribeTotal int `json:"transcribeTotal"`

	// Eval stage.
	// EvalCoverage is the fraction of done jobs that have been judged (0..1).
	// When EvalInPipeline is false it is set to -1 (sentinel: "not applicable").
	// When Done==0 and EvalInPipeline is true it is 0 (no jobs yet).
	EvalCoverage   float64 `json:"evalCoverage"`
	EvalInPipeline bool    `json:"evalInPipeline"`

	// Embed stage.
	EmbedBacklog int `json:"embedBacklog"`

	// GPU commitment derived from the phase + arbiter probe.
	// GPUCommitted is true when the pipeline currently owns the GPU (transcribing
	// or evaluating).
	GPUCommitted bool `json:"gpuCommitted"`
	// GPUProbed is true when a gpu-arbiter probe is configured and reachable, so
	// the consumer can distinguish "GPU free" from "GPU state unknown".
	GPUProbed bool `json:"gpuProbed"`

	// FullyDone is true when every stage has completed:
	//   pending == 0 AND claimed == 0 AND embedBacklog == 0
	//   AND (evalCoverage == 1 OR !evalInPipeline)
	FullyDone bool `json:"fullyDone"`

	// Per-track stage bucket counts for the segmented pipeline bar (CONTRACT
	// §2.12). These are populated from QueueStats.PipelineBuckets at lifecycle
	// computation time and exposed in /api/v1/status so agents get the same
	// breakdown the dashboard bar renders.
	//
	// notStarted   = pending + transcribing (split below for finer granularity).
	// transcribing = in-flight (claimed).
	// transcribedOnly = done, no eval completion, no embedded chunks.
	// evaldOnly       = done, eval finished, no embedded chunks.
	// embeddedReady   = done, has embedded chunks (the terminal/goal state).
	// failed           = status='failed' (off the bar fill; side-exit).
	NotStarted      int `json:"notStarted"`
	Transcribing    int `json:"transcribing"`
	TranscribedOnly int `json:"transcribedOnly"`
	EvaldOnly       int `json:"evaldOnly"`
	EmbeddedReady   int `json:"embeddedReady"`
	BucketFailed    int `json:"failed"`
}

// evalInPipelineSignal is the ambient flag for whether EVAL_IN_PIPELINE is
// enabled.  In production it is fed from config; in the demo it is hardcoded
// so that the fixture shows a realistic winding-down state.  The lifecycle
// computation needs it to distinguish "eval done" from "eval not configured".
//
// computePipelineLifecycle accepts it as a parameter rather than reading an env
// var directly so the function is pure and unit-testable.

// computePipelineLifecycle derives the pipeline lifecycle from already-fetched
// signals — no additional DB queries.
//
// Parameters:
//   - stats: GetServiceStatus result (queue counts, eval coverage).
//   - phase: coordinator phase from GetPipelinePhase.
//   - arbiter: gpu-arbiter probe result for the primary ASR server (may be
//     zero-value when no probe is configured).
//   - gpuProbed: true when the arbiter probe was actually attempted.
//   - evalEnabled: true when EVAL_IN_PIPELINE is active (config-driven).
func computePipelineLifecycle(
	stats *db.QueueStats,
	phase string,
	arbiter arbiterStatus,
	gpuProbed bool,
	evalEnabled bool,
) pipelineLifecycle {
	if stats == nil {
		stats = &db.QueueStats{}
	}

	lc := pipelineLifecycle{
		Phase:           phase,
		TranscribeDone:  stats.Done,
		TranscribeTotal: stats.TotalJobs,
		EmbedBacklog:    stats.EmbedBacklog,
		EvalInPipeline:  evalEnabled,
		GPUProbed:       gpuProbed,
	}

	// Eval coverage.
	if !evalEnabled {
		lc.EvalCoverage = -1 // sentinel: not applicable
	} else if stats.Done > 0 {
		lc.EvalCoverage = float64(stats.EvalCoverageDone) / float64(stats.Done)
	} else {
		lc.EvalCoverage = 0
	}

	// GPU committed: phase says the pipeline is actively using the GPU (either
	// the ASR runner or the eval judge), or the arbiter confirms active use.
	phaseActive := phase == db.PhaseTranscribe || phase == db.PhaseAnalyze
	arbiterActive := arbiter.Reachable && (arbiter.State == "available" || arbiter.State == "")
	// "committed" = pipeline phase owns the GPU, regardless of arbiter state
	lc.GPUCommitted = phaseActive || (gpuProbed && arbiterActive && stats.Claimed > 0)

	// FullyDone: nothing left to do in any stage.
	evalDone := !evalEnabled || lc.EvalCoverage >= 1.0
	lc.FullyDone = stats.Pending == 0 && stats.Claimed == 0 &&
		stats.EmbedBacklog == 0 && evalDone

	// Activity label.
	lc.Activity = deriveActivity(stats, phase, lc.EmbedBacklog, evalEnabled, lc.EvalCoverage, lc.FullyDone, lc.GPUCommitted)

	// Segment bucket counts for the API and the bar template.
	b := stats.PipelineBuckets
	lc.NotStarted = b.Pending + b.Claimed
	lc.Transcribing = b.Claimed
	lc.TranscribedOnly = b.TranscribedOnly
	lc.EvaldOnly = b.EvaldOnly
	lc.EmbeddedReady = b.EmbeddedReady
	lc.BucketFailed = b.Failed

	return lc
}

// deriveActivity maps the pipeline signals onto one of the operator-facing
// activity tokens. Precedence: paused > transcribing > evaluating > embedding
// > winding-down > idle.
func deriveActivity(
	stats *db.QueueStats,
	phase string,
	embedBacklog int,
	evalEnabled bool,
	evalCoverage float64,
	fullyDone bool,
	gpuCommitted bool,
) pipelineActivity {
	if stats.Paused {
		return activityPaused
	}
	// Transcribe phase: runner is claiming jobs.
	if stats.Claimed > 0 || phase == db.PhaseTranscribe {
		return activityTranscribing
	}
	// Analyze phase: eval judge is running.
	if phase == db.PhaseAnalyze {
		return activityEvaluating
	}
	// Embedding catch-up: embed backlog exists and the worker is making progress
	// (not a full stall check — just "something is in the backlog").
	if embedBacklog > 0 {
		return activityEmbedding
	}
	// Transcribe queue drained but not fully done OR GPU still committed.
	if !fullyDone || gpuCommitted {
		// If eval is in-pipeline and coverage < 1, we're still evaluating even
		// if the phase hasn't flipped yet.
		if evalEnabled && evalCoverage >= 0 && evalCoverage < 1.0 {
			return activityEvaluating
		}
		return activityWindingDown
	}
	return activityIdle
}

// WindingDown returns true when the pipeline is in the gap state: transcribe
// drained but eval / embed not yet finished.  Exported so Go templates can call
// it directly (html/template only invokes exported methods).
func (lc pipelineLifecycle) WindingDown() bool {
	return lc.Activity == activityWindingDown || lc.Activity == activityEvaluating
}

// EvalCoverageDisplay returns a human-readable fraction string for the eval
// coverage (e.g. "317 / 317" or "298 / 317"), or "not in pipeline" when eval
// is not enabled.  Exported for template use.
func (lc pipelineLifecycle) EvalCoverageDisplay(done int) string {
	if !lc.EvalInPipeline {
		return "not in pipeline"
	}
	if done == 0 {
		return "0 / 0"
	}
	evalDone := int(lc.EvalCoverage * float64(done))
	return commafy(evalDone) + " / " + commafy(done)
}

// EvalCoveragePct returns the integer percentage (0–100) for a progress bar.
// Returns 0 when eval is not in-pipeline or done==0.  Exported for template use.
func (lc pipelineLifecycle) EvalCoveragePct(done int) int {
	if !lc.EvalInPipeline || done == 0 || lc.EvalCoverage < 0 {
		return 0
	}
	pct := int(lc.EvalCoverage * 100)
	if pct > 100 {
		pct = 100
	}
	return pct
}
