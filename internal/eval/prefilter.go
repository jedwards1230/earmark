package eval

import (
	"slices"
	"strings"
	"unicode"

	"github.com/jedwards1230/earmark/internal/patch"
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
	// DropWindowTooLong — a changed hunk runs past maxSubstitutionWords on
	// either side: a rewrite, not a mishearing. Measured per hunk, as rung0@v2
	// does, so two small edits far apart in one span are not one long window.
	DropWindowTooLong = "window_too_long"
	// DropNumberFormat — a number rewritten between words and digits
	// ("nineteen thirty seven" → "1937").
	DropNumberFormat = "number_format"
	// DropBadInsertion — a dropped_word fix that does more than insert one or
	// two words.
	DropBadInsertion = "bad_insertion"
	// DropNotRemoval — a repeated_text fix that does not only remove words.
	DropNotRemoval = "not_a_removal"
)

// Edit-shape limits, the same as decide's rung 0 (MaxSubstitutionTokens,
// MaxInsertedTokens). Copied, not imported: internal/eval must not depend on
// the decide step, and a looser copy only lets more findings through.
const (
	maxSubstitutionWords = 8
	maxInsertedWords     = 2
)

// prefilter drops findings the decide step's rung 0 would reject on their
// shape alone, so they never become rows. Every check here is deterministic
// and needs only the chunk text the judge saw — no phonetics, no model.
//
// Each check is meant to reject a SUBSET of what rung 0 (rung0@v2, which
// checks a substitution hunk by hunk) rejects, so a finding rung 0 could pass
// is never lost here. The one deliberate exception is DropNumberFormat when
// the two sides do not read aloud the same (rung0@v2 already rejects a
// same-reading rewrite as cosmetic_only): the prompt forbids format changes,
// and rung 0 would score such an edit on sound alone. Word shapes are only
// checked when neither side has a digit: rung 0 reads "1,500" as one numeral
// token, this tokenizer would not.
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

	o, r := words(p.OriginalText), words(p.SuggestedCorrection)
	if p.IssueType != issueRepeatedText && p.IssueType != issueDroppedWord && isNumberFormat(o, r) {
		return DropNumberFormat
	}
	if hasDigit(p.OriginalText) || hasDigit(p.SuggestedCorrection) {
		return ""
	}
	switch p.IssueType {
	case issueRepeatedText:
		if len(r) >= len(o) || !isSubsequence(r, o) {
			return DropNotRemoval
		}
	case issueDroppedWord:
		added := len(r) - len(o)
		if len(o) == 0 || added < 1 || added > maxInsertedWords || !isSubsequence(o, r) {
			return DropBadInsertion
		}
	default: // misheard_proper_noun, misheard_word, homophone, number_artifact
		// Per changed hunk, as rung0@v2 scores a substitution: two small
		// mishearings far apart in one span are two short hunks, not one
		// long window.
		for _, h := range diffHunks(o, r) {
			ow, rw := o[h.oi:h.oj], r[h.ri:h.rj]
			if len(ow) == 0 || len(rw) == 0 {
				return DropNotSubstitution
			}
			if p.IssueType != issueNumberArtifact && (len(ow) > maxSubstitutionWords || len(rw) > maxSubstitutionWords) {
				return DropWindowTooLong
			}
		}
	}
	return ""
}

// hunk is one changed region of a word alignment: a[oi:oj] became b[ri:rj].
// One side may be empty (a pure insertion or deletion).
type hunk struct{ oi, oj, ri, rj int }

// diffHunks aligns a and b by word edit distance (keep, substitute, insert,
// delete; each change costs 1) and returns the maximal changed regions
// between kept words, in order. It is a copy of rung0@v2's alignment
// (internal/decide), tie-breaks included, so a pre-filter hunk is exactly a
// rung-0 hunk and the per-hunk limits here can never be stricter than
// rung 0's. Edit distance, not an LCS: a substitution is one change, so
// "a cross a road" → "across a road" is one hunk "a cross" → "across".
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

// splitsWord reports whether rune index i falls between two letters/digits —
// the span would cut a word in two. Looser than rung 0's boundary test (which
// also treats an apostrophe between letters as inside a word), so it never
// rejects a span rung 0 accepts.
func splitsWord(runes []rune, i int) bool {
	if i <= 0 || i >= len(runes) {
		return false
	}
	return isWordRune(runes[i-1]) && isWordRune(runes[i])
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func hasDigit(s string) bool { return strings.IndexFunc(s, unicode.IsDigit) >= 0 }

// words splits s into lower-cased words the way rung 0 does for text without
// numerals: a word is a run of letters and digits, apostrophes are dropped
// inside it, and every other rune separates words.
func words(s string) []string {
	var out []string
	var cur strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case isWordRune(r):
			cur.WriteRune(r)
		case r == '\'' || r == '’':
		default:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

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

// diffWindow trims the common leading and trailing words and returns the
// differing middles of a and b.
func diffWindow(a, b []string) (aw, bw []string) {
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

// isNumberFormat reports whether the changed words rewrite a number between
// spelled-out words and digits: one side's window is all number words (with
// at least one that is not "and"/"point"), the other's all digit groups.
func isNumberFormat(o, r []string) bool {
	ow, rw := diffWindow(o, r)
	return (allNumberWords(ow) && allDigits(rw)) || (allDigits(ow) && allNumberWords(rw))
}

// numberWords are the words of a spelled-out English number. "and" and
// "point" are joiners: they may appear but cannot make a number alone.
var numberWords = map[string]bool{
	"zero": true, "oh": true, "one": true, "two": true, "three": true, "four": true, "five": true,
	"six": true, "seven": true, "eight": true, "nine": true, "ten": true, "eleven": true,
	"twelve": true, "thirteen": true, "fourteen": true, "fifteen": true, "sixteen": true,
	"seventeen": true, "eighteen": true, "nineteen": true, "twenty": true, "thirty": true,
	"forty": true, "fifty": true, "sixty": true, "seventy": true, "eighty": true, "ninety": true,
	"hundred": true, "thousand": true, "million": true, "billion": true, "trillion": true,
}

func allNumberWords(ws []string) bool {
	core := 0
	for _, w := range ws {
		switch {
		case numberWords[w]:
			core++
		case w == "and" || w == "point":
		default:
			return false
		}
	}
	return core > 0
}

func allDigits(ws []string) bool {
	return len(ws) > 0 && !slices.ContainsFunc(ws, func(w string) bool {
		return strings.IndexFunc(w, func(r rune) bool { return !unicode.IsDigit(r) }) >= 0
	})
}
