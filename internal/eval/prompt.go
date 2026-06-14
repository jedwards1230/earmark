package eval

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jedwards1230/earmark/internal/db"
)

// Issue-type vocabulary (closed set the prompt advertises). An unknown type from
// the model is coerced to issueOther so the column stays enumerable.
const (
	issueMisheardProperNoun = "misheard_proper_noun"
	issueRunOn              = "run_on"
	issueNumberArtifact     = "number_artifact"
	issueHomophone          = "homophone"
	issueDroppedWord        = "dropped_word"
	issueOther              = "other"
)

// knownIssueTypes is the closed issue-type set. parseFindings coerces anything
// else to issueOther.
var knownIssueTypes = map[string]bool{
	issueMisheardProperNoun: true,
	issueRunOn:              true,
	issueNumberArtifact:     true,
	issueHomophone:          true,
	issueDroppedWord:        true,
	issueOther:              true,
}

// systemPrompt instructs the judge: advisory-only, conservative, JSON-out. It is
// a package var (not const) only so a test could swap it; it never changes at
// runtime.
var systemPrompt = strings.TrimSpace(`
You are a transcription QA reviewer. You are given a span of an audiobook
transcript produced by an automatic speech recognizer (ASR). Identify SUSPECTED
transcription errors in the span.

You are ADVISORY ONLY: never rewrite the transcript. Only flag spans you suspect
are wrong. Be conservative — prefer missing a subtle error to flagging text that
is probably correct.

Respond with STRICT JSON only (no prose, no markdown fences) in this shape:
{
  "findings": [
    {
      "original_text": "<verbatim span copied from the input>",
      "issue_type": "<one of: misheard_proper_noun | run_on | number_artifact | homophone | dropped_word | other>",
      "suggested_correction": "<your advisory correction, or empty string>",
      "confidence": <number 0.0-1.0>
    }
  ]
}

issue_type meanings:
- misheard_proper_noun: a name/place likely mis-recognized
- run_on: a sentence or segment boundary was lost
- number_artifact: digits, dates, or units garbled
- homophone: wrong word with the right sound (e.g. their/there)
- dropped_word: a likely omission
- other: any other suspected error

Confidence calibration: use >0.8 only for an obvious error; 0.4-0.7 for "looks
off but could be correct"; <0.4 for a weak hunch. An empty findings array is a
valid and expected answer for a clean span.
`)

// buildPrompt returns the (system, user) prompt pair for one chunk. The user
// message carries the book/track path (light context) and the verbatim text.
func buildPrompt(c db.EvalChunk) (system, user string) {
	var b strings.Builder
	fmt.Fprintf(&b, "Book/track: %s\n", c.FilePath)
	fmt.Fprintf(&b, "Span time: %.1fs–%.1fs\n\n", c.StartSec, c.EndSec)
	b.WriteString("Transcript span:\n")
	b.WriteString(c.Text)
	return systemPrompt, b.String()
}

// rawFinding is the wire shape of one finding in the judge's JSON response.
type rawFinding struct {
	OriginalText        string  `json:"original_text"`
	IssueType           string  `json:"issue_type"`
	SuggestedCorrection string  `json:"suggested_correction"`
	Confidence          float64 `json:"confidence"`
}

// judgeResponse is the top-level JSON object the judge returns.
type judgeResponse struct {
	Findings []rawFinding `json:"findings"`
}

// parsedFinding is a validated, normalized finding ready to become a db.Finding.
type parsedFinding struct {
	OriginalText        string
	IssueType           string
	SuggestedCorrection string
	Confidence          float64
}

// maxJudgeResponseBytes caps the judge response we will attempt to parse. A
// misbehaving or hostile endpoint returning a huge body could otherwise drive
// an unbounded allocation in json.Unmarshal. The openAIChatClient already caps
// its HTTP read at 1 MiB; this is a second, transport-independent guard so any
// ChatClient (incl. a future one) can't OOM the judge. 10 MiB is far above any
// legitimate findings array for a single chunk.
const maxJudgeResponseBytes = 10 << 20 // 10 MiB

// parseFindings extracts and normalizes findings from the model's raw text.
// It is defensive: it rejects an oversized response (OOM guard), tolerates
// surrounding prose / markdown fences by scanning for the JSON object, clamps
// confidence to [0,1], coerces an unknown issue_type to "other", and drops
// findings with an empty original_text. A response with no extractable JSON
// object (or an oversized one) returns an error, which the caller treats as no
// findings (soft-fail).
func parseFindings(raw string) ([]parsedFinding, error) {
	if len(raw) > maxJudgeResponseBytes {
		return nil, fmt.Errorf("judge response exceeds size limit (%d > %d bytes)", len(raw), maxJudgeResponseBytes)
	}

	jsonText, ok := extractJSONObject(raw)
	if !ok {
		return nil, fmt.Errorf("no JSON object found in judge response")
	}

	var resp judgeResponse
	if err := json.Unmarshal([]byte(jsonText), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal judge response: %w", err)
	}

	out := make([]parsedFinding, 0, len(resp.Findings))
	for _, f := range resp.Findings {
		text := strings.TrimSpace(f.OriginalText)
		if text == "" {
			continue // a finding with no span is unusable
		}
		issue := strings.TrimSpace(strings.ToLower(f.IssueType))
		if !knownIssueTypes[issue] {
			issue = issueOther
		}
		out = append(out, parsedFinding{
			OriginalText:        text,
			IssueType:           issue,
			SuggestedCorrection: strings.TrimSpace(f.SuggestedCorrection),
			Confidence:          clampConfidence(f.Confidence),
		})
	}
	return out, nil
}

// clampConfidence forces a confidence into [0,1] so a model that emits 1.2 or a
// negative never produces an out-of-range row.
func clampConfidence(c float64) float64 {
	switch {
	case c < 0:
		return 0
	case c > 1:
		return 1
	default:
		return c
	}
}

// extractJSONObject returns the substring from the first '{' to the last '}'
// (inclusive), tolerating a model that wraps its JSON in prose or ```json
// fences. Returns ("", false) when no plausible object is present.
func extractJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < 0 || end < start {
		return "", false
	}
	return s[start : end+1], true
}
