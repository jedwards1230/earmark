package chunker

import (
	"github.com/jedwards1230/lil-whisper/internal/tokenizer"
	"strings"
)

type SplitType string

const (
	SplitTypeChar  SplitType = "char"
	SplitTypeWord  SplitType = "word"
	SplitTypeToken SplitType = "token"
)

func Chunker(content string, chunkSize int, splitType SplitType) []string {
	switch splitType {
	case SplitTypeChar:
		return chunkByChar(content, chunkSize)
	case SplitTypeWord:
		return chunkByWord(content, chunkSize)
	case SplitTypeToken:
		return chunkByToken(content, chunkSize)
	default:
		return []string{content}
	}
}

func chunkByChar(content string, chunkSize int) []string {
	if content == "" {
		return []string{}
	}
	var chunks []string
	runes := []rune(content)

	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}

	return chunks
}

func chunkByWord(content string, chunkSize int) []string {
	if content == "" {
		return []string{}
	}

	var chunks []string
	// Do NOT strip periods — they are sentence boundaries and must be preserved
	// so that the word-split path produces correct output if ever enabled in
	// production (it is currently dormant; SplitTypeToken is used in prod).
	words := strings.Fields(content)
	currentChunk := strings.Builder{}
	wordCount := 0

	for _, word := range words {
		if wordCount >= chunkSize {
			chunks = append(chunks, strings.TrimSpace(currentChunk.String()))
			currentChunk.Reset()
			wordCount = 0
		}

		currentChunk.WriteString(word + " ")
		wordCount++
	}

	if currentChunk.Len() > 0 {
		chunks = append(chunks, strings.TrimSpace(currentChunk.String()))
	}

	return chunks
}

func chunkByToken(content string, chunkSize int) []string {
	if content == "" {
		return []string{""}
	}

	var chunks []string
	tokens, err := tokenizer.GetTokens(content)
	if err != nil {
		return []string{content}
	}

	if len(tokens) == 0 {
		return []string{content}
	}

	for i := 0; i < len(tokens); i += chunkSize {
		endIndex := i + chunkSize
		if endIndex > len(tokens) {
			endIndex = len(tokens)
		}

		chunkTokens := tokens[i:endIndex]
		decodedChunk, err := tokenizer.DecodeTokens(chunkTokens)
		if err != nil {
			continue
		}
		chunks = append(chunks, strings.TrimSpace(decodedChunk))
	}

	return chunks
}
