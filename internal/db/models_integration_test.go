package db

// Postgres integration tests for the Models page aggregates (CONTRACT §2.14):
// GetModelActivity, FindingsByModel, ASRProvenanceGroups, and the extended
// ListCurrentRecipes. Skipped unless EARMARK_TEST_DATABASE_URL is set.

import (
	"context"
	"testing"

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
	want := map[[2]string]int{
		{"anthropic/claude-haiku", "proposed"}: 2,
		{"anthropic/claude-haiku", "rejected"}: 1,
		{"qwen3.8", "proposed"}:                1, // resolved_model NULL → falls back to model
	}
	if len(got) != len(want) {
		t.Fatalf("FindingsByModel = %+v, want %d buckets (human row excluded)", got, len(want))
	}
	for _, g := range got {
		if want[[2]string{g.Model, g.PatchState}] != g.Count {
			t.Errorf("bucket %+v not expected", g)
		}
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
