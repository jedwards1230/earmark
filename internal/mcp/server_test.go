package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
)

// SimpleMockDB implements DBInterface for simple testing.
// pingErr controls whether Ping returns an error (nil = healthy).
type SimpleMockDB struct {
	pingErr error
}

func (m *SimpleMockDB) Search(ctx context.Context, query string, limit int, threshold float64) ([]db.SearchResultWithMetadata, error) {
	return []db.SearchResultWithMetadata{
		{
			ID:         "chunk-1",
			Content:    "Test content about dragons",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 0,
			Similarity: 0.85,
		},
	}, nil
}

func (m *SimpleMockDB) TextSearch(ctx context.Context, query string, limit int) ([]db.SearchResultWithMetadata, error) {
	return []db.SearchResultWithMetadata{
		{
			ID:         "chunk-2",
			Content:    "Text search result about dragons",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 0,
		},
	}, nil
}

func (m *SimpleMockDB) GetHierarchicalData(ctx context.Context) ([]db.HierarchicalEntry, error) {
	return []db.HierarchicalEntry{
		{
			FilePath:   "/books/Christopher Paolini/Eragon/chapter1.mp3",
			ChunkCount: 42,
		},
	}, nil
}

func (m *SimpleMockDB) GetChunkContext(ctx context.Context, chunkID string, contextWindow int) ([]db.SearchResultWithMetadata, error) {
	return []db.SearchResultWithMetadata{
		{
			ID:         "chunk-3",
			Content:    "Previous chunk content",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 0,
			ChunkID:    "chunk-3",
		},
		{
			ID:         "chunk-4",
			Content:    "Current chunk content",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 1,
			ChunkID:    "chunk-4",
		},
		{
			ID:         "chunk-5",
			Content:    "Next chunk content",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 2,
			ChunkID:    "chunk-5",
		},
	}, nil
}

// Ping satisfies DBInterface; returns m.pingErr so tests can inject failures.
func (m *SimpleMockDB) Ping(_ context.Context) error {
	return m.pingErr
}

// GetServiceStatus returns a minimal fake QueueStats for unit tests.
func (m *SimpleMockDB) GetServiceStatus(_ context.Context) (*db.QueueStats, error) {
	now := time.Now().UTC()
	runnerID := "asr-runner-test"
	return &db.QueueStats{
		Pending:       2,
		Claimed:       1,
		Done:          10,
		Failed:        0,
		Transcripts:   10,
		Chunks:        450,
		RunnerActive:  true,
		RunnerID:      runnerID,
		LastHeartbeat: &now,
	}, nil
}

// GetRecentJobs returns a small fake job list for unit tests.
func (m *SimpleMockDB) GetRecentJobs(_ context.Context, _ int) ([]db.RecentJob, error) {
	return []db.RecentJob{
		{
			ID:        "job-1",
			FilePath:  "/books/Author/Book/chapter01.m4b",
			Status:    "done",
			UpdatedAt: time.Now().UTC().Add(-5 * time.Minute),
		},
		{
			ID:        "job-2",
			FilePath:  "/books/Author/Book/chapter02.m4b",
			Status:    "claimed",
			UpdatedAt: time.Now().UTC().Add(-30 * time.Second),
		},
	}, nil
}

// buildTestMux returns the http.Handler that StartHTTP would listen on, wired to
// the given mock DB.  It lets us test all routes without binding a port.
func buildTestMux(mockDB DBInterface) http.Handler {
	return NewMCPServer(mockDB, &config.Config{}).buildMux()
}

// TestHealthEndpoint verifies that GET /health always returns 200 "ok".
func TestHealthEndpoint(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /health: want 200, got %d", w.Code)
	}
	if got := w.Body.String(); got != "ok" {
		t.Errorf("GET /health body: want %q, got %q", "ok", got)
	}
}

// TestReadyzEndpoint_Healthy verifies that /readyz returns 200 when the DB pings OK.
func TestReadyzEndpoint_Healthy(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{pingErr: nil})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /readyz (healthy): want 200, got %d", w.Code)
	}
}

// TestReadyzEndpoint_Unhealthy verifies that /readyz returns 503 when the DB is down.
func TestReadyzEndpoint_Unhealthy(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{pingErr: fmt.Errorf("connection refused")})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz (unhealthy): want 503, got %d", w.Code)
	}
}

// TestMCPServerCreation tests that we can create an MCP server without errors
func TestMCPServerCreation(t *testing.T) {
	mockDB := &SimpleMockDB{}
	cfg := &config.Config{}

	server := NewMCPServer(mockDB, cfg)
	if server == nil {
		t.Fatal("Expected server to be created, got nil")
	}

	name, version := server.GetServerInfo()
	expectedName := "lilbro-whisper"
	expectedVersion := "1.0.0"

	if name != expectedName {
		t.Errorf("Expected server name %s, got %s", expectedName, name)
	}
	if version != expectedVersion {
		t.Errorf("Expected server version %s, got %s", expectedVersion, version)
	}

	err := server.Close()
	if err != nil {
		t.Errorf("Expected no error on close, got %v", err)
	}
}

// TestMCPServerTools tests that tools are properly registered
func TestMCPServerTools(t *testing.T) {
	mockDB := &SimpleMockDB{}
	cfg := &config.Config{}

	server := NewMCPServer(mockDB, cfg)
	if server == nil {
		t.Fatal("Expected server to be created, got nil")
	}

	if server.handlers == nil {
		t.Error("Expected handlers to be initialized")
	}

	if server.handlers.db == nil {
		t.Error("Expected database interface to be set in handlers")
	}
}

// TestStartMCPServiceConfiguration tests environment variable handling
func TestStartMCPServiceConfiguration(t *testing.T) {
	mockDB := &SimpleMockDB{}
	cfg := &config.Config{}

	t.Setenv("MCP_TRANSPORT", "unsupported")

	err := StartMCPService(mockDB, cfg)
	if err == nil {
		t.Error("Expected error for unsupported transport, got nil")
	}

	expectedError := "unsupported MCP transport: unsupported (use 'stdio' or 'http')"
	if err.Error() != expectedError {
		t.Errorf("Expected error %q, got %q", expectedError, err.Error())
	}
}

// ─── Dashboard tests ──────────────────────────────────────────────────────────

// TestDashboardPage verifies that GET / returns 200 with the HTML shell.
func TestDashboardPage(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /: want 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("GET /: want text/html content-type, got %q", ct)
	}
	body := w.Body.String()
	for _, marker := range []string{
		"lilbro-whisper",
		"htmx.org",
		`hx-get="/status/data"`,
		`hx-trigger="load, every 3s"`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("GET /: body missing expected marker %q", marker)
		}
	}
}

// TestStatusDataFragment verifies that GET /status/data returns 200 with the
// counts rendered by the fake DB.
func TestStatusDataFragment(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/status/data", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("GET /status/data: want 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Stat values from SimpleMockDB.GetServiceStatus.
	for _, want := range []string{
		">2<",             // Pending
		">1<",             // Claimed
		">10<",            // Done / Transcripts
		">450<",           // Chunks
		"asr-runner-test", // RunnerID
		"chapter01.m4b",   // shortName of first recent job
		"chapter02.m4b",
		`badge done`,
		`badge claimed`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /status/data: body missing %q\nbody:\n%s", want, body)
		}
	}
}
