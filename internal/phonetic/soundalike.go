package phonetic

import (
	"strings"
	"unicode"
	"unicode/utf8"
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

// MaxPhraseRunes caps the length of each side Compare will score. Longer
// input scores 0 (fails closed) instead of paying for a quadratic edit
// distance over an unbounded string. With MaxReadings readings per side the
// worst case (adversarial strings of numerals) is a few hundred milliseconds;
// ordinary phrases take microseconds. internal/decide caps its substitution
// window at the same length and rejects longer windows with evidence.
const MaxPhraseRunes = 80

// Compare scores how alike two phrases sound.
//
// Each side is expanded into its spoken readings (Readings): lower-cased,
// each numeral spelled out as words in every way it is commonly said
// ("240" → "two hundred forty", "two forty", "two four zero"; "3.5" → "three
// point five"), everything but letters dropped, spaces included — so splits
// and fusions ("auto sebo" vs "arecibo", "placenes" vs "place names") compare
// as one word. Each reading is encoded with Double Metaphone at unbounded
// length, and the score is the best, over every pair of readings and the four
// {primary, alternate} code pairings, of
// 1 − Levenshtein(codeA, codeB) / max(len(codeA), len(codeB)).
// Match reports the readings that produced the best score (the first such
// pair, in reading order, when several tie).
//
// Because readings are encoded letters-only (that is what makes fusions
// match), the Double Metaphone rules that look at a space — "VAN "/"VON "
// germanic, "SAN " Spanish J, Mac " C"/" G"/" Q" — never fire through
// Compare. That is deliberate; call DoubleMetaphone directly to get them.
//
// Cost is bounded: at most MaxPhraseRunes runes per side, at most
// MaxReadings readings per side, so at most 4·MaxReadings² edit distances —
// and pairs whose length difference alone rules out beating the current best
// are skipped.
//
// Edge cases, all symmetric:
//   - either side normalizes to nothing (empty, or only punctuation), or is
//     longer than MaxPhraseRunes: 0 — the check fails closed;
//   - a reading of one side identical to a reading of the other: 1;
//   - different text whose codes are all empty (letters Double Metaphone
//     does not voice, such as "h" or "w" alone): 0, again failing closed —
//     no phonetic evidence is not evidence of a match;
//   - one side's code empty, the other's not: 0 (the distance is the whole
//     other code).
func Compare(a, b string) Match {
	if utf8.RuneCountInString(a) > MaxPhraseRunes || utf8.RuneCountInString(b) > MaxPhraseRunes {
		return Match{}
	}
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
	// Each distinct pair of codes is scored once: readings often share codes
	// (and primary often equals alternate). Iteration stays in reading order,
	// so the reported pair is still the first best one.
	scored := make(map[[2]string]bool)
	for _, pa := range as {
		for _, pb := range bs {
			for _, ca := range []string{pa.Codes.Primary, pa.Codes.Alternate} {
				for _, cb := range []string{pb.Codes.Primary, pb.Codes.Alternate} {
					key := [2]string{ca, cb}
					if scored[key] || similarityBound(ca, cb) <= m.Score {
						continue
					}
					scored[key] = true
					if s := codeSimilarity(ca, cb); s > m.Score {
						m = Match{A: pa, B: pb, Score: s}
					}
				}
			}
		}
	}
	return m
}

// SameReading reports whether some reading of a and some reading of b
// normalize to the same letters — the two say the same thing aloud, e.g.
// "10,000" and "10 thousand", or "1984" and "nineteen eighty four". Inputs
// longer than MaxPhraseRunes, or that normalize to nothing, never match.
func SameReading(a, b string) bool {
	if utf8.RuneCountInString(a) > MaxPhraseRunes || utf8.RuneCountInString(b) > MaxPhraseRunes {
		return false
	}
	as, bs := Readings(a), Readings(b)
	for _, ra := range as {
		na := lettersOnly(ra)
		if na == "" {
			continue
		}
		for _, rb := range bs {
			if na == lettersOnly(rb) {
				return true
			}
		}
	}
	return false
}

// similarityBound is an upper bound on codeSimilarity: the edit distance is at
// least the length difference.
func similarityBound(a, b string) float64 {
	la, lb := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
	longest := max(la, lb)
	if longest == 0 {
		return 0
	}
	diff := la - lb
	if diff < 0 {
		diff = -diff
	}
	return 1 - float64(diff)/float64(longest)
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

// MaxReadings caps how many readings Readings returns for one string: 27 is
// every combination of three numerals with three readings each. Numerals are
// expanded in order while the running product of reading counts stays within
// the cap; every later numeral is pinned to its cardinal reading.
const MaxReadings = 27

// Readings returns the spoken readings of s: lower-case words separated by
// single spaces, with each numeral replaced by one of its readings.
//
// A numeral is an ASCII digit run, optionally with thousands groups ("1,000"
// — a comma counts as a separator only when it is followed by exactly three
// digits, after a leading group of one to three, so "1,2,3" is three
// numerals) and a decimal part ("3.5" → "three point five", the fraction read
// digit by digit). A plain numeral reads as NumberReadings; a grouped one is
// written as a quantity and reads as its cardinal only.
//
// The first reading always uses every numeral's cardinal form. At most
// MaxReadings readings are returned (see MaxReadings for how numerals degrade
// past it); input longer than MaxPhraseRunes gets the cardinal reading only.
// Text without digits has exactly one reading.
func Readings(s string) []string {
	type part struct {
		text string   // literal letters or a separator
		alts []string // number readings, when a numeral
	}
	var parts []part
	runes := []rune(strings.ToLower(s))
	cardinalOnly := len(runes) > MaxPhraseRunes
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case isASCIIDigit(r):
			num, _ := ScanNumeral(runes, i) // r is a digit
			i = num.End
			whole, grouped, frac := num.Whole, num.Grouped, num.Fraction

			alts := []string{NumberWords(whole)}
			if !grouped && !cardinalOnly {
				alts = NumberReadings(whole)
			}
			if frac != "" {
				for n := range alts {
					alts[n] += " point " + digitWords(frac)
				}
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

	// Expand numerals in order while the product fits; pin the rest to the
	// cardinal.
	combos := 1
	for n := range parts {
		if len(parts[n].alts) == 0 {
			continue
		}
		if combos*len(parts[n].alts) > MaxReadings {
			parts[n].alts = parts[n].alts[:1]
			continue
		}
		combos *= len(parts[n].alts)
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
			b.WriteString(p.alts[rest%len(p.alts)])
			rest /= len(p.alts)
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
// every numeral (see Readings): lower-case, numerals spelled out, and every
// rune that is not a letter removed — spaces too. It is
// lettersOnly(Readings(s)[0]).
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

// Numeral is one numeral scanned from text by ScanNumeral.
type Numeral struct {
	// Whole is the integer digits with any thousands separators removed.
	Whole string
	// Grouped reports whether Whole was written with thousands separators.
	Grouped bool
	// Fraction is the digits after a decimal point, "" when there is none.
	Fraction string
	// End is the rune index just past the numeral.
	End int
}

// ScanNumeral reads the numeral starting at runes[i]. It reports false, with
// a zero Numeral, when i is out of range or runes[i] is not an ASCII digit.
// The grammar, shared by Readings and internal/decide's tokenizer so
// the two always agree on what one number is:
//
//	numeral  = digits [ groups ] [ "." digits ]
//	groups   = ( "," d d d )+   only after 1-3 leading digits, and each
//	                            group must not be followed by a 4th digit
//
// So "1,000", "12,345.67" and "3.5" are one numeral each, while "1,2,3",
// "1,0000" and "1234,567" are not grouped (the comma ends the numeral). A
// trailing "." not followed by a digit is not part of the numeral.
func ScanNumeral(runes []rune, i int) (Numeral, bool) {
	if i < 0 || i >= len(runes) || !isASCIIDigit(runes[i]) {
		return Numeral{}, false
	}
	digitsAt := func(from, n int) bool {
		if from+n > len(runes) {
			return false
		}
		for _, r := range runes[from : from+n] {
			if !isASCIIDigit(r) {
				return false
			}
		}
		return true
	}
	j := i
	for j < len(runes) && isASCIIDigit(runes[j]) {
		j++
	}
	n := Numeral{Whole: string(runes[i:j])}
	if j-i <= 3 {
		for j < len(runes) && runes[j] == ',' && digitsAt(j+1, 3) && !digitsAt(j+4, 1) {
			n.Whole += string(runes[j+1 : j+4])
			j += 4
			n.Grouped = true
		}
	}
	if j+1 < len(runes) && runes[j] == '.' && isASCIIDigit(runes[j+1]) {
		k := j + 1
		for k < len(runes) && isASCIIDigit(runes[k]) {
			k++
		}
		n.Fraction = string(runes[j+1 : k])
		j = k
	}
	n.End = j
	return n, true
}

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
