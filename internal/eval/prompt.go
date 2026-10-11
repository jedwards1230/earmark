package eval

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

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

// systemPrompt instructs the judge under judge@v2 (judge@v3 is
// systemPromptV3). It is built around what the
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

// userPromptTemplate is the judge@v2 user message wrapped around each chunk's
// text: the book/track path (light context) and the span's time range. The
// verbatim chunk text follows it.
const userPromptTemplate = "Book/track: %s\nSpan time: %.1fs–%.1fs\n\nTranscript span:\n"

// systemPromptV3 is judge@v3: judge@v2 plus the book record (CONTRACT §2.15
// "Book context"). Condition 2 names the record as name context, and one
// rule says what the record is for — a spelling source for a name the ASR
// misheard, never a reason to change a word on its own. Rule 1 (sounds
// alike) still gates every finding, so a record name the transcript does not
// sound like cannot become a patch.
var systemPromptV3 = strings.TrimSpace(`
You check an audiobook transcript made by speech recognition (ASR) for words the ASR MISHEARD. Each finding becomes a patch to the transcript, and a wrong patch corrupts it. Most spans have no error; for those, return {"findings":[]}.

The text is lowercase and unpunctuated by design, and numbers may be words or digits. None of that is an error.

Flag a span only when both hold:
1. A word or name was misheard as a similar-sounding one: read aloud, your correction sounds like the original ("auto sebo" → "arecibo", "pin name" → "pen name"; not "teeth" → "earth"). If it does not, it is a rewrite: skip it. (Repeats and dropped words are the only exceptions.)
2. The context (the sentence; the book record or book/track path for names) shows what was said. Never guess.

When the book record lists a name that the misheard words sound like, prefer the record's spelling. Never change a word only because a record name exists — rule 1 still applies.

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

// userPromptTemplateV3 is the judge@v3 user message: the path, then the
// book context (the rendered Book record block, or "" when path_only — which
// makes the message the v2 one byte for byte), the span time and the span.
const userPromptTemplateV3 = "Book/track: %s\n%sSpan time: %.1fs–%.1fs\n\nTranscript span:\n"

// Judge prompt versions (CONTRACT §1.9 provenance). judge@v2 is the default;
// judge@v3 is opt-in (`earmark eval --judge-prompt v3`, or the propose pin
// param judge_prompt: v3) until a labelled A/B shows it is better — then flip
// DefaultJudgePrompt. Bump a version whenever its system prompt, user template,
// book record layout or the response schema changes on purpose;
// TestJudgePromptVersionPinned fails until you do. (The recipe also carries
// the prompt sha256, so even an unversioned edit yields a new recipe — the
// version is the human-readable half.)
const (
	judgePromptVersionV2 = "judge@v2"
	judgePromptVersionV3 = "judge@v3"
)

// JudgePrompt selects the judge prompt version a Judge runs.
type JudgePrompt string

// Selectable judge prompts.
const (
	// JudgePromptV2 is judge@v2: path-only context (the default).
	JudgePromptV2 JudgePrompt = "v2"
	// JudgePromptV3 is judge@v3: the book record block (opt-in).
	JudgePromptV3 JudgePrompt = "v3"
)

// DefaultJudgePrompt is the prompt a judge runs unless a run's
// --judge-prompt or the propose pin's judge_prompt param selects another —
// the in-pipeline worker, the dashboard's run-eval buttons, the backfill
// CronJob and the startup current recipe all follow it. It is the single
// flip point: switch it to JudgePromptV3 only after the labelled v2-vs-v3 A/B
// (CONTRACT §2.15 "Book context") shows v3 is better. Flipping it changes the
// live judge's recipe on deploy.
const DefaultJudgePrompt = JudgePromptV2

// ParseJudgePrompt parses a --judge-prompt value: "v2" / "v3", or the full
// "judge@v2" / "judge@v3" (any case). "" is DefaultJudgePrompt; anything else
// is an error.
func ParseJudgePrompt(s string) (JudgePrompt, error) {
	switch strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "judge@") {
	case "":
		return DefaultJudgePrompt, nil
	case "v2":
		return JudgePromptV2, nil
	case "v3":
		return JudgePromptV3, nil
	}
	return "", fmt.Errorf("unknown judge prompt %q (want v2 or v3)", s)
}

// known reports whether p is a selectable prompt. A Judge only ever holds a
// known prompt (NewJudge, SetPrompt, NewJudgeForConfig all check), so the
// methods below never see another value; for one they return the zero value
// rather than quietly standing in for a real version.
func (p JudgePrompt) known() bool {
	switch p {
	case JudgePromptV2, JudgePromptV3:
		return true
	}
	return false
}

// Version is the prompt's provenance name (judge@v2 / judge@v3).
func (p JudgePrompt) Version() string {
	switch p {
	case JudgePromptV2:
		return judgePromptVersionV2
	case JudgePromptV3:
		return judgePromptVersionV3
	}
	return ""
}

// usesBookContext reports whether the prompt carries the Book record block.
func (p JudgePrompt) usesBookContext() bool {
	switch p {
	case JudgePromptV3:
		return true
	}
	return false
}

// sha256 is the prompt's hash: every static part the judge sends.
func (p JudgePrompt) sha256() string {
	switch p {
	case JudgePromptV2:
		return judgePromptSHA256V2()
	case JudgePromptV3:
		return judgePromptSHA256V3()
	}
	return ""
}

// responseSchemaJSON is the JSON schema the reply is pinned to, as hashed.
var responseSchemaJSON = sync.OnceValue(func() string {
	schema, err := json.Marshal(findingsResponseFormat)
	if err != nil {
		// A package-level literal of maps and strings; marshalling cannot fail.
		panic(fmt.Sprintf("marshal findings response format: %v", err))
	}
	return string(schema)
})

// judgePromptSHA256V2 hashes judge@v2's parts: the system prompt, the user
// template, and the JSON schema the reply is pinned to — the exact inputs and
// order of the pre-v3 judgePromptSHA256, so the v2 recipe id is unchanged.
var judgePromptSHA256V2 = sync.OnceValue(func() string {
	return recipe.PromptSHA256(systemPrompt, userPromptTemplate, responseSchemaJSON())
})

// judgePromptSHA256V3 hashes judge@v3's static parts: the system prompt, the
// user template, the Book record layout (header, line format, the Names
// label) and the schema. Per-book content is not hashed here — it is
// book_context_sha256, recorded per transcript.
var judgePromptSHA256V3 = sync.OnceValue(func() string {
	layout := bookRecordHeader + bookRecordLine + bookRecordNames
	return recipe.PromptSHA256(systemPromptV3, userPromptTemplateV3, layout, responseSchemaJSON())
})

// buildPrompt returns the (system, user) prompt pair for one chunk under
// prompt p. bc is the chunk's book context; judge@v2 ignores it, and judge@v3
// with a path_only context sends v2's user message unchanged.
func buildPrompt(p JudgePrompt, c db.EvalChunk, bc BookContext) (system, user string) {
	var b strings.Builder
	switch p {
	case JudgePromptV3:
		fmt.Fprintf(&b, userPromptTemplateV3, c.FilePath, bc.Block, c.StartSec, c.EndSec)
		b.WriteString(c.Text)
		return systemPromptV3, b.String()
	default: // JudgePromptV2; a Judge holds no other value (known)
		fmt.Fprintf(&b, userPromptTemplate, c.FilePath, c.StartSec, c.EndSec)
		b.WriteString(c.Text)
		return systemPrompt, b.String()
	}
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
		// punctuation (or to hyphenate, re-space or re-spell a number). Those
		// are not transcription errors. Drop any finding rung 0 would refuse as
		// cosmetic_only — the same words once case, punctuation, hyphens and
		// spacing are folded, where every changed hunk reads aloud the same
		// ("nineteen thirty seven" ↔ "1937"). It is rung0@v2's own test,
		// copied (cosmetic), so this never drops a finding rung 0 could pass.
		// The prompt also forbids these, but the model ignores it often enough
		// that a structural guard is warranted.
		if cosmetic(p.OriginalText, p.SuggestedCorrection) {
			dropped = append(dropped, dropOf(p, DropCosmeticOnly))
			continue
		}
		out = append(out, p)
	}
	return out, dropped, nil
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
