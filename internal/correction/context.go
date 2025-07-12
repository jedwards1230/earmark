package correction

import (
	"fmt"
	"strings"

	"github.com/jedwards1230/lil-whisper/internal/meta"
)

type CorrectionContext struct {
	// Book metadata
	BookTitle    string
	Author       string
	
	// Chapter metadata
	ChapterTitle string
	ChapterIndex int
	
	// Content
	OriginalText string
	
	// Additional context
	FilePath     string
	ISBN         string
	ASIN         string
}

func NewCorrectionContext(fileMeta *meta.FileMetadata, originalText string) *CorrectionContext {
	ctx := &CorrectionContext{
		OriginalText: originalText,
		FilePath:     fileMeta.FilePath,
	}

	// Extract book information
	if fileMeta.Title != "" {
		ctx.BookTitle = fileMeta.Title
	}
	
	if fileMeta.Author != "" {
		ctx.Author = fileMeta.Author
	}
	
	// Extract chapter information
	if fileMeta.Chapter != "" {
		ctx.ChapterTitle = fileMeta.Chapter
	}
	
	ctx.ChapterIndex = fileMeta.ChapterIndex
	
	// Additional identifiers
	if fileMeta.ISBN != "" {
		ctx.ISBN = fileMeta.ISBN
	}
	
	if fileMeta.ASIN != "" {
		ctx.ASIN = fileMeta.ASIN
	}

	return ctx
}

func (c *CorrectionContext) GetContextSummary() string {
	var parts []string
	
	if c.BookTitle != "" {
		parts = append(parts, fmt.Sprintf("Book: \"%s\"", c.BookTitle))
	}
	
	if c.Author != "" {
		parts = append(parts, fmt.Sprintf("Author: %s", c.Author))
	}
	
	if c.ChapterTitle != "" {
		parts = append(parts, fmt.Sprintf("Chapter: \"%s\"", c.ChapterTitle))
	} else if c.ChapterIndex > 0 {
		parts = append(parts, fmt.Sprintf("Chapter %d", c.ChapterIndex))
	}
	
	if len(parts) == 0 {
		return "Unknown audiobook content"
	}
	
	return strings.Join(parts, ", ")
}

func (c *CorrectionContext) GetFormattedMetadata() string {
	var metadata strings.Builder
	
	metadata.WriteString("Content Information:\n")
	
	if c.BookTitle != "" {
		metadata.WriteString(fmt.Sprintf("- Title: %s\n", c.BookTitle))
	}
	
	if c.Author != "" {
		metadata.WriteString(fmt.Sprintf("- Author: %s\n", c.Author))
	}
	
	if c.ChapterTitle != "" {
		metadata.WriteString(fmt.Sprintf("- Chapter: %s\n", c.ChapterTitle))
	} else if c.ChapterIndex > 0 {
		metadata.WriteString(fmt.Sprintf("- Chapter: %d\n", c.ChapterIndex))
	}
	
	return metadata.String()
}

func (c *CorrectionContext) IsNonfiction() bool {
	// Without genre information, we can't determine content type
	return false
}

func (c *CorrectionContext) IsFiction() bool {
	// Without genre information, we can't determine content type
	return false
}

func (c *CorrectionContext) GetContentType() string {
	// Without genre information, return unknown
	return "unknown"
}