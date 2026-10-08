package decide

import (
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

func seg(start, end float64, text string) db.Segment {
	return db.Segment{Start: start, End: end, Text: text}
}

// spanOf locates the first occurrence of sub in text, in runes.
func spanOf(t *testing.T, text, sub string) patch.Span {
	t.Helper()
	occ := patch.Occurrences(text, sub)
	if len(occ) == 0 {
		t.Fatalf("%q not in %q", sub, text)
	}
	return patch.Span{Start: occ[0], End: occ[0] + len([]rune(sub))}
}

func words(prefix string, n int) string {
	w := make([]string, n)
	for i := range w {
		w[i] = prefix + string(rune('a'+i%26))
	}
	return strings.Join(w, " ")
}

func TestBuildContextFromSegments(t *testing.T) {
	segs := []db.Segment{
		seg(0, 2, "previous chunk words"),
		seg(2, 4, "the dish at auto sebo picked"),
		seg(4, 6, "up the signal"),
		seg(6, 8, "next chunk words"),
	}
	chunk := ChunkWindow{Text: "the dish at auto sebo picked up the signal", StartSec: 2, EndSec: 6}

	t.Run("span in one segment", func(t *testing.T) {
		c := BuildContext(segs, chunk, spanOf(t, chunk.Text, "auto sebo"), "Arecibo")
		want := Context{
			Original:      "the dish at [[auto sebo]] picked",
			Corrected:     "the dish at [[Arecibo]] picked",
			Before:        "previous chunk words",
			After:         "up the signal",
			Reconstructed: true,
		}
		if c != want {
			t.Errorf("got  %+v\nwant %+v", c, want)
		}
	})

	t.Run("span across segments", func(t *testing.T) {
		c := BuildContext(segs, chunk, spanOf(t, chunk.Text, "picked up"), "pick up")
		if !c.Reconstructed || c.Original != "the dish at auto sebo [[picked up]] the signal" ||
			c.Before != "previous chunk words" || c.After != "next chunk words" {
			t.Errorf("got %+v", c)
		}
	})

	t.Run("first segment of the transcript has no before", func(t *testing.T) {
		ch := ChunkWindow{Text: "previous chunk words", StartSec: 0, EndSec: 2}
		c := BuildContext(segs, ch, spanOf(t, ch.Text, "chunk"), "chunks")
		if !c.Reconstructed || c.Before != "" || c.After != "the dish at auto sebo picked" {
			t.Errorf("got %+v", c)
		}
	})

	t.Run("truncates long segments to ContextWords", func(t *testing.T) {
		long := words("p", 60) + " target " + words("q", 60)
		s := []db.Segment{seg(0, 1, words("b", 50)), seg(1, 2, long), seg(2, 3, words("n", 50))}
		ch := ChunkWindow{Text: long, StartSec: 1, EndSec: 2}
		c := BuildContext(s, ch, spanOf(t, long, "target"), "targets")
		if !c.Reconstructed {
			t.Fatal("expected reconstruction")
		}
		for name, got := range map[string]int{
			"original": len(strings.Fields(c.Original)), "before": len(strings.Fields(c.Before)), "after": len(strings.Fields(c.After)),
		} {
			want := ContextWords
			if name == "original" {
				want = 2*ContextWords + 1
			}
			if got != want {
				t.Errorf("%s has %d words, want %d", name, got, want)
			}
		}
		if !strings.Contains(c.Original, " [[target]] ") {
			t.Errorf("span not marked: %q", c.Original)
		}
	})
}

func TestBuildContextFallback(t *testing.T) {
	text := words("w", 40) + " auto sebo " + words("x", 40)
	chunk := ChunkWindow{Text: text, StartSec: 10, EndSec: 20}
	span := spanOf(t, text, "auto sebo")
	cases := map[string][]db.Segment{
		"no segments":               nil,
		"segments differ from text": {seg(10, 20, strings.ToUpper(text))},
		"no segment in window":      {seg(0, 5, "early")},
		"segments too short":        {seg(10, 15, words("w", 40))},
	}
	for name, segs := range cases {
		t.Run(name, func(t *testing.T) {
			c := BuildContext(segs, chunk, span, "Arecibo")
			if c.Reconstructed || c.Before != "" || c.After != "" {
				t.Fatalf("expected fallback, got %+v", c)
			}
			if n := len(strings.Fields(c.Original)); n != 2*FallbackWords+2 {
				t.Errorf("fallback original has %d words, want %d: %q", n, 2*FallbackWords+2, c.Original)
			}
			if !strings.Contains(c.Original, " [[auto sebo]] ") || !strings.Contains(c.Corrected, " [[Arecibo]] ") {
				t.Errorf("marks missing: %q / %q", c.Original, c.Corrected)
			}
		})
	}
}

func TestBuildContextBadSpan(t *testing.T) {
	ch := ChunkWindow{Text: "abc"}
	for _, s := range []patch.Span{{Start: -1, End: 1}, {Start: 2, End: 9}, {Start: 2, End: 2}} {
		if c := BuildContext(nil, ch, s, "x"); c != (Context{}) {
			t.Errorf("span %+v: got %+v, want zero", s, c)
		}
	}
}
