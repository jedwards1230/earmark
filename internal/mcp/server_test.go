package mcp

import (
	"bytes"
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
	pingErr       error
	requeuedID    string // last id passed to RequeueByID
	retriedFailed bool   // RequeueFailed was called
}

func (m *SimpleMockDB) RequeueByID(_ context.Context, id string) (string, error) {
	m.requeuedID = id
	return "/books/Author/Book/" + id + ".m4b", nil
}

func (m *SimpleMockDB) RequeueFailed(_ context.Context) ([]string, error) {
	m.retriedFailed = true
	return []string{"/books/Author/Book/chapter03.m4b"}, nil
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
		`src="/static/htmx.min.js"`, // vendored, not a CDN
		`hx-get="/status/data"`,
		`hx-trigger="load, every 3s"`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("GET /: body missing expected marker %q", marker)
		}
	}
	if strings.Contains(body, "unpkg.com") || strings.Contains(body, "//cdn") {
		t.Error("GET /: dashboard must not reference an external CDN")
	}
}

// TestUnmatchedPathIs404 verifies the "/" catch-all doesn't serve the dashboard
// for unmatched paths (e.g. /favicon.ico) — they get a 404, not a 200 page.
func TestUnmatchedPathIs404(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /favicon.ico: want 404, got %d", w.Code)
	}
}

// TestRequeueAction verifies POST /actions/requeue calls RequeueByID and returns
// the refreshed fragment, and that the htmx guard + id validation hold.
func TestRequeueAction(t *testing.T) {
	mock := &SimpleMockDB{}
	h := buildTestMux(mock)

	// Happy path: htmx header + id → 200, RequeueByID called with the id.
	req := httptest.NewRequest(http.MethodPost, "/actions/requeue?id=job-1", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("requeue: want 200, got %d", w.Code)
	}
	if mock.requeuedID != "job-1" {
		t.Errorf("RequeueByID called with %q, want job-1", mock.requeuedID)
	}
	if !strings.Contains(w.Body.String(), "Recent Activity") {
		t.Error("requeue should return the refreshed status fragment")
	}

	// Missing HX-Request header → 403 (CSRF guard).
	mock2 := &SimpleMockDB{}
	req = httptest.NewRequest(http.MethodPost, "/actions/requeue?id=job-1", nil)
	w = httptest.NewRecorder()
	buildTestMux(mock2).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-htmx requeue: want 403, got %d", w.Code)
	}
	if mock2.requeuedID != "" {
		t.Error("non-htmx request must not mutate")
	}

	// Missing id → 400.
	req = httptest.NewRequest(http.MethodPost, "/actions/requeue", nil)
	req.Header.Set("HX-Request", "true")
	w = httptest.NewRecorder()
	buildTestMux(&SimpleMockDB{}).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("requeue without id: want 400, got %d", w.Code)
	}
}

// TestRetryFailedAction verifies POST /actions/retry-failed calls RequeueFailed.
func TestRetryFailedAction(t *testing.T) {
	mock := &SimpleMockDB{}
	req := httptest.NewRequest(http.MethodPost, "/actions/retry-failed", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("retry-failed: want 200, got %d", w.Code)
	}
	if !mock.retriedFailed {
		t.Error("RequeueFailed was not called")
	}
}

// TestRequeueButtonRendersForDoneFailedOnly verifies the per-row requeue button
// appears for done/failed jobs (SimpleMockDB returns a done + a claimed job).
func TestRequeueButtonRendersForDoneFailedOnly(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/status/data", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "/actions/requeue?id=job-1") { // job-1 is 'done'
		t.Error("expected a requeue button for the done job")
	}
	if strings.Contains(body, "/actions/requeue?id=job-2") { // job-2 is 'claimed'
		t.Error("claimed job should not get a requeue button")
	}
}

// TestHTMXAssetServed verifies the vendored htmx library is served locally with
// a JS content-type, so the dashboard has no runtime CDN dependency.
func TestHTMXAssetServed(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/static/htmx.min.js", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /static/htmx.min.js: want 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("want javascript content-type, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "htmx") || w.Body.Len() < 1000 {
		t.Errorf("htmx asset looks wrong: %d bytes", w.Body.Len())
	}
}

// TestDashboardRoutesGETOnly verifies that the dashboard routes reject non-GET
// methods with 405 (they are read-only) while /mcp keeps accepting other methods.
func TestDashboardRoutesGETOnly(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	for _, path := range []string{"/", "/status/data"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: want 405, got %d", path, w.Code)
		}
		if allow := w.Header().Get("Allow"); allow != "GET" {
			t.Errorf("POST %s: want Allow: GET, got %q", path, allow)
		}
	}
}

// TestFragmentEscapesJobError guards the claim that a job's error text is
// HTML-escaped on the dashboard: derefStr returns a plain string, so
// html/template auto-escapes it. A regression that returns template.HTML (or
// otherwise marks it safe) would render the raw markup and fail this test.
func TestFragmentEscapesJobError(t *testing.T) {
	evil := `<script>alert(1)</script>`
	var buf bytes.Buffer
	err := fragmentTmpl.Execute(&buf, dashboardData{
		Stats: &db.QueueStats{},
		Jobs: []db.RecentJob{
			{ID: "x", FilePath: "/books/a/b.m4b", Status: "failed", Error: &evil},
		},
	})
	if err != nil {
		t.Fatalf("fragment execute: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, evil) {
		t.Error("job error rendered unescaped — XSS risk")
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("expected escaped error text in output; got:\n%s", out)
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
