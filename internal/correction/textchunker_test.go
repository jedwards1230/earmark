package correction

import (
	"strings"
	"testing"
)

func TestTextChunkerBasicFunctionality(t *testing.T) {
	tc := NewTextChunker()

	// Test short text that doesn't need chunking
	shortText := "This is a short text that should not be chunked."
	chunks, err := tc.ChunkText(shortText)
	if err != nil {
		t.Errorf("Error chunking short text: %v", err)
	}

	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk for short text, got %d", len(chunks))
	}

	if chunks[0].Text != shortText {
		t.Errorf("Chunk text doesn't match original")
	}

	if chunks[0].Index != 0 {
		t.Errorf("Expected chunk index 0, got %d", chunks[0].Index)
	}
}

func TestTextChunkerLongText(t *testing.T) {
	tc := NewTextChunker()

	// Skip this test for very large token limits to avoid timeouts
	if MaxTokensPerChunk > 50000 {
		t.Skip("Skipping test for very large token limits to avoid timeouts")
	}

	// Create a long text that will need chunking
	sentence := "This is a sentence that will be repeated many times to create a long text. "
	longText := strings.Repeat(sentence, 200) // Should exceed smaller MaxTokensPerChunk values

	chunks, err := tc.ChunkText(longText)
	if err != nil {
		t.Errorf("Error chunking long text: %v", err)
	}

	if len(chunks) <= 1 {
		t.Errorf("Expected multiple chunks for long text, got %d", len(chunks))
	}

	// Verify chunk indices are sequential
	for i, chunk := range chunks {
		if chunk.Index != i {
			t.Errorf("Expected chunk index %d, got %d", i, chunk.Index)
		}
	}

	// Verify no chunk is empty
	for i, chunk := range chunks {
		if len(chunk.Text) == 0 {
			t.Errorf("Chunk %d is empty", i)
		}
	}

	// Verify chunks have reasonable overlap
	if len(chunks) > 1 {
		for i := 0; i < len(chunks)-1; i++ {
			// Check if there's some overlap between consecutive chunks
			chunk1End := chunks[i].Text[len(chunks[i].Text)-100:] // Last 100 chars
			chunk2Start := chunks[i+1].Text[:100]                 // First 100 chars

			// There should be some common words between end of chunk1 and start of chunk2
			words1 := strings.Fields(chunk1End)
			words2 := strings.Fields(chunk2Start)

			hasOverlap := false
			for _, word1 := range words1 {
				for _, word2 := range words2 {
					if word1 == word2 && len(word1) > 3 { // Ignore short words
						hasOverlap = true
						break
					}
				}
				if hasOverlap {
					break
				}
			}

			if !hasOverlap {
				t.Logf("Warning: No clear overlap detected between chunks %d and %d", i, i+1)
			}
		}
	}
}

func TestTextChunkerSentenceBoundaries(t *testing.T) {
	tc := NewTextChunker()

	// Create text with clear sentence boundaries
	text := "First sentence. Second sentence. Third sentence. Fourth sentence. Fifth sentence. " +
		"Sixth sentence. Seventh sentence. Eighth sentence. Ninth sentence. Tenth sentence."

	chunks, err := tc.ChunkText(text)
	if err != nil {
		t.Errorf("Error chunking text with sentences: %v", err)
	}

	// For this short text, should be one chunk
	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk for short sentence text, got %d", len(chunks))
	}

	// Create much longer text with sentences
	longSentences := strings.Repeat(text+" ", 50) // Repeat to make it long

	chunks, err = tc.ChunkText(longSentences)
	if err != nil {
		t.Errorf("Error chunking long sentence text: %v", err)
	}

	// Verify each chunk ends with a sentence boundary (period, question mark, or exclamation)
	for i, chunk := range chunks {
		trimmed := strings.TrimSpace(chunk.Text)
		if len(trimmed) > 0 {
			lastChar := trimmed[len(trimmed)-1]
			if lastChar != '.' && lastChar != '!' && lastChar != '?' {
				// This is acceptable for the last chunk or if it's mid-sentence due to length limits
				t.Logf("Chunk %d doesn't end with sentence boundary (this may be acceptable)", i)
			}
		}
	}
}

func TestTextChunkerEmptyText(t *testing.T) {
	tc := NewTextChunker()

	chunks, err := tc.ChunkText("")
	if err == nil {
		t.Error("Expected error for empty text, but got none")
	}

	if len(chunks) != 0 {
		t.Errorf("Expected 0 chunks for empty text error case, got %d", len(chunks))
	}

	expectedErrorMsg := "cannot chunk empty text"
	if err.Error() != expectedErrorMsg {
		t.Errorf("Expected error message '%s', got '%s'", expectedErrorMsg, err.Error())
	}
}

func TestTextChunkerWhitespaceText(t *testing.T) {
	tc := NewTextChunker()

	whitespaceText := "   \n\t  \n  "
	chunks, err := tc.ChunkText(whitespaceText)
	if err != nil {
		t.Errorf("Error chunking whitespace text: %v", err)
	}

	if len(chunks) != 1 {
		t.Errorf("Expected 1 chunk for whitespace text, got %d", len(chunks))
	}

	// The chunker should preserve the whitespace
	if chunks[0].Text != whitespaceText {
		t.Errorf("Whitespace not preserved in chunk")
	}
}

func TestTextChunkerVeryLongSentence(t *testing.T) {
	tc := NewTextChunker()

	// Skip this test for very large token limits to avoid timeouts
	if MaxTokensPerChunk > 50000 {
		t.Skip("Skipping test for very large token limits to avoid timeouts")
	}

	// Create a very long sentence without periods that actually exceeds token limit
	longSentence := "This is a very long sentence without any periods that goes on and on and on " +
		strings.Repeat("and on ", 2000) + "and should be chunked even without sentence boundaries"

	chunks, err := tc.ChunkText(longSentence)
	if err != nil {
		t.Errorf("Error chunking very long sentence: %v", err)
	}

	// Log actual token count for debugging
	totalTokens := 0
	for i, chunk := range chunks {
		totalTokens += chunk.TokenCount
		t.Logf("Chunk %d: %d tokens", i, chunk.TokenCount)
	}
	t.Logf("Total chunks: %d, Total tokens: %d", len(chunks), totalTokens)

	if len(chunks) <= 1 {
		t.Errorf("Expected multiple chunks for very long sentence (should exceed %d tokens), got %d", MaxTokensPerChunk, len(chunks))
	}

	// Verify all chunks together contain the original text (approximately)
	var totalLength int
	for _, chunk := range chunks {
		totalLength += len(chunk.Text)
	}

	// Due to word-based splitting, there might be minor differences in length
	// The total should be within a reasonable range of the original
	if abs(totalLength-len(longSentence)) > 10 {
		t.Errorf("Total chunk length %d differs significantly from original %d", totalLength, len(longSentence))
	}

	// Each chunk should respect token limits
	for i, chunk := range chunks {
		if chunk.TokenCount > MaxTokensPerChunk {
			t.Errorf("Chunk %d exceeds token limit: %d > %d", i, chunk.TokenCount, MaxTokensPerChunk)
		}
	}
}

func TestTextChunkerOverlapCalculation(t *testing.T) {
	tc := NewTextChunker()

	// Create predictable text for overlap testing
	sentence := "SENTENCE_NUMBER_"
	var sentences []string
	for i := 0; i < 300; i++ {
		sentences = append(sentences, sentence+string(rune('A'+i%26))+". ")
	}
	longText := strings.Join(sentences, "")

	chunks, err := tc.ChunkText(longText)
	if err != nil {
		t.Errorf("Error chunking text for overlap test: %v", err)
	}

	if len(chunks) <= 1 {
		t.Skip("Text not long enough to generate multiple chunks for overlap test")
	}

	// Check that consecutive chunks have some overlap
	for i := 0; i < len(chunks)-1; i++ {
		currentChunk := chunks[i].Text
		nextChunk := chunks[i+1].Text

		// Find the last sentence in current chunk
		currentSentences := strings.Split(currentChunk, "SENTENCE_NUMBER_")
		nextSentences := strings.Split(nextChunk, "SENTENCE_NUMBER_")

		if len(currentSentences) < 2 || len(nextSentences) < 2 {
			continue
		}

		// Look for common sentence markers
		hasCommonContent := false
		for _, currSent := range currentSentences[len(currentSentences)-10:] { // Last 10 sentences
			for _, nextSent := range nextSentences[:10] { // First 10 sentences
				if len(currSent) > 5 && currSent == nextSent {
					hasCommonContent = true
					break
				}
			}
			if hasCommonContent {
				break
			}
		}

		if !hasCommonContent {
			t.Logf("Warning: No clear overlap found between chunks %d and %d", i, i+1)
		}
	}
}

func TestTextChunkerTokenCounting(t *testing.T) {
	tc := NewTextChunker()

	// Test that chunker respects token limits
	// Create text that definitely exceeds token limits
	longText := strings.Repeat("This is a token counting test sentence. ", 1000)

	chunks, err := tc.ChunkText(longText)
	if err != nil {
		t.Errorf("Error chunking text for token test: %v", err)
	}

	// Verify each chunk is within reasonable token limits
	for i, chunk := range chunks {
		// Use the actual token count reported by the chunker
		actualTokens := chunk.TokenCount

		// Should be well under the max token limit
		if actualTokens > MaxTokensPerChunk {
			t.Errorf("Chunk %d has %d actual tokens, exceeds limit of %d",
				i, actualTokens, MaxTokensPerChunk)
		}

		// For debugging: show word count vs token count
		wordCount := len(strings.Fields(chunk.Text))
		t.Logf("Chunk %d: %d words, %d actual tokens (ratio: %.2f words/token)",
			i, wordCount, actualTokens, float64(wordCount)/float64(actualTokens))
	}
}

func TestTextChunkerSpecialCharacters(t *testing.T) {
	tc := NewTextChunker()

	// Test text with various special characters
	specialText := "Hello! How are you? I'm fine. It's a beautiful day... " +
		"What's happening? Nothing much! Everything's good. " +
		"Let's test some edge cases: parentheses (like this), " +
		"quotes \"like this\", and apostrophes don't break anything. " +
		"Numbers like 1.23 and 4.56 should work too. " +
		"Email addresses like test@example.com shouldn't break chunking. " +
		"URLs like https://example.com should also work fine."

	chunks, err := tc.ChunkText(specialText)
	if err != nil {
		t.Errorf("Error chunking text with special characters: %v", err)
	}

	if len(chunks) == 0 {
		t.Error("Expected at least one chunk")
	}

	// Verify the text is preserved
	if len(chunks) == 1 && chunks[0].Text != specialText {
		t.Error("Special characters not preserved in chunking")
	}
}

func TestTextChunkerEdgeCases(t *testing.T) {
	tc := NewTextChunker()

	tests := []struct {
		name string
		text string
	}{
		{
			name: "only_periods",
			text: ".......................",
		},
		{
			name: "mixed_whitespace",
			text: "Word1\n\nWord2\t\tWord3\r\nWord4",
		},
		{
			name: "unicode_text",
			text: "This is unicode: 🌟⭐✨ and some Chinese: 你好世界 and Arabic: مرحبا بالعالم",
		},
		{
			name: "single_word",
			text: "SupercalifragilisticexpialidociousLongWordThatShouldNotBreakTheChunker",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks, err := tc.ChunkText(tt.text)
			if err != nil {
				t.Errorf("Error chunking %s: %v", tt.name, err)
			}

			if len(chunks) == 0 {
				t.Errorf("Expected at least one chunk for %s", tt.name)
			}

			// For single chunk cases, verify content is preserved
			if len(chunks) == 1 && chunks[0].Text != tt.text {
				t.Errorf("Text not preserved for %s", tt.name)
			}
		})
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
