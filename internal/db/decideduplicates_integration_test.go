package db

// Postgres proofs for the decide step's duplicates (migration 00015, CONTRACT
// §2.19 "Agreeing and conflicting findings"): the revert follows a
// representative across batches and its dry run counts what --yes does, and
// the migration's Down refuses to drop recorded pointers. Skipped unless
// EARMARK_TEST_DATABASE_URL is set (see migrate_integration_test.go).

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

func dupEvent(id, rep, recipe, sha string) DecisionEvent {
	return DecisionEvent{FindingID: id, RecipeID: recipe, Outcome: OutcomeDuplicate, Reason: ReasonDuplicate,
		RelatedFindingID: rep, ChunkTextSHA256: sha}
}

func stateOf(t *testing.T, conn *pgx.Conn, id string) (state, by, dupOf string) {
	t.Helper()
	if err := conn.QueryRow(context.Background(), `SELECT patch_state, COALESCE(decided_by, ''), COALESCE(duplicate_of::text, '')
		FROM transcript_findings WHERE id = $1`, id).Scan(&state, &by, &dupOf); err != nil {
		t.Fatal(err)
	}
	return state, by, dupOf
}

// TestIntegrationRevertDuplicatesFollowAcrossBatches: with one finding per
// revert transaction, the representative's batch moves its duplicates; their
// own (later) batches neither move them again nor count them skipped. The dry
// run reports exactly what --yes then does, the revokes of a duplicate
// another recipe grouped included.
func TestIntegrationRevertDuplicatesFollowAcrossBatches(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 6)
	anchorAll(t, d, ids)
	r1, r2 := strings.Repeat("c3", 32), strings.Repeat("d4", 32)
	registerTestRecipe(t, conn, r1, 3)
	registerTestRecipe(t, conn, r2, 3)
	sha := []string{patch.ChunkHash("ganema said"), patch.ChunkHash("and left")}
	// Chunk 0: r1 accepts ids[0] and groups ids[2], ids[4] under it.
	// Chunk 1: r1 accepts ids[1]; r2 later groups ids[3] under it.
	if _, err := d.ApplyDecisions(ctx, r1, []DecisionEvent{
		{FindingID: ids[0], RecipeID: r1, Outcome: OutcomeApply, Reason: "confident_with_evidence", ChunkTextSHA256: sha[0]},
		dupEvent(ids[2], ids[0], r1, sha[0]),
		dupEvent(ids[4], ids[0], r1, sha[0]),
		{FindingID: ids[1], RecipeID: r1, Outcome: OutcomeApply, Reason: "confident_with_evidence", ChunkTextSHA256: sha[1]},
	}); err != nil {
		t.Fatal(err)
	}
	if res, err := d.ApplyDecisions(ctx, r2, []DecisionEvent{dupEvent(ids[3], ids[1], r2, sha[1])}); err != nil || len(res.Duplicates) != 1 {
		t.Fatalf("r2 grouping %+v, %v", res, err)
	}

	defer func(n int) { maxRevertBatch = n }(maxRevertBatch)
	maxRevertBatch = 1

	scope := RevertScope{RecipeID: r1}
	dry, err := d.RevertDecisions(ctx, scope, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.RevertDecisions(ctx, scope, true)
	if err != nil {
		t.Fatal(err)
	}
	// ids[0], ids[1] leave the overlay; ids[2], ids[4] (r1's) and ids[3]
	// (r2's) follow them to rejected. ids[3]'s duplicate decision is r2's:
	// outside the scope, revoked with the follower move.
	for name, r := range map[string]RevertReport{"dry run": dry, "apply": got} {
		if r.Moved != 2 || r.Cascaded != 3 || r.Transitions["accepted->rejected"] != 2 || r.Transitions["duplicate->rejected"] != 3 ||
			r.Transitions["duplicate->proposed"] != 0 || r.Revoked != 5 {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if got.Skipped != 0 {
		t.Errorf("skipped %d: a duplicate its representative's batch already moved is not skipped", got.Skipped)
	}
	for _, id := range []string{ids[2], ids[4]} {
		if st, by, of := stateOf(t, conn, id); st != "rejected" || by != "revert:jev:"+r1 || of != "" {
			t.Errorf("%s: %s %s %s", id, st, by, of)
		}
	}
	if st, by, _ := stateOf(t, conn, ids[3]); st != "rejected" || by != "revert:jev:"+r1 {
		t.Errorf("r2's duplicate of a reverted representative: %s by %s (attributed to the recipe being reverted)", st, by)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE kind = 'decision' AND outcome = 'reject'
		AND reason = 'duplicate_of_reverted' AND actor = $1 AND recipe_id = $2`, "revert:jev:"+r1, r1); n != 3 {
		t.Errorf("%d duplicate_of_reverted decisions, want 3", n)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events e WHERE e.kind = 'decision' AND e.outcome = 'duplicate'
		AND NOT EXISTS (SELECT 1 FROM finding_events v WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)`); n != 0 {
		t.Errorf("%d duplicate decisions still live", n)
	}
	// Idempotent, and the undo's own decisions are not undone by undoing again.
	again, err := d.RevertDecisions(ctx, scope, true)
	if err != nil || again.Moved != 0 || again.Cascaded != 0 || again.Revoked != 0 {
		t.Errorf("second revert %+v, %v", again, err)
	}
}

// A representative the undo returns to proposed takes its duplicates with
// it, and a revert of a revert's duplicate_of_reverted rejects… never
// happens: those are attributed to the undo, so only the representative's own
// return to proposed restores them.
func TestIntegrationRevertReturnsDuplicatesWithRepresentative(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 4)
	anchorAll(t, d, ids)
	r1 := strings.Repeat("e5", 32)
	registerTestRecipe(t, conn, r1, 3)
	sha := patch.ChunkHash("ganema said")
	// r1 rejects ids[0] and groups ids[2] under it.
	if _, err := d.ApplyDecisions(ctx, r1, []DecisionEvent{
		{FindingID: ids[0], RecipeID: r1, Outcome: OutcomeReject, Reason: "jev_reject", ChunkTextSHA256: sha},
		dupEvent(ids[2], ids[0], r1, sha),
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := d.RevertDecisions(ctx, RevertScope{FindingID: ids[0]}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Moved != 1 || rep.Cascaded != 1 || rep.Transitions["rejected->proposed"] != 1 || rep.Transitions["duplicate->proposed"] != 1 {
		t.Errorf("revert %+v", rep)
	}
	if st, by, of := stateOf(t, conn, ids[2]); st != "proposed" || by != "revert:jev:"+r1 || of != "" {
		t.Errorf("duplicate after its representative returned: %s %s %s", st, by, of)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1 AND kind = 'transition'
		AND to_state = 'proposed' AND reason = 'duplicate_released'`, ids[2]); n != 1 {
		t.Errorf("%d release transitions", n)
	}
}

// TestIntegrationDecideDuplicatesDownRefuses: once an event names a related
// finding, rolling migration 15 back refuses until the session opts in; the
// opted-in Down keeps the recorded duplicate decisions.
func TestIntegrationDecideDuplicatesDownRefuses(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 2)
	anchorAll(t, d, ids)
	r := strings.Repeat("f6", 32)
	registerTestRecipe(t, conn, r, 3)
	if _, err := d.ApplyDecisions(ctx, r, []DecisionEvent{
		{FindingID: ids[0], RecipeID: r, Outcome: OutcomeHold, Reason: "uncertain", ChunkTextSHA256: patch.ChunkHash("ganema said")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id, related_finding_id)
		VALUES ($1, $2, 'decision', 'duplicate', 'duplicate', $3, $4, $5)`, ids[1], feTranscript, "jev:"+r, r, ids[0]); err != nil {
		t.Fatal(err)
	}
	_, err := providerFor(t, dbURL).DownTo(ctx, 13)
	if err == nil || !strings.Contains(err.Error(), "refusing to roll back migration 15") {
		t.Fatalf("Down with related findings recorded: err = %v, want a refusal", err)
	}
	if v := gooseVersion(t, dbURL); v != 15 {
		t.Errorf("goose version %d after a refused Down, want 15", v)
	}
	var name string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `ALTER DATABASE `+pgx.Identifier{name}.Sanitize()+` SET earmark.allow_data_loss = 'on'`); err != nil {
		t.Fatal(err)
	}
	if _, err := providerFor(t, dbURL).DownTo(ctx, 14); err != nil {
		t.Fatalf("Down with earmark.allow_data_loss = on: %v", err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'finding_events' AND column_name = 'related_finding_id'`); n != 0 {
		t.Error("the opted-in Down kept related_finding_id")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE outcome = 'duplicate'`); n != 1 {
		t.Errorf("%d duplicate decisions after Down, want the recorded one kept", n)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id)
		VALUES ($1, $2, 'decision', 'duplicate', 'duplicate', $3, $4)`, ids[1], feTranscript, "jev:"+r, r); err == nil {
		t.Error("the restored outcome CHECK must refuse a new duplicate decision")
	}
	// …and Up again: the duplicate decision the Down kept has no
	// related_finding_id now, which Up's NOT VALID CHECK leaves alone.
	if _, err := providerFor(t, dbURL).UpTo(ctx, 15); err != nil {
		t.Fatalf("Up after an opted-in Down: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id)
		VALUES ($1, $2, 'decision', 'duplicate', 'duplicate', $3, $4)`, ids[1], feTranscript, "jev:"+r, r); err == nil {
		t.Error("a new duplicate decision without a representative must still be refused")
	}
}

// An overlap_dup reject is conditional on its winner's accept in the same
// write: with the winner absent (capped, or decided elsewhere) or skipped
// (locked), the loser is held overlap_pending naming it; with the winner
// accepted, it is rejected.
func TestIntegrationLoserRejectRequiresWinnerAccept(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 6)
	anchorAll(t, d, ids)
	r := strings.Repeat("a7", 32)
	registerTestRecipe(t, conn, r, 3)
	sha := patch.ChunkHash("ganema said")
	loser := func(id, winner string) DecisionEvent {
		return DecisionEvent{FindingID: id, RecipeID: r, Outcome: OutcomeReject, Reason: "overlap_dup",
			RelatedFindingID: winner, RequiresAccepted: winner, ChunkTextSHA256: sha}
	}
	// Winner absent from the write.
	res, err := d.ApplyDecisions(ctx, r, []DecisionEvent{loser(ids[2], ids[0])})
	if err != nil || len(res.Held) != 1 || len(res.Rejected) != 0 {
		t.Fatalf("winner absent: %+v, %v", res, err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_events WHERE finding_id = $1 AND kind = 'decision'
		AND outcome = 'hold' AND reason = 'overlap_pending' AND related_finding_id = $2`, ids[2], ids[0]); n != 1 {
		t.Errorf("loser of an absent winner: %d overlap_pending holds", n)
	}
	// Winner accepted in the same write.
	res, err = d.ApplyDecisions(ctx, r, []DecisionEvent{
		{FindingID: ids[0], RecipeID: r, Outcome: OutcomeApply, Reason: "confident_with_evidence", ChunkTextSHA256: sha},
		loser(ids[4], ids[0]),
	})
	if err != nil || len(res.Accepted) != 1 || len(res.Rejected) != 1 {
		t.Fatalf("winner accepted: %+v, %v", res, err)
	}
	if st, _, _ := stateOf(t, conn, ids[4]); st != "rejected" {
		t.Errorf("loser of an accepted winner is %s", st)
	}
}

// A representative that is no longer a live fix (here: rejected between
// grouping and the write) cannot take duplicates: they are skipped.
func TestIntegrationDuplicateOfRejectedRepresentativeSkipped(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	ids := seedFindingEvents(t, conn, 4)
	anchorAll(t, d, ids)
	r := strings.Repeat("b8", 32)
	registerTestRecipe(t, conn, r, 3)
	if err := d.SetPatchState(ctx, ids[0], "proposed", "rejected", "mcp:alice"); err != nil {
		t.Fatal(err)
	}
	res, err := d.ApplyDecisions(ctx, r, []DecisionEvent{dupEvent(ids[2], ids[0], r, patch.ChunkHash("ganema said"))})
	if err != nil || len(res.Skipped) != 1 || len(res.Duplicates) != 0 {
		t.Fatalf("%+v, %v", res, err)
	}
	if st, _, _ := stateOf(t, conn, ids[2]); st != "proposed" {
		t.Errorf("duplicate of a rejected representative is %s", st)
	}
}
