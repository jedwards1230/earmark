package eval

import (
	"strings"
	"testing"
)

func TestParseFindings_ValidJSON(t *testing.T) {
	raw := `{"findings":[
		{"original_text":"Doctor Grace","issue_type":"misheard_proper_noun","suggested_correction":"Dr. Grace","confidence":0.91},
		{"original_text":"nineteen eighty four","issue_type":"number_artifact","suggested_correction":"1984","confidence":0.5}
	]}`
	got, err := parseFindings(raw)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 findings, got %d", len(got))
	}
	if got[0].IssueType != issueMisheardProperNoun || got[0].Confidence != 0.91 {
		t.Errorf("finding[0] mismatch: %+v", got[0])
	}
	if got[1].SuggestedCorrection != "1984" {
		t.Errorf("finding[1] correction = %q", got[1].SuggestedCorrection)
	}
}

func TestParseFindings_EmptyArrayIsValid(t *testing.T) {
	got, err := parseFindings(`{"findings":[]}`)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 findings, got %d", len(got))
	}
}

func TestParseFindings_FencedJSON(t *testing.T) {
	// A model that wraps its answer in prose / markdown fences must still parse.
	raw := "Sure, here are the findings:\n```json\n{\"findings\":[{\"original_text\":\"there\",\"issue_type\":\"homophone\",\"suggested_correction\":\"their\",\"confidence\":0.6}]}\n```\nHope that helps!"
	got, err := parseFindings(raw)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if len(got) != 1 || got[0].IssueType != issueHomophone {
		t.Fatalf("fenced parse mismatch: %+v", got)
	}
}

func TestParseFindings_Malformed(t *testing.T) {
	for _, raw := range []string{
		"",                       // empty
		"the model refused",      // no JSON object at all
		`{"findings": not json}`, // a brace but invalid JSON inside
	} {
		if _, err := parseFindings(raw); err == nil {
			t.Errorf("expected error for malformed input %q", raw)
		}
	}
}

func TestParseFindings_ConfidenceClampedAndIssueCoerced(t *testing.T) {
	raw := `{"findings":[
		{"original_text":"high","issue_type":"misheard_proper_noun","suggested_correction":"hide","confidence":1.7},
		{"original_text":"low","issue_type":"misheard_proper_noun","suggested_correction":"law","confidence":-0.3},
		{"original_text":"weird","issue_type":"totally_made_up","suggested_correction":"wired","confidence":0.5}
	]}`
	got, err := parseFindings(raw)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if got[0].Confidence != 1.0 {
		t.Errorf("high confidence not clamped to 1.0: %v", got[0].Confidence)
	}
	if got[1].Confidence != 0.0 {
		t.Errorf("low confidence not clamped to 0.0: %v", got[1].Confidence)
	}
	if got[2].IssueType != issueOther {
		t.Errorf("unknown issue_type not coerced to 'other': %q", got[2].IssueType)
	}
}

func TestParseFindings_RejectsOversizedResponse(t *testing.T) {
	// A response above the size cap is rejected before unmarshal (OOM guard).
	huge := "{" + strings.Repeat("a", maxJudgeResponseBytes) + "}"
	if _, err := parseFindings(huge); err == nil {
		t.Fatal("expected error for oversized judge response")
	}
}

func TestParseFindings_DropsEmptyOriginalText(t *testing.T) {
	raw := `{"findings":[
		{"original_text":"   ","issue_type":"repeated_text","suggested_correction":"x","confidence":0.9},
		{"original_text":"real span","issue_type":"repeated_text","suggested_correction":"real","confidence":0.9}
	]}`
	got, err := parseFindings(raw)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 finding (empty span dropped), got %d", len(got))
	}
}

func TestParseFindings_DropsEmptyCorrection(t *testing.T) {
	// A finding with no (or blank) suggested_correction is the dominant noise
	// class; parseFindings drops it even when the span and type are valid.
	raw := `{"findings":[
		{"original_text":"flagged but no fix","issue_type":"misheard_word","suggested_correction":"","confidence":0.9},
		{"original_text":"no correction key at all","issue_type":"homophone","confidence":0.9},
		{"original_text":"actionable","issue_type":"misheard_word","suggested_correction":"action able","confidence":0.7}
	]}`
	got, err := parseFindings(raw)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if len(got) != 1 || got[0].OriginalText != "actionable" {
		t.Fatalf("want only the finding with a correction, got %+v", got)
	}
}

func TestNormalizeForCompare(t *testing.T) {
	// Pairs that should normalize EQUAL (restyling only — caps/punct/hyphen).
	equal := [][2]string{
		{"the French", "the french"},
		{"twenty-six", "twenty six"},
		{"information its capacity", "information: its capacity"},
		{"  Padded  Text. ", "padded text"},
	}
	for _, p := range equal {
		if a, b := normalizeForCompare(p[0]), normalizeForCompare(p[1]); a != b {
			t.Errorf("want equal: %q (%q) vs %q (%q)", p[0], a, p[1], b)
		}
	}
	// Pairs that should normalize DIFFERENT (a real word change).
	diff := [][2]string{
		{"logo graphic", "logographic"}, // split vs merged — different words
		{"the the cat", "the cat"},      // duplication
		{"unit code", "unicode"},        // mis-recognition
		{"walter brtane", "walter brattain"},
	}
	for _, p := range diff {
		if a, b := normalizeForCompare(p[0]), normalizeForCompare(p[1]); a == b {
			t.Errorf("want different: %q vs %q both normalized to %q", p[0], p[1], a)
		}
	}
}

func TestParseFindings_DropsRestylingOnlyCorrection(t *testing.T) {
	// Corrections that only re-case / re-punctuate the original are not word
	// errors and must be dropped; a genuine word change survives.
	raw := `{"findings":[
		{"original_text":"the french engineer","issue_type":"misheard_proper_noun","suggested_correction":"the French engineer","confidence":0.9},
		{"original_text":"twenty six letters","issue_type":"number_artifact","suggested_correction":"twenty-six letters","confidence":0.9},
		{"original_text":"unit code","issue_type":"misheard_word","suggested_correction":"Unicode","confidence":0.9}
	]}`
	got, err := parseFindings(raw)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if len(got) != 1 || got[0].OriginalText != "unit code" {
		t.Fatalf("want only the real word change kept, got %+v", got)
	}
}

func TestParseFindings_DroppedRunOnTypeCoercedToOther(t *testing.T) {
	// run_on left the vocabulary in taxonomy rev 2; a model that still emits it
	// must coerce to "other" rather than smuggle an off-enum value into the row.
	raw := `{"findings":[{"original_text":"the the cat","issue_type":"run_on","suggested_correction":"the cat","confidence":0.9}]}`
	got, err := parseFindings(raw)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if len(got) != 1 || got[0].IssueType != issueOther {
		t.Fatalf("want run_on coerced to other, got %+v", got)
	}
}
