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

	"github.com/jedwards1230/earmark/internal/db"
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
	// RunnerUpdate is the version-skew + self-update state (CONTRACT §2.12). null
	// until the runner reports a version or an update is requested.
	RunnerUpdate *apiRunnerUpdate `json:"runnerUpdate,omitempty"`
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
	// Their state is liveness only (GET /models) — see Roles for call outcomes.
	Endpoints []apiEndpoint `json:"endpoints"`
	// Roles is the Models page's role board (CONTRACT §2.14): one entry per
	// pipeline role (asr, judge, embeddings, decide, format) with its health
	// folded from liveness AND call outcomes, what was requested vs pinned vs
	// answered, and the step's stale count. Always five entries.
	Roles []apiRole `json:"roles"`
	// Gateways is every distinct LiteLLM gateway the AI registry routes
	// through, read with earmark's own virtual key: readiness, key alias,
	// status, spend/budget, limits, and the key's allowed models. Empty when
	// no endpoint is behind LiteLLM.
	Gateways []apiGateway `json:"gateways"`
	// Pipeline is the derived 3-stage lifecycle view (CONTRACT §2.12). It
	// summarises transcribe / eval / embed progress and the GPU commitment so an
	// agent can read one self-describing object rather than piecing it together
	// from Pending/Claimed/EmbedBacklog/Paused.
	Pipeline *pipelineLifecycle `json:"pipeline"`
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
	// Gateway is the declared AI_ENDPOINTS gateway, or "litellm" inferred from
	// the host name (GatewayInferred). Omitted for a direct endpoint.
	Gateway         string `json:"gateway,omitempty"`
	GatewayInferred bool   `json:"gatewayInferred,omitempty"`
}

// apiRole is one pipeline role in GET /api/v1/status roles[] — the same
// builder as the Models page's role cards (buildRoleCards), so the two cannot
// disagree. state is "healthy" | "idle" | "degraded" | "failing" | "down" |
// "not_configured" | "unknown". Timestamps are RFC 3339; null = never or
// unknown. countsAsOf dates the 30 s aggregate snapshot (answered, judge
// lastOkAt/lastFailedAt/lastError/failingNow, ASR lastOkAt); staleAsOf dates
// the separate 5 min stale-count snapshot. Embeddings' lastOkAt comes from the
// live queue stats, not a snapshot.
type apiRole struct {
	Role            string  `json:"role"`
	Step            string  `json:"step"`
	State           string  `json:"state"`
	Reason          string  `json:"reason"`
	Configured      bool    `json:"configured"`
	Endpoint        string  `json:"endpoint,omitempty"`
	Gateway         string  `json:"gateway,omitempty"`
	GatewayInferred bool    `json:"gatewayInferred,omitempty"`
	Requested       string  `json:"requested,omitempty"`
	Expected        string  `json:"expected,omitempty"`
	Answered        string  `json:"answered,omitempty"`
	AnsweredMatch   string  `json:"answeredMatch,omitempty"`
	ModelAllowed    *bool   `json:"modelAllowed"`
	LastOKAt        *string `json:"lastOkAt"`
	LastFailedAt    *string `json:"lastFailedAt"`
	LastError       *string `json:"lastError"`
	FailingNow      *int    `json:"failingNow"`
	Stale           *int64  `json:"stale"`
	CountsAsOf      *string `json:"countsAsOf"`
	CountsError     bool    `json:"countsError"`
	StaleAsOf       *string `json:"staleAsOf"`
}

// apiGateway is one LiteLLM gateway as seen through earmark's virtual key
// (CONTRACT §2.14). It never carries key material.
type apiGateway struct {
	BaseHost      string   `json:"baseHost"`
	Endpoints     []string `json:"endpoints"`
	Ready         bool     `json:"ready"`
	Health        string   `json:"health,omitempty"`
	DB            string   `json:"db,omitempty"`
	Version       string   `json:"version,omitempty"`
	KeyAlias      string   `json:"keyAlias,omitempty"`
	KeyStatus     string   `json:"keyStatus,omitempty"`
	KeyBlocked    bool     `json:"keyBlocked"`
	KeyExpires    string   `json:"keyExpires,omitempty"`
	Spend         *float64 `json:"spend"`
	MaxBudget     *float64 `json:"maxBudget"`
	BudgetResetAt string   `json:"budgetResetAt,omitempty"`
	RPMLimit      *int64   `json:"rpmLimit"`
	TPMLimit      *int64   `json:"tpmLimit"`
	AllowedModels []string `json:"allowedModels"`
	KeyInfoError  string   `json:"keyInfoError,omitempty"`
}

// apiGatewaysFrom maps the probed gateways to the API shape.
func apiGatewaysFrom(sts []gatewayStatus, targets []gatewayTarget) []apiGateway {
	out := make([]apiGateway, 0, len(sts))
	for i, st := range sts {
		g := apiGateway{
			BaseHost: st.Host, Ready: st.Reachable, Health: st.Health, DB: st.DB, Version: st.Version,
			KeyInfoError: st.KeyInfoErr,
		}
		if i < len(targets) {
			for _, ep := range targets[i].Endpoints {
				g.Endpoints = append(g.Endpoints, ep.ID)
			}
		}
		if st.KeyInfoOK {
			k := st.Key
			spend := k.Spend
			g.KeyAlias, g.KeyStatus, g.KeyExpires = k.KeyAlias, k.Status, k.Expires
			g.KeyBlocked = k.Blocked != nil && *k.Blocked
			g.Spend, g.MaxBudget, g.BudgetResetAt = &spend, k.MaxBudget, k.BudgetResetAt
			g.RPMLimit, g.TPMLimit = k.RPMLimit, k.TPMLimit
			g.AllowedModels = append([]string{}, k.Models...)
		}
		out = append(out, g)
	}
	return out
}

// apiRolesFrom maps the role cards to the API shape.
func apiRolesFrom(cards []roleCard, ev modelsEvidence) []apiRole {
	var asOf, staleAsOf *string
	if ev.Snap != nil {
		asOf = rfc3339Ptr(ev.SnapAt)
	}
	if ev.Stale != nil {
		staleAsOf = rfc3339Ptr(ev.StaleAt)
	}
	out := make([]apiRole, 0, len(cards))
	for _, c := range cards {
		r := apiRole{
			Role: c.Key, Step: c.Step, State: c.Health.Token, Reason: c.Health.Sub,
			Configured: c.Configured, Endpoint: c.EndpointID,
			Gateway: c.Gateway, GatewayInferred: c.GatewayInferred,
			Requested: c.Requested, Expected: c.Expected,
			Answered: c.Answered, AnsweredMatch: c.AnsweredMatch, ModelAllowed: c.ModelAllowed,
			LastOKAt: rfc3339Ptr(c.LastOK), LastFailedAt: rfc3339Ptr(c.LastFail),
			Stale: c.Stale, CountsAsOf: asOf, CountsError: ev.SnapErr != nil, StaleAsOf: staleAsOf,
		}
		if c.Key == "judge" && c.CountsKnown {
			n := c.FailingNow
			r.FailingNow = &n
		}
		if c.LastError != "" {
			e := truncateRunes(c.LastError, maxAPIErrorLen)
			r.LastError = &e
		}
		out = append(out, r)
	}
	return out
}

// rfc3339Ptr formats t as RFC 3339 UTC, nil for the zero time.
func rfc3339Ptr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
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

// The request bodies and the error envelope are named types rather than
// anonymous structs or a bare map so that openapi_schemas.go can derive their
// documented shape by reflection — an inline struct has no type to reflect on,
// which is how a request schema silently drifts from what the handler decodes.

// errorEnvelope is the uniform non-2xx body written by writeJSONError.
type errorEnvelope struct {
	Error string `json:"error"`
}

// pauseRequest is the PUT /api/v1/pipeline/pause body. Paused is a pointer so a
// missing field is distinguishable from an explicit false and can be rejected.
type pauseRequest struct {
	Paused *bool `json:"paused"`
}

// runRequest is the POST /api/v1/pipeline/run body. Limit is a pointer for the
// same reason: absent and 0 mean different things.
type runRequest struct {
	Limit *int `json:"limit"`
}

// runnerUpdateRequest is the POST /api/v1/runner/update body. An absent or empty
// Version clears the update request.
type runnerUpdateRequest struct {
	Version *string `json:"version"`
}

// pauseState is the JSON shape of the pipeline pause endpoints.
type pauseState struct {
	Paused   bool `json:"paused"`
	RunLimit *int `json:"runLimit"`
}

// apiRunnerUpdate is the JSON shape of the runner version-skew + self-update
// state (CONTRACT §2.12). RunningVersion is what the runner reports it runs;
// DesiredVersion is what the operator asked for; State is the update state
// machine token; UpdateAvailable is true when a different version is desired.
type apiRunnerUpdate struct {
	RunningVersion  string  `json:"runningVersion,omitempty"`
	DesiredVersion  string  `json:"desiredVersion,omitempty"`
	State           string  `json:"state,omitempty"`
	Error           string  `json:"error,omitempty"`
	UpdateAvailable bool    `json:"updateAvailable"`
	UpdatedAt       *string `json:"updatedAt,omitempty"`
}

// runnerUpdateFromStats builds the runner-update view from the control-row
// state, or nil when nothing is known (no reported version, no request) so the
// field is omitted for runners that predate the feature. UpdateAvailable is true
// whenever a desired version is set and differs from the running one (the button
// authors intent directly; we do not call GitHub from the status path).
func runnerUpdateFromStats(running, desired, state, errMsg *string, at *time.Time) *apiRunnerUpdate {
	rv := derefStr(running)
	dv := derefStr(desired)
	st := derefStr(state)
	if rv == "" && dv == "" && st == "" {
		return nil
	}
	ru := &apiRunnerUpdate{
		RunningVersion:  rv,
		DesiredVersion:  dv,
		State:           st,
		Error:           derefStr(errMsg),
		UpdateAvailable: dv != "" && dv != rv,
	}
	if at != nil {
		s := at.UTC().Format("2006-01-02T15:04:05Z07:00")
		ru.UpdatedAt = &s
	}
	return ru
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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
	out.RunnerUpdate = runnerUpdateFromStats(
		stats.RunnerVersion, stats.DesiredRunnerVersion,
		stats.RunnerUpdateState, stats.RunnerUpdateError, stats.RunnerUpdateAt,
	)

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

	// Pipeline lifecycle (CONTRACT §2.12). Computed from already-read stats plus
	// the coordinator phase + the primary server's gpu-arbiter probe. Best-effort:
	// a phase read error degrades to "idle" without failing the response; the probe
	// may be nil (no servers configured) — lifecycle handles that gracefully.
	{
		phase := db.PhaseIdle
		if p, perr := s.db.GetPipelinePhase(r.Context()); perr != nil {
			s.logger.Warn("api status: GetPipelinePhase error; lifecycle phase degraded", "error", perr)
		} else {
			phase = p
		}
		// Pick the primary server's arbiter probe (if any). "primary" role wins;
		// fall back to the first configured server if none is explicitly primary.
		probes := s.probeServers(r.Context())
		primaryArbiter := arbiterStatus{}
		gpuProbed := false
		for _, c := range s.asrServers {
			if st, ok := probes[c.Name]; ok {
				gpuProbed = true
				primaryArbiter = st
				if c.Role == "primary" {
					break // prefer the primary role
				}
			}
		}
		lc := computePipelineLifecycle(stats, phase, primaryArbiter, gpuProbed, s.evalInPipeline)
		out.Pipeline = &lc
	}

	// Servers view is supplementary: a query error here logs but does not fail the
	// whole status response (the counts above are the primary payload).
	now := time.Now()
	var runners []serverView
	runnersKnown := false
	if obs, err := s.db.GetServerObservation(r.Context()); err != nil {
		s.logger.Error("api status servers error", "error", err)
	} else {
		runners, runnersKnown = buildServerViews(s.asrServers, obs, s.probeServers(r.Context()), now, s.runnerStaleAfter), true
		for _, v := range runners {
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
	eps := buildEndpointViews(s.cfg, s.probeEndpoints(r.Context()))
	for _, v := range eps {
		out.Endpoints = append(out.Endpoints, apiEndpoint{
			ID:              v.ID,
			Type:            v.Type,
			Backend:         v.Backend,
			BaseURL:         v.BaseURL,
			Model:           v.Model,
			Options:         optsByID[v.ID],
			Role:            v.Role,
			State:           v.StateToken,
			Probed:          v.Probed,
			Gateway:         v.Gateway,
			GatewayInferred: v.GatewayInferred,
		})
	}

	// Role board (CONTRACT §2.14): the same builder and 30 s snapshot as the
	// Models page. A snapshot error degrades counts to null, never the response.
	gws, targets, gwByEndpoint := s.probeGateways(r.Context())
	out.Gateways = apiGatewaysFrom(gws, targets)
	roles, ev := s.modelRoles(r.Context(), stats, runners, runnersKnown, eps, gwByEndpoint, now)
	out.Roles = apiRolesFrom(roles, ev)

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
	var body pauseRequest
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
	var body runRequest
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

// handleAPIRunnerUpdate serves POST /api/v1/runner/update with body
// {"version":"<tag>"} — sets desired_runner_version + state='requested' so the
// runner self-updates (CONTRACT §2.12). An empty/omitted version clears the
// request (resets to idle). Bearer-token guarded (fails closed) like the other
// mutating endpoints; the runner — not this handler — performs the swap.
func (s *MCPServer) handleAPIRunnerUpdate(w http.ResponseWriter, r *http.Request) {
	var body runnerUpdateRequest
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	version := ""
	if body.Version != nil {
		version = strings.TrimSpace(*body.Version)
	}
	if version == "" {
		if err := s.db.ClearRunnerUpdate(ctx, "api"); err != nil {
			s.logger.Error("api runner-update clear error", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "clear failed")
			return
		}
		s.logger.Info("runner update request cleared via control API")
	} else {
		if err := s.db.SetDesiredRunnerVersion(ctx, version, "api"); err != nil {
			s.logger.Error("api runner-update error", "error", err)
			writeJSONError(w, http.StatusInternalServerError, "runner update request failed")
			return
		}
		s.logger.Info("runner update requested via control API", "version", version)
	}
	// Echo back the authoritative post-write runner-update state.
	stats, err := s.db.GetServiceStatus(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	ru := runnerUpdateFromStats(stats.RunnerVersion, stats.DesiredRunnerVersion,
		stats.RunnerUpdateState, stats.RunnerUpdateError, stats.RunnerUpdateAt)
	if ru == nil {
		ru = &apiRunnerUpdate{}
	}
	writeJSON(w, http.StatusAccepted, ru)
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
	writeJSON(w, code, errorEnvelope{Error: msg})
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
