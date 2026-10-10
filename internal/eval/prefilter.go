package eval

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/phonetic"
)

// Pre-filter drop reasons (proposeStepVersion 2). Each names an edit the
// decide step's rung 0 (CONTRACT §2.19) would refuse, so the finding is
// dropped before it becomes a row. Named after the rung-0 reason they mirror
// where there is one.
const (
	// DropUnsupportedIssueType — issue type "other" (an unknown model value
	// coerced): it names no edit shape decide can check.
	DropUnsupportedIssueType = "unsupported_issue_type"
	// DropAnchorMissing — original_text is not in the chunk, or occurs more
	// than once and the anchor does not say which (patch.Locate refuses).
	DropAnchorMissing = "anchor_missing"
	// DropNotWordBounded — the located span starts or ends inside a word.
	DropNotWordBounded = "not_word_bounded"
	// DropNotSubstitution — a misheard_*/homophone/number_artifact fix with a
	// changed hunk that only inserts or only deletes words: no existing word
	// is replaced there, so there is nothing to have misheard.
	DropNotSubstitution = "not_substitution"
	// DropWindowTooLong — a changed hunk runs past maxSubstitutionWords or
	// maxSubstitutionRunes on either side, or a side is too long to align: a
	// rewrite, not a mishearing. Measured per hunk, as rung0@v2 does, so two
	// small edits far apart in one span are not one long window.
	DropWindowTooLong = "window_too_long"
	// DropBadInsertion — a dropped_word fix that does more than insert one or
	// two words.
	DropBadInsertion = "bad_insertion"
	// DropNotRemoval — a repeated_text fix that does not only remove words.
	DropNotRemoval = "not_a_removal"
)

// Edit-shape limits, the same as decide's rung 0 (MaxSubstitutionTokens,
// MaxSubstitutionRunes, MaxInsertedTokens, MaxDiffTokens). Copied, not
// imported: internal/eval must not depend on the decide step, and a looser
// copy only lets more findings through.
const (
	maxSubstitutionWords = 8
	maxSubstitutionRunes = phonetic.MaxPhraseRunes
	maxInsertedWords     = 2
	maxDiffWords         = 256
)

// prefilter drops findings the decide step's rung 0 would reject on their
// shape alone, so they never become rows. Every check here is deterministic
// and needs only the chunk text the judge saw — no model.
//
// Each check rejects a SUBSET of what rung 0 rejects, with no exception: a
// finding rung 0 could pass is never lost here (TestPrefilterNeverStricter
// pins that on rung 0's own test vectors). The word alignment, tokenizer and
// cosmetic test are copies of rung0@v2's (see diffHunks), so a pre-filter
// hunk is exactly a rung-0 hunk.
func prefilter(chunkText string, parsed []parsedFinding, dropped []Dropped) ([]parsedFinding, []Dropped) {
	kept := parsed[:0:0]
	for _, p := range parsed {
		if reason := prefilterReason(chunkText, p); reason != "" {
			dropped = append(dropped, dropOf(p, reason))
			continue
		}
		kept = append(kept, p)
	}
	return kept, dropped
}

// prefilterReason is the first pre-filter p fails, or "".
func prefilterReason(chunkText string, p parsedFinding) string {
	if p.IssueType == issueOther {
		return DropUnsupportedIssueType
	}
	span, err := patch.Locate(chunkText, patch.Anchor{
		OriginalText: p.OriginalText, Offset: p.AnchorOffset, Occurrence: p.AnchorOccurrence,
	})
	if err != nil {
		return DropAnchorMissing
	}
	if runes := []rune(chunkText); splitsWord(runes, span.Start) || splitsWord(runes, span.End) {
		return DropNotWordBounded
	}

	switch p.IssueType {
	case issueRepeatedText:
		// Rung 0 needs an exact adjacent repeat collapsed, which implies
		// fewer words that are an in-order subset of the original.
		o, r := tokens(p.OriginalText), tokens(p.SuggestedCorrection)
		if len(r) >= len(o) || !isSubsequence(r, o) {
			return DropNotRemoval
		}
	case issueDroppedWord:
		// Rung 0's insertion rule, in full.
		o, r := tokens(p.OriginalText), tokens(p.SuggestedCorrection)
		added := len(r) - len(o)
		if len(o) == 0 || added < 1 || added > maxInsertedWords || !isSubsequence(o, r) {
			return DropBadInsertion
		}
	default: // misheard_proper_noun, misheard_word, homophone, number_artifact
		return substitutionReason(p)
	}
	return ""
}

// substitutionReason runs the shape half of rung 0's per-hunk substitution
// check: every hunk replaces words on both sides, within the word and rune
// limits (number_artifact: runes only). The phonetic half is left to rung 0.
func substitutionReason(p parsedFinding) string {
	o, r := tokenSpans(p.OriginalText), tokenSpans(p.SuggestedCorrection)
	if len(o) > maxDiffWords || len(r) > maxDiffWords {
		return DropWindowTooLong
	}
	for _, h := range diffHunks(texts(o), texts(r)) {
		ow, rw := o[h.oi:h.oj], r[h.ri:h.rj]
		if len(ow) == 0 || len(rw) == 0 {
			return DropNotSubstitution
		}
		for _, side := range []struct {
			text string
			n    int
		}{{rawText(p.OriginalText, ow), len(ow)}, {rawText(p.SuggestedCorrection, rw), len(rw)}} {
			tooManyWords := side.n > maxSubstitutionWords && p.IssueType != issueNumberArtifact
			if tooManyWords || utf8.RuneCountInString(side.text) > maxSubstitutionRunes {
				return DropWindowTooLong
			}
		}
	}
	return ""
}

// The rest of this file is copied from internal/decide/rung0.go as at
// rung0@v2 (jedwards1230/earmark branch feat/decide-quality-wins, 2e37f08):
// hunk, diffHunks, cosmetic, token, tokenSpans, tokens, texts and rawText,
// with their tie-breaks. TestDiffHunksMatchesRung0, TestTokenSpansMatchesRung0
// and TestPrefilterNeverStricter carry rung 0's own vectors, so a drift on
// either side fails here. Change them together.

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
// is never cosmetic. Inputs past maxDiffWords are compared token for token.
func cosmetic(a, b string) bool {
	at, bt := tokenSpans(a), tokenSpans(b)
	if len(at) > maxDiffWords || len(bt) > maxDiffWords {
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

// token is one word of a string: its comparison text and its rune range in
// the source.
type token struct {
	text       string
	start, end int // rune indices into the source string
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
			out = append(out, token{text: text, start: n, end: num.End})
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

// splitsWord reports whether rune index i falls between two letters/digits —
// the span would cut a word in two. Looser than rung 0's boundary test (which
// also treats an apostrophe between letters, or a comma or point between
// digits, as inside a word), so it never rejects a span rung 0 accepts.
func splitsWord(runes []rune, i int) bool {
	if i <= 0 || i >= len(runes) {
		return false
	}
	return isWordRune(runes[i-1]) && isWordRune(runes[i])
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// isSubsequence reports whether sub appears in full in order (gaps allowed).
func isSubsequence(sub, full []string) bool {
	i := 0
	for _, w := range full {
		if i < len(sub) && w == sub[i] {
			i++
		}
	}
	return i == len(sub)
}
