package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

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
		{"offline beats not-allowed", with(func(c *roleCard) { c.ModelAllowed = boolp(false) }), "offline", true, roleDown, "gateway unreachable"},
		{"not on key allowlist beats a failure", with(func(c *roleCard) { c.ModelAllowed, c.LastFail = boolp(false), ago(time.Minute) }), "model_not_loaded", true, roleDegraded, "earmark-judge is not on earmark's LiteLLM key allowlist — every call 403s"},
		{"allowed is no signal", with(func(c *roleCard) { c.ModelAllowed = boolp(true) }), "ready", true, roleHealthy, "judged"},
		{"model missing behind litellm", with(func(c *roleCard) { c.Gateway = "litellm" }), "model_not_loaded", true, roleDegraded, "not on earmark's LiteLLM key allowlist (403 on call)"},
		{"last fail after last ok", with(func(c *roleCard) { c.LastFail = ago(time.Minute) }), "ready", true, roleFailing, "last attempt failed 1m ago"},
		{"failure but never succeeded", with(func(c *roleCard) { c.LastOK, c.LastFail = time.Time{}, ago(time.Minute) }), "", true, roleFailing, "last attempt failed"},
		{"old failure, newer success", with(func(c *roleCard) { c.LastFail = ago(time.Hour) }), "ready", true, roleHealthy, "judged 10m ago"},
		{"model not loaded", base, "model_not_loaded", true, roleDegraded, "endpoint does not list earmark-judge"},
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
		{"not on key allowlist", with(func(c *roleCard) { c.ModelAllowed = boolp(false) }), "ready", true, roleDegraded, "every call 403s"},
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
	// A free GPU whose asr-runner is stopped needs the operator even with an
	// empty queue: DEGRADED, not IDLE (M3).
	stopped := asrHealth([]serverView{sv("a", "busy", true, "available"), sv("b", "busy", true, "gaming")}, 0, true, "")
	assert.Equal(t, roleDegraded, stopped.Token)
	assert.Contains(t, stopped.Sub, "asr-runner stopped on a")
	// …unless `earmark batch` parked it on purpose for the analyze phase.
	parked := asrHealth([]serverView{sv("a", "busy", true, "available")}, 78, true, db.PhaseAnalyze)
	assert.Equal(t, roleIdle, parked.Token)
	assert.Contains(t, parked.Sub, "parked on a for the batch analyze phase")
	// Held GPUs with nothing queued are not degrading anything: IDLE.
	h := asrHealth([]serverView{sv("a", "busy", true, "gaming")}, 0, true, "")
	assert.Equal(t, roleIdle, h.Token)
	assert.Contains(t, h.Sub, "nothing queued")
	// Unknown queue depth: stay conservative (DEGRADED).
	assert.Equal(t, roleDegraded, asrHealth([]serverView{sv("a", "busy", true, "gaming")}, 0, false, "").Token)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := asrHealth(tc.servers, 5, true, "")
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

func TestSameModel(t *testing.T) {
	assert.True(t, sameModel("Qwen3.8", "qwen3.8:latest"))
	assert.True(t, sameModel("anthropic/claude-haiku", "anthropic/claude-haiku"))
	assert.False(t, sameModel("qwen3.8", "qwen3.8:14b"), "a bare pin must not match an arbitrary tag")
	assert.False(t, sameModel("", ""))
	// Provider-prefix-insensitive: LiteLLM answers the bare provider id.
	assert.True(t, sameModel("claude-haiku-5-5", "anthropic/claude-haiku-5-5"))
	assert.True(t, sameModel("Anthropic/Claude-Haiku-5-5", "claude-haiku-5-5"))
	assert.True(t, sameModel("ollama/qwen3.8:latest", "qwen3.8"))
	assert.False(t, sameModel("claude-haiku-5-5", "anthropic/claude-haiku-4-5-20251001"))
	assert.False(t, sameModel("claude-haiku-5-5", "claude-haiku-5-5-20260101"), "a dated snapshot is a different model")
}

// The Judge's "answered ≠ expected" check reads LiteLLM's prefix-less answer
// for an unpinned anthropic/… route as a match, not DEGRADED.
func TestAnsweredMatch_ProviderPrefix(t *testing.T) {
	assert.Equal(t, matchOK, answeredMatch("claude-haiku-5-5", "anthropic/claude-haiku-5-5", true))
	assert.Equal(t, matchMismatch, answeredMatch("claude-haiku-4-5-20251001", "anthropic/claude-haiku-5-5", true))
}

func TestModelAllowed(t *testing.T) {
	tr, fa := true, false
	tests := []struct {
		name    string
		allowed []string
		model   string
		want    *bool
	}{
		{"empty list allows all", nil, "anything", &tr},
		{"exact", []string{"nomic-embed-text", "earmark-judge"}, "earmark-judge", &tr},
		{"case and :latest", []string{"ollama/qwen3.8:latest"}, "OLLAMA/qwen3.8", &tr},
		{"missing", []string{"nomic-embed-text"}, "earmark-judge", &fa},
		{"provider wildcard", []string{"anthropic/*"}, "anthropic/claude-haiku-4-5", &tr},
		{"wildcard other provider", []string{"anthropic/*"}, "openai/gpt-5", &fa},
		{"star", []string{"*"}, "x", &tr},
		{"all-proxy-models", []string{"all-proxy-models"}, "x", &tr},
		{"all-team-models is unknowable", []string{"all-team-models"}, "x", nil},
		{"tag mismatch", []string{"qwen3.8:14b"}, "qwen3.8", &fa},
		// The allowlist stays prefix-SENSITIVE: LiteLLM matches the requested
		// name, so a bare entry does not admit a provider-routed request.
		{"provider prefix is significant", []string{"claude-haiku-5-5"}, "anthropic/claude-haiku-5-5", &fa},
		{"empty model", []string{"x"}, "", &fa},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := modelAllowed(tc.allowed, tc.model)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tc.want, *got)
		})
	}
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

// TestBuildRecipeRows: only steps with a current recipe become rows (their
// stale work is counted); every other step is a label on the one-line
// "Not tracked:" note, in canonical step order.
func TestBuildRecipeRows(t *testing.T) {
	snap := &modelsSnapshot{
		Recipes: []db.CurrentRecipe{
			{Step: recipe.StepPropose, RecipeID: strings.Repeat("a", 64), ModelAlias: "earmark-judge", ModelResolved: "haiku"},
			{Step: recipe.StepEmbed, RecipeID: strings.Repeat("b", 64), ModelAlias: "nomic"},
		},
	}
	stale := &staleSnapshot{Counts: map[string]int64{recipe.StepPropose: 5, recipe.StepEmbed: 0}}

	t.Run("stale counts unavailable", func(t *testing.T) {
		rows, _ := buildRecipeRows(snap, nil)
		require.Len(t, rows, 2, "recipe columns still render")
		for _, r := range rows {
			assert.False(t, r.StaleKnown, r.Step)
			assert.Nil(t, r.Stale, r.Step)
		}
	})

	t.Run("tracked rows and untracked labels", func(t *testing.T) {
		rows, untracked := buildRecipeRows(snap, stale)
		steps := make([]string, 0, len(rows))
		for _, r := range rows {
			steps = append(steps, r.Step)
		}
		assert.Equal(t, []string{recipe.StepPropose, recipe.StepEmbed}, steps, "only tracked steps, canonical order")
		assert.Equal(t, []string{"asr (provenance per transcript)", "decide", "propagate", "scan", "format"}, untracked)

		p := rows[0]
		assert.Equal(t, "aaaaaaaaaaaa", p.RecipeShort)
		assert.Equal(t, "Judge", p.RoleTitle)
		assert.Equal(t, "haiku", p.Model)
		require.NotNil(t, p.Stale)
		assert.Equal(t, int64(5), *p.Stale)
		assert.True(t, p.StaleKnown)
		assert.Equal(t, "only by re-judging", p.ConvergeNote)

		e := rows[1]
		assert.Equal(t, "nomic", e.Model, "falls back to the alias when nothing resolved")
		assert.Empty(t, e.ConvergeNote, "no note when nothing is stale")
	})

	t.Run("no current recipes", func(t *testing.T) {
		rows, untracked := buildRecipeRows(&modelsSnapshot{}, stale)
		assert.Empty(t, rows)
		assert.Len(t, untracked, len(recipe.Steps))
	})
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
				"runner version: running", "not reported (pre-provenance runner)", "AI endpoints (3)", "unbound", "direct",
				"✗ unreachable", "✓ lists model", `<span title="role token: eval">Judge</span>`,
				"29,001", "3,336", "decided by humans", "· counts as of", "· stale counts as of",
				"1 transcript currently failing", "<dt>last error</dt>", "· last fail",
				"LiteLLM gateway", "proxy healthy · db connected", "v1.102.1", "$12.25 · no budget set",
				"<dt>used by</dt><dd>Embeddings, Judge</dd>",
				`<th scope="row" class="mono">propose</th>`, `<h2 class="section-title"`,
				"○ Not configured: Decide", "Not tracked: asr (provenance per transcript), decide, propagate, scan, format",
				`class="badge mismatch" title="a different version is requested">update requested`,
				`title="converges only by re-judging"`, "Avg / job", "Precision", `aria-labelledby="recipes-note"`},
			wantAbsent: []string{"counts unavailable", "FAILING", "about-page", "Family", "Size", "<form",
				"1 transcripts", `<div class="server-sub err" title="chat completion`, "gpu-retired", "NOT ALLOWED",
				// the gateway's role rows and the card's allowlist row only repeat "allowed"
				"✓ allowed", "key allowlist</dt>",
				"○ NOT CONFIGURED", `id="recipe-asr"`, `id="recipe-decide"`, "unpinned", "<caption", "Pin</th>",
				"Backend</th>", "openai-compat", "runner did not report provenance", "last fail <span class=\"time-muted\">never",
				`class="badge unconfigured" title="a different version`, "✓ READY</span>", "AVG PROC", "Mode</th>"},
			wantCount: map[string]int{`class="panel server-card role-card`: 3},
		},
		{
			scenario: "failed",
			wantContains: []string{"✗ FAILING", "invalid LiteLLM virtual key", "12 transcripts currently failing", "see error below",
				`<div class="server-sub err mono err-clamp" title="401 Unauthorized`},
		},
		{
			scenario: "stale",
			wantContains: []string{"▲ DEGRADED", "≠ expected anthropic/claude-haiku-4-5-20251001", "answered by qwen3.8",
				"embedded by mxbai-embed-large", "holds a claim with a stale heartbeat", "33,870"},
		},
		{
			scenario: "idle",
			wantContains: []string{"✓ HEALTHY · idle", "every transcript judged", "nothing waiting to embed",
				`class="panel server-card role-card state-running" id="role-judge"`,
				`class="panel server-card role-card state-running" id="role-embeddings"`,
				"asr-runner stopped on gpu-1", "$12.25 of $50.00 budget, resets in", "rpm 60 · tpm 100,000", "expires in",
				"· <span class=\"time-muted\">no failures</span>"},
			wantAbsent: []string{"FAILING", "● IDLE", "state-idle\" id=\"role-", "last fail"},
		},
		{
			scenario: "winddown",
			wantContains: []string{"✗ DOWN", "gateway unreachable (GET /models failed) — llm-gateway.demo:4000", "endpoint unreachable",
				"readiness and liveliness probes failed", "key info unreachable"},
		},
		{
			scenario: "empty",
			wantContains: []string{"no ASR_SERVERS", "judging is off", "no judge findings yet", "no transcripts yet", "_legacy",
				"No current recipes yet. Current recipes are registered by", "○ Not configured: ASR", ", Format</p>",
				"Not tracked: asr (provenance per transcript), propose, decide, propagate, scan, format, embed"},
			wantAbsent: []string{"via LiteLLM", "LiteLLM gateway", "○ NOT CONFIGURED", `class="recipes-table"`,
				// no endpoint has options and the judge is not env-sourced
				"Options</th>"},
			wantCount: map[string]int{`class="panel server-card role-card`: 1, "○ Not configured:": 1},
		},
		{
			scenario:     "multibackend",
			wantContains: []string{"v0.46.0-whisper", "v0.45.2", "4b1e9d0c7a2f", "not reported (pre-provenance runner)"},
		},
		{
			scenario:     "batch-analyze",
			wantContains: []string{"✓ HEALTHY", "32,337", "asr-runner parked on gpu-1 for the batch analyze phase"},
		},
		{
			scenario: demoScenarioSnapshotError,
			// Only the aggregate snapshot fails: the separately cached stale counts
			// still render.
			wantContains: []string{"counts unavailable", "counts unavailable — see server logs", "call outcomes unavailable", "? UNKNOWN",
				"· stale counts as of", "32,337 rows"},
			// no recipes table to link to: the stale count is plain text
			wantAbsent: []string{"· counts as of", "stale counts unavailable", `href="#recipe-`},
		},
		{
			scenario: demoScenarioGatewayAllowlist,
			wantContains: []string{"▲ DEGRADED", "earmark-judge is not on earmark&#39;s LiteLLM key allowlist — every call 403s",
				"<dt>Judge</dt>", "✗ NOT ALLOWED", "earmark-judge (Judge) is not on the key allowlist — calls 403", "▲ not allowed",
				`title="not on earmark&#39;s LiteLLM key allowlist (403 on call)"`},
		},
		{
			scenario:     demoScenarioGatewayKeyInfo,
			wantContains: []string{"key info not readable by earmark&#39;s key (HTTP 403)", "✓ HEALTHY", "<dt>Judge</dt>", "? not checked"},
			wantAbsent:   []string{"NOT ALLOWED", "$12.25"},
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

// TestServersPageShell: the About text and the runner-update form live in the
// static shell, not the polled fragment, so a poll can neither collapse About
// nor wipe what the operator is typing (the vendored htmx 4 ignores a trigger
// filter on "every", so pausing the poll is not an option).
func TestServersPageShell(t *testing.T) {
	srv := newDemoServer(":0", "active")
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers", nil))
	require.Equal(t, http.StatusOK, w.Code)
	out := w.Body.String()
	assert.Contains(t, out, `id="models-region"`)
	assert.Contains(t, out, `hx-trigger="load, every 5s"`)
	assert.NotContains(t, out, "focus-within")
	assert.Contains(t, out, `class="about-page"`)
	assert.Contains(t, out, "no per-backfill-run marker")
	assert.Contains(t, out, ">Models</a>")
	// The form: token-gated, posts into the region, placeholder only (no value).
	assert.Contains(t, out, `id="runner-version"`)
	assert.Contains(t, out, `placeholder="vX.Y.Z"`)
	assert.Contains(t, out, `hx-post="/actions/runner-update" hx-target="#models-region"`)
	assert.NotContains(t, out, `name="version" value=`)

	noToken := NewMCPServer(&SimpleMockDB{}, &config.Config{})
	w = httptest.NewRecorder()
	noToken.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers", nil))
	assert.NotContains(t, w.Body.String(), `id="runner-version"`)
	assert.Contains(t, w.Body.String(), "Set <code>CONTROL_API_TOKEN</code> to enable runner updates")
}

// TestRunnerUpdateFormOutsidePolledRegion: the runner-update form must never be
// in the polled fragment (a 5 s swap would wipe the typed version); on the full
// page it follows #models-region's closing tag.
func TestRunnerUpdateFormOutsidePolledRegion(t *testing.T) {
	srv := newDemoServer(":0", "active")
	h := srv.buildMux()
	get := func(path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, w.Code, path)
		return w.Body.String()
	}
	frag := get("/servers/data")
	assert.NotContains(t, frag, `id="runner-version"`)
	assert.NotContains(t, frag, "<form")

	page := get("/servers")
	region := strings.Index(page, `<div id="models-region"`)
	require.Positive(t, region)
	regionEnd := strings.Index(page[region:], "</div>")
	require.Positive(t, regionEnd)
	form := strings.Index(page, `id="runner-version"`)
	require.Positive(t, form)
	assert.Greater(t, form, region+regionEnd, "the form follows the closed polled region")
}

// TestServersData_StaleCountFailureIsIsolated: a failing stale count marks only
// the stale numbers unavailable; recipes, activity and findings still render.
func TestServersData_StaleCountFailureIsIsolated(t *testing.T) {
	srv := NewMCPServer(&staleErrDB{}, &config.Config{})
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/data", nil))
	require.Equal(t, http.StatusOK, w.Code)
	out := w.Body.String()
	assert.Contains(t, out, "stale counts unavailable")
	assert.Contains(t, out, "· counts as of")
	assert.Contains(t, out, `id="recipe-propose"`, "the recipes table still renders")
	assert.Regexp(t, `id="recipe-propose">\s*<th scope="row" class="mono">propose</th>\s*<td>Judge</td>\s*<td class="num"><span class="time-muted">—</span></td>`, out,
		"the stale cell is unknown (—), not a count")
	assert.Contains(t, out, "Not tracked: asr")
	assert.NotContains(t, out, "counts unavailable — see server logs")
}

type staleErrDB struct{ SimpleMockDB }

func (*staleErrDB) ListCurrentRecipes(context.Context) ([]db.CurrentRecipe, error) {
	return []db.CurrentRecipe{{Step: recipe.StepPropose, RecipeID: strings.Repeat("a", 64), ModelAlias: "earmark-judge", UpdatedAt: testNow}}, nil
}

func (*staleErrDB) StaleItemCounts(context.Context) (map[string]int64, error) {
	return nil, errors.New("statement timeout")
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
	require.NotNil(t, j.FailingNow)
	assert.Equal(t, 12, *j.FailingNow)
	assert.False(t, j.CountsError)
	require.NotNil(t, j.StaleAsOf)
	require.NotNil(t, j.ModelAllowed)
	assert.True(t, *j.ModelAllowed)
	require.Len(t, got.Gateways, 1)
	g := got.Gateways[0]
	assert.Equal(t, "llm-gateway.demo:4000", g.BaseHost)
	assert.True(t, g.Ready)
	assert.Equal(t, "earmark", g.KeyAlias)
	assert.Equal(t, []string{"litellm-embed", "litellm-judge"}, g.Endpoints)
	require.NotNil(t, g.Spend)
	assert.InDelta(t, 12.25, *g.Spend, 0.001)
	assert.Nil(t, g.MaxBudget)
	assert.Contains(t, g.AllowedModels, "earmark-judge")
	assert.Nil(t, got.Roles[2].FailingNow, "failingNow is judge-only")
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
	assert.True(t, se.Roles[1].CountsError)
	assert.Nil(t, se.Roles[1].FailingNow, "unknown, not 0")
	assert.Equal(t, roleUnknown, se.Roles[1].State)
	require.NotNil(t, se.Roles[1].Stale, "stale counts are cached separately and still load")

	al := get(demoScenarioGatewayAllowlist)
	require.NotNil(t, al.Roles[1].ModelAllowed)
	assert.False(t, *al.Roles[1].ModelAllowed)
	assert.Equal(t, roleDegraded, al.Roles[1].State)
}

// TestModelsCaches_StalePending: a slow first stale count renders as pending
// (not an error) and does not hold up the cheap aggregates.
func TestModelsCaches_StalePending(t *testing.T) {
	d := &slowStaleDB{release: make(chan struct{})}
	c := newModelsCaches(d, nil)
	start := time.Now()
	ev := c.read(context.Background(), 50*time.Millisecond)
	assert.Less(t, time.Since(start), time.Second, "the page must not wait on the stale scan")
	assert.NotNil(t, ev.Snap, "aggregates loaded independently")
	assert.Nil(t, ev.Stale)
	assert.True(t, ev.StalePending)
	assert.NoError(t, ev.StaleErr)

	close(d.release)
	c.stale.waitIdle()
	ev = c.read(context.Background(), 50*time.Millisecond)
	require.NotNil(t, ev.Stale)
	assert.False(t, ev.StalePending)
	assert.Equal(t, int64(7), ev.Stale.Counts["embed"])
}

type slowStaleDB struct {
	SimpleMockDB
	release chan struct{}
}

func (s *slowStaleDB) StaleItemCounts(ctx context.Context) (map[string]int64, error) {
	select {
	case <-s.release:
		return map[string]int64{"embed": 7}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestRunnerUpdateAction covers the htmx runner-update action: hx-post sends
// the form fields in the BODY (the bug was reading only the query string, so
// "Update runner" cleared the request instead of setting it).
func TestRunnerUpdateAction(t *testing.T) {
	post := func(mock *SimpleMockDB, token, body, query string, htmx bool) *httptest.ResponseRecorder {
		t.Helper()
		srv := NewMCPServer(mock, &config.Config{ControlAPIToken: token})
		req := httptest.NewRequest(http.MethodPost, "/actions/runner-update"+query, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if htmx {
			req.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		srv.buildMux().ServeHTTP(w, req)
		return w
	}

	t.Run("form body sets the version", func(t *testing.T) {
		m := &SimpleMockDB{}
		w := post(m, "tok", "version=+v9.9.9+", "", true)
		require.Equal(t, http.StatusOK, w.Code)
		assert.True(t, m.desiredVersionSet)
		assert.Equal(t, "v9.9.9", m.desiredVersionSetTo)
		assert.False(t, m.runnerUpdateCleared)
	})
	t.Run("clear (hx-vals empty version) clears", func(t *testing.T) {
		m := &SimpleMockDB{}
		w := post(m, "tok", "version=", "", true)
		require.Equal(t, http.StatusOK, w.Code)
		assert.True(t, m.runnerUpdateCleared)
		assert.False(t, m.desiredVersionSet)
	})
	t.Run("query string still works", func(t *testing.T) {
		m := &SimpleMockDB{}
		post(m, "tok", "", "?version=v1.2.3", true)
		assert.Equal(t, "v1.2.3", m.desiredVersionSetTo)
	})
	t.Run("an empty body field is an explicit clear, query ignored", func(t *testing.T) {
		m := &SimpleMockDB{}
		post(m, "tok", "version=", "?version=v1.0.0", true)
		assert.True(t, m.runnerUpdateCleared)
		assert.False(t, m.desiredVersionSet, "a present-but-empty body field must not fall back to ?version=")
	})
	t.Run("body without a version field falls back to query", func(t *testing.T) {
		m := &SimpleMockDB{}
		post(m, "tok", "other=x", "?version=v1.0.0", true)
		assert.Equal(t, "v1.0.0", m.desiredVersionSetTo)
	})
	t.Run("form body wins over query", func(t *testing.T) {
		m := &SimpleMockDB{}
		post(m, "tok", "version=v2.0.0", "?version=v1.0.0", true)
		assert.Equal(t, "v2.0.0", m.desiredVersionSetTo)
	})
	t.Run("not htmx is forbidden", func(t *testing.T) {
		m := &SimpleMockDB{}
		w := post(m, "tok", "version=v9.9.9", "", false)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.False(t, m.desiredVersionSet)
	})
	t.Run("no control token fails closed", func(t *testing.T) {
		m := &SimpleMockDB{}
		w := post(m, "", "version=v9.9.9", "", true)
		assert.Contains(t, w.Body.String(), "control token not configured")
		assert.False(t, m.desiredVersionSet)
	})
}

// TestRunnerUpdateClearIsOutsideTheForm: the Clear control must not submit the
// typed version, so it lives outside the form and sends an explicit empty one.
func TestRunnerUpdateClearIsOutsideTheForm(t *testing.T) {
	srv := newDemoServer(":0", "active")
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers", nil))
	out := w.Body.String()
	formStart := strings.Index(out, `<form class="rb-form" hx-post="/actions/runner-update"`)
	require.Positive(t, formStart, "runner-update form not found")
	formLen := strings.Index(out[formStart:], "</form>")
	require.Positive(t, formLen, "runner-update form not closed")
	formEnd := formStart + formLen
	clear := strings.Index(out, `hx-vals='{"version": ""}'`)
	require.Positive(t, clear)
	assert.Greater(t, clear, formEnd, "Clear must be outside the form")
}

// TestRunnerUpdateDemoRoundTrip: through the demo, a set version shows up in
// the re-rendered fragment and a clear removes it.
func TestRunnerUpdateDemoRoundTrip(t *testing.T) {
	srv := newDemoServer(":0", "idle") // idle has no fixture request
	h := srv.buildMux()
	do := func(body string) string {
		req := httptest.NewRequest(http.MethodPost, "/actions/runner-update", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		return w.Body.String()
	}
	assert.Contains(t, do("version=v9.9.9"), "requested <code>v9.9.9</code>")
	assert.NotContains(t, do("version="), "requested <code>")
}

// TestRoleHealthIdleRendersHealthy: an idle role keeps its "idle" API token
// but gets the healthy treatment (green ✓), so a working-but-quiet role never
// reads as not-OK next to a HEALTHY one.
func TestRoleHealthIdleRendersHealthy(t *testing.T) {
	idle, healthy := health(roleIdle, "nothing to do"), health(roleHealthy, "")
	assert.Equal(t, roleIdle, idle.Token, "the API token is unchanged")
	assert.Equal(t, "HEALTHY · idle", idle.Label)
	assert.Equal(t, healthy.Glyph, idle.Glyph)
	assert.Equal(t, healthy.Class, idle.Class)
	assert.Equal(t, healthy.Dot, idle.Dot)
}

// TestServersFragmentRoleCards renders the fragment from crafted role cards:
// not-configured roles collapse into the one-line strip, the card's
// "key allowlist" row is gone, an unpinned role has no "pinned" row, and the
// allowlist warning still renders when the model is denied but the health
// line says something else.
func TestServersFragmentRoleCards(t *testing.T) {
	denied := roleCard{Key: "judge", Title: "Judge", Step: recipe.StepPropose, HasModel: true, Configured: true,
		Requested: "earmark-judge", AllowState: "denied", AllowlistWarn: true,
		Health: health(roleDown, "gateway unreachable")}
	allowed := roleCard{Key: "embeddings", Title: "Embeddings", Step: recipe.StepEmbed, HasModel: true, Configured: true,
		Requested: "nomic-embed-text", AllowState: "allowed", Health: health(roleIdle, "nothing waiting to embed")}
	decided := 7
	tests := []struct {
		name         string
		cards        []roleCard
		unconfigured []roleCard
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:  "denied model warns without a key allowlist row",
			cards: []roleCard{denied},
			wantContains: []string{"✗ earmark-judge is not on earmark's LiteLLM key allowlist — every call 403s",
				`id="role-judge"`},
			wantAbsent: []string{"key allowlist</dt>", "NOT ALLOWED", "<dt>pinned</dt>", "Not configured"},
		},
		{
			name:         "allowed model: no allowlist row, no warning",
			cards:        []roleCard{allowed},
			wantContains: []string{"✓ HEALTHY · idle"},
			wantAbsent:   []string{"key allowlist", "✓ allowed", "every call 403s"},
		},
		{
			name: "not-configured roles render in the strip, not as cards",
			unconfigured: []roleCard{
				{Key: "judge", Title: "Judge", HasModel: true, Health: health(roleNotConfigured, "judging is off")},
				{Key: "decide", Title: "Decide", HumanDecided: &decided, Health: health(roleNotConfigured, "no model role for this step yet")},
				{Key: "format", Title: "Format", Health: health(roleNotConfigured, "no model role for this step yet")},
			},
			wantContains: []string{`○ Not configured: Judge <span class="time-muted">(judging is off)</span>, Decide <span class="time-muted">(decided by humans 7)</span>, Format</p>`},
			wantAbsent:   []string{"role-card", "NOT CONFIGURED", "no model role for this step yet", `class="panels role-panels"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			require.NoError(t, serversFragmentTmpl.Execute(&buf, modelsData{RoleCards: tc.cards, UnconfiguredRoles: tc.unconfigured}))
			out := buf.String()
			for _, want := range tc.wantContains {
				assert.Contains(t, out, want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, out, absent)
			}
		})
	}
}

// TestServersFragmentSectionOrder: ASR runners is the last polled section, so
// the static update form in the shell follows it directly.
func TestServersFragmentSectionOrder(t *testing.T) {
	srv := newDemoServer(":0", "active")
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/data", nil))
	require.Equal(t, http.StatusOK, w.Code)
	out := w.Body.String()
	order := []string{`id="roles-title"`, `id="recipes-title"`, `id="gateway-title"`, `id="endpoints-title"`,
		`id="judge-output-title"`, `id="runners-title"`}
	last := -1
	for _, id := range order {
		i := strings.Index(out, id)
		require.Positive(t, i, id)
		assert.Greater(t, i, last, "%s is out of order", id)
		last = i
	}
}
