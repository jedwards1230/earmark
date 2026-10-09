package decide

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
)

// DumpRecord is one line of `earmark decide --dump`: a sampled finding, its
// rung-0 verdict and, when the evaluator ran, what it decided. Unlike the
// report it carries finding ids and text — it is a local measurement file
// for comparing two rung-0 versions over the same sample, never persisted.
type DumpRecord struct {
	FindingID  string  `json:"finding_id"`
	IssueType  string  `json:"issue_type"`
	JudgeModel string  `json:"judge_model,omitempty"`
	Confidence float64 `json:"confidence"`
	// PatchState is the finding's state when sampled; in a calibration run
	// it is the human label (accepted/applied vs rejected).
	PatchState  string    `json:"patch_state"`
	Original    string    `json:"original"`
	Replacement string    `json:"replacement"`
	Rung0       DumpRung0 `json:"rung0"`
	// Class is the report class: apply/hold/reject/reanchor, or rung0_pass
	// when the evaluator did not run.
	Class string `json:"class"`
	// Outcome, Reason and P are set only when the evaluator ran on a rung-0
	// pass (P only when the model answered usably).
	Outcome string   `json:"outcome,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	P       *float64 `json:"p,omitempty"`
	// Evidence is the text-evidence class of a rung-0 pass.
	Evidence string `json:"evidence,omitempty"`
}

// DumpRung0 is the rung-0 verdict in a DumpRecord.
type DumpRung0 struct {
	Pass     bool   `json:"pass"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence"`
}

// dumpRecord builds the DumpRecord of one evaluated (or rung-0-only) item.
func dumpRecord(it *item) DumpRecord {
	f, o := it.finding, it.outcome
	r := DumpRecord{
		FindingID: f.ID, IssueType: f.IssueType, JudgeModel: f.JudgeModel, Confidence: f.Confidence,
		PatchState: f.PatchState, Original: f.Original, Replacement: f.Replacement,
		Rung0: DumpRung0{Pass: o.Rung0.Pass, Reason: o.Rung0.Reason, Evidence: o.Rung0.Evidence},
		Class: classOf(o),
	}
	if o.Rung0.Pass {
		r.Evidence = o.Evidence
		if o.Decision != "" {
			r.Outcome, r.Reason, r.P = o.Decision, o.Reason, o.P
		}
	}
	return r
}

// writeDump writes one JSON line per item, sorted by finding id.
func writeDump(w io.Writer, items []*item) error {
	recs := make([]DumpRecord, len(items))
	for i, it := range items {
		recs[i] = dumpRecord(it)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].FindingID < recs[j].FindingID })
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return bw.Flush()
}
