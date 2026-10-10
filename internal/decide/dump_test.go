package decide

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// noWriteStore is a runFixture that fails the test on any fn.Store call:
// a --rung0-only replay must neither register a recipe nor touch fn_calls.
type noWriteStore struct {
	*fakeRunStore
	t *testing.T
}

func (s noWriteStore) RegisterRecipe(context.Context, recipe.Recipe) (string, error) {
	s.t.Error("rung-0 replay registered a recipe")
	return "", nil
}

func (s noWriteStore) LookupFnCache(context.Context, db.FnCacheKey, string) (*db.FnCall, bool, error) {
	s.t.Error("rung-0 replay read the fn_calls cache")
	return nil, false, nil
}

func (s noWriteStore) InsertFnCall(context.Context, db.FnCall) (int64, bool, error) {
	s.t.Error("rung-0 replay wrote fn_calls")
	return 0, false, nil
}

func readDump(t *testing.T, b []byte) []DumpRecord {
	t.Helper()
	var out []DumpRecord
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		dec := json.NewDecoder(strings.NewReader(sc.Text()))
		dec.DisallowUnknownFields()
		var r DumpRecord
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("dump line %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

func TestDryRunRung0Only(t *testing.T) {
	fx := runFixture()
	fx.sample[0].JudgeModel = "qwen3.8"
	var dump bytes.Buffer
	// A nil asker: a rung-0 replay must not need one.
	rep, err := DryRun(context.Background(), noWriteStore{fx, t}, nil, RunOptions{
		Scope: db.DecideScope{Sample: 50, Seed: "s"}, Concurrency: 2, Params: DefaultShouldApplyParams(),
		Rung0Only: true, Dump: &dump,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{ClassRung0Pass: 5, DecisionReject: 2, ClassReanchor: 1}
	for k, v := range want {
		if rep.Outcomes[k] != v {
			t.Errorf("outcome %s = %d, want %d (all %v)", k, rep.Outcomes[k], v, rep.Outcomes)
		}
	}
	if !rep.Rung0Only || rep.Jev.Asked != 0 || len(rep.Reasons) != 0 || rep.Projection.Asked != 0 {
		t.Errorf("rung-0 report asked the model: %+v", rep)
	}
	if rep.Rung0Rejects[ReasonNotSoundAlike] != 1 || rep.Rung0Rejects[ReasonOverlapDup] != 1 || rep.Reanchor[ReasonChunkChanged] != 1 {
		t.Errorf("rung-0 %v reanchor %v", rep.Rung0Rejects, rep.Reanchor)
	}
	wantID, err := mustFn(t).Recipe("").ID()
	if err != nil || rep.RecipeID != wantID {
		t.Errorf("recipe id %q, want %q (%v)", rep.RecipeID, wantID, err)
	}

	recs := readDump(t, dump.Bytes())
	if len(recs) != len(fx.sample) {
		t.Fatalf("%d dump lines, want %d", len(recs), len(fx.sample))
	}
	if !slices.IsSortedFunc(recs, func(a, b DumpRecord) int { return strings.Compare(a.FindingID, b.FindingID) }) {
		t.Error("dump not sorted by finding id")
	}
	byID := map[string]DumpRecord{}
	for _, r := range recs {
		byID[r.FindingID] = r
		if r.Outcome != "" || r.P != nil {
			t.Errorf("%s: rung-0 replay dumped a decision %+v", r.FindingID, r)
		}
	}
	if r := byID["f-apply"]; !r.Rung0.Pass || r.Class != ClassRung0Pass || r.Evidence != EvidenceASINVerbatim ||
		r.JudgeModel != "qwen3.8" || r.Original != "auto sebo" || r.Replacement != "Arecibo" || r.Rung0.Evidence == "" {
		t.Errorf("f-apply %+v", r)
	}
	if r := byID["f-notsound"]; r.Rung0.Pass || r.Rung0.Reason != ReasonNotSoundAlike || r.Class != DecisionReject || r.Evidence != "" {
		t.Errorf("f-notsound %+v", r)
	}
	if r := byID["f-stale"]; r.Class != ClassReanchor {
		t.Errorf("f-stale %+v", r)
	}

	var text bytes.Buffer
	if err := rep.Print(&text, false); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"rung 0 only", "rung0_pass 5", "no model calls, no recipe registered, no decisions written (startup migrations still run)"} {
		if !strings.Contains(text.String(), s) {
			t.Errorf("text report lacks %q:\n%s", s, text.String())
		}
	}
	if strings.Contains(text.String(), "p histogram") || strings.Contains(text.String(), "model calls ·") {
		t.Errorf("rung-0 report prints model sections:\n%s", text.String())
	}
}

// The dump of a full dry run carries each asked finding's decision.
func TestDryRunDump(t *testing.T) {
	var dump bytes.Buffer
	_, err := DryRun(context.Background(), runFixture(), runAsker(), RunOptions{
		Scope: db.DecideScope{Sample: 50, Seed: "s"}, Concurrency: 2, Params: DefaultShouldApplyParams(), Dump: &dump,
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]DumpRecord{}
	for _, r := range readDump(t, dump.Bytes()) {
		byID[r.FindingID] = r
	}
	if r := byID["f-apply"]; r.Outcome != DecisionApply || r.Reason != ReasonConfident || r.P == nil || *r.P != 0.97 || r.Class != DecisionApply {
		t.Errorf("f-apply %+v", r)
	}
	if r := byID["f-outage"]; r.Outcome != DecisionHold || r.Reason != ReasonJevUnavailable || r.P != nil {
		t.Errorf("f-outage %+v", r)
	}
	if r := byID["f-dup"]; r.Outcome != "" || r.Rung0.Reason != ReasonOverlapDup {
		t.Errorf("f-dup %+v", r)
	}
}

// A rung-0-only calibration still splits rung-0 passes by human label.
func TestDryRunRung0OnlyCalibrate(t *testing.T) {
	fx := runFixture()
	for i := range fx.sample {
		fx.sample[i].PatchState = "accepted"
	}
	rep, err := DryRun(context.Background(), noWriteStore{fx, t}, nil, RunOptions{
		Scope: db.DecideScope{Sample: 50, Seed: "s", Calibrate: true}, Concurrency: 1, Params: DefaultShouldApplyParams(), Rung0Only: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := rep.Calibration; c == nil || c.Matrix[HumanAccepted][ClassRung0Pass] != 5 || c.Matrix[HumanAccepted][DecisionReject] != 2 {
		t.Fatalf("calibration %+v", rep.Calibration)
	}
	var text bytes.Buffer
	if err := rep.Print(&text, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "human accepted → rung0_pass 5") || strings.Contains(text.String(), "apply precision") {
		t.Errorf("text:\n%s", text.String())
	}
}

func mustFn(t *testing.T) interface{ Recipe(string) recipe.Recipe } {
	t.Helper()
	f, err := ShouldApplyFn(DefaultShouldApplyParams())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// letter_swap and too_many_changes reach the report's rung-0 rejects and
// the dump like any other rung-0 reason.
func TestDryRunRung0OnlyNewReasons(t *testing.T) {
	const tid, text = "00000000-0000-0000-0000-0000000000a3",
		"take vitamin c and their a their b their c their d their e their f their g their h their i daily"
	const orig = "their a their b their c their d their e their f their g their h their i"
	path := "/b/Other/01.m4b"
	fx := runFixture()
	fx.sample = []db.DecideFinding{
		finding("f-letter", tid, path, IssueMisheardWord, text, "vitamin c", "vitamin k", 0, 0.9),
		finding("f-rewrite", tid, path, IssueHomophone, text, orig, strings.ReplaceAll(orig, "their", "there"), 0, 0.9),
	}
	fx.chunks[db.ChunkKey{TranscriptID: tid, ChunkIndex: 0}] = &db.DecideChunk{
		Key: db.ChunkKey{TranscriptID: tid, ChunkIndex: 0}, Text: text, StartSec: 0, EndSec: 9}
	var dump bytes.Buffer
	rep, err := DryRun(context.Background(), noWriteStore{fx, t}, nil, RunOptions{
		Scope: db.DecideScope{Sample: 50, Seed: "s"}, Concurrency: 1, Params: DefaultShouldApplyParams(),
		Rung0Only: true, Dump: &dump,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rung0Rejects[ReasonLetterSwap] != 1 || rep.Rung0Rejects[ReasonTooManyChanges] != 1 {
		t.Errorf("rung-0 rejects %v, want one letter_swap and one too_many_changes", rep.Rung0Rejects)
	}
	got := map[string]string{}
	for _, r := range readDump(t, dump.Bytes()) {
		got[r.FindingID] = r.Rung0.Reason
	}
	if got["f-letter"] != ReasonLetterSwap || got["f-rewrite"] != ReasonTooManyChanges {
		t.Errorf("dump reasons %v", got)
	}
	var out bytes.Buffer
	if err := rep.Print(&out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "letter_swap 1") || !strings.Contains(out.String(), "too_many_changes 1") {
		t.Errorf("text report lacks the new reasons:\n%s", out.String())
	}
}
