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
var asinExact = regexp.MustCompile(`^(?:` + asinIDPattern + `)$`)

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

// titleMatchThreshold is the minimum token overlap for two titles to name the
// same book. Measured against the smaller token set, so a catalogue title with
// a subtitle or series suffix ("Children of Dune: Dune Chronicles, Book 3")
// still matches the directory's "Children of Dune", while an unrelated book
// sharing one common word does not.
const titleMatchThreshold = 0.6

// titleStopwords carry no identity: two titles never match on these alone.
var titleStopwords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "and": true, "in": true,
	"on": true, "to": true, "for": true, "book": true, "unabridged": true,
	"abridged": true, "audiobook": true, "novel": true,
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

// TitleOverlap is |A∩B| / min(|A|,|B|) over the two titles' TitleTokens; 0
// when either has no tokens.
func TitleOverlap(a, b string) float64 {
	ta, tb := TitleTokens(a), TitleTokens(b)
	small, large := ta, tb
	if len(small) > len(large) {
		small, large = large, small
	}
	if len(small) == 0 {
		return 0
	}
	n := 0
	for w := range small {
		if large[w] {
			n++
		}
	}
	return float64(n) / float64(len(small))
}

// TitlesMatch reports whether two titles plausibly name the same book
// (normalized token overlap at or above titleMatchThreshold).
func TitlesMatch(a, b string) bool {
	return TitleOverlap(a, b) >= titleMatchThreshold
}
