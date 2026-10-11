package db

// Postgres integration test for migration 14's recipe_scorecard and
// decide_scorecard over a small hand-built history. Skipped unless
// EARMARK_TEST_DATABASE_URL is set (see migrate_integration_test.go).

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Scorecard fixture recipes.
const (
	scJob2        = "00000000-0000-0000-0000-00000000c002"
	scTranscript2 = "00000000-0000-0000-0000-00000000c102"
	scChunkT2     = "00000000-0000-0000-0000-00000000c200"
	scGemma       = "a000000000000000000000000000000000000000000000000000000000000001" // propose, legacy
	scLuna        = "a000000000000000000000000000000000000000000000000000000000000002" // propose, recorded
	scJev         = "d000000000000000000000000000000000000000000000000000000000000001" // decide, first
	scClef        = "d000000000000000000000000000000000000000000000000000000000000002" // decide, later
)

// scorecardFixture names the seeded findings.
type scorecardFixture struct {
	gemmaName, gemmaWord, gemmaRevert, gemmaAlone, lunaName, lunaHold, legacy, unattributed string
}

// seedScorecard builds, on one transcript with two chunks:
//
//	gemma (legacy: only a legacy pass): G1 "ganema"→"Ghanima" (proper noun,
//	  applied by Jev), G2 "left"→"lift" (Jev rejects, Clef applies), G3
//	  "the the"→"the" (applied by Jev, reverted, Jev's decision revoked).
//	luna (recorded pass + judge calls): L1 the same fix as G1 (agrees; a
//	  duplicate of G1), L2 "hall"→"haul" (Jev holds; a human rejects).
//	seedLabelChunks' three unattributed findings: one the same fix as G1,
//	  one archived, one with no chunk hash.
//
// Labels: G1's fix good, G2's junk (human) over an opus good, G3's partial,
// L2's unsure, the legacy finding good (by id).
func seedScorecard(t *testing.T, conn *pgx.Conn, d *DB) scorecardFixture {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO recipes (recipe_id, step, step_version, code_version, model_alias, prompt_version, created_at) VALUES
		($1, 'propose', 1, 't', 'gemma3:12b', 'judge@v1', '2026-06-01'),
		($2, 'propose', 2, 't', 'openrouter/openai/gpt-6-luna', 'judge@v2', '2026-10-01'),
		($3, 'decide', 1, 't', 'jev-1.13.0', 'should_apply@v2', '2026-08-01'),
		($4, 'decide', 1, 't', 'clef:27b', 'should_apply@v2', '2026-10-02')`, scGemma, scLuna, scJev, scClef)
	seedLabelChunks(t, conn) // transcript, chunks and three unattributed (NULL-recipe) findings
	h0, h1 := sha(lbChunk0Text), sha(lbChunk1Text)
	ins := func(recipe *string, chunk string, idx int, hash *string, orig, corr, issue string, at string) string {
		t.Helper()
		var id string
		if err := conn.QueryRow(ctx, `
			INSERT INTO transcript_findings (transcript_id, file_path, chunk_id, chunk_index, start_sec, end_sec,
			       original_text, issue_type, suggested_correction, confidence, model, chunk_text_sha256,
			       recipe_id, created_at, anchor_offset)
			VALUES ($1, '/b/LB/01.m4b', $2, $3, 0, 30, $4, $5, $6, 0.9, 'm', $7, $8, $9, 0)
			RETURNING id`, lbTranscript, chunk, idx, orig, issue, corr, hash, recipe, at).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	g, l := scGemma, scLuna
	f := scorecardFixture{
		gemmaName:   ins(&g, lbChunk0, 0, &h0, "ganema", "Ghanima", "misheard_proper_noun", "2026-06-02"),
		gemmaWord:   ins(&g, lbChunk1, 1, &h1, "left", "lift", "misheard_word", "2026-06-02"),
		gemmaRevert: ins(&g, lbChunk1, 1, &h1, "the the", "the", "repeated_text", "2026-06-03"),
		lunaName:    ins(&l, lbChunk0, 0, &h0, " Ganema", "GHANIMA", "misheard_proper_noun", "2026-10-03"),
		lunaHold:    ins(&l, lbChunk1, 1, &h1, "hall", "haul", "homophone", "2026-10-03"),
	}
	// A second transcript only gemma judged: its fix is not comparable
	// for agreement (no other configuration looked at it).
	exec(`INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES ($1, '/b/LB/02.m4b', 'lb2', 'done')`, scJob2)
	exec(`INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name)
		VALUES ($1, $2, '/b/LB/02.m4b', 'lb2', 'en', 60, '[]', 'x', 'parakeet')`, scTranscript2, scJob2)
	if err := conn.QueryRow(ctx, `
		INSERT INTO transcript_findings (transcript_id, file_path, chunk_id, chunk_index, start_sec, end_sec,
		       original_text, issue_type, suggested_correction, confidence, model, chunk_text_sha256, recipe_id, created_at)
		VALUES ($1, '/b/LB/02.m4b', $2, 0, 0, 30, 'tullah', 'misheard_proper_noun', 'Tleilax', 0.9, 'm', $3, $4, '2026-06-03')
		RETURNING id`, scTranscript2, scChunkT2, sha("only gemma judged this"), scGemma).Scan(&f.gemmaAlone); err != nil {
		t.Fatal(err)
	}
	// seedLabelChunks' finding with no chunk hash, labelled by id below.
	if err := conn.QueryRow(ctx, `SELECT id FROM transcript_findings WHERE recipe_id IS NULL AND chunk_text_sha256 IS NULL`).
		Scan(&f.legacy); err != nil {
		t.Fatal(err)
	}

	if err := conn.QueryRow(ctx, `SELECT id FROM transcript_findings WHERE recipe_id IS NULL AND chunk_text_sha256 = $1`, h0).
		Scan(&f.unattributed); err != nil {
		t.Fatal(err)
	}

	// Judge passes: gemma legacy on both transcripts; luna recorded on the
	// first, plus a legacy pass there (a mixed recipe). Values deliberately
	// unequal: 5 chunks, 4 judged (1 cached, 1 failed), 2 kept + 1 already
	// recorded, $0.006; a non-number drop value is ignored.
	exec(`INSERT INTO judge_passes (transcript_id, job_id, file_path, recipe_id, status, findings_kept, started_at, finished_at)
		VALUES ($1, $2, '/b/LB/01.m4b', $3, 'legacy', 3, '2026-06-02', '2026-06-03'),
		       ($4, $5, '/b/LB/02.m4b', $3, 'legacy', 1, '2026-06-03', '2026-06-03'),
		       ($1, $2, '/b/LB/01.m4b', $6, 'legacy', 0, '2026-10-02', '2026-10-02')`,
		lbTranscript, lbJob, scGemma, scTranscript2, scJob2, scLuna)
	exec(`INSERT INTO judge_passes (transcript_id, job_id, file_path, recipe_id, status, chunks_total, chunks_judged,
		       chunks_failed, chunks_cached, findings_kept, dropped, cost_usd, latency_ms_p50, latency_ms_p95,
		       started_at, finished_at)
		VALUES ($1, $2, '/b/LB/01.m4b', $3, 'complete', 5, 4, 1, 1, 2,
		        '{"cosmetic_only": 3, "over_cap": 1, "already_recorded": 1, "note": "x"}',
		        0.006, 1000, 3000, '2026-10-03', '2026-10-03 00:10')`, lbTranscript, lbJob, scLuna)
	// Luna's judge calls: two answered, one cached, one unparseable.
	call := func(recipe string, fn, inHash string, latency int, cost float64, cache bool, errClass *string, at string) {
		exec(`INSERT INTO fn_calls (fn, prompt_version, prompt_sha256, model_alias, recipe_id, input_sha256, input,
		       latency_ms, cost_usd, cache_hit, error_class, created_at)
		      VALUES ($1, 'v', 'p', 'm', $2, $3, '{}', $4, $5, $6, $7, $8)`,
			fn, recipe, sha(inHash), latency, cost, cache, errClass, at)
	}
	invalid := "invalid_reply"
	call(scLuna, "judge_chunk", "c0", 1200, 0.001, false, nil, "2026-10-03")
	call(scLuna, "judge_chunk", "c3", 2000, 0.001, false, nil, "2026-10-03")
	call(scLuna, "judge_chunk", "c1", 0, 0, true, nil, "2026-10-03")
	call(scLuna, "judge_chunk", "c2", 900, 0.001, false, &invalid, "2026-10-03")
	// Jev: two answered calls ($0.0001, $0.0003) and one failed ($0.0005).
	timeout := "timeout"
	call(scJev, "should_apply", "j1", 800, 0.0001, false, nil, "2026-09-01")
	call(scJev, "should_apply", "j2", 1200, 0.0003, false, nil, "2026-09-01")
	call(scJev, "should_apply", "j3", 9000, 0.0005, false, &timeout, "2026-09-01")
	call(scClef, "should_apply", "k1", 400, 0, false, nil, "2026-10-05")

	// Decisions.
	decide := func(finding, recipe, outcome, reason string, p float64, evidence, at string) int64 {
		t.Helper()
		var id int64
		if err := conn.QueryRow(ctx, `
			INSERT INTO finding_events (finding_id, transcript_id, kind, outcome, reason, actor, recipe_id, p, evidence, created_at)
			VALUES ($1, $2, 'decision', $3, $4, 'jev:' || $5, $5, $6, $7, $8) RETURNING id`,
			finding, lbTranscript, outcome, reason, recipe, p, evidence, at).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	move := func(finding, state, by string) {
		exec(`UPDATE transcript_findings SET patch_state = $2, decided_by = $3, decided_at = now(),
		       applied_at = CASE WHEN $2 = 'applied' THEN now() END WHERE id = $1`, finding, state, by)
	}
	decide(f.gemmaName, scJev, "apply", "confident_with_evidence", 0.9, "asin_verbatim", "2026-09-01")
	move(f.gemmaName, "accepted", "jev:"+scJev)
	move(f.gemmaName, "applied", "jev:"+scJev)
	decide(f.gemmaWord, scJev, "reject", "jev_reject", 0.1, "none", "2026-09-01")
	move(f.gemmaWord, "rejected", "jev:"+scJev)
	revoked := decide(f.gemmaRevert, scJev, "apply", "confident_with_evidence", 0.95, "exact_repeat", "2026-09-01")
	move(f.gemmaRevert, "accepted", "jev:"+scJev)
	move(f.gemmaRevert, "applied", "jev:"+scJev)
	move(f.gemmaRevert, "reverted", "revert:jev:"+scJev)
	exec(`INSERT INTO finding_events (finding_id, transcript_id, kind, reason, actor, recipe_id, revokes_event_id)
		VALUES ($1, $2, 'revoke', 'decide revert', 'revert:jev:' || $3, $3, $4)`, f.gemmaRevert, lbTranscript, scJev, revoked)
	decide(f.lunaHold, scJev, "hold", "uncertain", 0.5, "none", "2026-10-04")
	move(f.lunaHold, "rejected", "mcp:agent") // a human rejects the held finding
	exec(`UPDATE transcript_findings SET patch_state = 'duplicate', duplicate_of = $2 WHERE id = $1`, f.lunaName, f.gemmaName)
	// Jev holds the duplicate: its fix (G1's) must not count twice in
	// Jev's label outcomes.
	decide(f.lunaName, scJev, "hold", "uncertain", 0.55, "none", "2026-10-04")
	decide(f.gemmaName, scClef, "apply", "confident_with_evidence", 0.97, "asin_verbatim", "2026-10-05")
	// Clef also applies the unattributed finding of G1's fix: one fix, one
	// label count.
	decide(f.unattributed, scClef, "apply", "confident_with_evidence", 0.9, "asin_verbatim", "2026-10-05")
	decide(f.gemmaWord, scClef, "apply", "confident_with_evidence", 0.8, "asin_verbatim", "2026-10-05")

	// Labels.
	day := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	imports := []LabelImport{
		{Set: "bakeoff", Labeler: "opus:bakeoff", Items: []LabelItem{
			{FindingID: f.gemmaName, Label: LabelGood, LabelledAt: day},
			{FindingID: f.gemmaWord, Label: LabelGood, LabelledAt: day.Add(24 * time.Hour)},
			{ChunkID: lbChunk1, Original: "the the", Correction: "the", Label: LabelPartial, LabelledAt: day},
			{FindingID: f.lunaHold, Label: LabelUnsure, LabelledAt: day},
			{FindingID: f.legacy, Label: LabelGood, LabelledAt: day},
		}},
		{Set: "review", Labeler: "human:t", Items: []LabelItem{
			{FindingID: f.gemmaWord, Label: LabelJunk, LabelledAt: day}}},
	}
	for _, imp := range imports {
		rep, err := d.ImportLabels(ctx, imp)
		if err != nil || rep.Unresolved != 0 {
			t.Fatalf("import %s: %+v, %v", imp.Set, rep, err)
		}
	}
	return f
}

func approx(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-9 {
		if got == nil {
			t.Errorf("%s = NULL, want %v", name, want)
		} else {
			t.Errorf("%s = %v, want %v", name, *got, want)
		}
	}
}

func TestIntegrationScorecards(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	seedScorecard(t, conn, d)

	props, err := d.RecipeScorecard(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ProposeScore{}
	for _, p := range props {
		by[p.RecipeID] = p
	}
	if len(props) != 3 {
		t.Fatalf("%d propose rows, want gemma, luna and unattributed: %+v", len(props), props)
	}

	gm := by[scGemma]
	if !gm.Legacy || gm.Mixed || gm.ChunksJudged != nil || gm.CostPerChunkUSD != nil || gm.LatencyMsP50 != nil ||
		gm.Drops != nil || gm.ParseFailures != nil || gm.FindingsPer1kChunks != nil {
		t.Errorf("gemma must be legacy with nothing per-chunk recorded: %+v", gm)
	}
	if gm.FindingsTotal != 4 || gm.PassesLegacy != 2 || gm.TranscriptsJudged != 2 {
		t.Errorf("gemma findings %d, legacy passes %d, transcripts %d", gm.FindingsTotal, gm.PassesLegacy, gm.TranscriptsJudged)
	}
	if gm.IssueMix["misheard_proper_noun"] != 2 || gm.IssueMix["misheard_word"] != 1 || gm.IssueMix["repeated_text"] != 1 {
		t.Errorf("gemma issue mix %v", gm.IssueMix)
	}
	if gm.StateMix["applied"] != 1 || gm.StateMix["rejected"] != 1 || gm.StateMix["reverted"] != 1 || gm.StateMix["proposed"] != 1 {
		t.Errorf("gemma state mix %v", gm.StateMix)
	}
	if gm.AppliedEver != 2 || gm.Reverted != 1 {
		t.Errorf("gemma applied %d reverted %d, want 2/1", gm.AppliedEver, gm.Reverted)
	}
	approx(t, "gemma revert rate", gm.RevertRate, 0.5)
	// 4 fixes, 3 on the transcript luna also judged, 1 of them agreed: the
	// fix on the transcript only gemma judged is not in the denominator.
	if gm.CorrectionKeys != 4 || gm.ComparableKeys != 3 || gm.AgreedKeys != 1 {
		t.Errorf("gemma keys %d comparable %d agreed %d, want 4/3/1", gm.CorrectionKeys, gm.ComparableKeys, gm.AgreedKeys)
	}
	approx(t, "gemma agreement", gm.AgreementRate, 1.0/3)
	if gm.Labels.Good != 1 || gm.Labels.Junk != 1 || gm.Labels.Partial != 1 || gm.Labels.Unsure != 0 {
		t.Errorf("gemma labels %+v (the human junk must outrank the newer opus good)", gm.Labels)
	}
	approx(t, "gemma precision", gm.Labels.Precision, 1.0/3)
	approx(t, "gemma proper-noun precision", gm.ProperNounLabels.Precision, 1)
	if len(gm.Decisions) != 2 {
		t.Fatalf("gemma decisions %+v, want Jev and Clef", gm.Decisions)
	}
	jev, clef := gm.Decisions[0], gm.Decisions[1] // ordered by recipe id: jev's d…1 < clef's d…2
	// G3's Jev apply was revoked: Jev's live decisions are G1 apply, G2 reject.
	if jev.DecideRecipeID != scJev || jev.Decided != 2 || jev.Apply != 1 || jev.Reject != 1 ||
		jev.Reasons["reject:jev_reject"] != 1 || jev.DecideModel != "jev-1.13.0" {
		t.Errorf("Jev on gemma %+v", jev)
	}
	if clef.DecideRecipeID != scClef || clef.Decided != 2 || clef.Apply != 2 {
		t.Errorf("Clef on gemma %+v", clef)
	}

	ln := by[scLuna]
	if ln.Legacy || !ln.Mixed || ln.PassesComplete != 1 || ln.PassesLegacy != 1 {
		t.Errorf("luna must be recorded and mixed: %+v", ln)
	}
	if ln.ChunksJudged == nil || *ln.ChunksJudged != 4 || ln.ChunksCached == nil || *ln.ChunksCached != 1 ||
		ln.ChunksFailed == nil || *ln.ChunksFailed != 1 {
		t.Errorf("luna chunks %v/%v/%v, want 4 judged, 1 cached, 1 failed", ln.ChunksJudged, ln.ChunksCached, ln.ChunksFailed)
	}
	approx(t, "luna cached share", ln.CachedShare, 0.25)
	approx(t, "luna findings per 1k (kept + already recorded)", ln.FindingsPer1kChunks, 750)
	approx(t, "luna cost per chunk", ln.CostPerChunkUSD, 0.0015)
	approx(t, "luna latency p50 (answered calls)", ln.LatencyMsP50, 1600)
	if ln.LatencySource != "calls" {
		t.Errorf("luna latency source %q", ln.LatencySource)
	}
	if len(ln.Drops) != 3 || ln.Drops["cosmetic_only"] != 3 || ln.Drops["over_cap"] != 1 || ln.Drops["already_recorded"] != 1 {
		t.Errorf("luna drops %v (a non-number value must be ignored)", ln.Drops)
	}
	if ln.ParseFailures == nil || *ln.ParseFailures != 1 {
		t.Errorf("luna parse failures %v", ln.ParseFailures)
	}
	if ln.StateMix["duplicate"] != 1 {
		t.Errorf("luna state mix %v, want the duplicate", ln.StateMix)
	}
	if ln.CorrectionKeys != 2 || ln.ComparableKeys != 2 || ln.AgreedKeys != 1 {
		t.Errorf("luna keys %d comparable %d agreed %d", ln.CorrectionKeys, ln.ComparableKeys, ln.AgreedKeys)
	}
	approx(t, "luna agreement", ln.AgreementRate, 0.5)
	// L1 shares G1's key → good; L2 unsure.
	if ln.Labels.Good != 1 || ln.Labels.Unsure != 1 {
		t.Errorf("luna labels %+v", ln.Labels)
	}
	approx(t, "luna precision", ln.Labels.Precision, 1)

	un := by[""]
	if un.FindingsTotal != 3 || un.Labels.Good != 2 { // the legacy finding by id, and one sharing G1's fix
		t.Errorf("unattributed row %+v", un)
	}

	decs, err := d.DecideScorecard(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(decs) != 2 || decs[0].RecipeID != scJev || decs[1].RecipeID != scClef {
		t.Fatalf("decide rows %+v, want Jev then Clef", decs)
	}
	j, c := decs[0], decs[1]
	if j.Decisions != 5 || j.Revoked != 1 || j.Live != 4 || j.Apply != 1 || j.Hold != 2 || j.Reject != 1 {
		t.Errorf("Jev counts %+v", j)
	}
	approx(t, "Jev revoke rate", j.RevokeRate, 0.2)
	if j.OutcomeEvidence["apply"]["asin_verbatim"] != 1 || j.OutcomeEvidence["hold"]["none"] != 2 {
		t.Errorf("Jev outcome × evidence %v", j.OutcomeEvidence)
	}
	if lo := j.LabelOutcomes["apply"]; lo.Good != 1 || lo.Decided() != 1 {
		t.Errorf("Jev apply labels %+v", lo)
	}
	if lo := j.LabelOutcomes["reject"]; lo.Junk != 1 {
		t.Errorf("Jev reject labels %+v", lo)
	}
	// The held duplicate L1 shares G1's (good) fix: per fix it is not
	// counted again under hold.
	if lo := j.LabelOutcomes["hold"]; lo.Good != 0 || lo.Unsure != 1 {
		t.Errorf("Jev hold labels %+v, want only L2's unsure", lo)
	}
	if b := j.LabelCalibration[4]; b.Good != 1 || b.Rate == nil || *b.Rate != 1 {
		t.Errorf("Jev p bucket 5 %+v", b)
	}
	if b := j.LabelCalibration[0]; b.Junk != 1 {
		t.Errorf("Jev p bucket 1 %+v", b)
	}
	if b := j.HumanCalibration[2]; b.Rejected != 1 || b.Rate == nil || *b.Rate != 0 {
		t.Errorf("Jev human bucket 3 %+v", b)
	}
	if j.HumanDecided != 0 || j.PrevRecipeID != "" {
		t.Errorf("Jev human_decided %d prev %q (a hold is not an apply/reject; Jev is first)", j.HumanDecided, j.PrevRecipeID)
	}
	if j.Calls != 2 || j.CallErrors != 1 {
		t.Errorf("Jev calls %d errors %d", j.Calls, j.CallErrors)
	}
	approx(t, "Jev cost (all non-cached calls)", j.CostUSD, 0.0009)
	approx(t, "Jev cost per answered call", j.CostPerCallUSD, 0.0002)
	approx(t, "Jev latency p50", j.LatencyMsP50, 1000)

	if c.Live != 3 || c.Apply != 3 {
		t.Errorf("Clef counts %+v", c)
	}
	if c.PrevRecipeID != scJev || c.FlipOverlap != 2 || c.Flips != 1 {
		t.Errorf("Clef flips vs %q: %d of %d, want 1 of 2 vs Jev", c.PrevRecipeID, c.Flips, c.FlipOverlap)
	}
	approx(t, "Clef flip rate", c.FlipRate, 0.5)
	// Clef applied G1 and the unattributed finding of the same fix: one
	// good fix, one junk (G2) — 1/2, not 2/3.
	if lo := c.LabelOutcomes["apply"]; lo.Good != 1 || lo.Junk != 1 {
		t.Errorf("Clef apply labels %+v", lo)
	}
	approx(t, "Clef apply precision", c.LabelOutcomes["apply"].Precision, 0.5)

	// --since: gemma's findings are from June, but Clef decided on them in
	// October, so it is still active then; Jev last decided on 10-04.
	since := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	recent, err := d.RecipeScorecard(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range recent {
		found = found || p.RecipeID == scGemma
	}
	if !found {
		t.Error("--since 2026-10-05 must keep gemma: decisions on its findings are activity")
	}
	recentDec, err := d.DecideScorecard(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(recentDec) != 1 || recentDec[0].RecipeID != scClef {
		t.Errorf("--since 2026-10-05 decide rows %+v, want Clef only", recentDec)
	}
}
