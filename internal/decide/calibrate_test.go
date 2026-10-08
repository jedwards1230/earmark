package decide

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// calItem builds a calibration item the way DryRun does: rung 0 on its own
// chunk, evidence from rec, and Decide on p (nil = the model gave no answer).
func calItem(t *testing.T, id, state, issue, text, original, replacement string, p *float64, rec *db.BookRecord) *item {
	t.Helper()
	f := finding(id, "t-"+id, "/b/x/1.m4b", issue, text, original, replacement, 0, 0.9)
	f.PatchState = state
	chunk := &db.DecideChunk{Key: db.ChunkKey{TranscriptID: f.TranscriptID}, Text: text}
	params := DefaultShouldApplyParams()
	v := chunkVerdicts([]db.DecideFinding{f}, chunk, params.PhoneticMinSim)[id]
	it := &item{finding: f, chunk: chunk, sentences: RecordSentences(rec)}
	it.input = Input{Candidate: candidateOf(f, chunk), Verdict: &v, Record: rec}
	o := Outcome{FindingID: id, Rung0: v, Evidence: EvidenceNone}
	switch {
	case !v.Pass:
		o.Decision, o.Reason = DecisionReject, v.Reason
	case p == nil:
		o.Decision, o.Reason = DecisionHold, ReasonJevUnavailable
	default:
		o.Evidence = TextEvidence(it.input.Candidate, v, it.sentences)
		o.P = p
		o.Decision, o.Reason = params.Decide(*p, o.Evidence, issue)
	}
	it.outcome = o
	return it
}

func TestCalibrate(t *testing.T) {
	p := func(f float64) *float64 { return &f }
	const radio = "the dish at auto sebo picked up the signal"
	const walk = "he walked to the store with dollars"
	items := []*item{
		calItem(t, "a1", patch.StateApplied, IssueMisheardProperNoun, radio, "auto sebo", "Arecibo", p(0.97), hailMary()),
		calItem(t, "a2", patch.StateAccepted, IssueMisheardWord, walk, "walked", "waltzed", nil, nil),
		calItem(t, "r1", patch.StateRejected, IssueMisheardWord, walk, "dollars", "dolors", p(0.99), nil),
		calItem(t, "r2", patch.StateRejected, IssueMisheardWord, walk, "store", "stoor", p(0.05), nil),
		calItem(t, "r3", patch.StateRejected, IssueMisheardProperNoun, radio, "auto sebo", "Arecibo", p(0.99), hailMary()),
		calItem(t, "a3", patch.StateAccepted, IssueMisheardWord, walk, "absent", "present", p(0.99), nil),
	}
	c := calibrate(items, DefaultShouldApplyParams(), []float64{0.55, 0.67})

	if c.HumanAccepted != 3 || c.HumanRejected != 3 {
		t.Errorf("labels %d/%d", c.HumanAccepted, c.HumanRejected)
	}
	if m := c.Matrix; m[HumanAccepted][DecisionApply] != 1 || m[HumanAccepted][DecisionReject] != 1 || m[HumanAccepted][ClassReanchor] != 1 ||
		m[HumanRejected][DecisionApply] != 1 || m[HumanRejected][DecisionHold] != 1 || m[HumanRejected][DecisionReject] != 1 {
		t.Errorf("matrix %v", m)
	}
	check := func(name string, got *float64, want float64) {
		t.Helper()
		if got == nil || *got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("apply precision", c.ApplyPrecision, 0.5)
	check("apply recall", c.ApplyRecall, 1.0/3)
	check("reject precision", c.RejectPrecision, 0.5)
	if c.HoldRate != 1.0/6 {
		t.Errorf("hold rate %v", c.HoldRate)
	}

	want := []SweepRow{
		// 0.55: "waltzed" (0.60) now passes but was never asked.
		{Threshold: 0.55, Rung0PassAccepted: 2, Rung0PassRejected: 3, Unasked: 1, Apply: 2, ApplyAgree: 1, Reject: 1, RejectAgree: 1, Hold: 1},
		// 0.67 reproduces the run: "waltzed" is a rung-0 reject a human accepted.
		{Threshold: 0.67, Rung0PassAccepted: 1, Rung0PassRejected: 3, Apply: 2, ApplyAgree: 1, Reject: 2, RejectAgree: 1, Hold: 1},
	}
	if len(c.Sweep) != len(want) {
		t.Fatalf("sweep %+v", c.Sweep)
	}
	for i, w := range want {
		got := c.Sweep[i]
		check("sweep precision", got.ApplyPrecision, 0.5)
		got.ApplyPrecision = nil
		if got != w {
			t.Errorf("sweep[%d]\n got %+v\nwant %+v", i, got, w)
		}
	}

	rep := buildReport(RunOptions{Scope: db.DecideScope{Sample: 6, Seed: "cal", Calibrate: true}}, strings.Repeat("cd", 32), items,
		map[string]int{IssueMisheardProperNoun: 20, IssueMisheardWord: 40})
	rep.Calibration = c
	var buf bytes.Buffer
	if err := rep.Print(&buf, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "calibration.golden")
	if *update {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if golden, err := os.ReadFile(path); err != nil || buf.String() != string(golden) {
		t.Errorf("calibration report (err %v):\n%s\n--- want ---\n%s", err, buf.String(), golden)
	}

	if r := calibrate(nil, DefaultShouldApplyParams(), nil); r.ApplyPrecision != nil || r.HoldRate != 0 {
		t.Errorf("empty calibration %+v", r)
	}
}
