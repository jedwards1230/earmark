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
					ID:            1,
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
					ID:            1,
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
					ID:            2,
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
		name     string
		entries  []db.HierarchicalEntry
		expected *mcp.CallToolResult
	}{
		{
			name: "single author with books",
			entries: []db.HierarchicalEntry{
				{
					Author: "J.R.R. Tolkien",
					Title:  "The Hobbit",
					Chapters: []string{
						"An Unexpected Party",
						"Roast Mutton",
					},
				},
				{
					Author: "J.R.R. Tolkien",
					Title:  "The Fellowship of the Ring",
					Chapters: []string{
						"A Long-expected Party",
					},
				},
			},
			expected: &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Type: "text",
						Text: `📚 **Audiobook Library**

**J.R.R. Tolkien**
├── The Hobbit (2 chapters)
│   ├── An Unexpected Party
│   └── Roast Mutton
└── The Fellowship of the Ring (1 chapter)
    └── A Long-expected Party
`,
					},
				},
			},
		},
		{
			name:    "empty library",
			entries: []db.HierarchicalEntry{},
			expected: &mcp.CallToolResult{
				Content: []mcp.Content{
					mcp.TextContent{
						Type: "text",
						Text: "📚 **Audiobook Library**\n\nNo audiobooks found.",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatHierarchicalData(tt.entries)
			assert.Equal(t, tt.expected.Content[0].(mcp.TextContent).Text,
				result.Content[0].(mcp.TextContent).Text)
		})
	}
}

func TestFilterHierarchicalData(t *testing.T) {
	entries := []db.HierarchicalEntry{
		{
			Author:   "J.R.R. Tolkien",
			Title:    "The Hobbit",
			Chapters: []string{"Chapter 1", "Chapter 2"},
		},
		{
			Author:   "J.R.R. Tolkien",
			Title:    "The Fellowship of the Ring",
			Chapters: []string{"Chapter 1"},
		},
		{
			Author:   "Brandon Sanderson",
			Title:    "The Way of Kings",
			Chapters: []string{"Prelude", "Chapter 1"},
		},
	}

	tests := []struct {
		name           string
		authorFilter   string
		bookFilter     string
		expectedCount  int
		expectedAuthor string
		expectedTitle  string
	}{
		{
			name:           "filter by author partial match",
			authorFilter:   "tolkien",
			bookFilter:     "",
			expectedCount:  2,
			expectedAuthor: "J.R.R. Tolkien",
		},
		{
			name:          "filter by book partial match",
			authorFilter:  "",
			bookFilter:    "hobbit",
			expectedCount: 1,
			expectedTitle: "The Hobbit",
		},
		{
			name:          "filter by both author and book",
			authorFilter:  "tolkien",
			bookFilter:    "fellowship",
			expectedCount: 1,
			expectedTitle: "The Fellowship of the Ring",
		},
		{
			name:          "no filters",
			authorFilter:  "",
			bookFilter:    "",
			expectedCount: 3,
		},
		{
			name:          "no matches",
			authorFilter:  "nonexistent",
			bookFilter:    "",
			expectedCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered := filterHierarchicalData(entries, tt.authorFilter, tt.bookFilter)
			assert.Len(t, filtered, tt.expectedCount)

			if tt.expectedCount > 0 {
				if tt.expectedAuthor != "" {
					assert.Equal(t, tt.expectedAuthor, filtered[0].Author)
				}
				if tt.expectedTitle != "" {
					found := false
					for _, entry := range filtered {
						if entry.Title == tt.expectedTitle {
							found = true
							break
						}
					}
					assert.True(t, found, "Expected title not found in filtered results")
				}
			}
		})
	}
}
