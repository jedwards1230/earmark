package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/fn"
	"github.com/jedwards1230/earmark/internal/genai"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/systemone"
)

// should_apply identity (CONTRACT §2.19). The prompt version is bumped by hand
// whenever the question text or the state layout changes; PromptSHA256 pins
// both.
const (
	ShouldApplyName          = "should_apply"
	ShouldApplyPromptVersion = "should_apply@v2"
	// ShouldApplyModel is the pinned decision model. Never an alias.
	ShouldApplyModel = "jev-1.13.0"
	// ShouldApplyStepVersion is bumped when the decide logic around the call
	// changes (context building, evidence, the decision rule). 3: agreeing
	// findings are grouped (one representative decided, the rest recorded as
	// duplicates) and overlapping different fixes are ranked by the model's p
	// instead of judge confidence (CONTRACT §2.19 "Agreeing and conflicting
	// findings").
	ShouldApplyStepVersion = 3
	// questionKey is the System One question name.
	questionKey = "should_apply"
)

// Rung0Version names the rung-0 rule set Evaluate runs. Bump it with any
// change to rung0.go that alters a verdict.
const Rung0Version = "rung0@v2"

// Decisions (Outcome.Decision).
const (
	DecisionApply  = "apply"
	DecisionHold   = "hold"
	DecisionReject = "reject"
	// DecisionDuplicate — the finding proposes the same fix as its group's
	// representative (Outcome.Related), which carries the decision. Never
	// applied, never a rejection.
	DecisionDuplicate = "duplicate"
)

// Outcome reasons beyond rung 0's Reason* constants. A data contract, like
// those: add, never rename.
const (
	// ReasonConfident — p ≥ apply_p with allowed text evidence: applied.
	ReasonConfident = "confident_with_evidence"
	// ReasonNoEvidence — p ≥ apply_p but no text evidence: held.
	ReasonNoEvidence = "no_evidence"
	// ReasonUncertain — reject_p < p < apply_p: held.
	ReasonUncertain = "uncertain"
	// ReasonTypeCapped — p ≥ apply_p for an issue type that may never be
	// applied by machine (dropped_word, number_artifact): held.
	ReasonTypeCapped = "issue_type_capped"
	// ReasonEvidenceExcluded — p ≥ apply_p with text evidence of a kind the
	// policy's apply_evidence leaves out (e.g. exact_repeat for Clef): held.
	ReasonEvidenceExcluded = "evidence_excluded"
	// ReasonCapitalised — p ≤ reject_p, but the policy rejects only an
	// all-lowercase replacement (or p < reject_floor) and this one has an
	// uppercase letter (a name the model may simply not know): held.
	ReasonCapitalised = "capitalised_replacement"
	// ReasonJevReject — p ≤ reject_p: rejected.
	ReasonJevReject = "jev_reject"
	// ReasonDuplicate — grouped as a duplicate of the representative.
	ReasonDuplicate = db.ReasonDuplicate
	// ReasonJevUnavailable — the model could not give a usable answer (error,
	// timeout, 4xx/5xx, a reply that does not decode, a fallback model, or a
	// probability outside [0, 1]): held, retryable. NEVER applied.
	ReasonJevUnavailable = "jev_unavailable"
)

// shouldApplyQuestion is the one question asked. Its canonical JSON is hashed,
// with the state layout, into PromptSHA256; a golden test pins the hash.
//
// v2 (should_apply@v2) asks a question the text can settle — is ORIGINAL
// wrong where it stands, and does PROPOSED restore what was said — instead of
// v1's "did the narrator say it", which a model that never hears the audio can
// only hedge on. It is kept short because it is paid on every call: what the
// text is (ASR, lowercase and unpunctuated by design), what the edit does, the
// error class (mishearing, stray repeats), and that the book reference spells
// names. The criteria name restyling (spelling variants, number forms, tense,
// rewording) of a wording that already fits as a reason for false, and leave
// uncertainty to the hold band rather than naming it as a reason for false (a
// reject is final). The text evidence the apply rule requires is deliberately
// NOT stated in the state: telling the model would raise p on exactly the
// findings the evidence gate lets through, so the two keys of the apply rule
// would no longer be independent.
var shouldApplyQuestion = systemone.Question{
	Type: systemone.TypeNoul,
	Instructions: "Audiobook ASR transcript, lowercase and unpunctuated by design. " +
		"The edit replaces the [[ ]] words of ORIGINAL with those of PROPOSED. " +
		"ASR mishears words as similar-sounding ones and sometimes repeats one; BOOK REFERENCE spells the book's names. " +
		"Is ORIGINAL wrong where it stands, and does PROPOSED restore what was said?",
	Criteria: map[string]string{
		"true":  "ORIGINAL is a garbled word, a misspelt name or a stray repeat, and PROPOSED sounds like it and fits BEFORE and AFTER.",
		"false": "ORIGINAL is real wording that fits, or PROPOSED only restyles it (spelling variant, number form, tense, rewording), sounds different, or contradicts the book reference.",
	},
}

// State labels, in layout order. They are hashed into ShouldApplyPromptSHA256
// with the question: the state is the model's input, so a layout change is a
// prompt change.
const (
	labelIssueType = "ISSUE TYPE"
	labelOriginal  = "ORIGINAL"
	labelProposed  = "PROPOSED"
	labelBefore    = "BEFORE"
	labelAfter     = "AFTER"
	labelReference = "BOOK REFERENCE"
	emptyBlock     = "(none)"
	linePrefix     = "- "
)

var stateLayout = []string{
	labelIssueType, labelOriginal, labelProposed, labelBefore, labelAfter, labelReference, emptyBlock, linePrefix,
}

// ShouldApplyPromptSHA256 is the hex sha256 (recipe.PromptSHA256) of the
// question's canonical JSON and the state layout.
var ShouldApplyPromptSHA256 = shouldApplyPromptSHA256(shouldApplyQuestion, stateLayout)

func shouldApplyPromptSHA256(q systemone.Question, layout []string) string {
	canon, _, err := fn.CanonicalInput(map[string]systemone.Question{questionKey: q})
	if err != nil {
		panic(fmt.Sprintf("decide: hash should_apply question: %v", err)) // static data
	}
	return recipe.PromptSHA256(append([]string{string(canon)}, layout...)...)
}

// ShouldApplyParams are the decide recipe's params: changing any of them
// yields a new recipe (CONTRACT §1.9), so decisions made under different
// thresholds are never confused. ApplyP, RejectP, ApplyEvidence,
// RejectRequiresLowercase and RejectFloor are the decide policy (CONTRACT
// §2.19 "Decide policy"): per decide model, resolved by ResolvePolicy.
type ShouldApplyParams struct {
	// ApplyP: p at or above it may apply (with allowed evidence). Default 0.85.
	ApplyP float64
	// RejectP: p at or below it rejects. Default 0.20.
	RejectP float64
	// ApplyEvidence are the text-evidence kinds an apply needs (a subset of
	// asin_verbatim, exact_repeat; non-empty). Default both.
	ApplyEvidence []string
	// RejectRequiresLowercase: a p ≤ reject_p reject also needs a replacement
	// with no uppercase letter, or p < RejectFloor; otherwise it holds
	// (capitalised_replacement). Default false.
	RejectRequiresLowercase bool
	// RejectFloor: with RejectRequiresLowercase, p below it rejects whatever
	// the case. In [0, reject_p]; must be 0 without RejectRequiresLowercase.
	RejectFloor float64
	// PhoneticMinSim is rung 0's sound-alike threshold. Default 0.67.
	PhoneticMinSim float64
}

// applyEvidenceKinds are the evidence kinds an apply may rest on, in
// canonical order.
var applyEvidenceKinds = []string{EvidenceASINVerbatim, EvidenceExactRepeat}

// DefaultShouldApplyParams returns the default policy: apply at p ≥ 0.85
// with either kind of text evidence, reject at p ≤ 0.20. It is also Jev's
// preset (CONTRACT §2.19 "Decide policy").
func DefaultShouldApplyParams() ShouldApplyParams {
	return ShouldApplyParams{
		ApplyP: 0.85, RejectP: 0.20,
		ApplyEvidence:  slices.Clone(applyEvidenceKinds),
		PhoneticMinSim: DefaultParams().SoundAlikeThreshold,
	}
}

// Validate requires 0 ≤ reject_p < apply_p ≤ 1, a non-empty set of known
// apply evidence kinds without repeats, 0 ≤ reject_floor ≤ reject_p (and 0
// unless reject_requires_lowercase), and a sound-alike threshold in (0, 1].
func (p ShouldApplyParams) Validate() error {
	ok := func(f float64) bool { return !math.IsNaN(f) && f >= 0 && f <= 1 }
	switch {
	case !ok(p.ApplyP) || !ok(p.RejectP) || p.RejectP >= p.ApplyP:
		return fmt.Errorf("decide: need 0 ≤ reject_p (%v) < apply_p (%v) ≤ 1", p.RejectP, p.ApplyP)
	case !ok(p.PhoneticMinSim) || p.PhoneticMinSim == 0:
		return fmt.Errorf("decide: phonetic_min_sim %v outside (0, 1]", p.PhoneticMinSim)
	case len(p.ApplyEvidence) == 0:
		return fmt.Errorf("decide: apply_evidence is empty (allowed: %s)", strings.Join(applyEvidenceKinds, ", "))
	case !ok(p.RejectFloor) || p.RejectFloor > p.RejectP:
		return fmt.Errorf("decide: need 0 ≤ reject_floor (%v) ≤ reject_p (%v)", p.RejectFloor, p.RejectP)
	case p.RejectFloor != 0 && !p.RejectRequiresLowercase:
		return fmt.Errorf("decide: reject_floor (%v) only applies with reject_requires_lowercase=true; "+
			"to turn the lowercase rule off, also set reject_floor=0", p.RejectFloor)
	}
	seen := map[string]bool{}
	for _, k := range p.ApplyEvidence {
		switch {
		case !slices.Contains(applyEvidenceKinds, k):
			return fmt.Errorf("decide: apply_evidence %q is not an evidence kind (allowed: %s)", k, strings.Join(applyEvidenceKinds, ", "))
		case seen[k]:
			return fmt.Errorf("decide: apply_evidence lists %q twice", k)
		}
		seen[k] = true
	}
	return nil
}

// applyEvidence is ApplyEvidence in canonical order, so the order a policy
// lists the kinds in never changes a recipe.
func (p ShouldApplyParams) applyEvidence() []string {
	var out []string
	for _, k := range applyEvidenceKinds {
		if slices.Contains(p.ApplyEvidence, k) {
			out = append(out, k)
		}
	}
	return out
}

// PolicyParams are the decide policy as config params (the keys of
// config.DecidePolicyParams), for display.
func (p ShouldApplyParams) PolicyParams() map[string]any {
	return map[string]any{
		config.ParamApplyP:                  p.ApplyP,
		config.ParamRejectP:                 p.RejectP,
		config.ParamApplyEvidence:           p.applyEvidence(),
		config.ParamRejectRequiresLowercase: p.RejectRequiresLowercase,
		config.ParamRejectFloor:             p.RejectFloor,
	}
}

// Describe is the policy in one line: "apply p ≥ 0.85 with asin_verbatim or
// exact_repeat · reject p ≤ 0.20". Thresholds print exactly (at least two
// decimals, more when the value has them: 0.855 is never shown as 0.85), so
// the line a reviewer gates a run on is the policy the recipe holds.
func (p ShouldApplyParams) Describe() string {
	s := fmt.Sprintf("apply p ≥ %s with %s · reject p ≤ %s", fmtP(p.ApplyP), strings.Join(p.applyEvidence(), " or "), fmtP(p.RejectP))
	if p.RejectRequiresLowercase {
		if p.RejectFloor > 0 {
			s += fmt.Sprintf(" if the replacement is lowercase or p < %s", fmtP(p.RejectFloor))
		} else {
			s += " if the replacement is lowercase"
		}
	}
	return s
}

// fmtP formats a threshold exactly, with at least two decimals.
func fmtP(v float64) string {
	exact := strconv.FormatFloat(v, 'f', -1, 64)
	if _, dec, _ := strings.Cut(exact, "."); len(dec) >= 2 {
		return exact
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// recipeParams is every knob that changes an outcome, as recorded in the
// recipe.
func (p ShouldApplyParams) recipeParams() map[string]any {
	return map[string]any{
		"apply_p":                   p.ApplyP,
		"reject_p":                  p.RejectP,
		"apply_evidence":            p.applyEvidence(),
		"reject_requires_lowercase": p.RejectRequiresLowercase,
		"reject_floor":              p.RejectFloor,
		"phonetic_min_sim":          p.PhoneticMinSim,
		"evidence_rule":             EvidenceRule,
		"rung0_version":             Rung0Version,
		"context_words":             ContextWords,
		"neighbour_words":           NeighbourWords,
		"fallback_words":            FallbackWords,
		"max_relevant_sentences":    MaxRelevantSentences,
		"max_relevant_runes":        MaxRelevantRunes,
		"max_sentence_runes":        MaxSentenceRunes,
		"clip_words":                ClipWords,
	}
}

// Model is the decision model should_apply asks: the model id requested
// (Alias), the model expected to answer ("" = Alias) and its revision pin.
// They are the decide recipe's model fields, so another model — e.g. a
// runtime model setting (CONTRACT §2.20) choosing another System One
// model — is another recipe. The zero value is DefaultModel.
type Model struct {
	Alias    string
	Expected string
	Revision string
}

// DefaultModel is should_apply's built-in model, ShouldApplyModel, unpinned.
func DefaultModel() Model { return Model{Alias: ShouldApplyModel} }

func (m Model) orDefault() Model {
	if m.Alias == "" {
		return DefaultModel()
	}
	return m
}

// ModelFromConfig is the decide model cfg configures: the model the
// AI_ROLES.decide endpoint requests, with the decide pin's expected model and
// revision (MODELS_FILE, or a runtime setting's pin). ok=false when the role
// is unbound.
func ModelFromConfig(cfg *config.Config) (Model, bool) {
	ep, ok := cfg.DecideEndpoint()
	if !ok {
		return Model{}, false
	}
	pin := cfg.ModelPin(recipe.StepDecide)
	alias := ep.Model
	if genai.SameModel(alias, ShouldApplyModel) {
		// The built-in model, perhaps behind a provider prefix
		// ("typesafe/jev-1.13.0"): ask for it verbatim, as before runtime
		// settings, so the recipe and the request are unchanged.
		alias = ShouldApplyModel
	}
	return Model{Alias: alias, Expected: pin.ExpectedModel, Revision: pin.Revision}, true
}

// ShouldApplyFn returns the should_apply function for params, asking the
// default model.
func ShouldApplyFn(p ShouldApplyParams) (fn.Fn, error) {
	return ShouldApplyFnFor(p, DefaultModel())
}

// ShouldApplyFnFor returns the should_apply function for params, asking m.
func ShouldApplyFnFor(p ShouldApplyParams, m Model) (fn.Fn, error) {
	if err := p.Validate(); err != nil {
		return fn.Fn{}, err
	}
	m = m.orDefault()
	return fn.New(fn.Fn{
		Name:          ShouldApplyName,
		Step:          recipe.StepDecide,
		PromptVersion: ShouldApplyPromptVersion,
		PromptSHA256:  ShouldApplyPromptSHA256,
		ModelAlias:    m.Alias,
		ExpectedModel: m.Expected,
		Revision:      m.Revision,
		StepVersion:   ShouldApplyStepVersion,
		Params:        p.recipeParams(),
	})
}

// cappedTypes may never be applied by machine (D2): an inserted word and a
// number's written form cannot be checked against the audio by any rung, so
// their best outcome is hold.
var cappedTypes = map[string]bool{IssueDroppedWord: true, IssueNumberArtifact: true}

// Decide maps the model's probability, the text evidence, the issue type and
// the proposed replacement to a decision and its reason:
//
//	p NaN or outside [0, 1]                    → hold   jev_unavailable
//	p ≥ apply_p, capped issue type             → hold   issue_type_capped
//	p ≥ apply_p, evidence in apply_evidence    → apply  confident_with_evidence
//	p ≥ apply_p, evidence not in apply_evidence → hold  evidence_excluded
//	p ≥ apply_p, no evidence                   → hold   no_evidence
//	reject_p < p < apply_p                     → hold   uncertain
//	p ≤ reject_p, reject_requires_lowercase,
//	  replacement has an uppercase letter,
//	  p ≥ reject_floor                         → hold   capitalised_replacement
//	p ≤ reject_p                               → reject jev_reject
func (p ShouldApplyParams) Decide(prob float64, evidence, issueType, replacement string) (decision, reason string) {
	switch {
	case math.IsNaN(prob) || prob < 0 || prob > 1:
		return DecisionHold, ReasonJevUnavailable
	case prob >= p.ApplyP:
		switch {
		case cappedTypes[issueType]:
			return DecisionHold, ReasonTypeCapped
		case evidence != EvidenceASINVerbatim && evidence != EvidenceExactRepeat:
			return DecisionHold, ReasonNoEvidence
		case !slices.Contains(p.ApplyEvidence, evidence):
			return DecisionHold, ReasonEvidenceExcluded
		default:
			return DecisionApply, ReasonConfident
		}
	case prob <= p.RejectP:
		if p.RejectRequiresLowercase && hasUpper(replacement) && prob >= p.RejectFloor {
			return DecisionHold, ReasonCapitalised
		}
		return DecisionReject, ReasonJevReject
	default:
		return DecisionHold, ReasonUncertain
	}
}

// hasUpper reports whether s has an uppercase letter.
func hasUpper(s string) bool {
	return strings.IndexFunc(s, unicode.IsUpper) >= 0
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
	// Decision is apply, hold, reject or duplicate.
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
	// Related is the other finding the decision is about: a duplicate's
	// representative, or the winner an overlap_dup reject lost to ("" = none).
	Related string
	// Rung0 is the rung-0 verdict the outcome rests on.
	Rung0 Verdict
	// ChunkHash is the chunk revision decided on (the finding's
	// chunk_text_sha256, verified by rung 0).
	ChunkHash string
	// FnCallID is the fn_calls row of the model call (0 when not asked).
	FnCallID int64
	// CallRecipeID is the recipe the fn_calls row was stamped with — the
	// answering model's ("" when not asked or no reply). The DECISION's
	// recipe, the one a decision is attributed to (decided_by
	// "jev:<recipe_id>"), is Evaluator.Recipe for every outcome, including
	// rung-0 rejects and holds that made no call.
	CallRecipeID string
	// Model is the model that answered ("" when none did).
	Model    string
	CacheHit bool
	// Asked is true when the model was asked (rung 0 passed), whether or not
	// it answered usably.
	Asked bool
	// ErrorClass is the bounded failure class of a jev_unavailable hold
	// (genai.ErrorClass, "model_fallback" or "invalid_reply"); "" otherwise.
	ErrorClass string
	// Latency, tokens and cost of the call. A cache hit makes no request:
	// its tokens and cost are zero.
	Latency      time.Duration
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

// Evaluator runs should_apply.
type Evaluator struct {
	fn     fn.Fn
	params ShouldApplyParams
	store  fn.Store
	asker  Asker
}

// NewEvaluator builds an Evaluator asking the default model. store logs and
// caches the model calls (*db.DB); asker makes them (*systemone.Client).
func NewEvaluator(store fn.Store, asker Asker, p ShouldApplyParams) (*Evaluator, error) {
	return NewEvaluatorFor(store, asker, p, DefaultModel())
}

// NewEvaluatorFor builds an Evaluator asking model m (zero = DefaultModel).
func NewEvaluatorFor(store fn.Store, asker Asker, p ShouldApplyParams, m Model) (*Evaluator, error) {
	if store == nil || asker == nil {
		return nil, errors.New("decide: evaluator needs a store and a System One client")
	}
	f, err := ShouldApplyFnFor(p, m)
	if err != nil {
		return nil, err
	}
	return &Evaluator{fn: f, params: p, store: store, asker: asker}, nil
}

// Recipe is the decide recipe every outcome of this Evaluator is attributed
// to: should_apply answered by its pinned model, with this Evaluator's params.
// Register it (db.RegisterRecipe) before recording decisions.
func (e *Evaluator) Recipe() recipe.Recipe { return e.fn.Recipe("") }

// Model is the model id this Evaluator asks.
func (e *Evaluator) Model() string { return e.fn.ModelAlias }

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

	unavailable := func(class string) Outcome {
		out.Decision, out.Reason, out.Retryable, out.P = DecisionHold, ReasonJevUnavailable, true, nil
		out.ErrorClass = class
		return out
	}
	out.Asked = true
	res, meta, err := e.fn.Invoke(ctx, e.store, shouldApplyInput{State: state}, e.call(state))
	out.FnCallID, out.CallRecipeID, out.CacheHit = meta.CallID, meta.RecipeID, meta.CacheHit
	out.Model, out.Latency = res.Model, meta.Latency
	if res.InputTokens != nil {
		out.InputTokens = *res.InputTokens
	}
	if res.OutputTokens != nil {
		out.OutputTokens = *res.OutputTokens
	}
	if res.CostUSD != nil {
		out.CostUSD = *res.CostUSD
	}
	switch {
	case err != nil:
		return unavailable(genai.ErrorClass(err))
	case meta.Fallback:
		return unavailable(db.ErrorClassModelFallback)
	}
	p, err := decodeShouldApply(res.Output)
	if err != nil {
		return unavailable(genai.ErrorClass(err))
	}
	out.P = &p
	out.Decision, out.Reason = e.params.Decide(p, out.Evidence, c.IssueType, c.Replacement)
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
			Model:     e.fn.ModelAlias,
			State:     state,
			Questions: map[string]systemone.Question{questionKey: shouldApplyQuestion},
		})
		if err != nil {
			return fn.Result{}, err
		}
		if resp == nil {
			return fn.Result{}, &systemone.DecodeError{Reason: "empty reply"}
		}
		if _, ok := resp.Answers[questionKey]; !ok {
			// LiteLLM's unified /v1/systemone drops a refused answer from
			// "answers". An error, not a result: never cached, so a later
			// run asks again; Evaluate turns it into a retryable hold.
			return fn.Result{}, &systemone.DecodeError{Reason: "should_apply has no answer (refused)"}
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
			text = emptyBlock
		}
		b.WriteString(label)
		b.WriteString(":\n")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	block(labelIssueType, issueType)
	block(labelOriginal, c.Original)
	block(labelProposed, c.Corrected)
	block(labelBefore, c.Before)
	block(labelAfter, c.After)
	var lines []string
	for _, s := range record {
		lines = append(lines, linePrefix+s.Field+": "+s.Text)
	}
	block(labelReference, strings.Join(lines, "\n"))
	return strings.TrimRight(b.String(), "\n") + "\n"
}
