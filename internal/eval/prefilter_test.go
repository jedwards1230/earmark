package eval

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jedwards1230/earmark/internal/patch"
)

// TestPrefilterReason pins every pre-filter: each junk class from the
// production sample is dropped with its reason, and the genuine mishearings
// next to it survive. A finding that survives here still has to pass decide.
func TestPrefilterReason(t *testing.T) {
	const chunk = "the auto sebo telescope said dogs ran far and the the cat sat " +
		"in one thousand nine hundred thirty seven he trades it was too forty miles " +
		"there pin name was kava akbar and the c vocabulary and then went home"
	tests := []struct {
		name, issue, orig, corr string
		offset                  int // -1 = unknown
		want                    string
	}{
		// Survivors: real sound-alike mishearings in each edit shape.
		{"proper noun", issueMisheardProperNoun, "auto sebo", "arecibo", -1, ""},
		{"homophone", issueHomophone, "pin name", "pen name", -1, ""},
		{"number value", issueNumberArtifact, "too forty", "two forty", -1, ""},
		{"repeat removal", issueRepeatedText, "the the cat", "the cat", -1, ""},
		{"dropped word", issueDroppedWord, "said dogs", "said the dogs", -1, ""},
		{"apostrophe word", issueHomophone, "there pin", "they're pin", -1, ""},
		// Shape-valid guesses the prompt must stop; rung 0 scores them on sound.
		{"guess passes shape", issueMisheardProperNoun, "kava akbar", "keats akbar", -1, ""},
		// Two small mishearings 11 words apart: two one-word hunks, as rung0@v2
		// scores them, not one 12-word first-to-last window.
		{"far-apart hunks", issueHomophone,
			"pin name was kava akbar and the c vocabulary and then went",
			"pen name was kava akbar and the c vocabulary and then sent", -1, ""},
		// A spelled-out number is one value: number_artifact hunks have no
		// word limit (a same-reading re-spelling is cosmetic_only at parse).
		{"long number hunk", issueNumberArtifact, "one thousand nine hundred thirty seven", "1938", -1, ""},

		{"other type", issueOther, "kava", "java", -1, DropUnsupportedIssueType},
		{"span not in chunk", issueMisheardWord, "teeth", "earth", -1, DropAnchorMissing},
		{"ambiguous span", issueMisheardWord, "the", "a", -1, DropAnchorMissing},
		{"inside a word", issueMisheardWord, "tele", "tela", -1, DropNotWordBounded},
		{"digits not in chunk", issueMisheardWord, "1937", "nineteen thirty seven", -1, DropAnchorMissing},
		{"inserted words as a substitution", issueMisheardWord, "said dogs", "he said the dogs", -1, DropNotSubstitution},
		{"deleted words as a substitution", issueMisheardWord, "ran far and", "ran and", -1, DropNotSubstitution},
		{"insertion hunk beside a substitution", issueMisheardWord, "pin name was kava", "pen name was the kava", -1, DropNotSubstitution},
		{"one long hunk among short ones", issueMisheardWord,
			"pin name was kava akbar and the c vocabulary and then went home",
			"pen name of a poet whose verse sang fields rivers hills then went home", -1, DropWindowTooLong},
		{"rewrite too long", issueMisheardWord,
			"in one thousand nine hundred thirty seven he trades it was too",
			"in the year after that she sold all her goods we were two", -1, DropWindowTooLong},
		{"hunk past the rune limit", issueNumberArtifact, "one thousand nine hundred thirty seven",
			strings.Repeat("seventy ", 11) + "seven", -1, DropWindowTooLong},
		{"dropped word that rewrites", issueDroppedWord, "said dogs", "said the cats", -1, DropBadInsertion},
		{"dropped word adding three", issueDroppedWord, "said dogs", "said that all the dogs", -1, DropBadInsertion},
		{"repeat that adds", issueRepeatedText, "the the cat", "the cat cat sat", -1, DropNotRemoval},
		{"repeat that rewrites", issueRepeatedText, "the the cat", "a cat", -1, DropNotRemoval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := parsedFinding{OriginalText: tt.orig, SuggestedCorrection: tt.corr, IssueType: tt.issue,
				Confidence: 0.9, AnchorOffset: tt.offset, AnchorOccurrence: -1}
			if got := prefilterReason(chunk, p); got != tt.want {
				t.Errorf("prefilterReason(%q → %q, %s) = %q, want %q", tt.orig, tt.corr, tt.issue, got, tt.want)
			}
		})
	}
}

// TestPrefilterUsesAnchor: a span that occurs twice is kept when the anchor
// says which copy — the same resolution patch.Locate gives decide.
func TestPrefilterUsesAnchor(t *testing.T) {
	const chunk = "the cat and the dog"
	p := parsedFinding{OriginalText: "the dog", SuggestedCorrection: "the fog", IssueType: issueMisheardWord,
		Confidence: 0.9, AnchorOffset: 12, AnchorOccurrence: 0}
	if got := prefilterReason(chunk, p); got != "" {
		t.Errorf("unique span: %q", got)
	}
	p = parsedFinding{OriginalText: "the", SuggestedCorrection: "a", IssueType: issueMisheardWord,
		Confidence: 0.9, AnchorOffset: 12, AnchorOccurrence: 1}
	if got := prefilterReason(chunk, p); got != "" {
		t.Errorf("anchored repeat: %q", got)
	}
}

// TestJudgeChunk_PrefilterBeforeCap: junk is dropped before the per-chunk cap,
// so it cannot crowd a real finding out of a capped slot; the drops are
// reported and counted in earmark_judge_dropped_findings{reason}.
func TestJudgeChunk_PrefilterBeforeCap(t *testing.T) {
	_, reader := installTestProviders(t)
	t.Setenv("EVAL_MAX_FINDINGS_PER_CHUNK", "1")
	resp := `{"findings":[
		{"original_text":"one thousand nine hundred","issue_type":"number_artifact","suggested_correction":"1900","confidence":0.99,"anchor_offset":-1,"anchor_occurrence":-1},
		{"original_text":"said dogs","issue_type":"misheard_word","suggested_correction":"he said the dogs","confidence":0.98},
		{"original_text":"auto sebo","issue_type":"misheard_proper_noun","suggested_correction":"arecibo","confidence":0.7}
	]}`
	res, err := NewJudge(&fakeChat{resp: resp}).JudgeChunk(context.Background(),
		chunkWith("the auto sebo dish said dogs barked in one thousand nine hundred"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].OriginalText != "auto sebo" {
		t.Fatalf("findings = %+v, want only the sound-alike", res.Findings)
	}
	reasons := map[string]bool{}
	for _, d := range res.Dropped {
		reasons[d.Reason] = true
	}
	if !reasons[DropCosmeticOnly] || !reasons[DropNotSubstitution] || reasons[DropOverCap] {
		t.Errorf("drop reasons = %v", reasons)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "earmark_judge_dropped_findings" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				r, _ := dp.Attributes.Value("reason")
				got[r.AsString()] += dp.Value
			}
		}
	}
	if got[DropCosmeticOnly] != 1 || got[DropNotSubstitution] != 1 || len(got) != 2 {
		t.Errorf("earmark_judge_dropped_findings = %v", got)
	}
}

// keptBy reports whether a finding anchored at the first occurrence of orig
// in chunk — the way rung 0's tests build a candidate — survives every
// proposal-time filter that mirrors rung 0: the parse-time cosmetic_only and
// prefilterReason. It returns the drop reason, or "".
func keptBy(chunk, issue, orig, corr string) string {
	off := -1
	if occ := patch.Occurrences(chunk, orig); len(occ) > 0 {
		off = occ[0]
	}
	if cosmetic(orig, corr) {
		return DropCosmeticOnly
	}
	return prefilterReason(chunk, parsedFinding{OriginalText: orig, SuggestedCorrection: corr, IssueType: issue,
		Confidence: 0.9, AnchorOffset: off, AnchorOccurrence: 0})
}

// TestPrefilterNeverStricter pins the invariant: no proposal-time filter
// rejects a finding rung0@v2 passes. The vectors are every passing case of
// internal/decide's rung-0 tests as at 2e37f08 (TestCheck, TestCheckChangeCap,
// TestCheckYearOffByOnePasses), copied verbatim — internal/eval cannot import
// the decide step. When rung 0 gains a passing vector, add it here.
func TestPrefilterNeverStricter(t *testing.T) {
	const radio = "the dish at auto sebo picked up the signal"
	type vec struct{ name, issue, chunk, orig, corr string }
	tests := []vec{
		{"prompt: auto sebo → Arecibo", issueMisheardProperNoun, radio, "auto sebo", "Arecibo"},
		{"prompt: Holovo → Holevo", issueMisheardProperNoun, "the holovo bound limits it", "holovo", "Holevo"},
		{"prompt: placenes → place names", issueMisheardWord, "old placenes survive", "placenes", "place names"},
		{"prompt: the the cat → the cat", issueRepeatedText, "and the the cat sat", "the the cat", "the cat"},
		{"prompt: can be can be → can be", issueRepeatedText, "it can be can be done", "can be can be", "can be"},
		{"prompt: pin name → pen name", issueHomophone, "under a pin name", "pin name", "pen name"},
		{"prompt: their → there", issueHomophone, "over their by the door", "their", "there"},
		{"prompt: thegreycourses → thegreatcourses", issueMisheardWord, "from thegreycourses plus", "thegreycourses", "thegreatcourses"},
		{"prompt: Limpel Ziv → Lempel-Ziv", issueMisheardWord, "limpel ziv coding", "limpel ziv", "Lempel-Ziv"},
		{"prompt: too forty → 240", issueNumberArtifact, "about too forty miles", "too forty", "240"},
		{"number: misheard word plus a re-spelling", issueNumberArtifact, "about too forty and nineteen eighty four", "too forty and nineteen eighty four", "240 and 1984"},
		{"number: two paired readings", issueNumberArtifact, "at too forty at two oh five", "too forty at two oh five", "240 at 205"},
		{"number: same numeral, homophone beside it", issueHomophone, "x the 240 feat y", "240 feat", "240 feet"},
		{"number: nine spelled-out words, one misheard", issueNumberArtifact, "exactly one million too hundred thousand three hundred forty five votes",
			"one million too hundred thousand three hundred forty five", "1,200,345"},
		{"anchor text defaults to original", issueHomophone, "over their by", "their", "there"},
		{"soundalike window ignores shared context", issueHomophone, "i went over their by the door", "over their by", "over there by"},
		{"hunks: two separate mishearings with case added", issueMisheardProperNoun, "x erugon realized that the dwarf was orig derun y",
			"erugon realized that the dwarf was orig derun", "Eragon realized that the dwarf was Oric Derun"},
		{"hunks: two mishearings far apart exceed the old window", issueMisheardWord, "x their dog ran across the long wide field to meet pin friends y",
			"their dog ran across the long wide field to meet pin friends", "there dog ran across the long wide field to meet pen friends"},
		{"hunks: merge two words into one", issueMisheardWord, "x in to the woods y", "in to the woods", "unto the woods"},
		{"hunks: split one word into two", issueMisheardWord, "old placenes survive", "placenes", "place names"},
		{"hunks: hyphenated proper noun", issueMisheardWord, "the limpel ziv coding", "the limpel ziv coding", "the Lempel-Ziv coding"},
		{"tense change passes rung 0: trades → traded", issueMisheardWord, "he trades furs", "trades", "traded"},
		{"not inflection: short words", issueHomophone, "it is red", "is", "as"},
		{"not inflection: different stems", issueHomophone, "over their by", "their", "there"},
		{"repeat: three copies collapsed", issueRepeatedText, "no no no way", "no no no", "no"},
		{"repeat: six-word unit", issueRepeatedText, "x a b c d e f a b c d e f y", "a b c d e f a b c d e f", "a b c d e f"},
		{"repeat: decimal numeral stutter", issueRepeatedText, "it was 3.5 3.5 liters", "3.5 3.5", "3.5"},
		{"repeat: comma inside the stutter is fine", issueRepeatedText, "well, well, well then", "well, well, well", "well"},
		{"insertion: one word", issueDroppedWord, "i went the store", "went the store", "went to the store"},
		{"insertion: two words", issueDroppedWord, "he said was late", "said was late", "said that he was late"},
		{"change cap: 4×2 words", issueMisheardProperNoun, "x auto sebo a auto sebo b auto sebo c auto sebo d y",
			"auto sebo a auto sebo b auto sebo c auto sebo", "Arecibo a Arecibo b Arecibo c Arecibo"},
	}
	// TestCheckChangeCap: MaxChangedWords (8) one-word hunks pass.
	var o, r []string
	for i := range 8 {
		o = append(o, "their", "w"+strconv.Itoa(i))
		r = append(r, "there", "w"+strconv.Itoa(i))
	}
	orig, repl := strings.Join(o, " "), strings.Join(r, " ")
	tests = append(tests, vec{"change cap: 8 one-word hunks", issueHomophone, "x " + orig + " y", orig, repl})
	// TestCheckYearOffByOnePasses: one side has no numeral, so rung 0 scores
	// it on sound (0.700) — a pass the model must catch, not a pre-filter.
	for _, issue := range []string{issueNumberArtifact, issueMisheardWord, issueHomophone} {
		tests = append(tests, vec{"year off by one " + issue, issue, "in nineteen thirty seven we", "nineteen thirty seven", "1938"})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keptBy(tt.chunk, tt.issue, tt.orig, tt.corr); got != "" {
				t.Errorf("%q → %q (%s) passes rung 0 but is dropped as %s", tt.orig, tt.corr, tt.issue, got)
			}
		})
	}
}

// TestCosmeticMatchesRung0: every rung-0 cosmetic_only vector (TestCheck at
// 2e37f08) is dropped as cosmetic_only here, and the near misses rung 0
// scores instead are kept.
func TestCosmeticMatchesRung0(t *testing.T) {
	cosmeticPairs := [][2]string{
		{"nineteen eighty four", "1984"},
		{"one thousand nine hundred thirty seven", "1937"},
		{"1937", "nineteen thirty seven"},
		{"one thousand", "1,000"},
		{"twenty five thousand", "25,000"},
		{"three point five", "3.5"},
		{"nineteen eighty four to nineteen ninety", "1984 to 1990"},
		{"nineteen 84", "1984"},
		{"10,000", "10 thousand"},
		{"one million two hundred thousand three hundred forty five", "1,200,345"},
		{"tic tac toe", "tic-tac-toe"},
		{"french", "French"},
		{"well i think", "Well, I think"},
		{"hairs", "hair's"},
		{"placenames", "place names"},
		{"the dwarf was oric derun", "The dwarf was Oric-Derun."},
		{"a cross a road", "across a road"},
		{"dogs", "dog's"},
		// Parse-time pairs the earlier fold also caught.
		{"the French", "the french"},
		{"twenty-six", "twenty six"},
		{"information its capacity", "information: its capacity"},
		{"  Padded  Text. ", "padded text"},
		{"logo graphic", "logographic"},
	}
	for _, p := range cosmeticPairs {
		if !cosmetic(p[0], p[1]) {
			t.Errorf("cosmetic(%q, %q) = false, want true (rung 0: cosmetic_only)", p[0], p[1])
		}
	}
	scored := [][2]string{
		{"3.5", "35"},        // a value change: rung 0's numeral check
		{"too forty", "240"}, // a mishearing rung 0 passes
		{"nineteen thirty seven", "1938"},
		{"the the cat", "the cat"}, // a deletion is never cosmetic
		{"unit code", "unicode"},
		{"walter brtane", "walter brattain"},
		{"the 240", "the 240 feet"},
	}
	for _, p := range scored {
		if cosmetic(p[0], p[1]) {
			t.Errorf("cosmetic(%q, %q) = true, want false", p[0], p[1])
		}
	}
}

// TestDiffHunksMatchesRung0 pins the alignment copied from rung0@v2: the
// first block is internal/decide's TestDiffHunks verbatim (2e37f08), so the
// same inputs give the same hunks, tie-breaks included.
func TestDiffHunksMatchesRung0(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want []hunk
	}{
		// internal/decide TestDiffHunks.
		{"", "", nil},
		{"a b c", "a b c", nil},
		{"their dog", "there dog", []hunk{{0, 1, 0, 1}}},
		{"a cross a road", "across a road", []hunk{{0, 2, 0, 1}}},
		{"said dogs", "he said the dogs", []hunk{{0, 0, 0, 1}, {1, 1, 2, 3}}},
		{"x y z", "", []hunk{{0, 3, 0, 0}}},
		{"erugon was orig derun", "eragon was oric derun", []hunk{{0, 1, 0, 1}, {2, 3, 2, 3}}},
		{"placenes", "place names", []hunk{{0, 1, 0, 2}}},
		// Tie-breaks: a kept token beats a substitution, a substitution beats
		// an insert/delete, a delete beats an insert.
		{"cat sat on the mat", "bat sat on the hat", []hunk{{0, 1, 0, 1}, {4, 5, 4, 5}}},
		{"a b", "b a", []hunk{{0, 2, 0, 2}}},
		{"a a b", "a b b", []hunk{{1, 2, 1, 2}}},
		{"a b c", "c", []hunk{{0, 2, 0, 0}}},
		{"x a", "a x", []hunk{{0, 2, 0, 2}}},
	} {
		got := diffHunks(strings.Fields(tt.a), strings.Fields(tt.b))
		if !slices.Equal(got, tt.want) {
			t.Errorf("diffHunks(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// TestTokenSpansMatchesRung0 is internal/decide's TestTokenSpans and
// TestTokens verbatim (2e37f08): the tokenizer the hunks are cut from.
func TestTokenSpansMatchesRung0(t *testing.T) {
	const s = "Don't, 1,000 o’brien 3.15 1,2 21st end."
	ts := tokenSpans(s)
	var got, txt []string
	for _, tk := range ts {
		got = append(got, string([]rune(s)[tk.start:tk.end]))
		txt = append(txt, tk.text)
	}
	want := []string{"Don't", "1,000", "o’brien", "3.15", "1", "2", "21", "st", "end"}
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
	const dotted = "İstanbul 1,000"
	if raw := rawText(dotted, tokenSpans(dotted)[1:]); raw != "1,000" {
		t.Errorf("rawText after İ = %q, want %q", raw, "1,000")
	}

	for in, want := range map[string]string{
		"Don't stop":         "dont|stop",
		"tic-tac-toe":        "tic|tac|toe",
		"  The, the  cat. ":  "the|the|cat",
		"240 miles":          "240|miles",
		"o’brien":            "obrien",
		"":                   "",
		"--":                 "",
		"Ünïcode wörds here": "ünïcode|wörds|here",
	} {
		if got := strings.Join(tokens(in), "|"); got != want {
			t.Errorf("tokens(%q) = %q, want %q", in, got, want)
		}
	}
}
