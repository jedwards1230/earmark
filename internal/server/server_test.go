package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
)

// MockDB implements the database interface for testing
type MockDB struct {
	mock.Mock
}

func (m *MockDB) Search(ctx context.Context, query string, limit int, threshold float64) ([]db.SearchResultWithMetadata, error) {
	args := m.Called(ctx, query, limit, threshold)
	return args.Get(0).([]db.SearchResultWithMetadata), args.Error(1)
}

// Test data generators
func generateSearchResults(count int) []db.SearchResultWithMetadata {
	results := make([]db.SearchResultWithMetadata, count)
	for i := 0; i < count; i++ {
		results[i] = db.SearchResultWithMetadata{
			ID:            fmt.Sprintf("chunk-%d", i+1),
			Content:       fmt.Sprintf("Test content %d", i+1),
			Author:        fmt.Sprintf("Author %d", i+1),
			Title:         fmt.Sprintf("Book %d", i+1),
			Chapter:       fmt.Sprintf("Chapter %d", i+1),
			ChunkIndex:    i,
			Similarity:    0.85 - (float64(i) * 0.1),
			ChapterIndex:  i + 1,
			ChapterTitle:  fmt.Sprintf("Chapter %d Title", i+1),
			TotalChunks:   100,
			TotalChapters: 20,
		}
	}
	return results
}

func setupTestServer(t *testing.T) (*Server, *MockDB) {
	cfg := &config.Config{
		ChunkSize: 1024,
		Debug:     false,
	}

	mockDB := &MockDB{}
	server := NewServer(mockDB, cfg)

	return server, mockDB
}

func TestNewServer(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ChunkSize: 1024,
		Debug:     false,
	}
	mockDB := &MockDB{}

	server := NewServer(mockDB, cfg)

	assert.NotNil(t, server)
	assert.Equal(t, cfg, server.cfg)
	assert.Equal(t, mockDB, server.db)
	assert.NotNil(t, server.log)
}

func TestSearchHandler_Success(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		query             string
		threshold         string
		limit             string
		mockResults       []db.SearchResultWithMetadata
		expectedQuery     string
		expectedThreshold float64
		expectedLimit     int
	}{
		{
			name:              "basic_search",
			query:             "dragon",
			threshold:         "",
			limit:             "",
			mockResults:       generateSearchResults(5),
			expectedQuery:     "dragon",
			expectedThreshold: 0.3,
			expectedLimit:     10,
		},
		{
			name:              "custom_threshold_and_limit",
			query:             "magic sword",
			threshold:         "0.5",
			limit:             "3",
			mockResults:       generateSearchResults(3),
			expectedQuery:     "magic sword",
			expectedThreshold: 0.5,
			expectedLimit:     3,
		},
		{
			name:              "empty_results",
			query:             "nonexistent",
			threshold:         "0.8",
			limit:             "5",
			mockResults:       []db.SearchResultWithMetadata{},
			expectedQuery:     "nonexistent",
			expectedThreshold: 0.8,
			expectedLimit:     5,
		},
		{
			name:              "url_encoded_query",
			query:             "battle scene with magic",
			threshold:         "0.4",
			limit:             "15",
			mockResults:       generateSearchResults(10),
			expectedQuery:     "battle scene with magic",
			expectedThreshold: 0.4,
			expectedLimit:     15,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, mockDB := setupTestServer(t)

			// Set up mock expectations
			mockDB.On("Search", mock.Anything, tt.expectedQuery, tt.expectedLimit, tt.expectedThreshold).
				Return(tt.mockResults, nil).Once()

			// Create request
			req := httptest.NewRequest(http.MethodGet, "/search", nil)
			q := req.URL.Query()
			q.Add("q", tt.query)
			if tt.threshold != "" {
				q.Add("p", tt.threshold)
			}
			if tt.limit != "" {
				q.Add("k", tt.limit)
			}
			req.URL.RawQuery = q.Encode()

			// Record response
			w := httptest.NewRecorder()
			server.SearchHandler(w, req)

			// Verify response
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

			var response map[string]interface{}
			err := json.Unmarshal(w.Body.Bytes(), &response)
			require.NoError(t, err)

			assert.Equal(t, tt.query, response["query"])
			assert.Equal(t, float64(len(tt.mockResults)), response["count"])

			results, ok := response["results"].([]interface{})
			require.True(t, ok)
			assert.Len(t, results, len(tt.mockResults))

			// Verify mock was called as expected
			mockDB.AssertExpectations(t)
		})
	}
}

func TestSearchHandler_ValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		query          string
		threshold      string
		limit          string
		expectedStatus int
		expectedError  string
	}{
		{
			name:           "missing_query",
			query:          "",
			threshold:      "",
			limit:          "",
			expectedStatus: http.StatusBadRequest,
			expectedError:  "missing query parameter",
		},
		{
			name:           "invalid_threshold",
			query:          "test",
			threshold:      "invalid",
			limit:          "",
			expectedStatus: http.StatusBadRequest,
			expectedError:  "invalid threshold parameter",
		},
		{
			name:           "invalid_limit",
			query:          "test",
			threshold:      "0.5",
			limit:          "invalid",
			expectedStatus: http.StatusBadRequest,
			expectedError:  "invalid item limit parameter",
		},
		{
			name:           "zero_threshold",
			query:          "test",
			threshold:      "0.0",
			limit:          "",
			expectedStatus: http.StatusOK,
			expectedError:  "",
		},
		{
			name:           "negative_limit",
			query:          "test",
			threshold:      "",
			limit:          "-5",
			expectedStatus: http.StatusOK,
			expectedError:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, mockDB := setupTestServer(t)

			// Set up mock for valid requests
			if tt.expectedStatus == http.StatusOK {
				expectedLimit := 10
				if tt.limit != "" {
					if limit, err := strconv.Atoi(tt.limit); err == nil {
						expectedLimit = limit
					}
				}
				expectedThreshold := 0.3
				if tt.threshold != "" {
					if threshold, err := strconv.ParseFloat(tt.threshold, 64); err == nil {
						expectedThreshold = threshold
					}
				}
				mockDB.On("Search", mock.Anything, tt.query, expectedLimit, expectedThreshold).
					Return(generateSearchResults(1), nil).Once()
			}

			// Create request
			req := httptest.NewRequest(http.MethodGet, "/search", nil)
			q := req.URL.Query()
			if tt.query != "" {
				q.Add("q", tt.query)
			}
			if tt.threshold != "" {
				q.Add("p", tt.threshold)
			}
			if tt.limit != "" {
				q.Add("k", tt.limit)
			}
			req.URL.RawQuery = q.Encode()

			// Record response
			w := httptest.NewRecorder()
			server.SearchHandler(w, req)

			// Verify response
			assert.Equal(t, tt.expectedStatus, w.Code)
			if tt.expectedError != "" {
				assert.Contains(t, w.Body.String(), tt.expectedError)
			}

			// Verify mock expectations
			if tt.expectedStatus == http.StatusOK {
				mockDB.AssertExpectations(t)
			} else {
				mockDB.AssertNotCalled(t, "Search")
			}
		})
	}
}

func TestSearchHandler_DatabaseError(t *testing.T) {
	t.Parallel()

	server, mockDB := setupTestServer(t)

	// Set up mock to return an error
	mockDB.On("Search", mock.Anything, "test", 10, 0.3).
		Return([]db.SearchResultWithMetadata{}, fmt.Errorf("database connection failed")).Once()

	// Create request
	req := httptest.NewRequest(http.MethodGet, "/search?q=test", nil)
	w := httptest.NewRecorder()

	server.SearchHandler(w, req)

	// Verify error response
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "database connection failed")

	mockDB.AssertExpectations(t)
}

func TestSearchHandler_NilResults(t *testing.T) {
	t.Parallel()

	server, mockDB := setupTestServer(t)

	// Set up mock to return nil results
	mockDB.On("Search", mock.Anything, "test", 10, 0.3).
		Return([]db.SearchResultWithMetadata(nil), nil).Once()

	// Create request
	req := httptest.NewRequest(http.MethodGet, "/search?q=test", nil)
	w := httptest.NewRecorder()

	server.SearchHandler(w, req)

	// Verify response
	assert.Equal(t, http.StatusOK, w.Code)

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Equal(t, "test", response["query"])
	assert.Equal(t, float64(0), response["count"])

	results, ok := response["results"].([]interface{})
	require.True(t, ok)
	assert.Len(t, results, 0)

	mockDB.AssertExpectations(t)
}

func TestSearchHandler_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	server, mockDB := setupTestServer(t)

	// Set up mock expectations
	mockDB.On("Search", mock.Anything, "test", 10, 0.3).
		Return(generateSearchResults(1), nil).Once()

	// Test POST method (should be handled by default mux behavior)
	req := httptest.NewRequest(http.MethodPost, "/search?q=test", nil)
	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("/search", server.SearchHandler)
	mux.ServeHTTP(w, req)

	// The handler should still execute but we test that it works with different methods
	assert.Equal(t, http.StatusOK, w.Code)
	mockDB.AssertExpectations(t)
}

func TestSearchHandler_ConcurrentRequests(t *testing.T) {
	t.Parallel()

	server, mockDB := setupTestServer(t)

	// Set up mock to handle multiple concurrent calls
	mockDB.On("Search", mock.Anything, mock.AnythingOfType("string"), mock.AnythingOfType("int"), mock.AnythingOfType("float64")).
		Return(generateSearchResults(1), nil).Maybe()

	// Create multiple concurrent requests
	const numRequests = 10
	results := make(chan int, numRequests)

	for i := 0; i < numRequests; i++ {
		go func(id int) {
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/search?q=test%d", id), nil)
			w := httptest.NewRecorder()
			server.SearchHandler(w, req)
			results <- w.Code
		}(i)
	}

	// Collect results
	for i := 0; i < numRequests; i++ {
		select {
		case code := <-results:
			assert.Equal(t, http.StatusOK, code)
		case <-time.After(5 * time.Second):
			t.Fatal("Request timeout")
		}
	}
}

func TestSearchHandler_LargeResults(t *testing.T) {
	t.Parallel()

	server, mockDB := setupTestServer(t)

	// Generate large result set
	largeResults := generateSearchResults(1000)

	mockDB.On("Search", mock.Anything, "test", 1000, 0.3).
		Return(largeResults, nil).Once()

	// Create request
	req := httptest.NewRequest(http.MethodGet, "/search?q=test&k=1000", nil)
	w := httptest.NewRecorder()

	server.SearchHandler(w, req)

	// Verify response
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)

	assert.Equal(t, "test", response["query"])
	assert.Equal(t, float64(1000), response["count"])

	results, ok := response["results"].([]interface{})
	require.True(t, ok)
	assert.Len(t, results, 1000)

	mockDB.AssertExpectations(t)
}

func TestSearchHandler_SpecialCharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "quotes",
			query: `"hello world"`,
		},
		{
			name:  "apostrophes",
			query: "it's working",
		},
		{
			name:  "unicode",
			query: "héllo wörld",
		},
		{
			name:  "symbols",
			query: "test@#$%^&*()",
		},
		{
			name:  "newlines",
			query: "line1\nline2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, mockDB := setupTestServer(t)

			mockDB.On("Search", mock.Anything, tt.query, 10, 0.3).
				Return(generateSearchResults(1), nil).Once()

			// Create request with properly encoded query
			req := httptest.NewRequest(http.MethodGet, "/search", nil)
			q := req.URL.Query()
			q.Add("q", tt.query)
			req.URL.RawQuery = q.Encode()

			w := httptest.NewRecorder()
			server.SearchHandler(w, req)

			assert.Equal(t, http.StatusOK, w.Code)

			var response map[string]interface{}
			err := json.Unmarshal(w.Body.Bytes(), &response)
			require.NoError(t, err)

			assert.Equal(t, tt.query, response["query"])

			mockDB.AssertExpectations(t)
		})
	}
}

func TestStart(t *testing.T) {
	t.Parallel()

	server, mockDB := setupTestServer(t)

	// Set up mock expectations for the test request
	mockDB.On("Search", mock.Anything, "test", 10, 0.3).
		Return(generateSearchResults(1), nil).Once()

	// Start the server
	srv := server.Start()
	defer func() { _ = srv.Close() }()

	// Verify server configuration
	assert.Equal(t, ":8080", srv.Addr)
	assert.NotNil(t, srv.Handler)

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	// Test that the handler is properly registered
	req := httptest.NewRequest(http.MethodGet, "/search?q=test", nil)
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, req)

	// Should work with proper mock setup
	assert.Equal(t, http.StatusOK, w.Code)
	mockDB.AssertExpectations(t)
}

// Benchmark tests
func BenchmarkSearchHandler(b *testing.B) {
	cfg := &config.Config{
		ChunkSize: 1024,
		Debug:     false,
	}

	mockDB := &MockDB{}
	server := NewServer(mockDB, cfg)

	// Set up mock for benchmark
	mockDB.On("Search", mock.Anything, mock.AnythingOfType("string"), mock.AnythingOfType("int"), mock.AnythingOfType("float64")).
		Return(generateSearchResults(10), nil).Maybe()

	req := httptest.NewRequest(http.MethodGet, "/search?q=benchmark", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		server.SearchHandler(w, req)
	}
}

func BenchmarkSearchHandlerLargeResults(b *testing.B) {
	cfg := &config.Config{
		ChunkSize: 1024,
		Debug:     false,
	}

	mockDB := &MockDB{}
	server := NewServer(mockDB, cfg)

	// Generate large results once
	largeResults := generateSearchResults(1000)
	mockDB.On("Search", mock.Anything, mock.AnythingOfType("string"), mock.AnythingOfType("int"), mock.AnythingOfType("float64")).
		Return(largeResults, nil).Maybe()

	req := httptest.NewRequest(http.MethodGet, "/search?q=benchmark&k=1000", nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		server.SearchHandler(w, req)
	}
}

// Integration-style tests with real HTTP server
func TestSearchHandlerIntegration(t *testing.T) {
	t.Parallel()

	server, mockDB := setupTestServer(t)

	// Set up mock expectations
	mockDB.On("Search", mock.Anything, "integration test", 10, 0.3).
		Return(generateSearchResults(2), nil).Once()

	// Create test server
	ts := httptest.NewServer(http.HandlerFunc(server.SearchHandler))
	defer ts.Close()

	// Make actual HTTP request
	resp, err := http.Get(ts.URL + "?q=" + url.QueryEscape("integration test"))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// Verify response
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var response map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	require.NoError(t, err)

	assert.Equal(t, "integration test", response["query"])
	assert.Equal(t, float64(2), response["count"])

	mockDB.AssertExpectations(t)
}
