package eval

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
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

// systemPrompt instructs the judge (judge@v2). It is built around what the
// downstream steps can use: decide's rung 0 rejects any substitution whose
// changed words do not SOUND like the originals, and apply needs exact text
// evidence, so a finding that is not a sound-alike mishearing is pure waste.
// v2 replaced v1's long, repetitive list of don'ts with two conditions and
// one short list (1,105 → 501 cl100k tokens; TestSystemPromptTokenBudget). The edit shapes it
// forbids are also enforced after parsing (prefilter), because a prompt alone
// does not hold. It is a package var (not const) only so a test could swap it;
// it never changes at runtime.
var systemPrompt = strings.TrimSpace(`
You check an audiobook transcript made by speech recognition (ASR) for words the ASR MISHEARD. Each finding becomes a patch to the transcript, and a wrong patch corrupts it. Most spans have no error; for those, return {"findings":[]}.

The text is lowercase and unpunctuated by design, and numbers may be words or digits. None of that is an error.

Flag a span only when both hold:
1. A word or name was misheard as a similar-sounding one: read aloud, your correction sounds like the original ("auto sebo" → "arecibo", "pin name" → "pen name"; not "teeth" → "earth"). If it does not, it is a rewrite: skip it. (Repeats and dropped words are the only exceptions.)
2. The context (the sentence; the book/track path for names) shows what was said. Never guess.

Never flag:
- grammar, tense or word-choice fixes ("trades" → "traded")
- added words, except one or two whose absence leaves the sentence broken; never add a subject or article to smooth it ("said dogs" → "he said the dogs")
- case, punctuation, hyphen or spacing changes
- numbers rewritten between words and digits; flag only a misheard value
- unusual words or names that are plausible as written

Fields:
- original_text: only the misheard words, copied exactly (a repeat: every copy; a dropped word: the words around the gap).
- suggested_correction: those words as spoken, lowercase without punctuation, never empty.
- anchor_offset: 0-based character index of original_text within the transcript span text; anchor_occurrence: which identical copy (0 = first).
- confidence: 0.8+ obvious, 0.6-0.8 likely; below that, do not flag.
- issue_type:
  - misheard_proper_noun: a name, place, brand or title
  - misheard_word: any other word or phrase
  - homophone: a same-sounding real word ("their" → "there")
  - number_artifact: a number whose value was misheard ("too forty" → "two forty")
  - repeated_text: an accidental literal repeat ("the the" → "the")
  - dropped_word: one or two missing words; correction = original plus them

Reply with JSON only.
`)

// anchorValue normalizes an optional anchor field to the convention
// internal/patch expects: a non-negative index, or -1 for "unknown".
//
// A missing field and a negative one collapse to the same answer on purpose.
// Both mean the model did not give us a position we can trust, and inventing
// 0 would be worse than admitting ignorance — offset 0 is a real, plausible
// position, so a fabricated 0 would silently patch the start of the span.
func anchorValue(p *int) int {
	if p == nil || *p < 0 {
		return -1
	}
	return *p
}

// userPromptTemplate is the user message wrapped around each chunk's text:
// the book/track path (light context) and the span's time range. The verbatim
// chunk text follows it.
const userPromptTemplate = "Book/track: %s\nSpan time: %.1fs–%.1fs\n\nTranscript span:\n"

// judgePromptVersion names the judge prompt for provenance (CONTRACT §1.9).
// Bump it whenever systemPrompt, userPromptTemplate or the response schema
// changes on purpose; TestJudgePromptVersionPinned fails until you do. (The
// recipe also carries judgePromptSHA256, so even an unversioned edit yields a
// new recipe — the version is the human-readable half.)
const judgePromptVersion = "judge@v2"

// judgePromptSHA256 hashes every prompt part the judge sends: the system
// prompt, the user template, and the JSON schema the reply is pinned to.
// Computed once; none of them change at runtime.
var judgePromptSHA256 = sync.OnceValue(func() string {
	schema, err := json.Marshal(findingsResponseFormat)
	if err != nil {
		// A package-level literal of maps and strings; marshalling cannot fail.
		panic(fmt.Sprintf("marshal findings response format: %v", err))
	}
	return recipe.PromptSHA256(systemPrompt, userPromptTemplate, string(schema))
})

// buildPrompt returns the (system, user) prompt pair for one chunk.
func buildPrompt(c db.EvalChunk) (system, user string) {
	var b strings.Builder
	fmt.Fprintf(&b, userPromptTemplate, c.FilePath, c.StartSec, c.EndSec)
	b.WriteString(c.Text)
	return systemPrompt, b.String()
}

// rawFinding is the wire shape of one finding in the judge's JSON response.
type rawFinding struct {
	OriginalText        string  `json:"original_text"`
	IssueType           string  `json:"issue_type"`
	SuggestedCorrection string  `json:"suggested_correction"`
	Confidence          float64 `json:"confidence"`
	// Anchor fields. Pointers so "absent" is distinguishable from "0" — a model
	// that omits them (or an older response replayed from a log) must fall back
	// to unique-match recovery in internal/patch rather than silently claiming
	// the span is at offset 0, occurrence 0.
	AnchorOffset     *int `json:"anchor_offset"`
	AnchorOccurrence *int `json:"anchor_occurrence"`
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
	// Resolved anchor. Negative means "the model did not tell us", which
	// internal/patch treats as a cue to fall back to unique-match recovery
	// instead of trusting a fabricated position.
	AnchorOffset     int
	AnchorOccurrence int
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
// findings (soft-fail). Every finding it drops is returned with its reason.
func parseFindings(raw string) ([]parsedFinding, []Dropped, error) {
	if len(raw) > maxJudgeResponseBytes {
		return nil, nil, fmt.Errorf("judge response exceeds size limit (%d > %d bytes)", len(raw), maxJudgeResponseBytes)
	}

	jsonText, ok := extractJSONObject(raw)
	if !ok {
		return nil, nil, fmt.Errorf("no JSON object found in judge response")
	}

	var resp judgeResponse
	if err := json.Unmarshal([]byte(jsonText), &resp); err != nil {
		return nil, nil, fmt.Errorf("unmarshal judge response: %w", err)
	}

	out := make([]parsedFinding, 0, len(resp.Findings))
	var dropped []Dropped
	for _, f := range resp.Findings {
		issue := strings.TrimSpace(strings.ToLower(f.IssueType))
		if !knownIssueTypes[issue] {
			issue = issueOther
		}
		p := parsedFinding{
			OriginalText:        strings.TrimSpace(f.OriginalText),
			IssueType:           issue,
			SuggestedCorrection: strings.TrimSpace(f.SuggestedCorrection),
			Confidence:          clampConfidence(f.Confidence),
			AnchorOffset:        anchorValue(f.AnchorOffset),
			AnchorOccurrence:    anchorValue(f.AnchorOccurrence),
		}
		if p.OriginalText == "" {
			dropped = append(dropped, dropOf(p, DropEmptySpan)) // a finding with no span is unusable
			continue
		}
		// A finding with no proposed correction is the least actionable and, in
		// practice, the dominant noise source (a "this looks off" with no fix).
		// The prompt requires a correction; enforce it structurally so a model
		// that ignores the instruction can't reintroduce that noise class.
		if p.SuggestedCorrection == "" {
			dropped = append(dropped, dropOf(p, DropEmptyCorrection))
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
		if normalizeForCompare(p.OriginalText) == normalizeForCompare(p.SuggestedCorrection) {
			dropped = append(dropped, dropOf(p, DropCosmeticOnly))
			continue
		}
		out = append(out, p)
	}
	return out, dropped, nil
}

// normalizeForCompare folds a span for a "is this a real word change?" test,
// exactly as decide's rung 0 does for cosmetic_only: lowercase, and every rune
// that is not a letter or digit removed — punctuation, hyphens AND spaces. Two
// spans that differ only in capitalization, punctuation, hyphenation or
// spacing fold to the same string ("the French" == "the french",
// "twenty-six" == "twenty six", "logo graphic" == "logographic"); a different
// word or a duplication does not. Spacing counts as cosmetic since judge@v2 /
// propose step 2: a pure split or merge changes no sound, and rung 0 rejects
// it, so it was only ever a row nobody could apply.
func normalizeForCompare(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
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
