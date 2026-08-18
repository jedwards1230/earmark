package eval

// findingsResponseFormat is the server-side JSON schema the judge's reply is
// pinned to.
//
// Verified against Ollama's OpenAI-compatible endpoint (v0.32.x): it honours
// response_format {"type":"json_schema"} and returns schema-conforming output.
// That matters more here than it would for a chat feature — these replies are
// parsed into patches a human is shown and can apply to the corpus, so
// "usually well-formed" is not good enough. The schema makes the shape a
// server-side guarantee rather than a prompt-following behaviour.
//
// The enum mirrors the closed issue-type vocabulary in prompt.go. "other" is
// deliberately EXCLUDED here even though parseFindings still coerces unknown
// values to it: the schema should not offer the judge an escape hatch the
// prompt forbids.
//
// anchor_offset / anchor_occurrence are what make a finding applicable rather
// than merely readable. See internal/patch for how they are resolved.
var findingsResponseFormat = &responseFormat{
	Type: "json_schema",
	JSONSchema: &jsonSchema{
		Name:   "transcript_findings",
		Strict: true,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"findings": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"original_text": map[string]any{
								"type":        "string",
								"description": "Verbatim span copied from the input, exactly as it appears.",
							},
							"issue_type": map[string]any{
								"type": "string",
								"enum": []string{
									issueMisheardProperNoun,
									issueMisheardWord,
									issueRepeatedText,
									issueNumberArtifact,
									issueHomophone,
									issueDroppedWord,
								},
							},
							"suggested_correction": map[string]any{
								"type":        "string",
								"description": "The corrected words. Required, never empty.",
							},
							"confidence": map[string]any{
								"type":    "number",
								"minimum": 0,
								"maximum": 1,
							},
							"anchor_offset": map[string]any{
								"type":        "integer",
								"minimum":     0,
								"description": "0-based character index of original_text within the span text.",
							},
							"anchor_occurrence": map[string]any{
								"type":        "integer",
								"minimum":     0,
								"description": "0-based index among identical occurrences of original_text.",
							},
						},
						"required": []string{
							"original_text", "issue_type", "suggested_correction",
							"confidence", "anchor_offset", "anchor_occurrence",
						},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"findings"},
			"additionalProperties": false,
		},
	},
}
