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
		// KNOWN GAP, kept as a test so a fix is a conscious change: the prompt's
		// number_artifact example scores 0.444. "240" spells out as "two hundred
		// forty" (THNTRTFRT) while "too forty" is TFRT — the spoken "two forty"
		// reading of a numeral is not modelled.
		{"prompt: too forty → 240 (known gap)", cand("a", IssueNumberArtifact, "about too forty miles", "too forty", "240", 0.9), false, ReasonNotSoundAlike},

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

		// Substitutions that do not sound alike.
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
		{"repeat: inside a longer word", cand("a", IssueRepeatedText, "bathe the cat", "the the", "the", 0.9), false, ReasonNotExactRepeat},

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
