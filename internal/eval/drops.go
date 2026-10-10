package eval

// Drop reasons: why a finding the judge returned never became a row. They
// label RunStats.Dropped, the --dump harness lines and
// earmark_judge_dropped_findings_total{reason}, so treat them as a data
// contract: add new ones, never rename.
const (
	// DropEmptySpan — no original_text.
	DropEmptySpan = "empty_span"
	// DropEmptyCorrection — no suggested_correction.
	DropEmptyCorrection = "empty_correction"
	// DropCosmeticOnly — original and correction are the same words once case,
	// punctuation and hyphens are folded away.
	DropCosmeticOnly = "cosmetic_only"
	// DropBelowMinConfidence — below EVAL_MIN_CONFIDENCE.
	DropBelowMinConfidence = "below_min_confidence"
	// DropOverCap — past EVAL_MAX_FINDINGS_PER_CHUNK (the lowest-confidence
	// ones go).
	DropOverCap = "over_cap"
)

// Dropped is one finding the judge returned that was filtered out, and why.
// Kept so a measurement run (--dump) sees the judge's raw output, not only
// the survivors.
type Dropped struct {
	Reason              string
	OriginalText        string
	SuggestedCorrection string
	IssueType           string
	Confidence          float64
}

func dropOf(p parsedFinding, reason string) Dropped {
	return Dropped{
		Reason:              reason,
		OriginalText:        p.OriginalText,
		SuggestedCorrection: p.SuggestedCorrection,
		IssueType:           p.IssueType,
		Confidence:          p.Confidence,
	}
}
