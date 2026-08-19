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

// livePatch builds the replayable patch for a probe result. The chunk hash is
// the live span's own, so these exercise the normal (hash-verified) path.
func livePatch(a Anchor, correction string) Patch {
	return Patch{
		ID:         "live",
		Anchor:     a,
		Correction: correction,
		ChunkHash:  ChunkHash(liveSpan),
	}
}

// TestLiveModelOffsetIsUntrusted is the regression guard for the observed
// failure. qwen3.8, asked with response_format json_schema and thinking
// suppressed, reported anchor_offset=27 for "auto sebo" — which actually starts
// at 29. Offset 27 selects "e auto se": two characters early, straddling the
// word boundary.
//
// If Locate trusted the reported offset, the replayed result would have been
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

	// The resolution ladder itself, called directly — this is the actual
	// regression guard and must stay a direct Locate call.
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

	// And end-to-end through the production replay path.
	got, err := ApplyCorrections(liveSpan, []Patch{livePatch(Anchor{
		OriginalText: original,
		Offset:       reportedOffset,
		Occurrence:   0,
	}, correction)})
	if err != nil {
		t.Fatalf("ApplyCorrections: %v", err)
	}

	const want = "the signal was sent from the arecibo observatory in nineteen seventy four"
	if got != want {
		t.Errorf("ApplyCorrections produced corrupted text.\n want: %q\n  got: %q", want, got)
	}
	// The specific corruption the bad offset would have caused.
	if strings.Contains(got, "thareciboobservatory") {
		t.Error("replay used the model's raw offset and welded words together")
	}

	// The lenient path must reach the same text and quarantine nothing — a
	// recoverable bad offset is not a reason to retire a finding.
	res := Replay(liveSpan, []Patch{livePatch(Anchor{
		OriginalText: original,
		Offset:       reportedOffset,
		Occurrence:   0,
	}, correction)})
	if res.Text != want {
		t.Errorf("Replay diverged from ApplyCorrections.\n want: %q\n  got: %q", want, res.Text)
	}
	if len(res.Stale) != 0 {
		t.Errorf("a recoverable model offset must not make the patch stale: %+v", res.Stale)
	}
	if len(res.Applied) != 1 || res.Applied[0].Span.Start != trueOffset {
		t.Errorf("Replay recorded the wrong span: %+v", res.Applied)
	}
	if len(res.Applied) == 1 && res.Applied[0].Before != original {
		t.Errorf("recorded before-text should be the located span %q, got %q",
			original, res.Applied[0].Before)
	}
}

// TestLiveModelOffsetCorrectPathStillWorks is the same finding from the other
// probe: with thinking ENABLED (the pre-#131 request shape) qwen3.8 reported
// offset 29, which is correct. Suppressing thinking made the finding cheaper
// (87 vs 953 completion tokens) but the offset worse.
//
// All three paths must land on the same replayed text. If they ever diverge,
// the resolution ladder has become sensitive to how the model was prompted,
// which would make patch behaviour depend on inference settings — exactly the
// kind of coupling the anchor design exists to prevent.
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
			p := livePatch(Anchor{
				OriginalText: original,
				Offset:       tc.offset,
				Occurrence:   0,
			}, correction)

			got, err := ApplyCorrections(liveSpan, []Patch{p})
			if err != nil {
				t.Fatalf("ApplyCorrections: %v", err)
			}
			if got != want {
				t.Errorf("want %q, got %q", want, got)
			}

			// The projection layer uses Replay, so it must agree exactly.
			res := Replay(liveSpan, []Patch{p})
			if res.Text != want {
				t.Errorf("Replay disagreed with ApplyCorrections: want %q, got %q", want, res.Text)
			}
			if len(res.Applied) != 1 || len(res.Stale) != 0 {
				t.Errorf("expected exactly one applied patch and no stale refs, got %+v / %+v",
					res.Applied, res.Stale)
			}
		})
	}
}
