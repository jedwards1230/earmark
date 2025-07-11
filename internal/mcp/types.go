package mcp

import (
	"fmt"
	"strings"
	"transcriber/internal/db"

	"github.com/mark3labs/mcp-go/mcp"
)

// formatSearchResults converts search results to MCP text content
func formatSearchResults(results []db.SearchResultWithMetadata) *mcp.CallToolResult {
	if len(results) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				mcp.TextContent{
					Type: "text",
					Text: "No results found.",
				},
			},
		}
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d result(s):\n\n", len(results)))

	for _, result := range results {
		// Format: **Title** by Author
		output.WriteString(fmt.Sprintf("**%s** by %s\n", result.Title, result.Author))

		// Format: Chapter X: Title (chunk Y/Z, similarity: XX%)
		similarity := int(result.Similarity * 100)
		output.WriteString(fmt.Sprintf("Chapter %d: %s (chunk %d/%d, similarity: %d%%)\n",
			result.ChapterIndex, result.ChapterTitle, result.ChunkIndex+1, result.TotalChunks, similarity))

		// Format: > Content
		output.WriteString(fmt.Sprintf("> %s\n", result.Content))

		// Add spacing between results
		if result.ID != results[len(results)-1].ID {
			output.WriteString("\n")
		}
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{
				Type: "text",
				Text: output.String(),
			},
		},
	}
}

// formatHierarchicalData converts hierarchical library data to MCP text content
func formatHierarchicalData(entries []db.HierarchicalEntry) *mcp.CallToolResult {
	var output strings.Builder
	output.WriteString("📚 **Audiobook Library**\n\n")

	if len(entries) == 0 {
		output.WriteString("No audiobooks found.")
	} else {
		// Group by author
		authorBooks := make(map[string][]db.HierarchicalEntry)
		for _, entry := range entries {
			authorBooks[entry.Author] = append(authorBooks[entry.Author], entry)
		}

		authorIndex := 0
		for author, books := range authorBooks {
			output.WriteString(fmt.Sprintf("**%s**\n", author))

			for bookIndex, book := range books {
				var bookPrefix string
				if bookIndex == len(books)-1 {
					bookPrefix = "└──"
				} else {
					bookPrefix = "├──"
				}

				chapterCount := len(book.Chapters)
				chapterWord := "chapters"
				if chapterCount == 1 {
					chapterWord = "chapter"
				}

				output.WriteString(fmt.Sprintf("%s %s (%d %s)\n",
					bookPrefix, book.Title, chapterCount, chapterWord))

				// List chapters
				for chapterIndex, chapter := range book.Chapters {
					var chapterIndent, chapterPrefix string
					if bookIndex == len(books)-1 {
						chapterIndent = "    "
					} else {
						chapterIndent = "│   "
					}

					if chapterIndex == len(book.Chapters)-1 {
						chapterPrefix = "└──"
					} else {
						chapterPrefix = "├──"
					}

					output.WriteString(fmt.Sprintf("%s%s %s\n",
						chapterIndent, chapterPrefix, chapter))
				}
			}

			authorIndex++
			if authorIndex < len(authorBooks) {
				output.WriteString("\n")
			}
		}
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{
				Type: "text",
				Text: output.String(),
			},
		},
	}
}

// filterHierarchicalData filters entries by author and book name (case-insensitive partial match)
func filterHierarchicalData(entries []db.HierarchicalEntry, authorFilter, bookFilter string) []db.HierarchicalEntry {
	if authorFilter == "" && bookFilter == "" {
		return entries
	}

	var filtered []db.HierarchicalEntry
	authorFilter = strings.ToLower(authorFilter)
	bookFilter = strings.ToLower(bookFilter)

	for _, entry := range entries {
		authorMatch := authorFilter == "" || strings.Contains(strings.ToLower(entry.Author), authorFilter)
		bookMatch := bookFilter == "" || strings.Contains(strings.ToLower(entry.Title), bookFilter)

		if authorMatch && bookMatch {
			filtered = append(filtered, entry)
		}
	}

	return filtered
}
