package decide

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/patch"
)

// cand builds a candidate anchored at the first occurrence of original in
// chunk, hashed against chunk, the way a fresh finding would be recorded.
func cand(id, issue, chunk, original, replacement string, conf float64) Candidate {
	off := -1
	if occ := patch.Occurrences(chunk, original); len(occ) > 0 {
		off = occ[0]
	}
	return Candidate{
		FindingID:   id,
		IssueType:   issue,
		Original:    original,
		Replacement: replacement,
		Confidence:  conf,
		Anchor:      patch.Anchor{OriginalText: original, Offset: off, Occurrence: 0},
		ChunkText:   chunk,
		ChunkHash:   patch.ChunkHash(chunk),
	}
}

func TestCheck(t *testing.T) {
	const radio = "the dish at auto sebo picked up the signal"
	tests := []struct {
		name       string
		c          Candidate
		wantPass   bool
		wantReason string
	}{
		// The judge prompt's own examples (internal/eval/prompt.go).
		{"prompt: auto sebo → Arecibo", cand("a", IssueMisheardProperNoun, radio, "auto sebo", "Arecibo", 0.9), true, ""},
		{"prompt: Holovo → Holevo", cand("a", IssueMisheardProperNoun, "the holovo bound limits it", "holovo", "Holevo", 0.9), true, ""},
		{"prompt: placenes → place names", cand("a", IssueMisheardWord, "old placenes survive", "placenes", "place names", 0.9), true, ""},
		{"prompt: the the cat → the cat", cand("a", IssueRepeatedText, "and the the cat sat", "the the cat", "the cat", 0.9), true, ""},
		{"prompt: can be can be → can be", cand("a", IssueRepeatedText, "it can be can be done", "can be can be", "can be", 0.9), true, ""},
		{"prompt: pin name → pen name", cand("a", IssueHomophone, "under a pin name", "pin name", "pen name", 0.9), true, ""},
		{"prompt: their → there", cand("a", IssueHomophone, "over their by the door", "their", "there", 0.9), true, ""},
		{"prompt: thegreycourses → thegreatcourses", cand("a", IssueMisheardWord, "from thegreycourses plus", "thegreycourses", "thegreatcourses", 0.9), true, ""},
		{"prompt: Limpel Ziv → Lempel-Ziv", cand("a", IssueMisheardWord, "limpel ziv coding", "limpel ziv", "Lempel-Ziv", 0.9), true, ""},
		// "240" is scored by its best spoken reading, "two forty" (TFRT).
		{"prompt: too forty → 240", cand("a", IssueNumberArtifact, "about too forty miles", "too forty", "240", 0.9), true, ""},
		{"number: nineteen eighty four → 1984", cand("a", IssueNumberArtifact, "in nineteen eighty four we", "nineteen eighty four", "1984", 0.9), true, ""},
		{"number: grouped thousands", cand("a", IssueNumberArtifact, "about one thousand miles", "one thousand", "1,000", 0.9), true, ""},
		{"number: grouped tens of thousands", cand("a", IssueNumberArtifact, "some twenty five thousand people", "twenty five thousand", "25,000", 0.9), true, ""},
		{"number: decimal", cand("a", IssueNumberArtifact, "about three point five liters", "three point five", "3.5", 0.9), true, ""},
		{"number: two years", cand("a", IssueNumberArtifact, "from nineteen eighty four to nineteen ninety we", "nineteen eighty four to nineteen ninety", "1984 to 1990", 0.9), true, ""},
		{"number: two paired readings", cand("a", IssueNumberArtifact, "at too forty at two oh five", "too forty at two oh five", "240 at 205", 0.9), true, ""},
		{"number: decimal digits differ", cand("a", IssueNumberArtifact, "pi is 3.15 here", "3.15", "3.50", 0.9), false, ReasonNotSoundAlike},
		{"number: grouped digits differ", cand("a", IssueNumberArtifact, "paid 1,500 dollars", "1,500", "1,550", 0.9), false, ReasonNotSoundAlike},
		{"number: plain digits differ", cand("a", IssueNumberArtifact, "x 240 y", "240", "250", 0.9), false, ReasonNotSoundAlike},
		{"number: grouped vs words", cand("a", IssueNumberArtifact, "some 10,000 people", "10,000", "10 thousand", 0.9), true, ""},
		{"number: nine spelled-out words", cand("a", IssueNumberArtifact, "exactly one million two hundred thousand three hundred forty five votes",
			"one million two hundred thousand three hundred forty five", "1,200,345", 0.9), true, ""},
		{"number: unrelated value", cand("a", IssueNumberArtifact, "about too forty miles", "too forty", "17", 0.9), false, ReasonNotSoundAlike},

		// Target exists.
		{"chunk changed", func() Candidate {
			c := cand("a", IssueHomophone, "over their by", "their", "there", 0.9)
			c.ChunkHash = patch.ChunkHash("something else")
			return c
		}(), false, ReasonChunkChanged},
		{"no chunk hash recorded", func() Candidate {
			c := cand("a", IssueHomophone, "over their by", "their", "there", 0.9)
			c.ChunkHash = ""
			return c
		}(), false, ReasonChunkChanged},
		{"anchor not found", cand("a", IssueHomophone, "over there by", "their", "there", 0.9), false, ReasonAnchorMissing},
		{"anchor ambiguous", Candidate{
			FindingID: "a", IssueType: IssueHomophone, Original: "their", Replacement: "there",
			Anchor:    patch.Anchor{OriginalText: "their", Offset: -1, Occurrence: -1},
			ChunkText: "their dog and their cat", ChunkHash: patch.ChunkHash("their dog and their cat"),
		}, false, ReasonAnchorMissing},
		{"anchor text disagrees with original", func() Candidate {
			c := cand("a", IssueHomophone, "over their by", "their", "there", 0.9)
			c.Anchor.OriginalText = "over"
			return c
		}(), false, ReasonAnchorMissing},
		{"anchor text defaults to original", func() Candidate {
			c := cand("a", IssueHomophone, "over their by", "their", "there", 0.9)
			c.Anchor.OriginalText = ""
			return c
		}(), true, ""},

		{"empty correction", cand("a", IssueHomophone, "over their by", "their", "  ", 0.9), false, ReasonEmptyCorrection},

		// Cosmetic only.
		{"cosmetic: hyphenation", cand("a", IssueMisheardWord, "play tic tac toe now", "tic tac toe", "tic-tac-toe", 0.9), false, ReasonCosmeticOnly},
		{"cosmetic: capitalisation", cand("a", IssueMisheardProperNoun, "speaks french well", "french", "French", 0.9), false, ReasonCosmeticOnly},
		{"cosmetic: punctuation", cand("a", IssueMisheardWord, "well i think so", "well i think", "Well, I think", 0.9), false, ReasonCosmeticOnly},
		{"cosmetic beats unsupported type", cand("a", IssueOther, "speaks french well", "french", "French", 0.9), false, ReasonCosmeticOnly},

		// Unsupported issue types fail closed.
		{"other", cand("a", IssueOther, "over their by", "their", "there", 0.9), false, ReasonUnsupportedIssueType},
		{"unknown type", cand("a", "run_on", "over their by", "their", "there", 0.9), false, ReasonUnsupportedIssueType},

		// Spans must be word-bounded for every issue type.
		{"mid-word: homophone inside therein", cand("a", IssueHomophone, "therein lies the rub", "there", "their", 0.9), false, ReasonNotWordBounded},
		{"mid-word: before an apostrophe", cand("a", IssueHomophone, "i won't go", "won", "one", 0.9), false, ReasonNotWordBounded},
		{"mid-word: can inside can't", cand("a", IssueMisheardWord, "you can't go", "can", "cant", 0.9), false, ReasonNotWordBounded},
		{"mid-word: after an apostrophe", cand("a", IssueMisheardWord, "you can't go", "t go", "t goo", 0.9), false, ReasonNotWordBounded},
		{"mid-word: inside a grouped number", cand("a", IssueNumberArtifact, "paid 1,500 now", "1", "one", 0.9), false, ReasonNotWordBounded},
		{"mid-word: inside a decimal", cand("a", IssueNumberArtifact, "about 3.5 liters", "5", "five", 0.9), false, ReasonNotWordBounded},
		{"apostrophe at a word end is a boundary", cand("a", IssueHomophone, "the dogs' bowl", "dogs", "dog's", 0.9), false, ReasonCosmeticOnly},
		{"mid-word: insertion inside cathedral", cand("a", IssueDroppedWord, "near the cathedral", "the cat", "the big cat", 0.9), false, ReasonNotWordBounded},

		// Substitutions that do not sound alike.
		{"window too many words", cand("a", IssueMisheardWord, "x a b c d e f g h i y", "a b c d e f g h i", "j k l m n o p q r", 0.9), false, ReasonNotSoundAlike},
		{"window too many runes", cand("a", IssueMisheardProperNoun, "x supercalifragilisticexpialidocioussupercalifragilisticexpialidociousandmoreandmore y",
			"supercalifragilisticexpialidocioussupercalifragilisticexpialidociousandmoreandmore", "supercalifragilisticexpialidocioussupercalifragilisticexpialidociousandmoreandless", 0.9), false, ReasonNotSoundAlike},
		{"not soundalike: proper noun", cand("a", IssueMisheardProperNoun, "the holevo bound", "holevo", "Shannon", 0.9), false, ReasonNotSoundAlike},
		{"not soundalike: word", cand("a", IssueMisheardWord, "the cat sat", "cat", "dog", 0.9), false, ReasonNotSoundAlike},
		{"not soundalike: pure deletion", cand("a", IssueMisheardWord, "a big red ball", "big red ball", "red ball", 0.9), false, ReasonNotSoundAlike},
		{"not soundalike: pure insertion", cand("a", IssueHomophone, "a red ball", "red ball", "big red ball", 0.9), false, ReasonNotSoundAlike},
		{"soundalike window ignores shared context", cand("a", IssueHomophone, "i went over their by the door", "over their by", "over there by", 0.9), true, ""},

		// repeated_text.
		{"repeat: three copies collapsed", cand("a", IssueRepeatedText, "no no no way", "no no no", "no", 0.9), true, ""},
		{"repeat: six-word unit", cand("a", IssueRepeatedText, "x a b c d e f a b c d e f y", "a b c d e f a b c d e f", "a b c d e f", 0.9), true, ""},
		{"repeat: seven-word unit is too long", cand("a", IssueRepeatedText, "x a b c d e f g a b c d e f g y", "a b c d e f g a b c d e f g", "a b c d e f g", 0.9), false, ReasonNotExactRepeat},
		{"repeat: not adjacent", cand("a", IssueRepeatedText, "the cat and the dog", "the cat and the", "the cat and", 0.9), false, ReasonNotExactRepeat},
		{"repeat: replacement changes a word", cand("a", IssueRepeatedText, "the the cat sat", "the the cat", "the dog", 0.9), false, ReasonNotExactRepeat},
		{"repeat: no repeat at all", cand("a", IssueRepeatedText, "a quick fox", "a quick fox", "a fox", 0.9), false, ReasonNotExactRepeat},
		{"repeat: replacement longer", cand("a", IssueRepeatedText, "the cat", "the cat", "the the cat", 0.9), false, ReasonNotExactRepeat},
		{"repeat: inside a longer word", cand("a", IssueRepeatedText, "bathe the cat", "the the", "the", 0.9), false, ReasonNotWordBounded},
		{"repeat: across a sentence break", cand("a", IssueRepeatedText, "it was the end. the end came", "the end. the end", "the end", 0.9), false, ReasonNotExactRepeat},
		{"repeat: across a semicolon", cand("a", IssueRepeatedText, "it ended; ended there", "ended; ended", "ended", 0.9), false, ReasonNotExactRepeat},
		{"repeat: across an ellipsis", cand("a", IssueRepeatedText, "so\u2026 so be it", "so\u2026 so", "so", 0.9), false, ReasonNotExactRepeat},
		{"repeat: across a question mark", cand("a", IssueRepeatedText, "why? why not", "why? why", "why", 0.9), false, ReasonNotExactRepeat},
		{"repeat: comma inside the stutter is fine", cand("a", IssueRepeatedText, "well, well, well then", "well, well, well", "well", 0.9), true, ""},

		// dropped_word.
		{"insertion: one word", cand("a", IssueDroppedWord, "i went the store", "went the store", "went to the store", 0.9), true, ""},
		{"insertion: two words", cand("a", IssueDroppedWord, "he said was late", "said was late", "said that he was late", 0.9), true, ""},
		{"insertion: three words", cand("a", IssueDroppedWord, "he said late", "said late", "said that he was late", 0.9), false, ReasonBadInsertion},
		{"insertion: changes a word", cand("a", IssueDroppedWord, "i went the store", "went the store", "go to the store", 0.9), false, ReasonBadInsertion},
		{"insertion: removes a word", cand("a", IssueDroppedWord, "i went to the store", "went to the store", "went the store", 0.9), false, ReasonBadInsertion},
		{"insertion: reorders", cand("a", IssueDroppedWord, "the cat sat", "the cat sat", "cat the sat down", 0.9), false, ReasonBadInsertion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := Check(tc.c, DefaultParams())
			if v.Pass != tc.wantPass || v.Reason != tc.wantReason {
				t.Fatalf("Check = pass %v reason %q (%s); want pass %v reason %q",
					v.Pass, v.Reason, v.Evidence, tc.wantPass, tc.wantReason)
			}
			if v.Pass && v.Reason != "" {
				t.Errorf("a passing verdict carries reason %q", v.Reason)
			}
			if v.Evidence == "" {
				t.Error("verdict has no evidence")
			}
			// Whenever the anchor resolved, the span is the original text.
			if v.Span != (patch.Span{}) {
				if got := string([]rune(tc.c.ChunkText)[v.Span.Start:v.Span.End]); got != tc.c.Original {
					t.Errorf("span %v covers %q, want %q", v.Span, got, tc.c.Original)
				}
			}
		})
	}
}

func TestCheckEvidenceCarriesScoreAndCodes(t *testing.T) {
	v := Check(cand("a", IssueMisheardProperNoun, "at auto sebo today", "auto sebo", "Arecibo", 0.9), DefaultParams())
	for _, want := range []string{"0.750", "ATSP", "ARSP", "0.67"} {
		if !strings.Contains(v.Evidence, want) {
			t.Errorf("evidence %q lacks %q", v.Evidence, want)
		}
	}
}

func TestCheckEvidenceNamesTheWinningReading(t *testing.T) {
	v := Check(cand("a", IssueNumberArtifact, "about too forty miles", "too forty", "240", 0.9), DefaultParams())
	if !strings.Contains(v.Evidence, `"240" read as "two forty" (best of 3 readings)`) {
		t.Errorf("evidence %q does not name the winning reading", v.Evidence)
	}
}

func TestCheckWindowLimitEvidence(t *testing.T) {
	v := Check(cand("a", IssueMisheardWord, "x a b c d e f g h i y", "a b c d e f g h i", "j k l m n o p q r", 0.9), DefaultParams())
	if !strings.Contains(v.Evidence, "too long to be a mishearing") {
		t.Errorf("evidence %q does not say the window is too long", v.Evidence)
	}
}

func TestCheckThreshold(t *testing.T) {
	c := cand("a", IssueMisheardProperNoun, "at auto sebo today", "auto sebo", "Arecibo", 0.9) // scores 0.75
	if v := Check(c, Params{SoundAlikeThreshold: 0.8}); v.Pass {
		t.Errorf("threshold 0.8 passed a 0.75 match: %s", v.Evidence)
	}
	if v := Check(c, Params{}); !v.Pass {
		t.Errorf("zero threshold should fall back to the default and pass: %s", v.Evidence)
	}
}

func TestDedupe(t *testing.T) {
	const chunk = "the holovo bound and the the cat sat at auto sebo"
	hash := patch.ChunkHash(chunk)
	at := func(id string, conf float64, original, replacement, issue string) Candidate {
		return cand(id, issue, chunk, original, replacement, conf)
	}
	accepted := func(id, original, correction string) patch.Patch {
		return patch.Patch{
			ID:         id,
			Anchor:     patch.Anchor{OriginalText: original, Offset: patch.Occurrences(chunk, original)[0], Occurrence: 0},
			Correction: correction,
			ChunkHash:  hash,
		}
	}

	tests := []struct {
		name     string
		cands    []Candidate
		existing []patch.Patch
		want     []string // reason per candidate, "" = pass
	}{
		{
			name: "disjoint spans all pass",
			cands: []Candidate{
				at("a", 0.9, "holovo", "Holevo", IssueMisheardProperNoun),
				at("b", 0.8, "auto sebo", "Arecibo", IssueMisheardProperNoun),
			},
			want: []string{"", ""},
		},
		{
			name: "overlap keeps the higher confidence",
			cands: []Candidate{
				at("a", 0.7, "holovo", "Holevo", IssueMisheardProperNoun),
				at("b", 0.9, "holovo bound", "Holevo bound", IssueMisheardProperNoun),
			},
			want: []string{ReasonOverlapDup, ""},
		},
		{
			name: "confidence tie goes to the lower finding id",
			cands: []Candidate{
				at("b", 0.8, "holovo", "Holevo", IssueMisheardProperNoun),
				at("a", 0.8, "holovo bound", "Holevo bound", IssueMisheardProperNoun),
			},
			want: []string{ReasonOverlapDup, ""},
		},
		{
			name: "touching spans do not overlap",
			cands: []Candidate{
				at("a", 0.9, "the the", "the", IssueRepeatedText),
				at("b", 0.8, " cat", " dog", IssueMisheardWord), // fails its own check
			},
			want: []string{"", ReasonNotSoundAlike},
		},
		{
			name: "failed candidates do not knock out passing ones",
			cands: []Candidate{
				at("a", 0.5, "holovo", "Holevo", IssueMisheardProperNoun),
				at("b", 0.99, "holovo bound", "Shannon bound", IssueMisheardProperNoun),
			},
			want: []string{"", ReasonNotSoundAlike},
		},
		{
			name: "chain: winner knocks out its neighbour, the far end survives",
			cands: []Candidate{
				at("a", 0.9, "the holovo", "the Holevo", IssueMisheardProperNoun),
				at("b", 0.8, "holovo bound", "Holevo bound", IssueMisheardProperNoun),
				at("c", 0.7, "bound and", "bound end", IssueHomophone),
			},
			want: []string{"", ReasonOverlapDup, ""},
		},
		{
			name: "overlaps an accepted correction",
			cands: []Candidate{
				at("a", 0.9, "holovo bound", "Holevo bound", IssueMisheardProperNoun),
				at("b", 0.8, "auto sebo", "Arecibo", IssueMisheardProperNoun),
			},
			existing: []patch.Patch{accepted("x", "holovo", "Holevo")},
			want:     []string{ReasonOverlapsOverlay, ""},
		},
		{
			name: "overlay loser does not suppress a lower-confidence candidate",
			cands: []Candidate{
				at("a", 0.9, "holovo bound", "Holevo bound", IssueMisheardProperNoun),
				at("b", 0.8, "bound", "bond", IssueHomophone),
			},
			existing: []patch.Patch{accepted("x", "holovo", "Holevo")},
			want:     []string{ReasonOverlapsOverlay, ""},
		},
		{
			name:  "the candidate's own row does not hide another accepted row",
			cands: []Candidate{at("x", 0.9, "holovo", "Holevo", IssueMisheardProperNoun)},
			existing: []patch.Patch{
				accepted("x", "holovo", "Holevo"),
				accepted("y", "holovo bound", "Holevo bound"),
			},
			want: []string{ReasonOverlapsOverlay},
		},
		{
			name:  "accepted rows that overlap each other still occupy their spans",
			cands: []Candidate{at("a", 0.9, "holovo", "Holevo", IssueMisheardProperNoun)},
			existing: []patch.Patch{
				accepted("y", "holovo bound", "Holevo bound"),
				accepted("z", "the holovo", "the Holevo"),
			},
			want: []string{ReasonOverlapsOverlay},
		},
		{
			name:  "an empty-correction overlay row occupies no span",
			cands: []Candidate{at("a", 0.9, "holovo", "Holevo", IssueMisheardProperNoun)},
			existing: []patch.Patch{
				accepted("y", "holovo bound", " "),
			},
			want: []string{""},
		},
		{
			name:     "the candidate's own accepted row is not a conflict",
			cands:    []Candidate{at("x", 0.9, "holovo", "Holevo", IssueMisheardProperNoun)},
			existing: []patch.Patch{accepted("x", "holovo", "Holevo")},
			want:     []string{""},
		},
		{
			name:  "a stale overlay row occupies no span",
			cands: []Candidate{at("a", 0.9, "holovo", "Holevo", IssueMisheardProperNoun)},
			existing: []patch.Patch{{
				ID:         "x",
				Anchor:     patch.Anchor{OriginalText: "holovo", Offset: 4, Occurrence: 0},
				Correction: "Holevo",
				ChunkHash:  patch.ChunkHash("an older revision"),
			}},
			want: []string{""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Rung0(tc.cands, tc.existing, DefaultParams())
			if len(got) != len(tc.cands) {
				t.Fatalf("got %d verdicts for %d candidates", len(got), len(tc.cands))
			}
			for i, v := range got {
				if v.Reason != tc.want[i] || v.Pass != (tc.want[i] == "") {
					t.Errorf("candidate %s: pass %v reason %q (%s); want reason %q",
						tc.cands[i].FindingID, v.Pass, v.Reason, v.Evidence, tc.want[i])
				}
			}
		})
	}
}

func TestDedupeOnlyComparesTheSameChunkText(t *testing.T) {
	a := cand("a", IssueHomophone, "over their by", "their", "there", 0.9)
	b := cand("b", IssueHomophone, "over their by!", "their", "there", 0.8) // same span, other chunk
	for i, v := range Rung0([]Candidate{a, b}, nil, DefaultParams()) {
		if !v.Pass {
			t.Errorf("candidate %d: %s %s", i, v.Reason, v.Evidence)
		}
	}
}

func TestDedupeDoesNotMutateInput(t *testing.T) {
	const chunk = "the holovo bound"
	cands := []Candidate{
		cand("a", IssueMisheardProperNoun, chunk, "holovo", "Holevo", 0.7),
		cand("b", IssueMisheardProperNoun, chunk, "holovo bound", "Holevo bound", 0.9),
	}
	in := []Verdict{Check(cands[0], DefaultParams()), Check(cands[1], DefaultParams())}
	out := Dedupe(cands, in, nil)
	if !in[0].Pass || out[0].Pass {
		t.Fatalf("want input untouched and output deduped; in %+v out %+v", in[0], out[0])
	}
}

func TestDedupePanicsOnMisalignedInput(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	Dedupe([]Candidate{{}}, nil, nil)
}

// TestIssueTypesMatchEval keeps this package's issue-type constants in step
// with the closed vocabulary the judge is prompted with.
func TestIssueTypesMatchEval(t *testing.T) {
	src, err := os.ReadFile("../eval/prompt.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range []string{
		IssueMisheardProperNoun, IssueMisheardWord, IssueHomophone,
		IssueNumberArtifact, IssueRepeatedText, IssueDroppedWord, IssueOther,
	} {
		if !strings.Contains(string(src), strconv.Quote(it)) {
			t.Errorf("issue type %q is not declared in internal/eval/prompt.go", it)
		}
	}
}

func TestTokenSpans(t *testing.T) {
	const s = "Don't, 1,000 o\u2019brien 3.15 1,2 21st end."
	ts := tokenSpans(s)
	var got, txt []string
	for _, tk := range ts {
		got = append(got, string([]rune(s)[tk.start:tk.end]))
		txt = append(txt, tk.text)
	}
	want := []string{"Don't", "1,000", "o\u2019brien", "3.15", "1", "2", "21", "st", "end"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("token ranges cover %q, want %q", got, want)
	}
	wantText := []string{"dont", "1000", "obrien", "3.15", "1", "2", "21", "st", "end"}
	if strings.Join(txt, "|") != strings.Join(wantText, "|") {
		t.Errorf("token texts %q, want %q", txt, wantText)
	}
	if raw := rawText(s, ts[1:2]); raw != "1,000" {
		t.Errorf("rawText = %q, want %q", raw, "1,000")
	}
	// Lower-casing that changes the rune count (İ → i̇) must not shift ranges.
	const dotted = "\u0130stanbul 1,000"
	dt := tokenSpans(dotted)
	if raw := rawText(dotted, dt[1:]); raw != "1,000" {
		t.Errorf("rawText after İ = %q, want %q", raw, "1,000")
	}
}

func TestTokens(t *testing.T) {
	tests := map[string]string{
		"Don't stop":         "dont|stop",
		"tic-tac-toe":        "tic|tac|toe",
		"  The, the  cat. ":  "the|the|cat",
		"240 miles":          "240|miles",
		"o’brien":            "obrien",
		"":                   "",
		"--":                 "",
		"Ünïcode wörds here": "ünïcode|wörds|here",
	}
	for in, want := range tests {
		if got := strings.Join(tokens(in), "|"); got != want {
			t.Errorf("tokens(%q) = %q, want %q", in, got, want)
		}
	}
}

// FuzzCheck exercises the token and span arithmetic: Check must never panic,
// a passing verdict carries no reason, and a resolved span covers the original.
func FuzzCheck(f *testing.F) {
	f.Add("and the the cat sat", "the the cat", "the cat", IssueRepeatedText)
	f.Add("i went the store", "went the store", "went to the store", IssueDroppedWord)
	f.Add("at auto sebo today", "auto sebo", "Arecibo", IssueMisheardProperNoun)
	f.Add("bathe the cat", "the the", "the", IssueRepeatedText)
	f.Add("ü ü ü", "ü ü", "ü", IssueRepeatedText)
	f.Add("about one thousand miles", "one thousand", "1,000", IssueNumberArtifact)
	f.Add("therein lies", "there", "their", IssueHomophone)
	f.Add("the end. the end", "the end. the end", "the end", IssueRepeatedText)
	f.Add("paid 1,500 now", "1,500", "1,550", IssueNumberArtifact)
	f.Add("i won't go", "won", "one", IssueHomophone)
	f.Fuzz(func(t *testing.T, chunk, original, replacement, issue string) {
		c := cand("a", issue, chunk, original, replacement, 0.5)
		v := Check(c, DefaultParams())
		if v.Pass == (v.Reason != "") {
			t.Fatalf("pass %v with reason %q", v.Pass, v.Reason)
		}
		if v.Pass {
			// Compared as runes: invalid UTF-8 decodes to U+FFFD on both sides.
			if got, want := string([]rune(chunk)[v.Span.Start:v.Span.End]), string([]rune(original)); got != want {
				t.Fatalf("span %v covers %q, want %q", v.Span, got, want)
			}
		}
		_ = Rung0([]Candidate{c, c}, nil, DefaultParams())
	})
}
