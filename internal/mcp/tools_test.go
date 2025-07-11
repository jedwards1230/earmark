package mcp

import (
	"context"
	"errors"
	"testing"
	"github.com/jedwards1230/lil-whisper/internal/db"

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

func (m *MockDBInterface) GetHierarchicalData(ctx context.Context) ([]db.HierarchicalEntry, error) {
	args := m.Called(ctx)
	return args.Get(0).([]db.HierarchicalEntry), args.Error(1)
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
					ID:            1,
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
			mockResults: []db.SearchResultWithMetadata{}, mockError: errors.New("database connection failed"),
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
			}, mockResults: nil,
			mockError:     nil,
			expectedError: true,
			expectedText:  "Missing or invalid query parameter:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &MockDBInterface{}

			// Only set up mock expectation if we have a valid query
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

			handler := NewToolHandlers(mockDB)
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
					Name: "text_search_audiobooks", Arguments: map[string]interface{}{
						"query": "exact phrase",
						"limit": 15.0,
					},
				},
			},
			mockResults: []db.SearchResultWithMetadata{
				{
					ID:            1,
					Content:       "This contains the exact phrase we're looking for",
					Author:        "Text Author",
					Title:         "Text Book",
					Chapter:       "Chapter 1",
					ChapterIndex:  1,
					ChapterTitle:  "Chapter 1",
					ChunkIndex:    2,
					Similarity:    0.0, // Text search doesn't use similarity
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

			handler := NewToolHandlers(mockDB)
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

func TestHandleBrowseLibrary(t *testing.T) {
	tests := []struct {
		name          string
		request       mcp.CallToolRequest
		mockEntries   []db.HierarchicalEntry
		mockError     error
		expectedError bool
		expectedText  string
	}{
		{
			name: "browse all books",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name:      "browse_audiobook_library",
					Arguments: map[string]interface{}{},
				},
			},
			mockEntries: []db.HierarchicalEntry{
				{
					Author:   "Test Author",
					Title:    "Test Book",
					Chapters: []string{"Chapter 1", "Chapter 2"},
				},
			},
			mockError:     nil,
			expectedError: false,
			expectedText:  "📚 **Audiobook Library**",
		},
		{
			name: "filter by author",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name: "browse_audiobook_library",
					Arguments: map[string]interface{}{
						"author": "tolkien",
					},
				},
			},
			mockEntries: []db.HierarchicalEntry{
				{
					Author:   "J.R.R. Tolkien",
					Title:    "The Hobbit",
					Chapters: []string{"An Unexpected Party"},
				},
				{
					Author:   "Brandon Sanderson",
					Title:    "The Way of Kings",
					Chapters: []string{"Prelude"},
				},
			},
			mockError:     nil,
			expectedError: false,
			expectedText:  "J.R.R. Tolkien",
		},
		{
			name: "database error",
			request: mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Name:      "browse_audiobook_library",
					Arguments: map[string]interface{}{},
				},
			}, mockEntries: []db.HierarchicalEntry{},
			mockError:     errors.New("database error"),
			expectedError: true,
			expectedText:  "Failed to browse library: database error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &MockDBInterface{}

			mockDB.On("GetHierarchicalData", mock.Anything).
				Return(tt.mockEntries, tt.mockError).Once()

			handler := NewToolHandlers(mockDB)
			result, err := handler.handleBrowseLibrary(context.Background(), tt.request)

			if tt.expectedError {
				if err != nil {
					assert.Error(t, err)
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
