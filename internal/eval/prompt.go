package eval

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/jedwards1230/earmark/internal/db"
)

// Issue-type vocabulary (closed set the prompt advertises). An unknown type from
// the model is coerced to issueOther so the column stays enumerable.
//
// Taxonomy rev 2 (2026-06): dropped run_on — it over-fired on normal long
// sentences (a ground-truth audit found ~half of run_on findings were correct
// text). Added misheard_word (a non-proper-noun mis-recognition; previously
// dumped into "other") and repeated_text (literal word/phrase duplication —
// the genuine, detectable subset of the old run_on). "other" is retained ONLY
// as the coercion sink for an unknown model value; the prompt instructs the
// judge never to choose it.
const (
	issueMisheardProperNoun = "misheard_proper_noun"
	issueMisheardWord       = "misheard_word"
	issueRepeatedText       = "repeated_text"
	issueNumberArtifact     = "number_artifact"
	issueHomophone          = "homophone"
	issueDroppedWord        = "dropped_word"
	issueOther              = "other"
)

// knownIssueTypes is the closed issue-type set. parseFindings coerces anything
// else to issueOther.
var knownIssueTypes = map[string]bool{
	issueMisheardProperNoun: true,
	issueMisheardWord:       true,
	issueRepeatedText:       true,
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
transcript produced by an automatic speech recognizer (ASR). Your job is to flag
spans where the ASR almost certainly MISHEARD the spoken audio.

You are ADVISORY ONLY: never rewrite the transcript.

Default to flagging NOTHING. The large majority of spans contain no ASR error.
Only flag a span when you are reasonably confident the words shown are not what
was actually spoken AND you can give the correct words. When in doubt, do not
flag — a missed error is fine; a false alarm is not.

Respond with STRICT JSON only (no prose, no markdown fences) in this shape:
{
  "findings": [
    {
      "original_text": "<verbatim span copied from the input>",
      "issue_type": "<one of the types below>",
      "suggested_correction": "<the corrected words — REQUIRED, never empty>",
      "confidence": <number 0.0-1.0>
    }
  ]
}

issue_type — pick the single best fit:
- misheard_proper_noun: a NAME, place, brand, or title the ASR garbled
  (e.g. "auto sebo" → "Arecibo", "Holovo" → "Holevo").
- misheard_word: an ordinary (non-name) word or phrase the ASR got wrong, or
  two words wrongly fused/split (e.g. "Placenes" → "place names",
  "thegreycourses" → "thegreatcourses", "Limpel Ziv" → "Lempel-Ziv").
- repeated_text: a word or short phrase ACCIDENTALLY DUPLICATED, with the
  duplication visible in the span (e.g. "the the cat" → "the cat",
  "can be can be" → "can be"). Do NOT use this for a long or complex sentence
  that merely sounds awkward — only for a literal stutter/duplication.
- number_artifact: a number, date, or unit that came out WRONG
  (e.g. "too forty" → "240"). Must include the corrected value.
- homophone: a real word swapped for its sound-alike (e.g. "pin name" →
  "pen name", "their" → "there").
- dropped_word: a word clearly omitted, leaving the sentence broken.

Do NOT use "other". If a suspected error does not fit a type above, do not flag it.

The transcript is intentionally ALL LOWERCASE with NO punctuation. That is
normal and correct — it is NOT an error. Your correction must change the actual
WORDS; if your "fix" only adds capital letters, punctuation, or hyphens, do not
emit it.

NEVER flag (these are NOT errors):
- Missing capitalization ("french" → "French", "von neumann") — the transcript
  is lowercase by design. Never flag a span just to capitalize it.
- Missing or different punctuation (adding commas, periods, colons, apostrophes,
  quotation marks).
- Numerals vs spelled-out numbers ("10 or 12" is fine — do not "correct" it to
  "ten or twelve", and vice versa). Only flag a number that is actually WRONG.
- Hyphenation or spacing style ("tic-tac-toe" vs "tic tac toe", "twenty-six").
- A sentence that is grammatically correct, even if long, listy, or awkward.
- Deliberate enumerations or read-aloud lists of words.
- A valid word choice you would have phrased differently ("addressed" vs "sent").
- Any span where you cannot supply a concrete correction.

The test for every finding: read your suggested_correction out loud. If it sounds
the SAME as the original (you only changed case, punctuation, or formatting), it
is not an error — drop it. Only flag when a DIFFERENT word was written than was
spoken.

Every finding MUST include a non-empty suggested_correction. If you cannot
propose the corrected words, do not emit the finding.

Confidence: use ≥0.8 only when the error is obvious and your correction is almost
certainly right; 0.6-0.8 when it looks wrong but the correction is a guess; below
0.6 means you are unsure — and if you are unsure, prefer not to flag at all. An
empty findings array is the correct, expected answer for a clean span.
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
		// A finding with no proposed correction is the least actionable and, in
		// practice, the dominant noise source (a "this looks off" with no fix).
		// The prompt requires a correction; enforce it structurally so a model
		// that ignores the instruction can't reintroduce that noise class.
		correction := strings.TrimSpace(f.SuggestedCorrection)
		if correction == "" {
			continue
		}
		// The ASR transcript is all-lowercase and unpunctuated by design, so the
		// judge tends to "correct" spans purely to add capitalization or
		// punctuation (or to hyphenate / restyle). Those are not transcription
		// errors. Drop any finding whose correction is identical to the original
		// once case and punctuation are normalized away — a genuine word change
		// (substitution, split/merge, duplication) survives this comparison; a
		// restyling does not. The prompt also forbids this, but the model ignores
		// it often enough that a structural guard is warranted.
		if normalizeForCompare(text) == normalizeForCompare(correction) {
			continue
		}
		issue := strings.TrimSpace(strings.ToLower(f.IssueType))
		if !knownIssueTypes[issue] {
			issue = issueOther
		}
		out = append(out, parsedFinding{
			OriginalText:        text,
			IssueType:           issue,
			SuggestedCorrection: correction,
			Confidence:          clampConfidence(f.Confidence),
		})
	}
	return out, nil
}

// normalizeForCompare folds a span for a "is this a real word change?" test:
// lowercase, every non-alphanumeric rune becomes a single space, runs of space
// collapse, and the result is trimmed. Two spans that differ only in
// capitalization, punctuation, or hyphenation normalize to the same string; a
// different WORD (substitution, split, merge, or duplication) does not — word
// boundaries (spaces) are preserved, so "logo graphic" ≠ "logographic" but
// "the French" == "the french" and "twenty-six" == "twenty six". Used to drop
// findings whose "correction" only restyles text the ASR already got right.
func normalizeForCompare(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true // leading-space suppression
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevSpace = false
		case !prevSpace:
			b.WriteByte(' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(b.String())
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
