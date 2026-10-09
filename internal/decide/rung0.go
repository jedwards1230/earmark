package decide

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

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
	// ReasonNotWordBounded — the anchor resolved, but the span starts or ends
	// inside a word ("there" found in "therein"): editing it would rewrite
	// part of a word the judge did not flag.
	ReasonNotWordBounded = "not_word_bounded"
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
	// ReasonNotSoundAlike — a substitution with a changed region whose words
	// do not sound like the words they replace, or that only inserts or
	// deletes words.
	ReasonNotSoundAlike = "not_soundalike"
	// ReasonInflectionOnly — a substitution that changes one word only by
	// adding or removing a single regular ending ("hair" → "hairs", "box" →
	// "boxes", "walk" → "walked"; the exact rule is inflection's). A
	// grammar edit and a misheard ending look the same to every rung-0
	// signal, so neither is passed as a mishearing. Proper nouns are exempt.
	ReasonInflectionOnly = "inflection_only"
	// ReasonLetterSwap — a substitution hunk that swaps one single-letter
	// word for another ("c" → "b", "i" → "a"): letter names rhyme, and their
	// one-letter codes carry no evidence.
	ReasonLetterSwap = "letter_swap"
	// ReasonTooManyChanges — a substitution whose hunks each pass, but which
	// changes more than MaxChangedWords words in total: a rewrite, not a
	// mishearing.
	ReasonTooManyChanges = "too_many_changes"
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
	// MaxSubstitutionTokens and MaxSubstitutionRunes bound each side of each
	// changed region (hunk) a substitution is scored on. A misheard word or
	// name is a few words; a longer region is a rewrite, not a mishearing,
	// and would also make the phonetic comparison expensive. Past either
	// limit the candidate fails not_soundalike. number_artifact hunks are
	// held to the rune limit only — "one million two hundred thousand three
	// hundred forty five" is nine words but one number.
	// MaxSubstitutionRunes equals phonetic.MaxPhraseRunes.
	MaxSubstitutionTokens = 8
	MaxSubstitutionRunes  = phonetic.MaxPhraseRunes
	// MaxChangedWords caps the words a substitution changes, summed over its
	// hunks (each hunk counting its longer side), so many small hunks cannot
	// add up to a rewrite. number_artifact is exempt, as it is from the
	// per-hunk word limit: a spelled-out number is many words but one value,
	// and number_artifact is never applied by machine (it caps at hold).
	MaxChangedWords = 8
	// MaxDiffTokens bounds each side of the token alignment (an O(n·m)
	// table). An original or replacement past it fails not_soundalike for
	// a substitution; a judge span is a few words to a sentence.
	MaxDiffTokens = 256
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
//     one span (anchor_missing), and that span starts and ends on word
//     boundaries in the chunk (not_word_bounded);
//  2. the replacement is not empty (empty_correction);
//  3. the edit changes more than case, punctuation, hyphens and spacing, or
//     re-spellings that read aloud the same ("1937" for "nineteen thirty
//     seven") (cosmetic_only);
//  4. the issue-type rule — substitution, repeat removal or insertion; any
//     other type fails closed (unsupported_issue_type).
//
// Words are compared case-insensitively with punctuation removed: an
// apostrophe joins ("don't" is one word "dont"), any other non-letter,
// non-digit rune separates. The edit is split into hunks — the maximal
// changed regions of a token alignment (diffHunks) — so two separate
// mishearings in one span are checked separately, never as one window
// padded with the unchanged words between them.
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
	if runes := []rune(c.ChunkText); !wordBoundary(runes, span.Start) || !wordBoundary(runes, span.End) {
		return fail(ReasonNotWordBounded, span, "span %d-%d starts or ends inside a word in the chunk", span.Start, span.End)
	}

	if strings.TrimSpace(c.Replacement) == "" {
		return fail(ReasonEmptyCorrection, span, "suggested correction is empty")
	}
	if cosmetic(c.Original, c.Replacement) {
		return fail(ReasonCosmeticOnly, span, "%q and %q differ only in case, punctuation, hyphens, spacing or a re-spelling that reads the same", c.Original, c.Replacement)
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

// checkSubstitution requires every changed hunk of the edit to be a
// sound-alike substitution. Per hunk, in order:
//
//   - both sides non-empty: a hunk that only inserts or deletes words is not
//     a mishearing (that is dropped_word's or repeated_text's shape);
//   - each side within MaxSubstitutionTokens / MaxSubstitutionRunes;
//   - a hunk that reads aloud the same on both sides ("1937" ↔ "nineteen
//     thirty seven") passes as a re-spelling (Check has already refused an
//     edit made only of those);
//   - the numeral-value check: both sides write numerals with different
//     values → fail, since spoken numbers share most of their sounds;
//   - a letter swap (one single-letter word for another: "c" → "b", "i" →
//     "a") fails letter_swap: letter names rhyme, and their codes are one
//     letter long, so the score says nothing;
//   - a one-word change by one regular ending (inflection) fails
//     inflection_only, unless the issue type is misheard_proper_noun or
//     either word is capitalised (a name: "Jon" → "Jones");
//   - otherwise phonetic.Compare over the hunk's raw text, which joins words
//     (so "auto sebo" ↔ "arecibo" and "placenes" ↔ "place names" compare as
//     one word) and reads numerals every common way.
//
// When every hunk passes, the words changed across all hunks (the longer
// side of each) must not exceed MaxChangedWords (too_many_changes), except
// for number_artifact.
//
// A hunk is compared as the RAW text from its first word to its last on each
// side, so a numeral keeps its punctuation ("1,000", "3.5").
func checkSubstitution(c Candidate, span patch.Span, threshold float64) Verdict {
	o, r := tokenSpans(c.Original), tokenSpans(c.Replacement)
	if len(o) > MaxDiffTokens || len(r) > MaxDiffTokens {
		return fail(ReasonNotSoundAlike, span, "%d → %d words is too long to align (limit %d)", len(o), len(r), MaxDiffTokens)
	}
	hunks := diffHunks(texts(o), texts(r))
	evs := make([]string, 0, len(hunks))
	changed := 0
	for k, h := range hunks {
		ow, rw := o[h.oi:h.oj], r[h.ri:h.rj]
		at := fmt.Sprintf("hunk %d/%d", k+1, len(hunks))
		if len(ow) == 0 || len(rw) == 0 {
			return fail(ReasonNotSoundAlike, span, "%s: not a substitution: %q → %q only inserts or deletes words",
				at, joinTexts(ow), joinTexts(rw))
		}
		changed += max(len(ow), len(rw))
		a, b := rawText(c.Original, ow), rawText(c.Replacement, rw)
		for _, side := range []struct {
			text string
			n    int
		}{{a, len(ow)}, {b, len(rw)}} {
			// A spelled-out number is long in words but still one number,
			// so number_artifact hunks are measured in runes only.
			tooManyWords := side.n > MaxSubstitutionTokens && c.IssueType != IssueNumberArtifact
			if tooManyWords || utf8.RuneCountInString(side.text) > MaxSubstitutionRunes {
				return fail(ReasonNotSoundAlike, span, "%s: substituted words %q are too long to be a mishearing (%d words; limits %d words, %d runes)",
					at, side.text, side.n, MaxSubstitutionTokens, MaxSubstitutionRunes)
			}
		}
		if phonetic.SameReading(a, b) {
			evs = append(evs, fmt.Sprintf("%s: %q reads the same as %q", at, a, b))
			continue
		}
		if na, nb := numerals(ow), numerals(rw); len(na) > 0 && len(nb) > 0 && !slices.Equal(na, nb) {
			// Both sides write numerals and the written values differ, so
			// the edit changes a value, not a spelling. Spoken numbers share
			// most of their sounds ("two hundred forty" vs "... fifty"), so
			// a phonetic score would pass wrong numbers; rung 0 cannot tell
			// which value was spoken.
			return fail(ReasonNotSoundAlike, span, "%s: numeral change %q → %q: a value change cannot be checked phonetically", at, a, b)
		}
		if len(ow) == 1 && len(rw) == 1 && singleLetter(ow[0].text) && singleLetter(rw[0].text) {
			return fail(ReasonLetterSwap, span, "%s: letter swap %q → %q: letter names rhyme, so their sound is no evidence", at, a, b)
		}
		if len(ow) == 1 && len(rw) == 1 && c.IssueType != IssueMisheardProperNoun &&
			!capitalised(a) && !capitalised(b) && inflection(ow[0].text, rw[0].text) {
			return fail(ReasonInflectionOnly, span, "%s: %q → %q changes only an inflectional ending", at, a, b)
		}
		m := phonetic.Compare(a, b)
		ev := fmt.Sprintf("%s: soundalike %.3f (threshold %.2f): %s vs %s",
			at, m.Score, threshold, describe(a, m.A), describe(b, m.B))
		if !m.Passes(threshold) {
			return Verdict{Reason: ReasonNotSoundAlike, Span: span, Evidence: ev}
		}
		evs = append(evs, ev)
	}
	if changed > MaxChangedWords && c.IssueType != IssueNumberArtifact {
		return fail(ReasonTooManyChanges, span, "%d hunks change %d words in total (limit %d): a rewrite, not a mishearing",
			len(hunks), changed, MaxChangedWords)
	}
	if len(evs) == 0 { // unreachable after Check's cosmetic step; fail closed
		return fail(ReasonCosmeticOnly, span, "%q → %q changes no word", c.Original, c.Replacement)
	}
	return Verdict{Pass: true, Span: span, Evidence: strings.Join(evs, "; ")}
}

// hunk is one changed region of a token alignment: a[oi:oj] became b[ri:rj].
// One side may be empty (a pure insertion or deletion).
type hunk struct{ oi, oj, ri, rj int }

// diffHunks aligns a and b by token edit distance — keep, substitute, insert
// and delete, each change costing 1 — and returns the maximal runs of
// changes between kept tokens, in order. Edit distance rather than a
// longest-common-subsequence diff because a substitution is one change, not
// a delete plus an insert: "a cross a road" → "across a road" aligns as one
// hunk "a cross" → "across", where an LCS may keep the first "a" and split
// the edit into an insertion and a deletion. Ties keep tokens first, then
// substitute; the result is deterministic.
func diffHunks(a, b []string) []hunk {
	n, m := len(a), len(b)
	d := make([][]int, n+1)
	for i := range d {
		d[i] = make([]int, m+1)
		d[i][0] = i
	}
	for j := 0; j <= m; j++ {
		d[0][j] = j
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j-1]+cost, d[i-1][j]+1, d[i][j-1]+1)
		}
	}
	// Walk back from the end, marking kept pairs; hunks are the gaps.
	type pair struct{ i, j int }
	var kept []pair
	for i, j := n, m; i > 0 || j > 0; {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1] && d[i][j] == d[i-1][j-1]:
			kept = append(kept, pair{i - 1, j - 1})
			i, j = i-1, j-1
		case i > 0 && j > 0 && d[i][j] == d[i-1][j-1]+1:
			i, j = i-1, j-1
		case i > 0 && d[i][j] == d[i-1][j]+1:
			i--
		default:
			j--
		}
	}
	slices.Reverse(kept)
	kept = append(kept, pair{n, m}) // sentinel: the end of both
	var out []hunk
	pi, pj := 0, 0
	for _, k := range kept {
		if k.i > pi || k.j > pj {
			out = append(out, hunk{oi: pi, oj: k.i, ri: pj, rj: k.j})
		}
		pi, pj = k.i+1, k.j+1
	}
	return out
}

// cosmetic reports whether the edit from a to b changes nothing that is
// spoken: the same words after case, punctuation, hyphens and spacing are
// normalised, where every changed hunk reads aloud the same on both sides
// ("tic tac toe" ↔ "tic-tac-toe", "place names" ↔ "placenames",
// "nineteen thirty seven" ↔ "1937"). A hunk that inserts or deletes words
// is never cosmetic. Inputs past MaxDiffTokens are compared token for token.
func cosmetic(a, b string) bool {
	at, bt := tokenSpans(a), tokenSpans(b)
	if len(at) > MaxDiffTokens || len(bt) > MaxDiffTokens {
		return slices.Equal(texts(at), texts(bt))
	}
	for _, h := range diffHunks(texts(at), texts(bt)) {
		if h.oi == h.oj || h.ri == h.rj {
			return false
		}
		if !phonetic.SameReading(rawText(a, at[h.oi:h.oj]), rawText(b, bt[h.ri:h.rj])) {
			return false
		}
	}
	return true
}

// singleLetter reports whether a token is one letter — a spelled-out letter
// ("the c vocabulary") or the one-letter words "a" and "i".
func singleLetter(t string) bool {
	r, size := utf8.DecodeRuneInString(t)
	return size == len(t) && unicode.IsLetter(r)
}

// minStemRunes is the shortest stem inflection accepts for -s and -es, so
// short words that merely end in s ("is", "as") are not read as inflected
// forms; minEdStemRunes is the shortest for -ed.
const (
	minStemRunes   = 3
	minEdStemRunes = 4
)

// inflection reports whether one of a and b (lower-cased words) is EXACTLY
// the other plus one regular ending, and nothing else changed:
//
//   - "s" after a stem of at least minStemRunes letters ("hair" → "hairs");
//   - "es" after a stem of at least minStemRunes letters that ends in a
//     sibilant — s, x, z, ch or sh ("box" → "boxes"; not "tim" → "times");
//   - "ed" after a stem of at least minEdStemRunes letters ("walk" →
//     "walked"; not "bed" → "bed"+"ed").
//
// There is no other stemming: no -ing (distinct words collide there:
// "even"/"evening", "brown"/"browning"), no -d, no y → ies/ied, no
// fuzzy prefixes ("see"/"seed", "breed"/"bring"). Rung 0 rejects only clear
// junk; anything else is left to the phonetic score and the model.
func inflection(a, b string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	suffix, ok := strings.CutPrefix(b, a)
	if !ok || suffix == "" {
		return false
	}
	for _, r := range b {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	n := utf8.RuneCountInString(a)
	switch suffix {
	case "s":
		return n >= minStemRunes
	case "es":
		return n >= minStemRunes && sibilant(a)
	case "ed":
		return n >= minEdStemRunes
	}
	return false
}

// sibilant reports whether stem ends in a sound that takes "-es" for the
// plural: s, x, z, ch or sh.
func sibilant(stem string) bool {
	for _, e := range []string{"s", "x", "z", "ch", "sh"} {
		if strings.HasSuffix(stem, e) {
			return true
		}
	}
	return false
}

// capitalised reports whether a raw word starts with an upper-case letter.
// The transcript is lower-case by design, so a capital in the judge's text
// marks a name ("Jones", "Lucas"), and a name is exempt from inflection.
func capitalised(raw string) bool {
	r, _ := utf8.DecodeRuneInString(raw)
	return unicode.IsUpper(r)
}

func joinTexts(ts []token) string { return strings.Join(texts(ts), " ") }

// checkRepeat requires the replacement to be the original with an exact
// adjacent repeat collapsed: P + X×k + S → P + X + S, for a unit X of 1 to
// MaxRepeatTokens words and k >= 2 adjacent copies, with any prefix P and
// suffix S left unchanged.
//
// This is deliberately wider than the plan's literal "original is X repeated
// and replacement == X": the judge prompt's own example, "the the cat" → "the
// cat", carries context around the stutter (S = "cat"), and the literal rule
// would reject it. The change is still exactly "remove duplicate copies".
//
// The repeat is checked on the located span's text in the chunk (Check has
// already required it to be word-bounded, so "bathe the" does not contain
// "the the"), and a collapse is refused when the removed text contains
// sentence punctuation (hasSentenceBreak: . ? ! and other Sentence_Terminal
// runes, ; and …) in the gaps between its words: "the end. The end" is two
// sentences, not a stutter, while "3.5 3.5" is a stutter.
func checkRepeat(c Candidate, span patch.Span) Verdict {
	raw := string([]rune(c.ChunkText)[span.Start:span.End])
	o, r := tokenSpans(raw), tokens(c.Replacement)
	if unit, copies, ok := findCollapse(raw, o, r); ok {
		return Verdict{Pass: true, Span: span, Evidence: fmt.Sprintf("exact repeat: %q ×%d → ×1", strings.Join(unit, " "), copies)}
	}
	return fail(ReasonNotExactRepeat, span, "%q → %q does not remove an adjacent repeat of 1-%d words", c.Original, c.Replacement, MaxRepeatTokens)
}

// findCollapse searches o (tokens of raw) for a run of k >= 2 adjacent copies
// of a unit whose collapse to one copy yields exactly r, and whose removed
// region of raw has no sentence punctuation.
func findCollapse(raw string, ot []token, r []string) (unit []string, copies int, ok bool) {
	o := texts(ot)
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
			if slices.Equal(collapsed, r) && !breakBetween(raw, ot[i+size-1:i+k*size]) {
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
//  1. A candidate whose span overlaps a span OCCUPIED by an existing
//     accepted/applied correction fails overlaps_overlay. A correction
//     occupies its span when it resolves on the candidate's chunk text: a
//     non-empty correction, a matching (or legacy empty) chunk hash, and an
//     anchor patch.Locate places. That is stricter than the corrections
//     patch.Replay would apply: Replay quarantines two accepted corrections
//     that overlap EACH OTHER, so neither appears in its Applied set, and
//     using that set would let a new candidate over both through. Each
//     candidate's set excludes its own FindingID (re-deciding an accepted
//     finding must not conflict with itself) and is computed from that
//     candidate's view, so the self-exclusion never hides a conflict between
//     the candidate's row and another.
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

	occupiedBy := make(map[string][]occupied) // by chunk text
	var live []int
	for i, v := range out {
		if !v.Pass {
			continue
		}
		c := cands[i]
		occ, seen := occupiedBy[c.ChunkText]
		if !seen {
			occ = occupiedSpans(c.ChunkText, existing)
			occupiedBy[c.ChunkText] = occ
		}
		if o, hit := firstOverlap(v.Span, occ, c.FindingID); hit {
			out[i] = Verdict{Reason: ReasonOverlapsOverlay, Span: v.Span,
				Evidence: fmt.Sprintf("overlaps correction %s at runes %d-%d", o.id, o.span.Start, o.span.End)}
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

// describe renders one side of a phonetic comparison for evidence: the text,
// the reading that won when the text had several (numerals), and its codes.
func describe(text string, p phonetic.Phrase) string {
	if p.Readings > 1 {
		return fmt.Sprintf("%q read as %q (best of %d readings) [%s/%s]", text, p.Reading, p.Readings, p.Codes.Primary, p.Codes.Alternate)
	}
	return fmt.Sprintf("%q [%s/%s]", text, p.Codes.Primary, p.Codes.Alternate)
}

// occupied is the span an existing correction claims on a chunk text.
type occupied struct {
	id   string
	span patch.Span
}

// occupiedSpans resolves every existing correction that is usable on text —
// the checks of the overlay's resolvePatch (non-empty correction, hash match
// or legacy empty hash, Locate) WITHOUT its overlap quarantine. See Dedupe.
func occupiedSpans(text string, existing []patch.Patch) []occupied {
	hash := patch.ChunkHash(text)
	var out []occupied
	for _, p := range existing {
		if strings.TrimSpace(p.Correction) == "" || (p.ChunkHash != "" && p.ChunkHash != hash) {
			continue
		}
		span, err := patch.Locate(text, p.Anchor)
		if err != nil {
			continue
		}
		out = append(out, occupied{id: p.ID, span: span})
	}
	return out
}

// breakBetween reports whether any gap between consecutive tokens of ts (the
// separators in raw, not the tokens themselves — "3.5" holds a point but no
// break) contains a sentence break.
func breakBetween(raw string, ts []token) bool {
	runes := []rune(raw)
	for t := 0; t+1 < len(ts); t++ {
		if hasSentenceBreak(runes[ts[t].end:ts[t+1].start]) {
			return true
		}
	}
	return false
}

// hasSentenceBreak reports whether rs contains a sentence terminator
// (unicode.Sentence_Terminal: . ? ! and their script variants), a
// semicolon, or an ellipsis.
func hasSentenceBreak(rs []rune) bool {
	for _, r := range rs {
		if unicode.Is(unicode.Sentence_Terminal, r) || r == ';' || r == '\u2026' {
			return true
		}
	}
	return false
}

func firstOverlap(s patch.Span, occ []occupied, self string) (occupied, bool) {
	for _, o := range occ {
		if o.id != self && overlaps(s, o.span) {
			return o, true
		}
	}
	return occupied{}, false
}

// overlaps is the overlay's half-open interval test (see
// patch.PlanDirectEdit).
func overlaps(a, b patch.Span) bool { return a.Start < b.End && b.Start < a.End }

func fail(reason string, span patch.Span, format string, args ...any) Verdict {
	return Verdict{Reason: reason, Span: span, Evidence: fmt.Sprintf(format, args...)}
}

// token is one word of a string: its comparison text and its rune range in
// the source.
type token struct {
	text       string
	start, end int  // rune indices into the source string
	numeral    bool // scanned by phonetic.ScanNumeral
}

// tokenSpans splits s into lower-cased words with their rune ranges.
//
// A numeral that starts a word is ONE token, scanned with
// phonetic.ScanNumeral — the grammar phonetic.Readings uses — so "1,500",
// "3.15" and "12,345.67" are never split into fragments that diff and score
// separately. Its text is canonical: thousands separators dropped, the
// decimal point kept ("1,500" → "1500", "3.15" → "3.15"). Otherwise a word is
// a run of letters and digits; apostrophes are dropped inside it (and stay
// inside its range); every other rune separates words.
func tokenSpans(s string) []token {
	runes := []rune(s)
	var out []token
	var cur strings.Builder
	start, lastWord := -1, 0 // lastWord: rune index just past the last word rune
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, token{text: cur.String(), start: start, end: lastWord})
			cur.Reset()
		}
		start = -1
	}
	for n := 0; n < len(runes); {
		r := unicode.ToLower(runes[n])
		switch {
		case start < 0 && r >= '0' && r <= '9':
			num, _ := phonetic.ScanNumeral(runes, n) // runes[n] is a digit
			text := num.Whole
			if num.Fraction != "" {
				text += "." + num.Fraction
			}
			out = append(out, token{text: text, start: n, end: num.End, numeral: true})
			n = num.End
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if start < 0 {
				start = n
			}
			cur.WriteRune(r)
			lastWord = n + 1
		case r == '\'' || r == '\u2019':
		default:
			flush()
		}
		n++
	}
	flush()
	return out
}

// numerals returns the canonical text of every numeral token in ts, in order
// (see tokenSpans: thousands separators dropped, decimal point kept).
func numerals(ts []token) []string {
	var out []string
	for _, t := range ts {
		if t.numeral {
			out = append(out, t.text)
		}
	}
	return out
}

// tokens is the comparison text of tokenSpans.
func tokens(s string) []string { return texts(tokenSpans(s)) }

func texts(ts []token) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.text
	}
	return out
}

// rawText is the source text covered by a run of tokens, first to last.
func rawText(s string, ts []token) string {
	return string([]rune(s)[ts[0].start:ts[len(ts)-1].end])
}

// diffWindow counts the common leading (pre) and trailing (suf) tokens; the
// differing middles are a[pre:len(a)-suf] and b[pre:len(b)-suf].
func diffWindow(a, b []string) (pre, suf int) {
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	return pre, suf
}

// wordBoundary reports whether rune index i sits between words: at either
// end of the text, or not inside a word. A position is inside a word when
// both neighbours are letters or digits, or when it is next to a joiner that
// joins its own neighbours — an apostrophe between two letters ("won|'t",
// "can'|t") or a comma or point between two digits ("1|,000", "3.|5").
func wordBoundary(runes []rune, i int) bool {
	if i <= 0 || i >= len(runes) {
		return true
	}
	if isWordRune(runes[i-1]) && isWordRune(runes[i]) {
		return false
	}
	if i+1 < len(runes) && joins(runes[i-1], runes[i], runes[i+1]) {
		return false
	}
	if i >= 2 && joins(runes[i-2], runes[i-1], runes[i]) {
		return false
	}
	return true
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// joins reports whether j, between x and y, is part of one word.
func joins(x, j, y rune) bool {
	switch j {
	case '\'', '\u2019':
		return unicode.IsLetter(x) && unicode.IsLetter(y)
	case ',', '.':
		return unicode.IsDigit(x) && unicode.IsDigit(y)
	}
	return false
}
