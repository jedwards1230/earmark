package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

var testNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return testNow.Add(-d) }

// TestJudgeHealthPrecedence covers every row of the Judge precedence table
// (first match wins), including the rows that must NOT win when an earlier one
// applies.
func TestJudgeHealthPrecedence(t *testing.T) {
	base := roleCard{
		Configured: true, CountsKnown: true, Requested: "earmark-judge", Expected: "haiku",
		Answered: "haiku", AnsweredMatch: matchOK, LastOK: ago(10 * time.Minute),
		CoverageDone: 90, CoverageTotal: 100,
	}
	with := func(f func(*roleCard)) roleCard { c := base; f(&c); return c }
	tests := []struct {
		name       string
		card       roleCard
		probe      string
		statsKnown bool
		wantToken  string
		wantSub    string
	}{
		{"not configured", with(func(c *roleCard) { c.Configured = false }), "ready", true, roleNotConfigured, "judging is off"},
		{"probe offline beats a recent failure", with(func(c *roleCard) { c.LastFail = ago(time.Minute) }), "offline", true, roleDown, "gateway unreachable"},
		{"last fail after last ok", with(func(c *roleCard) { c.LastFail = ago(time.Minute) }), "ready", true, roleFailing, "last attempt failed 1m ago"},
		{"failure but never succeeded", with(func(c *roleCard) { c.LastOK, c.LastFail = time.Time{}, ago(time.Minute) }), "", true, roleFailing, "last attempt failed"},
		{"old failure, newer success", with(func(c *roleCard) { c.LastFail = ago(time.Hour) }), "ready", true, roleHealthy, "judged 10m ago"},
		{"model not loaded", base, "model_not_loaded", true, roleDegraded, "gateway does not list earmark-judge"},
		{"answered by the fallback", with(func(c *roleCard) { c.Answered, c.AnsweredMatch = "qwen3.8", matchMismatch }), "ready", true, roleDegraded, "answered by qwen3.8, expected haiku"},
		{"counts unavailable", with(func(c *roleCard) { c.CountsKnown = false }), "ready", true, roleUnknown, "counts query failed"},
		{"stats unavailable", base, "ready", false, roleUnknown, "counts query failed"},
		{"quiet with a gap", with(func(c *roleCard) { c.LastOK = ago(5 * time.Hour) }), "ready", true, roleDegraded, "no successful judge call in 5h; 10 transcripts unjudged"},
		{"never succeeded with a gap", with(func(c *roleCard) { c.LastOK = time.Time{} }), "ready", true, roleDegraded, "no successful judge call yet"},
		{"quiet but nothing to do", with(func(c *roleCard) { c.LastOK, c.CoverageDone = ago(5*time.Hour), 100 }), "ready", true, roleIdle, "every transcript judged"},
		{"empty library", with(func(c *roleCard) { c.CoverageDone, c.CoverageTotal = 0, 0 }), "ready", true, roleIdle, "nothing to judge yet"},
		{"healthy", base, "ready", true, roleHealthy, "judged 10m ago"},
		{"env judge is not probed", base, "", true, roleHealthy, "judged"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := judgeHealth(tc.card, tc.probe, "gw:4000", tc.statsKnown, testNow)
			assert.Equal(t, tc.wantToken, h.Token)
			assert.Contains(t, h.Sub, tc.wantSub)
			assert.NotEmpty(t, h.Glyph, "state must carry a glyph, never color alone")
		})
	}
}

func TestEmbedHealthPrecedence(t *testing.T) {
	base := roleCard{
		Configured: true, CountsKnown: true, Requested: "nomic-embed-text",
		Answered: "nomic-embed-text", AnsweredMatch: matchOK, LastOK: ago(time.Minute), Backlog: 3,
	}
	with := func(f func(*roleCard)) roleCard { c := base; f(&c); return c }
	tests := []struct {
		name       string
		card       roleCard
		probe      string
		statsKnown bool
		wantToken  string
		wantSub    string
	}{
		{"not configured", with(func(c *roleCard) { c.Configured = false }), "", true, roleNotConfigured, "no embeddings endpoint"},
		{"offline", base, "offline", true, roleDown, "endpoint unreachable"},
		{"model not loaded", base, "model_not_loaded", true, roleDegraded, "does not list nomic-embed-text"},
		{"embed model differs", with(func(c *roleCard) { c.Answered, c.AnsweredMatch = "mxbai", matchMismatch }), "ready", true, roleDegraded, "embedded by mxbai"},
		{"unreported model is not a mismatch", with(func(c *roleCard) { c.Answered, c.AnsweredMatch = "", matchUnreported }), "ready", true, roleHealthy, "3 waiting"},
		{"stats unavailable", base, "ready", false, roleUnknown, "queue stats unavailable"},
		{"stalled backlog", with(func(c *roleCard) { c.LastOK = ago(2 * time.Hour) }), "ready", true, roleDegraded, "3 waiting, nothing embedded in 2h"},
		{"backlog, never embedded", with(func(c *roleCard) { c.LastOK = time.Time{} }), "ready", true, roleDegraded, "nothing embedded yet"},
		{"idle", with(func(c *roleCard) { c.Backlog = 0; c.LastOK = ago(48 * time.Hour) }), "ready", true, roleIdle, "nothing waiting"},
		{"healthy", base, "ready", true, roleHealthy, "embedded 1m ago"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := embedHealth(tc.card, tc.probe, "gw:4000", tc.statsKnown, testNow)
			assert.Equal(t, tc.wantToken, h.Token)
			assert.Contains(t, h.Sub, tc.wantSub)
		})
	}
}

func TestASRHealthPrecedence(t *testing.T) {
	sv := func(name, token string, probed bool, gpu string) serverView {
		return serverView{Name: name, Probed: probed, GPUState: gpu, State: serverState{Token: token, Sub: "idle — last active 2h ago"}}
	}
	tests := []struct {
		name      string
		servers   []serverView
		wantToken string
		wantSub   string
	}{
		{"transcribing beats stalled", []serverView{sv("a", "stalled", false, ""), sv("b", "transcribing", false, "")}, roleHealthy, "transcribing on b"},
		{"stalled", []serverView{sv("a", "stalled", false, ""), sv("b", "ready", true, "available")}, roleFailing, "a holds a claim"},
		{"ready", []serverView{sv("a", "ready", true, "available"), sv("b", "busy", true, "gaming")}, roleHealthy, "ready on a"},
		{"all probed busy", []serverView{sv("a", "busy", true, "gaming"), sv("b", "busy", true, "evicting")}, roleDegraded, "a: GPU held by a game (gaming) — jobs wait"},
		{"busy plus offline", []serverView{sv("a", "offline", true, ""), sv("b", "busy", true, "evicting")}, roleDegraded, "evicting other GPU work"},
		{"all probed offline", []serverView{sv("a", "offline", true, ""), sv("b", "offline", true, "")}, roleDown, "unreachable"},
		{"idle", []serverView{sv("a", "idle", false, "")}, roleIdle, "a idle — last active 2h ago"},
		{"none", nil, roleNotConfigured, "no ASR_SERVERS"},
		{"only not-seen", []serverView{sv("a", "not_seen", false, "")}, roleUnknown, "no runner activity"},
	}
	// Held GPUs with nothing queued are not degrading anything: IDLE.
	h := asrHealth([]serverView{sv("a", "busy", true, "gaming")}, 0, true)
	assert.Equal(t, roleIdle, h.Token)
	assert.Contains(t, h.Sub, "nothing queued")
	// Unknown queue depth: stay conservative (DEGRADED).
	assert.Equal(t, roleDegraded, asrHealth([]serverView{sv("a", "busy", true, "gaming")}, 0, false).Token)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := asrHealth(tc.servers, 5, true)
			assert.Equal(t, tc.wantToken, h.Token)
			assert.Contains(t, h.Sub, tc.wantSub)
		})
	}
}

func TestModelsMatch(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"nomic-embed-text", "nomic-embed-text", true},
		{"nomic-embed-text", "nomic-embed-text:latest", true},
		{"nomic-embed-text:latest", "nomic-embed-text", true},
		{"qwen3.8:14b", "qwen3.8", true},
		{"Anthropic/Claude-Haiku", "anthropic/claude-haiku", true},
		{" qwen3.8 ", "qwen3.8", true},
		{"qwen3.8", "qwen3.80", false},
		{"anthropic/claude-haiku-4-5", "qwen3.8", false},
		{"", "", false},
		{"", "qwen3.8", false},
		{"qwen3.8", "", false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, modelsMatch(tc.a, tc.b), "modelsMatch(%q, %q)", tc.a, tc.b)
	}
	// modelLoaded keeps its tag-tolerant behavior on top of modelsMatch.
	assert.True(t, modelLoaded(map[string]bool{"nomic-embed-text:latest": true}, "nomic-embed-text"))
	assert.False(t, modelLoaded(map[string]bool{"other": true}, "nomic-embed-text"))
}

func TestAnsweredMatch(t *testing.T) {
	assert.Equal(t, matchNone, answeredMatch("", "x", false))
	assert.Equal(t, matchUnreported, answeredMatch("", "x", true))
	assert.Equal(t, matchUnchecked, answeredMatch("x", "", true))
	assert.Equal(t, matchOK, answeredMatch("X:latest", "x", true))
	assert.Equal(t, matchMismatch, answeredMatch("y", "x", true))
}

func TestGatewayFor(t *testing.T) {
	tests := []struct {
		declared, url string
		want          string
		inferred      bool
	}{
		{"litellm", "http://10.0.0.5:4000/v1", "litellm", false},
		{"portkey", "http://litellm.lan:4000/v1", "portkey", false},
		{"", "http://litellm.lan:4000/v1", "litellm", true},
		{"", "http://llm-gateway.svc:4000/v1", "litellm", true},
		{"", "llm-gateway.svc:4000", "litellm", true},
		{"", "http://ollama:11434/v1", "", false},
	}
	for _, tc := range tests {
		gw, inf := gatewayFor(tc.declared, tc.url)
		assert.Equal(t, tc.want, gw, tc.url)
		assert.Equal(t, tc.inferred, inf, tc.url)
	}
}

// modelsCountingDB counts snapshot refreshes and can be made to fail.
type modelsCountingDB struct {
	SimpleMockDB
	mu    sync.Mutex
	calls int
	fail  bool
}

func (m *modelsCountingDB) GetModelActivity(context.Context) (db.ModelActivity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.fail {
		return db.ModelActivity{}, errors.New("db down")
	}
	return db.ModelActivity{EvalLastModel: "haiku"}, nil
}

func TestModelsSnapshotCache(t *testing.T) {
	clock := testNow
	c := newModelsSnapshotCache(30 * time.Second)
	c.now = func() time.Time { return clock }
	d := &modelsCountingDB{}
	ctx := context.Background()

	s1, err := c.get(ctx, d)
	require.NoError(t, err)
	require.NotNil(t, s1)
	assert.Equal(t, testNow, s1.At)
	assert.Equal(t, 1, d.calls)

	clock = clock.Add(10 * time.Second) // within TTL → cached
	s2, err := c.get(ctx, d)
	require.NoError(t, err)
	assert.Same(t, s1, s2)
	assert.Equal(t, 1, d.calls)

	clock = clock.Add(25 * time.Second) // past TTL → refresh
	_, err = c.get(ctx, d)
	require.NoError(t, err)
	assert.Equal(t, 2, d.calls)
	good := c.last

	// A failed refresh keeps the last good snapshot and reports the error.
	d.fail = true
	clock = clock.Add(31 * time.Second)
	s3, err := c.get(ctx, d)
	require.Error(t, err)
	assert.Same(t, good, s3, "keep-last-good on error")
	assert.Equal(t, 3, d.calls)

	// Retry-after: no new attempt within a TTL of the failure.
	clock = clock.Add(5 * time.Second)
	s4, err := c.get(ctx, d)
	require.Error(t, err)
	assert.Same(t, good, s4)
	assert.Equal(t, 3, d.calls, "a broken DB must not be re-queried on every poll")

	// After the retry window, a recovered DB refreshes and clears the error.
	d.fail = false
	clock = clock.Add(30 * time.Second)
	s5, err := c.get(ctx, d)
	require.NoError(t, err)
	assert.NotSame(t, good, s5)
	assert.Equal(t, 4, d.calls)
}

func TestModelsSnapshotCache_NeverGood(t *testing.T) {
	c := newModelsSnapshotCache(30 * time.Second)
	c.now = func() time.Time { return testNow }
	snap, err := c.get(context.Background(), &modelsCountingDB{fail: true})
	require.Error(t, err)
	assert.Nil(t, snap)
}

// A cancelled request context must not poison the cache for other viewers.
func TestModelsSnapshotCache_DetachedFromRequestCancel(t *testing.T) {
	c := newModelsSnapshotCache(30 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	snap, err := c.get(ctx, &ctxCheckingDB{})
	require.NoError(t, err)
	require.NotNil(t, snap)
}

type ctxCheckingDB struct{ SimpleMockDB }

func (ctxCheckingDB) GetModelActivity(ctx context.Context) (db.ModelActivity, error) {
	return db.ModelActivity{}, ctx.Err()
}

func TestBuildRecipeRows(t *testing.T) {
	cfg := &config.Config{Models: &config.ModelRegistry{Steps: map[string]config.ModelPin{
		recipe.StepPropose: {ExpectedModel: "haiku"},
		recipe.StepEmbed:   {ExpectedModel: "other"},
	}}}
	snap := &modelsSnapshot{
		Recipes: []db.CurrentRecipe{
			{Step: recipe.StepPropose, RecipeID: strings.Repeat("a", 64), ModelAlias: "earmark-judge", ModelResolved: "haiku"},
			{Step: recipe.StepEmbed, RecipeID: strings.Repeat("b", 64), ModelAlias: "nomic"},
		},
		Stale: map[string]int64{recipe.StepPropose: 5, recipe.StepEmbed: 0},
	}
	rows := buildRecipeRows(cfg, snap)
	require.Len(t, rows, len(recipe.Steps))
	byStep := map[string]recipeRow{}
	for _, r := range rows {
		byStep[r.Step] = r
	}
	p := byStep[recipe.StepPropose]
	assert.True(t, p.Current)
	assert.Equal(t, "aaaaaaaaaaaa", p.RecipeShort)
	assert.Equal(t, "Judge", p.RoleTitle)
	assert.Equal(t, matchOK, p.PinMatch)
	require.NotNil(t, p.Stale)
	assert.Equal(t, int64(5), *p.Stale)
	assert.Equal(t, "only by re-judging", p.ConvergeNote)

	e := byStep[recipe.StepEmbed]
	assert.Equal(t, "nomic", e.Model, "falls back to the alias when nothing resolved")
	assert.Equal(t, matchMismatch, e.PinMatch)
	assert.Empty(t, e.ConvergeNote, "no note when nothing is stale")

	a := byStep[recipe.StepASR]
	assert.False(t, a.Current)
	assert.Nil(t, a.Stale, "asr has no current recipe → not tracked")
	assert.Contains(t, a.NoneNote, "staleness not tracked")
	assert.Equal(t, "", byStep[recipe.StepScan].RoleTitle)
}

func TestBuildFindingsRows(t *testing.T) {
	rows, total := buildFindingsRows([]db.FindingsModelCount{
		{Model: "qwen", PatchState: "proposed", Count: 10},
		{Model: "qwen", PatchState: "superseded", Count: 2},
		{Model: "qwen", PatchState: "stale", Count: 1},
		{Model: "haiku", PatchState: "proposed", Count: 20},
		{Model: "haiku", PatchState: "accepted", Count: 3},
		{Model: "haiku", PatchState: "reverted", Count: 1},
		{Model: "haiku", PatchState: "unanchorable", Count: 4},
	})
	require.Len(t, rows, 2)
	assert.Equal(t, findingsModelRow{Model: "haiku", Proposed: 20, Unanchorable: 4, Decided: 4, Total: 28}, rows[0])
	assert.Equal(t, findingsModelRow{Model: "qwen", Proposed: 10, Other: 3, Total: 13}, rows[1])
	assert.Equal(t, findingsModelRow{Model: "total", Proposed: 30, Unanchorable: 4, Decided: 4, Other: 3, Total: 41}, total)
}

// TestBuildRoleCards_EnvJudge: a judge configured from EVAL_CHAT_* (not the
// registry) is configured, not probed, and labelled with the env source.
func TestBuildRoleCards_EnvJudge(t *testing.T) {
	cards := buildRoleCards(roleInputs{
		Cfg:          &config.Config{},
		Judge:        judgeConfig{Configured: true, Source: judgeSourceEnv, Model: "qwen3.8", Host: "litellm.lan:4000"},
		ServersKnown: true,
		Stats:        &db.QueueStats{Done: 10, EvalCoverageDone: 10},
		Snap:         &modelsSnapshot{Activity: db.ModelActivity{EvalLastModel: "qwen3.8"}},
		Now:          testNow,
	})
	require.Len(t, cards, 5)
	j := cards[1]
	assert.Equal(t, "judge", j.Key)
	assert.True(t, j.Configured)
	assert.Equal(t, judgeSourceEnv, j.EndpointID)
	assert.Equal(t, "qwen3.8", j.Requested)
	assert.Equal(t, "litellm", j.Gateway)
	assert.True(t, j.GatewayInferred)
	assert.Equal(t, matchOK, j.AnsweredMatch, "unpinned → compared against the request")
	assert.Equal(t, roleIdle, j.Health.Token)

	assert.Equal(t, roleNotConfigured, cards[3].Health.Token) // decide
	require.NotNil(t, cards[3].HumanDecided)
	assert.Equal(t, roleNotConfigured, cards[4].Health.Token) // format
	assert.Nil(t, cards[4].HumanDecided)
}

func TestBuildRoleCards_ServersUnknown(t *testing.T) {
	cards := buildRoleCards(roleInputs{Cfg: &config.Config{}, ServersKnown: false, Now: testNow})
	assert.Equal(t, roleUnknown, cards[0].Health.Token, "a failed runner read is UNKNOWN, not NOT CONFIGURED")
	assert.False(t, cards[0].CountsKnown)
}

// TestServersDataScenarios drives GET /servers/data through the mux for every
// demo scenario, asserting the role board renders the state it fixtures.
func TestServersDataScenarios(t *testing.T) {
	tests := []struct {
		scenario     string
		wantContains []string
		wantAbsent   []string
		wantCount    map[string]int
	}{
		{
			scenario: "active",
			wantContains: []string{"✓ HEALTHY", "transcribing on gpu-1", "via LiteLLM", "also:", "qwen3.8</span> ×3",
				"32,337", "39,644", "counts as of", "id=\"recipe-propose\"", "href=\"#recipe-propose\"",
				"runner version", "not reported (pre-provenance runner)", "AI endpoints (3)", "unbound", "direct",
				"✗ OFFLINE", "29,001", "3,336", "decided by humans", "hx-target=\"#models-region\""},
			wantAbsent: []string{"counts unavailable", "FAILING", "about-page", "Family", "Size"},
		},
		{
			scenario:     "failed",
			wantContains: []string{"✗ FAILING", "invalid LiteLLM virtual key", "12 transcripts currently failing", "see error below"},
		},
		{
			scenario: "stale",
			wantContains: []string{"▲ DEGRADED", "≠ expected anthropic/claude-haiku-4-5-20251001", "answered by qwen3.8",
				"embedded by mxbai-embed-large", "holds a claim with a stale heartbeat", "33,870"},
		},
		{
			scenario:     "idle",
			wantContains: []string{"● IDLE", "every transcript judged", "nothing waiting to embed"},
			wantAbsent:   []string{"FAILING", "DEGRADED"},
		},
		{
			scenario:     "winddown",
			wantContains: []string{"✗ DOWN", "gateway unreachable (GET /models failed) — llm-gateway.demo:4000", "endpoint unreachable"},
		},
		{
			scenario:     "empty",
			wantContains: []string{"no ASR_SERVERS", "judging is off", "no judge findings yet", "no transcripts yet", "_legacy", "Current recipes are registered by"},
			wantAbsent:   []string{"HEALTHY", "via LiteLLM"},
			wantCount:    map[string]int{"○ NOT CONFIGURED": 4},
		},
		{
			scenario:     "multibackend",
			wantContains: []string{"v0.46.0-whisper", "v0.45.2", "4b1e9d0c7a2f", "not reported (pre-provenance runner)"},
		},
		{
			scenario:     "batch-analyze",
			wantContains: []string{"✓ HEALTHY", "32,337"},
		},
		{
			scenario:     demoScenarioSnapshotError,
			wantContains: []string{"counts unavailable", "counts unavailable — see server logs", "call outcomes unavailable", "? UNKNOWN"},
			wantAbsent:   []string{"counts as of", "32,337"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.scenario, func(t *testing.T) {
			srv := newDemoServer(":0", tc.scenario)
			w := httptest.NewRecorder()
			srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/data", nil))
			require.Equal(t, http.StatusOK, w.Code, "the fragment stays 200 even when the snapshot fails")
			out := w.Body.String()
			for _, want := range tc.wantContains {
				assert.Contains(t, out, want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, out, absent)
			}
			for s, n := range tc.wantCount {
				assert.Equal(t, n, strings.Count(out, s), "count of %q", s)
			}
		})
	}
}

// TestServersPageShell: the About text and the focus-pause trigger live in the
// static shell, not the polled fragment.
func TestServersPageShell(t *testing.T) {
	srv := newDemoServer(":0", "active")
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers", nil))
	require.Equal(t, http.StatusOK, w.Code)
	out := w.Body.String()
	assert.Contains(t, out, `id="models-region"`)
	assert.Contains(t, out, `every 5s [!document.querySelector('#models-region form:focus-within')]`)
	assert.Contains(t, out, `class="about-page"`)
	assert.Contains(t, out, "no per-backfill-run marker")
	assert.Contains(t, out, ">Models</a>")
}

// TestServersData_ObservationErrorIs500 keeps the existing contract: only a
// runner-observation failure fails the fragment (htmx then keeps the last swap).
func TestServersData_ObservationErrorIs500(t *testing.T) {
	srv := NewMCPServer(&obsErrDB{}, &config.Config{})
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/data", nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

type obsErrDB struct{ SimpleMockDB }

func (*obsErrDB) GetServerObservation(context.Context) (*db.ServerObservation, error) {
	return nil, errors.New("boom")
}

// TestServersData_HTMLEscapesDBStrings: an eval_error is DB text; it must be
// escaped, never rendered as markup.
func TestServersData_HTMLEscapesDBStrings(t *testing.T) {
	srv := NewMCPServer(&xssDB{}, &config.Config{})
	srv.eval.configured, srv.eval.source, srv.eval.model = true, judgeSourceEnv, "m"
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/data", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "<script>alert(1)</script>")
	assert.Contains(t, w.Body.String(), "&lt;script&gt;")
}

type xssDB struct{ SimpleMockDB }

func (*xssDB) GetModelActivity(context.Context) (db.ModelActivity, error) {
	t := testNow
	return db.ModelActivity{EvalLastFail: &t, EvalLastError: "<script>alert(1)</script>", EvalLastModel: "<b>m</b>"}, nil
}

func TestAPIStatusRolesArray(t *testing.T) {
	get := func(scenario string) apiStatus {
		t.Helper()
		srv := newDemoServer(":0", scenario)
		w := httptest.NewRecorder()
		srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
		require.Equal(t, http.StatusOK, w.Code)
		var got apiStatus
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		return got
	}

	got := get("failed")
	require.Len(t, got.Roles, 5)
	keys := make([]string, 0, 5)
	for _, r := range got.Roles {
		keys = append(keys, r.Role)
	}
	assert.Equal(t, []string{"asr", "judge", "embeddings", "decide", "format"}, keys)

	j := got.Roles[1]
	assert.Equal(t, "propose", j.Step)
	assert.Equal(t, roleFailing, j.State)
	assert.True(t, j.Configured)
	assert.Equal(t, "litellm-judge", j.Endpoint)
	assert.Equal(t, "litellm", j.Gateway)
	assert.False(t, j.GatewayInferred)
	assert.Equal(t, "earmark-judge", j.Requested)
	assert.Equal(t, "anthropic/claude-haiku-4-5-20251001", j.Expected)
	assert.Equal(t, matchOK, j.AnsweredMatch)
	assert.Equal(t, 12, j.FailingNow)
	require.NotNil(t, j.LastError)
	assert.Contains(t, *j.LastError, "invalid LiteLLM virtual key")
	require.NotNil(t, j.LastOKAt)
	require.NotNil(t, j.LastFailedAt)
	require.NotNil(t, j.Stale)
	assert.Equal(t, int64(32_337), *j.Stale)
	require.NotNil(t, j.CountsAsOf)

	assert.Nil(t, got.Roles[0].Stale, "asr has no current recipe → stale null")
	assert.Equal(t, roleNotConfigured, got.Roles[3].State)

	// Endpoint liveness stays liveness: the judge's gateway is READY while the
	// role is FAILING — the exact gap roles[] exists to close.
	for _, e := range got.Endpoints {
		if e.ID == "litellm-judge" {
			assert.Equal(t, "ready", e.State)
			assert.Equal(t, "litellm", e.Gateway)
		}
	}

	// A failed snapshot degrades counts to null, never the response.
	se := get(demoScenarioSnapshotError)
	require.Len(t, se.Roles, 5)
	assert.Nil(t, se.Roles[1].CountsAsOf)
	assert.Nil(t, se.Roles[1].Stale)
	assert.Equal(t, roleUnknown, se.Roles[1].State)
}
