// Package eval implements the read-only LLM-as-judge eval layer (CONTRACT
// §2.15, GitHub issue #49).
//
// The judge READS transcript chunks and records suspected transcription errors
// as PROPOSED PATCHES in transcript_findings. It NEVER edits transcripts: this
// package issues no UPDATE/DELETE/ALTER against transcripts/segments/
// transcript_chunks. The asymmetry is the whole point — a wrong flag is
// harmless, a wrong autonomous correction would corrupt the corpus, so
// corrections (suggested_correction) are recorded but never applied *here*.
//
// Applying a correction is a human decision and lives in internal/patch
// (CONTRACT §2.17). The split is deliberate: keeping the write path in another
// package is what makes the read-only guarantee mechanically checkable rather
// than a promise in a comment. If you are about to add an UPDATE to this
// package, you want internal/patch instead.
//
// Cost is operator-bounded: the judge runs on-demand per book or over a random
// sample of N chunks (never every segment of every book), behind a dry-run gate.
package eval

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// defaultMaxFindingsPerChunk bounds how many findings the judge keeps for a
// single chunk. The judge over-flags in practice (one chunk produced 31;
// llama3.2:3b and qwen2.5:7b both averaged ~3/chunk over a 50-chunk sample, but
// the tail is long), so a per-chunk cap keeps the highest-confidence signal and
// drops the noisy remainder. Lowered 8 → 5 with taxonomy rev 2: a ~10-minute
// chunk with more than a handful of genuine ASR errors is rare, so a tighter cap
// trims the over-flagged tail. Tunable via EVAL_MAX_FINDINGS_PER_CHUNK; <= 0
// disables the cap.
const defaultMaxFindingsPerChunk = 5

// defaultMinConfidence is the floor below which a finding is dropped. A
// ground-truth audit found high-confidence (≥0.8) findings were ~100% real while
// the low tail was mostly noise, so a floor trades a little recall for precision.
// 0.6 keeps the "looks wrong, correction is a guess" band and up; tune via
// EVAL_MIN_CONFIDENCE. A value <= 0 disables the floor.
const defaultMinConfidence = 0.6

// maxFindingsPerChunk resolves the per-chunk cap from EVAL_MAX_FINDINGS_PER_CHUNK,
// falling back to defaultMaxFindingsPerChunk. A blank/invalid value uses the
// default; an explicit <= 0 disables capping (returned as 0).
func maxFindingsPerChunk() int {
	raw := os.Getenv("EVAL_MAX_FINDINGS_PER_CHUNK")
	if raw == "" {
		return defaultMaxFindingsPerChunk
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultMaxFindingsPerChunk
	}
	if n < 0 {
		return 0
	}
	return n
}

// minConfidence resolves the confidence floor from EVAL_MIN_CONFIDENCE, falling
// back to defaultMinConfidence. A blank/invalid value uses the default; an
// explicit <= 0 disables the floor (returned as 0). Values are not clamped to
// [0,1] here — a floor above 1 simply drops everything, which is a valid (if
// extreme) operator choice.
func minConfidence() float64 {
	raw := os.Getenv("EVAL_MIN_CONFIDENCE")
	if raw == "" {
		return defaultMinConfidence
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return defaultMinConfidence
	}
	if f < 0 {
		return 0
	}
	return f
}

// ChatClient is the small abstraction over the chat-LLM endpoint the judge
// needs. Implemented by openAIChatClient (OpenAI-compatible /v1/chat/completions)
// and faked in tests. Keeping the judge behind this interface means the endpoint
// binding (AI registry vs env-var fallback) is resolved in one place
// (ResolveChatClient) rather than threaded through the judge.
type ChatClient interface {
	// Complete sends a system + user prompt and returns the model's raw text
	// response. The judge is responsible for parsing JSON out of it.
	Complete(ctx context.Context, system, user string) (string, error)
	// Model returns the judge model id, recorded on each finding for
	// attribution.
	Model() string
}

// ModelReportingClient is an optional ChatClient extension for endpoints that
// report which model actually served a request (the OpenAI response "model"
// field). A router such as LiteLLM can resolve an alias to a dated id or fall
// back to a different model, so the judge records that alongside the requested
// Model(). openAIChatClient implements it; a ChatClient that doesn't simply
// records no resolved model.
type ModelReportingClient interface {
	CompleteWithModel(ctx context.Context, system, user string) (Completion, error)
}

// TemperatureReporter is an optional ChatClient extension reporting the
// sampling temperature the client actually sends: nil when it omits the field
// (hosted routes by default — the provider's default applies). The judge's
// recipe params and the gen_ai.request.temperature span attribute follow it.
// openAIChatClient implements it; a ChatClient that doesn't is assumed to send
// temperature 0 (the historical judge setting).
type TemperatureReporter interface {
	Temperature() *float64
}

// ChunkReader is the read-only slice of the DB the judge needs to fetch chunks.
// Intentionally read-only — there is no transcript-mutating method here.
type ChunkReader interface {
	GetEvalChunksForBook(ctx context.Context, dir string, limit int) ([]db.EvalChunk, error)
	SampleEvalChunks(ctx context.Context, limit int) ([]db.EvalChunk, error)
}

// FindingWriter is the insert-only slice of the DB the judge writes through.
// Insert-only by construction — no update/delete.
type FindingWriter interface {
	InsertFindings(ctx context.Context, findings []db.Finding) error
}

// Judge runs the LLM-as-judge over chunks and turns its output into findings.
type Judge struct {
	chat   ChatClient
	logger log.Logger
	// maxPerChunk caps findings kept per chunk (highest-confidence first); 0
	// disables the cap. Resolved once from EVAL_MAX_FINDINGS_PER_CHUNK at
	// construction so a single value applies for the whole run.
	maxPerChunk int
	// minConf drops findings whose confidence is below this floor; 0 disables.
	// Resolved once from EVAL_MIN_CONFIDENCE at construction.
	minConf float64
	// pin is the model registry's entry for the propose step (CONTRACT §2.18).
	pin ModelPin
	// resolvedWarn fires the not-the-expected-model warning once per judge.
	resolvedWarn sync.Once
}

// ModelPin is the model registry's pin for the judge: the model expected to
// answer for the requested alias, and its revision (config.ModelPin, without
// the eval core importing config).
type ModelPin struct {
	ExpectedModel string
	Revision      string
}

// proposeStepVersion is bumped whenever the judge's own logic (parsing,
// filtering, capping, anchoring) changes which findings it writes (§1.9).
const proposeStepVersion = 1

// NewJudge constructs a Judge backed by the given chat client.
func NewJudge(chat ChatClient) *Judge {
	return &Judge{
		chat:        chat,
		logger:      log.NewLogger("eval"),
		maxPerChunk: maxFindingsPerChunk(),
		minConf:     minConfidence(),
	}
}

// SetModelPin records the registry pin for the propose step; it feeds the
// judge's recipe.
func (j *Judge) SetModelPin(pin ModelPin) { j.pin = pin }

// Recipe is the judge's CURRENT recipe (CONTRACT §1.9): what it asks for, the
// model expected to answer (the registry's pin, else the requested model), the
// prompt version and hash, and the parameters that shape its findings.
func (j *Judge) Recipe() recipe.Recipe {
	model := j.Model()
	expected := j.pin.ExpectedModel
	if expected == "" {
		expected = model
	}
	return recipe.Recipe{
		Step:          recipe.StepPropose,
		StepVersion:   proposeStepVersion,
		CodeVersion:   recipe.CodeVersion(),
		ModelAlias:    model,
		ModelResolved: expected,
		ModelRevision: j.pin.Revision,
		PromptVersion: judgePromptVersion,
		PromptSHA256:  judgePromptSHA256(),
		Params:        j.recipeParams(),
	}
}

// recipeParams are the propose recipe's output-shaping params. temperature is
// recorded only when it is actually sent (sentTemperature): a hosted route
// that omits it has no temperature param, which is a different recipe from
// the historical temperature-0 one (CONTRACT §1.9 / §2.15). max_tokens is not
// a param — it bounds the reply's length, and a reply that hits it is an error
// (ErrTruncatedResponse), never a finding set.
func (j *Judge) recipeParams() map[string]any {
	params := map[string]any{
		"min_confidence":         j.minConf,
		"max_findings_per_chunk": j.maxPerChunk,
	}
	if t := j.sentTemperature(); t != nil {
		params["temperature"] = *t
	}
	return params
}

// sentTemperature is the temperature the judge's chat client sends, nil when
// omitted. A client that does not report it is assumed to send 0.
func (j *Judge) sentTemperature() *float64 {
	if j == nil || j.chat == nil {
		return nil
	}
	if tr, ok := j.chat.(TemperatureReporter); ok {
		return tr.Temperature()
	}
	zero := 0.0
	return &zero
}

// recipeFor is the recipe that actually produced a reply: the current recipe
// with model_resolved set to the model the endpoint REPORTED serving it ("what
// answered"). A fallback model therefore stamps a different recipe, which
// InsertFindings registers on the fly. An endpoint that reports nothing keeps
// the expected model.
func (j *Judge) recipeFor(resolved string) recipe.Recipe {
	r := j.Recipe()
	if resolved != "" {
		r.ModelResolved = resolved
	}
	return r
}

// Model reports the judge's chat model id (for run_metrics.eval_model
// attribution). Returns "" if the judge has no chat client.
func (j *Judge) Model() string {
	if j == nil || j.chat == nil {
		return ""
	}
	return j.chat.Model()
}

// Result is the outcome of judging one chunk: the findings derived from it and
// any error (the caller decides whether to abort the run or skip the chunk).
type Result struct {
	Chunk    db.EvalChunk
	Findings []db.Finding
	// ResolvedModel is the model the endpoint reported serving this chunk
	// ("" when unknown).
	ResolvedModel string
	// Usage is the call's token and cost accounting, as the endpoint reported
	// it (also set on a failed call that got a reply).
	Usage Usage
	// Elapsed is the wall time of the model call.
	Elapsed time.Duration
	// Dropped is every finding the judge returned that did not become a row,
	// with the reason.
	Dropped []Dropped
}

// Usage is one judge call's accounting: token counts from the response's
// usage block and the gateway-reported cost (Completion).
type Usage struct {
	InputTokens  int
	OutputTokens int
	HasUsage     bool
	CostUSD      float64
	HasCost      bool
}

func usageOf(c Completion) Usage {
	return Usage{InputTokens: c.InputTokens, OutputTokens: c.OutputTokens, HasUsage: c.HasUsage,
		CostUSD: c.CostUSD, HasCost: c.HasCost}
}

// JudgeChunk evaluates a single chunk: build the prompt, call the model, parse
// the response, and map suspected errors into db.Finding rows (attributed to the
// chunk, its transcript, and its run). It performs NO database writes — the
// caller persists via FindingWriter only when not in dry-run.
func (j *Judge) JudgeChunk(ctx context.Context, c db.EvalChunk) (Result, error) {
	system, user := buildPrompt(c)
	var ep Endpoint
	if er, ok := j.chat.(EndpointReporter); ok {
		ep = er.Endpoint()
	}
	ctx, span := startChatSpan(ctx, j.chat.Model(), j.sentTemperature(), ep, chunkRef{transcriptID: c.TranscriptID, chunkID: c.ChunkID})
	callStart := time.Now()
	comp, err := j.complete(ctx, system, user)
	elapsed := time.Since(callStart)
	resolved := comp.ResolvedModel
	var recipeID string
	if err == nil {
		recipeID, _ = j.recipeFor(resolved).ID()
	}
	endChatSpan(ctx, span, j.chat.Model(), j.pin.ExpectedModel, comp, recipeID, err)
	if err != nil {
		return Result{Chunk: c, Usage: usageOf(comp), Elapsed: elapsed}, fmt.Errorf("judge chunk %s: %w", c.ChunkID, err)
	}
	if resolved != "" && resolved != j.chat.Model() {
		j.logger.DebugContext(ctx, "judge request served by a different model id",
			"chunk_id", c.ChunkID, "requested", j.chat.Model(), "resolved", resolved)
	}
	if expected := j.Recipe().ModelResolved; servedUnexpectedModel(expected, resolved) {
		j.resolvedWarn.Do(func() {
			j.logger.WarnContext(ctx, "the eval endpoint reported a different model than the propose recipe expects; "+
				"findings it answers are stamped with a non-current recipe and listed in stale_work. "+
				"If this is the normal answer for the alias (e.g. LiteLLM reporting the provider id), "+
				"pin steps.propose.expected_model in MODELS_FILE (CONTRACT §2.18); if it is a fallback, this is expected",
				"requested", j.chat.Model(), "expected", expected, "reported", resolved,
				"expected_model_pinned", j.pin.ExpectedModel != "")
		})
	}

	res := Result{Chunk: c, ResolvedModel: resolved, Usage: usageOf(comp), Elapsed: elapsed}
	parsed, dropped, perr := parseFindings(comp.Content)
	if perr != nil {
		// A malformed judge response is a soft failure: log and treat the chunk
		// as "no findings" rather than aborting the whole run. The judge is
		// advisory; a parse miss costs nothing.
		j.logger.WarnContext(ctx, "dropping unparseable judge response",
			"chunk_id", c.ChunkID, "recipe_id", recipeID, "error", perr)
		return res, nil
	}

	parsed, dropped = j.floorFindings(c, parsed, dropped)
	parsed, dropped = j.capFindings(c, parsed, dropped)
	res.Dropped = dropped

	model := j.chat.Model()
	rec := j.recipeFor(resolved)
	findings := make([]db.Finding, 0, len(parsed))
	chunkID := c.ChunkID
	chunkIndex := c.ChunkIndex
	runID := c.TranscriptionRunID
	for _, p := range parsed {
		findings = append(findings, db.Finding{
			TranscriptID:        c.TranscriptID,
			FilePath:            c.FilePath,
			ChunkID:             &chunkID,
			ChunkIndex:          &chunkIndex,
			StartSec:            c.StartSec,
			EndSec:              c.EndSec,
			OriginalText:        p.OriginalText,
			IssueType:           p.IssueType,
			SuggestedCorrection: optionalStr(p.SuggestedCorrection),
			Confidence:          p.Confidence,
			Model:               model,
			ResolvedModel:       optionalStr(resolved),
			TranscriptionRunID:  optionalStr(runID),
			AnchorOffset:        optionalInt(p.AnchorOffset),
			AnchorOccurrence:    optionalInt(p.AnchorOccurrence),
			// Fingerprint the chunk exactly as the judge saw it. This is what
			// lets the apply path later prove it is editing the same revision
			// the model reviewed, instead of text that changed in between.
			ChunkTextSHA256: optionalStr(patch.ChunkHash(c.Text)),
			Recipe:          &rec,
		})
	}
	res.Findings = findings
	return res, nil
}

// complete calls the chat client, using CompleteWithModel when the client
// reports its resolved model (and usage).
func (j *Judge) complete(ctx context.Context, system, user string) (Completion, error) {
	if mr, ok := j.chat.(ModelReportingClient); ok {
		return mr.CompleteWithModel(ctx, system, user)
	}
	raw, err := j.chat.Complete(ctx, system, user)
	return Completion{Content: raw}, err
}

// floorFindings drops findings below the confidence floor (j.minConf). Applied
// before capFindings so the cap operates on the survivors. A floor of 0
// (disabled) returns the input unchanged. Logs at DEBUG when it drops any.
func (j *Judge) floorFindings(c db.EvalChunk, parsed []parsedFinding, dropped []Dropped) ([]parsedFinding, []Dropped) {
	if j.minConf <= 0 || len(parsed) == 0 {
		return parsed, dropped
	}
	kept := parsed[:0:0]
	for _, p := range parsed {
		if p.Confidence >= j.minConf {
			kept = append(kept, p)
		} else {
			dropped = append(dropped, dropOf(p, DropBelowMinConfidence))
		}
	}
	if n := len(parsed) - len(kept); n > 0 {
		j.logger.Debug("dropping low-confidence chunk findings",
			"chunk_id", c.ChunkID, "kept", len(kept), "dropped", n, "floor", j.minConf)
	}
	return kept, dropped
}

// capFindings bounds a single chunk's findings to j.maxPerChunk, keeping the
// highest-confidence ones (the judge over-flags, and a wrong flag is only noise
// — so when forced to drop, drop the least-confident). It sorts by confidence
// descending (stable, so equal-confidence findings keep their original order
// for a deterministic result) and truncates. A cap of 0 (disabled) or a set
// already within the cap is returned unchanged. Logs at DEBUG when it truncates.
func (j *Judge) capFindings(c db.EvalChunk, parsed []parsedFinding, dropped []Dropped) ([]parsedFinding, []Dropped) {
	if j.maxPerChunk <= 0 || len(parsed) <= j.maxPerChunk {
		return parsed, dropped
	}
	sort.SliceStable(parsed, func(a, b int) bool {
		return parsed[a].Confidence > parsed[b].Confidence
	})
	for _, p := range parsed[j.maxPerChunk:] {
		dropped = append(dropped, dropOf(p, DropOverCap))
	}
	j.logger.Debug("capping over-flagged chunk findings",
		"chunk_id", c.ChunkID, "kept", j.maxPerChunk, "dropped", len(parsed)-j.maxPerChunk, "cap", j.maxPerChunk)
	return parsed[:j.maxPerChunk], dropped
}

// servedUnexpectedModel reports whether a response came from a model other
// than the one the current recipe expects. An endpoint that reports nothing is
// not a mismatch (the finding keeps the expected model).
func servedUnexpectedModel(expected, reported string) bool {
	return reported != "" && reported != expected
}

// optionalStr maps an empty string to nil (NULL), else a pointer to the value.
func optionalStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// optionalInt maps the "-1 means unknown" anchor convention onto a nullable
// column. A negative value becomes NULL so the database records "the model did
// not say" rather than a position nothing should trust.
func optionalInt(i int) *int {
	if i < 0 {
		return nil
	}
	return &i
}
