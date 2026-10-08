package phonetic

import (
	"strings"
)

var (
	smallNumbers = [...]string{
		"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
		"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen",
		"seventeen", "eighteen", "nineteen",
	}
	tens   = [...]string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
	scales = [...]string{"", "thousand", "million", "billion", "trillion", "quadrillion", "quintillion"}
)

// maxCardinalDigits is the longest digit run read as one cardinal number.
// 18 digits always fit a uint64 and stay below the quintillion scale's end;
// a longer run (a phone number, an ISBN) is read digit by digit, which is also
// how it would be spoken.
const maxCardinalDigits = 18

// NumberWords spells a run of ASCII digits as English words, the way the
// number would usually be read aloud as a cardinal: "240" → "two hundred
// forty", "1000000" → "one million", "0" → "zero". There is no "and".
//
// A run with a leading zero ("007") or longer than 18 digits is read digit by
// digit ("zero zero seven"). Non-digit input returns "".
func NumberWords(digits string) string {
	if digits == "" {
		return ""
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return ""
		}
	}
	if (len(digits) > 1 && digits[0] == '0') || len(digits) > maxCardinalDigits {
		return digitWords(digits)
	}
	var n uint64
	for i := 0; i < len(digits); i++ {
		n = n*10 + uint64(digits[i]-'0')
	}
	if n == 0 {
		return smallNumbers[0]
	}

	// Split into groups of three from the right and name each non-zero group
	// with its scale.
	var groups []string
	for scale := 0; n > 0; scale++ {
		g := n % 1000
		n /= 1000
		if g == 0 {
			continue
		}
		w := threeDigitWords(g)
		if scales[scale] != "" {
			w += " " + scales[scale]
		}
		groups = append(groups, w)
	}
	for i, j := 0, len(groups)-1; i < j; i, j = i+1, j-1 {
		groups[i], groups[j] = groups[j], groups[i]
	}
	return strings.Join(groups, " ")
}

// threeDigitWords spells 1..999.
func threeDigitWords(n uint64) string {
	var parts []string
	if h := n / 100; h > 0 {
		parts = append(parts, smallNumbers[h], "hundred")
	}
	switch r := n % 100; {
	case r == 0:
	case r < 20:
		parts = append(parts, smallNumbers[r])
	default:
		parts = append(parts, tens[r/10])
		if r%10 != 0 {
			parts = append(parts, smallNumbers[r%10])
		}
	}
	return strings.Join(parts, " ")
}

// NumberReadings returns the common spoken readings of a run of ASCII digits,
// cardinal first, without duplicates:
//
//   - cardinal — NumberWords ("240" → "two hundred forty");
//   - paired, for 3- and 4-digit runs without a leading zero — the way
//     prices, times, room numbers and years are said: "240" → "two forty",
//     "205" → "two oh five", "1984" → "nineteen eighty four", "2005" →
//     "twenty oh five", "1900" → "nineteen hundred". A round thousand
//     ("2000") has no paired form; its cardinal is how it is said;
//   - digit by digit, for runs of two or more digits — "240" → "two four
//     zero".
//
// Non-digit input returns nil.
func NumberReadings(digits string) []string {
	cardinal := NumberWords(digits)
	if cardinal == "" {
		return nil
	}
	out := []string{cardinal}
	add := func(r string) {
		for _, have := range out {
			if have == r {
				return
			}
		}
		out = append(out, r)
	}
	if p := pairedWords(digits); p != "" {
		add(p)
	}
	if len(digits) > 1 {
		add(digitWords(digits))
	}
	return out
}

// digitWords reads ASCII digits one by one: "240" → "two four zero".
func digitWords(digits string) string {
	words := make([]string, 0, len(digits))
	for i := 0; i < len(digits); i++ {
		words = append(words, smallNumbers[digits[i]-'0'])
	}
	return strings.Join(words, " ")
}

// pairedWords is the paired reading of a 3- or 4-digit run, or "" when it has
// none. The run is split into a head (all but the last two digits) and a
// two-digit tail: "1984" → "19" + "84", "240" → "2" + "40".
func pairedWords(digits string) string {
	if (len(digits) != 3 && len(digits) != 4) || digits[0] == '0' {
		return ""
	}
	head, tail := digits[:len(digits)-2], digits[len(digits)-2:]
	headWords := NumberWords(head)
	switch {
	case tail == "00":
		if len(head) == 2 && head[1] == '0' {
			return "" // "2000": "twenty hundred" is not how it is said
		}
		return headWords + " hundred"
	case tail[0] == '0':
		return headWords + " oh " + smallNumbers[tail[1]-'0']
	default:
		return headWords + " " + NumberWords(tail)
	}
}
