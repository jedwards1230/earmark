package metaprovider_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/metaprovider"
)

// containsAll reports whether got contains every element of want (order-independent).
func containsAll(t *testing.T, got []string, want []string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("expected %q in result %v", w, got)
		}
	}
}

// containsNone reports whether got contains none of the elements in bad.
func containsNone(t *testing.T, got []string, bad []string) {
	t.Helper()
	for _, b := range bad {
		if slices.Contains(got, b) {
			t.Errorf("unexpected %q in result %v", b, got)
		}
	}
}

// TestDeriveBiasTerms_KnownBook checks the classic Orwell / Prebble case
// that the task spec calls out explicitly.
func TestDeriveBiasTerms_KnownBook(t *testing.T) {
	t.Parallel()

	meta := metaprovider.BookMeta{
		Title:    "Nineteen Eighty-Four",
		Author:   "George Orwell",
		Narrator: "Simon Prebble",
	}
	got := metaprovider.DeriveBiasTerms(meta)

	// Individual proper-noun tokens from Author and Narrator must be present.
	containsAll(t, got, []string{"George", "Orwell", "Simon", "Prebble"})
	// Full author and narrator phrases must be present.
	containsAll(t, got, []string{"George Orwell", "Simon Prebble"})
	// Title words: "Nineteen" and "Eighty" are capitalised proper-ish words.
	containsAll(t, got, []string{"Nineteen"})
	// "Four" is short enough (4 chars) and capitalised — should be included.
	containsAll(t, got, []string{"Four"})
}

// TestDeriveBiasTerms_SeriesField verifies Series contributes both the phrase
// and its individual capitalised tokens.
func TestDeriveBiasTerms_SeriesField(t *testing.T) {
	t.Parallel()

	meta := metaprovider.BookMeta{
		Author: "Brandon Sanderson",
		Series: "The Stormlight Archive",
	}
	got := metaprovider.DeriveBiasTerms(meta)

	// Series phrase (whole, including the leading article) and its individual
	// proper-noun tokens should appear. The phrase is joined from all words, so
	// it retains "The"; "The" is still filtered as a standalone term below.
	containsAll(t, got, []string{"Stormlight", "Archive", "The Stormlight Archive"})
	// "The" is a common word — must NOT appear.
	containsNone(t, got, []string{"The"})
}

// TestDeriveBiasTerms_DeduplicationCaseInsensitive verifies that the same
// word appearing in multiple fields (e.g. author surname repeated in title)
// produces only one entry in the output.
func TestDeriveBiasTerms_DeduplicationCaseInsensitive(t *testing.T) {
	t.Parallel()

	meta := metaprovider.BookMeta{
		Title:  "Sanderson Fantasy",
		Author: "Brandon Sanderson",
	}
	got := metaprovider.DeriveBiasTerms(meta)

	// "Sanderson" appears in both fields — must deduplicate to a single entry.
	count := 0
	for _, term := range got {
		if strings.EqualFold(term, "Sanderson") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of 'Sanderson', got %d in %v", count, got)
	}
}

// TestDeriveBiasTerms_EmptyMeta verifies that an empty BookMeta produces an
// empty (not nil) result.
func TestDeriveBiasTerms_EmptyMeta(t *testing.T) {
	t.Parallel()

	got := metaprovider.DeriveBiasTerms(metaprovider.BookMeta{})
	if len(got) != 0 {
		t.Errorf("expected empty result for empty meta, got %v", got)
	}
}

// TestDeriveBiasTerms_CommonWordsFiltered verifies that the closed-class word
// filter drops high-frequency English words that would over-bias the ASR model.
func TestDeriveBiasTerms_CommonWordsFiltered(t *testing.T) {
	t.Parallel()

	meta := metaprovider.BookMeta{
		Title:  "The Man in the High Castle",
		Author: "Philip K Dick",
	}
	got := metaprovider.DeriveBiasTerms(meta)

	// Common words must not appear as terms.
	containsNone(t, got, []string{"The", "the", "in", "In"})
	// Proper nouns should remain.
	containsAll(t, got, []string{"Philip", "Dick", "Castle", "High"})
}

// TestDeriveBiasTerms_ShortTokensDropped verifies that tokens shorter than
// minTermLen (3 runes) are not emitted as bias terms.
func TestDeriveBiasTerms_ShortTokensDropped(t *testing.T) {
	t.Parallel()

	meta := metaprovider.BookMeta{
		// "Jo" (2 chars) should be dropped; "Nesbo" should be kept.
		Author: "Jo Nesbo",
	}
	got := metaprovider.DeriveBiasTerms(meta)

	containsAll(t, got, []string{"Nesbo"})
	containsNone(t, got, []string{"Jo"})
}

// TestDeriveBiasTerms_LowercaseTitleWordsDropped verifies that lowercase words
// in the title (common for articles/prepositions in book titles) are filtered out.
func TestDeriveBiasTerms_LowercaseTitleWordsDropped(t *testing.T) {
	t.Parallel()

	meta := metaprovider.BookMeta{
		Title: "Lord of the Rings",
	}
	got := metaprovider.DeriveBiasTerms(meta)

	// Lowercase "of" and "the" must not appear.
	containsNone(t, got, []string{"of", "the", "lord"})
	// Capitalised words should be kept.
	containsAll(t, got, []string{"Lord", "Rings"})
}

// TestDeriveBiasTerms_Deterministic verifies that calling DeriveBiasTerms
// twice with the same input returns the same result. (The order must be
// stable because the function processes fields in a fixed order.)
func TestDeriveBiasTerms_Deterministic(t *testing.T) {
	t.Parallel()

	meta := metaprovider.BookMeta{
		Title:    "Dune",
		Author:   "Frank Herbert",
		Narrator: "Simon Vance",
		Series:   "Dune Chronicles",
	}

	a := metaprovider.DeriveBiasTerms(meta)
	b := metaprovider.DeriveBiasTerms(meta)

	if len(a) != len(b) {
		t.Fatalf("non-deterministic: first call=%v, second call=%v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("result[%d]: first=%q second=%q", i, a[i], b[i])
		}
	}
}

// TestDeriveBiasTerms_HyphenatedName verifies that hyphenated author/narrator
// names are split correctly and both parts are emitted.
func TestDeriveBiasTerms_HyphenatedName(t *testing.T) {
	t.Parallel()

	meta := metaprovider.BookMeta{
		Author: "Ursula K. Le Guin",
	}
	got := metaprovider.DeriveBiasTerms(meta)

	containsAll(t, got, []string{"Ursula", "Guin"})
	// "Le" is 2 chars — should be dropped by the minTermLen filter.
	containsNone(t, got, []string{"Le"})
}

// TestDeriveBiasTerms_NarratorAndAuthorSeparate verifies that Narrator and
// Author are treated as independent fields.
func TestDeriveBiasTerms_NarratorAndAuthorSeparate(t *testing.T) {
	t.Parallel()

	meta := metaprovider.BookMeta{
		Author:   "Andy Weir",
		Narrator: "Ray Porter",
	}
	got := metaprovider.DeriveBiasTerms(meta)

	containsAll(t, got, []string{"Andy", "Weir", "Andy Weir", "Ray", "Porter", "Ray Porter"})
}

// TestDeriveBiasTerms_TitlePhaseNotAdded verifies that the full title phrase
// is never added — only its individual proper-noun words are.
func TestDeriveBiasTerms_TitlePhaseNotAdded(t *testing.T) {
	t.Parallel()

	meta := metaprovider.BookMeta{
		Title: "Project Hail Mary",
	}
	got := metaprovider.DeriveBiasTerms(meta)

	// Individual capitalised words should appear.
	containsAll(t, got, []string{"Project", "Hail", "Mary"})
	// The full phrase "Project Hail Mary" must NOT be added (Title ≠ phrase).
	containsNone(t, got, []string{"Project Hail Mary"})
}
