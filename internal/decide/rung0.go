package decide

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/phonetic"
)

// Issue types the judge emits (transcript_findings.issue_type). They mirror
// the closed vocabulary in internal/eval/prompt.go; TestIssueTypesMatchEval
// keeps the two in step.
const (
	IssueMisheardProperNoun = "misheard_proper_noun"
	IssueMisheardWord       = "misheard_word"
	IssueHomophone          = "homophone"
	IssueNumberArtifact     = "number_artifact"
	IssueRepeatedText       = "repeated_text"
	IssueDroppedWord        = "dropped_word"
	// IssueOther is the eval layer's coercion sink for an unknown model value.
	// It names no edit shape, so rung 0 cannot vet it.
	IssueOther = "other"
)

// Rung-0 reject reasons, Verdict.Reason on a failing verdict. They are meant
// to be persisted with the decision, so treat them as a data contract: add
// new ones, never rename.
const (
	// ReasonChunkChanged — the chunk no longer hashes to the revision the judge
	// reviewed. Re-anchoring's problem, not the decide recipe's.
	ReasonChunkChanged = "chunk_changed"
	// ReasonAnchorMissing — the anchor resolves to no span, or to several
	// (patch.Locate refuses rather than guesses).
	ReasonAnchorMissing = "anchor_missing"
	// ReasonEmptyCorrection — the replacement is empty or whitespace; applying
	// it would silently delete text. Same rule as the overlay's
	// patch.StaleReasonEmptyCorrection.
	ReasonEmptyCorrection = "empty_correction"
	// ReasonCosmeticOnly — the edit only changes case, punctuation, hyphens or
	// spacing, which the transcript leaves out by design.
	ReasonCosmeticOnly = "cosmetic_only"
	// ReasonUnsupportedIssueType — the issue type ("other", or anything rung 0
	// does not know) names no edit shape that can be checked. Fails closed.
	ReasonUnsupportedIssueType = "unsupported_issue_type"
	// ReasonNotSoundAlike — a substitution whose changed words do not sound
	// like the words they replace.
	ReasonNotSoundAlike = "not_soundalike"
	// ReasonNotExactRepeat — a repeated_text fix that is not the removal of an
	// exact adjacent repeat, word-bounded, at the located span.
	ReasonNotExactRepeat = "not_exact_repeat"
	// ReasonBadInsertion — a dropped_word fix that does anything other than
	// insert one or two words.
	ReasonBadInsertion = "bad_insertion"
	// ReasonOverlapDup — another passing candidate on the same chunk claims
	// overlapping text and won the tie-break.
	ReasonOverlapDup = "overlap_dup"
	// ReasonOverlapsOverlay — the span overlaps a correction already accepted
	// or applied on this chunk.
	ReasonOverlapsOverlay = "overlaps_overlay"
)

// Limits on the edit shapes rung 0 accepts.
const (
	// MaxRepeatTokens is the longest repeated unit (in words) a repeated_text
	// fix may remove copies of.
	MaxRepeatTokens = 6
	// MaxInsertedTokens is the most words a dropped_word fix may insert.
	MaxInsertedTokens = 2
)

// Candidate is one proposed finding, with the pristine text of the chunk it
// was judged against, ready for the rung-0 checks.
type Candidate struct {
	// FindingID is transcript_findings.id (a UUID, kept as text like
	// patch.Patch.ID).
	FindingID   string
	IssueType   string
	Original    string // original_text, the verbatim span the judge copied
	Replacement string // suggested_correction
	Confidence  float64
	Anchor      patch.Anchor
	// ChunkText is the chunk's PRISTINE text — COALESCE(source_text, text) —
	// never the corrected surface, for the reason patch.PlanDirectEdit gives.
	ChunkText string
	// ChunkHash is the finding's chunk_text_sha256: the revision the judge saw.
	ChunkHash string
}

// Verdict is the rung-0 outcome for one candidate.
type Verdict struct {
	// Pass means the candidate survived every rung-0 check — it is eligible
	// for the next rung, not decided.
	Pass bool
	// Reason is one of the Reason* constants when Pass is false, and empty
	// when Pass is true.
	Reason string
	// Span is the located target in rune indices into ChunkText. It is set
	// whenever the anchor resolved, including on a later failure, and zero
	// when it did not.
	Span patch.Span
	// Evidence is a human-readable account of what the check saw — scores,
	// codes, tokens — for audit, not for parsing.
	Evidence string
}

// Params tunes the rung-0 checks. It is destined to be part of the decide
// recipe's params (CONTRACT §1.9).
type Params struct {
	// SoundAlikeThreshold is the phonetic.SoundAlike pass mark for
	// substitutions. Zero or negative means phonetic.DefaultSoundAlikeThreshold.
	SoundAlikeThreshold float64
}

// DefaultParams returns the rung-0 defaults.
func DefaultParams() Params {
	return Params{SoundAlikeThreshold: phonetic.DefaultSoundAlikeThreshold}
}

func (p Params) threshold() float64 {
	if p.SoundAlikeThreshold <= 0 {
		return phonetic.DefaultSoundAlikeThreshold
	}
	return p.SoundAlikeThreshold
}

// Rung0 runs Check on every candidate of ONE chunk, then Dedupe. existing is
// that chunk's accepted + applied corrections (one chunk's slice of the
// overlay db.BuildOverlay builds). The result is index-aligned with cands.
func Rung0(cands []Candidate, existing []patch.Patch, p Params) []Verdict {
	verdicts := make([]Verdict, len(cands))
	for i, c := range cands {
		verdicts[i] = Check(c, p)
	}
	return Dedupe(cands, verdicts, existing)
}

// Check runs the per-candidate rung-0 checks, in this order, and returns at
// the first failure:
//
//  1. target exists — ChunkText hashes to ChunkHash (chunk_changed; an empty
//     ChunkHash fails too: unlike the overlay's replay, which tolerates legacy
//     rows without a hash, a machine decision must not act on a revision it
//     cannot verify), the
//     anchor's text is the finding's original text and resolves to exactly
//     one span (anchor_missing);
//  2. the replacement is not empty (empty_correction);
//  3. the edit changes more than case, punctuation, hyphens and spacing
//     (cosmetic_only);
//  4. the issue-type rule — substitution, repeat removal or insertion; any
//     other type fails closed (unsupported_issue_type).
//
// Words are compared case-insensitively with punctuation removed: an
// apostrophe joins ("don't" is one word "dont"), any other non-letter,
// non-digit rune separates.
func Check(c Candidate, p Params) Verdict {
	if c.ChunkHash == "" {
		return fail(ReasonChunkChanged, patch.Span{}, "finding recorded no chunk hash; the judged revision cannot be verified")
	}
	if patch.ChunkHash(c.ChunkText) != c.ChunkHash {
		return fail(ReasonChunkChanged, patch.Span{}, "chunk text no longer hashes to the judged revision %.12s", c.ChunkHash)
	}
	anchor := c.Anchor
	if anchor.OriginalText == "" {
		anchor.OriginalText = c.Original
	}
	if anchor.OriginalText != c.Original {
		return fail(ReasonAnchorMissing, patch.Span{}, "anchor text %q differs from original %q", anchor.OriginalText, c.Original)
	}
	span, err := patch.Locate(c.ChunkText, anchor)
	if err != nil {
		what := "not found"
		if errors.Is(err, patch.ErrAnchorAmbiguous) {
			what = "ambiguous"
		}
		return fail(ReasonAnchorMissing, patch.Span{}, "anchor %s: %v", what, err)
	}

	if strings.TrimSpace(c.Replacement) == "" {
		return fail(ReasonEmptyCorrection, span, "suggested correction is empty")
	}
	if a, b := squash(c.Original), squash(c.Replacement); a == b {
		return fail(ReasonCosmeticOnly, span, "%q and %q differ only in case, punctuation, hyphens or spacing", c.Original, c.Replacement)
	}

	switch c.IssueType {
	case IssueMisheardProperNoun, IssueMisheardWord, IssueHomophone, IssueNumberArtifact:
		return checkSubstitution(c, span, p.threshold())
	case IssueRepeatedText:
		return checkRepeat(c, span)
	case IssueDroppedWord:
		return checkInsertion(c, span)
	default:
		return fail(ReasonUnsupportedIssueType, span, "issue type %q has no rung-0 rule", c.IssueType)
	}
}

// checkSubstitution requires the differing token window to sound alike.
func checkSubstitution(c Candidate, span patch.Span, threshold float64) Verdict {
	o, r := tokens(c.Original), tokens(c.Replacement)
	ow, rw := diffWindow(o, r)
	if len(ow) == 0 || len(rw) == 0 {
		return fail(ReasonNotSoundAlike, span, "not a substitution: %q → %q only inserts or deletes words", c.Original, c.Replacement)
	}
	a, b := strings.Join(ow, " "), strings.Join(rw, " ")
	m := phonetic.Compare(a, b)
	ev := fmt.Sprintf("soundalike %.3f (threshold %.2f): %q [%s/%s] vs %q [%s/%s]",
		m.Score, threshold, a, m.A.Codes.Primary, m.A.Codes.Alternate, b, m.B.Codes.Primary, m.B.Codes.Alternate)
	if ok, _ := phonetic.SoundAlike(a, b, threshold); !ok {
		return Verdict{Reason: ReasonNotSoundAlike, Span: span, Evidence: ev}
	}
	return Verdict{Pass: true, Span: span, Evidence: ev}
}

// checkRepeat requires the replacement to be the original with an exact
// adjacent repeat collapsed — P + X×k + S → P + X + S for a unit X of 1 to
// MaxRepeatTokens words and k >= 2 — and the located span to be
// word-bounded in the chunk, so a repeat found inside a longer word
// ("bathe the" containing "the the") is refused.
func checkRepeat(c Candidate, span patch.Span) Verdict {
	runes := []rune(c.ChunkText)
	if !wordBoundary(runes, span.Start) || !wordBoundary(runes, span.End) {
		return fail(ReasonNotExactRepeat, span, "span %d-%d is not word-bounded in the chunk", span.Start, span.End)
	}
	o, r := tokens(string(runes[span.Start:span.End])), tokens(c.Replacement)
	if unit, copies, ok := findCollapse(o, r); ok {
		return Verdict{Pass: true, Span: span, Evidence: fmt.Sprintf("exact repeat: %q ×%d → ×1", strings.Join(unit, " "), copies)}
	}
	return fail(ReasonNotExactRepeat, span, "%q → %q does not remove an adjacent repeat of 1-%d words", c.Original, c.Replacement, MaxRepeatTokens)
}

// findCollapse searches o for a run of k >= 2 adjacent copies of a unit whose
// collapse to one copy yields exactly r.
func findCollapse(o, r []string) (unit []string, copies int, ok bool) {
	removed := len(o) - len(r)
	if removed <= 0 {
		return nil, 0, false
	}
	for size := 1; size <= MaxRepeatTokens && size <= removed; size++ {
		if removed%size != 0 {
			continue
		}
		k := removed/size + 1
		for i := 0; i+k*size <= len(o); i++ {
			if !isRun(o[i:i+k*size], size) {
				continue
			}
			collapsed := slices.Concat(o[:i+size], o[i+k*size:])
			if slices.Equal(collapsed, r) {
				return o[i : i+size], k, true
			}
		}
	}
	return nil, 0, false
}

// isRun reports whether s is whole copies of its first size tokens.
func isRun(s []string, size int) bool {
	for j := size; j < len(s); j++ {
		if s[j] != s[j-size] {
			return false
		}
	}
	return true
}

// checkInsertion requires the original's words to appear, in order, in the
// replacement, with 1 to MaxInsertedTokens words added and none changed.
func checkInsertion(c Candidate, span patch.Span) Verdict {
	o, r := tokens(c.Original), tokens(c.Replacement)
	added := len(r) - len(o)
	if len(o) == 0 || added < 1 || added > MaxInsertedTokens {
		return fail(ReasonBadInsertion, span, "%q → %q adds %d words, want 1-%d", c.Original, c.Replacement, added, MaxInsertedTokens)
	}
	inserted, ok := subsequenceExtras(o, r)
	if !ok {
		return fail(ReasonBadInsertion, span, "%q → %q changes existing words, not only inserts", c.Original, c.Replacement)
	}
	return Verdict{Pass: true, Span: span, Evidence: fmt.Sprintf("inserts %q", strings.Join(inserted, " "))}
}

// subsequenceExtras reports whether sub is an in-order subsequence of full,
// returning the tokens of full that are not part of the match.
func subsequenceExtras(sub, full []string) ([]string, bool) {
	var extras []string
	i := 0
	for _, t := range full {
		if i < len(sub) && t == sub[i] {
			i++
			continue
		}
		extras = append(extras, t)
	}
	return extras, i == len(sub)
}

// Dedupe resolves overlaps among one chunk's candidates. verdicts must be
// index-aligned with cands (Check's output); the result is a new slice in the
// same order. It panics if the lengths differ, which is a programming error.
//
// Only passing verdicts take part — a failed check has no trustworthy span.
// In order:
//
//  1. A candidate whose span overlaps a correction that would actually replay
//     onto its chunk text — patch.Replay(ChunkText, existing).Applied, the same
//     test patch.PlanDirectEdit uses — fails overlaps_overlay. A correction
//     with the candidate's own FindingID is ignored (re-deciding an accepted
//     finding must not conflict with itself).
//  2. The rest are ranked by Confidence descending, then FindingID ascending,
//     and kept greedily: one that overlaps an already-kept candidate fails
//     overlap_dup.
//
// Spans overlap when they share at least one rune (half-open intervals, as in
// the overlay). Candidates are only compared with others judged against the
// same chunk text.
func Dedupe(cands []Candidate, verdicts []Verdict, existing []patch.Patch) []Verdict {
	if len(cands) != len(verdicts) {
		panic(fmt.Sprintf("decide.Dedupe: %d candidates but %d verdicts", len(cands), len(verdicts)))
	}
	out := slices.Clone(verdicts)

	overlays := make(map[string][]patch.AppliedPatch) // by chunk text
	var live []int
	for i, v := range out {
		if !v.Pass {
			continue
		}
		c := cands[i]
		applied, seen := overlays[c.ChunkText]
		if !seen {
			applied = patch.Replay(c.ChunkText, existing).Applied
			overlays[c.ChunkText] = applied
		}
		if a, hit := firstOverlap(v.Span, applied, c.FindingID); hit {
			out[i] = Verdict{Reason: ReasonOverlapsOverlay, Span: v.Span,
				Evidence: fmt.Sprintf("overlaps correction %s at runes %d-%d", a.ID, a.Span.Start, a.Span.End)}
			continue
		}
		live = append(live, i)
	}

	slices.SortStableFunc(live, func(a, b int) int {
		if c := cmp.Compare(cands[b].Confidence, cands[a].Confidence); c != 0 {
			return c
		}
		return strings.Compare(cands[a].FindingID, cands[b].FindingID)
	})
	var kept []int
	for _, i := range live {
		winner := -1
		for _, k := range kept {
			if cands[k].ChunkText == cands[i].ChunkText && overlaps(out[k].Span, out[i].Span) {
				winner = k
				break
			}
		}
		if winner < 0 {
			kept = append(kept, i)
			continue
		}
		out[i] = Verdict{Reason: ReasonOverlapDup, Span: out[i].Span,
			Evidence: fmt.Sprintf("overlaps finding %s (confidence %.2f) at runes %d-%d",
				cands[winner].FindingID, cands[winner].Confidence, out[winner].Span.Start, out[winner].Span.End)}
	}
	return out
}

func firstOverlap(s patch.Span, applied []patch.AppliedPatch, self string) (patch.AppliedPatch, bool) {
	for _, a := range applied {
		if a.ID != self && overlaps(s, a.Span) {
			return a, true
		}
	}
	return patch.AppliedPatch{}, false
}

// overlaps is the overlay's half-open interval test (see
// patch.PlanDirectEdit).
func overlaps(a, b patch.Span) bool { return a.Start < b.End && b.Start < a.End }

func fail(reason string, span patch.Span, format string, args ...any) Verdict {
	return Verdict{Reason: reason, Span: span, Evidence: fmt.Sprintf(format, args...)}
}

// squash lower-cases s and keeps only letters and digits, for the
// cosmetic-only comparison.
func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tokens splits s into lower-cased words of letters and digits. Apostrophes
// are dropped inside a word; every other rune that is not a letter or digit
// separates words.
func tokens(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur.WriteRune(r)
		case r == '\'' || r == '’':
		default:
			flush()
		}
	}
	flush()
	return out
}

// diffWindow strips the common leading and trailing tokens and returns the
// differing middles.
func diffWindow(a, b []string) ([]string, []string) {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	return a[pre : len(a)-suf], b[pre : len(b)-suf]
}

// wordBoundary reports whether rune index i sits between words: at either
// end of the text, or next to a rune that is not a letter or digit.
func wordBoundary(runes []rune, i int) bool {
	if i <= 0 || i >= len(runes) {
		return true
	}
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	return !isWord(runes[i-1]) || !isWord(runes[i])
}
