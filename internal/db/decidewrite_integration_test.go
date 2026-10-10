package db

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/patch"
)

// anchorAll gives the seeded findings an anchor and the judged chunk hash
// (no state change, so no event), putting them in the decide scope.
func anchorAll(t *testing.T, d *DB, ids []string) {
	t.Helper()
	for i, id := range ids {
		sha := patch.ChunkHash([]string{"ganema said", "and left"}[i%2])
		if _, err := d.pool.Exec(context.Background(),
			`UPDATE transcript_findings SET anchor_offset = 0, anchor_occurrence = 0, chunk_text_sha256 = $2 WHERE id = $1`,
			id, sha); err != nil {
			t.Fatal(err)
		}
	}
}

func workIDs(t *testing.T, d *DB, s DecideWorkScope) []string {
	t.Helper()
	if s.Limit == 0 {
		s.Limit = MaxDecideWorkPage
	}
	if s.RetryReason == "" {
		s.RetryReason = "jev_unavailable"
	}
	fs, err := d.DecideWork(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	// The progress total counts exactly the list the pages return (without
	// a cursor: DecideWorkCount ignores After).
	if s.After == "" {
		if n, err := d.DecideWorkCount(context.Background(), s); err != nil || n != len(fs) {
			t.Errorf("DecideWorkCount = %d, %v; DecideWork returned %d", n, err, len(fs))
		}
	}
	var out []string
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

// TestIntegrationDecideWorkScope: who is in a --yes run's work list, under
// the recipe that ran and under a new one (CONTRACT §2.19).
func TestIntegrationDecideWorkScope(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 6)
	anchorAll(t, d, ids)
	r1, r2 := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	registerTestRecipe(t, conn, r1, 1)
	registerTestRecipe(t, conn, r2, 2)
	sha := []string{patch.ChunkHash("ganema said"), patch.ChunkHash("and left")}
	ev := func(i int, outcome, reason string) DecisionEvent {
		return DecisionEvent{FindingID: ids[i], RecipeID: r1, Outcome: outcome, Reason: reason, ChunkTextSHA256: sha[i%2]}
	}
	if err := d.SetPatchState(ctx, ids[5], patch.StateProposed, patch.StateAccepted, "mcp:justin"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplyDecisions(ctx, r1, []DecisionEvent{
		ev(0, OutcomeApply, "confident_with_evidence"),
		ev(1, OutcomeReject, "jev_reject"),
		ev(2, OutcomeHold, "uncertain"),
		ev(3, OutcomeHold, "jev_unavailable"),
	}); err != nil {
		t.Fatal(err)
	}
	// Under r1 again: ids[3] (retryable hold) and ids[4] (never decided).
	// Not ids[0] (r1 accepted it: no self re-check), ids[1] (rejected), ids[2]
	// (a final hold), ids[5] (a person accepted it).
	if got, want := workIDs(t, d, DecideWorkScope{RecipeID: r1}), sorted(ids[3], ids[4]); !slices.Equal(got, want) {
		t.Errorf("r1 work %v, want %v", got, want)
	}
	// Under r2: the re-check of r1's accept, r1's holds, and the undecided one.
	if got, want := workIDs(t, d, DecideWorkScope{RecipeID: r2}), sorted(ids[0], ids[2], ids[3], ids[4]); !slices.Equal(got, want) {
		t.Errorf("r2 work %v, want %v", got, want)
	}
	// A revoked decision no longer counts: undo r1 and r1 decides again.
	if _, err := d.RevertDecisions(ctx, RevertScope{RecipeID: r1}, true); err != nil {
		t.Fatal(err)
	}
	if got, want := workIDs(t, d, DecideWorkScope{RecipeID: r1}), sorted(ids[1], ids[2], ids[3], ids[4]); !slices.Equal(got, want) {
		// ids[0] is now rejected (accepted → rejected by the revert), out of scope.
		t.Errorf("r1 work after revert %v, want %v", got, want)
	}
	// Keyset: After skips up to and including the cursor.
	all := workIDs(t, d, DecideWorkScope{RecipeID: r2})
	if got := workIDs(t, d, DecideWorkScope{RecipeID: r2, After: all[0]}); !slices.Equal(got, all[1:]) {
		t.Errorf("after %s: %v, want %v", all[0], got, all[1:])
	}
}

func sorted(ids ...string) []string {
	s := slices.Clone(ids)
	slices.Sort(s)
	return s
}

// TestIntegrationDecideWorkShards: shards partition the work by transcript —
// disjoint, and together the whole list.
func TestIntegrationDecideWorkShards(t *testing.T) {
	ctx := context.Background()
	_, d := migratedTestDB(t)
	recipe := strings.Repeat("c3", 32)
	var all []string
	for i := range 12 {
		tid := fmt.Sprintf("00000000-0000-0000-0000-0000000005%02d", i)
		jid := fmt.Sprintf("00000000-0000-0000-0000-0000000006%02d", i)
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES ($1, $2, $2, 'done')`, jid, fmt.Sprintf("/b/S%d/1.m4b", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name)
			VALUES ($1, $2, $3, $3, 'en', 1, '[]', 'x', 'p')`, tid, jid, fmt.Sprintf("/b/S%d/1.m4b", i)); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			var id string
			if err := d.pool.QueryRow(ctx, `
				INSERT INTO transcript_findings (transcript_id, file_path, chunk_index, start_sec, end_sec, original_text,
				       issue_type, suggested_correction, confidence, model, origin, patch_state, anchor_offset, chunk_text_sha256)
				VALUES ($1, 'x', 0, 0, 1, 'a', 'misheard_word', 'b', 0.5, 'm', 'judge', 'proposed', 0, 'h') RETURNING id::text`, tid).Scan(&id); err != nil {
				t.Fatal(err)
			}
			all = append(all, id)
		}
	}
	slices.Sort(all)
	if got := workIDs(t, d, DecideWorkScope{RecipeID: recipe}); !slices.Equal(got, all) {
		t.Fatalf("unsharded %d, want %d", len(got), len(all))
	}
	var union []string
	nonEmpty := 0
	for s := range 3 {
		part := workIDs(t, d, DecideWorkScope{RecipeID: recipe, Shard: s, Shards: 3})
		if len(part) > 0 {
			nonEmpty++
		}
		union = append(union, part...)
	}
	slices.Sort(union)
	if !slices.Equal(union, all) {
		t.Errorf("shards are not a partition: %d ids over 3 shards, want %d each once", len(union), len(all))
	}
	if nonEmpty < 2 {
		t.Errorf("12 transcripts landed in %d of 3 shards", nonEmpty)
	}
}

// TestIntegrationRecheckDecisions: a new recipe's verdicts on another decide
// recipe's accepts — reject undoes, apply and hold keep, a person's accept is
// never touched.
func TestIntegrationRecheckDecisions(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 4)
	anchorAll(t, d, ids)
	oldR, newR := strings.Repeat("d4", 32), strings.Repeat("e5", 32)
	registerTestRecipe(t, conn, oldR, 1)
	registerTestRecipe(t, conn, newR, 2)
	sha := []string{patch.ChunkHash("ganema said"), patch.ChunkHash("and left")}
	if _, err := d.ApplyDecisions(ctx, oldR, []DecisionEvent{
		{FindingID: ids[0], RecipeID: oldR, Outcome: OutcomeApply, Reason: "x", ChunkTextSHA256: sha[0]},
		{FindingID: ids[1], RecipeID: oldR, Outcome: OutcomeApply, Reason: "x", ChunkTextSHA256: sha[1]},
	}); err != nil {
		t.Fatal(err)
	}
	// ids[0] has since been replayed (applied); ids[1] is still accepted.
	if _, err := d.pool.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'applied' WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPatchState(ctx, ids[2], patch.StateProposed, patch.StateAccepted, "mcp:justin"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE transcript_chunks SET embedding_stale = false`); err != nil {
		t.Fatal(err)
	}
	res, err := d.ApplyRecheckDecisions(ctx, newR, []DecisionEvent{
		{FindingID: ids[0], RecipeID: newR, Outcome: OutcomeReject, Reason: "jev_reject", ChunkTextSHA256: sha[0]},
		{FindingID: ids[1], RecipeID: newR, Outcome: OutcomeHold, Reason: "uncertain", ChunkTextSHA256: sha[1]},
		{FindingID: ids[2], RecipeID: newR, Outcome: OutcomeReject, Reason: "jev_reject", ChunkTextSHA256: sha[0]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Reverted, []string{ids[0]}) || !slices.Equal(res.Kept, []string{ids[1]}) || !slices.Equal(res.Skipped, []string{ids[2]}) {
		t.Fatalf("recheck = %+v", res)
	}
	for id, want := range map[string]string{ids[0]: "reverted", ids[1]: "accepted", ids[2]: "accepted"} {
		if got := findingState(t, d, id); got != want {
			t.Errorf("%s is %s, want %s", id, got, want)
		}
	}
	tr := transitions(t, conn, ids[0])
	if last := tr[len(tr)-1]; last.from != "applied" || last.to != "reverted" || last.actor != "jev:"+newR || last.recipe != newR {
		t.Errorf("revert transition %+v", last)
	}
	if !chunkStale(t, conn, feChunk0) {
		t.Error("undoing an applied fix must flag its chunk")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE kind = 'decision' AND recipe_id = $1`, newR); n != 2 {
		t.Errorf("%d new-recipe decisions, want 2 (the skipped human accept gets none)", n)
	}
}

// TestIntegrationRevertDecisions: revert by recipe, by finding and by time —
// the right moves, every one attributed, decisions revoked, chunks re-flagged,
// a person's decisions untouched, and a second run a no-op.
func TestIntegrationRevertDecisions(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 6)
	anchorAll(t, d, ids)
	r := strings.Repeat("f6", 32)
	registerTestRecipe(t, conn, r, 1)
	sha := []string{patch.ChunkHash("ganema said"), patch.ChunkHash("and left")}
	ev := func(i int, outcome string) DecisionEvent {
		return DecisionEvent{FindingID: ids[i], RecipeID: r, Outcome: outcome, Reason: "x", ChunkTextSHA256: sha[i%2]}
	}
	if err := d.SetPatchState(ctx, ids[5], patch.StateProposed, patch.StateAccepted, "mcp:justin"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplyDecisions(ctx, r, []DecisionEvent{ev(0, OutcomeApply), ev(1, OutcomeApply), ev(2, OutcomeReject), ev(3, OutcomeHold), ev(4, OutcomeApply)}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'applied' WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE transcript_chunks SET embedding_stale = false`); err != nil {
		t.Fatal(err)
	}

	// One finding first.
	rep, err := d.RevertDecisions(ctx, RevertScope{FindingID: ids[4]}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Moved != 1 || rep.Transitions["accepted->rejected"] != 1 || rep.Revoked != 1 {
		t.Errorf("finding revert %+v", rep)
	}

	dry, err := d.RevertDecisions(ctx, RevertScope{RecipeID: r}, false)
	if err != nil {
		t.Fatal(err)
	}
	// ids[4] was reverted to rejected by revert:jev — no longer a jev decider.
	if !dry.DryRun || dry.Moved != 3 || dry.Revoked != 4 {
		t.Errorf("dry run %+v", dry)
	}
	if findingState(t, d, ids[0]) != "applied" {
		t.Fatal("dry run changed state")
	}
	rep, err = d.RevertDecisions(ctx, RevertScope{RecipeID: r, Since: time.Now().Add(-time.Hour)}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"applied->reverted": 1, "accepted->rejected": 1, "rejected->proposed": 1}
	for k, v := range want {
		if rep.Transitions[k] != v {
			t.Errorf("transitions %v, want %v", rep.Transitions, want)
			break
		}
	}
	if rep.Moved != 3 || rep.Revoked != 4 || rep.Reflagged < 2 || rep.Skipped != 0 {
		t.Errorf("revert %+v", rep)
	}
	for id, w := range map[string]string{ids[0]: "reverted", ids[1]: "rejected", ids[2]: "proposed", ids[3]: "proposed", ids[5]: "accepted"} {
		if got := findingState(t, d, id); got != w {
			t.Errorf("%s is %s, want %s", id, got, w)
		}
	}
	for _, id := range ids[:3] {
		tr := transitions(t, conn, id)
		if last := tr[len(tr)-1]; last.actor != "revert:jev:"+r || last.recipe != r {
			t.Errorf("%s last transition %+v, want attributed to the undo", id, last)
		}
	}
	if !chunkStale(t, conn, feChunk0) || !chunkStale(t, conn, feChunk1) {
		t.Error("every reverted finding's chunk must be re-flagged")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events e WHERE e.kind = 'decision' AND e.recipe_id = $1
		AND NOT EXISTS (SELECT 1 FROM finding_events v WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)`, r); n != 0 {
		t.Errorf("%d live decisions left", n)
	}

	again, err := d.RevertDecisions(ctx, RevertScope{RecipeID: r}, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Moved != 0 || again.Revoked != 0 || again.Reflagged != 0 {
		t.Errorf("second revert %+v, want a no-op", again)
	}
}
