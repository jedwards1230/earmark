package mcp

import (
	"context"
	"testing"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/earmark/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestListBooksStructuredContent asserts list_books emits a structuredContent
// payload (in addition to the back-compat text in Content[0]) that mirrors the
// underlying rows: the book list, whole-library totals, and pagination footer.
func TestListBooksStructuredContent(t *testing.T) {
	mockDB := &MockDBInterface{}
	books := []db.BookSummary{
		{Dir: "/books/audio-libation/Andy Weir/Project Hail Mary",
			SamplePath: "/books/audio-libation/Andy Weir/Project Hail Mary/PHM.m4b",
			Total:      1, Done: 1,
			DurationSeconds: fp(58320), WordCount: ip(124800), EmbedChunkCount: ip(412)},
	}
	// total (3) > page (1) so a nextOffset is expected in the structured payload.
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Query: "", Limit: 1, Offset: 0}).
		Return(books, 3, nil).Once()
	mockDB.On("GetLibraryTotals", mock.Anything, "").
		Return(db.LibraryTotals{TotalBooks: 3, FullyTranscribed: 1, WithPending: 2}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleListBooks(context.Background(), req("list_books", map[string]interface{}{"limit": float64(1)}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	// Back-compat text fallback is still present as the first content block.
	require.NotEmpty(t, res.Content)
	_, ok := res.Content[0].(*mcp.TextContent)
	assert.True(t, ok, "Content[0] should be the text fallback")

	// Structured payload is present and correctly typed.
	require.NotNil(t, res.StructuredContent, "list_books must emit structuredContent")
	out, ok := res.StructuredContent.(ListBooksOutput)
	require.True(t, ok, "structuredContent should be a ListBooksOutput, got %T", res.StructuredContent)

	assert.Equal(t, "flat", out.Format)
	assert.Equal(t, 3, out.Total)
	assert.Equal(t, 0, out.Offset)
	require.Len(t, out.Books, 1)
	assert.Equal(t, "Project Hail Mary", out.Books[0].Title)
	assert.Equal(t, "Andy Weir", out.Books[0].Author)
	assert.Equal(t, 1, out.Books[0].Done)
	require.NotNil(t, out.Books[0].WordCount)
	assert.Equal(t, 124800, *out.Books[0].WordCount)
	assert.Equal(t, 3, out.Totals.TotalBooks)
	assert.Equal(t, 1, out.Totals.FullyTranscribed)
	require.NotNil(t, out.NextOffset, "a further page exists → nextOffset set")
	assert.Equal(t, 1, *out.NextOffset)

	mockDB.AssertExpectations(t)
}

// TestSemanticSearchStructuredContent asserts the search tools emit a
// SearchResultsOutput carrying the typed rows and the ranking kind, with the
// text rendering kept as the fallback.
func TestSemanticSearchStructuredContent(t *testing.T) {
	mockDB := &MockDBInterface{}
	rows := []db.SearchResultWithMetadata{
		{ID: "v1", ChunkID: "v1", Content: "about amino acids", Title: "Project Hail Mary", Author: "Andy Weir", Similarity: 0.91, TotalChunks: 10, ChunkIndex: 2},
	}
	mockDB.On("Search", mock.Anything, "amino acids", 10, 0.3).Return(rows, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleSemanticSearch(context.Background(), req("semantic_search_audiobooks", map[string]interface{}{
		"query": "amino acids",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	require.NotNil(t, res.StructuredContent, "semantic search must emit structuredContent")
	out, ok := res.StructuredContent.(SearchResultsOutput)
	require.True(t, ok, "structuredContent should be a SearchResultsOutput, got %T", res.StructuredContent)
	assert.Equal(t, "semantic", out.Kind)
	assert.Equal(t, "amino acids", out.Query)
	assert.Equal(t, 1, out.Count)
	require.Len(t, out.Results, 1)
	assert.Equal(t, "Project Hail Mary", out.Results[0].Title)

	mockDB.AssertExpectations(t)
}

// TestSearchEmptyStructuredContent asserts the empty-result path still carries a
// well-shaped structured payload (results: []), so a consumer can rely on the
// shape unconditionally.
func TestSearchEmptyStructuredContent(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("TextSearch", mock.Anything, "nothing here", 10).
		Return([]db.SearchResultWithMetadata{}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleTextSearch(context.Background(), req("text_search_audiobooks", map[string]interface{}{
		"query": "nothing here",
	}))
	require.NoError(t, err)
	assert.False(t, res.IsError)

	out, ok := res.StructuredContent.(SearchResultsOutput)
	require.True(t, ok)
	assert.Equal(t, "trigram", out.Kind)
	assert.Equal(t, 0, out.Count)
	assert.NotNil(t, out.Results, "results should be a non-nil empty slice")
	assert.Len(t, out.Results, 0)

	mockDB.AssertExpectations(t)
}
