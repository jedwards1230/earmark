package mcp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// Demo fixtures for the Models page role board (CONTRACT §2.14). Every role
// health state is reachable through DEMO_SCENARIO:
//
//	active / batch-analyze — Judge HEALTHY (Haiku, with a qwen3.8 fallback ×3
//	                         visible in "also"), Embeddings HEALTHY, Decide
//	                         HEALTHY (1,189 decided by Jev, 6 by humans), Scan
//	                         HEALTHY
//	failed                 — Judge FAILING (401 from LiteLLM, 12 failing),
//	                         Decide FAILING (timeout after the last ok call)
//	stale                  — Judge DEGRADED (qwen3.8 ≠ pin), Embeddings
//	                         DEGRADED (embed_model differs), ASR FAILING
//	                         (stalled), Decide DEGRADED (jev-1.12.0 answered)
//	idle                   — Judge and Embeddings HEALTHY · idle, stale counts 0
//	winddown               — the LiteLLM gateway is offline → Judge, Embeddings,
//	                         Decide and Scan DOWN
//	recipe-changed         — the judge model changed after its newest answer:
//	                         "no calls since the model changed", not DEGRADED
//	empty                  — ASR/Judge/Decide/Scan/Format on the "Not configured"
//	                         line, no recipes
//	multibackend           — two runner builds and two .nemo shas plus legacy
//	snapshot-error         — the aggregate snapshot fails: "counts unavailable",
//	                         the fragment still renders 200
//	gateway-allowlist      — the judge's model is missing from earmark's LiteLLM
//	                         key allowlist → Judge DEGRADED ("every call 403s")
//	gateway-keyinfo        — /key/info is not readable by earmark's key (403):
//	                         the gateway card degrades, roles are unaffected
//
// The LiteLLM gateway is READY with key "earmark" ($12.25, no budget) in every
// scenario except winddown (gateway down); idle sets a budget and limits.

// demoScenarioSnapshotError is the scenario whose aggregate queries fail.
const demoScenarioSnapshotError = "snapshot-error"

// errDemoSnapshot is the injected FindingsByModel failure.
var errDemoSnapshot = errors.New("demo: injected aggregate query failure")

// Demo model ids (generic placeholders; the shape mirrors a LiteLLM deployment).
const (
	demoJudgeAlias  = "earmark-judge"
	demoJudgeModel  = "anthropic/claude-haiku-4-5-20251001"
	demoFallback    = "qwen3.8"
	demoEmbedModel  = "nomic-embed-text"
	demoASRModel    = "nvidia/parakeet-tdt-0.6b-v3"
	demoNemoSHA     = "9f3c2a71d4e8b6051c7f2e9a8d3b6c4f1e0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c"
	demoNemoSHAPrev = "4b1e9d0c7a2f8e3b6d5c4a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d"
	demoJevModel    = "jev-1.13.0"
	// demoPrevJudgeModel answered before the recipe-changed scenario's switch.
	demoPrevJudgeModel = "anthropic/claude-haiku-3-5-20241022"
)

// demoScenarioRecipeChanged: the judge's recipe moved after its newest answer.
const demoScenarioRecipeChanged = "recipe-changed"

// demoAIEndpoints is the synthetic AI endpoint registry: LiteLLM-fronted
// embeddings + judge (declared gateway), and a direct, unbound, offline Ollama
// chat endpoint — so the endpoints table shows bound/unbound, LiteLLM/direct and
// READY/OFFLINE. baseURLs are placeholders; demoEndpointProber routes by a
// sentinel in the URL, so no network call is made.
var demoAIEndpoints = []config.AIEndpoint{
	{ID: "litellm-embed", Type: config.AIEndpointTypeEmbeddings, Backend: config.AIBackendOpenAI,
		BaseURL: "http://llm-gateway.demo:4000/v1", Model: demoEmbedModel, Gateway: "litellm"},
	{ID: "litellm-judge", Type: config.AIEndpointTypeChat, Backend: config.AIBackendOpenAI,
		BaseURL: "http://llm-gateway.demo:4000/v1", Model: demoJudgeAlias, Gateway: "litellm",
		Options: map[string]string{"temperature": "0", "max_tokens": "4096"}},
	{ID: "ollama-chat", Type: config.AIEndpointTypeChat, Backend: config.AIBackendOllama,
		BaseURL: "http://desktop-2.demo:11434/v1", Model: demoFallback},
	{ID: "litellm-jev", Type: config.AIEndpointTypeSystemOne,
		BaseURL: "http://llm-gateway.demo:4000", Model: demoJevModel, Gateway: "litellm"},
}

var demoAIRoles = &config.AIRoles{Embeddings: "litellm-embed", Eval: "litellm-judge", Decide: "litellm-jev", Scan: "litellm-jev"}

// demoLegacyEndpoints is the empty scenario's registry: what LoadConfig
// synthesizes from the deprecated EMBEDDINGS_* vars on a fresh install.
var demoLegacyEndpoints = []config.AIEndpoint{
	{ID: "_legacy", Type: config.AIEndpointTypeEmbeddings, Backend: config.AIBackendOllama,
		BaseURL: "http://ollama.demo:11434/v1", Model: demoEmbedModel},
}

// demoModelRegistry is the MODELS_FILE pin set (CONTRACT §2.18).
var demoModelRegistry = &config.ModelRegistry{Steps: map[string]config.ModelPin{
	recipe.StepASR:     {ExpectedModel: demoASRModel, Revision: demoNemoSHA},
	recipe.StepPropose: {Alias: demoJudgeAlias, ExpectedModel: demoJudgeModel, Revision: "20251001", PromptVersion: "judge@v1"},
	recipe.StepEmbed:   {ExpectedModel: demoEmbedModel},
}}

// Demo scenarios specific to the LiteLLM gateway.
const (
	demoScenarioGatewayAllowlist = "gateway-allowlist"
	demoScenarioGatewayKeyInfo   = "gateway-keyinfo"
)

// demoEndpointProber is a static endpointProber: the direct desktop-2 Ollama is
// offline; in winddown the LiteLLM gateway is offline too (Judge + Embeddings
// DOWN); in gateway-allowlist the gateway's /v1/models omits the judge alias
// (LiteLLM lists only the key's allowed models). Everything else is ready.
type demoEndpointProber struct{ scenario string }

func (p demoEndpointProber) Probe(_ context.Context, baseURL, model, _ string) endpointProbe {
	if strings.Contains(baseURL, "desktop-2") ||
		(p.scenario == "winddown" && strings.Contains(baseURL, "llm-gateway")) {
		return endpointProbe{Probed: true, State: epStateOffline}
	}
	if p.scenario == demoScenarioGatewayAllowlist && model == demoJudgeAlias {
		return endpointProbe{Probed: true, State: epStateModelMissing}
	}
	return endpointProbe{Probed: true, State: epStateReady}
}

// demoGatewayProber is a static gatewayProber (no network): LiteLLM readiness
// plus earmark's own /key/info, varied by scenario.
type demoGatewayProber struct{ scenario string }

func (p demoGatewayProber) Probe(_ context.Context, baseURL, _ string) gatewayStatus {
	base := gatewayBase(baseURL)
	st := gatewayStatus{Probed: true, Base: base, Host: hostOnly(base)}
	if p.scenario == "winddown" {
		st.KeyInfoErr = "key info unreachable"
		return st
	}
	st.Reachable, st.Health, st.DB, st.Version = true, "healthy", "connected", "1.102.1"
	if p.scenario == demoScenarioGatewayKeyInfo {
		st.KeyInfoErr = "key info not readable by earmark's key (HTTP 403)"
		return st
	}
	models := []string{demoEmbedModel, demoJudgeAlias, demoJevModel, "ollama/qwen3.8:latest"}
	if p.scenario == demoScenarioGatewayAllowlist {
		models = []string{demoEmbedModel, demoJevModel, "ollama/qwen3.8:latest"}
	}
	blocked := false
	k := gatewayKeyInfo{KeyAlias: "earmark", Models: models, Spend: 12.25, Status: "active", Blocked: &blocked}
	if p.scenario == "idle" {
		budget, rpm, tpm := 50.0, int64(60), int64(100_000)
		k.MaxBudget, k.BudgetDuration, k.RPMLimit, k.TPMLimit = &budget, "30d", &rpm, &tpm
		k.BudgetResetAt = time.Now().Add(12 * 24 * time.Hour).UTC().Format(time.RFC3339)
		k.Expires = time.Now().Add(200 * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	st.KeyInfoOK, st.Key = true, k
	return st
}

// demoRecipeID is a deterministic 64-hex recipe id for a step.
func demoRecipeID(step string) string {
	switch step {
	case recipe.StepPropose:
		return "5127c0de9a4f3b2e1d0c9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0dcff1"
	case recipe.StepDecide:
		return "2c78840766491d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e"
	case recipe.StepScan:
		return "7a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b5ca1"
	default:
		return "e3b1d0c9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a30e3b"
	}
}

func (d demoDB) ListCurrentRecipes(context.Context) ([]db.CurrentRecipe, error) {
	if d.scenario == "empty" {
		return nil, nil
	}
	now := time.Now()
	proposeSince := now.Add(-3 * 24 * time.Hour)
	if d.scenario == demoScenarioRecipeChanged {
		proposeSince = now.Add(-20 * time.Hour) // after the judge's newest answer (1d ago)
	}
	return []db.CurrentRecipe{
		{Step: recipe.StepEmbed, RecipeID: demoRecipeID(recipe.StepEmbed), Model: demoEmbedModel,
			StepVersion: 1, ModelAlias: demoEmbedModel, ModelResolved: demoEmbedModel, UpdatedAt: now.Add(-9 * 24 * time.Hour)},
		{Step: recipe.StepPropose, RecipeID: demoRecipeID(recipe.StepPropose), Model: demoJudgeModel,
			PromptVersion: "judge@v1", Revision: "20251001", StepVersion: 1, ModelAlias: demoJudgeAlias,
			ModelResolved: demoJudgeModel, PromptSHA: "c0ffee", UpdatedAt: proposeSince},
		{Step: recipe.StepDecide, RecipeID: demoRecipeID(recipe.StepDecide), Model: demoJevModel,
			PromptVersion: "should_apply@v2", StepVersion: 2, ModelAlias: demoJevModel,
			ModelResolved: demoJevModel, PromptSHA: "5e1ec7", UpdatedAt: now.Add(-2 * 24 * time.Hour)},
		{Step: recipe.StepScan, RecipeID: demoRecipeID(recipe.StepScan), Model: demoJevModel,
			PromptVersion: "scan_chunk@v1", StepVersion: 1, ModelAlias: demoJevModel,
			ModelResolved: demoJevModel, PromptSHA: "5ca115", UpdatedAt: now.Add(-2 * 24 * time.Hour)},
	}, nil
}

func (d demoDB) StaleItemCounts(context.Context) (map[string]int64, error) {
	switch d.scenario {
	case "empty":
		return map[string]int64{}, nil
	case "idle":
		return map[string]int64{recipe.StepEmbed: 0, recipe.StepPropose: 0, recipe.StepDecide: 0, recipe.StepScan: 0}, nil
	case "stale":
		return map[string]int64{recipe.StepEmbed: 41_210, recipe.StepPropose: 33_870, recipe.StepDecide: 0, recipe.StepScan: 41_160}, nil
	default:
		return map[string]int64{recipe.StepEmbed: 39_644, recipe.StepPropose: 32_337, recipe.StepDecide: 0, recipe.StepScan: 39_594}, nil
	}
}

func (d demoDB) GetModelActivity(context.Context) (db.ModelActivity, error) {
	if d.scenario == "empty" {
		return db.ModelActivity{}, nil
	}
	now := time.Now()
	at := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }
	sp := func(s string) *string { return &s }
	a := db.ModelActivity{
		EvalLastOK:     at(14 * time.Minute),
		EvalLastFail:   at(3 * time.Hour),
		EvalLastError:  "chat completion: context deadline exceeded (chunk 12/40)",
		EvalFailingNow: 1,
		EvalLastModel:  demoJudgeModel,
		EvalModels7d:   []db.ModelCount{{Model: demoJudgeModel, Count: 41}, {Model: demoFallback, Count: 3}},
		EmbedLastModel: demoEmbedModel,
		ASRLatest: &db.ASRLatest{Model: demoASRModel, RunnerVersion: sp("v0.46.0"),
			ModelSHA256: sp(demoNemoSHA), At: now.Add(-2 * time.Minute)},
	}
	switch d.scenario {
	case demoScenarioRecipeChanged:
		// The newest answer is from before the model switch (20h ago).
		a.EvalLastOK, a.EvalLastFail, a.EvalLastError, a.EvalFailingNow = at(24*time.Hour), nil, "", 0
		a.EvalLastModel = demoPrevJudgeModel
		a.EvalModels7d = []db.ModelCount{{Model: demoPrevJudgeModel, Count: 52}}
	case "failed":
		a.EvalLastOK, a.EvalLastFail = at(2*time.Hour), at(6*time.Minute)
		a.EvalLastError = `401 Unauthorized: {"error":{"message":"Authentication Error, invalid LiteLLM virtual key","type":"auth_error"}}`
		a.EvalFailingNow = 12
	case "stale":
		a.EvalLastOK, a.EvalLastFail, a.EvalLastError, a.EvalFailingNow = at(40*time.Minute), nil, "", 0
		a.EvalLastModel = demoFallback
		a.EvalModels7d = []db.ModelCount{{Model: demoFallback, Count: 18}, {Model: demoJudgeModel, Count: 2}}
		a.EmbedLastModel = "mxbai-embed-large"
		a.ASRLatest.At = now.Add(-2 * time.Hour)
	case "idle":
		a.EvalLastOK, a.EvalLastFail, a.EvalLastError, a.EvalFailingNow = at(25*time.Minute), nil, "", 0
		a.EvalModels7d = []db.ModelCount{{Model: demoJudgeModel, Count: 45}}
	case "multibackend":
		a.ASRLatest = &db.ASRLatest{Model: "whisper-large-v3", RunnerVersion: sp("v0.46.0-whisper"), At: now.Add(-5 * time.Minute)}
	}
	a.EvalLastModelAt = a.EvalLastOK
	return a, nil
}

// FnRoleActivity is the decide/scan call evidence per scenario.
func (d demoDB) FnRoleActivity(context.Context) ([]db.FnRoleActivity, error) {
	if d.scenario == "empty" {
		return nil, nil
	}
	now := time.Now()
	at := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }
	scanned := int64(50)
	dec := db.FnRoleActivity{Step: recipe.StepDecide, Fn: "should_apply", RecipeID: demoRecipeID(recipe.StepDecide),
		ModelAlias: demoJevModel, Calls: 1_412, CacheHits: 230, LastOK: at(3 * time.Hour), LastModel: "typesafe/" + demoJevModel}
	scn := db.FnRoleActivity{Step: recipe.StepScan, Fn: "scan_chunk", RecipeID: demoRecipeID(recipe.StepScan),
		ModelAlias: demoJevModel, Calls: 50, LastOK: at(2 * 24 * time.Hour), LastModel: "typesafe/" + demoJevModel, Outputs: &scanned}
	switch d.scenario {
	case "failed":
		dec.LastOK, dec.LastFail, dec.LastErrorClass, dec.Failures24h = at(time.Hour), at(10*time.Minute), "timeout", 7
	case "stale":
		dec.LastModel, dec.Fallbacks = "typesafe/jev-1.12.0", 14
	}
	return []db.FnRoleActivity{dec, scn}, nil
}

func (d demoDB) FindingsByModel(context.Context) ([]db.FindingsModelCount, error) {
	switch d.scenario {
	case demoScenarioSnapshotError:
		return nil, errDemoSnapshot
	case "empty":
		return nil, nil
	case "idle":
		return []db.FindingsModelCount{
			{Model: demoJudgeModel, PatchState: "proposed", Count: 120},
			{Model: demoJudgeModel, PatchState: "accepted", Decider: db.DeciderHuman, Count: 4},
		}, nil
	}
	return []db.FindingsModelCount{
		{Model: demoJudgeModel, PatchState: "proposed", Count: 2_140},
		{Model: demoJudgeModel, PatchState: "unanchorable", Count: 12},
		{Model: demoJudgeModel, PatchState: "accepted", Decider: db.DeciderJev, Count: 820},
		{Model: demoJudgeModel, PatchState: "rejected", Decider: db.DeciderJev, Count: 369},
		{Model: demoJudgeModel, PatchState: "accepted", Decider: db.DeciderHuman, Count: 6},
		{Model: demoFallback, PatchState: "proposed", Count: 26_861},
		{Model: demoFallback, PatchState: "unanchorable", Count: 3_324},
		{Model: demoFallback, PatchState: "superseded", Count: 410},
	}, nil
}

func (d demoDB) ASRProvenanceGroups(_ context.Context, limit int) ([]db.ASRProvenanceGroup, error) {
	if d.scenario == "empty" {
		return nil, nil
	}
	now := time.Now()
	sp := func(s string) *string { return &s }
	days := func(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }
	var gs []db.ASRProvenanceGroup
	switch d.scenario {
	case "idle":
		gs = []db.ASRProvenanceGroup{
			{Model: demoASRModel, RunnerVersion: sp("v0.46.0"), SHA: sp(demoNemoSHA), Count: 362, First: days(20), Last: now.Add(-4 * time.Minute)},
		}
	case "multibackend":
		gs = []db.ASRProvenanceGroup{
			{Model: "whisper-large-v3", RunnerVersion: sp("v0.46.0-whisper"), Count: 12, First: days(1), Last: now.Add(-5 * time.Minute)},
			{Model: demoASRModel, RunnerVersion: sp("v0.46.0"), SHA: sp(demoNemoSHA), Count: 120, First: days(3), Last: now.Add(-20 * time.Minute)},
			{Model: demoASRModel, RunnerVersion: sp("v0.45.2"), SHA: sp(demoNemoSHAPrev), Count: 40, First: days(9), Last: days(3)},
			{Model: demoASRModel, Count: 2_900, First: days(120), Last: days(10)},
		}
	default:
		gs = []db.ASRProvenanceGroup{
			{Model: demoASRModel, RunnerVersion: sp("v0.46.0"), SHA: sp(demoNemoSHA), Count: 120, First: days(3), Last: now.Add(-2 * time.Minute)},
			{Model: demoASRModel, RunnerVersion: sp("v0.45.2"), SHA: sp(demoNemoSHA), Count: 40, First: days(9), Last: days(3)},
			{Model: demoASRModel, Count: 2_900, First: days(120), Last: days(10)},
		}
	}
	if limit > 0 && len(gs) > limit {
		gs = gs[:limit]
	}
	return gs, nil
}
