package asrpilot

import (
	"math"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
)

func TestWordErrors(t *testing.T) {
	tests := []struct {
		ref, hyp []string
		want     int
	}{
		{nil, nil, 0},
		{[]string{"a", "b", "c"}, []string{"a", "b", "c"}, 0},
		{[]string{"a", "b", "c"}, []string{"a", "x", "c"}, 1},
		{[]string{"a", "b", "c"}, []string{"a", "c"}, 1},
		{[]string{"a", "c"}, []string{"a", "b", "c", "d"}, 2},
		{[]string{"a", "b"}, nil, 2},
	}
	for _, tt := range tests {
		if got := WordErrors(tt.ref, tt.hyp); got != tt.want {
			t.Errorf("WordErrors(%v, %v) = %d, want %d", tt.ref, tt.hyp, got, tt.want)
		}
	}
}

func TestCompareFullAndPartial(t *testing.T) {
	prod := ProductionTokens([]db.Segment{
		prodSeg(0, 0, "one two three four"),      // 0..2 s
		prodSeg(1, 10, "five six seven eight"),   // 10..12 s
		prodSeg(2, 20, "nine ten eleven twelve"), // 20..22 s
	})
	t.Run("full scope compares the whole track", func(t *testing.T) {
		hyp := PilotTokens([]db.PilotSegment{
			timedPilot(0, "One two three four."), timedPilot(10, "Five six heaven eight."), timedPilot(20, "Nine ten eleven twelve."),
		})
		c := Compare(prod, hyp, FullCoverage())
		if c.Errors != 1 || c.RefWords != 12 || math.Abs(c.WER()-1.0/12) > 1e-9 {
			t.Errorf("full = %+v WER %v", c, c.WER())
		}
	})
	t.Run("findings scope compares only the covered windows", func(t *testing.T) {
		// Only the middle segment was transcribed: the other 8 production
		// words are outside the coverage and must not count as deletions.
		segs := []db.PilotSegment{timedPilot(10, "Five six heaven eight.")}
		cov := CoverageFor(db.PilotScopeFindings, segs)
		c := Compare(prod, PilotTokens(segs), cov)
		if c.Errors != 1 || c.RefWords != 4 || c.WER() != 0.25 {
			t.Errorf("partial = %+v", c)
		}
		if cov.Full || cov.Seconds() != 2 {
			t.Errorf("coverage = %+v (%v s)", cov, cov.Seconds())
		}
	})
}

func TestCoverage(t *testing.T) {
	c := SegmentCoverage([]db.PilotSegment{{Start: 5, End: 8}, {Start: 0, End: 2}, {Start: 7, End: 10}, {Start: 3, End: 3}})
	want := []Interval{{0, 2}, {5, 10}}
	if len(c.Intervals) != 2 || c.Intervals[0] != want[0] || c.Intervals[1] != want[1] {
		t.Fatalf("merged = %v, want %v", c.Intervals, want)
	}
	got := Intersect(c, Coverage{Intervals: []Interval{{1, 6}, {9, 20}}})
	wantI := []Interval{{1, 2}, {5, 6}, {9, 10}}
	if len(got.Intervals) != 3 {
		t.Fatalf("intersect = %v, want %v", got.Intervals, wantI)
	}
	for i := range wantI {
		if got.Intervals[i] != wantI[i] {
			t.Errorf("intersect[%d] = %v, want %v", i, got.Intervals[i], wantI[i])
		}
	}
	if x := Intersect(FullCoverage(), c); x.Full || len(x.Intervals) != 2 {
		t.Errorf("full ∩ c = %+v", x)
	}
	if !Intersect(FullCoverage(), FullCoverage()).Full {
		t.Error("full ∩ full must be full")
	}
	if !CoverageFor(db.PilotScopeFull, nil).Full {
		t.Error("scope full is full coverage")
	}
}

func TestTextStats(t *testing.T) {
	var s TextStats
	s.Add(PilotTokens([]db.PilotSegment{timedPilot(0, "Dorian Gray, well-known here")}))
	// 4 raw words: "Dorian" "Gray," "well-known" "here"; 2 cased, 2 punctuated.
	if s.Words != 4 || s.Cased != 2 || s.Punctuated != 2 || s.CasingRate() != 0.5 || !s.HasCasing() {
		t.Errorf("stats = %+v", s)
	}
	var lower TextStats
	lower.Add(ProductionTokens([]db.Segment{prodSeg(0, 0, "dorian gray well known here")}))
	if lower.HasCasing() || lower.PunctuationRate() != 0 {
		t.Errorf("lowercase stats = %+v", lower)
	}
}

func TestAgreement(t *testing.T) {
	cells := []VoteCell{
		{"canary", "granite", VoteHeardReplacement, VoteHeardReplacement, 5},
		{"canary", "granite", VoteHeardReplacement, VoteHeardOriginal, 1},
		{"canary", "granite", VoteNeither, VoteNeither, 2},
		{"canary", "granite", VoteNeither, VoteHeardOriginal, 1},
		{"canary", "granite", VoteUnmappable, VoteHeardOriginal, 3},
	}
	got := Agreement(cells)
	if len(got) != 1 {
		t.Fatalf("pairs = %d", len(got))
	}
	p := got[0]
	if p.Both != 12 || p.Mapped != 9 || p.Agree != 7 || p.Resolved != 6 || p.ResolvedAgree != 5 {
		t.Errorf("agreement = %+v", p)
	}
	if math.Abs(p.AgreementRate()-7.0/9) > 1e-9 || math.Abs(p.ResolvedAgreementRate()-5.0/6) > 1e-9 {
		t.Errorf("rates %v %v", p.AgreementRate(), p.ResolvedAgreementRate())
	}
}

func TestVoteTally(t *testing.T) {
	var v VoteTally
	v.Add(VoteHeardReplacement, 3)
	v.Add(VoteHeardOriginal, 2)
	v.Add(VoteNeither, 4)
	v.Add(VoteUnmappable, 1)
	v.Add("bogus", 9)
	if v.Voted() != 10 || v.Resolved() != 5 {
		t.Errorf("tally = %+v", v)
	}
}

func TestModelStatsRTFxAndVRAM(t *testing.T) {
	f := func(x float64) *float64 { return &x }
	i := func(x int64) *int64 { return &x }
	var m ModelStats
	m.AddTrack(db.PilotTranscript{AudioSec: f(3600), WallSec: f(60), PeakVRAMMiB: i(9000), PeakReservedMiB: i(12000)})
	m.AddTrack(db.PilotTranscript{AudioSec: f(1800), WallSec: f(40), PeakVRAMMiB: i(9500), PeakReservedMiB: i(11000)})
	m.AddTrack(db.PilotTranscript{AudioSec: f(600)}) // no wall time: audio counts, RTFx does not
	if m.Tracks != 3 || m.AudioSec != 6000 || m.RTFx() != 54 || m.PeakVRAMMiB != 9500 || m.PeakReservedMiB != 12000 {
		t.Errorf("stats = %+v rtfx %v", m, m.RTFx())
	}
	if (ModelStats{}).RTFx() != 0 {
		t.Error("RTFx with no wall time is 0")
	}
}
