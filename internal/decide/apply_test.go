package decide

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// fakeWriteStore serves runFixture's findings as the work list (in id order,
// keyset-paged) and records the decisions written.
type fakeWriteStore struct {
	*fakeRunStore
	work    []db.DecideFinding
	scopes  []db.DecideWorkScope
	fresh   [][]db.DecisionEvent
	recheck [][]db.DecisionEvent
	// skip makes ApplyDecisions report these ids skipped.
	skip map[string]bool
}

func newWriteStore() *fakeWriteStore {
	rs := runFixture()
	w := slices.Clone(rs.sample)
	sort.Slice(w, func(i, j int) bool { return w[i].ID < w[j].ID })
	return &fakeWriteStore{fakeRunStore: rs, work: w, skip: map[string]bool{}}
}

func (s *fakeWriteStore) DecideWork(_ context.Context, sc db.DecideWorkScope) ([]db.DecideFinding, error) {
	s.scopes = append(s.scopes, sc)
	var out []db.DecideFinding
	for _, f := range s.work {
		if f.ID > sc.After && len(out) < sc.Limit {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *fakeWriteStore) apply(evs []db.DecisionEvent, recheck bool) db.ApplyResult {
	var r db.ApplyResult
	for _, e := range evs {
		switch {
		case s.skip[e.FindingID]:
			r.Skipped = append(r.Skipped, e.FindingID)
		case recheck && e.Outcome == db.OutcomeReject:
			r.Reverted = append(r.Reverted, e.FindingID)
		case recheck:
			r.Kept = append(r.Kept, e.FindingID)
		case e.Outcome == db.OutcomeApply:
			r.Accepted = append(r.Accepted, e.FindingID)
		case e.Outcome == db.OutcomeReject:
			r.Rejected = append(r.Rejected, e.FindingID)
		default:
			r.Held = append(r.Held, e.FindingID)
		}
	}
	return r
}

func (s *fakeWriteStore) ApplyDecisions(_ context.Context, _ string, evs []db.DecisionEvent) (db.ApplyResult, error) {
	s.fresh = append(s.fresh, evs)
	return s.apply(evs, false), nil
}

func (s *fakeWriteStore) ApplyRecheckDecisions(_ context.Context, _ string, evs []db.DecisionEvent) (db.ApplyResult, error) {
	s.recheck = append(s.recheck, evs)
	return s.apply(evs, true), nil
}

func applyOpts() ApplyOptions {
	return ApplyOptions{Concurrency: 2, Params: DefaultShouldApplyParams()}
}

func eventsByID(batches [][]db.DecisionEvent) map[string]db.DecisionEvent {
	out := map[string]db.DecisionEvent{}
	for _, b := range batches {
		for _, e := range b {
			out[e.FindingID] = e
		}
	}
	return out
}

func TestApply(t *testing.T) {
	store, asker := newWriteStore(), runAsker()
	rep, err := Apply(context.Background(), store, asker, applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	w := rep.Write
	if w == nil || rep.DryRun {
		t.Fatalf("report %+v", rep)
	}
	// f-apply and f-repeat accept; f-notsound, f-dup (rung 0) and f-lowp reject;
	// f-capped and f-outage hold; f-stale needs re-anchoring and is not written.
	if w.Accepted != 2 || w.Rejected != 3 || w.Held != 2 || w.Reanchor != 1 || w.Skipped != 0 || w.Stopped != "" {
		t.Errorf("write stats %+v", w)
	}
	evs := eventsByID(store.fresh)
	if _, ok := evs["f-stale"]; ok {
		t.Error("a re-anchor case was written as a decision")
	}
	if e := evs["f-apply"]; e.Outcome != db.OutcomeApply || e.P == nil || e.FnCallID == nil || e.Evidence != EvidenceASINVerbatim ||
		e.ChunkTextSHA256 != patch.ChunkHash(runText) || e.RecipeID != rep.RecipeID {
		t.Errorf("apply event %+v", e)
	}
	if e := evs["f-notsound"]; e.Outcome != db.OutcomeReject || e.Reason != ReasonNotSoundAlike || e.P != nil || e.FnCallID != nil || e.Evidence != "" {
		t.Errorf("rung-0 reject event %+v", e)
	}
	if e := evs["f-outage"]; e.Outcome != db.OutcomeHold || e.Reason != ReasonJevUnavailable || e.FnCallID == nil {
		t.Errorf("unavailable hold event %+v (its errored fn_calls row is still cited)", e)
	}
	if sc := store.scopes[0]; sc.RecipeID != rep.RecipeID || sc.RetryReason != ReasonJevUnavailable || sc.Limit != workPage {
		t.Errorf("work scope %+v", sc)
	}
	if len(store.scopes) != 2 || store.scopes[1].After != store.work[len(store.work)-1].ID {
		t.Errorf("keyset paging %+v", store.scopes)
	}
}

func TestApplyBatchesAndLimit(t *testing.T) {
	store, asker := newWriteStore(), runAsker()
	o := applyOpts()
	o.Batch, o.Limit = 1, 3
	rep, err := Apply(context.Background(), store, asker, o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Findings != 3 || rep.Write.Stopped != "limit" {
		t.Errorf("findings %d stopped %q", rep.Findings, rep.Write.Stopped)
	}
	for _, b := range store.fresh {
		tids := map[string]bool{}
		for _, e := range b {
			for _, f := range store.work {
				if f.ID == e.FindingID {
					tids[f.TranscriptID] = true
				}
			}
		}
		if len(tids) > 1 {
			t.Errorf("one write spans %d transcripts with --batch 1", len(tids))
		}
	}
}

func TestApplyMaxAccepts(t *testing.T) {
	store, asker := newWriteStore(), runAsker()
	o := applyOpts()
	o.MaxAccepts = 1
	rep, err := Apply(context.Background(), store, asker, o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Write.Accepted != 1 || rep.Write.Capped != 1 || rep.Write.Stopped != "max-accepts" {
		t.Errorf("write %+v", rep.Write)
	}
	applies := 0
	for _, e := range eventsByID(store.fresh) {
		if e.Outcome == db.OutcomeApply {
			applies++
		}
	}
	if applies != 1 {
		t.Errorf("%d apply events written past --max-accepts 1", applies)
	}
}

// A finding another decide recipe accepted goes to the re-check write; a
// hold there is counted for review.
func TestApplyRecheck(t *testing.T) {
	store, asker := newWriteStore(), runAsker()
	for i := range store.work {
		switch store.work[i].ID {
		case "f-apply":
			store.work[i].PatchState, store.work[i].DecidedBy = patch.StateApplied, "jev:"+strings.Repeat("1", 64)
		case "f-outage":
			store.work[i].PatchState, store.work[i].DecidedBy = patch.StateAccepted, "jev:"+strings.Repeat("1", 64)
		}
	}
	rep, err := Apply(context.Background(), store, asker, applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	re := eventsByID(store.recheck)
	if len(re) != 2 || re["f-apply"].Outcome != db.OutcomeApply || re["f-outage"].Outcome != db.OutcomeHold {
		t.Errorf("recheck events %+v", re)
	}
	if _, ok := eventsByID(store.fresh)["f-apply"]; ok {
		t.Error("a re-check went to the proposed write")
	}
	if w := rep.Write; w.Kept != 2 || w.KeptHeld != 1 || w.Accepted != 1 {
		t.Errorf("write %+v", w)
	}
}

func TestApplySkipped(t *testing.T) {
	store, asker := newWriteStore(), runAsker()
	store.skip["f-apply"] = true
	rep, err := Apply(context.Background(), store, asker, applyOpts())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Write.Skipped != 1 || rep.Write.Accepted != 1 {
		t.Errorf("write %+v", rep.Write)
	}
}

func TestApplyOptionsValidated(t *testing.T) {
	store, asker := newWriteStore(), runAsker()
	for _, o := range []ApplyOptions{
		{Concurrency: 0, Params: DefaultShouldApplyParams()},
		{Concurrency: 1, Limit: -1, Params: DefaultShouldApplyParams()},
		{Concurrency: 1, MaxAccepts: -1, Params: DefaultShouldApplyParams()},
	} {
		if _, err := Apply(context.Background(), store, asker, o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
	if asker.calls != 0 || len(store.fresh) != 0 {
		t.Error("an invalid run did work")
	}
}
