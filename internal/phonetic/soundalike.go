package phonetic

import (
	"strings"
	"unicode"
)

// DefaultSoundAlikeThreshold is the similarity at or above which two phrases
// count as sounding alike. It is a starting point, not a calibrated value: the
// decide step will carry it as a recipe param and calibrate it on real
// findings.
const DefaultSoundAlikeThreshold = 0.67

// Phrase is one side of a comparison: the normalized letters that were
// encoded and their unbounded Double Metaphone codes.
type Phrase struct {
	Normalized string
	Codes      Codes
}

// Match is the full result of Compare, kept so a caller can explain a
// verdict (the codes behind a score), not just state it.
type Match struct {
	A, B  Phrase
	Score float64
}

// SoundAlike reports whether a and b plausibly sound the same, and the
// similarity score in [0, 1] that decided it. It passes iff score >= threshold
// and score > 0, so a zero threshold still cannot pass an empty comparison.
// See Compare for how the score is computed.
func SoundAlike(a, b string, threshold float64) (bool, float64) {
	m := Compare(a, b)
	return m.Score >= threshold && m.Score > 0, m.Score
}

// Compare scores how alike two phrases sound.
//
// Each side is normalized — lower-cased, digit runs spelled out as words
// ("240" → "two hundred forty", thousands separators allowed), everything but
// letters dropped, spaces included — so splits and fusions ("auto sebo" vs
// "arecibo", "placenes" vs "place names") compare as one word. Each is then
// encoded with Double Metaphone at unbounded length, and the score is the
// best, over the four {primary, alternate} pairings, of
// 1 − Levenshtein(codeA, codeB) / max(len(codeA), len(codeB)).
//
// Edge cases, all symmetric:
//   - either side normalizes to nothing (empty, or only punctuation): 0 —
//     there is nothing to compare, so the check fails closed;
//   - identical normalized text: 1;
//   - different text whose codes are all empty (letters Double Metaphone
//     does not voice, such as "h" or "w" alone): 0, again failing closed —
//     no phonetic evidence is not evidence of a match;
//   - one side's code empty, the other's not: 0 (the distance is the whole
//     other code).
func Compare(a, b string) Match {
	m := Match{A: encodePhrase(a), B: encodePhrase(b)}
	switch {
	case m.A.Normalized == "" || m.B.Normalized == "":
		return m
	case m.A.Normalized == m.B.Normalized:
		m.Score = 1
		return m
	}
	for _, ca := range []string{m.A.Codes.Primary, m.A.Codes.Alternate} {
		for _, cb := range []string{m.B.Codes.Primary, m.B.Codes.Alternate} {
			if s := codeSimilarity(ca, cb); s > m.Score {
				m.Score = s
			}
		}
	}
	return m
}

func encodePhrase(s string) Phrase {
	n := Normalize(s)
	return Phrase{Normalized: n, Codes: DoubleMetaphone(n, 0)}
}

// codeSimilarity is 1 − Levenshtein/max(len), 0 when both codes are empty.
func codeSimilarity(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	longest := max(len(ra), len(rb))
	if longest == 0 {
		return 0
	}
	return 1 - float64(levenshtein(ra, rb))/float64(longest)
}

// Normalize prepares text for phonetic encoding: lower-case, ASCII digit runs
// spelled out (NumberWords; a comma between digits is read as a thousands
// separator), and every rune that is not a letter removed — spaces too.
func Normalize(s string) string {
	runes := []rune(strings.ToLower(s))
	var b strings.Builder
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case isASCIIDigit(r):
			var digits []rune
			for i < len(runes) {
				if isASCIIDigit(runes[i]) {
					digits = append(digits, runes[i])
					i++
					continue
				}
				// "1,000": a comma between digits is a thousands separator.
				if runes[i] == ',' && i+1 < len(runes) && isASCIIDigit(runes[i+1]) {
					i++
					continue
				}
				break
			}
			for _, w := range NumberWords(string(digits)) {
				if w != ' ' {
					b.WriteRune(w)
				}
			}
		case unicode.IsLetter(r):
			b.WriteRune(r)
			i++
		default:
			i++
		}
	}
	return b.String()
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

// levenshtein is the classic two-row edit distance over runes.
func levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
