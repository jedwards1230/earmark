package correction

import (
	"strings"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/meta"
)

func TestCorrectionContext(t *testing.T) {
	fileMeta := &meta.FileMetadata{
		FilePath:     "/path/to/audiobook/chapter1.m4b",
		Title:        "The Great Gatsby",
		Author:       "F. Scott Fitzgerald",
		Chapter:      "Chapter 1: In My Younger Days",
		ChapterIndex: 1,
		ISBN:         "978-0-7432-7356-5",
		ASIN:         "B000FC2L2E",
	}

	originalText := "This is the original transcription text."

	ctx := NewCorrectionContext(fileMeta, originalText)

	// Test basic field extraction
	if ctx.BookTitle != "The Great Gatsby" {
		t.Errorf("Expected book title 'The Great Gatsby', got '%s'", ctx.BookTitle)
	}

	if ctx.Author != "F. Scott Fitzgerald" {
		t.Errorf("Expected author 'F. Scott Fitzgerald', got '%s'", ctx.Author)
	}

	if ctx.ChapterTitle != "Chapter 1: In My Younger Days" {
		t.Errorf("Expected chapter title 'Chapter 1: In My Younger Days', got '%s'", ctx.ChapterTitle)
	}

	if ctx.ChapterIndex != 1 {
		t.Errorf("Expected chapter index 1, got %d", ctx.ChapterIndex)
	}

	if ctx.OriginalText != originalText {
		t.Errorf("Expected original text '%s', got '%s'", originalText, ctx.OriginalText)
	}

	if ctx.FilePath != "/path/to/audiobook/chapter1.m4b" {
		t.Errorf("Expected file path '/path/to/audiobook/chapter1.m4b', got '%s'", ctx.FilePath)
	}

	if ctx.ISBN != "978-0-7432-7356-5" {
		t.Errorf("Expected ISBN '978-0-7432-7356-5', got '%s'", ctx.ISBN)
	}

	if ctx.ASIN != "B000FC2L2E" {
		t.Errorf("Expected ASIN 'B000FC2L2E', got '%s'", ctx.ASIN)
	}
}

func TestGetContextSummary(t *testing.T) {
	tests := []struct {
		name     string
		fileMeta *meta.FileMetadata
		expected []string // Parts that should be in the summary
	}{
		{
			name: "complete_metadata",
			fileMeta: &meta.FileMetadata{
				Title:        "Dune",
				Author:       "Frank Herbert",
				Chapter:      "Chapter 1: Arrakis",
				ChapterIndex: 1,
			},
			expected: []string{"Book: \"Dune\"", "Author: Frank Herbert", "Chapter: \"Chapter 1: Arrakis\""},
		},
		{
			name: "minimal_metadata",
			fileMeta: &meta.FileMetadata{
				Title:  "Unknown Book",
				Author: "Unknown Author",
			},
			expected: []string{"Book: \"Unknown Book\"", "Author: Unknown Author"},
		},
		{
			name: "no_chapter_title",
			fileMeta: &meta.FileMetadata{
				Title:        "Test Book",
				Author:       "Test Author",
				ChapterIndex: 5,
			},
			expected: []string{"Book: \"Test Book\"", "Author: Test Author", "Chapter 5"},
		},
		{
			name:     "empty_metadata",
			fileMeta: &meta.FileMetadata{},
			expected: []string{"Unknown audiobook content"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := NewCorrectionContext(tt.fileMeta, "test text")
			summary := ctx.GetContextSummary()

			for _, expectedPart := range tt.expected {
				if !strings.Contains(summary, expectedPart) {
					t.Errorf("Expected summary to contain '%s', got: %s", expectedPart, summary)
				}
			}
		})
	}
}

func TestGetFormattedMetadata(t *testing.T) {
	fileMeta := &meta.FileMetadata{
		Title:        "1984",
		Author:       "George Orwell",
		Chapter:      "Part One, Chapter 1",
		ChapterIndex: 1,
	}

	ctx := NewCorrectionContext(fileMeta, "test text")
	formatted := ctx.GetFormattedMetadata()

	expectedParts := []string{
		"Content Information:",
		"- Title: 1984",
		"- Author: George Orwell",
		"- Chapter: Part One, Chapter 1",
	}

	for _, expectedPart := range expectedParts {
		if !strings.Contains(formatted, expectedPart) {
			t.Errorf("Expected formatted metadata to contain '%s', got: %s", expectedPart, formatted)
		}
	}
}

func TestContentTypeDetection(t *testing.T) {
	// Since we don't have genre information in FileMetadata,
	// all content type detection should return "unknown" and false for fiction/nonfiction
	tests := []struct {
		name         string
		expectedType string
		isFiction    bool
		isNonfiction bool
	}{
		{
			name:         "any_book",
			expectedType: "unknown",
			isFiction:    false,
			isNonfiction: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fileMeta := &meta.FileMetadata{
				Title: "Some Book",
			}

			ctx := NewCorrectionContext(fileMeta, "test text")

			if ctx.GetContentType() != tt.expectedType {
				t.Errorf("Expected content type '%s', got '%s'", tt.expectedType, ctx.GetContentType())
			}

			if ctx.IsFiction() != tt.isFiction {
				t.Errorf("Expected IsFiction() to be %v, got %v", tt.isFiction, ctx.IsFiction())
			}

			if ctx.IsNonfiction() != tt.isNonfiction {
				t.Errorf("Expected IsNonfiction() to be %v, got %v", tt.isNonfiction, ctx.IsNonfiction())
			}
		})
	}
}
