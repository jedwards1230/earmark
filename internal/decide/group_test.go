package decide

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// testKey stands in for transcript_findings.correction_key in fixtures: equal
// for the same fix to the same chunk text, whatever the recipe.
func testKey(chunk, original, correction string) string {
	return patch.ChunkHash(chunk) + "|" + strings.ToLower(original) + "|" + strings.ToLower(correction)
}

// keyed is finding() with its correction key and creation time set.
func keyed(id, tid, path, issue, chunk, original, replacement string, conf float64, created time.Time) db.DecideFinding {
	f := finding(id, tid, path, issue, chunk, original, replacement, 0, conf)
	f.CorrectionKey = testKey(chunk, original, replacement)
	f.CreatedAt = created
	return f
}

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func TestGroupChunkRepresentative(t *testing.T) {
	const text = "the dish at auto sebo picked up the signal"
	mk := func(id string, mut func(*db.DecideFinding)) db.DecideFinding {
		f := keyed(id, runT1, "/b/x/1.m4b", IssueMisheardProperNoun, text, "auto sebo", "Arecibo", 0.9, t0)
		if mut != nil {
			mut(&f)
		}
		return f
	}
	newer := func(d time.Duration) func(*db.DecideFinding) {
		return func(f *db.DecideFinding) { f.CreatedAt = t0.Add(d) }
	}
	tests := []struct {
		name     string
		cands    []db.DecideFinding
		overlay  []db.DecideFinding
		wantRep  string
		wantTier string
		wantDups []string
	}{
		{
			name:    "newest wins when nothing else separates them",
			cands:   []db.DecideFinding{mk("a", nil), mk("b", newer(time.Hour)), mk("c", newer(time.Minute))},
			wantRep: "b", wantTier: TierNewest, wantDups: []string{"a", "c"},
		},
		{
			name: "the current propose recipe beats the newest",
			cands: []db.DecideFinding{
				mk("old-gemma", func(f *db.DecideFinding) { f.CurrentPropose = true }),
				mk("newer", newer(time.Hour)),
			},
			wantRep: "old-gemma", wantTier: TierCurrentRecipe, wantDups: []string{"newer"},
		},
		{
			name: "an accepted or applied member beats the current recipe",
			cands: []db.DecideFinding{
				mk("luna", func(f *db.DecideFinding) { f.CurrentPropose = true; f.CreatedAt = t0.Add(time.Hour) }),
			},
			overlay: []db.DecideFinding{mk("applied", func(f *db.DecideFinding) {
				f.PatchState, f.DecidedBy = patch.StateApplied, "jev:"+strings.Repeat("1", 64)
			})},
			wantRep: "applied", wantTier: TierApplied, wantDups: []string{"luna"},
		},
		{
			name: "an existing representative keeps its group (no flip-flop)",
			cands: []db.DecideFinding{
				mk("rep", func(f *db.DecideFinding) { f.HasDuplicates = true }),
				mk("luna", func(f *db.DecideFinding) { f.CurrentPropose = true; f.CreatedAt = t0.Add(time.Hour) }),
			},
			wantRep: "rep", wantTier: TierExisting, wantDups: []string{"luna"},
		},
		{
			name: "a finding already decided under this recipe stays the representative",
			cands: []db.DecideFinding{
				mk("held", func(f *db.DecideFinding) { f.DecidedHere = true }),
				mk("luna", func(f *db.DecideFinding) { f.CurrentPropose = true; f.CreatedAt = t0.Add(time.Hour) }),
			},
			wantRep: "held", wantTier: TierExisting, wantDups: []string{"luna"},
		},
		{
			name: "another recipe's decision is no claim",
			cands: []db.DecideFinding{
				mk("held-by-jev", nil), // decided by an older recipe: DecidedHere false
				mk("luna", func(f *db.DecideFinding) { f.CurrentPropose = true }),
			},
			wantRep: "luna", wantTier: TierCurrentRecipe, wantDups: []string{"held-by-jev"},
		},
		{
			name: "a member rung 0 can vet beats one labelled with a type it cannot",
			cands: []db.DecideFinding{
				mk("luna-other", func(f *db.DecideFinding) { f.IssueType = IssueOther; f.CurrentPropose = true }),
				mk("gemma", nil),
			},
			wantRep: "gemma", wantTier: TierRung0, wantDups: []string{"luna-other"},
		},
		{
			name: "a human-decided candidate is never grouped",
			cands: []db.DecideFinding{
				mk("human", func(f *db.DecideFinding) { f.PatchState, f.DecidedBy = patch.StateRejected, "mcp:alice" }),
				mk("p", nil),
			},
		},
		{
			name: "a different correction is not the same fix",
			cands: []db.DecideFinding{
				mk("a", nil),
				mk("b", func(f *db.DecideFinding) {
					f.Replacement = "Arrecibo"
					f.CorrectionKey = testKey(text, "auto sebo", "Arrecibo")
				}),
			},
		},
		{
			name: "no correction key, no grouping",
			cands: []db.DecideFinding{
				mk("a", func(f *db.DecideFinding) { f.CorrectionKey = "" }),
				mk("b", func(f *db.DecideFinding) { f.CorrectionKey = "" }),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verdicts := map[string]Verdict{}
			chunk := &db.DecideChunk{Text: text}
			for _, f := range tc.cands {
				verdicts[f.ID] = Check(candidateOf(f, chunk), DefaultParams())
			}
			dupOf, groups := groupChunk(text, tc.cands, verdicts, tc.overlay)
			if tc.wantRep == "" {
				if len(groups) != 0 || len(dupOf) != 0 {
					t.Fatalf("grouped: %+v %v", groups, dupOf)
				}
				return
			}
			if len(groups) != 1 {
				t.Fatalf("groups %+v", groups)
			}
			g := groups[0]
			if g.Rep != tc.wantRep || g.Tier != tc.wantTier || !slices.Equal(g.Members, tc.wantDups) {
				t.Errorf("group %+v, want rep %s (%s) with %v", g, tc.wantRep, tc.wantTier, tc.wantDups)
			}
			for _, d := range tc.wantDups {
				if dupOf[d] != tc.wantRep {
					t.Errorf("dupOf[%s] = %q", d, dupOf[d])
				}
			}
			if _, self := dupOf[tc.wantRep]; self {
				t.Error("the representative is a duplicate")
			}
		})
	}
}

// A conflict is ranked by the decide model's p, not judge confidence.
func TestRankConflicts(t *testing.T) {
	const text = "the holovo bound and the cat"
	p := func(v float64) *float64 { return &v }
	span := func(orig string) patch.Span {
		o := patch.Occurrences(text, orig)[0]
		return patch.Span{Start: o, End: o + len([]rune(orig))}
	}
	r := func(id, orig string, prob *float64, conf float64) Ranked {
		return Ranked{FindingID: id, ChunkText: text, Span: span(orig), P: prob, Confidence: conf}
	}
	t.Run("higher confidence, lower p loses", func(t *testing.T) {
		losers, ok := RankConflicts([]Ranked{r("confident", "holovo bound", p(0.40), 0.99), r("likely", "holovo", p(0.90), 0.50)})
		if !ok || losers["confident"] != "likely" || len(losers) != 1 {
			t.Errorf("losers %v ok %v", losers, ok)
		}
	})
	t.Run("equal p falls back to confidence, then id", func(t *testing.T) {
		losers, _ := RankConflicts([]Ranked{r("a", "holovo", p(0.5), 0.6), r("b", "holovo bound", p(0.5), 0.9)})
		if losers["a"] != "b" {
			t.Errorf("losers %v", losers)
		}
		losers, _ = RankConflicts([]Ranked{r("b", "holovo", p(0.5), 0.6), r("a", "holovo bound", p(0.5), 0.6)})
		if losers["b"] != "a" {
			t.Errorf("losers %v", losers)
		}
	})
	t.Run("chain: the far end survives its neighbour's loss", func(t *testing.T) {
		losers, _ := RankConflicts([]Ranked{
			r("a", "the holovo", p(0.9), 0.5), r("b", "holovo bound", p(0.8), 0.9), r("c", "bound and", p(0.7), 0.9),
		})
		if len(losers) != 1 || losers["b"] != "a" {
			t.Errorf("losers %v", losers)
		}
	})
	t.Run("a member without p leaves the set unranked", func(t *testing.T) {
		if losers, ok := RankConflicts([]Ranked{r("a", "holovo", p(0.9), 0.5), r("b", "holovo bound", nil, 0.9)}); ok || losers != nil {
			t.Errorf("losers %v ok %v", losers, ok)
		}
	})
}

// An unranked conflict never applies: a member's outage turns the other's
// apply into a retryable hold.
func TestResolveConflictsUnranked(t *testing.T) {
	const text = "the holovo bound and the cat"
	p := 0.97
	mk := func(id, orig string, out Outcome, conflicts ...string) *item {
		o := patch.Occurrences(text, orig)[0]
		out.FindingID = id
		out.Rung0 = Verdict{Pass: true, Span: patch.Span{Start: o, End: o + len(orig)}, Conflicts: conflicts}
		return &item{finding: db.DecideFinding{ID: id, Original: orig},
			input: Input{Candidate: Candidate{FindingID: id, ChunkText: text}}, outcome: out}
	}
	a := mk("a", "holovo", Outcome{Decision: DecisionApply, Reason: ReasonConfident, P: &p, Asked: true}, "b")
	b := mk("b", "holovo bound", Outcome{Decision: DecisionHold, Reason: ReasonJevUnavailable, Asked: true, ErrorClass: "503"}, "a")
	var g Grouping
	resolveConflicts([]*item{a, b}, &g, true)
	if a.outcome.Decision != DecisionHold || a.outcome.Reason != ReasonJevUnavailable || !a.outcome.Retryable {
		t.Errorf("a %+v: an unranked conflict must not apply", a.outcome)
	}
	if g.Conflicts != 1 || g.Unranked != 1 || g.Losers != 0 || len(g.ConflictLines) != 1 || !strings.Contains(g.ConflictLines[0], "unranked") {
		t.Errorf("grouping %+v", g)
	}
}

// groupFixture is a backlog where two recipes proposed the same fix: Luna
// (the current propose recipe, newer) and an older held gemma finding — plus
// an unsampled competitor proposing the same fix too.
func groupFixture() *fakeRunStore {
	path := "/b/PHM/01.m4b"
	luna := keyed("g-luna", runT1, path, IssueMisheardProperNoun, runText, "auto sebo", "Arecibo", 0.6, t0.Add(time.Hour))
	luna.CurrentPropose = true
	gemma := keyed("g-gemma", runT1, path, IssueMisheardProperNoun, runText, "auto sebo", "Arecibo", 0.9, t0)
	comp := keyed("g-comp", runT1, path, IssueMisheardProperNoun, runText, "auto sebo", "arecibo", 0.7, t0.Add(time.Minute))
	repeat := keyed("g-repeat", runT1, path, IssueRepeatedText, runText, "the the cat", "the cat", 0.8, t0)
	fx := runFixture()
	fx.sample = []db.DecideFinding{gemma, luna, repeat}
	fx.chunks[db.ChunkKey{TranscriptID: runT1, ChunkIndex: 0}].Competitors = []db.DecideFinding{comp}
	return fx
}

// Two recipes, same fix: one representative is decided and applied, the
// others are duplicates — never rejections — and the dry run lists the group.
func TestDryRunGroupsAgreeingFindings(t *testing.T) {
	store, asker := groupFixture(), runAsker()
	var dump bytes.Buffer
	rep, err := DryRun(context.Background(), store, asker, RunOptions{
		Scope: db.DecideScope{Sample: 50, Seed: "s"}, Concurrency: 2, Params: DefaultShouldApplyParams(), Dump: &dump,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcomes[DecisionApply] != 2 || rep.Outcomes[ClassDuplicate] != 1 || rep.Outcomes[DecisionReject] != 0 {
		t.Errorf("outcomes %v: want the representative and the repeat applied, gemma a duplicate", rep.Outcomes)
	}
	g := rep.Grouping
	if g.Groups != 1 || g.Duplicates != 2 || len(g.GroupLines) != 1 ||
		!strings.Contains(g.GroupLines[0], `g-luna [proposed · current propose recipe] "auto sebo" → "Arecibo" · duplicates: g-comp, g-gemma`) {
		t.Errorf("grouping %+v", g)
	}
	if asker.calls != 2 {
		t.Errorf("%d model calls: only the representative and the repeat are asked", asker.calls)
	}
	byID := map[string]DumpRecord{}
	for _, r := range readDump(t, dump.Bytes()) {
		byID[r.FindingID] = r
	}
	if r := byID["g-gemma"]; r.Class != ClassDuplicate || r.Outcome != DecisionDuplicate || r.DuplicateOf != "g-luna" || r.P != nil {
		t.Errorf("g-gemma %+v", r)
	}
	if _, ok := byID["g-comp"]; ok {
		t.Error("the unsampled duplicate was dumped")
	}
	var text bytes.Buffer
	if err := rep.Print(&text, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "duplicates grouped: 1 group(s) · 2 duplicate(s)") || !strings.Contains(text.String(), "  g-luna [proposed") {
		t.Errorf("text report:\n%s", text.String())
	}
}

// --yes writes the representative's decision and one duplicate decision per
// member — the competitor outside the page included — naming the
// representative; none is a rejection.
func TestApplyWritesDuplicates(t *testing.T) {
	rs := groupFixture()
	w := slices.Clone(rs.sample)
	slices.SortFunc(w, func(a, b db.DecideFinding) int { return strings.Compare(a.ID, b.ID) })
	store := &fakeWriteStore{fakeRunStore: rs, work: w, skip: map[string]bool{}}
	rep, err := Apply(context.Background(), store, runAsker(), applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	evs := eventsByID(store.fresh)
	if e := evs["g-luna"]; e.Outcome != db.OutcomeApply || e.RelatedFindingID != "" {
		t.Errorf("representative %+v", e)
	}
	for _, id := range []string{"g-gemma", "g-comp"} {
		e := evs[id]
		if e.Outcome != db.OutcomeDuplicate || e.Reason != ReasonDuplicate || e.RelatedFindingID != "g-luna" ||
			e.P != nil || e.FnCallID != nil || e.Evidence != "" || e.ChunkTextSHA256 != patch.ChunkHash(runText) {
			t.Errorf("%s event %+v", id, e)
		}
	}
	if ws := rep.Write; ws.Duplicates != 2 || ws.Rejected != 0 || ws.Accepted != 2 {
		t.Errorf("write %+v", ws)
	}
	if rep.Grouping.Groups != 1 || rep.Grouping.Duplicates != 2 || len(rep.Grouping.GroupLines) != 0 {
		t.Errorf("grouping %+v (a --yes run prints no group lines)", rep.Grouping)
	}
}

// Conflicting fixes on the same span are each asked; the higher p wins even
// against a higher judge confidence, and the loser's decision names it.
func TestApplyRanksConflictsByP(t *testing.T) {
	store, asker := newWriteStore(), runAsker()
	// Make the competitor part of the work, so its verdict is written.
	store.work = append(store.work, store.chunks[db.ChunkKey{TranscriptID: runT2, ChunkIndex: 0}].Competitors...)
	slices.SortFunc(store.work, func(a, b db.DecideFinding) int { return strings.Compare(a.ID, b.ID) })
	rep, err := Apply(context.Background(), store, asker, applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	evs := eventsByID(store.fresh)
	// The winner (f-dup, p 0.6) is only held, so the loser (judge confidence
	// 0.95, p 0.3) is held too, naming it — not rejected.
	loser := evs["c-overlap"]
	if loser.Outcome != db.OutcomeHold || loser.Reason != ReasonOverlapPending || loser.RelatedFindingID != "f-dup" ||
		loser.P == nil || *loser.P != 0.3 || loser.FnCallID == nil {
		t.Errorf("loser %+v", loser)
	}
	if w := evs["f-dup"]; w.Outcome != db.OutcomeHold || w.Reason != ReasonUncertain || w.RelatedFindingID != "" {
		t.Errorf("winner %+v", w)
	}
	if g := rep.Grouping; g.Conflicts != 1 || g.Losers != 0 || g.Pending != 1 || rep.ConflictAsks != 0 {
		t.Errorf("grouping %+v conflict asks %d", g, rep.ConflictAsks)
	}
}

// conflictStore is a work list of two different fixes to "auto sebo" (the
// book record spells Arecibo), with p per correction.
func conflictStore(pArecibo, pArrecibo float64) (*fakeWriteStore, *fakeAsker) {
	path := "/b/PHM/01.m4b"
	right := keyed("x-right", runT1, path, IssueMisheardProperNoun, runText, "auto sebo", "Arecibo", 0.5, t0)
	wrong := keyed("x-wrong", runT1, path, IssueMisheardProperNoun, runText, "auto sebo", "Arrecibo", 0.9, t0)
	rs := runFixture()
	rs.sample = []db.DecideFinding{right, wrong}
	store := &fakeWriteStore{fakeRunStore: rs, work: []db.DecideFinding{right, wrong}, skip: map[string]bool{}}
	asker := &fakeAsker{p: func(state string) (float64, error) {
		if strings.Contains(state, "[[Arrecibo]]") {
			return pArrecibo, nil
		}
		return pArecibo, nil
	}}
	return store, asker
}

// The winner applies: the loser is rejected overlap_dup, naming it.
func TestApplyConflictWinnerApplies(t *testing.T) {
	store, asker := conflictStore(0.97, 0.40)
	if _, err := Apply(context.Background(), store, asker, applyOpts()); err != nil {
		t.Fatal(err)
	}
	evs := eventsByID(store.fresh)
	if w := evs["x-right"]; w.Outcome != db.OutcomeApply {
		t.Errorf("winner %+v", w)
	}
	if l := evs["x-wrong"]; l.Outcome != db.OutcomeReject || l.Reason != ReasonOverlapDup || l.RelatedFindingID != "x-right" {
		t.Errorf("loser %+v", l)
	}
}

// The winner has the higher p but is held (no text evidence for its
// spelling); the loser would have applied on its own. Nothing is applied, and
// the loser is held pending the winner — never rejected for a fix that is not
// in.
func TestApplyConflictWinnerHeldHoldsLoser(t *testing.T) {
	store, asker := conflictStore(0.95, 0.99)
	rep, err := Apply(context.Background(), store, asker, applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	evs := eventsByID(store.fresh)
	if w := evs["x-wrong"]; w.Outcome != db.OutcomeHold || w.Reason != ReasonNoEvidence {
		t.Errorf("winner %+v", w)
	}
	if l := evs["x-right"]; l.Outcome != db.OutcomeHold || l.Reason != ReasonOverlapPending || l.RelatedFindingID != "x-wrong" ||
		l.P == nil || *l.P != 0.95 {
		t.Errorf("loser %+v: it would apply alone, so it must wait, not be rejected", l)
	}
	if rep.Write.Accepted != 0 || rep.Write.Rejected != 0 || rep.Grouping.Pending != 1 {
		t.Errorf("write %+v grouping %+v", rep.Write, rep.Grouping)
	}
}

// The model rejects the representative: its duplicates are still duplicates
// of it — agreement, not a reject of their own.
func TestApplyRepresentativeRejected(t *testing.T) {
	rs := groupFixture()
	w := slices.Clone(rs.sample)
	slices.SortFunc(w, func(a, b db.DecideFinding) int { return strings.Compare(a.ID, b.ID) })
	store := &fakeWriteStore{fakeRunStore: rs, work: w, skip: map[string]bool{}}
	asker := &fakeAsker{p: func(string) (float64, error) { return 0.05, nil }}
	if _, err := Apply(context.Background(), store, asker, applyOpts()); err != nil {
		t.Fatal(err)
	}
	evs := eventsByID(store.fresh)
	if e := evs["g-luna"]; e.Outcome != db.OutcomeReject || e.Reason != ReasonJevReject {
		t.Errorf("representative %+v", e)
	}
	for _, id := range []string{"g-gemma", "g-comp"} {
		if e := evs[id]; e.Outcome != db.OutcomeDuplicate || e.RelatedFindingID != "g-luna" {
			t.Errorf("%s %+v", id, e)
		}
	}
}

// Paged one finding at a time, a representative on a later page still gets
// the duplicates of every page, each written once.
func TestApplyGroupsAcrossPages(t *testing.T) {
	defer func(n int) { workPage = n }(workPage)
	workPage = 1
	rs := groupFixture()
	// As in the database, a chunk's competitors are all its proposed
	// findings, the run's own included.
	k := db.ChunkKey{TranscriptID: runT1, ChunkIndex: 0}
	rs.chunks[k].Competitors = append(slices.Clone(rs.sample), rs.chunks[k].Competitors...)
	w := slices.Clone(rs.sample)
	slices.SortFunc(w, func(a, b db.DecideFinding) int { return strings.Compare(a.ID, b.ID) })
	store := &fakeWriteStore{fakeRunStore: rs, work: w, skip: map[string]bool{}}
	// The out-of-run competitor (g-comp) is written as a duplicate with the
	// first page that groups it; mark it gone from the chunk afterwards, as
	// the database would (it is no longer proposed).
	writes := map[string]int{}
	rep, err := Apply(context.Background(), &countingStore{store, writes}, runAsker(), applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	for id, n := range writes {
		if n != 1 {
			t.Errorf("%s written %d times", id, n)
		}
	}
	evs := eventsByID(store.fresh)
	for _, id := range []string{"g-gemma", "g-comp"} {
		if e := evs[id]; e.Outcome != db.OutcomeDuplicate || e.RelatedFindingID != "g-luna" {
			t.Errorf("%s %+v", id, e)
		}
	}
	if evs["g-luna"].Outcome != db.OutcomeApply || rep.Write.Duplicates != 2 || len(store.scopes) < 3 {
		t.Errorf("rep %+v write %+v pages %d", evs["g-luna"], rep.Write, len(store.scopes))
	}
}

// countingStore counts writes per finding and, like the database, drops a
// finding written as a duplicate from its chunk's proposed competitors.
type countingStore struct {
	*fakeWriteStore
	writes map[string]int
}

func (s *countingStore) ApplyDecisions(ctx context.Context, recipe string, evs []db.DecisionEvent) (db.ApplyResult, error) {
	res, err := s.fakeWriteStore.ApplyDecisions(ctx, recipe, evs)
	for _, e := range evs {
		s.writes[e.FindingID]++
		if e.Outcome != db.OutcomeDuplicate {
			continue
		}
		for _, c := range s.chunks {
			c.Competitors = slices.DeleteFunc(c.Competitors, func(f db.DecideFinding) bool { return f.ID == e.FindingID })
		}
		s.work = slices.DeleteFunc(s.work, func(f db.DecideFinding) bool { return f.ID == e.FindingID })
	}
	return res, err
}
