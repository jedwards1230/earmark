package library

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

// ASIN sources, in precedence order (CONTRACT §1.6). Stored verbatim in
// book_metadata.asin_source.
const (
	ASINSourceDir         = "dir"
	ASINSourceFilename    = "filename"
	ASINSourceEmbeddedTag = "embedded_tag"
)

// asinExact matches a whole value that is a catalogue id of the same shapes
// ExtractASIN accepts inside brackets. Used for the embedded tag, which is a
// bare value rather than a bracketed path component.
var asinExact = regexp.MustCompile(`^(?:` + ASINIDPattern + `)$`)

// NormalizeASIN upper-cases and trims an embedded-tag ASIN and returns it if it
// has a catalogue-id shape, else "". Tags are producer-supplied, so anything
// else ("N/A", a URL, a title) is rejected rather than looked up.
func NormalizeASIN(tag string) string {
	v := strings.ToUpper(strings.TrimSpace(tag))
	if asinExact.MatchString(v) {
		return v
	}
	return ""
}

// ResolveASIN picks a file's ASIN by precedence: the book directory, then the
// filename (both bracketed ids, as ExtractASIN parses them), then the ASIN tag
// embedded in the audio (reported by the ASR runner, which runs ffprobe; the
// Go image has none). It returns the ASIN and its source, or ("", "").
//
// The embedded tag is the least trusted source — callers must cross-check the
// record it names against the book's own title (TitlesMatch) before using it.
func ResolveASIN(filePath, embeddedTag string) (asin, source string) {
	if a := ExtractASIN(path.Dir(filePath)); a != "" {
		return a, ASINSourceDir
	}
	if a := ExtractASIN(path.Base(filePath)); a != "" {
		return a, ASINSourceFilename
	}
	if a := NormalizeASIN(embeddedTag); a != "" {
		return a, ASINSourceEmbeddedTag
	}
	return "", ""
}

// titleCoverage is the minimum share of EACH title's identity tokens the other
// title must contain. Two-way coverage is what rejects a series sibling: "Dune"
// is fully covered by "Dune Messiah", but "messiah" is not covered by "Dune".
const titleCoverage = 0.8

// titleStopwords carry no identity: two titles never match on these alone.
var titleStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "and": true, "in": true,
	"on": true, "to": true, "for": true, "book": true, "unabridged": true,
	"abridged": true, "audiobook": true, "novel": true,
}

// seriesGroup matches a parenthesized or bracketed group that is a series or
// position marker — "(The Expanse, Book 1)", "[Dune Chronicles #3]",
// "(Vol. 2)" — and nothing else. Other groups ("(1 of 2)", "[Dramatized
// Adaptation]") are kept: they distinguish products.
var seriesGroup = regexp.MustCompile(`(?i)\s*[(\[][^)\]]*(?:\bbook\b|\bbk\.?\s*\d|\bvol(?:ume|\.)?\s*\d|\bseries\b|#\s*\d|,\s*\d)[^)\]]*[)\]]`)

// seriesMarker matches text that reads as a series position ("Dune
// Chronicles, Book 3", "Expanse #1", "Vol. 2").
var seriesMarker = regexp.MustCompile(`(?i)\bbook\b|\bbk\.?\s*\d|\bvol(?:ume|\.)?\s*\d|\bseries\b|#\s*\d|,\s*\d`)

// titleVariants returns the forms a title is compared in: the whole title with
// series groups removed; the part after the last colon (a series-name prefix:
// "Red Rising: Golden Son" → "Golden Son"); and the part before the first
// colon only when what follows it is a series marker ("Children of Dune: Dune
// Chronicles, Book 3" → "Children of Dune"). A plain subtitle is NOT dropped,
// so "Red Rising" never matches "Red Rising: Golden Son" — the cost is that a
// record with a subtitle the path lacks reads as a conflict, which is the safe
// direction (no ASIN context rather than the wrong one).
func titleVariants(title string) []string {
	t := seriesGroup.ReplaceAllString(StripASIN(title), "")
	out := []string{t}
	if i := strings.Index(t, ":"); i >= 0 {
		after := t[strings.LastIndex(t, ":")+1:]
		out = append(out, after)
		if seriesMarker.MatchString(t[i+1:]) {
			out = append(out, t[:i])
		}
	}
	return out
}

// TitleTokens normalizes a title into its identity tokens: bracketed ids
// stripped, lower-cased, split on anything that is not a letter or digit
// (apostrophes are removed first so "Muad'Dib" stays one token), stopwords
// dropped.
func TitleTokens(title string) map[string]bool {
	t := strings.ToLower(StripASIN(title))
	t = strings.NewReplacer("'", "", "’", "").Replace(t)
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(t, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if !titleStopwords[w] {
			out[w] = true
		}
	}
	return out
}

// coverage is the share of a's tokens found in b (0 when a is empty).
func coverage(a, b map[string]bool) float64 {
	if len(a) == 0 {
		return 0
	}
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return float64(n) / float64(len(a))
}

// without returns tokens minus drop, unless that would leave nothing (a book
// titled after its author keeps its title).
func without(tokens, drop map[string]bool) map[string]bool {
	out := map[string]bool{}
	for w := range tokens {
		if !drop[w] {
			out[w] = true
		}
	}
	if len(out) == 0 {
		return tokens
	}
	return out
}

// titlesMatch compares every variant pair with two-way coverage, after
// removing the authors' name tokens from both titles ("Frank Herbert -
// Children of Dune").
func titlesMatch(a, b string, authorTokens map[string]bool) bool {
	for _, va := range titleVariants(a) {
		ta := without(TitleTokens(va), authorTokens)
		for _, vb := range titleVariants(b) {
			tb := without(TitleTokens(vb), authorTokens)
			if coverage(ta, tb) >= titleCoverage && coverage(tb, ta) >= titleCoverage {
				return true
			}
		}
	}
	return false
}

// TitlesMatch reports whether two titles plausibly name the same book: some
// form of each (series groups removed, either side of a colon) covers at
// least 80% of the other's identity tokens, in both directions.
func TitlesMatch(a, b string) bool {
	return titlesMatch(a, b, nil)
}

// BookRef is the identity a title cross-check compares: a title and its
// author ("" when unknown).
type BookRef struct {
	Title  string
	Author string
}

// SameBook is the cross-check that guards the embedded ASIN tag (CONTRACT
// §1.6): the titles must match (TitlesMatch, after removing author names) and,
// when both authors are known, the authors must share a name token (so
// "Frank Herbert" matches "Frank Herbert, Brian Herbert" but not "Andy Weir").
func SameBook(record, own BookRef) bool {
	ra, oa := TitleTokens(record.Author), TitleTokens(own.Author)
	if len(ra) > 0 && len(oa) > 0 && coverage(oa, ra) == 0 {
		return false
	}
	authors := map[string]bool{}
	for w := range ra {
		authors[w] = true
	}
	for w := range oa {
		authors[w] = true
	}
	return titlesMatch(record.Title, own.Title, authors)
}
