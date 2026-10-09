package eval

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

		{"other type", issueOther, "kava", "java", -1, DropUnsupportedIssueType},
		{"span not in chunk", issueMisheardWord, "teeth", "earth", -1, DropAnchorMissing},
		{"ambiguous span", issueMisheardWord, "the", "a", -1, DropAnchorMissing},
		{"inside a word", issueMisheardWord, "tele", "tela", -1, DropNotWordBounded},
		{"inserted words as a substitution", issueMisheardWord, "said dogs", "he said the dogs", -1, DropNotSubstitution},
		{"deleted words as a substitution", issueMisheardWord, "ran far and", "ran and", -1, DropNotSubstitution},
		{"rewrite too long", issueMisheardWord,
			"in one thousand nine hundred thirty seven he trades it was too",
			"in the year after that she sold all of it and went", -1, DropWindowTooLong},
		{"words to digits", issueNumberArtifact, "one thousand nine hundred thirty seven", "1937", -1, DropNumberFormat},
		{"anchor checked before number format", issueMisheardWord, "1937", "nineteen thirty seven", -1, DropAnchorMissing},
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

// TestPrefilterNumberFormatBothWays: digits ↔ words is dropped in either
// direction; a misheard value in the same style is not.
func TestPrefilterNumberFormatBothWays(t *testing.T) {
	const chunk = "in 1937 there were forty two ships and one hundred and five men"
	for _, tt := range []struct {
		orig, corr, want string
	}{
		{"1937", "nineteen thirty seven", DropNumberFormat},
		{"forty two ships", "42 ships", DropNumberFormat},
		{"one hundred and five", "105", DropNumberFormat},
		{"forty two", "forty three", ""},
		{"1937", "1938", ""},
	} {
		p := parsedFinding{OriginalText: tt.orig, SuggestedCorrection: tt.corr, IssueType: issueNumberArtifact,
			Confidence: 0.9, AnchorOffset: -1, AnchorOccurrence: -1}
		if got := prefilterReason(chunk, p); got != tt.want {
			t.Errorf("%q → %q = %q, want %q", tt.orig, tt.corr, got, tt.want)
		}
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
	if !reasons[DropNumberFormat] || !reasons[DropNotSubstitution] || reasons[DropOverCap] {
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
	if got[DropNumberFormat] != 1 || got[DropNotSubstitution] != 1 || len(got) != 2 {
		t.Errorf("earmark_judge_dropped_findings = %v", got)
	}
}
