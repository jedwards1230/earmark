package patch

import (
	"slices"
	"testing"
)

func TestWordOccurrences(t *testing.T) {
	cases := []struct {
		name, text, span string
		want             []int
	}{
		{"single word", "Leto and ganema walked", "ganema", []int{9}},
		{"inside a longer word is not a match", "proganema ganemas", "ganema", nil},
		{"prefix of a word is not a match", "ganemas", "ganema", nil},
		{"case-sensitive", "Ganema said", "ganema", nil},
		{"punctuation is a boundary", "(ganema), ganema.", "ganema", []int{1, 10}},
		{"apostrophe is a boundary", "Muad'dib Muad", "Muad", []int{0, 9}},
		{"span ending in punctuation needs no right boundary", "the sietch.Then", "sietch.", []int{4}},
		{"span starting with punctuation needs no left boundary", "x...ganema y", "...ganema", []int{1}},
		{"left word edge still checked", "a...ganema y", "a...ganema", []int{0}},
		{"repeated occurrences", "the fox and the fox", "the fox", []int{0, 12}},
		{"overlapping occurrences both count", "ha ha ha", "ha ha", []int{0, 3}},
		{"underscore is a word rune", "snake_case case", "case", []int{11}},
		{"digits are word runes", "R2D2 D2", "D2", []int{5}},
		{"rune-indexed offsets", "café — ganema", "ganema", []int{7}},
		{"non-ASCII letter is a word rune", "éganema ganema", "ganema", []int{8}},
		{"empty span", "anything", "", nil},
		{"span longer than text", "ab", "abc", nil},
		{"multi-word span spanning whitespace", "the  fox the fox", "the fox", []int{9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WordOccurrences(tc.text, tc.span); !slices.Equal(got, tc.want) {
				t.Errorf("WordOccurrences(%q, %q) = %v, want %v", tc.text, tc.span, got, tc.want)
			}
		})
	}
}

func idx(i int) *int { return &i }

// chunks re-chunked under new ids: 30 s each, 0..150 s.
func reanchorChunks() []ReanchorChunk {
	return []ReanchorChunk{
		{ID: "c0", Index: 0, StartSec: 0, EndSec: 30, Text: "Leto and ganema walked to the sietch."},
		{ID: "c1", Index: 1, StartSec: 30, EndSec: 60, Text: "the fox and the fox ran."},
		{ID: "c2", Index: 2, StartSec: 60, EndSec: 90, Text: "Stilgar waited by the rock."},
		{ID: "c3", Index: 3, StartSec: 90, EndSec: 120, Text: "Later, proganema was other the word."},
		{ID: "c4", Index: 4, StartSec: 120, EndSec: 150, Text: "Stilgar spoke. Then Chani arrived."},
	}
}

func legacy(span string, chunkIndex int, start, end float64) ReanchorFinding {
	return ReanchorFinding{
		OriginalText: span, ChunkID: "dead", ChunkIndex: idx(chunkIndex),
		StartSec: start, EndSec: end, Offset: -1, Occurrence: -1,
	}
}

func TestReanchor(t *testing.T) {
	chunks := reanchorChunks()
	cases := []struct {
		name       string
		f          ReanchorFinding
		outcome    string
		chunk      string
		offset     int
		candidates int
	}{
		{"unique in the named chunk", legacy("ganema", 0, 0, 30), OutcomeUnique, "c0", 9, 1},
		{"named chunk wins even when the span also occurs elsewhere",
			legacy("Stilgar", 2, 60, 90), OutcomeUnique, "c2", 0, 1},
		{"moved: the audio window decides before the whole transcript",
			legacy("Stilgar", 1, 30, 90), OutcomeMoved, "c2", 0, 1},
		{"a match outside the judged window is a different place",
			legacy("Chani", 0, 0, 30), OutcomeNone, "", 0, 0},
		{"ambiguous in the named chunk is never guessed",
			legacy("the fox", 1, 30, 60), OutcomeAmbiguous, "", 0, 2},
		{"matches only outside the window are not candidates",
			legacy("Stilgar", 0, 0, 30), OutcomeNone, "", 0, 0},
		{"named chunk gets no preference over another window chunk",
			legacy("Stilgar", 2, 60, 150), OutcomeAmbiguous, "", 0, 2},
		{"no window: named chunk, then the whole transcript",
			legacy("Chani", 0, 0, 0), OutcomeMoved, "c4", 20, 1},
		{"no window: ambiguous across the transcript",
			legacy("Stilgar", 0, 0, 0), OutcomeAmbiguous, "", 0, 2},
		{"ambiguous within the window", legacy("Stilgar", 3, 60, 150), OutcomeAmbiguous, "", 0, 2},
		{"word boundary: substring of a word is none", legacy("gan", 0, 0, 30), OutcomeNone, "", 0, 0},
		{"case: a case-changed span is none", legacy("Ganema", 0, 0, 30), OutcomeNone, "", 0, 0},
		{"gone", legacy("zzz", 0, 0, 30), OutcomeNone, "", 0, 0},
		{"blank span", legacy("  ", 0, 0, 30), OutcomeNone, "", 0, 0},
		{"named chunk index no longer exists", legacy("rock", 9, 60, 90), OutcomeMoved, "c2", 22, 1},
		{"named chunk now covers other audio: its match is not the judged span",
			legacy("Stilgar", 4, 60, 90), OutcomeMoved, "c2", 0, 1},
		{"unchanged named chunk is the judged text: decisive despite window copies",
			ReanchorFinding{OriginalText: "Stilgar", ChunkID: "old-id", ChunkIndex: idx(2), StartSec: 60, EndSec: 150,
				ChunkHash: ChunkHash("Stilgar waited by the rock."), Offset: -1, Occurrence: -1},
			OutcomeUnique, "c2", 0, 1},
		{"unchanged named chunk: a misquoted span is not moved elsewhere",
			ReanchorFinding{OriginalText: "Chani", ChunkID: "c2", ChunkIndex: idx(2), StartSec: 60, EndSec: 90,
				ChunkHash: ChunkHash("Stilgar waited by the rock."), Offset: -1, Occurrence: -1},
			OutcomeNone, "", 0, 0},
		{"no chunk index at all", ReanchorFinding{OriginalText: "rock", Offset: -1, Occurrence: -1, StartSec: 60, EndSec: 90},
			OutcomeMoved, "c2", 22, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Reanchor(tc.f, chunks)
			if got.Outcome != tc.outcome || got.Candidates != tc.candidates {
				t.Fatalf("outcome %s (%d candidates), want %s (%d)", got.Outcome, got.Candidates, tc.outcome, tc.candidates)
			}
			if tc.chunk == "" {
				return
			}
			if got.Chunk.ID != tc.chunk || got.Offset != tc.offset {
				t.Errorf("anchored to %s@%d, want %s@%d", got.Chunk.ID, got.Offset, tc.chunk, tc.offset)
			}
			if got.Hash != ChunkHash(got.Chunk.Text) {
				t.Errorf("hash %s is not ChunkHash of the target chunk", got.Hash)
			}
			// The anchor must replay: Locate lands on exactly the span, and the
			// occurrence alone (offset lost) lands there too.
			for _, a := range []Anchor{
				{OriginalText: tc.f.OriginalText, Offset: got.Offset, Occurrence: got.Occurrence},
				{OriginalText: tc.f.OriginalText, Offset: -1, Occurrence: got.Occurrence},
			} {
				span, err := Locate(got.Chunk.Text, a)
				if err != nil || span.Start != got.Offset {
					t.Errorf("Locate(%+v) = %v, %v; want start %d", a, span, err, got.Offset)
				}
			}
		})
	}
}

// TestReanchorWindowStraddle: the window's chunks are searched as one text
// joined with " ", so an occurrence straddling a chunk boundary is a
// candidate. Alone it cannot be anchored to one chunk (none); next to another
// copy it makes the finding ambiguous instead of letting the copy win.
func TestReanchorWindowStraddle(t *testing.T) {
	chunks := []ReanchorChunk{
		{ID: "c0", Index: 0, StartSec: 0, EndSec: 10, Text: "they met Duncan"},
		{ID: "c1", Index: 1, StartSec: 10, EndSec: 20, Text: "Idaho at dawn."},
		{ID: "c2", Index: 2, StartSec: 20, EndSec: 30, Text: "Later Duncan Idaho slept."},
	}
	cases := []struct {
		name       string
		f          ReanchorFinding
		outcome    string
		candidates int
	}{
		{"lone straddler is none", legacy("Duncan Idaho", 0, 5, 15), OutcomeNone, 1},
		{"straddler plus a copy in a window chunk is ambiguous", legacy("Duncan Idaho", 0, 5, 25), OutcomeAmbiguous, 2},
		{"a copy inside one chunk alone re-anchors", legacy("Duncan Idaho", 0, 22, 28), OutcomeMoved, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Reanchor(tc.f, chunks)
			if got.Outcome != tc.outcome || got.Candidates != tc.candidates {
				t.Fatalf("outcome %s (%d candidates), want %s (%d)", got.Outcome, got.Candidates, tc.outcome, tc.candidates)
			}
		})
	}
	if got := Reanchor(legacy("Duncan Idaho", 0, 22, 28), chunks); got.Chunk.ID != "c2" || got.Offset != 6 {
		t.Errorf("anchored to %s@%d, want c2@6", got.Chunk.ID, got.Offset)
	}
}

// TestReanchorOccurrenceMatchesLocateNumbering: the word-bounded match is not
// always the first raw substring hit ("the" inside "other"), so the recorded
// occurrence must use Locate's substring numbering, not the word-match index.
func TestReanchorOccurrenceMatchesLocateNumbering(t *testing.T) {
	chunks := []ReanchorChunk{{ID: "c", Index: 0, StartSec: 0, EndSec: 30, Text: "Later, proganema was other the word."}}
	got := Reanchor(legacy("the", 0, 0, 30), chunks)
	if got.Outcome != OutcomeUnique || got.Offset != 27 {
		t.Fatalf("got %+v, want unique at rune 27", got)
	}
	if got.Occurrence != 1 {
		t.Errorf("occurrence = %d, want 1 (the substring hit inside \"other\" is occurrence 0)", got.Occurrence)
	}
}

func TestReanchorAlreadyAnchored(t *testing.T) {
	chunks := reanchorChunks()
	c2 := chunks[2]
	current := ReanchorFinding{
		OriginalText: "rock", ChunkID: "c2", ChunkIndex: idx(2), StartSec: 60, EndSec: 90,
		ChunkHash: ChunkHash(c2.Text), Offset: 22, Occurrence: 0,
	}
	if got := Reanchor(current, chunks); got.Outcome != OutcomeAnchored {
		t.Fatalf("current anchor: outcome %s, want %s", got.Outcome, OutcomeAnchored)
	}

	// Each way an anchor can be out of date sends it back through the matcher.
	for name, mut := range map[string]func(*ReanchorFinding){
		"chunk id changed":   func(f *ReanchorFinding) { f.ChunkID = "dead" },
		"chunk hash changed": func(f *ReanchorFinding) { f.ChunkHash = ChunkHash("other text") },
		"no hash recorded":   func(f *ReanchorFinding) { f.ChunkHash = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := current
			mut(&f)
			if got := Reanchor(f, chunks); got.Outcome != OutcomeUnique || got.Chunk.ID != "c2" {
				t.Errorf("outcome %s → %s, want unique in c2", got.Outcome, got.Chunk.ID)
			}
		})
	}
}

func TestReanchorPendingWhenTranscriptHasNoChunks(t *testing.T) {
	if got := Reanchor(legacy("ganema", 0, 0, 30), nil); got.Outcome != OutcomePending {
		t.Errorf("outcome %s, want %s — no chunks is not 'span gone'", got.Outcome, OutcomePending)
	}
}

// TestReanchorIsOrderIndependent: chunk order from the database must not
// change the outcome.
func TestReanchorIsOrderIndependent(t *testing.T) {
	chunks := reanchorChunks()
	rev := slices.Clone(chunks)
	slices.Reverse(rev)
	for _, f := range []ReanchorFinding{
		legacy("Chani", 0, 0, 30), legacy("Stilgar", 1, 30, 90), legacy("Stilgar", 0, 0, 30),
	} {
		a, b := Reanchor(f, chunks), Reanchor(f, rev)
		if a != b {
			t.Errorf("%q: %+v vs reversed %+v", f.OriginalText, a, b)
		}
	}
}
