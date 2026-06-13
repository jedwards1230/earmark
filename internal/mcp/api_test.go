package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
)

const testToken = "s3cr3t-control-token"

// muxWithToken wires the full mux with a configured control-API token so the
// authenticated mutation routes are exercisable.
func muxWithToken(mockDB DBInterface, token string) http.Handler {
	return NewMCPServer(mockDB, &config.Config{ControlAPIToken: token}).buildMux()
}

func TestAPIStatusJSON(t *testing.T) {
	h := buildTestMux(&SimpleMockDB{embedBacklog: 3})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var got apiStatus
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// Values come from SimpleMockDB.GetServiceStatus.
	if got.Pending != 2 || got.Done != 10 || got.TotalJobs != 13 {
		t.Errorf("unexpected counts: %+v", got)
	}
	if got.EmbedBacklog != 3 {
		t.Errorf("EmbedBacklog = %d, want 3", got.EmbedBacklog)
	}
	if got.RunLimit != nil {
		t.Errorf("RunLimit = %v, want nil (unlimited)", *got.RunLimit)
	}
	if got.LastHeartbeat == nil {
		t.Errorf("LastHeartbeat unexpectedly nil")
	}
}

func TestAPIStatusNeedsNoToken(t *testing.T) {
	// Even with a token configured, reads are unauthenticated.
	h := muxWithToken(&SimpleMockDB{}, testToken)
	for _, path := range []string{"/api/v1/status", "/api/v1/pipeline/pause"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (no token required)", path, w.Code)
		}
	}
}

func TestAPIPauseGet(t *testing.T) {
	limit := 2
	mock := &SimpleMockDB{paused: true, runLimit: &limit}
	h := buildTestMux(mock)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pipeline/pause", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got pauseState
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Paused {
		t.Errorf("Paused = false, want true")
	}
	if got.RunLimit == nil || *got.RunLimit != 2 {
		t.Errorf("RunLimit = %v, want 2", got.RunLimit)
	}
}

func TestAPIPausePut(t *testing.T) {
	mock := &SimpleMockDB{}
	h := muxWithToken(mock, testToken)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/pipeline/pause",
		strings.NewReader(`{"paused":true}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if mock.pausedSetTo == nil || !*mock.pausedSetTo {
		t.Errorf("SetPaused(true) not called")
	}
	var got pauseState
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Paused {
		t.Errorf("response Paused = false, want true")
	}
}

func TestAPIPutResumeClearsBound(t *testing.T) {
	// A resume (paused:false) must also clear any bounded run.
	limit := 5
	mock := &SimpleMockDB{paused: true, runLimit: &limit}
	h := muxWithToken(mock, testToken)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/pipeline/pause",
		strings.NewReader(`{"paused":false}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !mock.runLimitSet || mock.runLimitSetTo != nil {
		t.Errorf("resume should clear run_limit (SetRunLimit(nil)); set=%v to=%v",
			mock.runLimitSet, mock.runLimitSetTo)
	}
	if mock.pausedSetTo == nil || *mock.pausedSetTo {
		t.Errorf("resume should SetPaused(false)")
	}
}

func TestAPIPutMissingField(t *testing.T) {
	h := muxWithToken(&SimpleMockDB{}, testToken)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/pipeline/pause",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestAPIRunSetsLimitAndUnpauses(t *testing.T) {
	mock := &SimpleMockDB{paused: true}
	h := muxWithToken(mock, testToken)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run",
		strings.NewReader(`{"limit":1}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}
	if !mock.runLimitSet || mock.runLimitSetTo == nil || *mock.runLimitSetTo != 1 {
		t.Errorf("SetRunLimit(1) not called; set=%v to=%v", mock.runLimitSet, mock.runLimitSetTo)
	}
	if mock.pausedSetTo == nil || *mock.pausedSetTo {
		t.Errorf("run should unpause (SetPaused(false))")
	}
	var got pauseState
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RunLimit == nil || *got.RunLimit != 1 || got.Paused {
		t.Errorf("response = %+v, want {paused:false, runLimit:1}", got)
	}
}

func TestAPIRunRejectsBadLimit(t *testing.T) {
	for _, body := range []string{`{"limit":0}`, `{"limit":-3}`, `{}`} {
		mock := &SimpleMockDB{}
		h := muxWithToken(mock, testToken)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run",
			strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, w.Code)
		}
		if mock.runLimitSet {
			t.Errorf("body %s: SetRunLimit should not be called on bad input", body)
		}
	}
}

func TestAPIRunClear(t *testing.T) {
	limit := 4
	mock := &SimpleMockDB{runLimit: &limit}
	h := muxWithToken(mock, testToken)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/pipeline/run", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !mock.runLimitSet || mock.runLimitSetTo != nil {
		t.Errorf("clear should SetRunLimit(nil); set=%v to=%v", mock.runLimitSet, mock.runLimitSetTo)
	}
}

func TestAPIAuthRejectsMissingAndWrongToken(t *testing.T) {
	mutations := []struct {
		method, path, body string
	}{
		{http.MethodPut, "/api/v1/pipeline/pause", `{"paused":true}`},
		{http.MethodPost, "/api/v1/pipeline/run", `{"limit":1}`},
		{http.MethodDelete, "/api/v1/pipeline/run", ``},
	}
	for _, m := range mutations {
		// No Authorization header → 401.
		mock := &SimpleMockDB{}
		h := muxWithToken(mock, testToken)
		req := httptest.NewRequest(m.method, m.path, strings.NewReader(m.body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s no-token: status = %d, want 401", m.method, m.path, w.Code)
		}
		if mock.pausedSetTo != nil || mock.runLimitSet {
			t.Errorf("%s %s: mutation ran despite missing token", m.method, m.path)
		}

		// Wrong token → 401.
		req2 := httptest.NewRequest(m.method, m.path, strings.NewReader(m.body))
		req2.Header.Set("Authorization", "Bearer wrong-token")
		w2 := httptest.NewRecorder()
		h.ServeHTTP(w2, req2)
		if w2.Code != http.StatusUnauthorized {
			t.Errorf("%s %s wrong-token: status = %d, want 401", m.method, m.path, w2.Code)
		}
	}
}

func TestAPIAuthFailsClosedWithoutConfiguredToken(t *testing.T) {
	// No token configured → mutations 503 even if the caller supplies one.
	mock := &SimpleMockDB{}
	h := buildTestMux(mock) // config.Config{} → empty ControlAPIToken
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pipeline/run",
		strings.NewReader(`{"limit":1}`))
	req.Header.Set("Authorization", "Bearer anything")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (fail closed)", w.Code)
	}
	if mock.runLimitSet {
		t.Errorf("mutation ran with no configured token")
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":   "abc",
		"bearer abc":   "abc", // case-insensitive scheme
		"Bearer  abc ": "abc", // trimmed
		"Basic abc":    "",
		"abc":          "",
		"":             "",
	}
	for header, want := range cases {
		if got := bearerToken(header); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}
