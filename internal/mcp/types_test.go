package mcp

import (
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
)

func TestFormatSearchResults(t *testing.T) {
	tests := []struct {
		name     string
		results  []db.SearchResultWithMetadata
		expected *mcp.CallToolResult
	}{
		{
			name: "single search result",
			results: []db.SearchResultWithMetadata{
				{
					ID:            "chunk-uuid-1",
					Content:       "The dragon soared through the sky",
					Author:        "J.R.R. Tolkien",
					Title:         "The Hobbit",
					Chapter:       "Chapter 1: An Unexpected Party",
					ChunkIndex:    5,
					Similarity:    0.85,
					ChapterIndex:  1,
					ChapterTitle:  "An Unexpected Party",
					TotalChunks:   50,
					TotalChapters: 19,
					FilePath:      "/media/audiobooks/tolkien/the-hobbit.m4b",
					FileChecksum:  "abc123def456",
					ChunkID:       "J.R.R. Tolkien_The Hobbit_1_5",
					WordCount:     8,
					ISBN:          "9780547928227",
					ASIN:          "B0099RNJB2",
				},
			},
			expected: &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Type: "text",
						Text: `Found 1 result(s):

**The Hobbit** by J.R.R. Tolkien
Chapter 1: An Unexpected Party (chunk 6/50, similarity: 85%)
ID: J.R.R. Tolkien_The Hobbit_1_5 | File: /media/audiobooks/tolkien/the-hobbit.m4b | Words: 8
> The dragon soared through the sky
`,
					},
				},
			},
		},
		{
			name:    "empty results",
			results: []db.SearchResultWithMetadata{},
			expected: &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Type: "text",
						Text: "No results found.",
					},
				},
			},
		},
		{
			name: "multiple results",
			results: []db.SearchResultWithMetadata{
				{
					ID:            "chunk-uuid-2",
					Content:       "First result content",
					Author:        "Author One",
					Title:         "Book One",
					Chapter:       "Chapter 1",
					ChunkIndex:    0,
					Similarity:    0.9,
					ChapterIndex:  1,
					ChapterTitle:  "Chapter 1",
					TotalChunks:   10,
					TotalChapters: 5,
					FilePath:      "/media/audiobooks/author-one/book-one.m4b",
					FileChecksum:  "def789ghi012",
					ChunkID:       "Author One_Book One_1_0",
					WordCount:     4,
				},
				{
					ID:            "chunk-uuid-3",
					Content:       "Second result content",
					Author:        "Author Two",
					Title:         "Book Two",
					Chapter:       "Chapter 2",
					ChunkIndex:    1,
					Similarity:    0.7,
					ChapterIndex:  2,
					ChapterTitle:  "Chapter 2",
					TotalChunks:   20,
					TotalChapters: 8,
					FilePath:      "/media/audiobooks/author-two/book-two.m4b",
					FileChecksum:  "ghi345jkl678",
					ChunkID:       "Author Two_Book Two_2_1",
					WordCount:     5,
				},
			},
			expected: &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Type: "text",
						Text: `Found 2 result(s):

**Book One** by Author One
Chapter 1: Chapter 1 (chunk 1/10, similarity: 90%)
ID: Author One_Book One_1_0 | File: /media/audiobooks/author-one/book-one.m4b | Words: 4
> First result content

**Book Two** by Author Two
Chapter 2: Chapter 2 (chunk 2/20, similarity: 70%)
ID: Author Two_Book Two_2_1 | File: /media/audiobooks/author-two/book-two.m4b | Words: 5
> Second result content
`,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatSearchResults(tt.results)
			assert.Equal(t, tt.expected.Content[0].(mcp.TextContent).Text,
				result.Content[0].(mcp.TextContent).Text)
		})
	}
}

func TestFormatHierarchicalData(t *testing.T) {
	tests := []struct {
		name         string
		entries      []db.HierarchicalEntry
		expectedText string
	}{
		{
			name: "single file entry",
			entries: []db.HierarchicalEntry{
				{FilePath: "/books/Tolkien/The Hobbit/ch1.mp3", ChunkCount: 42},
			},
			expectedText: "📚 **Audiobook Library**\n\n└── ch1.mp3 (42 chunks)\n",
		},
		{
			name:         "empty library",
			entries:      []db.HierarchicalEntry{},
			expectedText: "📚 **Audiobook Library**\n\nNo audiobooks found.",
		},
		{
			name: "multiple entries",
			entries: []db.HierarchicalEntry{
				{FilePath: "/books/A/Book/ch1.mp3", ChunkCount: 10},
				{FilePath: "/books/B/Book/ch2.mp3", ChunkCount: 20},
			},
			expectedText: "📚 **Audiobook Library**\n\n├── ch1.mp3 (10 chunks)\n└── ch2.mp3 (20 chunks)\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatHierarchicalData(tt.entries)
			assert.Equal(t, tt.expectedText, result.Content[0].(mcp.TextContent).Text)
		})
	}
}

func TestFilterHierarchicalData(t *testing.T) {
	entries := []db.HierarchicalEntry{
		{FilePath: "/books/J.R.R. Tolkien/The Hobbit/ch1.mp3", ChunkCount: 5},
		{FilePath: "/books/J.R.R. Tolkien/The Fellowship/ch1.mp3", ChunkCount: 8},
		{FilePath: "/books/Brandon Sanderson/The Way of Kings/ch1.mp3", ChunkCount: 12},
	}

	tests := []struct {
		name          string
		authorFilter  string
		bookFilter    string
		expectedCount int
	}{
		{"no filters", "", "", 3},
		{"filter by tolkien", "tolkien", "", 2},
		{"filter by hobbit", "", "hobbit", 1},
		{"combined filter", "tolkien", "fellowship", 1},
		{"no matches", "nonexistent", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered := filterHierarchicalData(entries, tt.authorFilter, tt.bookFilter)
			assert.Len(t, filtered, tt.expectedCount)
		})
	}
}
