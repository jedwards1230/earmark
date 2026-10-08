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
		words := make([]string, 0, len(digits))
		for i := 0; i < len(digits); i++ {
			words = append(words, smallNumbers[digits[i]-'0'])
		}
		return strings.Join(words, " ")
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
