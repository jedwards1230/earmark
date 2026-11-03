package asrpilot

import (
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// prodSeg builds a production segment starting at start with one word per
// field, 0.5 s each — the parakeet shape (lowercase, no punctuation).
func prodSeg(id int, start float64, text string) db.Segment {
	s := db.Segment{ID: id, Start: start, Text: text}
	t := start
	for _, w := range strings.Fields(text) {
		s.Words = append(s.Words, db.Word{Word: w, Start: t, End: t + 0.4})
		t += 0.5
	}
	s.End = t
	return s
}

// timedPilot builds a pilot segment with word timestamps, 0.5 s per word.
func timedPilot(start float64, text string) db.PilotSegment {
	s := db.PilotSegment{Start: start, Text: text}
	t := start
	for _, w := range strings.Fields(text) {
		a, b := t, t+0.4
		s.Words = append(s.Words, db.PilotWord{Word: w, Start: &a, End: &b})
		t += 0.5
	}
	s.End = t
	return s
}

// untimedPilot is a Canary-style window: segment times only, words without.
func untimedPilot(start, end float64, text string) db.PilotSegment {
	s := db.PilotSegment{Start: start, End: end, Text: text}
	for _, w := range strings.Fields(text) {
		s.Words = append(s.Words, db.PilotWord{Word: w})
	}
	return s
}

func chunkOf(segs []db.Segment) string {
	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = s.Text
	}
	return strings.Join(parts, " ")
}

func finding(chunk, orig, repl string) Finding {
	off := strings.Index(chunk, orig) // ASCII fixtures: byte index = rune index
	return Finding{OriginalText: orig, Replacement: repl, ChunkText: chunk, ChunkSHA256: patch.ChunkHash(chunk),
		ChunkFound: true, AnchorOffset: off, AnchorOccurrence: -1}
}

func TestVote(t *testing.T) {
	segs := []db.Segment{
		prodSeg(0, 0, "it was a dark night"),
		prodSeg(1, 3, "the portrait of dorian grey hung there"),
		prodSeg(2, 7, "and lord basil looked at the cath painting"),
	}
	chunk := chunkOf(segs)
	tr := NewTrack(segs)
	pilotTimed := func(texts ...string) *Pilot {
		var ps []db.PilotSegment
		ps = append(ps, timedPilot(0, texts[0]), timedPilot(3, texts[1]), timedPilot(7, texts[2]))
		return NewPilot(ps, true)
	}

	tests := []struct {
		name       string
		f          Finding
		pilot      *Pilot
		wantVote   string
		wantHeard  string
		wantReason string
		wantMethod string
		wantAnchor string
	}{
		{
			name:     "timed pilot heard the replacement",
			f:        finding(chunk, "dorian grey", "dorian gray"),
			pilot:    pilotTimed("It was a dark night.", "The portrait of Dorian Gray hung there.", "And Lord Basil looked at the Cath painting."),
			wantVote: VoteHeardReplacement, wantHeard: "Dorian Gray", wantMethod: "time_align", wantAnchor: "offset",
		},
		{
			name:     "timed pilot heard the original",
			f:        finding(chunk, "dorian grey", "dorian gray"),
			pilot:    pilotTimed("It was a dark night.", "The portrait of Dorian Grey hung there.", "And Lord Basil looked at the Cath painting."),
			wantVote: VoteHeardOriginal, wantHeard: "Dorian Grey",
		},
		{
			name:     "pilot heard something else",
			f:        finding(chunk, "dorian grey", "dorian gray"),
			pilot:    pilotTimed("It was a dark night.", "The portrait of Morgan Blue hung there.", "And Lord Basil looked at the Cath painting."),
			wantVote: VoteNeither, wantHeard: "Morgan Blue", wantReason: "heard_other",
		},
		{
			name: "a wrong anchor_offset is recovered by Locate",
			f: func() Finding {
				f := finding(chunk, "dorian grey", "dorian gray")
				f.AnchorOffset = 3 // the judge's self-reported number is wrong
				return f
			}(),
			pilot:    pilotTimed("It was a dark night.", "The portrait of Dorian Gray hung there.", "And Lord Basil looked at the Cath painting."),
			wantVote: VoteHeardReplacement, wantAnchor: "recovered",
		},
		{
			name:     "near match with a margin",
			f:        finding(chunk, "cath", "kathe"),
			pilot:    pilotTimed("It was a dark night.", "The portrait of Dorian Gray hung there.", "And Lord Basil looked at the Kathy painting."),
			wantVote: VoteHeardReplacement, wantHeard: "Kathy",
		},
		{
			name:     "spacing: two words heard as one",
			f:        finding(chunk, "lord basil", "lordbasil"),
			pilot:    pilotTimed("It was a dark night.", "The portrait of Dorian Gray hung there.", "And Lordbasil looked at the Cath painting."),
			wantVote: VoteHeardReplacement, wantHeard: "Lordbasil",
		},
		{
			name:     "case-only correction, cased pilot",
			f:        finding(chunk, "basil", "Basil"),
			pilot:    pilotTimed("It was a dark night.", "The portrait of Dorian Gray hung there.", "And Lord Basil looked at the Cath painting."),
			wantVote: VoteHeardReplacement, wantHeard: "Basil",
		},
		{
			name:       "case-only correction, uncased pilot",
			f:          finding(chunk, "basil", "Basil"),
			pilot:      pilotTimed("it was a dark night", "the portrait of dorian gray hung there", "and lord basil looked at the cath painting"),
			wantVote:   VoteUnmappable,
			wantReason: "case_only_uncased_pilot",
		},
		{
			name: "chunk changed since the judge saw it",
			f: func() Finding {
				f := finding(chunk, "dorian grey", "dorian gray")
				f.ChunkSHA256 = patch.ChunkHash("other text")
				return f
			}(),
			pilot:    pilotTimed("a", "b", "c"),
			wantVote: VoteUnmappable, wantReason: "chunk_changed",
		},
		{
			name:     "ambiguous span",
			f:        Finding{OriginalText: "the", Replacement: "a", ChunkText: chunk, ChunkFound: true, AnchorOffset: 1, AnchorOccurrence: -1},
			pilot:    pilotTimed("a", "b", "c"),
			wantVote: VoteUnmappable, wantReason: "anchor_ambiguous",
		},
		{
			name:     "chunk missing",
			f:        Finding{OriginalText: "x", Replacement: "y"},
			pilot:    pilotTimed("a", "b", "c"),
			wantVote: VoteUnmappable, wantReason: "chunk_missing",
		},
		{
			name: "pilot did not transcribe the span's window",
			f:    finding(chunk, "dorian grey", "dorian gray"),
			pilot: NewPilot([]db.PilotSegment{
				timedPilot(7, "And Lord Basil looked at the Cath painting."),
			}, true),
			wantVote: VoteUnmappable, wantReason: "pilot_no_coverage",
		},
		{
			name: "untimed pilot: windows localised by alignment",
			f:    finding(chunk, "dorian grey", "dorian gray"),
			pilot: NewPilot([]db.PilotSegment{
				untimedPilot(0, 5, "It was a dark night. The portrait of"),
				untimedPilot(5, 12, "Dorian Gray hung there, and Lord Basil looked at the Cath painting."),
			}, false),
			wantVote: VoteHeardReplacement, wantHeard: "Dorian Gray", wantMethod: "segment_align",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Vote(tr, tt.f, tt.pilot)
			if got.Vote != tt.wantVote {
				t.Fatalf("vote = %s (reason %q, heard %q, detail %+v), want %s", got.Vote, got.Detail.Reason, got.HeardText, got.Detail, tt.wantVote)
			}
			if tt.wantHeard != "" && got.HeardText != tt.wantHeard {
				t.Errorf("heard = %q, want %q", got.HeardText, tt.wantHeard)
			}
			if tt.wantReason != "" && got.Detail.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", got.Detail.Reason, tt.wantReason)
			}
			if tt.wantMethod != "" && got.Detail.Method != tt.wantMethod {
				t.Errorf("method = %q, want %q", got.Detail.Method, tt.wantMethod)
			}
			if tt.wantAnchor != "" && got.Detail.Anchor != tt.wantAnchor {
				t.Errorf("anchor = %q, want %q", got.Detail.Anchor, tt.wantAnchor)
			}
			if got.Detail.Rule != VoteRule {
				t.Errorf("rule = %q", got.Detail.Rule)
			}
			if got.Vote != VoteUnmappable && (got.SpanStartSec == nil || got.SpanEndSec == nil) {
				t.Error("a mapped vote must carry the span's times")
			}
		})
	}
}

// TestVoteSpanTimes: the span's window comes from the production words of
// the span itself (segment 1 starts at 3 s; "dorian grey" are its 4th and
// 5th words), not the chunk window a finding stores.
func TestVoteSpanTimes(t *testing.T) {
	segs := []db.Segment{prodSeg(0, 0, "it was a dark night"), prodSeg(1, 3, "the portrait of dorian grey hung there")}
	chunk := chunkOf(segs)
	got := Vote(NewTrack(segs), finding(chunk, "dorian grey", "dorian gray"),
		NewPilot([]db.PilotSegment{timedPilot(0, "it was a dark night"), timedPilot(3, "the portrait of dorian gray hung there")}, true))
	if got.SpanStartSec == nil || *got.SpanStartSec != 4.5 || *got.SpanEndSec != 5.4 {
		t.Fatalf("span = %v..%v, want 4.5..5.4", got.SpanStartSec, got.SpanEndSec)
	}
	if got.Detail.Span != "dorian grey" || got.Detail.ProdTimes != "word" || got.Detail.Anchored != "both" {
		t.Errorf("detail = %+v", got.Detail)
	}
}

// TestVoteRuneOffsets: offsets are runes, not bytes — a multi-byte rune
// before the span must not shift it.
func TestVoteRuneOffsets(t *testing.T) {
	segs := []db.Segment{prodSeg(0, 0, "café noir"), prodSeg(1, 2, "said dorian grey softly")}
	chunk := chunkOf(segs)
	f := finding(chunk, "dorian grey", "dorian gray")
	f.AnchorOffset = len([]rune(chunk[:strings.Index(chunk, "dorian")]))
	got := Vote(NewTrack(segs), f, NewPilot([]db.PilotSegment{
		timedPilot(0, "Café noir,"), timedPilot(2, "said Dorian Gray softly."),
	}, true))
	if got.Vote != VoteHeardReplacement || got.Detail.Anchor != "offset" {
		t.Fatalf("got %s anchor %q (%+v)", got.Vote, got.Detail.Anchor, got.Detail)
	}
}

// TestVoteSegmentsMustReproduceChunk: a chunk the segments do not rebuild
// (another chunker, edited text) is unmappable, never guessed.
func TestVoteSegmentsMustReproduceChunk(t *testing.T) {
	segs := []db.Segment{prodSeg(0, 0, "the portrait of dorian grey")}
	chunk := "the portrait of dorian grey hung" // longer than the segments
	got := Vote(NewTrack(segs), finding(chunk, "dorian grey", "dorian gray"),
		NewPilot([]db.PilotSegment{timedPilot(0, "the portrait of dorian gray")}, true))
	if got.Vote != VoteUnmappable || got.Detail.Reason != "segments_do_not_reproduce_chunk" {
		t.Fatalf("got %s %q", got.Vote, got.Detail.Reason)
	}
}

// TestVoteInterpolatedProductionTimes: a segment whose words do not pair
// one-to-one with its text still places the span, by rune position.
func TestVoteInterpolatedProductionTimes(t *testing.T) {
	s := db.Segment{Start: 0, End: 10, Text: "aaaa bbbb cccc dddd eeee"} // no words
	got := Vote(NewTrack([]db.Segment{s}), finding(s.Text, "cccc", "kkkk"),
		NewPilot([]db.PilotSegment{timedPilot(0, "aaaa bbbb"), timedPilot(4, "kkkk dddd eeee")}, true))
	if got.Detail.ProdTimes != "interpolated" || got.SpanStartSec == nil || *got.SpanStartSec < 4.1 || *got.SpanStartSec > 4.2 { // 10 s × rune 10/24
		t.Fatalf("detail %+v start %v", got.Detail, got.SpanStartSec)
	}
	if got.Vote != VoteHeardReplacement {
		t.Errorf("vote %s (%+v)", got.Vote, got.Detail)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Dorian's  well-known—portrait.": "dorians well known portrait",
		"  Mr. Hyde!  ":                  "mr hyde",
		"":                               "",
		"ÉLAN":                           "élan",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAlign(t *testing.T) {
	a := strings.Fields("the portrait of dorian grey hung")
	b := strings.Fields("the portrait of dorian gray hung there")
	pairs := Align(a, b)
	var got []string
	for _, p := range pairs {
		x, y := "-", "-"
		if p.A >= 0 {
			x = a[p.A]
		}
		if p.B >= 0 {
			y = b[p.B]
		}
		got = append(got, x+"/"+y)
	}
	want := "the/the portrait/portrait of/of dorian/dorian grey/gray hung/hung -/there"
	if strings.Join(got, " ") != want {
		t.Errorf("Align = %s\nwant    %s", strings.Join(got, " "), want)
	}
	if Align(make([]string, MaxAlignTokens+1), nil) != nil {
		t.Error("Align must refuse past MaxAlignTokens")
	}
}

func TestGapOneSideAnchored(t *testing.T) {
	// The span is the window's first token: only the right side anchors.
	pn := []string{"grey", "hung"}
	qn := []string{"gray", "hung"}
	lo, hi, anchored := gap(Align(pn, qn), 0, 0, pn, qn)
	if anchored != "right" || lo != -1 || hi != 1 {
		t.Errorf("gap = %d,%d %s", lo, hi, anchored)
	}
}
