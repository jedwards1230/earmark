package mcp

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/predict"
	"github.com/jedwards1230/earmark/internal/eval"
)

// SimpleMockDB implements DBInterface for simple testing.
// pingErr controls whether Ping returns an error (nil = healthy).
type SimpleMockDB struct {
	pingErr         error
	requeueErr      error  // if set, RequeueByID/RequeueFailed return it
	requeuedID      string // last id passed to RequeueByID
	requeuedDir     string // last dir passed to RequeueByDir
	retriedFailed   bool   // RequeueFailed was called
	paused          bool   // current pause flag (mutated by SetPaused)
	pausedSetTo     *bool  // last value passed to SetPaused
	setPausedErr    error  // if set, SetPaused returns it
	runLimit        *int   // current run_limit (mutated by SetRunLimit)
	runLimitSetTo   *int   // last value passed to SetRunLimit (heap copy)
	runLimitSet     bool   // SetRunLimit was called
	setRunLimErr    error  // if set, SetRunLimit returns it
	embedBacklog    int    // EmbedBacklog reported by GetServiceStatus
	lastBookDir     string // last dir passed to GetBookTracks
	lastSearchDir   string // last dir passed to TextSearchInBook
	lastSearchQuery string // last query passed to TextSearchInBook

	clearFindingsCalled bool   // ClearFindings was called
	clearFindingsDir    string // last dir passed to ClearFindings
	clearFindingsN      int64  // rows ClearFindings reports deleted
	clearFindingsErr    error  // if set, ClearFindings returns it

	findingsCountByBook map[string]int // GetFindingsCountByBook result (⚑ column)

	phase string // GetPipelinePhase result ("" → defaults to "idle")

	bookSummaries  []db.BookSummary // when non-nil, GetBookSummaries returns this set (post status/query filter applied here)
	lastBookFilter db.BookFilter    // last filter passed to GetBookSummaries (asserts the all-in-Go fetch uses a high cap)

	findingsList []db.FindingRow // when non-nil, ListFindings returns this set
}

func (m *SimpleMockDB) SetPaused(_ context.Context, paused bool, _ string) error {
	if m.setPausedErr != nil {
		return m.setPausedErr
	}
	m.paused = paused
	m.pausedSetTo = &paused
	return nil
}

func (m *SimpleMockDB) GetControl(_ context.Context) (bool, *int, error) {
	return m.paused, m.runLimit, nil
}

func (m *SimpleMockDB) GetPipelinePhase(_ context.Context) (string, error) {
	if m.phase == "" {
		return "idle", nil
	}
	return m.phase, nil
}

func (m *SimpleMockDB) SetRunLimit(_ context.Context, limit *int, _ string) error {
	if m.setRunLimErr != nil {
		return m.setRunLimErr
	}
	m.runLimit = limit
	m.runLimitSet = true
	if limit != nil {
		v := *limit
		m.runLimitSetTo = &v
	} else {
		m.runLimitSetTo = nil
	}
	return nil
}

func (m *SimpleMockDB) GetBookSummaries(_ context.Context, f db.BookFilter) ([]db.BookSummary, int, error) {
	m.lastBookFilter = f
	if m.bookSummaries != nil {
		return m.bookSummaries, len(m.bookSummaries), nil
	}
	books := []db.BookSummary{
		{Dir: "/books/Author One/Book A", Title: "Book A", Author: "Author One",
			Total: 3, Done: 1, Pending: 2, LastUpdated: time.Now().UTC()},
		{Dir: "/books/Author Two/Book B", Title: "Book B", Author: "Author Two",
			Total: 2, Done: 2, LastUpdated: time.Now().UTC().Add(-time.Hour)},
	}
	// Honor the status filter minimally so the filter path is exercised in tests.
	if f.Status == "pending" {
		books = books[:1]
	}
	return books, len(books), nil
}

func (m *SimpleMockDB) GetLibraryTotals(_ context.Context, _ string) (db.LibraryTotals, error) {
	return db.LibraryTotals{TotalBooks: 2, FullyTranscribed: 1, WithPending: 1}, nil
}

func (m *SimpleMockDB) GetBookTracks(_ context.Context, dir string) ([]db.RecentJob, error) {
	m.lastBookDir = dir
	return []db.RecentJob{
		{ID: "t1", FilePath: dir + "/Track 1.mp3", Status: "done", UpdatedAt: time.Now().UTC()},
		{ID: "t2", FilePath: dir + "/Track 2.mp3", Status: "pending", UpdatedAt: time.Now().UTC()},
	}, nil
}

func (m *SimpleMockDB) GetTrackDetail(_ context.Context, jobID string) (*db.TrackDetail, error) {
	spk := "SPEAKER_00"
	return &db.TrackDetail{
		ID: jobID, FilePath: "/books/Author/Book/" + jobID + ".m4b", Status: "done",
		UpdatedAt: time.Now().UTC(), HasTranscript: true, Language: "en",
		DurationSeconds: 1830, ModelName: "large-v3",
		Segments: []db.Segment{{ID: 0, Start: 0, End: 4.2, Text: "Hello.", Speaker: &spk}},
		Chunks:   []db.ChunkRow{{ChunkIndex: 0, StartSec: 0, EndSec: 90, CharCount: 512, Speaker: &spk}},
	}, nil
}

func (m *SimpleMockDB) RequeueByID(_ context.Context, id string) (string, error) {
	m.requeuedID = id
	if m.requeueErr != nil {
		return "", m.requeueErr
	}
	return "/books/Author/Book/" + id + ".m4b", nil
}

func (m *SimpleMockDB) RequeueFailed(_ context.Context) ([]string, error) {
	m.retriedFailed = true
	if m.requeueErr != nil {
		return nil, m.requeueErr
	}
	return []string{"/books/Author/Book/chapter03.m4b"}, nil
}

func (m *SimpleMockDB) RequeueByDir(_ context.Context, dir string) ([]string, error) {
	m.requeuedDir = dir
	if m.requeueErr != nil {
		return nil, m.requeueErr
	}
	return []string{dir + "/01.mp3", dir + "/02.mp3"}, nil
}

func (m *SimpleMockDB) GetServerObservation(_ context.Context) (*db.ServerObservation, error) {
	return &db.ServerObservation{}, nil
}

func (m *SimpleMockDB) GetFindingsSummary(_ context.Context) (*db.FindingsSummary, error) {
	return &db.FindingsSummary{}, nil
}

func (m *SimpleMockDB) ListFindings(_ context.Context, _ string, _ int) ([]db.FindingRow, error) {
	return m.findingsList, nil
}

func (m *SimpleMockDB) GetFindingsCountByBook(_ context.Context) (map[string]int, error) {
	return m.findingsCountByBook, nil
}

func (m *SimpleMockDB) GetEvalChunksForBook(_ context.Context, _ string, _ int) ([]db.EvalChunk, error) {
	return nil, nil
}

func (m *SimpleMockDB) SampleEvalChunks(_ context.Context, _ int) ([]db.EvalChunk, error) {
	return nil, nil
}

func (m *SimpleMockDB) InsertFindings(_ context.Context, _ []db.Finding) error { return nil }

func (m *SimpleMockDB) ClearFindings(_ context.Context, dir string) (int64, error) {
	m.clearFindingsCalled = true
	m.clearFindingsDir = dir
	if m.clearFindingsErr != nil {
		return 0, m.clearFindingsErr
	}
	return m.clearFindingsN, nil
}

func (m *SimpleMockDB) GetFailedJobs(_ context.Context) ([]db.FailedJob, error) {
	err := "RuntimeError: CUDA out of memory"
	runner := "asr-runner-test"
	return []db.FailedJob{
		{ID: "f1", FilePath: "/books/Author/Book/ch03.m4b", Error: &err, Attempts: 2, ClaimedBy: &runner, UpdatedAt: time.Now().UTC()},
	}, nil
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

func (m *SimpleMockDB) TextSearchInBook(_ context.Context, dir, query string, _ int) ([]db.SearchResultWithMetadata, error) {
	m.lastSearchDir, m.lastSearchQuery = dir, query
	if query == "" {
		return nil, nil
	}
	return []db.SearchResultWithMetadata{
		{ID: "bs1", Content: "match in " + dir, FilePath: dir + "/01.mp3", StartSec: 12.5, EndSec: 18.0},
	}, nil
}

func (m *SimpleMockDB) SearchInBook(_ context.Context, query, dir string, _ int, _ float64) ([]db.SearchResultWithMetadata, error) {
	m.lastSearchDir, m.lastSearchQuery = dir, query
	if query == "" {
		return nil, nil
	}
	return []db.SearchResultWithMetadata{
		{ID: "vs1", Content: "semantic match in " + dir, FilePath: dir + "/01.mp3", StartSec: 12.5, EndSec: 18.0, Similarity: 0.8},
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
		EmbedBacklog:  m.embedBacklog,
		TotalJobs:     13,
		DoneLastHour:  3,
		Paused:        m.paused,
		RunLimit:      m.runLimit,
		RunnerActive:  true,
		RunnerID:      runnerID,
		LastHeartbeat: &now,
	}, nil
}

func (m *SimpleMockDB) GetPredictInputs(_ context.Context) (predict.Inputs, error) {
	return predict.Inputs{
		RemainingChunks: 100,
		Rates: predict.Rates{
			TranscribeSecPerChunk: 7, EmbedSecPerChunk: 0.5,
			EvalSecPerChunk: 1, EvalKnown: true,
		},
		AvailabilityFraction: 0.5,
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

// buildTestMuxWithToken wires the mux with a CONTROL_API_TOKEN set, so the
// token-gated pipeline controls (pause/resume, run-budget) render enabled rather
// than as the honest "disabled — no CONTROL_API_TOKEN" affordance.
func buildTestMuxWithToken(mockDB DBInterface) http.Handler {
	return NewMCPServer(mockDB, &config.Config{ControlAPIToken: "tok"}).buildMux()
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
	expectedName := "earmark"
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
// TestDashboardPage verifies the home page (GET /) is now the Library shell, and
// that it still serves the vendored htmx (no external CDN).
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
		"earmark",
		`src="/static/htmx.min.js"`, // vendored, not a CDN
		`id="library-region"`,       // home is now the Library
		`hx-get="/library/data`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("GET /: body missing expected marker %q", marker)
		}
	}
	if strings.Contains(body, "unpkg.com") || strings.Contains(body, "//cdn") {
		t.Error("GET /: dashboard must not reference an external CDN")
	}
}

// TestPipelinePageShell verifies GET /pipeline serves the Pipeline ops shell:
// the auto-refreshing status region plus the folded-in Failed region.
func TestPipelinePageShell(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/pipeline", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /pipeline: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, marker := range []string{
		`id="data-region"`,
		`hx-get="/status/data"`,
		`hx-trigger="load, every 3s"`,
		`id="failed-region"`, // Failed view folded in as a 2nd region
		`hx-get="/failed/data"`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("GET /pipeline: body missing expected marker %q", marker)
		}
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

// TestRequeueActionErrorSurfaces verifies a failed requeue does not vanish
// silently: the handler returns 200 with HX-Retarget so htmx swaps a visible
// error banner into #action-error (htmx ignores the body of a non-2xx response).
func TestRequeueActionErrorSurfaces(t *testing.T) {
	mock := &SimpleMockDB{requeueErr: fmt.Errorf("boom")}
	req := httptest.NewRequest(http.MethodPost, "/actions/requeue?id=job-1", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("failed requeue: want 200 (so htmx swaps), got %d", w.Code)
	}
	if got := w.Header().Get("HX-Retarget"); got != "#action-error" {
		t.Errorf("HX-Retarget = %q, want #action-error", got)
	}
	if !strings.Contains(w.Body.String(), "action-err") {
		t.Errorf("expected an error banner in body, got %q", w.Body.String())
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
	data := newStatusData(&db.QueueStats{}, []db.RecentJob{
		{ID: "x", FilePath: "/books/a/b.m4b", Status: "failed", Error: &evil},
	}, time.Now(), 30*time.Minute, "", nil)
	if err := statusFragmentTmpl.Execute(&buf, data); err != nil {
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
		`badge claimed`,  // CSS class stays the internal status
		`>transcribing<`, // ...but the visible badge text is the operator word
		"Transcribing",   // card label (was "Claimed")
		"transcription",  // card-group label
		"embedding",      // card-group label
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /status/data: body missing %q\nbody:\n%s", want, body)
		}
	}
	// The redundant Transcripts card (always == Done) is gone.
	if strings.Contains(body, `card-label">Transcripts<`) {
		t.Error("the Transcripts card should have been removed")
	}
}

func TestStatusLabel(t *testing.T) {
	cases := map[string]string{
		"claimed": "transcribing",
		"pending": "pending",
		"done":    "done",
		"failed":  "failed",
	}
	for in, want := range cases {
		if got := statusLabel(in); got != want {
			t.Errorf("statusLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPauseResumeActions verifies the pause/resume buttons toggle the DB pause
// flag and the fragment reflects the new state (htmx-guarded).
func TestPauseResumeActions(t *testing.T) {
	mock := &SimpleMockDB{}

	// Pause: POST /actions/pause with the htmx header. The control buttons are
	// token-gated, so wire a token so the resume button renders in the fragment.
	req := httptest.NewRequest(http.MethodPost, "/actions/pause", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMuxWithToken(mock).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("pause: want 200, got %d", w.Code)
	}
	if mock.pausedSetTo == nil || !*mock.pausedSetTo {
		t.Error("pause did not SetPaused(true)")
	}
	if !strings.Contains(w.Body.String(), "PAUSED") ||
		!strings.Contains(w.Body.String(), "/actions/resume") {
		t.Errorf("paused fragment missing PAUSED state / resume button:\n%s", w.Body.String())
	}

	// Resume: POST /actions/resume clears it.
	req = httptest.NewRequest(http.MethodPost, "/actions/resume", nil)
	req.Header.Set("HX-Request", "true")
	w = httptest.NewRecorder()
	buildTestMuxWithToken(mock).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resume: want 200, got %d", w.Code)
	}
	if mock.pausedSetTo == nil || *mock.pausedSetTo {
		t.Error("resume did not SetPaused(false)")
	}
	if !strings.Contains(w.Body.String(), "RUNNING") ||
		!strings.Contains(w.Body.String(), "/actions/pause") {
		t.Errorf("running fragment missing RUNNING state / pause button:\n%s", w.Body.String())
	}
}

// TestPauseRequiresHTMX verifies the pause action rejects non-htmx posts (CSRF
// guard), like the other mutating actions.
func TestPauseRequiresHTMX(t *testing.T) {
	mock := &SimpleMockDB{}
	req := httptest.NewRequest(http.MethodPost, "/actions/pause", nil) // no HX-Request
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("pause without HX-Request: want 403, got %d", w.Code)
	}
	if mock.pausedSetTo != nil {
		t.Error("SetPaused must not be called on a forbidden request")
	}
}

// TestPauseErrorSurfaces verifies a failed pause surfaces a visible banner
// (200 + HX-Retarget) rather than vanishing.
func TestPauseErrorSurfaces(t *testing.T) {
	mock := &SimpleMockDB{setPausedErr: fmt.Errorf("boom")}
	req := httptest.NewRequest(http.MethodPost, "/actions/pause", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed pause: want 200 (so htmx swaps), got %d", w.Code)
	}
	if got := w.Header().Get("HX-Retarget"); got != "#action-error" {
		t.Errorf("HX-Retarget = %q, want #action-error", got)
	}
	if !strings.Contains(w.Body.String(), "action-err") {
		t.Errorf("expected an error banner, got %q", w.Body.String())
	}
}

// TestEmbedStallWarning verifies a large embed backlog raises the stall callout
// (and the Unembedded card), which is the only place a silent embedding stall is
// visible (job rows stay 'done').
func TestEmbedStallWarning(t *testing.T) {
	// Below threshold: no callout.
	h := buildTestMux(&SimpleMockDB{embedBacklog: embedStallThreshold - 1})
	req := httptest.NewRequest(http.MethodGet, "/status/data", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "stall-callout") {
		t.Error("did not expect a stall callout below threshold")
	}

	// At/above threshold: callout appears.
	h = buildTestMux(&SimpleMockDB{embedBacklog: embedStallThreshold})
	req = httptest.NewRequest(http.MethodGet, "/status/data", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "stall-callout") {
		t.Errorf("expected a stall callout at/above threshold:\n%s", w.Body.String())
	}
}

// TestLibraryDataEndpoint verifies the library fragment lists books with
// resolver-derived author/title, honors the status filter, and links to book
// detail pages.
func TestLibraryDataEndpoint(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})

	// Unfiltered: both books present, with author/title from the resolver and a
	// link to the book detail page.
	req := httptest.NewRequest(http.MethodGet, "/library/data", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /library/data: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Book A", "Book B", "Author One", "Author Two", "/book?dir=", "open"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /library/data: missing %q", want)
		}
	}

	// Status filter narrows to the pending book and marks the chip active.
	req = httptest.NewRequest(http.MethodGet, "/library/data?status=pending", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body = w.Body.String()
	if strings.Contains(body, "Book B") {
		t.Error("status=pending should have filtered out the fully-done Book B")
	}
	if !strings.Contains(body, `chip active`) {
		t.Error("expected the active filter chip to be marked")
	}
}

// TestBookDataEndpoint verifies the book-detail fragment returns the per-track
// rows plus the resolver-derived author/title and a whole-book requeue action.
func TestBookDataEndpoint(t *testing.T) {
	mock := &SimpleMockDB{}
	dir := "/books/Author One/Book A"
	req := httptest.NewRequest(http.MethodGet, "/book/data?dir="+url.QueryEscape(dir), nil)
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /book/data: want 200, got %d", w.Code)
	}
	if mock.lastBookDir != dir {
		t.Errorf("dir not decoded: got %q want %q", mock.lastBookDir, dir)
	}
	body := w.Body.String()
	for _, want := range []string{"Book A", "Author One", "Track 1.mp3", "requeue entire book"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /book/data: missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestBookDataRequiresDir verifies the book fragment 400s without a dir.
func TestBookDataRequiresDir(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/book/data", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET /book/data without dir: want 400, got %d", w.Code)
	}
}

// TestBookPageShell verifies the book detail page shell renders and 400s without
// a dir.
func TestBookPageShell(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/book?dir="+url.QueryEscape("/books/Author One/Book A"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /book: want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "book-region") {
		t.Error("book page shell should contain the book-region container")
	}

	req = httptest.NewRequest(http.MethodGet, "/book", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET /book without dir: want 400, got %d", w.Code)
	}
}

// TestBookRequeueAction verifies the whole-book requeue posts the dir and
// re-renders the book fragment (htmx-guarded).
func TestBookRequeueAction(t *testing.T) {
	mock := &SimpleMockDB{}
	dir := "/books/Author One/Book A"
	req := httptest.NewRequest(http.MethodPost, "/actions/book-requeue?dir="+url.QueryEscape(dir), nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("book-requeue: want 200, got %d", w.Code)
	}
	if mock.requeuedDir != dir {
		t.Errorf("RequeueByDir got %q, want %q", mock.requeuedDir, dir)
	}
	if !strings.Contains(w.Body.String(), "requeue entire book") {
		t.Error("expected the book fragment to be re-rendered after requeue")
	}

	// Without the htmx header → forbidden.
	req = httptest.NewRequest(http.MethodPost, "/actions/book-requeue?dir="+url.QueryEscape(dir), nil)
	w = httptest.NewRecorder()
	buildTestMux(&SimpleMockDB{}).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("book-requeue without HX-Request: want 403, got %d", w.Code)
	}
}

// TestRequeueTrackFromBookPage verifies a per-track requeue carrying a book dir
// re-renders the book fragment instead of the status fragment.
func TestRequeueTrackFromBookPage(t *testing.T) {
	mock := &SimpleMockDB{}
	dir := "/books/Author One/Book A"
	req := httptest.NewRequest(http.MethodPost, "/actions/requeue?id=job-1&book="+url.QueryEscape(dir), nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("track requeue: want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "requeue entire book") {
		t.Error("track requeue with a book param should re-render the book fragment")
	}
}

// TestLibraryPageThreadsStatusFilter is the regression guard for the
// stat-card-link bug: GET /library?status=… must thread the filter into the
// page shell's initial fragment load, not drop it.
func TestLibraryPageThreadsStatusFilter(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})

	// status filter is forwarded
	req := httptest.NewRequest(http.MethodGet, "/library?status=failed", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `/library/data?status=failed`) {
		t.Errorf("library page should thread status into the fragment hx-get:\n%s", w.Body.String())
	}

	// status + search both forwarded (order per url.Values.Encode: q before status)
	req = httptest.NewRequest(http.MethodGet, "/library?status=pending&q=dune", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "q=dune") || !strings.Contains(body, "status=pending") {
		t.Errorf("library page should thread both status and q:\n%s", body)
	}

	// invalid status is dropped → plain /library/data, no query
	req = httptest.NewRequest(http.MethodGet, "/library?status=bogus", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `hx-get="/library/data"`) {
		t.Errorf("invalid status should yield an unfiltered /library/data load:\n%s", w.Body.String())
	}
}

// TestFailedPageRemoved verifies the standalone /failed page route is gone (the
// Failed view is folded into /pipeline), while the /failed/data fragment it loads
// is kept. An unmatched path under the "/" catch-all still 404s.
func TestFailedPageRemoved(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/failed", http.StatusNotFound}, // page route removed
		{"/failed/data", http.StatusOK},  // fragment kept (rendered inside /pipeline)
		{"/pipeline", http.StatusOK},     // the page that now hosts the Failed view
		{"/not-a-real-path", http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("GET %s: want %d, got %d", tc.path, tc.want, w.Code)
		}
	}
}

// TestFailedDataEndpoint verifies the failures fragment renders track-level
// detail: full error (expandable), attempts, claimed_by, requeue.
func TestFailedDataEndpoint(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/failed/data", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /failed/data: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"ch03.m4b",                        // track
		"CUDA out of memory",              // full error
		"<details class=\"error-row\"",    // expandable
		"2/3",                             // attempts
		"asr-runner-test",                 // claimed_by
		"/actions/requeue?id=f1&failed=1", // per-row requeue targets the failed view
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /failed/data: missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestRequeueFromFailedView verifies a requeue carrying failed=1 re-renders the
// failures fragment.
func TestRequeueFromFailedView(t *testing.T) {
	mock := &SimpleMockDB{}
	req := httptest.NewRequest(http.MethodPost, "/actions/requeue?id=f1&failed=1", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("failed-view requeue: want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed jobs") {
		t.Error("requeue with failed=1 should re-render the failures fragment")
	}
}

// TestBookSearchEndpoint verifies POST /search/book?dir=… runs the per-book
// search with the form's q and renders the matching chunk rows scoped to dir.
func TestBookSearchEndpoint(t *testing.T) {
	mock := &SimpleMockDB{}
	form := url.Values{"q": {"dragons"}}
	dir := "/books/audio-libation/Andy Weir/PHM"
	req := httptest.NewRequest(http.MethodPost, "/search/book?dir="+url.QueryEscape(dir),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("book search: want 200, got %d", w.Code)
	}
	if mock.lastSearchDir != dir {
		t.Errorf("search dir = %q, want %q", mock.lastSearchDir, dir)
	}
	if mock.lastSearchQuery != "dragons" {
		t.Errorf("search query = %q, want dragons", mock.lastSearchQuery)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Search results for") || !strings.Contains(body, "match in "+dir) {
		t.Errorf("book search body missing results:\n%s", body)
	}
}

// TestBookSearchEmptyQuery verifies an empty q renders nothing (no search run).
func TestBookSearchEmptyQuery(t *testing.T) {
	mock := &SimpleMockDB{}
	dir := "/books/A/B"
	req := httptest.NewRequest(http.MethodPost, "/search/book?dir="+url.QueryEscape(dir),
		strings.NewReader("q="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("empty book search: want 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "Search results for") {
		t.Error("empty query should render nothing")
	}
}

// TestBookSearchMissingDir verifies a missing dir param is a 400.
func TestBookSearchMissingDir(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/search/book", strings.NewReader("q=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	buildTestMux(&SimpleMockDB{}).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing dir: want 400, got %d", w.Code)
	}
}

// TestTrackSegmentsEndpoint verifies GET /track/segments?id=…&offset=… returns a
// page of the transcript reader (the SimpleMockDB track has one segment).
func TestTrackSegmentsEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/track/segments?id=t1&offset=0", nil)
	w := httptest.NewRecorder()
	buildTestMux(&SimpleMockDB{}).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("track segments: want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "seg-text") {
		t.Errorf("track segments should render seg rows:\n%s", w.Body.String())
	}
}

// ─── Eval-run action handlers (CONTRACT §2.15) ───────────────────────────────

// buildEvalTestMux wires a server with the eval layer "configured" (a recorded
// synchronous run func substituted for the real LLM call) and the given control
// token. recorded captures the RunOptions each triggered run saw; the WaitGroup
// lets a test wait out the background goroutine deterministically.
func buildEvalTestMux(t *testing.T, mockDB DBInterface, token string, configured bool) (http.Handler, *evalRunRecorder) {
	t.Helper()
	srv := NewMCPServer(mockDB, &config.Config{ControlAPIToken: token})
	rec := &evalRunRecorder{}
	if configured {
		srv.eval.configured = true
		srv.eval.run = rec.run
	} else {
		srv.eval.configured = false
		srv.eval.run = nil
	}
	return srv.buildMux(), rec
}

type evalRunRecorder struct {
	mu   sync.Mutex
	opts []eval.RunOptions
	done chan struct{}
}

func (r *evalRunRecorder) run(ctx context.Context, opts eval.RunOptions) (eval.RunStats, error) {
	r.mu.Lock()
	r.opts = append(r.opts, opts)
	ch := r.done
	r.mu.Unlock()
	if ch != nil {
		<-ch // block until the test releases, to exercise the in-flight guard
	}
	return eval.RunStats{ChunksEvaluated: 1, FindingsFound: 1, Persisted: true}, nil
}

func (r *evalRunRecorder) calls() []eval.RunOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]eval.RunOptions, len(r.opts))
	copy(out, r.opts)
	return out
}

// waitForCalls polls until at least n runs have been recorded (the run is async).
func (r *evalRunRecorder) waitForCalls(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if len(r.calls()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d eval run(s), saw %d", n, len(r.calls()))
}

// TestEvalBookAction: POST /actions/eval?dir=… kicks a per-book run with the
// chunk cap, returns the book fragment with an "Evaluating" notice, and is
// htmx-guarded.
func TestEvalBookAction(t *testing.T) {
	h, rec := buildEvalTestMux(t, &SimpleMockDB{}, "tok", true)

	req := httptest.NewRequest(http.MethodPost, "/actions/eval?dir=/books/Author/Book", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("eval book: want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Evaluating") {
		t.Errorf("eval book should return an evaluating notice:\n%s", w.Body.String())
	}
	rec.waitForCalls(t, 1)
	got := rec.calls()[0]
	if got.Book != "/books/Author/Book" {
		t.Errorf("Book = %q, want the dir", got.Book)
	}
	if got.Limit != perBookEvalLimit || !got.Write {
		t.Errorf("opts = %+v, want Limit=%d Write=true", got, perBookEvalLimit)
	}

	// Missing HX-Request header → 403, no run.
	h2, rec2 := buildEvalTestMux(t, &SimpleMockDB{}, "tok", true)
	req = httptest.NewRequest(http.MethodPost, "/actions/eval?dir=/x", nil)
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-htmx eval: want 403, got %d", w.Code)
	}
	if len(rec2.calls()) != 0 {
		t.Error("non-htmx request must not run the judge")
	}
}

// TestEvalSampleAction: POST /actions/eval-sample?n=N clamps n and kicks a
// sample run.
func TestEvalSampleAction(t *testing.T) {
	h, rec := buildEvalTestMux(t, &SimpleMockDB{}, "tok", true)
	req := httptest.NewRequest(http.MethodPost, "/actions/eval-sample?n=999", nil) // over the ceiling
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("eval sample: want 200, got %d", w.Code)
	}
	rec.waitForCalls(t, 1)
	got := rec.calls()[0]
	if got.Sample != maxEvalSampleN {
		t.Errorf("Sample = %d, want clamped to %d", got.Sample, maxEvalSampleN)
	}
	if !got.Write {
		t.Error("sample run should persist (Write=true)")
	}
}

// TestEvalActionFailClosedWithoutToken: an unset CONTROL_API_TOKEN disables the
// eval triggers (fail-closed) even when an endpoint is configured.
func TestEvalActionFailClosedWithoutToken(t *testing.T) {
	h, rec := buildEvalTestMux(t, &SimpleMockDB{}, "", true) // no token
	req := httptest.NewRequest(http.MethodPost, "/actions/eval-sample?n=10", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	// writeActionError returns 200 + HX-Retarget so htmx surfaces the banner.
	if w.Code != http.StatusOK {
		t.Fatalf("fail-closed: want 200 (banner), got %d", w.Code)
	}
	if w.Header().Get("HX-Retarget") != "#action-error" {
		t.Errorf("expected an action-error retarget, headers=%v", w.Header())
	}
	if len(rec.calls()) != 0 {
		t.Error("must not run the judge without a control token")
	}
}

// TestEvalActionNotConfigured: with no eval endpoint, the trigger reports the
// missing endpoint rather than running.
func TestEvalActionNotConfigured(t *testing.T) {
	h, _ := buildEvalTestMux(t, &SimpleMockDB{}, "tok", false) // not configured
	req := httptest.NewRequest(http.MethodPost, "/actions/eval-sample?n=10", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("not-configured: want 200 (banner), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no eval chat endpoint") {
		t.Errorf("expected a not-configured banner:\n%s", w.Body.String())
	}
}

// TestEvalActionInFlightGuard: a second trigger while one run is in flight is
// rejected (no overlapping run), then allowed again once the first finishes.
func TestEvalActionInFlightGuard(t *testing.T) {
	srv := NewMCPServer(&SimpleMockDB{}, &config.Config{ControlAPIToken: "tok"})
	rec := &evalRunRecorder{done: make(chan struct{})}
	srv.eval.configured = true
	srv.eval.run = rec.run
	h := srv.buildMux()

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/actions/eval-sample?n=10", nil)
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	w1 := post()
	if !strings.Contains(w1.Body.String(), "Evaluating") {
		t.Errorf("first run should start:\n%s", w1.Body.String())
	}
	rec.waitForCalls(t, 1) // first run is now blocked inside run()

	w2 := post()
	if !strings.Contains(w2.Body.String(), "already in flight") {
		t.Errorf("second run should be rejected as in-flight:\n%s", w2.Body.String())
	}
	if len(rec.calls()) != 1 {
		t.Errorf("in-flight guard breached: %d runs started", len(rec.calls()))
	}

	close(rec.done) // let the first run finish
	// in-flight flag clears in the goroutine's defer; poll for it.
	for i := 0; i < 200; i++ {
		if !srv.eval.inFlight.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if srv.eval.inFlight.Load() {
		t.Fatal("in-flight flag did not clear after run finished")
	}
}

// ─── Clear-findings action (CONTRACT §2.12) ──────────────────────────────────

// clearMux wires a server with the given control token and a mock DB, returning
// the handler and the mock so a test can assert ClearFindings was invoked.
func clearMux(token string, mock *SimpleMockDB) http.Handler {
	srv := NewMCPServer(mock, &config.Config{ControlAPIToken: token})
	return srv.buildMux()
}

// TestFindingsClearAction: POST /actions/findings-clear (htmx + token) deletes
// findings and re-renders the findings fragment.
func TestFindingsClearAction(t *testing.T) {
	mock := &SimpleMockDB{clearFindingsN: 37}
	h := clearMux("tok", mock)

	req := httptest.NewRequest(http.MethodPost, "/actions/findings-clear", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("clear findings: want 200, got %d", w.Code)
	}
	if !mock.clearFindingsCalled {
		t.Fatal("ClearFindings was not called")
	}
	if mock.clearFindingsDir != "" {
		t.Errorf("unscoped clear should pass empty dir, got %q", mock.clearFindingsDir)
	}
	// Re-renders the findings fragment (the mock summary is empty → zero state).
	if !strings.Contains(w.Body.String(), "updated ") {
		t.Errorf("clear should re-render the findings fragment:\n%s", w.Body.String())
	}
}

// TestFindingsClearScoped: an optional ?dir= scopes the clear to one book.
func TestFindingsClearScoped(t *testing.T) {
	mock := &SimpleMockDB{}
	h := clearMux("tok", mock)
	req := httptest.NewRequest(http.MethodPost, "/actions/findings-clear?dir=/books/Author/Book", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("scoped clear: want 200, got %d", w.Code)
	}
	if mock.clearFindingsDir != "/books/Author/Book" {
		t.Errorf("scoped clear dir = %q, want the book dir", mock.clearFindingsDir)
	}
}

// TestFindingsClearGuards: non-htmx → 403 (no delete); unset token → fail-closed
// banner (no delete).
func TestFindingsClearGuards(t *testing.T) {
	// Missing HX-Request → 403, no delete.
	mock := &SimpleMockDB{}
	h := clearMux("tok", mock)
	req := httptest.NewRequest(http.MethodPost, "/actions/findings-clear", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-htmx clear: want 403, got %d", w.Code)
	}
	if mock.clearFindingsCalled {
		t.Error("non-htmx request must not delete findings")
	}

	// Unset token → fail-closed banner (200 + retarget), no delete.
	mock2 := &SimpleMockDB{}
	h2 := clearMux("", mock2)
	req = httptest.NewRequest(http.MethodPost, "/actions/findings-clear", nil)
	req.Header.Set("HX-Request", "true")
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("fail-closed clear: want 200 (banner), got %d", w.Code)
	}
	if w.Header().Get("HX-Retarget") != "#action-error" {
		t.Errorf("expected action-error retarget, headers=%v", w.Header())
	}
	if mock2.clearFindingsCalled {
		t.Error("must not delete findings without a control token")
	}
}

// ─── Pipeline ops page: phase badge + run-budget controls ────────────────────

// TestStatusFragmentPhaseBadge verifies the status fragment renders the read-only
// coordinator phase badge with the phase word and matching CSS class.
func TestStatusFragmentPhaseBadge(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{phase: "transcribe"})
	req := httptest.NewRequest(http.MethodGet, "/status/data", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /status/data: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`phase-badge`, `phase-transcribe`, `phase: transcribe`} {
		if !strings.Contains(body, want) {
			t.Errorf("status fragment missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestStatusFragmentControlsDisabledWithoutToken verifies the honesty rule: with
// no CONTROL_API_TOKEN the pause + run-budget controls render disabled-with-
// explanation rather than as buttons that would fail-close (503) on click.
func TestStatusFragmentControlsDisabledWithoutToken(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{}) // empty config → no control token
	req := httptest.NewRequest(http.MethodGet, "/status/data", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "ctrl-disabled") {
		t.Errorf("expected a disabled-controls affordance:\n%s", body)
	}
	// No live control buttons should be offered.
	for _, banned := range []string{`/actions/pause`, `/actions/run"`, `/actions/resume`} {
		if strings.Contains(body, banned) {
			t.Errorf("token-less fragment must not offer control button %q", banned)
		}
	}
}

// TestStatusFragmentControlsEnabledWithToken verifies the controls render enabled
// (pause/run buttons present) once a token is configured.
func TestStatusFragmentControlsEnabledWithToken(t *testing.T) {
	h := buildTestMuxWithToken(&SimpleMockDB{})
	req := httptest.NewRequest(http.MethodGet, "/status/data", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{`/actions/pause`, `/actions/run"`, `run budget`} {
		if !strings.Contains(body, want) {
			t.Errorf("token fragment missing enabled control %q\nbody:\n%s", want, body)
		}
	}
}

// TestRunBudgetAction verifies POST /actions/run arms a bounded run: SetRunLimit(n)
// THEN SetPaused(false) (limit before unpause), htmx-guarded + token-gated.
func TestRunBudgetAction(t *testing.T) {
	mock := &SimpleMockDB{}
	req := httptest.NewRequest(http.MethodPost, "/actions/run?n=3", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMuxWithToken(mock).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("run budget: want 200, got %d", w.Code)
	}
	if mock.runLimitSetTo == nil || *mock.runLimitSetTo != 3 {
		t.Errorf("run budget did not SetRunLimit(3); got %v", mock.runLimitSetTo)
	}
	if mock.pausedSetTo == nil || *mock.pausedSetTo {
		t.Error("run budget must unpause (SetPaused(false)) after setting the limit")
	}

	// Non-htmx → 403, no writes.
	mock2 := &SimpleMockDB{}
	req = httptest.NewRequest(http.MethodPost, "/actions/run?n=2", nil)
	w = httptest.NewRecorder()
	buildTestMuxWithToken(mock2).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-htmx run: want 403, got %d", w.Code)
	}
	if mock2.runLimitSet {
		t.Error("non-htmx run must not set a limit")
	}

	// Invalid n → action-error banner, no writes.
	mock3 := &SimpleMockDB{}
	req = httptest.NewRequest(http.MethodPost, "/actions/run?n=0", nil)
	req.Header.Set("HX-Request", "true")
	w = httptest.NewRecorder()
	buildTestMuxWithToken(mock3).ServeHTTP(w, req)
	if mock3.runLimitSet {
		t.Error("invalid n must not set a limit")
	}
	if w.Header().Get("HX-Retarget") != "#action-error" {
		t.Errorf("invalid n: expected action-error retarget, got %v", w.Header())
	}
}

// TestRunBudgetFailClosedWithoutToken verifies the run controls fail closed
// (no write, banner) when CONTROL_API_TOKEN is unset.
func TestRunBudgetFailClosedWithoutToken(t *testing.T) {
	mock := &SimpleMockDB{}
	req := httptest.NewRequest(http.MethodPost, "/actions/run?n=1", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMux(mock).ServeHTTP(w, req) // no token
	if mock.runLimitSet {
		t.Error("run must not set a limit without a control token")
	}
	if w.Header().Get("HX-Retarget") != "#action-error" {
		t.Errorf("expected fail-closed banner, got %v", w.Header())
	}
}

// TestClearBudgetAction verifies POST /actions/run-clear clears the bound
// (SetRunLimit(nil)) without touching the pause flag.
func TestClearBudgetAction(t *testing.T) {
	mock := &SimpleMockDB{}
	req := httptest.NewRequest(http.MethodPost, "/actions/run-clear", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	buildTestMuxWithToken(mock).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("clear budget: want 200, got %d", w.Code)
	}
	if !mock.runLimitSet || mock.runLimitSetTo != nil {
		t.Errorf("clear budget should SetRunLimit(nil); set=%v val=%v", mock.runLimitSet, mock.runLimitSetTo)
	}
	if mock.pausedSetTo != nil {
		t.Error("clear budget must not touch the pause flag")
	}
}

// ─── Topbar phase badge (every page) ─────────────────────────────────────────

// TestTopbarPhaseBadge verifies the read-only phase + paused badge renders in the
// shared topbar on a non-pipeline page (the Library home), linking to /pipeline.
func TestTopbarPhaseBadge(t *testing.T) {
	mock := &SimpleMockDB{phase: "analyze", paused: true}
	h := buildTestMux(mock)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{`topbar-phase`, `href="/pipeline"`, `phase: analyze`, `phase-paused`} {
		if !strings.Contains(body, want) {
			t.Errorf("topbar missing %q\nbody:\n%s", want, body)
		}
	}
}

// ─── Library sort + has-findings filter (all-in-Go) ──────────────────────────

// libBooks is a fixture exercising the all-in-Go sort/filter: three books with
// distinct finding counts, progress, and titles. SamplePath drives the metadata
// resolver; the dirs are generic (no real lab paths).
func libBooks() []db.BookSummary {
	return []db.BookSummary{
		{Dir: "/books/Andy Weir/Project Hail Mary", SamplePath: "/books/Andy Weir/Project Hail Mary/01.m4b",
			Total: 4, Done: 2, Pending: 2},
		{Dir: "/books/Frank Herbert/Dune", SamplePath: "/books/Frank Herbert/Dune/01.m4b",
			Total: 2, Done: 2},
		{Dir: "/books/Carl Sagan/Cosmos", SamplePath: "/books/Carl Sagan/Cosmos/01.m4b",
			Total: 4, Done: 1, Pending: 3},
	}
}

// libFindings is the ⚑ count map: PHM 21, Dune 16, Cosmos 0 (absent → 0).
func libFindingsCounts() map[string]int {
	return map[string]int{
		"/books/Andy Weir/Project Hail Mary": 21,
		"/books/Frank Herbert/Dune":          16,
	}
}

// firstBookDirs extracts the ordered list of distinct book dirs from a library
// fragment body, in render order (the title-link href on each row).
func firstBookDirs(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		i := strings.Index(line, `class="file-name" href="/book?dir=`)
		if i < 0 {
			continue
		}
		rest := line[i+len(`class="file-name" href="/book?dir=`):]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			continue
		}
		dir := rest[:j]
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	return out
}

func TestLibrarySortFindings(t *testing.T) {
	mock := &SimpleMockDB{bookSummaries: libBooks(), findingsCountByBook: libFindingsCounts()}
	h := buildTestMux(mock)
	req := httptest.NewRequest(http.MethodGet, "/library/data?sort=findings", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("sort=findings: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	// PHM (21) must render before Dune (16); both before Cosmos (0).
	iPHM := strings.Index(body, "Project%20Hail%20Mary")
	iDune := strings.Index(body, "Dune")
	iCosmos := strings.Index(body, "Cosmos")
	if iPHM < 0 || iDune < 0 || iCosmos < 0 || iPHM >= iDune || iDune >= iCosmos {
		t.Errorf("sort=findings order wrong: PHM=%d Dune=%d Cosmos=%d", iPHM, iDune, iCosmos)
	}
	// The all-in-Go fetch must pull the full set (high cap), offset 0.
	if mock.lastBookFilter.Limit < 1000 || mock.lastBookFilter.Offset != 0 {
		t.Errorf("expected a full-set fetch (high limit, offset 0); got %+v", mock.lastBookFilter)
	}
}

func TestLibraryHasFindingsFilter(t *testing.T) {
	mock := &SimpleMockDB{bookSummaries: libBooks(), findingsCountByBook: libFindingsCounts()}
	h := buildTestMux(mock)
	req := httptest.NewRequest(http.MethodGet, "/library/data?findings=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	dirs := firstBookDirs(body)
	if len(dirs) != 2 {
		t.Errorf("findings=1 should keep exactly the 2 books with findings, got %d: %v", len(dirs), dirs)
	}
	if strings.Contains(body, "Cosmos") {
		t.Error("findings=1 must drop the zero-finding book (Cosmos)")
	}
	// The active ⚑ chip should be marked active.
	if !strings.Contains(body, "chip-findings active") {
		t.Error("has-findings chip should render active under findings=1")
	}
}

func TestLibrarySortTitle(t *testing.T) {
	mock := &SimpleMockDB{bookSummaries: libBooks(), findingsCountByBook: libFindingsCounts()}
	h := buildTestMux(mock)
	req := httptest.NewRequest(http.MethodGet, "/library/data?sort=title", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	// Title sort keys on the resolved "author title": Andy Weir < Carl Sagan < Frank Herbert.
	iAndy := strings.Index(body, "Project%20Hail%20Mary")
	iCarl := strings.Index(body, "Cosmos")
	iFrank := strings.Index(body, "Dune")
	if iAndy >= iCarl || iCarl >= iFrank {
		t.Errorf("sort=title order wrong: Andy=%d Carl=%d Frank=%d", iAndy, iCarl, iFrank)
	}
}

func TestLibraryPagerCarriesFilters(t *testing.T) {
	// Enough books to force a second page, all with findings so the filter keeps them.
	var books []db.BookSummary
	counts := map[string]int{}
	for i := 0; i < libraryPageSize+5; i++ {
		dir := fmt.Sprintf("/books/Author %02d/Title %02d", i, i)
		books = append(books, db.BookSummary{Dir: dir, SamplePath: dir + "/01.m4b", Total: 1, Done: 1})
		counts[dir] = i + 1
	}
	mock := &SimpleMockDB{bookSummaries: books, findingsCountByBook: counts}
	h := buildTestMux(mock)
	req := httptest.NewRequest(http.MethodGet, "/library/data?sort=findings&findings=1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	// The next-page pager link must carry the active sort + findings filter.
	if !strings.Contains(body, "sort=findings") || !strings.Contains(body, "findings=1") {
		t.Errorf("pager/chip links must carry sort + findings filter:\n%s", body)
	}
	if !strings.Contains(body, "&offset=") {
		t.Error("expected a next-page pager link (offset) for a multi-page result")
	}
}

// ─── Track deep-jump (&t=) ───────────────────────────────────────────────────

// deepJumpMock backs the deep-jump tests with a 72-segment transcript so the
// target lands on a later page (exercising the preload-to-target-page path).
type deepJumpMock struct {
	*SimpleMockDB
}

func (m *deepJumpMock) GetTrackDetail(_ context.Context, jobID string) (*db.TrackDetail, error) {
	segs := make([]db.Segment, 72)
	for i := range segs {
		start := float64(i) * 5.0
		segs[i] = db.Segment{ID: i, Start: start, End: start + 5.0, Text: "seg text"}
	}
	return &db.TrackDetail{
		ID: jobID, FilePath: "/books/A/B/track.m4b", Status: "done", HasTranscript: true,
		Language: "en", DurationSeconds: 360, ModelName: "m", Segments: segs,
	}, nil
}

func TestTrackDeepJumpPreloadsTargetPage(t *testing.T) {
	mock := &deepJumpMock{SimpleMockDB: &SimpleMockDB{}}
	h := NewMCPServer(mock, &config.Config{}).buildMux()
	// t=200 → segment index 39 (segment [195,200) has End==200 ≥ 200), on page 1
	// (39/30); the reader preloads pages [0..1] = segments [0,60).
	req := httptest.NewRequest(http.MethodGet, "/track/data?id=job-1&t=200", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("track deep-jump: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Pages [0..target page] preloaded → seg-0 and the target seg-39 are present.
	if !strings.Contains(body, `id="seg-0"`) || !strings.Contains(body, `id="seg-39"`) {
		t.Errorf("deep-jump should preload through the target segment anchor\n%s", body)
	}
	if strings.Contains(body, `id="seg-60"`) {
		t.Error("deep-jump should not preload beyond the target page")
	}
	// The target segment is highlighted.
	if !strings.Contains(body, `id="seg-39" class="seg seg-active"`) {
		t.Error("deep-jump target segment should carry .seg-active")
	}
	// A scroll script targets the anchor.
	if !strings.Contains(body, `getElementById('seg-39')`) {
		t.Error("deep-jump should emit a scroll-to-anchor script")
	}
}

func TestTrackNoDeepJumpFirstPageOnly(t *testing.T) {
	mock := &deepJumpMock{SimpleMockDB: &SimpleMockDB{}}
	h := NewMCPServer(mock, &config.Config{}).buildMux()
	req := httptest.NewRequest(http.MethodGet, "/track/data?id=job-1", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `id="seg-29"`) {
		t.Error("first page should include seg-29")
	}
	if strings.Contains(body, `id="seg-30"`) {
		t.Error("without t, only the first page should render (no seg-30)")
	}
	if strings.Contains(body, "seg-active") {
		t.Error("without t, no segment should be marked active")
	}
}

// TestFindingsWhereDeepLink verifies the global worklist "Where" cell links to
// /track?id=…&t=startSec when JobID is set, and falls back to a plain span when nil.
func TestFindingsWhereDeepLink(t *testing.T) {
	job := "job-42"
	withJob := []db.FindingRow{{
		ID: "f1", FilePath: "/books/A/B/01.m4b", BookDir: "/books/A/B", JobID: &job,
		StartSec: 145.2, OriginalText: "pin name", IssueType: "homophone", Confidence: 0.6,
	}}
	mock := &SimpleMockDB{
		findingsList: withJob,
	}
	// Give the summary a finding so the worklist renders (TotalFindings>0).
	h := newFindingsTestMux(mock, 1)
	req := httptest.NewRequest(http.MethodGet, "/findings/data", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `href="/track?id=job-42&t=145.2"`) {
		t.Errorf("Where cell should deep-link to the track with t=startSec:\n%s", body)
	}

	// JobID nil → plain span, no /track link.
	noJob := []db.FindingRow{{
		ID: "f2", FilePath: "/books/A/B/02.m4b", BookDir: "/books/A/B", JobID: nil,
		StartSec: 10, OriginalText: "x", IssueType: "homophone", Confidence: 0.6,
	}}
	mock2 := &SimpleMockDB{findingsList: noJob}
	h2 := newFindingsTestMux(mock2, 1)
	req = httptest.NewRequest(http.MethodGet, "/findings/data", nil)
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `href="/track?id=`) {
		t.Error("a nil-JobID finding must not render a /track deep link")
	}
}

// findingsSummaryMock overrides GetFindingsSummary to report a non-zero total so
// the findings worklist (gated on TotalFindings>0) renders.
type findingsSummaryMock struct {
	*SimpleMockDB
	total int
}

func (m *findingsSummaryMock) GetFindingsSummary(context.Context) (*db.FindingsSummary, error) {
	return &db.FindingsSummary{TotalFindings: m.total}, nil
}

func newFindingsTestMux(base *SimpleMockDB, total int) http.Handler {
	return NewMCPServer(&findingsSummaryMock{SimpleMockDB: base, total: total}, &config.Config{}).buildMux()
}
