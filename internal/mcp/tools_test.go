package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/metaprovider"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockDBInterface implements the database interface for testing
type MockDBInterface struct {
	mock.Mock
}

func (m *MockDBInterface) Search(ctx context.Context, query string, limit int, threshold float64) ([]db.SearchResultWithMetadata, error) {
	args := m.Called(ctx, query, limit, threshold)
	return args.Get(0).([]db.SearchResultWithMetadata), args.Error(1)
}

func (m *MockDBInterface) TextSearch(ctx context.Context, query string, limit int) ([]db.SearchResultWithMetadata, error) {
	args := m.Called(ctx, query, limit)
	return args.Get(0).([]db.SearchResultWithMetadata), args.Error(1)
}

func (m *MockDBInterface) TextSearchInBook(ctx context.Context, dir, query string, limit int) ([]db.SearchResultWithMetadata, error) {
	if !m.hasExpect("TextSearchInBook") {
		return nil, nil
	}
	args := m.Called(ctx, dir, query, limit)
	return args.Get(0).([]db.SearchResultWithMetadata), args.Error(1)
}

func (m *MockDBInterface) SearchInBook(ctx context.Context, query, dir string, limit int, threshold float64) ([]db.SearchResultWithMetadata, error) {
	if !m.hasExpect("SearchInBook") {
		return nil, nil
	}
	args := m.Called(ctx, query, dir, limit, threshold)
	return args.Get(0).([]db.SearchResultWithMetadata), args.Error(1)
}

// hasExpect reports whether an expectation was registered for method — lets the
// shared mock double as a plain stub for handlers that don't set expectations.
func (m *MockDBInterface) hasExpect(method string) bool {
	for _, c := range m.ExpectedCalls {
		if c.Method == method {
			return true
		}
	}
	return false
}

func (m *MockDBInterface) GetChunkContext(ctx context.Context, chunkID string, contextWindow int) ([]db.SearchResultWithMetadata, error) {
	args := m.Called(ctx, chunkID, contextWindow)
	return args.Get(0).([]db.SearchResultWithMetadata), args.Error(1)
}

func (m *MockDBInterface) Ping(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}

func (m *MockDBInterface) GetServiceStatus(ctx context.Context) (*db.QueueStats, error) {
	args := m.Called(ctx)
	return args.Get(0).(*db.QueueStats), args.Error(1)
}

func (m *MockDBInterface) GetRecentJobs(ctx context.Context, limit int) ([]db.RecentJob, error) {
	args := m.Called(ctx, limit)
	return args.Get(0).([]db.RecentJob), args.Error(1)
}

func (m *MockDBInterface) RequeueByID(ctx context.Context, id string) (string, error) {
	args := m.Called(ctx, id)
	return args.String(0), args.Error(1)
}

func (m *MockDBInterface) RequeueFailed(ctx context.Context) ([]string, error) {
	args := m.Called(ctx)
	return args.Get(0).([]string), args.Error(1)
}

// Pause/library methods are part of DBInterface but unused by the MCP tool
// handlers under test, so they are plain stubs (no testify expectations).
func (m *MockDBInterface) SetPaused(context.Context, bool, string) error { return nil }
func (m *MockDBInterface) GetControl(context.Context) (bool, *int, error) {
	return false, nil, nil
}
func (m *MockDBInterface) SetRunLimit(context.Context, *int, string) error { return nil }

func (m *MockDBInterface) GetBookSummaries(ctx context.Context, f db.BookFilter) ([]db.BookSummary, int, error) {
	if !m.hasExpect("GetBookSummaries") {
		return nil, 0, nil
	}
	args := m.Called(ctx, f)
	return args.Get(0).([]db.BookSummary), args.Int(1), args.Error(2)
}

func (m *MockDBInterface) GetLibraryTotals(ctx context.Context, query string) (db.LibraryTotals, error) {
	if !m.hasExpect("GetLibraryTotals") {
		return db.LibraryTotals{}, nil
	}
	args := m.Called(ctx, query)
	return args.Get(0).(db.LibraryTotals), args.Error(1)
}

func (m *MockDBInterface) GetBookTracks(ctx context.Context, dir string) ([]db.RecentJob, error) {
	if !m.hasExpect("GetBookTracks") {
		return nil, nil
	}
	args := m.Called(ctx, dir)
	return args.Get(0).([]db.RecentJob), args.Error(1)
}

func (m *MockDBInterface) GetTrackDetail(ctx context.Context, id string) (*db.TrackDetail, error) {
	if !m.hasExpect("GetTrackDetail") {
		return nil, nil
	}
	args := m.Called(ctx, id)
	td, _ := args.Get(0).(*db.TrackDetail)
	return td, args.Error(1)
}

func (m *MockDBInterface) RequeueByDir(context.Context, string) ([]string, error) {
	return nil, nil
}

func (m *MockDBInterface) GetFailedJobs(context.Context) ([]db.FailedJob, error) {
	return nil, nil
}

func TestHandleSemanticSearch(t *testing.T) {
	tests := []struct {
		name          string
		request       mcp.CallToolRequest
		mockResults   []db.SearchResultWithMetadata
		mockError     error
		expectedError bool
		expectedText  string
	}{
		{
			name: "successful search with results",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name: "semantic_search_audiobooks",
					Arguments: map[string]interface{}{
						"query":     "dragon",
						"threshold": 0.8,
						"limit":     5.0,
					},
				},
			},
			mockResults: []db.SearchResultWithMetadata{
				{
					ID:            "chunk-1",
					Content:       "The dragon soared majestically",
					Author:        "Fantasy Author",
					Title:         "Dragon Tales",
					Chapter:       "Chapter 1: The Beginning",
					ChapterIndex:  1,
					ChapterTitle:  "The Beginning",
					ChunkIndex:    0,
					Similarity:    0.9,
					TotalChunks:   10,
					TotalChapters: 5,
				},
			},
			mockError:     nil,
			expectedError: false,
			expectedText:  "Found 1 result(s):",
		},
		{
			name: "search with default parameters",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name: "semantic_search_audiobooks",
					Arguments: map[string]interface{}{
						"query": "magic",
					},
				},
			},
			mockResults:   []db.SearchResultWithMetadata{},
			mockError:     nil,
			expectedError: false,
			expectedText:  "No results found.",
		},
		{
			name: "database error",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name: "semantic_search_audiobooks",
					Arguments: map[string]interface{}{
						"query": "test",
					},
				},
			},
			mockResults:   []db.SearchResultWithMetadata{},
			mockError:     errors.New("database connection failed"),
			expectedError: true,
			expectedText:  "Search failed: database connection failed",
		},
		{
			name: "missing query parameter",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name:      "semantic_search_audiobooks",
					Arguments: map[string]interface{}{},
				},
			},
			mockResults:   nil,
			mockError:     nil,
			expectedError: true,
			expectedText:  "Missing or invalid query parameter:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &MockDBInterface{}

			args := tt.request.Params.Arguments.(map[string]interface{})
			if query, ok := args["query"].(string); ok && query != "" {
				expectedThreshold := 0.3 // default
				if threshold, exists := args["threshold"]; exists {
					expectedThreshold = threshold.(float64)
				}

				expectedLimit := 10 // default
				if limit, exists := args["limit"]; exists {
					switch v := limit.(type) {
					case float64:
						expectedLimit = int(v)
					case int:
						expectedLimit = v
					}
				}

				mockDB.On("Search", mock.Anything, query, expectedLimit, expectedThreshold).
					Return(tt.mockResults, tt.mockError).Once()
			}

			handler := NewToolHandlers(mockDB, nil)
			result, err := handler.handleSemanticSearch(context.Background(), tt.request)

			if tt.expectedError {
				if err != nil {
					assert.Error(t, err)
					assert.Contains(t, err.Error(), tt.expectedText)
				} else {
					assert.True(t, result.IsError)
					assert.Contains(t, result.Content[0].(mcp.TextContent).Text, tt.expectedText)
				}
			} else {
				assert.NoError(t, err)
				assert.False(t, result.IsError)
				assert.Contains(t, result.Content[0].(mcp.TextContent).Text, tt.expectedText)
			}

			mockDB.AssertExpectations(t)
		})
	}
}

func TestHandleTextSearch(t *testing.T) {
	tests := []struct {
		name          string
		request       mcp.CallToolRequest
		mockResults   []db.SearchResultWithMetadata
		mockError     error
		expectedError bool
		expectedText  string
	}{
		{
			name: "successful text search",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name: "text_search_audiobooks",
					Arguments: map[string]interface{}{
						"query": "exact phrase",
						"limit": 15.0,
					},
				},
			},
			mockResults: []db.SearchResultWithMetadata{
				{
					ID:            "chunk-2",
					Content:       "This contains the exact phrase we're looking for",
					Author:        "Text Author",
					Title:         "Text Book",
					Chapter:       "Chapter 1",
					ChapterIndex:  1,
					ChapterTitle:  "Chapter 1",
					ChunkIndex:    2,
					Similarity:    0.0,
					TotalChunks:   20,
					TotalChapters: 3,
				},
			},
			mockError:     nil,
			expectedError: false,
			expectedText:  "Found 1 result(s):",
		},
		{
			name: "text search with default limit",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name: "text_search_audiobooks",
					Arguments: map[string]interface{}{
						"query": "test query",
					},
				},
			},
			mockResults:   []db.SearchResultWithMetadata{},
			mockError:     nil,
			expectedError: false,
			expectedText:  "No results found.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &MockDBInterface{}

			args := tt.request.Params.Arguments.(map[string]interface{})
			query := args["query"].(string)

			expectedLimit := 10 // default
			if limit, exists := args["limit"]; exists {
				switch v := limit.(type) {
				case float64:
					expectedLimit = int(v)
				case int:
					expectedLimit = v
				}
			}

			mockDB.On("TextSearch", mock.Anything, query, expectedLimit).
				Return(tt.mockResults, tt.mockError).Once()

			handler := NewToolHandlers(mockDB, nil)
			result, err := handler.handleTextSearch(context.Background(), tt.request)

			if tt.expectedError {
				if err != nil {
					assert.Error(t, err)
				} else {
					assert.True(t, result.IsError)
				}
			} else {
				assert.NoError(t, err)
				assert.False(t, result.IsError)
				assert.Contains(t, result.Content[0].(mcp.TextContent).Text, tt.expectedText)
			}

			mockDB.AssertExpectations(t)
		})
	}
}

func TestHandleGetContext(t *testing.T) {
	tests := []struct {
		name          string
		request       mcp.CallToolRequest
		mockResults   []db.SearchResultWithMetadata
		mockError     error
		expectedError bool
		expectedText  string
	}{
		{
			name: "successful context retrieval",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name: "get_chunk_context",
					Arguments: map[string]interface{}{
						"chunkID":       "chunk-5",
						"contextWindow": 2,
					},
				},
			},
			mockResults: []db.SearchResultWithMetadata{
				{
					ID:         fmt.Sprintf("chunk-%d", 10),
					Content:    "Context before target chunk",
					Author:     "Christopher Paolini",
					Title:      "Eragon",
					ChunkIndex: 4,
					ChunkID:    "chunk-10",
				},
				{
					ID:         fmt.Sprintf("chunk-%d", 11),
					Content:    "Target chunk content",
					Author:     "Christopher Paolini",
					Title:      "Eragon",
					ChunkIndex: 5,
					ChunkID:    "chunk-11",
				},
				{
					ID:         fmt.Sprintf("chunk-%d", 12),
					Content:    "Context after target chunk",
					Author:     "Christopher Paolini",
					Title:      "Eragon",
					ChunkIndex: 6,
					ChunkID:    "chunk-12",
				},
			},
			mockError:     nil,
			expectedError: false,
			expectedText:  "Found 3 result(s)",
		},
		{
			name: "missing chunk ID parameter",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name: "get_chunk_context",
					Arguments: map[string]interface{}{
						"contextWindow": 2,
					},
				},
			},
			mockResults:   nil,
			mockError:     nil,
			expectedError: true,
			expectedText:  "Missing or invalid chunkID parameter",
		},
		{
			name: "database error",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name: "get_chunk_context",
					Arguments: map[string]interface{}{
						"chunkID":       "chunk-5",
						"contextWindow": 2,
					},
				},
			},
			mockResults:   nil,
			mockError:     errors.New("database connection failed"),
			expectedError: true,
			expectedText:  "Failed to get context",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := new(MockDBInterface)

			if tt.mockResults != nil || tt.mockError != nil {
				chunkID := tt.request.GetString("chunkID", "")
				contextWindow := tt.request.GetInt("contextWindow", 2)
				mockDB.On("GetChunkContext", mock.Anything, chunkID, contextWindow).Return(tt.mockResults, tt.mockError)
			}

			handlers := NewToolHandlers(mockDB, nil)
			result, err := handlers.handleGetContext(context.Background(), tt.request)

			if tt.expectedError {
				assert.NotNil(t, result)
				assert.Contains(t, result.Content[0].(mcp.TextContent).Text, tt.expectedText)
			} else {
				assert.Nil(t, err)
				assert.NotNil(t, result)
				assert.Contains(t, result.Content[0].(mcp.TextContent).Text, tt.expectedText)
			}

			mockDB.AssertExpectations(t)
		})
	}
}

// TestHandleGetContextClampsWindow verifies the clamp fires in the handler, not
// just in isolation: a huge contextWindow must reach GetChunkContext as the
// clamped maxContextWindow. The mock expectation is set to maxContextWindow, so
// if the handler passed the raw value, AssertExpectations would fail.
func TestHandleGetContextClampsWindow(t *testing.T) {
	mockDB := new(MockDBInterface)
	chunkID := "11111111-1111-1111-1111-111111111111"
	mockDB.On("GetChunkContext", mock.Anything, chunkID, maxContextWindow).
		Return([]db.SearchResultWithMetadata{
			{ID: chunkID, Content: "ctx", Author: "A", Title: "T", ChunkIndex: 1, ChunkID: chunkID},
		}, nil)

	handlers := NewToolHandlers(mockDB, nil)
	result, err := handlers.handleGetContext(context.Background(), req("get_chunk_context", map[string]interface{}{
		"chunkID":       chunkID,
		"contextWindow": 2147483647,
	}))

	assert.Nil(t, err)
	assert.NotNil(t, result)
	mockDB.AssertExpectations(t)
}

// req builds a CallToolRequest with the given name and arguments.
func req(name string, args map[string]interface{}) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}}
}

// providerForTest returns a MetadataProvider that labels the demo collections,
// so book-scope resolution in handler tests derives the same author/title as prod.
func providerForTest() metaprovider.MetadataProvider {
	const collectionsJSON = `[{"root":"audio-libation","layout":"author/title"}]`
	return metaprovider.NewPathProvider(collectionsJSON, "/books")
}

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }

// TestHandleListBooks asserts the inventory formatting: author/title, track
// progress, and the duration/word/chunk aggregates (em dash when nil).
func TestHandleListBooks(t *testing.T) {
	mockDB := &MockDBInterface{}
	books := []db.BookSummary{
		{Dir: "/books/audio-libation/Andy Weir/Project Hail Mary",
			SamplePath: "/books/audio-libation/Andy Weir/Project Hail Mary/PHM.m4b",
			Total:      1, Done: 1,
			DurationSeconds: fp(58320), WordCount: ip(124800), EmbedChunkCount: ip(412)},
		// No run_metrics yet → aggregates nil → em dashes.
		{Dir: "/books/audio-libation/Frank Herbert/Dune",
			SamplePath: "/books/audio-libation/Frank Herbert/Dune/01.mp3",
			Total:      24, Done: 0, Pending: 24},
	}
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Query: "", Limit: 50, Offset: 0}).
		Return(books, 2, nil).Once()
	mockDB.On("GetLibraryTotals", mock.Anything, "").
		Return(db.LibraryTotals{TotalBooks: 2, FullyTranscribed: 1, WithPending: 1}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleListBooks(context.Background(), req("list_books", map[string]interface{}{}))
	assert.NoError(t, err)
	assert.False(t, res.IsError)
	text := res.Content[0].(mcp.TextContent).Text
	// Leading whole-library summary line (TRUE totals).
	assert.Contains(t, text, "Library: 2 books — 1 fully transcribed, 1 with pending tracks.")
	assert.Contains(t, text, "Library: 2 book(s)")
	assert.Contains(t, text, "**Project Hail Mary** by Andy Weir")
	assert.Contains(t, text, "tracks: 1/1 done")
	assert.Contains(t, text, "words: 124800")
	assert.Contains(t, text, "chunks: 412")
	// Dune has no metrics → em dashes for words/chunks.
	assert.Contains(t, text, "**Dune** by Frank Herbert")
	assert.Contains(t, text, "words: — | chunks: —")
	// Flat view OMITS the per-book dir line (it ~doubles the payload).
	assert.NotContains(t, text, "dir:")
	mockDB.AssertExpectations(t)
}

// TestHandleSemanticSearchScoped asserts that passing `book` resolves to a single
// dir and routes through SearchInBook (the exact-scan path), not Search.
func TestHandleSemanticSearchScoped(t *testing.T) {
	mockDB := &MockDBInterface{}
	dir := "/books/audio-libation/Andy Weir/Project Hail Mary"
	summaries := []db.BookSummary{
		{Dir: dir, SamplePath: dir + "/PHM.m4b", Total: 1, Done: 1},
	}
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Query: "Project Hail Mary", Limit: 200}).
		Return(summaries, 1, nil).Once()
	mockDB.On("SearchInBook", mock.Anything, "amino acids", dir, 10, 0.3).
		Return([]db.SearchResultWithMetadata{{ID: "v1", Content: "about amino acids", Title: "Project Hail Mary", Author: "Andy Weir"}}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleSemanticSearch(context.Background(), req("semantic_search_audiobooks", map[string]interface{}{
		"query": "amino acids", "book": "Project Hail Mary",
	}))
	assert.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].(mcp.TextContent).Text, "Found 1 result(s)")
	mockDB.AssertExpectations(t)
}

// TestHandleTextSearchScoped asserts the `book` param routes text search through
// TextSearchInBook scoped to the resolved dir.
func TestHandleTextSearchScoped(t *testing.T) {
	mockDB := &MockDBInterface{}
	dir := "/books/audio-libation/Frank Herbert/Dune"
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Query: "Dune", Limit: 200}).
		Return([]db.BookSummary{{Dir: dir, SamplePath: dir + "/01.mp3"}}, 1, nil).Once()
	mockDB.On("TextSearchInBook", mock.Anything, dir, "spice", 10).
		Return([]db.SearchResultWithMetadata{{ID: "t1", Content: "the spice must flow", Title: "Dune", Author: "Frank Herbert"}}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleTextSearch(context.Background(), req("text_search_audiobooks", map[string]interface{}{
		"query": "spice", "book": "Dune",
	}))
	assert.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].(mcp.TextContent).Text, "Found 1 result(s)")
	mockDB.AssertExpectations(t)
}

// TestResolveBookDirAmbiguous asserts a `book` term matching multiple books
// returns a helpful disambiguation error listing the candidates.
func TestResolveBookDirAmbiguous(t *testing.T) {
	mockDB := &MockDBInterface{}
	d1 := "/books/audio-libation/Andy Weir/Project Hail Mary"
	d2 := "/books/audio-libation/Andy Weir/The Martian"
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Query: "Andy Weir", Limit: 200}).
		Return([]db.BookSummary{
			{Dir: d1, SamplePath: d1 + "/a.m4b"},
			{Dir: d2, SamplePath: d2 + "/b.m4b"},
		}, 2, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleSemanticSearch(context.Background(), req("semantic_search_audiobooks", map[string]interface{}{
		"query": "engineering", "book": "Andy Weir",
	}))
	assert.NoError(t, err)
	assert.True(t, res.IsError)
	text := res.Content[0].(mcp.TextContent).Text
	assert.Contains(t, text, "matched 2 books")
	assert.Contains(t, text, "Project Hail Mary")
	assert.Contains(t, text, "The Martian")
	mockDB.AssertExpectations(t)
}

// TestResolveBookDirASINCollision is the regression guard for the ASIN-collision
// bug: book="1984" must NOT resolve to Kahneman's *Noise* just because its ASIN
// (1984832069) contains the substring "1984". It should match the *1984* title
// (Orwell) instead. The ILIKE prefilter on file_path returns BOTH candidates;
// the in-Go label filter must reject the ASIN-only match.
func TestResolveBookDirASINCollision(t *testing.T) {
	mockDB := &MockDBInterface{}
	orwell := "/books/audio-libation/George Orwell/1984 [B0029Z9OBM]"
	noise := "/books/audio-libation/Daniel Kahneman/Noise [1984832069]"
	// The DB ILIKE '%1984%' on file_path returns both (Noise's ASIN contains 1984).
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Query: "1984", Limit: 200}).
		Return([]db.BookSummary{
			{Dir: orwell, SamplePath: orwell + "/1984.m4b"},
			{Dir: noise, SamplePath: noise + "/Noise.m4b"},
		}, 2, nil).Once()
	// Only the genuine 1984 title is searched — Noise is rejected.
	mockDB.On("TextSearchInBook", mock.Anything, orwell, "telescreen", 10).
		Return([]db.SearchResultWithMetadata{{ID: "t1", Content: "the telescreen", Title: "1984 [B0029Z9OBM]", Author: "George Orwell"}}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleTextSearch(context.Background(), req("text_search_audiobooks", map[string]interface{}{
		"query": "telescreen", "book": "1984",
	}))
	assert.NoError(t, err)
	assert.False(t, res.IsError, "book=1984 should resolve to the 1984 title, not collide on Noise's ASIN")
	assert.Contains(t, res.Content[0].(mcp.TextContent).Text, "Found 1 result(s)")
	mockDB.AssertExpectations(t)
}

// TestResolveBookDirASINExact asserts a bracketed ASIN in the query resolves
// EXACTLY to the book carrying that ASIN — a precise lookup, even when a title
// substring would be ambiguous.
func TestResolveBookDirASINExact(t *testing.T) {
	mockDB := &MockDBInterface{}
	noise := "/books/audio-libation/Daniel Kahneman/Noise [1984832069]"
	other := "/books/audio-libation/George Orwell/1984 [B0029Z9OBM]"
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Query: "[1984832069]", Limit: 200}).
		Return([]db.BookSummary{
			{Dir: noise, SamplePath: noise + "/Noise.m4b"},
			{Dir: other, SamplePath: other + "/1984.m4b"},
		}, 2, nil).Once()
	mockDB.On("TextSearchInBook", mock.Anything, noise, "bias", 10).
		Return([]db.SearchResultWithMetadata{{ID: "t1", Content: "noisy judgment", Title: "Noise [1984832069]", Author: "Daniel Kahneman"}}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleTextSearch(context.Background(), req("text_search_audiobooks", map[string]interface{}{
		"query": "bias", "book": "[1984832069]",
	}))
	assert.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].(mcp.TextContent).Text, "Found 1 result(s)")
	mockDB.AssertExpectations(t)
}

// TestResolveBookDirNoMatch asserts an unmatched `book` term returns a not-found
// hint that points at list_books.
func TestResolveBookDirNoMatch(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Query: "Nonexistent", Limit: 200}).
		Return([]db.BookSummary{}, 0, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleTextSearch(context.Background(), req("text_search_audiobooks", map[string]interface{}{
		"query": "x", "book": "Nonexistent",
	}))
	assert.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content[0].(mcp.TextContent).Text, "No book matched")
	mockDB.AssertExpectations(t)
}

// TestHandleGetTranscriptSingleTrack asserts a single-track book is resolved and
// its transcript paginated into timestamped segments with a next-offset footer.
func TestHandleGetTranscriptSingleTrack(t *testing.T) {
	mockDB := &MockDBInterface{}
	dir := "/books/audio-libation/Andy Weir/Project Hail Mary"
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Query: "Project Hail Mary", Limit: 200}).
		Return([]db.BookSummary{{Dir: dir, SamplePath: dir + "/PHM.m4b"}}, 1, nil).Once()
	mockDB.On("GetBookTracks", mock.Anything, dir).
		Return([]db.RecentJob{{ID: "job-1", FilePath: dir + "/PHM.m4b", Status: "done"}}, nil).Once()

	segs := make([]db.Segment, 4)
	for i := range segs {
		segs[i] = db.Segment{ID: i, Start: float64(i) * 10, End: float64(i)*10 + 8, Text: fmt.Sprintf("line %d", i)}
	}
	mockDB.On("GetTrackDetail", mock.Anything, "job-1").
		Return(&db.TrackDetail{
			ID: "job-1", FilePath: dir + "/PHM.m4b", Status: "done",
			HasTranscript: true, Language: "en", ModelName: "parakeet", DurationSeconds: 60,
			Segments: segs,
		}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"book": "Project Hail Mary", "limit": 2.0,
	}))
	assert.NoError(t, err)
	assert.False(t, res.IsError)
	text := res.Content[0].(mcp.TextContent).Text
	assert.Contains(t, text, "Transcript: "+dir+"/PHM.m4b")
	assert.Contains(t, text, "[00:00 → 00:08] line 0")
	assert.Contains(t, text, "[00:10 → 00:18] line 1")
	// Paginated: only 2 of 4 shown, footer points at the next offset.
	assert.NotContains(t, text, "line 2")
	assert.Contains(t, text, "Showing segments 1–2 of 4. Next page: offset=2.")
	mockDB.AssertExpectations(t)
}

// TestHandleGetTranscriptMultiTrack asserts a multi-track book returns a track
// chooser (so the caller picks a trackID) rather than a single transcript.
func TestHandleGetTranscriptMultiTrack(t *testing.T) {
	mockDB := &MockDBInterface{}
	dir := "/books/audio-libation/Frank Herbert/Dune"
	mockDB.On("GetBookSummaries", mock.Anything, db.BookFilter{Query: "Dune", Limit: 200}).
		Return([]db.BookSummary{{Dir: dir, SamplePath: dir + "/01.mp3"}}, 1, nil).Once()
	mockDB.On("GetBookTracks", mock.Anything, dir).
		Return([]db.RecentJob{
			{ID: "j1", FilePath: dir + "/01.mp3", Status: "done"},
			{ID: "j2", FilePath: dir + "/02.mp3", Status: "done"},
		}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"book": "Dune",
	}))
	assert.NoError(t, err)
	assert.False(t, res.IsError)
	text := res.Content[0].(mcp.TextContent).Text
	assert.Contains(t, text, "has 2 tracks")
	assert.Contains(t, text, "trackID=j1")
	assert.Contains(t, text, "trackID=j2")
	mockDB.AssertExpectations(t)
}

// TestHandleGetTranscriptByTrackID asserts trackID takes precedence (no book
// resolution) and reads the named track directly.
func TestHandleGetTranscriptByTrackID(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetTrackDetail", mock.Anything, "job-9").
		Return(&db.TrackDetail{
			ID: "job-9", FilePath: "/books/x/y/z.m4b", Status: "done",
			HasTranscript: true, Language: "en", ModelName: "parakeet", DurationSeconds: 30,
			Segments: []db.Segment{{ID: 0, Start: 0, End: 5, Text: "hello"}},
		}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"trackID": "job-9",
	}))
	assert.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].(mcp.TextContent).Text, "[00:00 → 00:05] hello")
	mockDB.AssertExpectations(t)
}

// TestClampHelpers verifies the clampLimit and clampOffset helpers directly.
func TestClampHelpers(t *testing.T) {
	// clampLimit: values in [1,1000] pass through; outside → default.
	if got := clampLimit(10, 50); got != 10 {
		t.Errorf("clampLimit(10, 50) = %d, want 10", got)
	}
	if got := clampLimit(1000, 50); got != 1000 {
		t.Errorf("clampLimit(1000, 50) = %d, want 1000", got)
	}
	if got := clampLimit(0, 50); got != 50 {
		t.Errorf("clampLimit(0, 50) = %d, want 50 (reset to default)", got)
	}
	if got := clampLimit(1001, 50); got != 50 {
		t.Errorf("clampLimit(1001, 50) = %d, want 50 (reset to default)", got)
	}
	if got := clampLimit(-1, 50); got != 50 {
		t.Errorf("clampLimit(-1, 50) = %d, want 50 (reset to default)", got)
	}

	// clampOffset: negative → 0; in-range passes through; above ceiling → maxOffset.
	if got := clampOffset(0); got != 0 {
		t.Errorf("clampOffset(0) = %d, want 0", got)
	}
	if got := clampOffset(100); got != 100 {
		t.Errorf("clampOffset(100) = %d, want 100", got)
	}
	if got := clampOffset(-1); got != 0 {
		t.Errorf("clampOffset(-1) = %d, want 0", got)
	}
	if got := clampOffset(maxOffset); got != maxOffset {
		t.Errorf("clampOffset(maxOffset) = %d, want %d (exact ceiling passes through)", got, maxOffset)
	}
	if got := clampOffset(maxOffset + 1); got != maxOffset {
		t.Errorf("clampOffset(maxOffset+1) = %d, want %d (clamped to ceiling)", got, maxOffset)
	}
	if got := clampOffset(2147483647); got != maxOffset {
		t.Errorf("clampOffset(MaxInt32) = %d, want %d (clamped to ceiling)", got, maxOffset)
	}

	// clampContextWindow: negative → 0; in-range passes through; above ceiling → maxContextWindow.
	if got := clampContextWindow(-1); got != 0 {
		t.Errorf("clampContextWindow(-1) = %d, want 0", got)
	}
	if got := clampContextWindow(2); got != 2 {
		t.Errorf("clampContextWindow(2) = %d, want 2", got)
	}
	if got := clampContextWindow(maxContextWindow); got != maxContextWindow {
		t.Errorf("clampContextWindow(maxContextWindow) = %d, want %d", got, maxContextWindow)
	}
	if got := clampContextWindow(2147483647); got != maxContextWindow {
		t.Errorf("clampContextWindow(MaxInt32) = %d, want %d (clamped to ceiling)", got, maxContextWindow)
	}
}

// TestSnippetCharsClamp verifies that snippetChars enforces both floor and
// ceiling, preventing large rune-slice allocations from a giant snippet value.
func TestSnippetCharsClamp(t *testing.T) {
	// Zero / negative → disabled (return full chunk).
	if got := snippetChars(0); got != 0 {
		t.Errorf("snippetChars(0) = %d, want 0", got)
	}
	if got := snippetChars(-1); got != 0 {
		t.Errorf("snippetChars(-1) = %d, want 0", got)
	}

	// Below floor → raised to minSnippetChars.
	if got := snippetChars(5); got != minSnippetChars {
		t.Errorf("snippetChars(5) = %d, want %d (floor)", got, minSnippetChars)
	}

	// Normal value passes through unchanged.
	if got := snippetChars(300); got != 300 {
		t.Errorf("snippetChars(300) = %d, want 300", got)
	}

	// Exactly at ceiling passes through.
	if got := snippetChars(maxSnippetChars); got != maxSnippetChars {
		t.Errorf("snippetChars(%d) = %d, want %d", maxSnippetChars, got, maxSnippetChars)
	}

	// Above ceiling → clamped to maxSnippetChars (prevents large allocations).
	if got := snippetChars(maxSnippetChars + 1); got != maxSnippetChars {
		t.Errorf("snippetChars(%d) = %d, want %d (ceiling clamp)", maxSnippetChars+1, got, maxSnippetChars)
	}
	if got := snippetChars(2_147_483_647); got != maxSnippetChars {
		t.Errorf("snippetChars(MaxInt32) = %d, want %d (ceiling clamp)", got, maxSnippetChars)
	}
}

// TestHandleListBooksLimitClamping asserts that out-of-range limit/offset values
// are clamped to safe defaults before the DB query is issued.
func TestHandleListBooksLimitClamping(t *testing.T) {
	cases := []struct {
		name       string
		args       map[string]interface{}
		wantFilter db.BookFilter
	}{
		{
			name:       "huge limit clamped to default 50",
			args:       map[string]interface{}{"limit": 999999999.0},
			wantFilter: db.BookFilter{Query: "", Limit: 50, Offset: 0},
		},
		{
			name:       "zero limit clamped to default 50",
			args:       map[string]interface{}{"limit": 0.0},
			wantFilter: db.BookFilter{Query: "", Limit: 50, Offset: 0},
		},
		{
			name:       "negative offset clamped to 0",
			args:       map[string]interface{}{"offset": -5.0},
			wantFilter: db.BookFilter{Query: "", Limit: 50, Offset: 0},
		},
		{
			name:       "valid limit and offset pass through",
			args:       map[string]interface{}{"limit": 20.0, "offset": 10.0},
			wantFilter: db.BookFilter{Query: "", Limit: 20, Offset: 10},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockDB := &MockDBInterface{}
			mockDB.On("GetBookSummaries", mock.Anything, tc.wantFilter).
				Return([]db.BookSummary{}, 0, nil).Once()

			h := NewToolHandlers(mockDB, providerForTest())
			res, err := h.handleListBooks(context.Background(), req("list_books", tc.args))
			assert.NoError(t, err)
			assert.False(t, res.IsError)
			mockDB.AssertExpectations(t)
		})
	}
}

// TestHandleSemanticSearchLimitClamping asserts that out-of-range limit is
// clamped to the search default (10) before the DB query.
func TestHandleSemanticSearchLimitClamping(t *testing.T) {
	mockDB := &MockDBInterface{}
	// Limit 999999 should be clamped to default 10.
	mockDB.On("Search", mock.Anything, "dragons", 10, 0.3).
		Return([]db.SearchResultWithMetadata{}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleSemanticSearch(context.Background(), req("semantic_search_audiobooks", map[string]interface{}{
		"query": "dragons", "limit": 999999.0,
	}))
	assert.NoError(t, err)
	assert.False(t, res.IsError)
	mockDB.AssertExpectations(t)
}

// TestHandleTextSearchLimitClamping asserts that an out-of-range limit is
// clamped to the text-search default (10).
func TestHandleTextSearchLimitClamping(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("TextSearch", mock.Anything, "spice", 10).
		Return([]db.SearchResultWithMetadata{}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleTextSearch(context.Background(), req("text_search_audiobooks", map[string]interface{}{
		"query": "spice", "limit": -5.0,
	}))
	assert.NoError(t, err)
	assert.False(t, res.IsError)
	mockDB.AssertExpectations(t)
}

// TestHandleGetTranscriptNotTranscribed asserts a pending track (no transcript)
// returns a clear error instead of an empty body.
func TestHandleGetTranscriptNotTranscribed(t *testing.T) {
	mockDB := &MockDBInterface{}
	mockDB.On("GetTrackDetail", mock.Anything, "job-p").
		Return(&db.TrackDetail{ID: "job-p", FilePath: "/books/a/b.m4b", Status: "pending", HasTranscript: false}, nil).Once()

	h := NewToolHandlers(mockDB, providerForTest())
	res, err := h.handleGetTranscript(context.Background(), req("get_transcript", map[string]interface{}{
		"trackID": "job-p",
	}))
	assert.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content[0].(mcp.TextContent).Text, "not transcribed yet")
	mockDB.AssertExpectations(t)
}
