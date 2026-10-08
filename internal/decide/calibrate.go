package decide

import (
	"fmt"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// Human labels in a calibration run.
const (
	HumanAccepted = "accepted" // patch_state accepted or applied, decided through the review surface
	HumanRejected = "rejected"
)

// humanLabel is the human decision a calibration finding carries.
func humanLabel(f db.DecideFinding) string {
	if f.PatchState == patch.StateRejected {
		return HumanRejected
	}
	return HumanAccepted
}

// Calibration compares the decide step's outcomes with the decisions humans
// already made on the same findings. Aggregates only.
type Calibration struct {
	HumanAccepted int `json:"human_accepted"`
	HumanRejected int `json:"human_rejected"`
	// Matrix counts human label → outcome class.
	Matrix map[string]map[string]int `json:"matrix"`
	// ApplyPrecision: of the findings decide would apply, the share humans
	// accepted. ApplyRecall: of the human accepts, the share decide would
	// apply. RejectPrecision: of decide's rejects (re-anchor cases excluded),
	// the share humans rejected. nil when the denominator is zero.
	ApplyPrecision  *float64 `json:"apply_precision,omitempty"`
	ApplyRecall     *float64 `json:"apply_recall,omitempty"`
	RejectPrecision *float64 `json:"reject_precision,omitempty"`
	// HoldRate is the share of findings held.
	HoldRate float64 `json:"hold_rate"`
	// Sweep re-runs rung 0 at other phonetic_min_sim values offline: no new
	// model call is made, so a finding that passes only at a lower threshold
	// has no p and is counted Unasked.
	Sweep []SweepRow `json:"sweep"`
}

// SweepRow is the agreement at one phonetic_min_sim.
type SweepRow struct {
	Threshold float64 `json:"phonetic_min_sim"`
	// Rung0Pass counts findings passing rung 0, per human label.
	Rung0PassAccepted int `json:"rung0_pass_accepted"`
	Rung0PassRejected int `json:"rung0_pass_rejected"`
	// Unasked passed rung 0 here but has no p from the run.
	Unasked int `json:"unasked"`
	// Apply/Reject/Hold are the decisions at this threshold (re-anchor cases
	// and Unasked excluded); the *Agree counts are those matching the human.
	Apply          int      `json:"apply"`
	ApplyAgree     int      `json:"apply_agree"`
	Reject         int      `json:"reject"`
	RejectAgree    int      `json:"reject_agree"`
	Hold           int      `json:"hold"`
	ApplyPrecision *float64 `json:"apply_precision,omitempty"`
}

func ratio(num, den int) *float64 {
	if den == 0 {
		return nil
	}
	r := float64(num) / float64(den)
	return &r
}

// calibrate scores items (a calibration run's evaluated findings) against
// their human labels, and sweeps rung 0's threshold.
func calibrate(items []*item, p ShouldApplyParams, sweep []float64) *Calibration {
	c := &Calibration{Matrix: map[string]map[string]int{
		HumanAccepted: {}, HumanRejected: {},
	}}
	var apply, applyAgree, reject, rejectAgree, hold int
	for _, it := range items {
		label := humanLabel(it.finding)
		if label == HumanAccepted {
			c.HumanAccepted++
		} else {
			c.HumanRejected++
		}
		class := classOf(it.outcome)
		c.Matrix[label][class]++
		switch class {
		case DecisionApply:
			apply++
			if label == HumanAccepted {
				applyAgree++
			}
		case DecisionReject:
			reject++
			if label == HumanRejected {
				rejectAgree++
			}
		case DecisionHold:
			hold++
		}
	}
	c.ApplyPrecision = ratio(applyAgree, apply)
	c.ApplyRecall = ratio(applyAgree, c.HumanAccepted)
	c.RejectPrecision = ratio(rejectAgree, reject)
	if len(items) > 0 {
		c.HoldRate = float64(hold) / float64(len(items))
	}
	for _, t := range sweep {
		c.Sweep = append(c.Sweep, sweepAt(items, p, t))
	}
	return c
}

// sweepAt re-decides items with rung 0 at threshold t, reusing each item's p.
func sweepAt(items []*item, p ShouldApplyParams, t float64) SweepRow {
	row := SweepRow{Threshold: t}
	// Re-run rung 0 per chunk so dedupe sees the same competitors.
	byChunk := map[*db.DecideChunk][]db.DecideFinding{}
	for _, it := range items {
		byChunk[it.chunk] = append(byChunk[it.chunk], it.finding)
	}
	verdicts := map[string]Verdict{}
	for chunk, fs := range byChunk {
		for id, v := range chunkVerdicts(fs, chunk, t) {
			verdicts[id] = v
		}
	}
	for _, it := range items {
		label := humanLabel(it.finding)
		v := verdicts[it.finding.ID]
		if !v.Pass {
			if v.Reason == ReasonChunkChanged || v.Reason == ReasonAnchorMissing {
				continue
			}
			row.Reject++
			if label == HumanRejected {
				row.RejectAgree++
			}
			continue
		}
		if label == HumanAccepted {
			row.Rung0PassAccepted++
		} else {
			row.Rung0PassRejected++
		}
		if it.outcome.P == nil {
			row.Unasked++
			continue
		}
		evidence := TextEvidence(it.input.Candidate, v, it.sentences)
		switch d, _ := p.Decide(*it.outcome.P, evidence, it.finding.IssueType); d {
		case DecisionApply:
			row.Apply++
			if label == HumanAccepted {
				row.ApplyAgree++
			}
		case DecisionReject:
			row.Reject++
			if label == HumanRejected {
				row.RejectAgree++
			}
		default:
			row.Hold++
		}
	}
	row.ApplyPrecision = ratio(row.ApplyAgree, row.Apply)
	return row
}

func pct(r *float64) string {
	if r == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100**r)
}

func (c *Calibration) print(p func(string, ...any)) {
	p("\ncalibration: %d human-accepted · %d human-rejected\n", c.HumanAccepted, c.HumanRejected)
	for _, label := range []string{HumanAccepted, HumanRejected} {
		p("  human %-8s → %s\n", label, counts(c.Matrix[label], reportClasses))
	}
	p("  apply precision %s · apply recall %s · reject precision %s · hold rate %.1f%%\n",
		pct(c.ApplyPrecision), pct(c.ApplyRecall), pct(c.RejectPrecision), 100*c.HoldRate)
	p("  phonetic_min_sim sweep (rung 0 re-run offline; no new model calls):\n")
	p("    %-6s %10s %10s %8s %6s %10s %7s %11s %5s\n", "sim", "pass(acc)", "pass(rej)", "unasked", "apply", "precision", "reject", "rej agree", "hold")
	for _, r := range c.Sweep {
		p("    %-6.2f %10d %10d %8d %6d %10s %7d %11d %5d\n", r.Threshold, r.Rung0PassAccepted, r.Rung0PassRejected,
			r.Unasked, r.Apply, pct(r.ApplyPrecision), r.Reject, r.RejectAgree, r.Hold)
	}
}
