package patch

import (
	"errors"
	"strings"
	"testing"
)

// TestApplyCorrections covers the single-patch behaviour that TestApply used to
// assert against the deleted Apply(), plus the multi-patch cases the overlay
// model adds. Every case from the old test is present here so deleting Apply
// moved coverage rather than removing it.
func TestApplyCorrections(t *testing.T) {
	const chunk = "the quick brown fox jumped"
	hash := ChunkHash(chunk)
	const repeated = "the fox ran and the fox sat"

	tests := []struct {
		name    string
		text    string
		patches []Patch
		want    string
		wantErr error
	}{
		{
			name: "no patches returns the text unchanged",
			text: chunk,
			want: chunk,
		},
		{
			name: "applies at the anchored span",
			text: chunk,
			patches: []Patch{{
				ID:         "a",
				Anchor:     Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0},
				Correction: "folks",
				ChunkHash:  hash,
			}},
			want: "the quick brown folks jumped",
		},
		{
			name: "edits only the anchored occurrence",
			text: repeated,
			patches: []Patch{{
				ID:         "a",
				Anchor:     Anchor{OriginalText: "fox", Offset: 20, Occurrence: 1},
				Correction: "folks",
				ChunkHash:  ChunkHash(repeated),
			}},
			want: "the fox ran and the folks sat",
		},
		{
			name: "empty hash skips verification for legacy findings",
			text: chunk,
			patches: []Patch{{
				ID:         "a",
				Anchor:     Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0},
				Correction: "folks",
			}},
			want: "the quick brown folks jumped",
		},
		{
			name: "refuses when the chunk changed underneath",
			text: "something else entirely",
			patches: []Patch{{
				ID:         "a",
				Anchor:     Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0},
				Correction: "folks",
				ChunkHash:  hash,
			}},
			wantErr: ErrChunkChanged,
		},
		{
			name: "refuses an empty correction",
			text: chunk,
			patches: []Patch{{
				ID:         "a",
				Anchor:     Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0},
				Correction: "   ",
				ChunkHash:  hash,
			}},
			wantErr: ErrEmptyCorrection,
		},
		{
			name: "refuses an unresolvable anchor",
			text: repeated,
			patches: []Patch{{
				ID:         "a",
				Anchor:     Anchor{OriginalText: "fox", Offset: -1, Occurrence: -1},
				Correction: "folks",
			}},
			wantErr: ErrAnchorAmbiguous,
		},
		{
			name: "refuses a span that is gone",
			text: chunk,
			patches: []Patch{{
				ID:         "a",
				Anchor:     Anchor{OriginalText: "badger", Offset: 0, Occurrence: 0},
				Correction: "folks",
			}},
			wantErr: ErrAnchorNotFound,
		},
		{
			name: "one bad patch fails the whole chunk (strict mode)",
			text: chunk,
			patches: []Patch{
				{ID: "a", Anchor: Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0}, Correction: "folks"},
				{ID: "b", Anchor: Anchor{OriginalText: "badger", Offset: -1, Occurrence: -1}, Correction: "otter"},
			},
			wantErr: ErrAnchorNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApplyCorrections(tc.text, tc.patches)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want error %v, got %v (result %q)", tc.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("want %q, got %q", tc.want, got)
			}
		})
	}

	// The old "reapply is refused" case. Replay always starts from the pristine
	// text, so a second pass over the ALREADY-corrected text is refused by the
	// hash check rather than compounding the edit.
	t.Run("replaying onto already-corrected text is refused", func(t *testing.T) {
		p := Patch{
			ID:         "a",
			Anchor:     Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0},
			Correction: "folks",
			ChunkHash:  hash,
		}
		once, err := ApplyCorrections(chunk, []Patch{p})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := ApplyCorrections(once, []Patch{p}); !errors.Is(err, ErrChunkChanged) {
			t.Fatalf("want ErrChunkChanged on reapply, got %v", err)
		}
	})
}

// TestApplyCorrections_ByteIdenticalRebuild is the strongest guard in the
// design. transcript_chunks is disposable: it is dropped and regenerated from
// the immutable transcript source on every embed, then the overlay is replayed
// onto it. If that replay were not deterministic, the "corrected" corpus would
// drift silently every time the worker ran — same inputs, different searchable
// text, no error anywhere.
//
// So: same source text + same patch set, replayed repeatedly and with the input
// rows in shuffled order, must produce a BYTE-identical result every time.
func TestApplyCorrections_ByteIdenticalRebuild(t *testing.T) {
	const rebuilds = 5

	tests := []struct {
		name    string
		text    string
		patches []Patch
		want    string
	}{
		{
			name: "ascii, three patches",
			text: "the auto sebo dish heard a pin name in nineteen seventy four",
			patches: []Patch{
				{ID: "p1", Anchor: Anchor{OriginalText: "auto sebo", Offset: 4, Occurrence: 0}, Correction: "arecibo"},
				{ID: "p2", Anchor: Anchor{OriginalText: "pin name", Offset: 26, Occurrence: 0}, Correction: "pen name"},
				{ID: "p3", Anchor: Anchor{OriginalText: "nineteen seventy four", Offset: 38, Occurrence: 0}, Correction: "1974"},
			},
			want: "the arecibo dish heard a pen name in 1974",
		},
		{
			// Multi-byte: every offset here is a rune index that differs from the
			// byte index, so a byte-indexed splice would corrupt the string.
			name: "multi-byte runes",
			text: "café — the naïve fox — 東京 in nineteen ninety",
			patches: []Patch{
				{ID: "p1", Anchor: Anchor{OriginalText: "fox", Offset: 17, Occurrence: 0}, Correction: "fôx"},
				{ID: "p2", Anchor: Anchor{OriginalText: "東京", Offset: 23, Occurrence: 0}, Correction: "Tokyo"},
				{ID: "p3", Anchor: Anchor{OriginalText: "nineteen ninety", Offset: 29, Occurrence: 0}, Correction: "1990"},
			},
			want: "café — the naïve fôx — Tokyo in 1990",
		},
		{
			// Identical spans distinguished only by occurrence — the ordering
			// tiebreaks have to be total for this to be reproducible.
			name: "repeated spans selected by occurrence",
			text: "the fox and the fox and the fox",
			patches: []Patch{
				{ID: "p1", Anchor: Anchor{OriginalText: "fox", Offset: -1, Occurrence: 0}, Correction: "wolf"},
				{ID: "p2", Anchor: Anchor{OriginalText: "fox", Offset: -1, Occurrence: 2}, Correction: "badger"},
			},
			want: "the wolf and the fox and the badger",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first, err := ApplyCorrections(tc.text, tc.patches)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if first != tc.want {
				t.Fatalf("want %q, got %q", tc.want, first)
			}

			for i := 1; i < rebuilds; i++ {
				got, err := ApplyCorrections(tc.text, tc.patches)
				if err != nil {
					t.Fatalf("rebuild %d: unexpected error: %v", i, err)
				}
				if got != first {
					t.Fatalf("rebuild %d is not byte-identical:\n first: %q\n   got: %q", i, first, got)
				}
			}

			// Row order out of the database must not matter. Every permutation of
			// the patch list must land on the same bytes.
			for i, perm := range permutations(tc.patches) {
				got, err := ApplyCorrections(tc.text, perm)
				if err != nil {
					t.Fatalf("permutation %d: unexpected error: %v", i, err)
				}
				if got != first {
					t.Fatalf("permutation %d changed the result:\n first: %q\n   got: %q", i, first, got)
				}
			}

			// The source text is never mutated — replay is a pure function.
			if _, err := ApplyCorrections(tc.text, nil); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestApplyCorrections_TwoPatchesInOneChunk pins the descending-offset rule.
//
// The first (left-most) replacement here is LONGER than what it replaces, so
// applying left-to-right would shift every later span right by the length delta
// and splice the second correction into the wrong place — producing plausible
// text and no error. The naive sub-test below spells out exactly what that
// corruption looks like, so the regression is legible rather than folded into a
// "want/got" mismatch.
func TestApplyCorrections_TwoPatchesInOneChunk(t *testing.T) {
	const text = "the auto sebo dish heard a pin name"
	patches := []Patch{
		// "auto sebo" (9 runes) -> "arecibo radio" (13 runes): +4.
		{ID: "p1", Anchor: Anchor{OriginalText: "auto sebo", Offset: 4, Occurrence: 0}, Correction: "arecibo radio"},
		{ID: "p2", Anchor: Anchor{OriginalText: "pin name", Offset: 27, Occurrence: 0}, Correction: "pen name"},
	}
	const want = "the arecibo radio dish heard a pen name"

	got, err := ApplyCorrections(text, patches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}

	t.Run("naive ascending application would corrupt the chunk", func(t *testing.T) {
		// Deliberately WRONG implementation: resolve once, then splice in
		// ascending order without re-basing the later offsets.
		naive := []rune(text)
		spans := make([]Span, 0, len(patches))
		for _, p := range patches {
			s, err := Locate(text, p.Anchor)
			if err != nil {
				t.Fatalf("locate %s: %v", p.ID, err)
			}
			spans = append(spans, s)
		}
		for i, p := range patches { // ascending: p1 then p2
			out := make([]rune, 0, len(naive))
			out = append(out, naive[:spans[i].Start]...)
			out = append(out, []rune(p.Correction)...)
			out = append(out, naive[spans[i].End:]...)
			naive = out
		}

		if string(naive) == want {
			t.Fatal("the naive path produced the correct string — this test no longer " +
				"demonstrates why descending order is required")
		}
		// The concrete corruption: the first replacement grew by 4 runes, so the
		// second splice lands 4 runes early — it eats the tail of "heard" and
		// leaves a duplicated "name" behind.
		const corrupted = "the arecibo radio dish hearpen namename"
		if string(naive) != corrupted {
			t.Errorf("naive corruption changed shape:\n want: %q\n  got: %q", corrupted, string(naive))
		}
	})
}

// TestApplyCorrections_RejectsOverlap: two corrections editing the same
// characters have no well-defined composition, so strict mode refuses both
// rather than silently letting whichever sorted first win.
func TestApplyCorrections_RejectsOverlap(t *testing.T) {
	const text = "the quick brown fox jumped"

	tests := []struct {
		name    string
		patches []Patch
	}{
		{
			name: "partially overlapping spans",
			patches: []Patch{
				{ID: "p1", Anchor: Anchor{OriginalText: "quick brown", Offset: 4, Occurrence: 0}, Correction: "slow grey"},
				{ID: "p2", Anchor: Anchor{OriginalText: "brown fox", Offset: 10, Occurrence: 0}, Correction: "brown dog"},
			},
		},
		{
			name: "identical spans",
			patches: []Patch{
				{ID: "p1", Anchor: Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0}, Correction: "folks"},
				{ID: "p2", Anchor: Anchor{OriginalText: "fox", Offset: 16, Occurrence: 0}, Correction: "ox"},
			},
		},
		{
			name: "one span contained in another",
			patches: []Patch{
				{ID: "p1", Anchor: Anchor{OriginalText: "quick brown fox", Offset: 4, Occurrence: 0}, Correction: "dog"},
				{ID: "p2", Anchor: Anchor{OriginalText: "brown", Offset: 10, Occurrence: 0}, Correction: "beige"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyCorrections(text, tc.patches)
			if !errors.Is(err, ErrOverlappingPatches) {
				t.Fatalf("want ErrOverlappingPatches, got %v", err)
			}
			// Both offenders must be named — a human has to know which two rows
			// to reconcile.
			if !strings.Contains(err.Error(), "p1") || !strings.Contains(err.Error(), "p2") {
				t.Errorf("error must identify both patches, got: %v", err)
			}
		})
	}

	t.Run("adjacent but non-overlapping spans are fine", func(t *testing.T) {
		got, err := ApplyCorrections(text, []Patch{
			{ID: "p1", Anchor: Anchor{OriginalText: "quick ", Offset: 4, Occurrence: 0}, Correction: "slow "},
			{ID: "p2", Anchor: Anchor{OriginalText: "brown", Offset: 10, Occurrence: 0}, Correction: "beige"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "the slow beige fox jumped"; got != want {
			t.Errorf("want %q, got %q", want, got)
		}
	})
}

// TestApplyCorrections_StaleOnHashMismatch: a finding describes one specific
// revision of a chunk. If the regenerated chunk no longer hashes to it, the
// judge never reviewed this text and the patch must not land.
func TestApplyCorrections_StaleOnHashMismatch(t *testing.T) {
	const judged = "the auto sebo dish"
	const regenerated = "the auto sebo dish, rebuilt with different segmentation"

	p := Patch{
		ID:         "p1",
		Anchor:     Anchor{OriginalText: "auto sebo", Offset: 4, Occurrence: 0},
		Correction: "arecibo",
		ChunkHash:  ChunkHash(judged),
	}

	// The anchor still resolves in the new text — the hash is the only thing
	// stopping the edit, which is precisely why it exists.
	if _, err := Locate(regenerated, p.Anchor); err != nil {
		t.Fatalf("premise broken: the anchor should still resolve, got %v", err)
	}

	if _, err := ApplyCorrections(regenerated, []Patch{p}); !errors.Is(err, ErrChunkChanged) {
		t.Fatalf("want ErrChunkChanged, got %v", err)
	}

	res := Replay(regenerated, []Patch{p})
	if res.Text != regenerated {
		t.Errorf("stale patch must leave text untouched, got %q", res.Text)
	}
	if len(res.Applied) != 0 {
		t.Errorf("nothing should be applied, got %+v", res.Applied)
	}
	if len(res.Stale) != 1 || res.Stale[0].Reason != StaleReasonChunkChanged {
		t.Fatalf("want one chunk_changed stale ref, got %+v", res.Stale)
	}
}

// TestReplay_QuarantinesInsteadOfFailing: the projection layer cannot afford
// strict mode. One unusable correction must not cost a chunk its other,
// perfectly good corrections — but it must not vanish either, or the row would
// sit in "accepted" forever while never appearing in the text.
func TestReplay_QuarantinesInsteadOfFailing(t *testing.T) {
	const text = "the auto sebo dish and the fox and the fox"

	tests := []struct {
		name        string
		patches     []Patch
		wantText    string
		wantApplied []string
		wantStale   map[string]string // id -> reason
	}{
		{
			name: "good patch applied, unresolvable patch quarantined",
			patches: []Patch{
				{ID: "good", Anchor: Anchor{OriginalText: "auto sebo", Offset: 4, Occurrence: 0}, Correction: "arecibo"},
				{ID: "ambiguous", Anchor: Anchor{OriginalText: "fox", Offset: -1, Occurrence: -1}, Correction: "wolf"},
			},
			wantText:    "the arecibo dish and the fox and the fox",
			wantApplied: []string{"good"},
			wantStale:   map[string]string{"ambiguous": StaleReasonAnchorAmbiguous},
		},
		{
			name: "missing span",
			patches: []Patch{
				{ID: "good", Anchor: Anchor{OriginalText: "auto sebo", Offset: 4, Occurrence: 0}, Correction: "arecibo"},
				{ID: "gone", Anchor: Anchor{OriginalText: "badger", Offset: -1, Occurrence: -1}, Correction: "otter"},
			},
			wantText:    "the arecibo dish and the fox and the fox",
			wantApplied: []string{"good"},
			wantStale:   map[string]string{"gone": StaleReasonAnchorNotFound},
		},
		{
			name: "empty correction",
			patches: []Patch{
				{ID: "good", Anchor: Anchor{OriginalText: "auto sebo", Offset: 4, Occurrence: 0}, Correction: "arecibo"},
				{ID: "blank", Anchor: Anchor{OriginalText: "dish", Offset: 14, Occurrence: 0}, Correction: "  \t "},
			},
			wantText:    "the arecibo dish and the fox and the fox",
			wantApplied: []string{"good"},
			wantStale:   map[string]string{"blank": StaleReasonEmptyCorrection},
		},
		{
			name: "hash mismatch on one patch only",
			patches: []Patch{
				{ID: "good", Anchor: Anchor{OriginalText: "auto sebo", Offset: 4, Occurrence: 0}, Correction: "arecibo"},
				{ID: "old", Anchor: Anchor{OriginalText: "dish", Offset: 14, Occurrence: 0}, Correction: "telescope",
					ChunkHash: ChunkHash("some earlier revision")},
			},
			wantText:    "the arecibo dish and the fox and the fox",
			wantApplied: []string{"good"},
			wantStale:   map[string]string{"old": StaleReasonChunkChanged},
		},
		{
			name: "overlapping pair quarantines BOTH, unrelated patch still lands",
			patches: []Patch{
				{ID: "ok", Anchor: Anchor{OriginalText: "fox", Offset: -1, Occurrence: 1}, Correction: "wolf"},
				{ID: "over1", Anchor: Anchor{OriginalText: "auto sebo", Offset: 4, Occurrence: 0}, Correction: "arecibo"},
				{ID: "over2", Anchor: Anchor{OriginalText: "sebo dish", Offset: 9, Occurrence: 0}, Correction: "cibo dish"},
			},
			wantText:    "the auto sebo dish and the fox and the wolf",
			wantApplied: []string{"ok"},
			wantStale: map[string]string{
				"over1": StaleReasonOverlapping,
				"over2": StaleReasonOverlapping,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := Replay(text, tc.patches)

			if res.Text != tc.wantText {
				t.Errorf("text:\n want %q\n  got %q", tc.wantText, res.Text)
			}

			gotApplied := make([]string, 0, len(res.Applied))
			for _, a := range res.Applied {
				gotApplied = append(gotApplied, a.ID)
			}
			if strings.Join(gotApplied, ",") != strings.Join(tc.wantApplied, ",") {
				t.Errorf("applied: want %v, got %v", tc.wantApplied, gotApplied)
			}

			if len(res.Stale) != len(tc.wantStale) {
				t.Fatalf("stale: want %d refs %v, got %+v", len(tc.wantStale), tc.wantStale, res.Stale)
			}
			for _, s := range res.Stale {
				want, ok := tc.wantStale[s.ID]
				if !ok {
					t.Errorf("unexpected stale ref %+v", s)
					continue
				}
				if s.Reason != want {
					t.Errorf("stale %s: want reason %q, got %q", s.ID, want, s.Reason)
				}
			}

			// The partition must be total: every input patch lands in exactly one
			// of Applied/Stale. A silently dropped patch is the failure mode this
			// whole structure exists to prevent.
			if got, want := len(res.Applied)+len(res.Stale), len(tc.patches); got != want {
				t.Errorf("applied+stale = %d, want %d (a patch was silently dropped)", got, want)
			}
		})
	}
}

// TestReplay_RecordsSpanLevelBeforeAfter pins the narrowed meaning of the
// applied_before_text / applied_after_text columns: they hold the SPAN, not the
// whole chunk. Whole-chunk before/after is ill-defined once a chunk carries
// several patches.
func TestReplay_RecordsSpanLevelBeforeAfter(t *testing.T) {
	const text = "the auto sebo dish heard a pin name"
	res := Replay(text, []Patch{
		{ID: "p1", Anchor: Anchor{OriginalText: "auto sebo", Offset: 4, Occurrence: 0}, Correction: "arecibo"},
		{ID: "p2", Anchor: Anchor{OriginalText: "pin name", Offset: 27, Occurrence: 0}, Correction: "pen name"},
	})
	if len(res.Applied) != 2 {
		t.Fatalf("want 2 applied, got %+v", res.Applied)
	}
	for _, a := range res.Applied {
		if strings.Contains(a.Before, "dish") {
			t.Errorf("Before must be the span, not the whole chunk: %q", a.Before)
		}
		if got := string([]rune(text)[a.Span.Start:a.Span.End]); got != a.Before {
			t.Errorf("Before %q does not match the recorded span %+v (%q)", a.Before, a.Span, got)
		}
	}
}

// permutations returns every ordering of the input slice. Small n only — the
// patch sets under test are 2–3 elements.
func permutations(in []Patch) [][]Patch {
	if len(in) <= 1 {
		return [][]Patch{append([]Patch(nil), in...)}
	}
	var out [][]Patch
	for i := range in {
		rest := make([]Patch, 0, len(in)-1)
		rest = append(rest, in[:i]...)
		rest = append(rest, in[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]Patch{in[i]}, p...))
		}
	}
	return out
}
