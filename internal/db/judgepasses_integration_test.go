package db

// Postgres proofs for judge passes (CONTRACT §2.15 "Lineage", "Re-judge"):
// the recipe-scoped dedupe of CommitJudgePass, the pass's closing record, and
// the --rejudge selection with its shard partition. Skipped unless
// EARMARK_TEST_DATABASE_URL is set (see migrate_integration_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/recipe"
)

func proposeRecipe(model string) recipe.Recipe {
	return recipe.Recipe{Step: recipe.StepPropose, StepVersion: 2, CodeVersion: "test",
		ModelAlias: model, ModelResolved: model, PromptVersion: "judge@v2", PromptSHA256: "p",
		Params: map[string]any{"min_confidence": 0.6}}
}

func jobOf(t *testing.T, d *DB, tID string) string {
	t.Helper()
	var job string
	if err := d.pool.QueryRow(context.Background(), `SELECT job_id::text FROM transcripts WHERE id = $1`, tID).Scan(&job); err != nil {
		t.Fatal(err)
	}
	return job
}

func passFinding(tID, path string, r *recipe.Recipe, chunkID, original, correction string) Finding {
	idx, sha, corr := 0, "chunksha", correction
	return Finding{TranscriptID: tID, FilePath: path, ChunkID: &chunkID, ChunkIndex: &idx,
		OriginalText: original, IssueType: "homophone", SuggestedCorrection: &corr, Confidence: 0.9,
		Model: r.ModelAlias, ChunkTextSHA256: &sha, Recipe: r}
}

// TestIntegrationCommitJudgePassRecipeScopedDedupe: the same correction from
// two recipes is stored twice (the agreement is recorded; the rows share a
// correction_key), while the same recipe proposing it again is skipped and
// counted as already_recorded. The pass is closed with its counters, the
// findings carry judge_pass_id and fn_call_id, and run_metrics is latched in
// the same transaction.
func TestIntegrationCommitJudgePassRecipeScopedDedupe(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()
	tID, path := seedTranscript(t, d)
	job := jobOf(t, d, tID)
	chunk := "00000000-0000-0000-0000-00000000c001"
	ra, rb := proposeRecipe("model-a"), proposeRecipe("model-b")

	commit := func(r recipe.Recipe, fs []Finding, m *EvalMetrics) (int64, JudgePassResult) {
		t.Helper()
		id, err := d.OpenJudgePass(ctx, JudgePassOpen{TranscriptID: tID, JobID: job, FilePath: path, Recipe: r, ChunksTotal: 1})
		if err != nil {
			t.Fatal(err)
		}
		res, err := d.CommitJudgePass(ctx, JudgePassCommit{PassID: id, TranscriptID: tID, Findings: fs, Metrics: m,
			Close: JudgePassClose{Status: JudgePassComplete, ChunksTotal: 1, ChunksJudged: 1,
				Dropped: map[string]int{"over_cap": 2}, BookContextSHA: "abc"}})
		if err != nil {
			t.Fatal(err)
		}
		return id, res
	}

	rid, _ := ra.ID()
	if _, err := d.RegisterRecipe(ctx, ra); err != nil {
		t.Fatal(err)
	}
	fnID, _, err := d.InsertFnCall(ctx, FnCall{Fn: "judge_chunk", PromptVersion: "judge@v2", PromptSHA256: "p",
		ModelAlias: "model-a", ModelResolved: "model-a", RecipeID: rid, InputSHA256: fmt.Sprintf("%064x", 1),
		Input: json.RawMessage(`{}`), Output: json.RawMessage(`{"content":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	fa := passFinding(tID, path, &ra, chunk, "pin name", "pen name")
	fa.FnCallID = &fnID
	now := mustNow(t, d)
	passA, res := commit(ra, []Finding{fa}, &EvalMetrics{JobID: job, StartedAt: now, FinishedAt: now, Model: "model-a", Chunks: 1})
	if res.Inserted != 1 || res.Duplicates != 0 {
		t.Fatalf("first pass %+v", res)
	}
	_, res = commit(rb, []Finding{passFinding(tID, path, &rb, chunk, "pin name", "pen name")}, nil)
	if res.Inserted != 1 {
		t.Fatalf("another recipe's identical correction must be stored (agreement): %+v", res)
	}
	passA2, res := commit(ra, []Finding{
		passFinding(tID, path, &ra, chunk, "pin name", "pen name"),
		passFinding(tID, path, &ra, chunk, "auto sebo", "Arecibo"),
	}, nil)
	if res.Inserted != 1 || res.Duplicates != 1 {
		t.Fatalf("same recipe again: %+v, want 1 inserted, 1 duplicate", res)
	}

	var keys, rows int
	if err := d.pool.QueryRow(ctx, `SELECT count(DISTINCT correction_key), count(*) FROM transcript_findings
		WHERE transcript_id = $1 AND original_text = 'pin name'`, tID).Scan(&keys, &rows); err != nil {
		t.Fatal(err)
	}
	if keys != 1 || rows != 2 {
		t.Errorf("pin name rows %d (keys %d): want both recipes' rows under one correction_key", rows, keys)
	}
	var gotPass, gotFn *int64
	if err := d.pool.QueryRow(ctx, `SELECT judge_pass_id, fn_call_id FROM transcript_findings
		WHERE transcript_id = $1 AND recipe_id = $2 AND original_text = 'pin name'`, tID, rid).Scan(&gotPass, &gotFn); err != nil {
		t.Fatal(err)
	}
	if gotPass == nil || *gotPass != passA || gotFn == nil || *gotFn != fnID {
		t.Errorf("finding lineage pass=%v fn=%v, want %d / %d", gotPass, gotFn, passA, fnID)
	}

	var status, sha string
	var kept, judged int
	var dropped map[string]int
	if err := d.pool.QueryRow(ctx, `SELECT status, findings_kept, chunks_judged, dropped, book_context_sha256
		FROM judge_passes WHERE id = $1 AND finished_at IS NOT NULL`, passA2).Scan(&status, &kept, &judged, &dropped, &sha); err != nil {
		t.Fatal(err)
	}
	if status != JudgePassComplete || kept != 1 || judged != 1 || sha != "abc" ||
		dropped[DropAlreadyRecorded] != 1 || dropped["over_cap"] != 2 {
		t.Errorf("closed pass: %s kept %d judged %d dropped %v sha %q", status, kept, judged, dropped, sha)
	}
	var latched bool
	if err := d.pool.QueryRow(ctx, `SELECT eval_finished_at IS NOT NULL AND eval_findings = 1 FROM run_metrics WHERE job_id = $1`,
		job).Scan(&latched); err != nil || !latched {
		t.Errorf("run_metrics latch not written in the commit (%v)", err)
	}

	// A closed pass cannot be closed again.
	if _, err := d.CommitJudgePass(ctx, JudgePassCommit{PassID: passA, TranscriptID: tID,
		Close: JudgePassClose{Status: JudgePassFailed}}); !errors.Is(err, ErrJudgePassNotRunning) {
		t.Errorf("second close: %v", err)
	}

	if ok, err := d.RecipesEquivalent(ctx, rid, rid); err != nil || !ok {
		t.Errorf("a recipe is equivalent to itself (%v)", err)
	}
	ridB, _ := rb.ID()
	if ok, err := d.RecipesEquivalent(ctx, rid, ridB); err != nil || ok {
		t.Errorf("model-a and model-b recipes equivalent (%v)", err)
	}
}

func mustNow(t *testing.T, d *DB) (now time.Time) {
	t.Helper()
	if err := d.pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// TestIntegrationRejudgeTargets: the selection skips transcripts with a
// complete pass under the recipe, without chunks, or outside --book; a
// partial pass stays selected. The --shard partition is disjoint and
// covering, page by page.
func TestIntegrationRejudgeTargets(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()
	r := proposeRecipe("model-a")
	rid, err := d.RegisterRecipe(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for range 40 {
		tID, path := seedTranscript(t, d)
		if _, err := d.pool.Exec(ctx, `INSERT INTO transcript_chunks (transcript_id, file_path, chunk_index,
			start_sec, end_sec, text, embedding) VALUES ($1, $2, 0, 0, 1, 'x', array_fill(0.1, ARRAY[768])::vector)`,
			tID, path); err != nil {
			t.Fatal(err)
		}
		all = append(all, tID)
	}
	noChunks, _ := seedTranscript(t, d)
	done, partial := all[0], all[1]
	for tid, status := range map[string]string{done: JudgePassComplete, partial: JudgePassPartial} {
		id, err := d.OpenJudgePass(ctx, JudgePassOpen{TranscriptID: tid, JobID: jobOf(t, d, tid), FilePath: "p", Recipe: r})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.CommitJudgePass(ctx, JudgePassCommit{PassID: id, TranscriptID: tid,
			Close: JudgePassClose{Status: status}}); err != nil {
			t.Fatal(err)
		}
	}

	walk := func(q RejudgeQuery) []string {
		t.Helper()
		var got []string
		q.Limit = 7
		for {
			page, err := d.RejudgeTargets(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range page {
				got = append(got, p.TranscriptID)
				q.After = p.Cursor()
			}
			if len(page) < q.Limit {
				return got
			}
		}
	}
	want := slices.Clone(all[1:]) // everything but the complete one
	slices.Sort(want)
	got := walk(RejudgeQuery{RecipeID: rid})
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("unsharded selection: %d rows, want %d (complete excluded, partial kept, no-chunk %s excluded)",
			len(got), len(want), noChunks)
	}
	if slices.Contains(got, noChunks) || slices.Contains(got, done) || !slices.Contains(got, partial) {
		t.Errorf("selection wrong about no-chunk/complete/partial transcripts")
	}

	seen := map[string]int{}
	for i := range 4 {
		for _, id := range walk(RejudgeQuery{RecipeID: rid, Shard: i, Shards: 4}) {
			seen[id]++
		}
	}
	for _, id := range want {
		if seen[id] != 1 {
			t.Errorf("transcript %s in %d shards, want exactly 1", id, seen[id])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("shards cover %d transcripts, want %d", len(seen), len(want))
	}

	if got := walk(RejudgeQuery{RecipeID: rid, Book: "no-such-book"}); len(got) != 0 {
		t.Errorf("--book filter let %d through", len(got))
	}
	if _, err := d.RejudgeTargets(ctx, RejudgeQuery{RecipeID: rid, Shard: 4, Shards: 4}); err == nil {
		t.Error("shard 4/4 accepted")
	}
	// Another recipe has judged nothing completely.
	other, _ := proposeRecipe("model-z").ID()
	if got := walk(RejudgeQuery{RecipeID: other}); len(got) != len(all) {
		t.Errorf("another recipe selects %d, want %d", len(got), len(all))
	}
}
