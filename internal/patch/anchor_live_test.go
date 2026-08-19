package patch

import (
	"strings"
	"testing"
)

// These cases are not hypothetical. They are verbatim captures of what real
// judge models returned when probed with earmark's own system prompt against
// the live Ollama endpoint on desktop-2 (2026-08-18). They exist because the
// synthetic tests in anchor_test.go prove the ladder is *internally*
// consistent, while these prove it survives what models actually emit.
//
// The headline finding: a model can return a CORRECT finding with a WRONG
// offset, in the same response. Structured output guarantees the shape of a
// reply, not the truth of the numbers inside it.

// liveSpan is the exact transcript span used in the probes.
const liveSpan = "the signal was sent from the auto sebo observatory in nineteen seventy four"

// TestLiveModelOffsetIsUntrusted is the regression guard for the observed
// failure. qwen3.8, asked with response_format json_schema and thinking
// suppressed, reported anchor_offset=27 for "auto sebo" — which actually starts
// at 29. Offset 27 selects "e auto se": two characters early, straddling the
// word boundary.
//
// If Locate trusted the reported offset, the applied result would have been
// "...from thareciboobservatory in..." — a corruption that produces no error
// and reads as a successful edit. This test fails if anyone ever "optimises"
// Locate into trusting the model's number.
func TestLiveModelOffsetIsUntrusted(t *testing.T) {
	const (
		original   = "auto sebo"
		correction = "arecibo"
		// What qwen3.8 actually reported.
		reportedOffset = 27
		// Where the span actually is.
		trueOffset = 29
	)

	// Guard the premise: if the fixture ever drifts so the model happened to be
	// right, this test is no longer testing anything.
	if got := strings.Index(liveSpan, original); got != trueOffset {
		t.Fatalf("fixture drift: %q starts at %d, expected %d", original, got, trueOffset)
	}
	if reportedOffset == trueOffset {
		t.Fatal("fixture drift: the reported offset is no longer wrong, so this test is vacuous")
	}

	// The bad offset really does select the wrong text.
	runes := []rune(liveSpan)
	if got := string(runes[reportedOffset : reportedOffset+len([]rune(original))]); got != "e auto se" {
		t.Fatalf("expected the bad offset to select %q, got %q", "e auto se", got)
	}

	span, err := Locate(liveSpan, Anchor{
		OriginalText: original,
		Offset:       reportedOffset,
		Occurrence:   0,
	})
	if err != nil {
		t.Fatalf("Locate should recover via the occurrence index, got error: %v", err)
	}
	if span.Start != trueOffset {
		t.Errorf("Locate trusted the model's offset: want start %d, got %d", trueOffset, span.Start)
	}

	got, _, err := Apply(liveSpan, ChunkHash(liveSpan), Anchor{
		OriginalText: original,
		Offset:       reportedOffset,
		Occurrence:   0,
	}, correction)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	const want = "the signal was sent from the arecibo observatory in nineteen seventy four"
	if got != want {
		t.Errorf("Apply produced corrupted text.\n want: %q\n  got: %q", want, got)
	}
	// The specific corruption the bad offset would have caused.
	if strings.Contains(got, "thareciboobservatory") {
		t.Error("Apply used the model's raw offset and welded words together")
	}
}

// TestLiveModelOffsetCorrectPathStillWorks is the same finding from the other
// probe: with thinking ENABLED (the pre-#131 request shape) qwen3.8 reported
// offset 29, which is correct. Suppressing thinking made the finding cheaper
// (87 vs 953 completion tokens) but the offset worse.
//
// Both paths must land on the same applied text. If they ever diverge, the
// resolution ladder has become sensitive to how the model was prompted, which
// would make patch behaviour depend on inference settings — exactly the kind of
// coupling the anchor design exists to prevent.
func TestLiveModelOffsetCorrectPathStillWorks(t *testing.T) {
	const (
		original   = "auto sebo"
		correction = "arecibo"
		want       = "the signal was sent from the arecibo observatory in nineteen seventy four"
	)

	for _, tc := range []struct {
		name   string
		offset int
	}{
		{"thinking on — model reported the correct offset", 29},
		{"thinking suppressed — model reported a wrong offset", 27},
		{"model omitted the offset entirely", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := Apply(liveSpan, ChunkHash(liveSpan), Anchor{
				OriginalText: original,
				Offset:       tc.offset,
				Occurrence:   0,
			}, correction)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got != want {
				t.Errorf("want %q, got %q", want, got)
			}
		})
	}
}
