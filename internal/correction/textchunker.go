package correction

import (
	"fmt"
	"strings"

	"github.com/jedwards1230/lil-whisper/internal/tokenizer"
)

const (
	// Conservative token limits to leave room for prompt templates
	MaxTokensPerChunk = 3000  // For models with 4k context
	ChunkOverlap      = 200   // Overlap between chunks for continuity
)

type TextChunk struct {
	Text       string
	Index      int
	TotalChunks int
	TokenCount int
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

	// If text fits in one chunk, return it as-is
	if totalTokens <= tc.maxTokens {
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
					currentChunk.WriteString(overlapText)
					currentChunk.WriteString(" ")
					overlapTokens, _ := tokenizer.CountTokens(overlapText)
					currentTokens += overlapTokens
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
		chunks = append(chunks, TextChunk{
			Text:       strings.TrimSpace(currentChunk.String()),
			Index:      chunkIndex,
			TokenCount: currentTokens,
		})
	}

	// Set total chunks for all chunks
	totalChunks := len(chunks)
	for i := range chunks {
		chunks[i].TotalChunks = totalChunks
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