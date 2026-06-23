package mcp

import (
	"html/template"

	"github.com/jedwards1230/earmark/internal/db"
)

// barSegID identifies one segment of the pipeline bar in pipeline order.
// The order determines left-to-right visual position in the stacked bar.
type barSegID int

const (
	segNotStarted      barSegID = iota // pending (not yet claimed)
	segTranscribing                    // claimed (in-flight)
	segTranscribedOnly                 // done, no eval, no chunks
	segEvaldOnly                       // done, eval finished, no chunks
	segEmbeddedReady                   // done, has chunks (terminal / goal)
)

// barSeg is one rendered segment of the pipeline bar.
type barSeg struct {
	ID     barSegID
	Label  string       // human label for the legend
	Color  template.CSS // CSS custom property, e.g. "var(--pp-not-started)"; typed template.CSS so html/template trusts the var() in a style attr (a plain string is sanitized to ZgotmplZ)
	Count  int          // raw track count
	Pct    int          // integer percent width (0–100); 0 means omit the segment
	Status string       // ?status= filter value for the legend link
}

// pipelineBarData is the fully-computed bar model passed to the template.
// It only contains segments with Count > 0.
type pipelineBarData struct {
	Segments  []barSeg
	Total     int    // denominator (non-failed tracks)
	Failed    int    // kept for the failed-callout below; NOT a bar segment
	EvalLabel string // "of N ready, M% judged" overlay near the green segment
}

// computePipelineBar converts PipelineBuckets into a pipelineBarData suitable
// for the stacked bar template.
//
// Width algorithm:
//  1. Denominator = all non-failed tracks (pending+claimed+done varieties).
//  2. Compute raw integer percents via truncating division.
//  3. Apply min-width floor: any segment with Count>0 but width<2% gets
//     minPct=2; the borrowed width is taken from the LARGEST segment so the
//     row always sums to 100% and never overflows.
//  4. Zero-count segments are omitted (no span, no legend row).
func computePipelineBar(b db.PipelineBuckets, evalCoverageDone int) pipelineBarData {
	// Denominator is non-failed tracks.
	total := b.Pending + b.Claimed + b.TranscribedOnly + b.EvaldOnly + b.EmbeddedReady

	bar := pipelineBarData{
		Total:  total,
		Failed: b.Failed,
	}

	if total <= 0 {
		return bar
	}

	// Eval coverage overlay on the EmbeddedReady (green) segment.
	if b.EmbeddedReady > 0 {
		evalPct := evalCoverageDone * 100 / b.EmbeddedReady
		if evalPct > 100 {
			evalPct = 100
		}
		bar.EvalLabel = commafy(evalCoverageDone) + " / " + commafy(b.EmbeddedReady) + " judged (" + commafy(evalPct) + "%)"
	}

	// Raw segment definitions (pipeline order).
	raw := []barSeg{
		{ID: segNotStarted, Label: "not started", Color: template.CSS("var(--pp-not-started)"), Count: b.Pending, Status: "pending"},
		{ID: segTranscribing, Label: "transcribing", Color: template.CSS("var(--pp-transcribing)"), Count: b.Claimed, Status: "claimed"},
		{ID: segTranscribedOnly, Label: "transcribed (awaiting embed)", Color: template.CSS("var(--pp-transcribed)"), Count: b.TranscribedOnly, Status: "done"},
		{ID: segEvaldOnly, Label: "eval'd (awaiting embed)", Color: template.CSS("var(--pp-evald)"), Count: b.EvaldOnly, Status: "done"},
		{ID: segEmbeddedReady, Label: "ready / searchable", Color: template.CSS("var(--pp-ready)"), Count: b.EmbeddedReady, Status: "done"},
	}

	// Compute raw integer percents (truncating division = largest-remainder base).
	for i := range raw {
		if raw[i].Count <= 0 {
			continue
		}
		raw[i].Pct = raw[i].Count * 100 / total
	}

	// Remainder: the difference between 100 and the sum of raw truncated percents.
	// Largest-remainder method: give the remaining point(s) to the largest segments
	// that have a remainder, then adjust for any minimum-width borrows below.
	sumPct := 0
	for _, s := range raw {
		sumPct += s.Pct
	}
	remainder := 100 - sumPct // >= 0 always (truncating division)
	// Give remainder to largest segment first.
	for i := 0; remainder > 0; i = (i + 1) % len(raw) {
		if raw[i].Count > 0 && raw[i].Pct > 0 {
			raw[i].Pct++
			remainder--
		}
	}

	// Min-width floor: raise every non-zero segment that truncated below 2% up to
	// 2% so it stays visible, then reclaim the width we added by trimming the
	// largest reducible segment(s) one point at a time (never below the floor).
	// Raise-then-reclaim (rather than per-segment borrow-if-affordable) keeps the
	// row summing to exactly 100% even when many tiny segments compete for floor
	// from one dominant segment — the cascade that a borrow-guard silently drops.
	const minPct = 2
	for i := range raw {
		if raw[i].Count > 0 && raw[i].Pct < minPct {
			raw[i].Pct = minPct
		}
	}
	// Reclaim the surplus (raising tiny segments pushed the sum above 100).
	sumPct = 0
	for _, s := range raw {
		sumPct += s.Pct
	}
	for surplus := sumPct - 100; surplus > 0; surplus-- {
		maxIdx := -1
		for j := range raw {
			if raw[j].Count > 0 && raw[j].Pct > minPct && (maxIdx < 0 || raw[j].Pct > raw[maxIdx].Pct) {
				maxIdx = j
			}
		}
		if maxIdx < 0 {
			break // pathological: every segment already at the floor — nothing to trim
		}
		raw[maxIdx].Pct--
	}

	// Collect non-zero segments for the template.
	for _, s := range raw {
		if s.Count > 0 {
			bar.Segments = append(bar.Segments, s)
		}
	}
	return bar
}
