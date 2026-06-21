package mcp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/predict"
)

// errInvalidJSON is returned for any malformed control-request body; the raw
// decoder error is logged but not surfaced (it can echo request bytes).
var errInvalidJSON = errors.New("invalid JSON body")

// JSON control API (/api/v1/*).
//
// This is the script/agent-facing control surface for the pipeline, distinct
// from the htmx dashboard actions in dashboard.go (which are guarded by the
// HX-Request header and return HTML fragments). Here:
//
//   - Reads (GET) are unauthenticated — status is non-sensitive.
//   - Mutations (PUT/POST/DELETE) require a bearer token (requireToken) and fail
//     closed (503) when no token is configured, so the pipeline can never be
//     paused or driven by an unauthenticated caller.
//
// Pause and the bounded-run counter are orthogonal axes of runner_control:
//
//	pause   → paused=true   (run_limit untouched; paused alone stops claims)
//	resume  → paused=false, run_limit=NULL  ("go, unlimited" — clears any bound)
//	run N   → paused=false, run_limit=N     ("go, exactly N then auto-pause")
//	clear   → run_limit=NULL (paused untouched)
//
// resume/run set the limit before flipping paused=false so there is no window in
// which the runner could claim unbounded work: it stays gated by paused until the
// final write. The runner performs the per-claim decrement, so N is exact.

// apiStatus is the JSON shape of GET /api/v1/status — QueueStats plus the
// pause/run-limit/runner state, camelCased for API consumers.
type apiStatus struct {
	Pending       int     `json:"pending"`
	Claimed       int     `json:"claimed"`
	Done          int     `json:"done"`
	Failed        int     `json:"failed"`
	Transcripts   int     `json:"transcripts"`
	Chunks        int     `json:"chunks"`
	EmbedBacklog  int     `json:"embedBacklog"`
	TotalJobs     int     `json:"totalJobs"`
	DoneLastHour  int     `json:"doneLastHour"`
	Paused        bool    `json:"paused"`
	RunLimit      *int    `json:"runLimit"`
	RunnerActive  bool    `json:"runnerActive"`
	RunnerID      string  `json:"runnerId,omitempty"`
	LastHeartbeat *string `json:"lastHeartbeat,omitempty"`
	// Per-run aggregates (run_metrics); null until the runner/worker populate them.
	AvgProcessingSeconds *float64 `json:"avgProcessingSeconds"`
	TotalEmbedTokens     *int64   `json:"totalEmbedTokens"`
	// ETA is the empirical pipeline ETA (CONTRACT §4). null when no estimate could
	// be computed (no history / predict-inputs read error).
	ETA *apiETA `json:"eta"`
	// Servers is the configured-vs-observed transcription-server view (see the
	// Servers dashboard page). Empty when no servers are configured or observed.
	Servers []apiServer `json:"servers"`
	// Endpoints is the AI endpoint registry (CONTRACT §2.14) with health probes.
	// Always non-empty (at least the embeddings endpoint after config load).
	Endpoints []apiEndpoint `json:"endpoints"`
}

// apiEndpoint is the JSON shape of one AI endpoint in GET /api/v1/status.
// state is a machine token: "ready" | "model_not_loaded" | "offline" |
// "unknown". baseURL is included because it is a LAN-internal, non-secret
// address useful to operators debugging connectivity (see CONTRACT §2.14).
type apiEndpoint struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Backend string            `json:"backend"`
	BaseURL string            `json:"baseURL"`
	Model   string            `json:"model"`
	Options map[string]string `json:"options,omitempty"`
	Role    string            `json:"role,omitempty"`
	State   string            `json:"state"`
	Probed  bool              `json:"probed"`
}

// apiServer is the JSON shape of one transcription server in GET /api/v1/status.
// state is a machine token: "transcribing" | "ready" | "busy" | "stalled" |
// "offline" | "idle" | "not_seen". The gpu-* fields are present only when a
// gpuArbiterUrl is configured for the server — they are the hook a future
// fallback automation reads to decide whether the primary is usable.
type apiServer struct {
	Name        string `json:"name"`
	Host        string `json:"host,omitempty"`
	Role        string `json:"role,omitempty"`
	Configured  bool   `json:"configured"`
	State       string `json:"state"`
	Model       string `json:"model,omitempty"`
	ModelSize   string `json:"modelSize,omitempty"`
	ComputeMode string `json:"computeMode,omitempty"`
	JobsDone    int    `json:"jobsDone"`
	// Backend descriptor (CONTRACT §2.13), resolved observed > configured. All
	// omitempty so old consumers are unaffected and an undescribed server stays
	// minimal. Capabilities is the resolved applied-or-declared map (key→bool);
	// CapsSkippedReason carries the why for any declined (false) capability.
	Family             string            `json:"family,omitempty"`
	Runtime            string            `json:"runtime,omitempty"`
	Capabilities       map[string]bool   `json:"capabilities,omitempty"`
	CapsSkippedReason  map[string]string `json:"capsSkippedReason,omitempty"`
	MeanWordConfidence *float64          `json:"meanWordConfidence,omitempty"`
	// GPU readiness (gpu-arbiter); omitted unless this server has a probe.
	GPUProbed    bool   `json:"gpuProbed,omitempty"`
	GPUReachable bool   `json:"gpuReachable,omitempty"`
	GPUState     string `json:"gpuState,omitempty"`
	VRAMUsedMB   *int   `json:"vramUsedMb,omitempty"`
	VRAMTotalMB  *int   `json:"vramTotalMb,omitempty"`
}

// apiETA is the JSON shape of the empirical pipeline ETA (CONTRACT §4). It
// exposes both the busy-time (workSeconds) and the calendar estimate
// (calendarSeconds, only meaningful when calendarKnown). evalIncluded reports
// whether eval time is part of the estimate (false → eval timing not yet
// measured). label is the human-rendered string the dashboard shows.
type apiETA struct {
	RemainingChunks int     `json:"remainingChunks"`
	WorkSeconds     float64 `json:"workSeconds"`
	CalendarSeconds float64 `json:"calendarSeconds"`
	CalendarKnown   bool    `json:"calendarKnown"`
	EvalIncluded    bool    `json:"evalIncluded"`
	HasWork         bool    `json:"hasWork"`
	Label           string  `json:"label"`
}

// pauseState is the JSON shape of the pipeline pause endpoints.
type pauseState struct {
	Paused   bool `json:"paused"`
	RunLimit *int `json:"runLimit"`
}

// ─── Auth middleware ─────────────────────────────────────────────────────────

// requireToken guards mutating control endpoints with a bearer token. It fails
// closed: when no token is configured the endpoint returns 503 rather than
// silently allowing unauthenticated mutations. The compare is constant-time over
// SHA-256 digests so neither the token value nor its length leaks via timing.
func (s *MCPServer) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.controlToken == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "control API token not configured")
			return
		}
		got := bearerToken(r.Header.Get("Authorization"))
		gotSum := sha256.Sum256([]byte(got))
		wantSum := sha256.Sum256([]byte(s.controlToken))
		if got == "" || subtle.ConstantTimeCompare(gotSum[:], wantSum[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header
// value (case-insensitive scheme). Returns "" if absent or malformed.
func bearerToken(header string) string {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// ─── Handlers ────────────────────────────────────────────────────────────────

// handleAPIStatus serves GET /api/v1/status — the full pipeline snapshot as JSON.
func (s *MCPServer) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.GetServiceStatus(r.Context())
	if err != nil {
		s.logger.Error("api status error", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := apiStatus{
		Pending:      stats.Pending,
		Claimed:      stats.Claimed,
		Done:         stats.Done,
		Failed:       stats.Failed,
		Transcripts:  stats.Transcripts,
		Chunks:       stats.Chunks,
		EmbedBacklog: stats.EmbedBacklog,
		TotalJobs:    stats.TotalJobs,
		DoneLastHour: stats.DoneLastHour,
		Paused:       stats.Paused,
		RunLimit:     stats.RunLimit,
		RunnerActive: stats.RunnerActive,
		RunnerID:     stats.RunnerID,

		AvgProcessingSeconds: stats.AvgProcessingSeconds,
		TotalEmbedTokens:     stats.TotalEmbedTokens,
	}
	if stats.LastHeartbeat != nil {
		hb := stats.LastHeartbeat.UTC().Format("2006-01-02T15:04:05Z07:00")
		out.LastHeartbeat = &hb
	}

	// Empirical ETA (CONTRACT §4). Best-effort: a predict-inputs read error logs
	// and leaves eta null rather than failing the status response.
	if in, perr := s.db.GetPredictInputs(r.Context()); perr != nil {
		s.logger.Warn("api status: predict inputs error; eta omitted", "error", perr)
	} else {
		e := predict.Compute(in)
		out.ETA = &apiETA{
			RemainingChunks: e.RemainingChunks,
			WorkSeconds:     e.WorkSeconds,
			CalendarSeconds: e.CalendarSeconds,
			CalendarKnown:   e.CalendarKnown,
			EvalIncluded:    e.EvalIncluded,
			HasWork:         e.HasWork,
			Label:           e.Label(),
		}
	}

	// Servers view is supplementary: a query error here logs but does not fail the
	// whole status response (the counts above are the primary payload).
	if obs, err := s.db.GetServerObservation(r.Context()); err != nil {
		s.logger.Error("api status servers error", "error", err)
	} else {
		for _, v := range buildServerViews(s.asrServers, obs, s.probeServers(r.Context()), time.Now(), s.runnerStaleAfter) {
			out.Servers = append(out.Servers, apiServer{
				Name:               v.Name,
				Host:               v.Host,
				Role:               v.Role,
				Configured:         v.Configured,
				State:              v.State.Token,
				Model:              v.Model,
				ModelSize:          v.ModelSize,
				ComputeMode:        v.ComputeMode,
				JobsDone:           v.JobsDone,
				Family:             v.Family,
				Runtime:            v.Runtime,
				Capabilities:       v.capsMap(),
				CapsSkippedReason:  v.capsSkippedReasons(),
				MeanWordConfidence: v.MeanConfidence,
				GPUProbed:          v.Probed,
				GPUReachable:       v.Reachable,
				GPUState:           v.GPUState,
				VRAMUsedMB:         v.VRAMUsedMB,
				VRAMTotalMB:        v.VRAMTotalMB,
			})
		}
	}

	// AI endpoint registry (CONTRACT §2.14), merged with liveness probes. Options
	// are emitted from the raw registry (the view drops the map for rendering).
	optsByID := map[string]map[string]string{}
	if s.cfg != nil {
		for _, ep := range s.cfg.AIEndpoints {
			optsByID[ep.ID] = ep.Options
		}
	}
	for _, v := range buildEndpointViews(s.cfg, s.probeEndpoints(r.Context())) {
		out.Endpoints = append(out.Endpoints, apiEndpoint{
			ID:      v.ID,
			Type:    v.Type,
			Backend: v.Backend,
			BaseURL: v.BaseURL,
			Model:   v.Model,
			Options: optsByID[v.ID],
			Role:    v.Role,
			State:   v.StateToken,
			Probed:  v.Probed,
		})
	}

	writeJSON(w, http.StatusOK, out)
}

// handleAPIPauseGet serves GET /api/v1/pipeline/pause — current pause + run-limit.
func (s *MCPServer) handleAPIPauseGet(w http.ResponseWriter, r *http.Request) {
	paused, runLimit, err := s.db.GetControl(r.Context())
	if err != nil {
		s.logger.Error("api pause-get error", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, pauseState{Paused: paused, RunLimit: runLimit})
}

// handleAPIPausePut serves PUT /api/v1/pipeline/pause with body {"paused":bool}.
// paused=true pauses (leaving any bounded run intact); paused=false resumes and
// clears the bound (back to unlimited).
func (s *MCPServer) handleAPIPausePut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused *bool `json:"paused"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Paused == nil {
		writeJSONError(w, http.StatusBadRequest, `field "paused" is required`)
		return
	}
	ctx := r.Context()
	if *body.Paused {
		if err := s.db.SetPaused(ctx, true, "api"); err != nil {
			s.logger.Error("api pause error", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "pause failed")
			return
		}
		s.logger.Info("pipeline paused via control API")
	} else {
		// Resume = clear any bounded run, then unpause (limit first so there is no
		// unbounded-claim window). A resume means "run normally".
		if err := s.db.SetRunLimit(ctx, nil, "api"); err != nil {
			s.logger.Error("api resume (clear limit) error", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "resume failed")
			return
		}
		if err := s.db.SetPaused(ctx, false, "api"); err != nil {
			s.logger.Error("api resume error", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "resume failed")
			return
		}
		s.logger.Info("pipeline resumed via control API")
	}
	s.writeControlState(ctx, w, http.StatusOK)
}

// handleAPIRun serves POST /api/v1/pipeline/run with body {"limit":N}. It starts
// a bounded run of N≥1 claims: sets run_limit=N then unpauses, so the runner
// processes exactly N jobs and then declines further claims (run_limit=0). This
// is the one-call single-job smoke test (limit:1).
func (s *MCPServer) handleAPIRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Limit *int `json:"limit"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Limit == nil || *body.Limit < 1 {
		writeJSONError(w, http.StatusBadRequest, `field "limit" must be an integer >= 1`)
		return
	}
	ctx := r.Context()
	// Limit before unpause: the runner stays gated by paused until the final
	// write, so it can never claim beyond N even if it polls mid-update.
	if err := s.db.SetRunLimit(ctx, body.Limit, "api"); err != nil {
		s.logger.Error("api run (set limit) error", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "run failed")
		return
	}
	if err := s.db.SetPaused(ctx, false, "api"); err != nil {
		s.logger.Error("api run (unpause) error", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "run failed")
		return
	}
	s.logger.Info("bounded run started via control API", "limit", *body.Limit)
	s.writeControlState(ctx, w, http.StatusAccepted)
}

// handleAPIRunClear serves DELETE /api/v1/pipeline/run — clears a bounded run
// (run_limit→NULL) without changing the pause flag.
func (s *MCPServer) handleAPIRunClear(w http.ResponseWriter, r *http.Request) {
	if err := s.db.SetRunLimit(r.Context(), nil, "api"); err != nil {
		s.logger.Error("api run-clear error", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "clear failed")
		return
	}
	s.logger.Info("bounded run cleared via control API")
	s.writeControlState(r.Context(), w, http.StatusOK)
}

// writeControlState re-reads runner_control and writes it as the response body,
// so callers always see the authoritative post-write state.
func (s *MCPServer) writeControlState(ctx context.Context, w http.ResponseWriter, code int) {
	paused, runLimit, err := s.db.GetControl(ctx)
	if err != nil {
		s.logger.Error("api control re-read error", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, code, pauseState{Paused: paused, RunLimit: runLimit})
}

// ─── JSON helpers ────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// decodeJSONBody decodes a small request body, rejecting unknown fields and any
// trailing garbage so malformed control requests fail loudly.
func decodeJSONBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errInvalidJSON
	}
	return nil
}
