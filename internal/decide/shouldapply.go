package decide

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/fn"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/systemone"
)

// should_apply identity (CONTRACT §2.19). The prompt version is bumped by hand
// whenever the question text changes; PromptSHA256 pins the exact text.
const (
	ShouldApplyName          = "should_apply"
	ShouldApplyPromptVersion = "should_apply@v1"
	// ShouldApplyModel is the pinned decision model. Never an alias.
	ShouldApplyModel = "jev-1.13.0"
	// ShouldApplyStepVersion is bumped when the decide logic around the call
	// changes (context building, evidence, the decision rule).
	ShouldApplyStepVersion = 1
	// questionKey is the System One question name.
	questionKey = "should_apply"
)

// Rung0Version names the rung-0 rule set Evaluate runs. Bump it with any
// change to rung0.go that alters a verdict.
const Rung0Version = "rung0@v1"

// Decisions (Outcome.Decision).
const (
	DecisionApply  = "apply"
	DecisionHold   = "hold"
	DecisionReject = "reject"
)

// Outcome reasons beyond rung 0's Reason* constants. A data contract, like
// those: add, never rename.
const (
	// ReasonConfident — p ≥ apply_p with text evidence: applied.
	ReasonConfident = "confident_with_evidence"
	// ReasonNoEvidence — p ≥ apply_p but no text evidence: held.
	ReasonNoEvidence = "no_evidence"
	// ReasonUncertain — reject_p < p < apply_p: held.
	ReasonUncertain = "uncertain"
	// ReasonTypeCapped — p ≥ apply_p for an issue type that may never be
	// applied by machine (dropped_word, number_artifact): held.
	ReasonTypeCapped = "issue_type_capped"
	// ReasonJevReject — p ≤ reject_p: rejected.
	ReasonJevReject = "jev_reject"
	// ReasonJevUnavailable — the model could not give a usable answer (error,
	// timeout, 4xx/5xx, a reply that does not decode, a fallback model, or a
	// probability outside [0, 1]): held, retryable. NEVER applied.
	ReasonJevUnavailable = "jev_unavailable"
)

// shouldApplyQuestion is the one question asked. Its canonical JSON is hashed
// into PromptSHA256; a golden test pins the hash.
var shouldApplyQuestion = systemone.Question{
	Type: systemone.TypeNoul,
	Instructions: "The state is an excerpt of an automatic speech-recognition (ASR) transcript of an audiobook. " +
		"Transcript text is all lowercase with no punctuation. A reviewer proposes one correction: " +
		"ORIGINAL SENTENCE marks the transcribed words in [[double brackets]] and CORRECTED SENTENCE marks the proposed replacement the same way. " +
		"BEFORE and AFTER are the neighbouring sentences. BOOK REFERENCE lists lines from the publisher's catalogue record of the book that may bear on the edit, or (none). " +
		"Decide whether the narrator actually said the replacement, so that applying it makes the transcript more faithful to the audio.",
	Criteria: map[string]string{
		"true": "The marked original is a transcription error and the replacement is what was spoken: it sounds like the marked words, " +
			"reads naturally in the sentence and its neighbours, and agrees with the book reference wherever the reference names it.",
		"false": "The marked original is plausibly what was spoken, or the replacement does not sound like it, changes the meaning, " +
			"is not supported by the context, conflicts with the book reference, or there is not enough information to tell.",
	},
}

// ShouldApplyPromptSHA256 is the hex sha256 of the question's canonical JSON.
var ShouldApplyPromptSHA256 = func() string {
	canon, _, err := fn.CanonicalInput(map[string]systemone.Question{questionKey: shouldApplyQuestion})
	if err != nil {
		panic(fmt.Sprintf("decide: hash should_apply question: %v", err))
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}()

// ShouldApplyParams are the decide recipe's params: changing any of them
// yields a new recipe (CONTRACT §1.9), so decisions made under different
// thresholds are never confused.
type ShouldApplyParams struct {
	// ApplyP: p at or above it may apply (with evidence). Default 0.95.
	ApplyP float64
	// RejectP: p at or below it rejects. Default 0.20.
	RejectP float64
	// PhoneticMinSim is rung 0's sound-alike threshold. Default 0.67.
	PhoneticMinSim float64
}

// DefaultShouldApplyParams returns the phase-1 thresholds.
func DefaultShouldApplyParams() ShouldApplyParams {
	return ShouldApplyParams{ApplyP: 0.95, RejectP: 0.20, PhoneticMinSim: DefaultParams().SoundAlikeThreshold}
}

// Validate requires 0 ≤ reject_p < apply_p ≤ 1 and a sound-alike threshold
// in (0, 1].
func (p ShouldApplyParams) Validate() error {
	ok := func(f float64) bool { return !math.IsNaN(f) && f >= 0 && f <= 1 }
	switch {
	case !ok(p.ApplyP) || !ok(p.RejectP) || p.RejectP >= p.ApplyP:
		return fmt.Errorf("decide: need 0 ≤ reject_p (%v) < apply_p (%v) ≤ 1", p.RejectP, p.ApplyP)
	case !ok(p.PhoneticMinSim) || p.PhoneticMinSim == 0:
		return fmt.Errorf("decide: phonetic_min_sim %v outside (0, 1]", p.PhoneticMinSim)
	}
	return nil
}

// recipeParams is every knob that changes an outcome, as recorded in the
// recipe.
func (p ShouldApplyParams) recipeParams() map[string]any {
	return map[string]any{
		"apply_p":                p.ApplyP,
		"reject_p":               p.RejectP,
		"phonetic_min_sim":       p.PhoneticMinSim,
		"evidence_rule":          EvidenceRule,
		"rung0_version":          Rung0Version,
		"context_words":          ContextWords,
		"fallback_words":         FallbackWords,
		"max_relevant_sentences": MaxRelevantSentences,
		"max_relevant_runes":     MaxRelevantRunes,
	}
}

// ShouldApplyFn returns the should_apply function for params.
func ShouldApplyFn(p ShouldApplyParams) (fn.Fn, error) {
	if err := p.Validate(); err != nil {
		return fn.Fn{}, err
	}
	return fn.New(fn.Fn{
		Name:          ShouldApplyName,
		Step:          recipe.StepDecide,
		PromptVersion: ShouldApplyPromptVersion,
		PromptSHA256:  ShouldApplyPromptSHA256,
		ModelAlias:    ShouldApplyModel,
		StepVersion:   ShouldApplyStepVersion,
		Params:        p.recipeParams(),
	})
}

// cappedTypes may never be applied by machine (D2): an inserted word and a
// number's written form cannot be checked against the audio by any rung, so
// their best outcome is hold.
var cappedTypes = map[string]bool{IssueDroppedWord: true, IssueNumberArtifact: true}

// Decide maps the model's probability, the text evidence and the issue type
// to a decision and its reason:
//
//	p NaN or outside [0, 1]          → hold   jev_unavailable
//	p ≥ apply_p, capped issue type   → hold   issue_type_capped
//	p ≥ apply_p, evidence            → apply  confident_with_evidence
//	p ≥ apply_p, no evidence         → hold   no_evidence
//	reject_p < p < apply_p           → hold   uncertain
//	p ≤ reject_p                     → reject jev_reject
func (p ShouldApplyParams) Decide(prob float64, evidence, issueType string) (decision, reason string) {
	switch {
	case math.IsNaN(prob) || prob < 0 || prob > 1:
		return DecisionHold, ReasonJevUnavailable
	case prob >= p.ApplyP:
		switch {
		case cappedTypes[issueType]:
			return DecisionHold, ReasonTypeCapped
		case evidence == EvidenceASINVerbatim || evidence == EvidenceExactRepeat:
			return DecisionApply, ReasonConfident
		default:
			return DecisionHold, ReasonNoEvidence
		}
	case prob <= p.RejectP:
		return DecisionReject, ReasonJevReject
	default:
		return DecisionHold, ReasonUncertain
	}
}

// Asker is the System One call (*systemone.Client implements it).
type Asker interface {
	Decide(ctx context.Context, req systemone.Request) (*systemone.Response, error)
}

// Input is everything Evaluate needs about one finding.
type Input struct {
	Candidate Candidate
	// Verdict is the candidate's rung-0 verdict when the caller already ran
	// Rung0 over the whole chunk (Dedupe needs every candidate of a chunk at
	// once). nil runs Check on this candidate alone — no dedupe.
	Verdict *Verdict
	// Chunk carries the chunk's time window; its Text must equal
	// Candidate.ChunkText.
	Chunk ChunkWindow
	// Segments is the transcript's full segment list (db.GetTranscriptSegments).
	Segments []db.Segment
	// Record is the book's catalogue record (db.GetBookRecords); nil when the
	// book has none.
	Record *db.BookRecord
}

// Outcome is the decide step's result for one finding — what PR5/PR6 persist
// as a decision event.
type Outcome struct {
	FindingID string
	// Decision is apply, hold or reject.
	Decision string
	// Reason is a rung-0 Reason* (on a rung-0 reject) or one of the reasons
	// above.
	Reason string
	// Retryable marks a hold that a later run may resolve without a recipe
	// change (jev_unavailable).
	Retryable bool
	// P is the model's probability that the correction should be applied;
	// nil when the model was not asked or gave no usable answer.
	P *float64
	// Evidence is asin_verbatim, exact_repeat or none.
	Evidence string
	// Rung0 is the rung-0 verdict the outcome rests on.
	Rung0 Verdict
	// ChunkHash is the chunk revision decided on (the finding's
	// chunk_text_sha256, verified by rung 0).
	ChunkHash string
	// FnCallID is the fn_calls row of the model call (0 when not asked).
	FnCallID int64
	// RecipeID is the decide recipe of the model call ("" when not asked or
	// the call failed before a reply).
	RecipeID string
	// Model is the model that answered ("" when none did).
	Model    string
	CacheHit bool
}

// Evaluator runs should_apply.
type Evaluator struct {
	fn     fn.Fn
	params ShouldApplyParams
	store  fn.Store
	asker  Asker
}

// NewEvaluator builds an Evaluator. store logs and caches the model calls
// (*db.DB); asker makes them (*systemone.Client).
func NewEvaluator(store fn.Store, asker Asker, p ShouldApplyParams) (*Evaluator, error) {
	if store == nil || asker == nil {
		return nil, errors.New("decide: evaluator needs a store and a System One client")
	}
	f, err := ShouldApplyFn(p)
	if err != nil {
		return nil, err
	}
	return &Evaluator{fn: f, params: p, store: store, asker: asker}, nil
}

// Evaluate decides one finding: rung 0 (in.Verdict, or Check), then text
// evidence, then — only for a rung-0 pass — the should_apply model call, then
// Decide. It never returns an error: every failure past rung 0 is a hold with
// reason jev_unavailable, so an outage can delay a correction but never apply
// one.
func (e *Evaluator) Evaluate(ctx context.Context, in Input) Outcome {
	c := in.Candidate
	var v Verdict
	if in.Verdict != nil {
		v = *in.Verdict
	} else {
		v = Check(c, Params{SoundAlikeThreshold: e.params.PhoneticMinSim})
	}
	out := Outcome{FindingID: c.FindingID, Rung0: v, ChunkHash: c.ChunkHash, Evidence: EvidenceNone}
	if !v.Pass {
		out.Decision, out.Reason = DecisionReject, v.Reason
		return out
	}

	sentences := RecordSentences(in.Record)
	out.Evidence = TextEvidence(c, v, sentences)
	chunk := in.Chunk
	chunk.Text = c.ChunkText
	state := BuildState(c.IssueType, BuildContext(in.Segments, chunk, v.Span, c.Replacement),
		Relevant(sentences, c.Original, c.Replacement))

	unavailable := func() Outcome {
		out.Decision, out.Reason, out.Retryable, out.P = DecisionHold, ReasonJevUnavailable, true, nil
		return out
	}
	res, meta, err := e.fn.Invoke(ctx, e.store, shouldApplyInput{State: state}, e.call(state))
	out.FnCallID, out.RecipeID, out.CacheHit = meta.CallID, meta.RecipeID, meta.CacheHit
	out.Model = res.Model
	if err != nil || meta.Fallback {
		return unavailable()
	}
	p, err := decodeShouldApply(res.Output)
	if err != nil {
		return unavailable()
	}
	out.P = &p
	out.Decision, out.Reason = e.params.Decide(p, out.Evidence, c.IssueType)
	out.Retryable = out.Reason == ReasonJevUnavailable
	return out
}

// shouldApplyInput is the fn_calls input: the full state the model saw. The
// question is pinned by the prompt hash, the model by the alias.
type shouldApplyInput struct {
	State string `json:"state"`
}

// shouldApplyOutput is what a successful call stores in fn_calls.output and
// what a cache hit is decoded from.
type shouldApplyOutput struct {
	Model   string                      `json:"model"`
	Answers map[string]systemone.Answer `json:"answers"`
}

func (e *Evaluator) call(state string) fn.Call {
	return func(ctx context.Context) (fn.Result, error) {
		resp, err := e.asker.Decide(ctx, systemone.Request{
			Model:     ShouldApplyModel,
			State:     state,
			Questions: map[string]systemone.Question{questionKey: shouldApplyQuestion},
		})
		if err != nil {
			return fn.Result{}, err
		}
		if resp == nil {
			return fn.Result{}, &systemone.DecodeError{Reason: "empty reply"}
		}
		raw, err := json.Marshal(shouldApplyOutput{Model: resp.Model, Answers: resp.Answers})
		if err != nil {
			return fn.Result{}, fmt.Errorf("decide: encode reply: %w", err)
		}
		in, outTok := resp.Usage.InputTokens, resp.Usage.OutputTokens
		cost := resp.CostUSD
		return fn.Result{Output: raw, Model: resp.Model, InputTokens: &in, OutputTokens: &outTok, CostUSD: &cost}, nil
	}
}

// decodeShouldApply reads the probability from a stored output, strictly:
// unknown fields, a missing or extra-typed answer, or a value outside [0, 1]
// are errors. The live reply shape is documented, not yet recorded
// (systemone.TestLiveSmoke), so anything unexpected fails closed.
func decodeShouldApply(raw json.RawMessage) (float64, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out shouldApplyOutput
	if err := dec.Decode(&out); err != nil {
		return 0, &systemone.DecodeError{Reason: "stored output does not decode"}
	}
	if dec.More() {
		return 0, &systemone.DecodeError{Reason: "stored output has trailing data"}
	}
	a, ok := out.Answers[questionKey]
	switch {
	case !ok:
		return 0, &systemone.DecodeError{Reason: "should_apply has no answer"}
	case a.Type != systemone.TypeNoul:
		return 0, &systemone.DecodeError{Reason: "should_apply answer is not noul"}
	case a.Noul == nil:
		return 0, &systemone.DecodeError{Reason: "should_apply noul is missing"}
	case math.IsNaN(*a.Noul) || *a.Noul < 0 || *a.Noul > 1:
		return 0, &systemone.DecodeError{Reason: "should_apply noul outside [0,1]"}
	}
	return *a.Noul, nil
}

// BuildState renders the labelled state text the model is asked about. It is
// deterministic — the same inputs give byte-identical state, which is what
// makes the fn_calls cache hit.
func BuildState(issueType string, c Context, record []RecordSentence) string {
	var b strings.Builder
	block := func(label, text string) {
		if strings.TrimSpace(text) == "" {
			text = "(none)"
		}
		b.WriteString(label)
		b.WriteString(":\n")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	block("ISSUE TYPE", issueType)
	block("ORIGINAL SENTENCE", c.Original)
	block("CORRECTED SENTENCE", c.Corrected)
	block("BEFORE", c.Before)
	block("AFTER", c.After)
	var lines []string
	for _, s := range record {
		lines = append(lines, "- "+s.Field+": "+s.Text)
	}
	block("BOOK REFERENCE", strings.Join(lines, "\n"))
	return strings.TrimRight(b.String(), "\n") + "\n"
}
