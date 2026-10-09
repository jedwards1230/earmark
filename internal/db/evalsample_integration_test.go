package db

import (
	"context"
	"slices"
	"testing"
)

func evalChunkIDs(cs []EvalChunk) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ChunkID
	}
	return out
}

// TestIntegrationSampleEvalChunksSeeded: the same seed picks the same chunks
// in the same order (what lets an old and a new judge prompt be compared on
// identical input), a different seed orders them differently, the limit caps
// the sample, and a blank seed is refused. Skipped unless
// EARMARK_TEST_DATABASE_URL.
func TestIntegrationSampleEvalChunksSeeded(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()
	seedChunkScan(t, d)

	a, err := d.SampleEvalChunksSeeded(ctx, 4, "q4")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 4 {
		t.Fatalf("%d chunks, want 4", len(a))
	}
	again, err := d.SampleEvalChunksSeeded(ctx, 4, "q4")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(evalChunkIDs(a), evalChunkIDs(again)) {
		t.Errorf("same seed, different sample: %v vs %v", evalChunkIDs(a), evalChunkIDs(again))
	}
	head, err := d.SampleEvalChunksSeeded(ctx, 2, "q4")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(evalChunkIDs(head), evalChunkIDs(a)[:2]) {
		t.Errorf("limit 2 is not the head of the seeded order: %v vs %v", evalChunkIDs(head), evalChunkIDs(a))
	}
	// Pristine text, like every eval chunk read.
	for _, c := range a {
		if c.ChunkIndex == 1 && c.FilePath == "/b/Dune/01.m4b" && c.Text != "seg two" {
			t.Errorf("chunk 1 text = %q, want the pristine source text", c.Text)
		}
	}

	// Some other seed orders the four chunks differently (24 orders; try a few).
	differs := false
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		o, err := d.SampleEvalChunksSeeded(ctx, 4, s)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(evalChunkIDs(o), evalChunkIDs(a)) {
			differs = true
			break
		}
	}
	if !differs {
		t.Error("every seed produced the same order")
	}

	if _, err := d.SampleEvalChunksSeeded(ctx, 4, "  "); err == nil {
		t.Error("blank seed accepted")
	}
}
