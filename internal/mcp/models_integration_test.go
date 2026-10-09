package mcp

// Postgres proof of the Models page role board (CONTRACT §2.14) end to end:
// real aggregate queries → the cached snapshot → the role cards → the rendered
// /servers/data fragment. Covers the two live bugs of 2026-10-09: a judge
// answer recorded before the propose recipe changed must not read as a
// fallback, and decide-step decisions (decided_by jev:<recipe>) are not a
// person's. Skipped unless EARMARK_TEST_DATABASE_URL points at a server the
// test may create and drop databases on (see
// internal/db/migrate_integration_test.go).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

const (
	itJudgeNew = "anthropic/claude-haiku-5-5"
	itJudgeOld = "anthropic/claude-haiku-4-5-20251001"
	itJev      = "jev-1.13.0"
)

// modelsIntegrationDB creates a throwaway database, migrates it, and returns
// the earmark handle plus a raw connection for seeding.
func modelsIntegrationDB(t *testing.T) (*db.DB, *pgx.Conn) {
	t.Helper()
	admin := os.Getenv("EARMARK_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("EARMARK_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	var b [6]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	name := "earmark_it_" + hex.EncodeToString(b[:])
	ac, err := pgx.Connect(ctx, admin)
	require.NoError(t, err)
	_, err = ac.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	_ = ac.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	require.NoError(t, err)
	u.Path = "/" + name
	d, err := db.New(&config.Config{
		DatabaseURL: u.String(),
		ChunkSize:   512,
		AIEndpoints: []config.AIEndpoint{{ID: "e", Type: config.AIEndpointTypeEmbeddings, Model: "nomic-embed-text"}},
		AIRoles:     &config.AIRoles{Embeddings: "e"},
	})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	conn, err := pgx.Connect(ctx, u.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return d, conn
}

// modelsITSeedSQL: two done, judged tracks — both answered by the OLD judge
// model a day ago — and their findings (proposed, plus decided rows the
// tests stamp with deciders).
const modelsITSeedSQL = `
	INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
	  ('00000000-0000-0000-0000-0000000000a1', '/b/A/01.m4b', 'a1', 'done'),
	  ('00000000-0000-0000-0000-0000000000a2', '/b/A/02.m4b', 'a2', 'done');
	INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name) VALUES
	  ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000a1', '/b/A/01.m4b', 'a1', 'en', 60, '[]', 'x', 'parakeet'),
	  ('00000000-0000-0000-0000-0000000000b2', '00000000-0000-0000-0000-0000000000a2', '/b/A/02.m4b', 'a2', 'en', 60, '[]', 'y', 'parakeet');
	INSERT INTO run_metrics (job_id, eval_finished_at, eval_resolved_model) VALUES
	  ('00000000-0000-0000-0000-0000000000a1', now() - interval '26 hours', '` + itJudgeOld + `'),
	  ('00000000-0000-0000-0000-0000000000a2', now() - interval '24 hours', '` + itJudgeOld + `');
	INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec, original_text,
	                                 issue_type, confidence, model, resolved_model, patch_state, origin)
	SELECT '00000000-0000-0000-0000-0000000000b1', '/b/A/01.m4b', g, g + 1, 'w' || g, 'misheard', 0.9,
	       'earmark-judge', '` + itJudgeOld + `', 'proposed', 'judge'
	  FROM generate_series(0, 9) AS g;`

// seedModelsIT seeds the tracks and makes the propose recipe (the NEW model)
// current as of 20 hours ago — after the judge's newest answer.
func seedModelsIT(t *testing.T, d *db.DB, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	_, err := conn.Exec(ctx, modelsITSeedSQL)
	require.NoError(t, err)
	require.NoError(t, d.SetCurrentRecipes(ctx, recipe.Recipe{Step: recipe.StepPropose, StepVersion: 1,
		CodeVersion: "test", ModelAlias: itJudgeNew, ModelResolved: itJudgeNew, PromptVersion: "judge@v1", PromptSHA256: "c0ffee"}))
	_, err = conn.Exec(ctx, `UPDATE current_recipes SET updated_at = now() - interval '20 hours' WHERE step = 'propose'`)
	require.NoError(t, err)
}

// seedDecideIT makes a decide recipe current, logs one successful
// should_apply call under it, and has it decide three findings (accept 2,
// reject 1); a person accepts one more.
func seedDecideIT(t *testing.T, d *db.DB, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	r := recipe.Recipe{Step: recipe.StepDecide, StepVersion: 1, CodeVersion: "test", ModelAlias: itJev,
		ModelResolved: itJev, PromptVersion: "should_apply@v1", PromptSHA256: "5e1ec7",
		Params: map[string]any{"fn": "should_apply"}}
	require.NoError(t, d.SetCurrentRecipes(ctx, r))
	rid, err := r.ID()
	require.NoError(t, err)
	_, _, err = d.InsertFnCall(ctx, db.FnCall{Fn: "should_apply", PromptVersion: "should_apply@v1", PromptSHA256: "5e1ec7",
		ModelAlias: itJev, ModelResolved: "typesafe/" + itJev, RecipeID: rid, InputSHA256: strings.Repeat("ab", 32),
		Input: []byte(`{}`), Output: []byte(`{"should_apply":0.97}`)})
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `
		UPDATE transcript_findings SET patch_state = s.state, decided_by = s.by, decided_at = now()
		  FROM (VALUES (0, 'accepted', 'jev:' || $1::text), (1, 'accepted', 'jev:' || $1::text),
		               (2, 'rejected', 'jev:' || $1::text), (3, 'accepted', 'mcp:alice')) AS s(n, state, by)
		 WHERE start_sec = s.n`, rid)
	require.NoError(t, err)
}

// modelsITConfig binds a pinned LiteLLM-free judge and, with decide, the
// System One endpoint to decide and scan. Probers are static (no network).
func modelsITConfig(decide bool) *config.Config {
	eps := []config.AIEndpoint{
		{ID: "embed", Type: config.AIEndpointTypeEmbeddings, Backend: config.AIBackendOllama,
			BaseURL: "http://embed.test:11434/v1", Model: "nomic-embed-text"},
		{ID: "judge", Type: config.AIEndpointTypeChat, Backend: config.AIBackendOpenAI,
			BaseURL: "http://judge.test:4000/v1", Model: itJudgeNew},
	}
	roles := &config.AIRoles{Embeddings: "embed", Eval: "judge"}
	if decide {
		eps = append(eps, config.AIEndpoint{ID: "litellm-jev", Type: config.AIEndpointTypeSystemOne,
			BaseURL: "http://jev.test:4000", Model: itJev})
		roles.Decide, roles.Scan = "litellm-jev", "litellm-jev"
	}
	return &config.Config{
		AIEndpoints: eps, AIRoles: roles,
		Models: &config.ModelRegistry{Steps: map[string]config.ModelPin{
			recipe.StepPropose: {ExpectedModel: itJudgeNew},
		}},
	}
}

// renderModelsIT renders /servers/data through a fresh server (fresh caches).
func renderModelsIT(t *testing.T, d *db.DB, cfg *config.Config) string {
	t.Helper()
	srv := NewMCPServer(d, cfg)
	srv.endpointProber = demoEndpointProber{}
	srv.gatewayProber = demoGatewayProber{}
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/data", nil))
	require.Equal(t, http.StatusOK, w.Code)
	return w.Body.String()
}

// roleCardHTML cuts one role card out of the fragment.
func roleCardHTML(t *testing.T, out, key string) string {
	t.Helper()
	start := strings.Index(out, `id="role-`+key+`"`)
	require.Positive(t, start, "card %s not rendered", key)
	end := strings.Index(out[start:], "</dl>")
	require.Positive(t, end)
	return out[start : start+end]
}

func TestIntegrationModelsPageJudgeRecipeGate(t *testing.T) {
	d, conn := modelsIntegrationDB(t)
	seedModelsIT(t, d, conn)

	t.Run("old-recipe evidence only is not degraded", func(t *testing.T) {
		card := roleCardHTML(t, renderModelsIT(t, d, modelsITConfig(false)), "judge")
		assert.NotContains(t, card, "DEGRADED")
		assert.NotContains(t, card, "fallback route?")
		assert.NotContains(t, card, "≠ expected")
		assert.Contains(t, card, "✓ HEALTHY · idle")
		assert.Contains(t, card, "no calls since the model changed (now "+itJudgeNew+")")
		assert.Contains(t, card, `before: </span><span class="mono">`+itJudgeOld+`</span>`)
	})

	t.Run("a mismatch under the current recipe is degraded", func(t *testing.T) {
		// A third track judged an hour ago — after the recipe changed — and
		// answered by the old model: a real fallback.
		_, err := conn.Exec(context.Background(), `
			INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
			  ('00000000-0000-0000-0000-0000000000a3', '/b/A/03.m4b', 'a3', 'done');
			INSERT INTO run_metrics (job_id, eval_finished_at, eval_resolved_model) VALUES
			  ('00000000-0000-0000-0000-0000000000a3', now() - interval '1 hour', '`+itJudgeOld+`');`)
		require.NoError(t, err)
		card := roleCardHTML(t, renderModelsIT(t, d, modelsITConfig(false)), "judge")
		assert.Contains(t, card, "▲ DEGRADED")
		assert.Contains(t, card, "answered by "+itJudgeOld+", expected "+itJudgeNew+" — new findings are stamped stale (fallback route?)")
		assert.Contains(t, card, "≠ expected "+itJudgeNew)
		assert.NotContains(t, card, "no calls since")
	})
}

func TestIntegrationModelsPageDecide(t *testing.T) {
	d, conn := modelsIntegrationDB(t)
	seedModelsIT(t, d, conn)
	seedDecideIT(t, d, conn)

	t.Run("decide role card renders", func(t *testing.T) {
		out := renderModelsIT(t, d, modelsITConfig(true))
		card := roleCardHTML(t, out, "decide")
		assert.Contains(t, card, "✓ HEALTHY")
		assert.Contains(t, card, "6 proposed findings await a decision")
		assert.Contains(t, card, `<span class="mono">`+itJev+`</span> <span class="time-muted">@ litellm-jev</span>`)
		assert.Contains(t, card, `<span class="mono">typesafe/`+itJev+`</span> <span class="match-ok">✓ matches request</span>`)
		assert.Contains(t, card, `1 <span class="mono">should_apply</span> call`)
		assert.Contains(t, card, "<dt>decided</dt><dd>3 by Jev · 1 by humans · 6 proposed awaiting</dd>")
		assert.Contains(t, roleCardHTML(t, out, "scan"), "no current scan recipe yet")
		assert.NotContains(t, out, "Not configured: Decide")
		// The judge-output table splits Decided by decider.
		assert.Contains(t, out, `<td class="num">6</td>
      <td class="num">0</td>
      <td class="num">1</td>
      <td class="num">3</td>`)
	})

	t.Run("jev decisions are not counted as human", func(t *testing.T) {
		// Decide unbound: the strip still reports the counts, split by decider.
		out := renderModelsIT(t, d, modelsITConfig(false))
		assert.Contains(t, out, "decided by humans 1 · by Jev 3")
		assert.NotContains(t, out, "decided by humans 4")
	})
}
