package decide

// Postgres proof of the audio-vote rung (CONTRACT §2.19 "Audio-vote rung"):
// against held findings with pilot votes it writes only applies and rejects,
// attributed to its own recipe with the votes in finding_events.detail; holds
// stay as they were; --max-accepts caps; the should_apply recipe stays current
// and its work list is unchanged; and `decide revert --recipe` of the audio
// recipe undoes exactly the rung's decisions. Skipped unless
// EARMARK_TEST_DATABASE_URL is set.

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/asrpilot"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

const (
	avIT1 = "00000000-0000-0000-0000-0000000000f1" // auto sebo → Arecibo: both heard the replacement → apply
	avIT2 = "00000000-0000-0000-0000-0000000000f2" // dish → telescope: rung 0 rejects (not_soundalike)
	avIT8 = "00000000-0000-0000-0000-0000000000f8" // picked → packed: both heard the original → reject
	avIT9 = "00000000-0000-0000-0000-0000000000f9" // sat → sad: one vote is on a stale chunk revision → hold
	avITa = "00000000-0000-0000-0000-0000000000fa" // signal → single: both heard the replacement, over --max-accepts
)

// seedAudioVote adds three held findings to seedDecide's chunk, holds f1, f2
// and the new ones under a should_apply recipe (made current), and records
// pilot votes. It returns the should_apply recipe id.
func seedAudioVote(t *testing.T, d *db.DB, conn *pgx.Conn) string {
	t.Helper()
	ctx := context.Background()
	seedDecide(t, conn)
	hash := patch.ChunkHash(runText)
	off := func(s string) int { return patch.Occurrences(runText, s)[0] }
	for _, f := range []struct{ id, orig, repl string }{
		{avIT8, "picked", "packed"}, {avIT9, "sat", "sad"}, {avITa, "signal", "single"},
	} {
		if _, err := conn.Exec(ctx, `
			INSERT INTO transcript_findings (id, transcript_id, file_path, chunk_index, start_sec, end_sec, original_text,
			       issue_type, suggested_correction, confidence, model, patch_state, origin, chunk_text_sha256,
			       anchor_offset, anchor_occurrence)
			VALUES ($1, $2, '/b/PHM/01.m4b', 0, 0, 6, $3, 'misheard_word', $4, 0.9, 'judge-model', 'proposed', 'judge', $5, $6, 0)`,
			f.id, itT, f.orig, f.repl, hash, off(f.orig)); err != nil {
			t.Fatalf("seed %s: %v", f.id, err)
		}
	}
	jev, err := ShouldApplyFn(DefaultShouldApplyParams())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetCurrentRecipes(ctx, jev.Recipe("")); err != nil {
		t.Fatal(err)
	}
	jevID := recipeID(t, jev.Recipe(""))
	p := func(v float64) *float64 { return &v }
	hold := func(id, reason string, pv *float64) db.DecisionEvent {
		return db.DecisionEvent{FindingID: id, RecipeID: jevID, Outcome: db.OutcomeHold, Reason: reason, P: pv,
			Evidence: db.EvidenceNone, ChunkTextSHA256: hash}
	}
	res, err := d.ApplyDecisions(ctx, jevID, []db.DecisionEvent{
		hold(avIT1, ReasonUncertain, p(0.5)),
		hold(avIT2, ReasonJevUnavailable, nil),
		hold(avIT8, ReasonUncertain, p(0.4)),
		hold(avIT9, ReasonUncertain, p(0.6)),
		hold(avITa, ReasonNoEvidence, p(0.9)),
	})
	if err != nil || len(res.Held) != 5 {
		t.Fatalf("seed holds: %+v, %v", res, err)
	}

	pilot := func(model, rev, scope string) int64 {
		var id int64
		if err := conn.QueryRow(ctx, `INSERT INTO pilot_transcripts (file_path, production_transcript_id, backend,
			model_id, model_revision, scope, has_word_times, segments, raw_text)
			VALUES ('/b/PHM/01.m4b', $1, 'x', $2, $3, $4, false, '[]', 'x') RETURNING id`, itT, model, rev, scope).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pa, pb := pilot(avA, avRevA, "full"), pilot(avB, avRevB, "full")
	paOld := pilot(avA, avRevA, "findings") // same model and revision, voted under an older rule
	pbRev := pilot(avB, "3333333333333333333333333333333333333333", "full")
	R, O := asrpilot.VoteHeardReplacement, asrpilot.VoteHeardOriginal
	ins := func(finding string, pid int64, model, sha, v, rule, updated string) {
		if _, err := conn.Exec(ctx, `INSERT INTO finding_asr_votes (finding_id, pilot_transcript_id, model_id,
			chunk_text_sha256, vote, detail, updated_at) VALUES ($1, $2, $3, $4, $5, jsonb_build_object('rule', $6::text), $7::timestamptz)`,
			finding, pid, model, sha, v, rule, updated); err != nil {
			t.Fatalf("vote %s/%d: %v", finding, pid, err)
		}
	}
	const t0, t1 = "2026-11-01T00:00:00Z", "2026-11-02T00:00:00Z"
	for _, id := range []string{avIT1, avIT2, avITa} {
		ins(id, pa, avA, hash, R, asrpilot.VoteRule, t0)
		ins(id, pb, avB, hash, R, asrpilot.VoteRule, t0)
	}
	// Newer, but under an older rule / by another revision: ignored.
	ins(avIT1, paOld, avA, hash, O, "asr_vote@v1", t1)
	ins(avIT1, pbRev, avB, hash, O, asrpilot.VoteRule, t1)
	ins(avIT8, pa, avA, hash, O, asrpilot.VoteRule, t0)
	ins(avIT8, pb, avB, hash, O, asrpilot.VoteRule, t0)
	ins(avIT9, pa, avA, hash, R, asrpilot.VoteRule, t0)
	ins(avIT9, pb, avB, "stale-chunk-revision", R, asrpilot.VoteRule, t0)
	return jevID
}

func TestIntegrationAudioVote(t *testing.T) {
	d, conn := integrationDB(t)
	ctx := context.Background()
	jevID := seedAudioVote(t, d, conn)
	jevWork := func() []string {
		t.Helper()
		ws, err := d.DecideWork(ctx, db.DecideWorkScope{RecipeID: jevID, RetryReason: ReasonJevUnavailable, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, f := range ws {
			ids = append(ids, f.ID)
		}
		return ids
	}
	jevBefore := jevWork()

	// The reader: the current vote per configured model, under the rule,
	// against the finding's chunk revision.
	votes, err := d.CurrentASRVotes(ctx, []string{avIT1, avIT9}, asrpilot.VoteRule, avConfig().Models)
	if err != nil {
		t.Fatal(err)
	}
	if v := votes[avIT1]; len(v) != 2 || v[0].ModelID != avA || v[0].Vote != asrpilot.VoteHeardReplacement ||
		v[1].ModelID != avB || v[1].ModelRevision != avRevB || v[1].Vote != asrpilot.VoteHeardReplacement {
		t.Errorf("f1 votes %+v (old-rule and other-revision votes must be ignored)", v)
	}
	if v := votes[avIT9]; len(v) != 1 || v[0].ModelID != avA {
		t.Errorf("f9 votes %+v (a vote on a stale chunk revision must be ignored)", v)
	}

	// Dry run: nothing written, not even the recipe.
	before := snapshot(t, conn)
	recipesBefore := countOf(t, conn, `SELECT count(*) FROM recipes`)
	dry, err := AudioVote(ctx, d, AudioVoteOptions{Config: avConfig(), MaxAccepts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, conn); !maps.Equal(before, after) || countOf(t, conn, `SELECT count(*) FROM recipes`) != recipesBefore {
		t.Error("the dry run wrote")
	}
	if dry.Findings != 5 || dry.Outcomes[DecisionApply] != 2 || dry.Outcomes[DecisionReject] != 2 ||
		dry.Reasons["hold/"+ReasonASRVoteMissing] != 1 || dry.Capped != 1 {
		t.Errorf("dry run %+v", dry)
	}

	rep, err := AudioVote(ctx, d, AudioVoteOptions{Config: avConfig(), Yes: true, MaxAccepts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RecipeID != dry.RecipeID {
		t.Error("dry run and --yes name different recipes")
	}
	if w := rep.Write; w.Accepted != 1 || w.Rejected != 2 || w.Held != 0 || w.Capped != 1 || w.Skipped != 0 {
		t.Errorf("write %+v", w)
	}
	actor := "jev:" + rep.RecipeID
	states := findingStates(t, conn)
	for id, want := range map[string]string{
		"f1": "accepted|" + actor, "f2": "rejected|" + actor, "f8": "rejected|" + actor,
		"f9": "proposed|", "fa": "proposed|",
	} {
		if states[id] != want {
			t.Errorf("%s: %q, want %q", id, states[id], want)
		}
	}
	type ev struct {
		finding, outcome, reason string
		p                        *float64
		evidence                 *string
		detail                   []byte
	}
	rows, err := conn.Query(ctx, `SELECT right(finding_id::text, 2), outcome, reason, p, evidence, detail
		FROM finding_events WHERE kind = 'decision' AND actor = $1 AND recipe_id = $2 ORDER BY finding_id`, actor, rep.RecipeID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ev, error) {
		var e ev
		return e, r.Scan(&e.finding, &e.outcome, &e.reason, &e.p, &e.evidence, &e.detail)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 || evs[0].finding != "f1" || evs[0].outcome != "apply" || evs[0].reason != ReasonASRAgree ||
		evs[1].finding != "f2" || evs[1].reason != ReasonNotSoundAlike ||
		evs[2].finding != "f8" || evs[2].outcome != "reject" || evs[2].reason != ReasonASRHeardOriginal {
		t.Fatalf("events %+v", evs)
	}
	if evs[0].p != nil || evs[0].evidence == nil || *evs[0].evidence != db.EvidenceNone {
		t.Errorf("apply event p %v evidence %v; want no p, evidence none", evs[0].p, evs[0].evidence)
	}
	var detail struct {
		Rung, Rule string
		Votes      []db.ASRVote
	}
	if err := json.Unmarshal(evs[0].detail, &detail); err != nil {
		t.Fatal(err)
	}
	var voteIDs []int64
	if err := conn.QueryRow(ctx, `SELECT array_agg(v.id ORDER BY p.model_id) FROM finding_asr_votes v
		JOIN pilot_transcripts p ON p.id = v.pilot_transcript_id
		WHERE v.finding_id = $1 AND p.scope = 'full' AND p.model_revision IN ($2, $3)`, avIT1, avRevA, avRevB).Scan(&voteIDs); err != nil {
		t.Fatal(err)
	}
	if detail.Rung != AudioVoteDetailRung || detail.Rule != asrpilot.VoteRule || len(detail.Votes) != 2 ||
		detail.Votes[0].VoteID != voteIDs[0] || detail.Votes[1].VoteID != voteIDs[1] || detail.Votes[0].UpdatedAt.IsZero() {
		t.Errorf("detail %s, want votes %v", evs[0].detail, voteIDs)
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events WHERE detail IS NOT NULL AND actor <> $1`, actor); n != 0 {
		t.Errorf("%d events by other actors carry a detail", n)
	}

	// The should_apply recipe stays current; its work list only gains the
	// re-check of the audio accept (another recipe's accept), nothing else.
	var current string
	if err := conn.QueryRow(ctx, `SELECT recipe_id FROM current_recipes WHERE step = 'decide'`).Scan(&current); err != nil || current != jevID {
		t.Errorf("current decide recipe %q (%v), want the should_apply recipe", current, err)
	}
	jevAfter := jevWork()
	if !slices.Equal(jevBefore, []string{avIT2}) || !slices.Equal(jevAfter, []string{avIT1}) {
		t.Errorf("should_apply work list %v → %v; want [f2] → [f1] (only its retryable hold, then only the "+
			"re-check of the audio accept: the rung's rejects leave it, its holds are never written)", jevBefore, jevAfter)
	}

	// A re-run: f1 is decided, f2/f8 rejected; fa (no cap now) applies, f9 stays held.
	again, err := AudioVote(ctx, d, AudioVoteOptions{Config: avConfig(), Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.Findings != 2 || again.Write.Accepted != 1 || again.Reasons["hold/"+ReasonASRVoteMissing] != 1 {
		t.Errorf("re-run %+v %+v", again.Reasons, again.Write)
	}

	// Revert by the audio recipe undoes only the rung's decisions.
	jevEvents := countOf(t, conn, `SELECT count(*) FROM finding_events e WHERE e.kind = 'decision' AND e.recipe_id = $1
		AND NOT EXISTS (SELECT 1 FROM finding_events r WHERE r.kind = 'revoke' AND r.revokes_event_id = e.id)`, jevID)
	rr, err := d.RevertDecisions(ctx, db.RevertScope{RecipeID: rep.RecipeID}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Moved != 4 || rr.Revoked != 4 {
		t.Errorf("revert %+v, want 4 moved and 4 revoked", rr)
	}
	states = findingStates(t, conn)
	revert := "revert:" + actor
	for id, want := range map[string]string{
		"f1": "rejected|" + revert, "fa": "rejected|" + revert, "f2": "proposed|" + revert, "f8": "proposed|" + revert, "f9": "proposed|",
	} {
		if states[id] != want {
			t.Errorf("after revert %s: %q, want %q", id, states[id], want)
		}
	}
	if n := countOf(t, conn, `SELECT count(*) FROM finding_events e WHERE e.kind = 'decision' AND e.recipe_id = $1
		AND NOT EXISTS (SELECT 1 FROM finding_events r WHERE r.kind = 'revoke' AND r.revokes_event_id = e.id)`, jevID); n != jevEvents {
		t.Errorf("revert touched the should_apply recipe's decisions: %d live, want %d", n, jevEvents)
	}
	// f2 and f8 are held again by their should_apply hold.
	held, err := d.AudioVoteWork(ctx, db.AudioVoteScope{Rule: asrpilot.VoteRule, Models: avConfig().Models, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var heldIDs []string
	for _, h := range held {
		heldIDs = append(heldIDs, h.ID)
		if h.HoldRecipeID != jevID {
			t.Errorf("%s held by %s, want the should_apply recipe", h.ID, h.HoldRecipeID)
		}
	}
	if !slices.Equal(heldIDs, []string{avIT2, avIT8, avIT9}) {
		t.Errorf("held after revert %v", heldIDs)
	}
}
