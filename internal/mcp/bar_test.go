package mcp

import (
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
)

func TestComputePipelineBar_Empty(t *testing.T) {
	got := computePipelineBar(db.PipelineBuckets{}, 0)
	if got.Total != 0 {
		t.Errorf("empty: Total = %d, want 0", got.Total)
	}
	if len(got.Segments) != 0 {
		t.Errorf("empty: Segments = %v, want nil/empty", got.Segments)
	}
}

func TestComputePipelineBar_SumsTo100(t *testing.T) {
	// Representative distribution: all five non-failed buckets populated.
	b := db.PipelineBuckets{
		Pending:         42,
		Claimed:         1,
		TranscribedOnly: 20,
		EvaldOnly:       3,
		EmbeddedReady:   280,
		Failed:          2,
	}
	bar := computePipelineBar(b, 250)

	// Denominator excludes failed.
	wantTotal := 42 + 1 + 20 + 3 + 280
	if bar.Total != wantTotal {
		t.Errorf("Total = %d, want %d", bar.Total, wantTotal)
	}
	if bar.Failed != 2 {
		t.Errorf("Failed = %d, want 2", bar.Failed)
	}

	// All segments must be present (every bucket > 0).
	if len(bar.Segments) != 5 {
		t.Errorf("len(Segments) = %d, want 5; segments: %v", len(bar.Segments), bar.Segments)
	}

	// Widths must sum to exactly 100.
	sum := 0
	for _, s := range bar.Segments {
		sum += s.Pct
	}
	if sum != 100 {
		t.Errorf("segment widths sum to %d, want 100; segments: %v", sum, bar.Segments)
	}
}

func TestComputePipelineBar_ZeroOmitted(t *testing.T) {
	// Only two buckets populated: pending and embedded.
	b := db.PipelineBuckets{
		Pending:       10,
		EmbeddedReady: 90,
	}
	bar := computePipelineBar(b, 0)

	if len(bar.Segments) != 2 {
		t.Errorf("len(Segments) = %d, want 2", len(bar.Segments))
	}
	// Verify the two present segments sum to 100.
	sum := 0
	for _, s := range bar.Segments {
		sum += s.Pct
	}
	if sum != 100 {
		t.Errorf("widths sum to %d, want 100", sum)
	}
}

func TestComputePipelineBar_MinWidthFloor(t *testing.T) {
	// A single-track minority bucket must not shrink to 0% when a large dominant
	// bucket exists. With 1 claimed out of 1000 total, raw = 0%; floor = 2%.
	b := db.PipelineBuckets{
		Pending:       1,
		Claimed:       1,
		EmbeddedReady: 998,
	}
	bar := computePipelineBar(b, 0)

	// Three segments.
	if len(bar.Segments) != 3 {
		t.Fatalf("len(Segments) = %d, want 3", len(bar.Segments))
	}
	// Each non-zero segment must be >= 1% (the borrow can never go below 1 due to
	// the guard) and, specifically, the tiny segments should have been boosted.
	for _, s := range bar.Segments {
		if s.Pct < 1 {
			t.Errorf("segment %q has Pct=%d < 1", s.Label, s.Pct)
		}
	}
	// Widths must still sum to exactly 100.
	sum := 0
	for _, s := range bar.Segments {
		sum += s.Pct
	}
	if sum != 100 {
		t.Errorf("widths sum to %d after floor, want 100", sum)
	}
}

func TestComputePipelineBar_LargestRemainderSumsTo100(t *testing.T) {
	// Three equal buckets of 33 each (total 99 — truncating division leaves 1
	// in the remainder to distribute). Sum must still be 100.
	b := db.PipelineBuckets{
		Pending:         33,
		TranscribedOnly: 33,
		EmbeddedReady:   34,
	}
	bar := computePipelineBar(b, 0)

	sum := 0
	for _, s := range bar.Segments {
		sum += s.Pct
	}
	if sum != 100 {
		t.Errorf("widths sum to %d, want 100 (largest-remainder)", sum)
	}
}

func TestComputePipelineBar_OnlyReady(t *testing.T) {
	// All embedded: single green segment should be 100%.
	b := db.PipelineBuckets{EmbeddedReady: 500}
	bar := computePipelineBar(b, 480)

	if len(bar.Segments) != 1 {
		t.Fatalf("len(Segments) = %d, want 1", len(bar.Segments))
	}
	if bar.Segments[0].Pct != 100 {
		t.Errorf("Pct = %d, want 100", bar.Segments[0].Pct)
	}
	if bar.EvalLabel == "" {
		t.Error("EvalLabel should be populated when EmbeddedReady > 0")
	}
}

func TestComputePipelineBar_PipelineOrder(t *testing.T) {
	// Verify left-to-right order: not-started < transcribing < transcribed-only
	// < evald-only < ready.
	b := db.PipelineBuckets{
		Pending:         10,
		Claimed:         5,
		TranscribedOnly: 15,
		EvaldOnly:       5,
		EmbeddedReady:   65,
	}
	bar := computePipelineBar(b, 60)
	if len(bar.Segments) != 5 {
		t.Fatalf("len(Segments) = %d, want 5", len(bar.Segments))
	}
	wantIDs := []barSegID{
		segNotStarted,
		segTranscribing,
		segTranscribedOnly,
		segEvaldOnly,
		segEmbeddedReady,
	}
	for i, s := range bar.Segments {
		if s.ID != wantIDs[i] {
			t.Errorf("Segments[%d].ID = %d, want %d", i, s.ID, wantIDs[i])
		}
	}
}

func TestComputePipelineBar_FailedNotInSegments(t *testing.T) {
	b := db.PipelineBuckets{
		EmbeddedReady: 100,
		Failed:        7,
	}
	bar := computePipelineBar(b, 0)

	// Failed is off the bar; only one green segment.
	if len(bar.Segments) != 1 {
		t.Errorf("len(Segments) = %d, want 1", len(bar.Segments))
	}
	// But Failed is preserved for the callout.
	if bar.Failed != 7 {
		t.Errorf("Failed = %d, want 7", bar.Failed)
	}
	// Denominator excludes failed.
	if bar.Total != 100 {
		t.Errorf("Total = %d, want 100", bar.Total)
	}
}
