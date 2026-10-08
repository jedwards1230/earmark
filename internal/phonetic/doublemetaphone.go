// Derived from Apache Commons Codec
// org.apache.commons.codec.language.DoubleMetaphone
// (https://github.com/apache/commons-codec, commit
// d7bff678de1efdf767f713c5a178ba9dd9e1562d), which implements the Double
// Metaphone algorithm by Lawrence Philips.
//
// Original work Copyright The Apache Software Foundation.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. A copy is included at
// internal/phonetic/LICENSE-APACHE-2.0.txt and at
// https://www.apache.org/licenses/LICENSE-2.0. Unless required by applicable
// law or agreed to in writing, software distributed under the License is
// distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the specific language
// governing permissions and limitations under the License. See also the
// repository NOTICE file.
//
// Modifications for earmark: translated from Java to Go; operates on runes;
// the maximum code length is a parameter and a non-positive value means
// unbounded; blank input yields empty codes instead of null; the
// isDoubleMetaphoneEqual and StringEncoder APIs are not ported.

package phonetic

import (
	"strings"
)

// DefaultMaxCodeLen is commons-codec's default maximum code length. Name
// matching wants short codes; whole phrases want MaxCodeLen <= 0 (unbounded).
const DefaultMaxCodeLen = 4

// Codes is a Double Metaphone encoding: the primary code and the alternate
// one (equal to the primary when the word has a single plausible reading).
type Codes struct {
	Primary   string
	Alternate string
}

// DoubleMetaphone encodes s with the Double Metaphone algorithm, truncating
// each code to maxCodeLen runes; maxCodeLen <= 0 means unbounded.
//
// The behaviour matches commons-codec's DoubleMetaphone for any positive
// maxCodeLen (its golden vectors are this package's tests), except that blank
// input returns empty Codes where commons-codec returns null. Non-letters,
// including spaces, are skipped, though a space still counts as a neighbour
// for the rules that look for one ("VAN ", "SAN ").
func DoubleMetaphone(s string, maxCodeLen int) Codes {
	value := cleanInput(s)
	if len(value) == 0 {
		return Codes{}
	}
	e := encoder{value: value, slavoGermanic: isSlavoGermanic(string(value))}
	res := dmResult{max: maxCodeLen}

	index := 0
	if isSilentStart(string(value)) {
		index = 1
	}
	for !res.isComplete() && index <= len(value)-1 {
		switch value[index] {
		case 'A', 'E', 'I', 'O', 'U', 'Y':
			index = e.handleAEIOUY(&res, index)
		case 'B':
			res.add("P")
			index = e.skipIfNext(index, 'B')
		case 'Ç':
			// C with a cedilla.
			res.add("S")
			index++
		case 'C':
			index = e.handleC(&res, index)
		case 'D':
			index = e.handleD(&res, index)
		case 'F':
			res.add("F")
			index = e.skipIfNext(index, 'F')
		case 'G':
			index = e.handleG(&res, index)
		case 'H':
			index = e.handleH(&res, index)
		case 'J':
			index = e.handleJ(&res, index)
		case 'K':
			res.add("K")
			index = e.skipIfNext(index, 'K')
		case 'L':
			index = e.handleL(&res, index)
		case 'M':
			res.add("M")
			if e.conditionM0(index) {
				index += 2
			} else {
				index++
			}
		case 'N':
			res.add("N")
			index = e.skipIfNext(index, 'N')
		case 'Ñ':
			// N with a tilde (Spanish ene).
			res.add("N")
			index++
		case 'P':
			index = e.handleP(&res, index)
		case 'Q':
			res.add("K")
			index = e.skipIfNext(index, 'Q')
		case 'R':
			index = e.handleR(&res, index)
		case 'S':
			index = e.handleS(&res, index)
		case 'T':
			index = e.handleT(&res, index)
		case 'V':
			res.add("F")
			index = e.skipIfNext(index, 'V')
		case 'W':
			index = e.handleW(&res, index)
		case 'X':
			index = e.handleX(&res, index)
		case 'Z':
			index = e.handleZ(&res, index)
		default:
			index++
		}
	}
	return Codes{Primary: string(res.primary), Alternate: string(res.alternate)}
}

// dmResult accumulates the two codes, honouring the maximum length
// (DoubleMetaphoneResult in commons-codec). max <= 0 means unbounded.
type dmResult struct {
	primary   []rune
	alternate []rune
	max       int
}

func (r *dmResult) add(v string)          { r.addPrimary(v); r.addAlternate(v) }
func (r *dmResult) addPair(p, a string)   { r.addPrimary(p); r.addAlternate(a) }
func (r *dmResult) addPrimary(v string)   { r.primary = appendBounded(r.primary, v, r.max) }
func (r *dmResult) addAlternate(v string) { r.alternate = appendBounded(r.alternate, v, r.max) }
func appendBounded(dst []rune, v string, limit int) []rune {
	for _, c := range v {
		if limit > 0 && len(dst) >= limit {
			break
		}
		dst = append(dst, c)
	}
	return dst
}

func (r *dmResult) isComplete() bool {
	return r.max > 0 && len(r.primary) >= r.max && len(r.alternate) >= r.max
}

// encoder carries the cleaned input through the per-letter handlers.
type encoder struct {
	value         []rune
	slavoGermanic bool
}

// cleanInput mirrors commons-codec: Java String.trim (strip runes <= ' ' at
// both ends), then upper-case. Java's toUpperCase(Locale.ENGLISH) expands
// 'ß' to "SS" where Go's ToUpper leaves it alone, so that one case is mapped
// explicitly.
func cleanInput(s string) []rune {
	s = strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(strings.ToUpper(s), "ß", "SS")
	return []rune(s)
}

// charAt returns the rune at index, or 0 when out of bounds
// (Character.MIN_VALUE in commons-codec).
func (e *encoder) charAt(index int) rune {
	if index < 0 || index >= len(e.value) {
		return 0
	}
	return e.value[index]
}

// contains reports whether the length-rune window at start equals any of
// criteria. An out-of-range window never matches.
func (e *encoder) contains(start, length int, criteria ...string) bool {
	if start < 0 || start+length > len(e.value) {
		return false
	}
	target := string(e.value[start : start+length])
	for _, c := range criteria {
		if target == c {
			return true
		}
	}
	return false
}

// skipIfNext advances past a doubled letter ("BB", "FF", ...).
func (e *encoder) skipIfNext(index int, r rune) int {
	if e.charAt(index+1) == r {
		return index + 2
	}
	return index + 1
}

func isVowel(r rune) bool { return strings.ContainsRune("AEIOUY", r) }

// isSilentStart: a leading GN, KN, PN, WR or PS is not pronounced.
func isSilentStart(v string) bool {
	for _, p := range []string{"GN", "KN", "PN", "WR", "PS"} {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// isSlavoGermanic: the value contains W, K, CZ or WITZ.
func isSlavoGermanic(v string) bool {
	return strings.ContainsAny(v, "WK") || strings.Contains(v, "CZ") || strings.Contains(v, "WITZ")
}

var (
	lrnmbhfvwSpace       = []string{"L", "R", "N", "M", "B", "H", "F", "V", "W", " "}
	esEpEbElEyIbIlInIeEi = []string{"ES", "EP", "EB", "EL", "EY", "IB", "IL", "IN", "IE", "EI", "ER"}
	ltksnmbz             = []string{"L", "T", "K", "S", "N", "M", "B", "Z"}
)

// conditionC0 is commons-codec's "complex condition 0 for 'C'".
func (e *encoder) conditionC0(index int) bool {
	if e.contains(index, 4, "CHIA") {
		return true
	}
	if index <= 1 {
		return false
	}
	if isVowel(e.charAt(index - 2)) {
		return false
	}
	if !e.contains(index-1, 3, "ACH") {
		return false
	}
	c := e.charAt(index + 2)
	return c != 'I' && c != 'E' || e.contains(index-2, 6, "BACHER", "MACHER")
}

// conditionCH0 — Greek roots ("chemistry", "chorus").
func (e *encoder) conditionCH0(index int) bool {
	if index != 0 {
		return false
	}
	if !e.contains(index+1, 5, "HARAC", "HARIS") &&
		!e.contains(index+1, 3, "HOR", "HYM", "HIA", "HEM") {
		return false
	}
	return !e.contains(0, 5, "CHORE")
}

// conditionCH1 — Germanic, Greek, or otherwise 'ch' for the 'kh' sound.
func (e *encoder) conditionCH1(index int) bool {
	return e.contains(0, 4, "VAN ", "VON ") || e.contains(0, 3, "SCH") ||
		e.contains(index-2, 6, "ORCHES", "ARCHIT", "ORCHID") ||
		e.contains(index+2, 1, "T", "S") ||
		(e.contains(index-1, 1, "A", "O", "U", "E") || index == 0) &&
			(e.contains(index+2, 1, lrnmbhfvwSpace...) || index+1 == len(e.value)-1)
}

// conditionL0 — Spanish "-illo", "-illa", "-alle".
func (e *encoder) conditionL0(index int) bool {
	n := len(e.value)
	if index == n-3 && e.contains(index-1, 4, "ILLO", "ILLA", "ALLE") {
		return true
	}
	return (e.contains(n-2, 2, "AS", "OS") || e.contains(n-1, 1, "A", "O")) &&
		e.contains(index-1, 4, "ALLE")
}

// conditionM0 — "MM", or "UMB" at the end / before "ER" ("dumb", "thumb").
func (e *encoder) conditionM0(index int) bool {
	if e.charAt(index+1) == 'M' {
		return true
	}
	return e.contains(index-1, 3, "UMB") &&
		(index+1 == len(e.value)-1 || e.contains(index+2, 2, "ER"))
}

func (e *encoder) handleAEIOUY(r *dmResult, index int) int {
	if index == 0 {
		r.add("A")
	}
	return index + 1
}

func (e *encoder) handleC(r *dmResult, index int) int {
	switch {
	case e.conditionC0(index):
		r.add("K")
		index += 2
	case index == 0 && e.contains(index, 6, "CAESAR"):
		r.add("S")
		index += 2
	case e.contains(index, 2, "CH"):
		index = e.handleCH(r, index)
	case e.contains(index, 2, "CZ") && !e.contains(index-2, 4, "WICZ"):
		// "Czerny"
		r.addPair("S", "X")
		index += 2
	case e.contains(index+1, 3, "CIA"):
		// "focaccia"
		r.add("X")
		index += 3
	case e.contains(index, 2, "CC") && (index != 1 || e.charAt(0) != 'M'):
		// double "cc" but not "McClelland"
		return e.handleCC(r, index)
	case e.contains(index, 2, "CK", "CG", "CQ"):
		r.add("K")
		index += 2
	case e.contains(index, 2, "CI", "CE", "CY"):
		// Italian vs. English
		if e.contains(index, 3, "CIO", "CIE", "CIA") {
			r.addPair("S", "X")
		} else {
			r.add("S")
		}
		index += 2
	default:
		r.add("K")
		switch {
		case e.contains(index+1, 2, " C", " Q", " G"):
			// Mac Caffrey, Mac Gregor
			index += 3
		case e.contains(index+1, 1, "C", "K", "Q") && !e.contains(index+1, 2, "CE", "CI"):
			index += 2
		default:
			index++
		}
	}
	return index
}

func (e *encoder) handleCC(r *dmResult, index int) int {
	if e.contains(index+2, 1, "I", "E", "H") && !e.contains(index+2, 2, "HU") {
		// "bellocchio" but not "bacchus"
		if index == 1 && e.charAt(index-1) == 'A' || e.contains(index-1, 5, "UCCEE", "UCCES") {
			// "accident", "accede", "succeed"
			r.add("KS")
		} else {
			// "bacci", "bertucci", other Italian
			r.add("X")
		}
		return index + 3
	}
	// Pierce's rule
	r.add("K")
	return index + 2
}

func (e *encoder) handleCH(r *dmResult, index int) int {
	if index > 0 && e.contains(index, 4, "CHAE") {
		// Michael
		r.addPair("K", "X")
		return index + 2
	}
	if e.conditionCH0(index) || e.conditionCH1(index) {
		r.add("K")
		return index + 2
	}
	switch {
	case index > 0 && e.contains(0, 2, "MC"):
		r.add("K")
	case index > 0:
		r.addPair("X", "K")
	default:
		r.add("X")
	}
	return index + 2
}

func (e *encoder) handleD(r *dmResult, index int) int {
	switch {
	case e.contains(index, 2, "DG"):
		if e.contains(index+2, 1, "I", "E", "Y") {
			// "Edge"
			r.add("J")
			return index + 3
		}
		// "Edgar"
		r.add("TK")
		return index + 2
	case e.contains(index, 2, "DT", "DD"):
		r.add("T")
		return index + 2
	default:
		r.add("T")
		return index + 1
	}
}

func (e *encoder) handleG(r *dmResult, index int) int {
	sg := e.slavoGermanic
	switch {
	case e.charAt(index+1) == 'H':
		return e.handleGH(r, index)
	case e.charAt(index+1) == 'N':
		switch {
		case index == 1 && isVowel(e.charAt(0)) && !sg:
			r.addPair("KN", "N")
		case !e.contains(index+2, 2, "EY") && e.charAt(index+1) != 'Y' && !sg:
			r.addPair("N", "KN")
		default:
			r.add("KN")
		}
		return index + 2
	case e.contains(index+1, 2, "LI") && !sg:
		r.addPair("KL", "L")
		return index + 2
	case index == 0 && (e.charAt(index+1) == 'Y' || e.contains(index+1, 2, esEpEbElEyIbIlInIeEi...)):
		// -ges-, -gep-, -gel-, -gie- at beginning
		r.addPair("K", "J")
		return index + 2
	case (e.contains(index+1, 2, "ER") || e.charAt(index+1) == 'Y') &&
		!e.contains(0, 6, "DANGER", "RANGER", "MANGER") &&
		!e.contains(index-1, 1, "E", "I") &&
		!e.contains(index-1, 3, "RGY", "OGY"):
		// -ger-, -gy-
		r.addPair("K", "J")
		return index + 2
	case e.contains(index+1, 1, "E", "I", "Y") || e.contains(index-1, 4, "AGGI", "OGGI"):
		// Italian "biaggi"
		switch {
		case e.contains(0, 4, "VAN ", "VON ") || e.contains(0, 3, "SCH") || e.contains(index+1, 2, "ET"):
			// obvious germanic
			r.add("K")
		case e.contains(index+1, 3, "IER"):
			r.add("J")
		default:
			r.addPair("J", "K")
		}
		return index + 2
	default:
		r.add("K")
		return e.skipIfNext(index, 'G')
	}
}

func (e *encoder) handleGH(r *dmResult, index int) int {
	switch {
	case index > 0 && !isVowel(e.charAt(index-1)):
		r.add("K")
	case index == 0:
		if e.charAt(index+2) == 'I' {
			r.add("J")
		} else {
			r.add("K")
		}
	case index > 1 && e.contains(index-2, 1, "B", "H", "D") ||
		index > 2 && e.contains(index-3, 1, "B", "H", "D") ||
		index > 3 && e.contains(index-4, 1, "B", "H"):
		// Parker's rule (with some further refinements) - "hugh"
	default:
		if index > 2 && e.charAt(index-1) == 'U' && e.contains(index-3, 1, "C", "G", "L", "R", "T") {
			// "laugh", "McLaughlin", "cough", "gough", "rough", "tough"
			r.add("F")
		} else if index > 0 && e.charAt(index-1) != 'I' {
			r.add("K")
		}
	}
	return index + 2
}

func (e *encoder) handleH(r *dmResult, index int) int {
	// Only keep if first & before vowel or between 2 vowels; also takes care
	// of "HH".
	if (index == 0 || isVowel(e.charAt(index-1))) && isVowel(e.charAt(index+1)) {
		r.add("H")
		return index + 2
	}
	return index + 1
}

func (e *encoder) handleJ(r *dmResult, index int) int {
	if e.contains(index, 4, "JOSE") || e.contains(0, 4, "SAN ") {
		// obvious Spanish, "Jose", "San Jacinto"
		if index == 0 && e.charAt(index+4) == ' ' || len(e.value) == 4 || e.contains(0, 4, "SAN ") {
			r.add("H")
		} else {
			r.addPair("J", "H")
		}
		return index + 1
	}
	switch {
	case index == 0 && !e.contains(index, 4, "JOSE"):
		r.addPair("J", "A")
	case isVowel(e.charAt(index-1)) && !e.slavoGermanic &&
		(e.charAt(index+1) == 'A' || e.charAt(index+1) == 'O'):
		r.addPair("J", "H")
	case index == len(e.value)-1:
		// commons-codec appends a literal space to the alternate here.
		r.addPair("J", " ")
	case !e.contains(index+1, 1, ltksnmbz...) && !e.contains(index-1, 1, "S", "K", "L"):
		r.add("J")
	}
	return e.skipIfNext(index, 'J')
}

func (e *encoder) handleL(r *dmResult, index int) int {
	if e.charAt(index+1) == 'L' {
		if e.conditionL0(index) {
			r.addPrimary("L")
		} else {
			r.add("L")
		}
		return index + 2
	}
	r.add("L")
	return index + 1
}

func (e *encoder) handleP(r *dmResult, index int) int {
	if e.charAt(index+1) == 'H' {
		r.add("F")
		return index + 2
	}
	r.add("P")
	if e.contains(index+1, 1, "P", "B") {
		return index + 2
	}
	return index + 1
}

func (e *encoder) handleR(r *dmResult, index int) int {
	if index == len(e.value)-1 && !e.slavoGermanic &&
		e.contains(index-2, 2, "IE") && !e.contains(index-4, 2, "ME", "MA") {
		r.addAlternate("R")
	} else {
		r.add("R")
	}
	return e.skipIfNext(index, 'R')
}

func (e *encoder) handleS(r *dmResult, index int) int {
	switch {
	case e.contains(index-1, 3, "ISL", "YSL"):
		// special cases "island", "isle", "carlisle", "carlysle"
		return index + 1
	case index == 0 && e.contains(index, 5, "SUGAR"):
		// special case "sugar-"
		r.addPair("X", "S")
		return index + 1
	case e.contains(index, 2, "SH"):
		if e.contains(index+1, 4, "HEIM", "HOEK", "HOLM", "HOLZ") {
			// germanic
			r.add("S")
		} else {
			r.add("X")
		}
		return index + 2
	case e.contains(index, 3, "SIO", "SIA") || e.contains(index, 4, "SIAN"):
		// Italian and Armenian
		if e.slavoGermanic {
			r.add("S")
		} else {
			r.addPair("S", "X")
		}
		return index + 3
	case index == 0 && e.contains(index+1, 1, "M", "N", "L", "W") || e.contains(index+1, 1, "Z"):
		// german & anglicisations, e.g. "smith" match "schmidt", "snider"
		// match "schneider"; also -sz- in slavic languages
		r.addPair("S", "X")
		if e.contains(index+1, 1, "Z") {
			return index + 2
		}
		return index + 1
	case e.contains(index, 2, "SC"):
		return e.handleSC(r, index)
	default:
		if index == len(e.value)-1 && e.contains(index-2, 2, "AI", "OI") {
			// french, e.g. "resnais", "artois"
			r.addAlternate("S")
		} else {
			r.add("S")
		}
		if e.contains(index+1, 1, "S", "Z") {
			return index + 2
		}
		return index + 1
	}
}

func (e *encoder) handleSC(r *dmResult, index int) int {
	switch {
	case e.charAt(index+2) == 'H':
		// Schlesinger's rule
		switch {
		case e.contains(index+3, 2, "OO", "ER", "EN", "UY", "ED", "EM"):
			// Dutch origin, e.g. "school", "schooner"
			if e.contains(index+3, 2, "ER", "EN") {
				// "schermerhorn", "schenker"
				r.addPair("X", "SK")
			} else {
				r.add("SK")
			}
		case index == 0 && !isVowel(e.charAt(3)) && e.charAt(3) != 'W':
			r.addPair("X", "S")
		default:
			r.add("X")
		}
	case e.contains(index+2, 1, "I", "E", "Y"):
		r.add("S")
	default:
		r.add("SK")
	}
	return index + 3
}

func (e *encoder) handleT(r *dmResult, index int) int {
	switch {
	case e.contains(index, 4, "TION") || e.contains(index, 3, "TIA", "TCH"):
		r.add("X")
		return index + 3
	case e.contains(index, 2, "TH") || e.contains(index, 3, "TTH"):
		if e.contains(index+2, 2, "OM", "AM") ||
			// special case "thomas", "thames" or germanic
			e.contains(0, 4, "VAN ", "VON ") || e.contains(0, 3, "SCH") {
			r.add("T")
		} else {
			r.addPair("0", "T")
		}
		return index + 2
	default:
		r.add("T")
		if e.contains(index+1, 1, "T", "D") {
			return index + 2
		}
		return index + 1
	}
}

func (e *encoder) handleW(r *dmResult, index int) int {
	switch {
	case e.contains(index, 2, "WR"):
		// can also be in middle of word
		r.add("R")
		return index + 2
	case index == 0 && (isVowel(e.charAt(index+1)) || e.contains(index, 2, "WH")):
		if isVowel(e.charAt(index + 1)) {
			// Wasserman should match Vasserman
			r.addPair("A", "F")
		} else {
			// need Uomo to match Womo
			r.add("A")
		}
		return index + 1
	case index == len(e.value)-1 && isVowel(e.charAt(index-1)) ||
		e.contains(index-1, 5, "EWSKI", "EWSKY", "OWSKI", "OWSKY") ||
		e.contains(0, 3, "SCH"):
		// Arnow should match Arnoff
		r.addAlternate("F")
		return index + 1
	case e.contains(index, 4, "WICZ", "WITZ"):
		// Polish, e.g. "filipowicz"
		r.addPair("TS", "FX")
		return index + 4
	default:
		return index + 1
	}
}

func (e *encoder) handleX(r *dmResult, index int) int {
	if index == 0 {
		r.add("S")
		return index + 1
	}
	frenchSilentX := index == len(e.value)-1 &&
		(e.contains(index-3, 3, "IAU", "EAU") || e.contains(index-2, 2, "AU", "OU"))
	if !frenchSilentX {
		// Voiced unless word-final after French AU/EAU/OU (e.g. breaux).
		r.add("KS")
	}
	if e.contains(index+1, 1, "C", "X") {
		return index + 2
	}
	return index + 1
}

func (e *encoder) handleZ(r *dmResult, index int) int {
	if e.charAt(index+1) == 'H' {
		// Chinese pinyin, e.g. "zhao", "Zhang"
		r.add("J")
		return index + 2
	}
	if e.contains(index+1, 2, "ZO", "ZI", "ZA") ||
		e.slavoGermanic && index > 0 && e.charAt(index-1) != 'T' {
		r.addPair("S", "TS")
	} else {
		r.add("S")
	}
	return e.skipIfNext(index, 'Z')
}
