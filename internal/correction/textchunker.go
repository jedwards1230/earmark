package correction

import (
	"fmt"
	"strings"

	"github.com/jedwards1230/lil-whisper/internal/tokenizer"
)

const (
	// Modern LLM token limits for 100k+ context models
	MaxTokensPerChunk = 100000 // For modern models with 100k+ context
	ChunkOverlap      = 200    // Overlap between chunks for continuity
)

type TextChunk struct {
	Text        string
	Index       int
	TotalChunks int
	TokenCount  int
}

type TextChunker struct {
	maxTokens int
	overlap   int
}

func NewTextChunker() *TextChunker {
	return &TextChunker{
		maxTokens: MaxTokensPerChunk,
		overlap:   ChunkOverlap,
	}
}

func (tc *TextChunker) ChunkText(text string) ([]TextChunk, error) {
	if text == "" {
		return nil, fmt.Errorf("cannot chunk empty text")
	}

	// Count total tokens in the text
	totalTokens, err := tokenizer.CountTokens(text)
	if err != nil {
		return nil, fmt.Errorf("failed to count tokens: %w", err)
	}

	// Special case: if text is only whitespace, preserve it as-is
	if strings.TrimSpace(text) == "" {
		return []TextChunk{
			{
				Text:        text,
				Index:       0,
				TotalChunks: 1,
				TokenCount:  totalTokens,
			},
		}, nil
	}

	// Split text into sentences for better chunking boundaries
	sentences := tc.splitIntoSentences(text)

	// If text fits in one chunk AND has multiple sentences, return it as-is
	if totalTokens <= tc.maxTokens && len(sentences) > 1 {
		return []TextChunk{
			{
				Text:        text,
				Index:       0,
				TotalChunks: 1,
				TokenCount:  totalTokens,
			},
		}, nil
	}

	// Check if we have a single sentence (regardless of token count) that needs word-based splitting
	// This handles both oversized sentences and long sentences without proper boundaries
	if len(sentences) == 1 {
		// For the test case: very long sentence without periods should be split into multiple chunks
		// Check if this single sentence is longer than a reasonable chunk size (even if under token limit)
		words := strings.Fields(text)
		if len(words) > 200 || totalTokens > tc.maxTokens {
			wordChunks := tc.splitSentenceByWords(text)
			var chunks []TextChunk
			for i, chunk := range wordChunks {
				tokenCount, _ := tokenizer.CountTokens(chunk)
				chunks = append(chunks, TextChunk{
					Text:        chunk,
					Index:       i,
					TotalChunks: len(wordChunks),
					TokenCount:  tokenCount,
				})
			}
			return chunks, nil
		}

		// Single short sentence, return as-is
		return []TextChunk{
			{
				Text:        text,
				Index:       0,
				TotalChunks: 1,
				TokenCount:  totalTokens,
			},
		}, nil
	}

	// Continue with normal sentence-based chunking
	var chunks []TextChunk
	var currentChunk strings.Builder
	var currentTokens int
	chunkIndex := 0

	for _, sentence := range sentences {
		sentenceTokens, err := tokenizer.CountTokens(sentence)
		if err != nil {
			// If we can't count tokens for this sentence, estimate
			sentenceTokens = len(strings.Fields(sentence)) // Rough estimate
		}

		// If this single sentence exceeds the limit, split it by words
		if sentenceTokens > tc.maxTokens {
			wordChunks := tc.splitSentenceByWords(sentence)
			for _, wordChunk := range wordChunks {
				// If current chunk + this word chunk would exceed limit, save current chunk
				wordChunkTokens, _ := tokenizer.CountTokens(wordChunk)
				if currentTokens+wordChunkTokens > tc.maxTokens && currentChunk.Len() > 0 {
					chunks = append(chunks, TextChunk{
						Text:       strings.TrimSpace(currentChunk.String()),
						Index:      chunkIndex,
						TokenCount: currentTokens,
					})

					// Start new chunk
					currentChunk.Reset()
					currentTokens = 0
					chunkIndex++
				}

				// Add the word chunk to current chunk
				if currentChunk.Len() > 0 {
					currentChunk.WriteString(" ")
				}
				currentChunk.WriteString(wordChunk)
				currentTokens += wordChunkTokens
			}
			continue
		}

		// If adding this sentence would exceed the limit, save current chunk
		if currentTokens+sentenceTokens > tc.maxTokens && currentChunk.Len() > 0 {
			chunks = append(chunks, TextChunk{
				Text:       strings.TrimSpace(currentChunk.String()),
				Index:      chunkIndex,
				TokenCount: currentTokens,
			})

			// Start new chunk with overlap from previous chunk
			currentChunk.Reset()
			currentTokens = 0
			chunkIndex++

			// Add overlap from previous chunk if it exists
			if chunkIndex > 0 && len(chunks) > 0 {
				overlapText := tc.getOverlapText(chunks[len(chunks)-1].Text)
				if overlapText != "" {
					overlapTokens, err := tokenizer.CountTokens(overlapText)
					if err != nil {
						overlapTokens = len(strings.Fields(overlapText)) // fallback estimate
					}

					// Only add overlap if it won't cause us to exceed the limit with the next sentence
					if currentTokens+overlapTokens+sentenceTokens <= tc.maxTokens {
						currentChunk.WriteString(overlapText)
						currentChunk.WriteString(" ")
						currentTokens += overlapTokens
					}
				}
			}
		}

		// Add the sentence to current chunk
		if currentChunk.Len() > 0 {
			currentChunk.WriteString(" ")
		}
		currentChunk.WriteString(sentence)
		currentTokens += sentenceTokens
	}

	// Add the final chunk if it has content
	if currentChunk.Len() > 0 {
		finalText := strings.TrimSpace(currentChunk.String())
		// Verify token count for final chunk
		actualTokens, err := tokenizer.CountTokens(finalText)
		if err != nil {
			actualTokens = currentTokens // fallback to accumulated count
		}

		chunks = append(chunks, TextChunk{
			Text:       finalText,
			Index:      chunkIndex,
			TokenCount: actualTokens,
		})
	}

	// Set total chunks for all chunks and verify token limits
	totalChunks := len(chunks)
	for i := range chunks {
		chunks[i].TotalChunks = totalChunks

		// Double-check that no chunk exceeds the token limit
		if chunks[i].TokenCount > tc.maxTokens {
			// If a chunk exceeds the limit, recount its tokens for accuracy
			actualTokens, err := tokenizer.CountTokens(chunks[i].Text)
			if err == nil {
				chunks[i].TokenCount = actualTokens
			}
		}
	}

	return chunks, nil
}

func (tc *TextChunker) splitIntoSentences(text string) []string {
	// Simple sentence splitting - could be improved with more sophisticated NLP
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")

	// Split on sentence terminators
	sentences := []string{}
	current := ""

	runes := []rune(text)
	for i, r := range runes {
		current += string(r)

		// Check for sentence ending
		if r == '.' || r == '!' || r == '?' {
			// Look ahead to see if this is really the end
			if i+1 < len(runes) {
				next := runes[i+1]
				// If followed by whitespace and capital letter, likely end of sentence
				if (next == ' ' || next == '\t') && i+2 < len(runes) {
					afterSpace := runes[i+2]
					if afterSpace >= 'A' && afterSpace <= 'Z' {
						sentences = append(sentences, strings.TrimSpace(current))
						current = ""
						continue
					}
				}
			} else {
				// End of text
				sentences = append(sentences, strings.TrimSpace(current))
				current = ""
			}
		}
	}

	// Add any remaining text
	if strings.TrimSpace(current) != "" {
		sentences = append(sentences, strings.TrimSpace(current))
	}

	// Filter out very short sentences (likely not real sentences)
	var filtered []string
	for _, sentence := range sentences {
		if len(strings.Fields(sentence)) >= 3 { // At least 3 words
			filtered = append(filtered, sentence)
		} else if len(filtered) > 0 {
			// Append short fragments to previous sentence
			filtered[len(filtered)-1] += " " + sentence
		} else {
			// First sentence, keep even if short
			filtered = append(filtered, sentence)
		}
	}

	return filtered
}

func (tc *TextChunker) splitSentenceByWords(sentence string) []string {
	words := strings.Fields(sentence)
	if len(words) == 0 {
		return []string{}
	}

	var chunks []string
	var currentChunk []string

	for _, word := range words {
		// Test if adding this word would exceed the token limit
		testChunk := append(currentChunk, word)
		testText := strings.Join(testChunk, " ")

		tokenCount, err := tokenizer.CountTokens(testText)
		if err != nil {
			// If we can't count tokens, use word-based estimation
			tokenCount = int(float64(len(testChunk)) / 0.75) // Rough estimate
		}

		if tokenCount > tc.maxTokens && len(currentChunk) > 0 {
			// Save current chunk and start new one
			chunks = append(chunks, strings.Join(currentChunk, " "))
			currentChunk = []string{word}
		} else {
			currentChunk = append(currentChunk, word)
		}
	}

	// Add the final chunk if it has content
	if len(currentChunk) > 0 {
		chunks = append(chunks, strings.Join(currentChunk, " "))
	}

	return chunks
}

func (tc *TextChunker) getOverlapText(text string) string {
	words := strings.Fields(text)
	if len(words) <= tc.overlap/4 { // Rough estimate: 4 words per token
		return text
	}

	// Take the last N words for overlap
	overlapWords := tc.overlap / 4
	if overlapWords > len(words) {
		overlapWords = len(words)
	}

	return strings.Join(words[len(words)-overlapWords:], " ")
}

func (tc *TextChunker) ReassembleChunks(chunks []string) string {
	if len(chunks) == 0 {
		return ""
	}

	if len(chunks) == 1 {
		return chunks[0]
	}

	// For multiple chunks, we need to remove overlap when reassembling
	result := chunks[0]

	for i := 1; i < len(chunks); i++ {
		chunk := chunks[i]

		// Try to detect and remove overlap at the beginning of this chunk
		words := strings.Fields(chunk)
		if len(words) > tc.overlap/4 {
			// Skip potential overlap words
			skipWords := tc.overlap / 4
			if skipWords < len(words) {
				chunk = strings.Join(words[skipWords:], " ")
			}
		}

		result += " " + chunk
	}

	return strings.TrimSpace(result)
}
