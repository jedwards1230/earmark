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

// Phrase is one side of a comparison: the reading that won (Compare tries
// several spoken readings of each numeral), the letters that were encoded, and
// their unbounded Double Metaphone codes.
type Phrase struct {
	// Reading is the winning reading, lower-case words separated by spaces
	// ("two forty" for "240").
	Reading string
	// Normalized is Reading with everything but letters removed — what was
	// encoded.
	Normalized string
	Codes      Codes
	// Readings is how many readings of this side were tried.
	Readings int
}

// Match is the full result of Compare, kept so a caller can explain a
// verdict (the readings and codes behind a score), not just state it.
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
	return m.Passes(threshold), m.Score
}

// Passes applies SoundAlike's pass rule to an existing Match: score >=
// threshold and score > 0.
func (m Match) Passes(threshold float64) bool { return m.Score >= threshold && m.Score > 0 }

// Compare scores how alike two phrases sound.
//
// Each side is expanded into its spoken readings (Readings): lower-cased,
// each digit run spelled out as words in every way it is commonly said
// ("240" → "two hundred forty", "two forty", "two four zero"), everything but
// letters dropped, spaces included — so splits and fusions ("auto sebo" vs
// "arecibo", "placenes" vs "place names") compare as one word. Each reading
// is encoded with Double Metaphone at unbounded length, and the score is the
// best, over every pair of readings and the four {primary, alternate} code
// pairings, of 1 − Levenshtein(codeA, codeB) / max(len(codeA), len(codeB)).
// Match reports the readings that produced the best score (the first such
// pair, in reading order, when several tie).
//
// Edge cases, all symmetric:
//   - either side normalizes to nothing (empty, or only punctuation): 0 —
//     there is nothing to compare, so the check fails closed;
//   - a reading of one side identical to a reading of the other: 1;
//   - different text whose codes are all empty (letters Double Metaphone
//     does not voice, such as "h" or "w" alone): 0, again failing closed —
//     no phonetic evidence is not evidence of a match;
//   - one side's code empty, the other's not: 0 (the distance is the whole
//     other code).
func Compare(a, b string) Match {
	as, bs := encodeReadings(a), encodeReadings(b)
	m := Match{A: as[0], B: bs[0]}
	if m.A.Normalized == "" || m.B.Normalized == "" {
		return m
	}
	for _, pa := range as {
		for _, pb := range bs {
			if pa.Normalized == pb.Normalized {
				return Match{A: pa, B: pb, Score: 1}
			}
		}
	}
	for _, pa := range as {
		for _, pb := range bs {
			for _, ca := range []string{pa.Codes.Primary, pa.Codes.Alternate} {
				for _, cb := range []string{pb.Codes.Primary, pb.Codes.Alternate} {
					if s := codeSimilarity(ca, cb); s > m.Score {
						m = Match{A: pa, B: pb, Score: s}
					}
				}
			}
		}
	}
	return m
}

// encodeReadings encodes every reading of s. The result is never empty.
func encodeReadings(s string) []Phrase {
	rs := Readings(s)
	out := make([]Phrase, len(rs))
	for i, r := range rs {
		n := lettersOnly(r)
		out[i] = Phrase{Reading: r, Normalized: n, Codes: DoubleMetaphone(n, 0), Readings: len(rs)}
	}
	return out
}

// MaxReadings caps how many readings Readings returns for one string. Every
// numeral multiplies the count, so past the cap each numeral falls back to its
// cardinal reading alone and the string has exactly one reading.
const MaxReadings = 8

// Readings returns the spoken readings of s: lower-case words separated by
// single spaces, with each ASCII digit run replaced by one of its
// NumberReadings. The first reading always uses every numeral's cardinal
// form. When the combinations would exceed MaxReadings, only that first
// reading is returned. Text without digits has exactly one reading.
func Readings(s string) []string {
	type part struct {
		text string   // literal letters or a separator
		alts []string // number readings, when a numeral
	}
	var parts []part
	runes := []rune(strings.ToLower(s))
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case isASCIIDigit(r):
			var digits []rune
			grouped := false
			for i < len(runes) {
				if isASCIIDigit(runes[i]) {
					digits = append(digits, runes[i])
					i++
					continue
				}
				// "1,000": a comma between digits is a thousands separator.
				if runes[i] == ',' && i+1 < len(runes) && isASCIIDigit(runes[i+1]) {
					grouped = true
					i++
					continue
				}
				break
			}
			alts := []string{NumberWords(string(digits))}
			if !grouped {
				// A grouped number ("1,984") is written as a quantity; read it
				// as one.
				alts = NumberReadings(string(digits))
			}
			parts = append(parts, part{text: " "}, part{alts: alts}, part{text: " "})
		case unicode.IsLetter(r):
			parts = append(parts, part{text: string(r)})
			i++
		default:
			parts = append(parts, part{text: " "})
			i++
		}
	}

	combos := 1
	for _, p := range parts {
		if len(p.alts) > 0 {
			combos *= len(p.alts)
			if combos > MaxReadings {
				break
			}
		}
	}
	if combos > MaxReadings {
		combos = 1
	}

	out := make([]string, 0, combos)
	seen := make(map[string]bool, combos)
	for k := 0; k < combos; k++ {
		var b strings.Builder
		rest := k
		for _, p := range parts {
			if len(p.alts) == 0 {
				b.WriteString(p.text)
				continue
			}
			n := 1
			if combos > 1 {
				n = len(p.alts)
			}
			b.WriteString(p.alts[rest%n])
			rest /= n
		}
		reading := strings.Join(strings.Fields(b.String()), " ")
		if !seen[reading] {
			seen[reading] = true
			out = append(out, reading)
		}
	}
	return out
}

// Normalize prepares text for phonetic encoding using the cardinal reading of
// every numeral: lower-case, ASCII digit runs spelled out (NumberWords; a
// comma between digits is read as a thousands separator), and every rune that
// is not a letter removed — spaces too. It is lettersOnly(Readings(s)[0]).
func Normalize(s string) string { return lettersOnly(Readings(s)[0]) }

// lettersOnly drops every rune that is not a letter.
func lettersOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
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
