package mcp

import (
	"strings"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/library"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
)

// TestFormatSearchResults covers the get_chunk_context path: positional
// neighbours with NO similarity/relevance line (that line is reserved for the
// ranked search tools, which use formatSearchResultsOpts).
func TestFormatSearchResults(t *testing.T) {
	tests := []struct {
		name     string
		results  []db.SearchResultWithMetadata
		expected string
	}{
		{
			name: "single context result has no relevance line",
			results: []db.SearchResultWithMetadata{
				{
					ID:           "chunk-uuid-1",
					Content:      "The dragon soared through the sky",
					Author:       "J.R.R. Tolkien",
					Title:        "The Hobbit",
					ChunkIndex:   5,
					Similarity:   0.85,
					ChapterIndex: 1,
					ChapterTitle: "An Unexpected Party",
					TotalChunks:  50,
					FilePath:     "/media/audiobooks/tolkien/the-hobbit.m4b",
					ChunkID:      "11111111-1111-1111-1111-111111111111",
					WordCount:    8,
				},
			},
			expected: `Found 1 result(s):

**The Hobbit** by J.R.R. Tolkien
Chapter 1: An Unexpected Party (chunk 6/50)
ID: 11111111-1111-1111-1111-111111111111 | File: /media/audiobooks/tolkien/the-hobbit.m4b | Words: 8
> The dragon soared through the sky
`,
		},
		{
			name:     "empty results",
			results:  []db.SearchResultWithMetadata{},
			expected: "No results found.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatSearchResults(tt.results)
			assert.Equal(t, tt.expected, result.Content[0].(mcp.TextContent).Text)
		})
	}
}

// TestFormatSemanticVsTextRelevance is the regression guard for the misleading
// text-search "similarity" line: semantic results show a similarity percentage;
// text results show "trigram match" and NEVER a percentage.
func TestFormatSemanticVsTextRelevance(t *testing.T) {
	res := []db.SearchResultWithMetadata{{
		ID: "c1", Content: "the spice must flow", Author: "Frank Herbert", Title: "Dune",
		ChunkIndex: 2, ChapterIndex: 3, ChapterTitle: "Arrakis", TotalChunks: 40,
		Similarity: 0.012, ChunkID: "c1",
	}}

	semantic := formatSearchResultsOpts(res, searchSemantic, "spice", 0).Content[0].(mcp.TextContent).Text
	assert.Contains(t, semantic, "similarity: 1%")

	text := formatSearchResultsOpts(res, searchText, "spice", 0).Content[0].(mcp.TextContent).Text
	assert.Contains(t, text, "ranked by trigram match")
	assert.NotContains(t, text, "similarity:")
	assert.NotContains(t, text, "%)") // no percentage on a literal-match hit
}

// TestSnippetTruncation covers both truncation paths: text search centres the
// window on the literal match; semantic search returns a leading preview. Both
// append the get_chunk_context marker only when actually shortened.
func TestSnippetTruncation(t *testing.T) {
	// A leading run of filler well over the 80-char window, then NEEDLE far out so
	// a leading preview clearly excludes it while a centred window includes it.
	long := "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi " +
		"omicron pi rho sigma tau upsilon phi chi psi omega aaa bbb ccc ddd eee fff " +
		"ggg hhh iii jjj NEEDLE kkk lll mmm nnn ooo ppp qqq rrr sss ttt uuu vvv www"
	res := []db.SearchResultWithMetadata{{
		ID: "c1", Content: long, Author: "A", Title: "B", ChunkIndex: 0, TotalChunks: 1, ChunkID: "c1",
	}}

	// Text search, snippet=80, query NEEDLE → centred excerpt that includes NEEDLE.
	text := formatSearchResultsOpts(res, searchText, "NEEDLE", 80).Content[0].(mcp.TextContent).Text
	assert.Contains(t, text, "NEEDLE")
	assert.Contains(t, text, "(truncated, use get_chunk_context for full text)")
	assert.NotContains(t, text, "alpha beta gamma") // leading text dropped by centring

	// Semantic search, snippet=80 → leading preview (starts at alpha, excludes the
	// far-out NEEDLE since there is no sub-chunk match position to centre on).
	sem := formatSearchResultsOpts(res, searchSemantic, "NEEDLE", 80).Content[0].(mcp.TextContent).Text
	assert.Contains(t, sem, "alpha beta")
	assert.NotContains(t, sem, "NEEDLE")
	assert.Contains(t, sem, "(truncated, use get_chunk_context for full text)")

	// snippet larger than the content → no truncation, no marker.
	full := formatSearchResultsOpts(res, searchSemantic, "", 100000).Content[0].(mcp.TextContent).Text
	assert.Contains(t, full, long)
	assert.NotContains(t, full, "(truncated")
}

// TestMakeSnippetKindGate tests makeSnippet directly to assert the centering gate
// is kind-gated: searchSemantic ALWAYS returns a leading window, even when the
// query string appears mid-chunk; searchText centres on the match.
func TestMakeSnippetKindGate(t *testing.T) {
	// Build a chunk whose query word sits well past the first 80 chars so that a
	// leading window excludes it while a centred window includes it.
	prefix := "word1 word2 word3 word4 word5 word6 word7 word8 word9 word10 word11 word12 "
	needle := "TARGET"
	suffix := " word13 word14 word15 word16 word17 word18 word19 word20"
	chunk := prefix + needle + suffix
	max := 80

	// searchSemantic: NEVER centres — even when needle is in the content, the
	// result must be a leading window that starts at the beginning of the chunk.
	semSnip, truncated := makeSnippet(chunk, needle, max, searchSemantic)
	if truncated {
		// Leading window: must start with the first word(s) of the chunk.
		if !strings.HasPrefix(semSnip, "word1") && !strings.HasPrefix(semSnip, "…") {
			// Allow a leading ellipsis only if start==0 (it won't be here).
			t.Errorf("semantic snippet does not start at chunk beginning: %q", semSnip)
		}
		// Must NOT contain the needle (it is beyond the leading window).
		if strings.Contains(semSnip, needle) {
			t.Errorf("semantic snippet centered on query match (contains %q); want leading preview: %q", needle, semSnip)
		}
	}

	// searchText: DOES centre — the excerpt must contain the needle.
	textSnip, _ := makeSnippet(chunk, needle, max, searchText)
	if !strings.Contains(textSnip, needle) {
		t.Errorf("text snippet does not contain query match %q: %q", needle, textSnip)
	}
}

// TestFormatBookTree asserts the author-grouped list_books output groups the same
// rows under their author with the same per-book metadata.
func TestFormatBookTree(t *testing.T) {
	resolver := library.NewResolver("/books", []library.Collection{
		{Root: "audio-libation", Layout: "author/title"},
	})
	books := []db.BookSummary{
		{Dir: "/books/audio-libation/Andy Weir/Project Hail Mary",
			SamplePath: "/books/audio-libation/Andy Weir/Project Hail Mary/PHM.m4b",
			Total:      1, Done: 1, DurationSeconds: fpT(58320), WordCount: ipT(124800), EmbedChunkCount: ipT(412)},
		{Dir: "/books/audio-libation/Andy Weir/The Martian",
			SamplePath: "/books/audio-libation/Andy Weir/The Martian/TM.m4b",
			Total:      1, Done: 1},
		{Dir: "/books/audio-libation/Frank Herbert/Dune",
			SamplePath: "/books/audio-libation/Frank Herbert/Dune/01.mp3",
			Total:      24, Done: 22, Pending: 2},
	}
	out := formatBookTree(books, 3, 0, resolver).Content[0].(mcp.TextContent).Text
	assert.Contains(t, out, "Library: 3 book(s) across 2 author(s)")
	assert.Contains(t, out, "Andy Weir\n")
	assert.Contains(t, out, "Frank Herbert\n")
	assert.Contains(t, out, "• Project Hail Mary — tracks: 1/1 done")
	assert.Contains(t, out, "• The Martian — tracks: 1/1 done")
	assert.Contains(t, out, "• Dune — tracks: 22/24 done, 2 pending")
	assert.Contains(t, out, "words: 124800")
	assert.Contains(t, out, "chunks: 412")
}

func fpT(v float64) *float64 { return &v }
func ipT(v int) *int         { return &v }
