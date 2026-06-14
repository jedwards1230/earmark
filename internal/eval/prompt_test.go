package eval

import "testing"

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
	raw := "Sure, here are the findings:\n```json\n{\"findings\":[{\"original_text\":\"there\",\"issue_type\":\"homophone\",\"confidence\":0.6}]}\n```\nHope that helps!"
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
		{"original_text":"high","issue_type":"misheard_proper_noun","confidence":1.7},
		{"original_text":"low","issue_type":"misheard_proper_noun","confidence":-0.3},
		{"original_text":"weird","issue_type":"totally_made_up","confidence":0.5}
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

func TestParseFindings_DropsEmptyOriginalText(t *testing.T) {
	raw := `{"findings":[
		{"original_text":"   ","issue_type":"run_on","confidence":0.9},
		{"original_text":"real span","issue_type":"run_on","confidence":0.9}
	]}`
	got, err := parseFindings(raw)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 finding (empty span dropped), got %d", len(got))
	}
}
