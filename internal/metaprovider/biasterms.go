package metaprovider

import (
	"strings"
	"unicode"
)

// commonWords is a set of English words that are too generic to be useful ASR
// bias terms. These words appear frequently in everyday speech, so adding them
// to a boosting list would over-bias the ASR model for those phonemes and
// degrade overall transcription quality. The list is intentionally small and
// conservative: it targets only the most frequent closed-class words (articles,
// prepositions, conjunctions, pronouns, auxiliaries) that are almost never
// meaningful as proper-noun bias signals.
//
// We do NOT attempt to enumerate every common English word — that would discard
// too many legitimate terms. The heuristic is: if a word appears on this list,
// drop it; if it passes the capitalization check AND is longer than minTermLen,
// keep it. This produces false-negatives (too few bias terms) rather than
// false-positives (too many noisy terms), which is the safer failure mode for
// ASR biasing.
var commonWords = map[string]bool{
	// Articles
	"a": true, "an": true, "the": true,
	// Prepositions
	"in": true, "on": true, "at": true, "to": true, "of": true, "for": true,
	"by": true, "up": true, "as": true, "or": true, "if": true, "it": true,
	"its": true, "is": true, "be": true, "do": true, "no": true,
	// Conjunctions
	"and": true, "but": true, "nor": true, "yet": true, "so": true,
	// Pronouns
	"i": true, "me": true, "my": true, "we": true, "us": true, "our": true,
	"he": true, "him": true, "his": true, "she": true, "her": true, "they": true, "them": true, "their": true, "you": true, "your": true,
	// Auxiliaries / very common verbs
	"am": true, "are": true, "was": true, "were": true, "has": true,
	"have": true, "had": true, "does": true, "did": true, "will": true,
	"would": true, "could": true, "should": true, "may": true, "might": true,
	"must": true, "can": true, "get": true, "got": true,
	// Demonstratives / common adverbs
	"this": true, "that": true, "these": true, "those": true,
	"not": true, "all": true, "one": true, "two": true, "new": true,
	"also": true, "just": true, "then": true, "than": true, "with": true,
	"from": true, "into": true, "what": true, "who": true, "how": true,
	"when": true, "where": true, "why": true,
}

// minTermLen is the minimum number of characters a token must have to be
// kept as a bias term. Very short tokens ("Ed", "Jo") are nearly always
// noise in an ASR biasing context and the NeMo boosting system handles
// sub-word tokens internally.
const minTermLen = 3

// DeriveBiasTerms derives a deduplicated list of proper-noun bias terms from
// a book's metadata. The resulting terms are suitable for use as a NeMo
// word-boosting list (see homelab-ansible runner PR).
//
// Source fields and strategy:
//   - Author, Narrator, Series: each full field value is added as a phrase;
//     individual capitalized words from the field are also added (so "George
//     Orwell" contributes both "George Orwell" and the individual tokens
//     "George" and "Orwell").
//   - Title: only capitalized individual words are added, not the full title
//     phrase — titles often contain common words in sequence that produce
//     unhelpful phrase-level bias (e.g. "The Man Who Was Thursday"). The full
//     Title phrase adds noise; its capitalized components are useful.
//
// Deduplication is case-insensitive: "Orwell" and "orwell" collapse to a
// single entry (the first-seen capitalisation is kept).
//
// Filtering: a word is kept only when it:
//  1. Is at least minTermLen (3) characters long.
//  2. Is not in commonWords (the closed-class English word list above).
//  3. Starts with a Unicode upper-case letter (capitalization heuristic for
//     proper nouns). Title words that happen to be lowercased (e.g. a leading
//     "the" from a series name formatted as "the Stormlight Archive") are
//     filtered out by this rule; that is intentional — they are not useful
//     bias targets.
//
// Full-field phrases (Author, Narrator, Series) bypass the capitalization
// filter but still require at least one non-common-word token to be present
// in the phrase before they are included — a phrase made entirely of common
// words (e.g. "The End") is not useful.
//
// DeriveBiasTerms is a pure function: given the same BookMeta it always
// returns the same slice (though the order may vary by map iteration).
func DeriveBiasTerms(meta BookMeta) []string {
	seen := make(map[string]bool)
	terms := make([]string, 0)

	// add inserts term into the deduplicated output. The key is lowercased so
	// "Orwell" and "ORWELL" are treated as the same term; the original
	// capitalisation of the first occurrence is preserved in the output.
	add := func(term string) {
		key := strings.ToLower(term)
		if !seen[key] {
			seen[key] = true
			terms = append(terms, term)
		}
	}

	// addField adds both the full multi-word phrase and its capitalized
	// component words. Used for Author, Narrator, Series — fields that are
	// typically proper names/phrases where the whole phrase is a meaningful
	// bias target (e.g. "George Orwell", "Terry Pratchett").
	addField := func(field string) {
		words := splitWords(field)
		if len(words) == 0 {
			return
		}

		// Collect individual proper-noun words first.
		var properWords []string
		for _, w := range words {
			if isProperNoun(w) {
				add(w)
				properWords = append(properWords, w)
			}
		}

		// Add the full phrase only when it contains at least one proper-noun
		// word and is not identical to a single already-added word (avoids
		// adding a redundant single-word "phrase").
		if len(properWords) > 0 && len(words) > 1 {
			phrase := strings.Join(words, " ")
			add(phrase)
		}
	}

	// For Title, only add capitalized individual words — not the whole phrase.
	addTitleWords := func(title string) {
		for _, w := range splitWords(title) {
			if isProperNoun(w) {
				add(w)
			}
		}
	}

	addField(meta.Author)
	addField(meta.Narrator)
	addField(meta.Series)
	addTitleWords(meta.Title)

	return terms
}

// splitWords splits a string into tokens on whitespace and common punctuation
// (hyphens, slashes, parentheses, colons, commas, periods, apostrophes) and
// returns non-empty tokens. This is intentionally simple: we avoid unicode
// segmentation libraries to keep the package dependency-free.
func splitWords(s string) []string {
	// Replace punctuation-ish separators with a space so a simple Fields call
	// handles all delimiters at once.
	mapped := strings.Map(func(r rune) rune {
		switch r {
		case '-', '/', '\\', '(', ')', '[', ']', ':', ',', '.', '\'', '"',
			'\t', '\n', '\r':
			return ' '
		}
		return r
	}, s)

	raw := strings.Fields(mapped)
	out := make([]string, 0, len(raw))
	for _, tok := range raw {
		if tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// isProperNoun returns true when word looks like a proper noun suitable for
// ASR biasing: it must start with an upper-case Unicode letter, be at least
// minTermLen characters, and not appear in commonWords.
func isProperNoun(word string) bool {
	runes := []rune(word)
	if len(runes) < minTermLen {
		return false
	}
	if !unicode.IsUpper(runes[0]) {
		return false
	}
	return !commonWords[strings.ToLower(word)]
}
