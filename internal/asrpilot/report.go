package asrpilot

import (
	"math"
	"sort"

	"github.com/jedwards1230/earmark/internal/db"
)

// ProductionModel labels the reference side of the report's WER: the
// production transcripts (NeMo parakeet-tdt-1.1b). It is a reference, not
// ground truth — the "WER" is word disagreement with the production model.
const ProductionModel = "parakeet-tdt-1.1b"

// Interval is a closed time range in track seconds.
type Interval struct{ Start, End float64 }

// Coverage is the part of a track a transcript covers: the whole track, or
// (scope 'findings') the union of its segment windows.
type Coverage struct {
	Full      bool
	Intervals []Interval // merged, ascending; ignored when Full
}

// FullCoverage covers the whole track.
func FullCoverage() Coverage { return Coverage{Full: true} }

// SegmentCoverage is the union of the segments' [start, end] windows, merged.
func SegmentCoverage(segs []db.PilotSegment) Coverage {
	iv := make([]Interval, 0, len(segs))
	for _, s := range segs {
		if s.End > s.Start {
			iv = append(iv, Interval{s.Start, s.End})
		}
	}
	return Coverage{Intervals: mergeIntervals(iv)}
}

// CoverageFor is the coverage of a pilot transcript of the given scope.
func CoverageFor(scope string, segs []db.PilotSegment) Coverage {
	if scope == db.PilotScopeFindings {
		return SegmentCoverage(segs)
	}
	return FullCoverage()
}

// Seconds is the covered duration (+Inf when Full).
func (c Coverage) Seconds() float64 {
	if c.Full {
		return math.Inf(1)
	}
	var s float64
	for _, iv := range c.Intervals {
		s += iv.End - iv.Start
	}
	return s
}

func mergeIntervals(iv []Interval) []Interval {
	if len(iv) == 0 {
		return nil
	}
	sort.Slice(iv, func(i, j int) bool { return iv[i].Start < iv[j].Start })
	out := []Interval{iv[0]}
	for _, x := range iv[1:] {
		if last := &out[len(out)-1]; x.Start <= last.End {
			last.End = max(last.End, x.End)
		} else {
			out = append(out, x)
		}
	}
	return out
}

// Intersect is the part of the track both coverages cover.
func Intersect(a, b Coverage) Coverage {
	switch {
	case a.Full:
		return b
	case b.Full:
		return a
	}
	var out []Interval
	i, j := 0, 0
	for i < len(a.Intervals) && j < len(b.Intervals) {
		x, y := a.Intervals[i], b.Intervals[j]
		if s, e := max(x.Start, y.Start), min(x.End, y.End); e > s {
			out = append(out, Interval{s, e})
		}
		if x.End < y.End {
			i++
		} else {
			j++
		}
	}
	return Coverage{Intervals: out}
}

// Comparison is the word-level edit distance of a hypothesis against a
// reference over some coverage. WER = Errors / RefWords.
type Comparison struct {
	Errors   int64
	RefWords int64
	HypWords int64
}

// Add accumulates another comparison (tracks are compared one at a time).
func (c *Comparison) Add(o Comparison) {
	c.Errors += o.Errors
	c.RefWords += o.RefWords
	c.HypWords += o.HypWords
}

// WER is Errors / RefWords (0 with no reference words).
func (c Comparison) WER() float64 { return ratio(c.Errors, c.RefWords) }

// Compare is the word disagreement of hyp against ref over cov. Full coverage
// compares the whole token sequences; otherwise each covered interval is
// compared on its own — the tokens whose time midpoint lies in it — so text
// outside the covered windows is never counted as deleted.
func Compare(ref, hyp []Token, cov Coverage) Comparison {
	if cov.Full {
		return Comparison{
			Errors:   int64(WordErrors(norms(ref), norms(hyp))),
			RefWords: int64(len(ref)),
			HypWords: int64(len(hyp)),
		}
	}
	var out Comparison
	for _, iv := range cov.Intervals {
		r, h := within(ref, iv), within(hyp, iv)
		out.Add(Comparison{
			Errors:   int64(WordErrors(r, h)),
			RefWords: int64(len(r)),
			HypWords: int64(len(h)),
		})
	}
	return out
}

func within(toks []Token, iv Interval) []string {
	var out []string
	for _, t := range toks {
		if m := t.Mid(); m >= iv.Start && m <= iv.End {
			out = append(out, t.Norm)
		}
	}
	return out
}

// VoteTally is one model's current votes on held findings.
type VoteTally struct {
	HeardReplacement, HeardOriginal, Neither, Unmappable int64
}

// Add counts n votes of kind vote.
func (t *VoteTally) Add(vote string, n int64) {
	switch vote {
	case VoteHeardReplacement:
		t.HeardReplacement += n
	case VoteHeardOriginal:
		t.HeardOriginal += n
	case VoteNeither:
		t.Neither += n
	case VoteUnmappable:
		t.Unmappable += n
	}
}

// Voted is every finding with a vote; Resolved those the pilot settled one
// way or the other (heard_replacement + heard_original).
func (t VoteTally) Voted() int64 {
	return t.HeardReplacement + t.HeardOriginal + t.Neither + t.Unmappable
}

// Resolved is heard_replacement + heard_original.
func (t VoteTally) Resolved() int64 { return t.HeardReplacement + t.HeardOriginal }

// PairAgreement is how often two models' votes on the same findings agree.
type PairAgreement struct {
	ModelA, ModelB string
	// Both counts findings both voted on; Mapped those neither marked
	// unmappable, Agree those with equal mapped votes. Resolved counts the
	// findings both resolved (heard_*), ResolvedAgree those resolved the same
	// way — the agreement that matters for evidence.
	Both, Mapped, Agree, Resolved, ResolvedAgree int64
}

// AgreementRate is Agree / Mapped.
func (p PairAgreement) AgreementRate() float64 { return ratio(p.Agree, p.Mapped) }

// ResolvedAgreementRate is ResolvedAgree / Resolved.
func (p PairAgreement) ResolvedAgreementRate() float64 { return ratio(p.ResolvedAgree, p.Resolved) }

// VoteCell is one cell of the two-model vote cross-tabulation.
type VoteCell struct {
	ModelA, ModelB string
	VoteA, VoteB   string
	N              int64
}

// Agreement folds the cross-tabulation into one PairAgreement per model pair,
// in first-seen order.
func Agreement(cells []VoteCell) []PairAgreement {
	idx := map[[2]string]int{}
	var out []PairAgreement
	for _, c := range cells {
		k := [2]string{c.ModelA, c.ModelB}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, PairAgreement{ModelA: c.ModelA, ModelB: c.ModelB})
		}
		p := &out[i]
		p.Both += c.N
		if c.VoteA == VoteUnmappable || c.VoteB == VoteUnmappable {
			continue
		}
		p.Mapped += c.N
		if c.VoteA == c.VoteB {
			p.Agree += c.N
		}
		if resolved(c.VoteA) && resolved(c.VoteB) {
			p.Resolved += c.N
			if c.VoteA == c.VoteB {
				p.ResolvedAgree += c.N
			}
		}
	}
	return out
}

func resolved(v string) bool { return v == VoteHeardReplacement || v == VoteHeardOriginal }

// ModelStats is one pilot model's report row, accumulated track by track.
type ModelStats struct {
	ModelID, ModelRevision, Backend, Scope string

	Tracks   int
	AudioSec float64
	// WallSec sums wall time over the tracks that reported both audio and
	// wall time; RTFxAudioSec is their audio. RTFx = RTFxAudioSec / WallSec.
	WallSec, RTFxAudioSec float64
	PeakVRAMMiB           int64
	PeakReservedMiB       int64
	// CoveredSec is the audio the WER was computed over (scope 'findings':
	// the windows only).
	CoveredSec float64

	VsProduction Comparison
	Text         TextStats
	Votes        VoteTally
}

// AddTrack accumulates one pilot transcript's metadata.
func (m *ModelStats) AddTrack(p db.PilotTranscript) {
	m.Tracks++
	if p.AudioSec != nil {
		m.AudioSec += *p.AudioSec
		if p.WallSec != nil && *p.WallSec > 0 {
			m.WallSec += *p.WallSec
			m.RTFxAudioSec += *p.AudioSec
		}
	}
	if p.PeakVRAMMiB != nil && *p.PeakVRAMMiB > m.PeakVRAMMiB {
		m.PeakVRAMMiB = *p.PeakVRAMMiB
	}
	if p.PeakReservedMiB != nil && *p.PeakReservedMiB > m.PeakReservedMiB {
		m.PeakReservedMiB = *p.PeakReservedMiB
	}
}

// RTFx is total audio / total wall time (0 when no track reported both).
func (m ModelStats) RTFx() float64 {
	if m.WallSec <= 0 {
		return 0
	}
	return m.RTFxAudioSec / m.WallSec
}

// CrossStats is the word disagreement between two pilot models on the
// tracks (and, for partial scopes, the windows) both transcribed. A is the
// reference side.
type CrossStats struct {
	ModelA, ModelB string
	Partial        bool // at least one side was scope 'findings'
	Tracks         int
	CoveredSec     float64
	Comparison
}
