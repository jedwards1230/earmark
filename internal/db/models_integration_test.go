package db

// Postgres integration tests for the Models page aggregates (CONTRACT §2.14):
// GetModelActivity, FindingsByModel, ASRProvenanceGroups, and the extended
// ListCurrentRecipes. Skipped unless EARMARK_TEST_DATABASE_URL is set.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/recipe"
)

const modelsSeedSQL = `
	INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
	  ('00000000-0000-0000-0000-0000000000c1', '/b/A/01.m4b', 'c1', 'done'),
	  ('00000000-0000-0000-0000-0000000000c2', '/b/A/02.m4b', 'c2', 'done'),
	  ('00000000-0000-0000-0000-0000000000c3', '/b/B/01.m4b', 'c3', 'done');

	INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
	                         segments, raw_text, model_name, asr_model_sha256, asr_runner_version,
	                         created_at)
	VALUES
	  ('00000000-0000-0000-0000-0000000000d1', '00000000-0000-0000-0000-0000000000c1',
	   '/b/A/01.m4b', 'c1', 'en', 60, '[]', 'x', 'nvidia/parakeet-tdt-1.1b', 'sha-new', 'v0.46.0',
	   now() - interval '1 hour'),
	  ('00000000-0000-0000-0000-0000000000d2', '00000000-0000-0000-0000-0000000000c2',
	   '/b/A/02.m4b', 'c2', 'en', 60, '[]', 'y', 'nvidia/parakeet-tdt-1.1b', 'sha-new', 'v0.46.0',
	   now() - interval '10 minutes');
	-- A legacy transcript: the runner reported no provenance.
	INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
	                         segments, raw_text, model_name, created_at)
	VALUES ('00000000-0000-0000-0000-0000000000d3', '00000000-0000-0000-0000-0000000000c3',
	        '/b/B/01.m4b', 'c3', 'en', 60, '[]', 'z', 'nvidia/parakeet-tdt-0.6b-v3',
	        now() - interval '30 days');

	-- c1: judged by Haiku 2h ago; c2: judged by the qwen fallback 1h ago;
	-- c3: latest attempt failed 5 minutes ago.
	INSERT INTO run_metrics (job_id, eval_finished_at, eval_resolved_model, embed_model, embed_finished_at) VALUES
	  ('00000000-0000-0000-0000-0000000000c1', now() - interval '2 hours', 'anthropic/claude-haiku',
	   'nomic-old', now() - interval '3 hours'),
	  ('00000000-0000-0000-0000-0000000000c2', now() - interval '1 hour', 'qwen3.8',
	   'nomic-embed-text', now() - interval '1 hour');
	INSERT INTO run_metrics (job_id, eval_failed_at, eval_failed_chunks, eval_error) VALUES
	  ('00000000-0000-0000-0000-0000000000c3', now() - interval '5 minutes', 3, '401 Unauthorized');

	INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec, original_text,
	                                 issue_type, confidence, model, resolved_model, patch_state, origin)
	VALUES
	  ('00000000-0000-0000-0000-0000000000d1', '/b/A/01.m4b', 0, 1, 'a', 'misheard', 0.9,
	   'earmark-judge', 'anthropic/claude-haiku', 'proposed', 'judge'),
	  ('00000000-0000-0000-0000-0000000000d1', '/b/A/01.m4b', 1, 2, 'b', 'misheard', 0.9,
	   'earmark-judge', 'anthropic/claude-haiku', 'proposed', 'judge'),
	  ('00000000-0000-0000-0000-0000000000d1', '/b/A/01.m4b', 2, 3, 'c', 'misheard', 0.9,
	   'earmark-judge', 'anthropic/claude-haiku', 'rejected', 'judge'),
	  ('00000000-0000-0000-0000-0000000000d2', '/b/A/02.m4b', 0, 1, 'd', 'misheard', 0.9,
	   'qwen3.8', NULL, 'proposed', 'judge'),
	  -- A human correction: not a judge finding, so it is not counted.
	  ('00000000-0000-0000-0000-0000000000d2', '/b/A/02.m4b', 1, 2, 'e', 'misheard', 1,
	   'human', NULL, 'accepted', 'human');
`

func TestIntegrationModelActivity(t *testing.T) {
	ctx := context.Background()
	d := integrationDB(t, newTestDatabase(t))

	// Empty database: one row of NULLs, no error, no ASR latest.
	empty, err := d.GetModelActivity(ctx)
	if err != nil {
		t.Fatalf("GetModelActivity (empty): %v", err)
	}
	if empty.EvalLastOK != nil || empty.EvalLastFail != nil || empty.ASRLatest != nil ||
		empty.EvalFailingNow != 0 || len(empty.EvalModels7d) != 0 {
		t.Errorf("empty activity = %+v, want zero", empty)
	}

	if _, err := d.pool.Exec(ctx, modelsSeedSQL); err != nil {
		t.Fatal(err)
	}
	a, err := d.GetModelActivity(ctx)
	if err != nil {
		t.Fatalf("GetModelActivity: %v", err)
	}
	if a.EvalLastOK == nil || a.EvalLastFail == nil || !a.EvalLastFail.After(*a.EvalLastOK) {
		t.Errorf("last ok/fail = %v/%v, want fail after ok", a.EvalLastOK, a.EvalLastFail)
	}
	if a.EvalLastError != "401 Unauthorized" || a.EvalFailingNow != 1 {
		t.Errorf("error/failing = %q/%d", a.EvalLastError, a.EvalFailingNow)
	}
	if a.EvalLastModel != "qwen3.8" {
		t.Errorf("EvalLastModel = %q, want the newest resolved model qwen3.8", a.EvalLastModel)
	}
	// qwen3.8 (c2) is the newest finish, so its time is max(eval_finished_at).
	if a.EvalLastModelAt == nil || !a.EvalLastModelAt.Equal(*a.EvalLastOK) {
		t.Errorf("EvalLastModelAt = %v, want qwen3.8's finish %v", a.EvalLastModelAt, a.EvalLastOK)
	}
	if len(a.EvalModels7d) != 2 {
		t.Errorf("EvalModels7d = %+v, want two models", a.EvalModels7d)
	}
	if a.EmbedLastModel != "nomic-embed-text" {
		t.Errorf("EmbedLastModel = %q", a.EmbedLastModel)
	}
	if a.ASRLatest == nil || a.ASRLatest.Model != "nvidia/parakeet-tdt-1.1b" ||
		a.ASRLatest.RunnerVersion == nil || *a.ASRLatest.RunnerVersion != "v0.46.0" ||
		a.ASRLatest.ModelSHA256 == nil || *a.ASRLatest.ModelSHA256 != "sha-new" {
		t.Errorf("ASRLatest = %+v", a.ASRLatest)
	}
}

func TestIntegrationFindingsByModel(t *testing.T) {
	ctx := context.Background()
	d := integrationDB(t, newTestDatabase(t))
	if _, err := d.pool.Exec(ctx, modelsSeedSQL); err != nil {
		t.Fatal(err)
	}
	got, err := d.FindingsByModel(ctx)
	if err != nil {
		t.Fatalf("FindingsByModel: %v", err)
	}
	want := map[[3]string]int{
		{"anthropic/claude-haiku", "proposed", DeciderNone}: 2,
		{"anthropic/claude-haiku", "rejected", DeciderNone}: 1, // decided before attribution
		{"qwen3.8", "proposed", DeciderNone}:                1, // resolved_model NULL → falls back to model
	}
	if len(got) != len(want) {
		t.Fatalf("FindingsByModel = %+v, want %d buckets (human row excluded)", got, len(want))
	}
	for _, g := range got {
		if want[[3]string{g.Model, g.PatchState, g.Decider}] != g.Count {
			t.Errorf("bucket %+v not expected", g)
		}
	}
}

// TestIntegrationFindingsByModelDecider: decided_by is classified by the SQL —
// jev:<recipe> is the decide step and revert:jev:<recipe> its undo, never a
// person; mcp:, cli: and anything else are people.
func TestIntegrationFindingsByModelDecider(t *testing.T) {
	ctx := context.Background()
	d := integrationDB(t, newTestDatabase(t))
	if _, err := d.pool.Exec(ctx, modelsSeedSQL); err != nil {
		t.Fatal(err)
	}
	rid, err := d.RegisterRecipe(ctx, recipe.Recipe{Step: recipe.StepDecide, StepVersion: 1, CodeVersion: "test",
		ModelAlias: "jev-1.13.0"})
	if err != nil {
		t.Fatal(err)
	}
	jev := "jev:" + rid // the finding_events trigger resolves it to a registered recipe
	for i, by := range []string{jev, jev, jev, "revert:" + jev, "mcp:alice", "cli:bob", "agent"} {
		state := "accepted"
		if i == 3 {
			state = "rejected"
		}
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec, original_text,
			                                 issue_type, confidence, model, resolved_model, patch_state, origin,
			                                 decided_by, decided_at)
			VALUES ('00000000-0000-0000-0000-0000000000d2', '/b/A/02.m4b', $1::float8, $1::float8 + 1, 'x', 'misheard', 0.9,
			        'earmark-judge', 'anthropic/claude-haiku', $2, 'judge', $3, now())`, 10+i, state, by); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.FindingsByModel(ctx)
	if err != nil {
		t.Fatalf("FindingsByModel: %v", err)
	}
	by := map[[2]string]int{}
	for _, g := range got {
		if g.Model == "anthropic/claude-haiku" {
			by[[2]string{g.PatchState, g.Decider}] += g.Count
		}
	}
	want := map[[2]string]int{
		{"proposed", DeciderNone}:      2,
		{"rejected", DeciderNone}:      1,
		{"accepted", DeciderJev}:       3,
		{"rejected", DeciderJevRevert}: 1,
		{"accepted", DeciderHuman}:     3, // mcp:, cli: and a bare "agent"
	}
	if len(by) != len(want) {
		t.Fatalf("haiku buckets = %+v, want %+v", by, want)
	}
	for k, n := range want {
		if by[k] != n {
			t.Errorf("bucket %v = %d, want %d", k, by[k], n)
		}
	}
}

// TestIntegrationFnRoleActivity: the decide/scan evidence is the fn_calls rows
// of the current recipe's request (fn + model alias + prompt hash) — errors and
// fallbacks included, another prompt's calls excluded — plus the scan recipe's
// chunk_scan rows.
func TestIntegrationFnRoleActivity(t *testing.T) {
	ctx := context.Background()
	d := integrationDB(t, newTestDatabase(t))
	if _, err := d.pool.Exec(ctx, modelsSeedSQL); err != nil {
		t.Fatal(err)
	}
	if got, err := d.FnRoleActivity(ctx); err != nil || len(got) != 0 {
		t.Fatalf("no current recipes: %+v, %v; want none", got, err)
	}
	mk := func(step, fn, prompt string) recipe.Recipe {
		return recipe.Recipe{Step: step, StepVersion: 1, CodeVersion: "test", ModelAlias: "jev-1.13.0",
			ModelResolved: "jev-1.13.0", PromptVersion: fn + "@v1", PromptSHA256: prompt,
			Params: map[string]any{"fn": fn}}
	}
	dec, scn := mk(recipe.StepDecide, "should_apply", "5e1ec7"), mk(recipe.StepScan, "scan_chunk", "5ca115")
	if err := d.SetCurrentRecipes(ctx, dec, scn); err != nil {
		t.Fatal(err)
	}
	decID, _ := dec.ID()
	scnID, _ := scn.ID()

	call := func(fn, prompt, resolved, errClass string, ago time.Duration, hit *int64, rid string) int64 {
		t.Helper()
		c := FnCall{Fn: fn, PromptVersion: fn + "@v1", PromptSHA256: prompt, ModelAlias: "jev-1.13.0",
			ModelResolved: resolved, RecipeID: rid, InputSHA256: hexSHA(fmt.Sprintf("%s%s%s%d", fn, prompt, resolved, ago)),
			Input: json.RawMessage(`{}`), ErrorClass: errClass, CacheHit: hit != nil, CachedFrom: hit}
		if errClass == "" || errClass == ErrorClassModelFallback {
			c.Output = json.RawMessage(`{"p":0.5}`)
		}
		id, ok, err := d.InsertFnCall(ctx, c)
		if err != nil || !ok {
			t.Fatalf("insert %s: %v, %v", fn, ok, err)
		}
		if _, err := d.pool.Exec(ctx, `UPDATE fn_calls SET created_at = now() - $2 * interval '1 second' WHERE id = $1`,
			id, int(ago.Seconds())); err != nil {
			t.Fatal(err)
		}
		return id
	}
	okID := call("should_apply", "5e1ec7", "typesafe/jev-1.13.0", "", 3*time.Hour, nil, decID)
	call("should_apply", "5e1ec7", "typesafe/jev-1.13.0", "", 2*time.Hour, &okID, decID) // cache hit
	call("should_apply", "5e1ec7", "jev-1.12.0", ErrorClassModelFallback, 90*time.Minute, nil, "")
	call("should_apply", "5e1ec7", "", "timeout", time.Hour, nil, "")
	call("should_apply", "5e1ec7", "", "429", 48*time.Hour, nil, "")
	call("should_apply", "0dd", "typesafe/jev-1.13.0", "", time.Minute, nil, "") // another prompt: not this recipe
	call("scan_chunk", "5ca115", "typesafe/jev-1.13.0", "", 5*time.Hour, nil, scnID)
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO chunk_scan (transcript_id, chunk_index, chunk_text_sha256, recipe_id, p_needs_fix, quality,
		                        p_boilerplate, p_garbled, p_dialogue, issue_type, issue_probs)
		VALUES ('00000000-0000-0000-0000-0000000000d1', 0, $1, $2, 0.1, 4, 0, 0, 0, 'none', '{}'),
		       ('00000000-0000-0000-0000-0000000000d1', 1, $1, $2, 0.1, 4, 0, 0, 0, 'none', '{}')`,
		hexSHA("chunk"), scnID); err != nil {
		t.Fatal(err)
	}

	got, err := d.FnRoleActivity(ctx)
	if err != nil {
		t.Fatalf("FnRoleActivity: %v", err)
	}
	if len(got) != 2 || got[0].Step != recipe.StepDecide || got[1].Step != recipe.StepScan {
		t.Fatalf("FnRoleActivity = %+v, want decide then scan", got)
	}
	a := got[0]
	if a.Fn != "should_apply" || a.RecipeID != decID || a.ModelAlias != "jev-1.13.0" {
		t.Errorf("decide identity = %+v", a)
	}
	if a.Calls != 4 || a.CacheHits != 1 || a.Fallbacks != 1 || a.Failures24h != 1 {
		t.Errorf("decide counts calls/hits/fallbacks/fail24h = %d/%d/%d/%d, want 4/1/1/1",
			a.Calls, a.CacheHits, a.Fallbacks, a.Failures24h)
	}
	if a.LastOK == nil || a.LastFail == nil || !a.LastFail.After(*a.LastOK) || a.LastErrorClass != "timeout" {
		t.Errorf("decide last ok/fail/class = %v/%v/%q", a.LastOK, a.LastFail, a.LastErrorClass)
	}
	if a.LastModel != "jev-1.12.0" {
		t.Errorf("decide LastModel = %q, want the newest reply (the fallback)", a.LastModel)
	}
	if a.Outputs != nil {
		t.Errorf("decide Outputs = %v, want nil", *a.Outputs)
	}
	s := got[1]
	if s.Calls != 1 || s.LastFail != nil || s.LastModel != "typesafe/jev-1.13.0" || s.Outputs == nil || *s.Outputs != 2 {
		t.Errorf("scan = %+v", s)
	}
}

func TestIntegrationASRProvenanceGroups(t *testing.T) {
	ctx := context.Background()
	d := integrationDB(t, newTestDatabase(t))
	if _, err := d.pool.Exec(ctx, modelsSeedSQL); err != nil {
		t.Fatal(err)
	}
	got, err := d.ASRProvenanceGroups(ctx, 8)
	if err != nil {
		t.Fatalf("ASRProvenanceGroups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("groups = %+v, want 2", got)
	}
	if got[0].RunnerVersion == nil || *got[0].RunnerVersion != "v0.46.0" || got[0].Count != 2 ||
		!got[0].Last.After(got[0].First) {
		t.Errorf("newest group = %+v", got[0])
	}
	if got[1].RunnerVersion != nil || got[1].SHA != nil || got[1].Count != 1 {
		t.Errorf("legacy group = %+v, want NULL provenance", got[1])
	}

	limited, err := d.ASRProvenanceGroups(ctx, 1)
	if err != nil || len(limited) != 1 {
		t.Errorf("limit 1 → %d groups, err %v", len(limited), err)
	}
	if _, err := d.ASRProvenanceGroups(ctx, 0); err == nil {
		t.Error("limit 0 must be rejected")
	}
}

func TestIntegrationListCurrentRecipesDetail(t *testing.T) {
	ctx := context.Background()
	d := integrationDB(t, newTestDatabase(t))
	r := recipe.Recipe{
		Step: recipe.StepPropose, StepVersion: 1, CodeVersion: "test",
		ModelAlias: "earmark-judge", ModelResolved: "anthropic/claude-haiku",
		PromptVersion: "judge@v1", PromptSHA256: "abc",
	}
	if err := d.SetCurrentRecipes(ctx, r); err != nil {
		t.Fatalf("SetCurrentRecipes: %v", err)
	}
	got, err := d.ListCurrentRecipes(ctx)
	if err != nil {
		t.Fatalf("ListCurrentRecipes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("recipes = %+v", got)
	}
	c := got[0]
	if c.Step != recipe.StepPropose || c.StepVersion != 1 || c.ModelAlias != "earmark-judge" ||
		c.ModelResolved != "anthropic/claude-haiku" || c.Model != "anthropic/claude-haiku" ||
		c.PromptVersion != "judge@v1" || c.PromptSHA != "abc" || c.UpdatedAt.IsZero() {
		t.Errorf("current recipe = %+v", c)
	}
}

// TestIntegrationServerObservationRecency: a retired host whose run_metrics
// rows were touched recently (the eval backfill bumps updated_at) must still
// report its LAST TRANSCRIPTION as LastFinished, sort after a host that
// actually transcribed recently, and pick its model by transcription recency.
func TestIntegrationServerObservationRecency(t *testing.T) {
	ctx := context.Background()
	d := integrationDB(t, newTestDatabase(t))
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('00000000-0000-0000-0000-0000000000e1', '/b/Old/01.m4b', 'e1', 'done'),
		  ('00000000-0000-0000-0000-0000000000e2', '/b/Old/02.m4b', 'e2', 'done'),
		  ('00000000-0000-0000-0000-0000000000e3', '/b/New/01.m4b', 'e3', 'done');
		-- retired-host: transcribed 100 and 105 days ago; the NEWER transcription
		-- used model-new, but the OLDER row was just re-touched by eval (updated_at now).
		INSERT INTO run_metrics (job_id, runner_host, asr_model, transcribe_started_at,
		                         transcribe_finished_at, updated_at) VALUES
		  ('00000000-0000-0000-0000-0000000000e1', 'retired-host', 'model-old',
		   now() - interval '105 days 1 hour', now() - interval '105 days', now()),
		  ('00000000-0000-0000-0000-0000000000e2', 'retired-host', 'model-new',
		   now() - interval '100 days 1 hour', now() - interval '100 days', now() - interval '100 days'),
		  ('00000000-0000-0000-0000-0000000000e3', 'live-host', 'parakeet',
		   now() - interval '2 hours', now() - interval '1 hour', now() - interval '1 hour');
	`); err != nil {
		t.Fatal(err)
	}
	obs, err := d.GetServerObservation(ctx)
	if err != nil {
		t.Fatalf("GetServerObservation: %v", err)
	}
	if len(obs.Hosts) != 2 {
		t.Fatalf("hosts = %+v", obs.Hosts)
	}
	if obs.Hosts[0].Host != "live-host" {
		t.Errorf("hosts must be ordered by last transcription, got %s first", obs.Hosts[0].Host)
	}
	old := obs.Hosts[1]
	if old.LastFinished == nil || time.Since(*old.LastFinished) < 99*24*time.Hour {
		t.Errorf("retired-host LastFinished = %v, want ~100 days ago (not the fresh updated_at)", old.LastFinished)
	}
	if old.ASRModel == nil || *old.ASRModel != "model-new" {
		t.Errorf("retired-host model = %v, want model-new (latest TRANSCRIPTION, not latest updated_at)", old.ASRModel)
	}
	if old.JobsDone != 2 {
		t.Errorf("JobsDone = %d", old.JobsDone)
	}
}

// hexSHA is s's lowercase hex sha256 (a valid fn_calls/chunk_scan hash).
func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
