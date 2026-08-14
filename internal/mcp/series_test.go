package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/metaprovider"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// sp returns a pointer to s, for the nullable raw db.BookSummary.Series column.
func sp(s string) *string { return &s }

// duneSeriesRaw is the real stored shape of book_metadata.series: a comma-joined
// list of "Name #Sequence" entries, because a book can be in several series.
const duneSeriesRaw = "Dune #2, The Dune Sequence #13"

// TestListBooksSeriesStructuredContent asserts list_books parses the raw
// book_metadata.series column into the structured {name, sequence} entries.
func TestListBooksSeriesStructuredContent(t *testing.T) {
	mockDB := &MockDBInterface{}
	books := []db.BookSummary{
		{Dir: "/books/audio-libation/Frank Herbert/Dune Messiah",
			SamplePath: "/books/audio-libation/Frank Herbert/Dune Messiah/01.m4b",
			Total:      1, Done: 1, Series: sp(duneSeriesRaw)},
	}
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Limit: 50}).
		Return(books, 1, nil).Once()
	mockDB.On("GetLibraryTotals", mock.Anything, "").
		Return(db.LibraryTotals{TotalBooks: 1, FullyTranscribed: 1}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleListBooks(context.Background(), req("list_books", nil))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	out, ok := res.StructuredContent.(ListBooksOutput)
	require.True(t, ok, "structuredContent should be a ListBooksOutput, got %T", res.StructuredContent)
	require.Len(t, out.Books, 1)
	assert.Equal(t, []metaprovider.SeriesRef{
		{Name: "Dune", Sequence: "2"},
		{Name: "The Dune Sequence", Sequence: "13"},
	}, out.Books[0].Series)

	mockDB.AssertExpectations(t)
}

// TestListBooksNoSeriesOmitsKey asserts a book with no series metadata round-trips
// cleanly: the parsed slice is nil and `series` is absent from the marshalled JSON
// (the omitempty tag actually works).
func TestListBooksNoSeriesOmitsKey(t *testing.T) {
	mockDB := &MockDBInterface{}
	books := []db.BookSummary{
		{Dir: "/books/audio-libation/Andy Weir/Project Hail Mary",
			SamplePath: "/books/audio-libation/Andy Weir/Project Hail Mary/PHM.m4b",
			Total:      1, Done: 1},
	}
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Limit: 50}).
		Return(books, 1, nil).Once()
	mockDB.On("GetLibraryTotals", mock.Anything, "").
		Return(db.LibraryTotals{TotalBooks: 1, FullyTranscribed: 1}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleListBooks(context.Background(), req("list_books", nil))
	require.NoError(t, err)

	out, ok := res.StructuredContent.(ListBooksOutput)
	require.True(t, ok)
	require.Len(t, out.Books, 1)
	assert.Nil(t, out.Books[0].Series)

	raw, err := json.Marshal(out.Books[0])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"series"`,
		"omitempty must drop the key entirely for a book with no series")

	mockDB.AssertExpectations(t)
}

// TestSearchHitCarriesSeries asserts a search hit's structured row exposes the
// series enrichment the db layer attached (and that a hit without one omits the
// key entirely).
func TestSearchHitCarriesSeries(t *testing.T) {
	mockDB := &MockDBInterface{}
	rows := []db.SearchResultWithMetadata{
		{ID: "s1", ChunkID: "s1", Content: "the spice must flow", Title: "Dune Messiah",
			Author: "Frank Herbert", Similarity: 0.88, TotalChunks: 10, ChunkIndex: 2,
			Series: metaprovider.ParseSeries(duneSeriesRaw)},
		{ID: "s2", ChunkID: "s2", Content: "amino acids", Title: "Project Hail Mary",
			Author: "Andy Weir", Similarity: 0.71, TotalChunks: 10, ChunkIndex: 3},
	}
	mockDB.On("Search", mock.Anything, "spice", 10, 0.3).Return(rows, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleSemanticSearch(context.Background(), req("semantic_search_audiobooks", map[string]interface{}{
		"query": "spice",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	out, ok := res.StructuredContent.(SearchResultsOutput)
	require.True(t, ok, "structuredContent should be a SearchResultsOutput, got %T", res.StructuredContent)
	require.Len(t, out.Results, 2)
	assert.Equal(t, []metaprovider.SeriesRef{
		{Name: "Dune", Sequence: "2"},
		{Name: "The Dune Sequence", Sequence: "13"},
	}, out.Results[0].Series)

	raw, err := json.Marshal(out.Results[1])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"series"`,
		"a hit whose book has no series must omit the key")

	mockDB.AssertExpectations(t)
}

// TestListBooksSeriesFilterReachesDB asserts the `series` argument is trimmed and
// threaded into db.BookFilter (the mock expectation fails the test otherwise).
func TestListBooksSeriesFilterReachesDB(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Series: "Dune", Limit: 50}).
		Return([]db.BookSummary{}, 0, nil).Once()
	mockDB.On("GetLibraryTotals", mock.Anything, "").
		Return(db.LibraryTotals{}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleListBooks(context.Background(), req("list_books", map[string]interface{}{
		"series": "  Dune  ",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	mockDB.AssertExpectations(t)
}

// TestListBooksFormatSeriesRendering asserts format=series groups books by series
// name, orders them by sequence (numerically, with the 1.5 novella between 1 and
// 2), lists a multi-series book under EACH of its series, and puts books with no
// series in a trailing "No series" group so nothing disappears.
func TestListBooksFormatSeriesRendering(t *testing.T) {
	mockDB := &MockDBInterface{}
	books := []db.BookSummary{
		// Deliberately out of sequence order, and #10 first so a string sort would
		// wrongly place it before #2.
		{Dir: "/books/audio-libation/Frank Herbert/Book Ten", SamplePath: "/books/audio-libation/Frank Herbert/Book Ten/01.m4b",
			Total: 1, Done: 1, Series: sp("Dune #10")},
		{Dir: "/books/audio-libation/Frank Herbert/Dune Messiah", SamplePath: "/books/audio-libation/Frank Herbert/Dune Messiah/01.m4b",
			Total: 1, Done: 1, Series: sp(duneSeriesRaw)},
		{Dir: "/books/audio-libation/Frank Herbert/Novella", SamplePath: "/books/audio-libation/Frank Herbert/Novella/01.m4b",
			Total: 1, Done: 1, Series: sp("Dune #1.5")},
		{Dir: "/books/audio-libation/Andy Weir/Project Hail Mary", SamplePath: "/books/audio-libation/Andy Weir/Project Hail Mary/PHM.m4b",
			Total: 1, Done: 1},
	}
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Limit: 50}).
		Return(books, 4, nil).Once()
	mockDB.On("GetLibraryTotals", mock.Anything, "").
		Return(db.LibraryTotals{TotalBooks: 4, FullyTranscribed: 4}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleListBooks(context.Background(), req("list_books", map[string]interface{}{
		"format": "series",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	out, ok := res.StructuredContent.(ListBooksOutput)
	require.True(t, ok)
	assert.Equal(t, "series", out.Format)
	// The structured payload stays the flat page — grouping is a text concern.
	require.Len(t, out.Books, 4)

	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "Content[0] should be the text fallback")
	text := tc.Text

	// Group headers: the two series plus the trailing no-series bucket.
	assert.Contains(t, text, "\nDune\n")
	assert.Contains(t, text, "\nThe Dune Sequence\n")
	assert.Contains(t, text, "\n"+noSeriesGroup+"\n")
	assert.Contains(t, text, "a book in several series is listed under each of them")

	// Sequence ordering within the Dune group: 1.5 → 2 → 10 (numeric, not string).
	i15 := strings.Index(text, "#1.5 Novella")
	i2 := strings.Index(text, "#2 Dune Messiah")
	i10 := strings.Index(text, "#10 Book Ten")
	require.True(t, i15 >= 0 && i2 >= 0 && i10 >= 0, "all sequenced books must render:\n%s", text)
	assert.Less(t, i15, i2, "1.5 sorts before 2")
	assert.Less(t, i2, i10, "2 sorts before 10 (numeric, not lexical)")

	// The multi-series book appears under its second series too, with that
	// series' own sequence.
	assert.Contains(t, text, "#13 Dune Messiah")
	// The unsequenced book is still listed.
	assert.Contains(t, text, "Project Hail Mary")

	mockDB.AssertExpectations(t)
}

// TestCompareSequence covers the string-typed sequence ordering rules: numeric
// when both parse, empty last, string fallback otherwise.
func TestCompareSequence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "numeric ascending", a: "2", b: "10", want: true},
		{name: "numeric descending", a: "10", b: "2", want: false},
		{name: "decimal novella between integers", a: "1.5", b: "2", want: true},
		{name: "equal", a: "3", b: "3", want: false},
		{name: "empty sorts last", a: "", b: "1", want: false},
		{name: "non-empty before empty", a: "1", b: "", want: true},
		{name: "non-numeric falls back to string compare", a: "1-3", b: "2", want: true},
		{name: "both non-numeric", a: "IV", b: "II", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := compareSequence(tc.a, tc.b); got != tc.want {
				t.Errorf("compareSequence(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
