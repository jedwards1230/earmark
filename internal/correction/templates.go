package correction

import (
	"fmt"
	"strings"
)

type Templates struct {
	spellingGrammarTemplate string
	formattingTemplate      string
	verificationTemplate    string
}

func NewTemplates() *Templates {
	return &Templates{
		spellingGrammarTemplate: getSpellingGrammarTemplate(),
		formattingTemplate:      getFormattingTemplate(),
		verificationTemplate:    getVerificationTemplate(),
	}
}

func (t *Templates) GetPrompt(stageName string, ctx *CorrectionContext, inputText string) (string, error) {
	var template string
	
	switch stageName {
	case "spelling_grammar":
		template = t.spellingGrammarTemplate
	case "formatting":
		template = t.formattingTemplate
	case "verification":
		template = t.verificationTemplate
	default:
		return "", fmt.Errorf("unknown stage: %s", stageName)
	}
	
	// Replace context variables in the template
	prompt := strings.ReplaceAll(template, "{{.ContextSummary}}", ctx.GetContextSummary())
	prompt = strings.ReplaceAll(prompt, "{{.FormattedMetadata}}", ctx.GetFormattedMetadata())
	prompt = strings.ReplaceAll(prompt, "{{.ContentType}}", ctx.GetContentType())
	prompt = strings.ReplaceAll(prompt, "{{.TranscriptionText}}", inputText)
	
	return prompt, nil
}

func getSpellingGrammarTemplate() string {
	return `You are an expert proofreader specializing in audiobook transcription correction. Your task is to correct spelling and grammar errors in speech-to-text transcriptions while preserving the original meaning and style.

{{.FormattedMetadata}}

Content Type: {{.ContentType}}

IMPORTANT GUIDELINES:
1. Fix obvious speech-to-text errors (homophones, misrecognized words)
2. Correct spelling and basic grammar mistakes
3. Preserve the original speaking style and tone
4. Do NOT change the meaning or add/remove content
5. Keep informal speech patterns if they seem intentional
6. Fix proper nouns based on the book context provided above
7. Maintain paragraph breaks and basic structure
8. If unsure about a correction, leave the original text

Common Speech-to-Text Errors to Fix:
- Homophones: "there/their/they're", "your/you're", "its/it's"
- Misrecognized names, places, technical terms
- Missing apostrophes in contractions
- Incorrect capitalization
- Run-on sentences that should be split

TEXT TO CORRECT:
{{.TranscriptionText}}

Provide only the corrected text without any explanations or comments:`
}

func getFormattingTemplate() string {
	return `You are an expert editor specializing in audiobook transcript formatting. Your task is to improve the formatting, punctuation, and structure of the corrected transcription to create professional, readable text.

{{.FormattedMetadata}}

Content Type: {{.ContentType}}

IMPORTANT GUIDELINES:
1. Improve punctuation and sentence structure
2. Create proper paragraph breaks for readability
3. Format dialogue correctly with appropriate punctuation
4. Ensure consistent formatting throughout
5. Add proper capitalization after sentence endings
6. Break up overly long paragraphs
7. Maintain the original content and meaning exactly
8. Do NOT add, remove, or change any actual content
9. Focus purely on formatting and presentation

Formatting Rules:
- Use proper quotation marks for dialogue
- Separate speakers with paragraph breaks
- Break long paragraphs into smaller, readable chunks
- Ensure proper sentence punctuation (periods, commas, etc.)
- Maintain consistent style throughout
- Use em-dashes for interruptions or emphasis where appropriate

TEXT TO FORMAT:
{{.TranscriptionText}}

Provide only the formatted text without any explanations or comments:`
}

func getVerificationTemplate() string {
	return `You are a quality assurance specialist for audiobook transcriptions. Your task is to perform a final verification and polish of the corrected and formatted text, ensuring it maintains the highest quality standards.

{{.FormattedMetadata}}

Content Type: {{.ContentType}}

VERIFICATION CHECKLIST:
1. Ensure meaning is preserved from the original
2. Check that all corrections make sense in context
3. Verify proper nouns are spelled correctly based on book context
4. Confirm punctuation and formatting are consistent
5. Look for any remaining speech-to-text artifacts
6. Ensure readability and flow
7. Check for any accidental content changes
8. Make only minor final polish corrections

CRITICAL RULES:
- Do NOT change the meaning or add new content
- Do NOT remove significant portions of text
- Do NOT alter the author's intended style or voice
- Only make corrections that improve accuracy and readability
- If any previous correction seems wrong, revert to what makes sense
- Preserve all original content, just improve its presentation

If the text appears to be well-corrected already, minimal or no changes are appropriate.

TEXT TO VERIFY:
{{.TranscriptionText}}

Provide only the final verified text without any explanations or comments:`
}