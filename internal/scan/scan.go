// Package scan is the chunk quality scan (CONTRACT §1.9 "Chunk scan"): one
// System One call per chunk asks six questions about its pristine text — does
// it need a fix, how good is it (1..5), is it boilerplate, garbled, dialogue,
// and which issue type dominates — through the pure-function wrapper
// (internal/fn, fn "scan_chunk"), so every answer is logged and cached in
// fn_calls.
//
// The scan is read-only over transcripts: it judges, it never edits. Results
// land in chunk_scan (db.InsertChunkScan) only when the caller asks for it.
//
// Answer mapping is strict: a reply (fresh or cached) that is missing any of
// the six answers, answers with the wrong type, or lacks a field the mapping
// needs is an error, and no chunk_scan row is written for it.
package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/fn"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/systemone"
)

// Function identity (fn_calls.fn, the recipe's prompt version and step).
const (
	FnName        = "scan_chunk"
	PromptVersion = "scan_chunk@v1"
	// StepVersion is bumped when earmark's logic for the scan changes its
	// output (state layout, answer mapping).
	StepVersion = 1
	// DefaultModel is the System One model the scan is pinned to. The
	// AI_ROLES.scan endpoint's model is what is asked for; it must be a
	// pinned version (config.PinnedModel).
	DefaultModel = "jev-1.13.0"
)

// Question names (System One answer keys).
const (
	QNeedsFix    = "needs_fix"
	QQuality     = "quality"
	QBoilerplate = "boilerplate"
	QGarbled     = "garbled"
	QDialogue    = "dialogue"
	QIssueType   = "issue_type"
)

// IssueNone is the issue_type answer for a chunk with no issue.
const IssueNone = "none"

// IssueTypes is the issue_type label set, in a fixed order: the judge's six
// issue types (internal/eval) plus IssueNone.
var IssueTypes = []string{
	"misheard_proper_noun", "misheard_word", "repeated_text",
	"number_artifact", "homophone", "dropped_word", IssueNone,
}

// qualityLevels describe the five quality levels, worst first: score s maps
// to quality s+1.
var qualityLevels = []string{
	"1 — unusable: mostly wrong or unintelligible words; the meaning cannot be recovered",
	"2 — poor: frequent misrecognitions; the meaning is often unclear",
	"3 — fair: readable, with several clear misrecognitions",
	"4 — good: reads naturally, with at most one or two small slips",
	"5 — clean: reads exactly like correct, natural English prose (ignoring case and punctuation)",
}

// textNote reminds the model of the ASR conventions so they are not judged as
// errors. Every instruction ends with the scope rule.
const textNote = " The text is automatic speech recognition (ASR) output of an audiobook: " +
	"all lowercase with no punctuation by design, so never count missing capitals or " +
	"punctuation as errors. Judge ONLY the section marked TEXT TO JUDGE; the CONTEXT " +
	"sections are neighbouring audio shown for orientation and must not be judged."

// Questions returns the six questions, built fresh each call (callers may not
// mutate a shared map).
func Questions() map[string]systemone.Question {
	issueLabels := map[string]string{
		"misheard_proper_noun": "a name, place or invented word was transcribed as different words",
		"misheard_word":        "an ordinary word was transcribed as a different, wrong word",
		"repeated_text":        "a word or phrase is duplicated verbatim where the speaker said it once",
		"number_artifact":      "a number, date or quantity was rendered wrongly or inconsistently",
		"homophone":            "a word was replaced by one that sounds the same but means something else",
		"dropped_word":         "a word the sentence needs is missing",
		IssueNone:              "no transcription error",
	}
	return map[string]systemone.Question{
		QNeedsFix: {
			Type:         systemone.TypeNoul,
			Instructions: "Does the text contain at least one transcription error a careful editor listening to the audio would correct?" + textNote,
			Criteria: map[string]string{
				"true":  "at least one word is almost certainly not what was spoken",
				"false": "every word is plausibly what was spoken",
			},
		},
		QQuality: {
			Type:         systemone.TypeScore,
			Instructions: "Rate the transcription quality of the text." + textNote,
			Criteria:     append([]string(nil), qualityLevels...),
		},
		QBoilerplate: {
			Type:         systemone.TypeNoul,
			Instructions: "Is the text audiobook boilerplate rather than the book's content — opening or closing credits, copyright notices, publisher or narrator announcements, chapter-number announcements alone, or advertising?" + textNote,
			Criteria: map[string]string{
				"true":  "the text is boilerplate",
				"false": "the text is the book's own content",
			},
		},
		QGarbled: {
			Type:         systemone.TypeNoul,
			Instructions: "Is the text garbled — word salad, long runs of nonsense, or a loop of repeated fragments — rather than coherent language?" + textNote,
			Criteria: map[string]string{
				"true":  "the text is garbled",
				"false": "the text is coherent",
			},
		},
		QDialogue: {
			Type:         systemone.TypeNoul,
			Instructions: "Is the text mostly spoken dialogue between characters (rather than narration)?" + textNote,
			Criteria: map[string]string{
				"true":  "mostly dialogue",
				"false": "mostly narration",
			},
		},
		QIssueType: {
			Type:         systemone.TypeChoice,
			Instructions: "Which kind of transcription error is most prominent in the text? Choose none when there is no error." + textNote,
			Criteria:     issueLabels,
		},
	}
}

// stateTemplate lays out the state string. It is part of the prompt hash, so
// a layout change is a new recipe.
const stateTemplate = "{{context_before}}=== TEXT TO JUDGE ===\n{{text}}\n=== END TEXT TO JUDGE ===\n{{context_after}}"

const (
	beforeHeader = "=== CONTEXT BEFORE (context only — do not judge) ===\n"
	afterHeader  = "=== CONTEXT AFTER (context only — do not judge) ===\n"
	contextEnd   = "=== END CONTEXT ===\n"
)

// PromptSHA256 hashes everything that shapes the request besides the input:
// the canonical JSON of the questions and the state layout.
func PromptSHA256() string {
	q, err := json.Marshal(Questions())
	if err != nil {
		panic(fmt.Sprintf("scan: questions do not marshal: %v", err)) // static data
	}
	canon, _, err := fn.CanonicalInput(json.RawMessage(q))
	if err != nil {
		panic(fmt.Sprintf("scan: questions do not canonicalize: %v", err))
	}
	return recipe.PromptSHA256(string(canon), stateTemplate, beforeHeader, afterHeader, contextEnd)
}

// Input is the function input: the chunk's pristine text and its context.
// Its canonical JSON is the fn_calls cache key, so the same text with the
// same neighbours is asked once.
type Input struct {
	Text          string   `json:"text"`
	ContextBefore []string `json:"context_before"`
	ContextAfter  []string `json:"context_after"`
}

// InputFor builds the input for a scan candidate.
func InputFor(c db.ScanCandidate) Input {
	return Input{
		Text:          c.Text,
		ContextBefore: nonNil(c.ContextBefore),
		ContextAfter:  nonNil(c.ContextAfter),
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// State renders in as the System One state string.
func State(in Input) string {
	section := func(header string, segs []string) string {
		if len(segs) == 0 {
			return ""
		}
		return header + strings.Join(segs, "\n") + "\n" + contextEnd
	}
	r := strings.NewReplacer(
		"{{context_before}}", section(beforeHeader, in.ContextBefore),
		"{{text}}", in.Text,
		"{{context_after}}", section(afterHeader, in.ContextAfter),
	)
	return r.Replace(stateTemplate)
}

// Result is one chunk's mapped answers.
type Result struct {
	PNeedsFix         float64
	Quality           float64  // 1..5 (score + 1)
	QualityConfidence *float64 // nil when the model reported none
	PBoilerplate      float64
	PGarbled          float64
	PDialogue         float64
	IssueType         string
	IssueProbs        map[string]float64
	// CostUSD is what the call cost (0 when served from fn_calls).
	CostUSD float64
}

// MappingError is a reply that does not answer the six questions.
type MappingError struct{ Reason string }

func (e *MappingError) Error() string      { return "scan: unusable answers: " + e.Reason }
func (e *MappingError) ErrorClass() string { return "invalid_reply" }

// output is the fn_calls output document of a scan call.
type output struct {
	Answers map[string]systemone.Answer `json:"answers"`
}

// Decode maps a stored scan output (fresh or served from cache).
func Decode(raw json.RawMessage) (Result, error) {
	var o output
	if err := json.Unmarshal(raw, &o); err != nil {
		return Result{}, &MappingError{Reason: "output is not a scan answer document"}
	}
	return MapAnswers(o.Answers)
}

// MapAnswers maps the six answers strictly: every question must be answered,
// with its type and the field the mapping reads.
func MapAnswers(a map[string]systemone.Answer) (Result, error) {
	noul := func(q string) (float64, error) {
		ans, ok := a[q]
		switch {
		case !ok:
			return 0, &MappingError{Reason: fmt.Sprintf("%s has no answer", q)}
		case ans.Type != systemone.TypeNoul:
			return 0, &MappingError{Reason: fmt.Sprintf("%s answered as %q, want noul", q, ans.Type)}
		case ans.Noul == nil || !unit(*ans.Noul):
			return 0, &MappingError{Reason: fmt.Sprintf("%s has no probability in [0,1]", q)}
		}
		return *ans.Noul, nil
	}
	var r Result
	var err error
	if r.PNeedsFix, err = noul(QNeedsFix); err != nil {
		return Result{}, err
	}
	if r.PBoilerplate, err = noul(QBoilerplate); err != nil {
		return Result{}, err
	}
	if r.PGarbled, err = noul(QGarbled); err != nil {
		return Result{}, err
	}
	if r.PDialogue, err = noul(QDialogue); err != nil {
		return Result{}, err
	}

	q, ok := a[QQuality]
	switch {
	case !ok:
		return Result{}, &MappingError{Reason: "quality has no answer"}
	case q.Type != systemone.TypeScore:
		return Result{}, &MappingError{Reason: fmt.Sprintf("quality answered as %q, want score", q.Type)}
	case q.Score == nil || math.IsNaN(*q.Score) || *q.Score < 0 || *q.Score > float64(len(qualityLevels)-1):
		return Result{}, &MappingError{Reason: "quality has no score in 0..4"}
	case q.Confidence != nil && !unit(*q.Confidence):
		return Result{}, &MappingError{Reason: "quality confidence outside [0,1]"}
	}
	r.Quality = *q.Score + 1
	if q.Confidence != nil {
		c := *q.Confidence
		r.QualityConfidence = &c
	}

	it, ok := a[QIssueType]
	switch {
	case !ok:
		return Result{}, &MappingError{Reason: "issue_type has no answer"}
	case it.Type != systemone.TypeChoice:
		return Result{}, &MappingError{Reason: fmt.Sprintf("issue_type answered as %q, want choice", it.Type)}
	case !knownIssue(it.Choice):
		return Result{}, &MappingError{Reason: "issue_type chose an unknown label"}
	case len(it.Probabilities) == 0:
		return Result{}, &MappingError{Reason: "issue_type has no probabilities"}
	}
	r.IssueProbs = make(map[string]float64, len(it.Probabilities))
	for k, p := range it.Probabilities {
		if !knownIssue(k) || !unit(p) {
			return Result{}, &MappingError{Reason: "issue_type probabilities name an unknown label or leave [0,1]"}
		}
		r.IssueProbs[k] = p
	}
	r.IssueType = it.Choice
	return r, nil
}

func knownIssue(s string) bool {
	for _, t := range IssueTypes {
		if s == t {
			return true
		}
	}
	return false
}

func unit(f float64) bool { return !math.IsNaN(f) && f >= 0 && f <= 1 }

// Row is r as the chunk_scan row for candidate c, made by the call meta.
func (r Result) Row(c db.ScanCandidate, meta fn.Meta) db.ChunkScan {
	return db.ChunkScan{
		TranscriptID:      c.TranscriptID,
		ChunkIndex:        c.ChunkIndex,
		ChunkTextSHA256:   c.TextSHA256,
		RecipeID:          meta.RecipeID,
		FnCallID:          meta.CallID,
		PNeedsFix:         r.PNeedsFix,
		Quality:           r.Quality,
		QualityConfidence: r.QualityConfidence,
		PBoilerplate:      r.PBoilerplate,
		PGarbled:          r.PGarbled,
		PDialogue:         r.PDialogue,
		IssueType:         r.IssueType,
		IssueProbs:        r.IssueProbs,
	}
}

// NewFn is the scan_chunk function for the given pinned model. expected and
// revision come from the model registry's scan pin ("" = unpinned).
// contextSegments is recorded in the recipe: a different neighbour count is a
// different input, and so a different recipe.
func NewFn(model, expected, revision string, contextSegments int) (fn.Fn, error) {
	return fn.New(fn.Fn{
		Name:          FnName,
		Step:          recipe.StepScan,
		StepVersion:   StepVersion,
		PromptVersion: PromptVersion,
		PromptSHA256:  PromptSHA256(),
		ModelAlias:    model,
		ExpectedModel: expected,
		Revision:      revision,
		Params:        map[string]any{"context_segments": contextSegments},
	})
}

// FnFromConfig builds the scan function and client from AI_ROLES.scan and the
// MODELS_FILE scan pin. ok=false when no scan endpoint is bound.
func FnFromConfig(cfg *config.Config, contextSegments int) (f fn.Fn, client *systemone.Client, ok bool, err error) {
	ep, ok := cfg.ScanEndpoint()
	if !ok {
		return fn.Fn{}, nil, false, nil
	}
	pin := cfg.ModelPin(recipe.StepScan)
	f, err = NewFn(ep.Model, pin.ExpectedModel, pin.Revision, contextSegments)
	if err != nil {
		return fn.Fn{}, nil, true, err
	}
	var opts []systemone.Option
	if p, set := pin.FloatParam(config.ParamUSDPerMTokIn); set {
		opts = append(opts, systemone.WithUSDPerMTokIn(p))
	}
	client, err = systemone.New(ep.BaseURL, ep.APIKey, opts...)
	if err != nil {
		return fn.Fn{}, nil, true, err
	}
	return f, client, true, nil
}

// CurrentRecipe is the scan recipe this configuration would stamp (the
// expected model answering), for current_recipes. ok=false when no scan
// endpoint is bound.
func CurrentRecipe(cfg *config.Config) (recipe.Recipe, bool, error) {
	f, _, ok, err := FnFromConfig(cfg, db.DefaultScanContextSegments)
	if !ok || err != nil {
		return recipe.Recipe{}, ok, err
	}
	return f.Recipe(""), true, nil
}

// Decider is the System One call (*systemone.Client implements it).
type Decider interface {
	Decide(ctx context.Context, req systemone.Request) (*systemone.Response, error)
}

// Scanner scans chunks through one function, client and store.
type Scanner struct {
	Fn     fn.Fn
	Client Decider
	Store  fn.Store
}

// Scan asks the six questions about c (or serves them from fn_calls) and maps
// the answers. The returned meta says whether the call was cached or answered
// by a fallback model; a fallback result is returned but callers must not
// store it as the scan recipe's answer (it carries another model's recipe).
func (s Scanner) Scan(ctx context.Context, c db.ScanCandidate) (Result, fn.Meta, error) {
	if s.Client == nil || s.Store == nil {
		return Result{}, fn.Meta{}, errors.New("scan: scanner needs a client and a store")
	}
	in := InputFor(c)
	res, meta, err := s.Fn.Invoke(ctx, s.Store, in, func(ctx context.Context) (fn.Result, error) {
		resp, err := s.Client.Decide(ctx, systemone.Request{
			Model: s.Fn.ModelAlias, State: State(in), Questions: Questions(),
		})
		if err != nil {
			return fn.Result{}, err
		}
		// Map before the reply is logged as a success: an answer set the
		// mapping cannot use is an error row (never cached), not a cache
		// entry that would fail the same way on every later run.
		if _, err := MapAnswers(resp.Answers); err != nil {
			return fn.Result{}, err
		}
		out, err := json.Marshal(output{Answers: resp.Answers})
		if err != nil {
			return fn.Result{}, fmt.Errorf("scan: encode output: %w", err)
		}
		in, outTok, cost := resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.CostUSD
		return fn.Result{Output: out, Model: resp.Model, InputTokens: &in, OutputTokens: &outTok, CostUSD: &cost}, nil
	})
	if err != nil {
		return Result{}, meta, err
	}
	r, err := Decode(res.Output)
	if err == nil && !meta.CacheHit && res.CostUSD != nil {
		r.CostUSD = *res.CostUSD
	}
	return r, meta, err
}
