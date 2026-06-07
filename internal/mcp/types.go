package mcp

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jedwards1230/lil-whisper/internal/db"

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
	fmt.Fprintf(&output, "Found %d result(s):\n\n", len(results))

	for _, result := range results {
		// Format: **Title** by Author
		fmt.Fprintf(&output, "**%s** by %s\n", result.Title, result.Author)

		// Format: Chapter X: Title (chunk Y/Z, similarity: XX%)
		similarity := int(result.Similarity * 100)
		fmt.Fprintf(&output, "Chapter %d: %s (chunk %d/%d, similarity: %d%%)\n",
			result.ChapterIndex, result.ChapterTitle, result.ChunkIndex+1, result.TotalChunks, similarity)

		// Enhanced citation info
		if result.ChunkID != "" {
			fmt.Fprintf(&output, "ID: %s", result.ChunkID)
			if result.FilePath != "" {
				fmt.Fprintf(&output, " | File: %s", result.FilePath)
			}
			if result.WordCount > 0 {
				fmt.Fprintf(&output, " | Words: %d", result.WordCount)
			}
			output.WriteString("\n")
		}

		// Format: > Content
		fmt.Fprintf(&output, "> %s\n", result.Content)

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

// formatHierarchicalData converts hierarchical library data to MCP text content.
// Each HierarchicalEntry has a FilePath and ChunkCount; we derive a display
// name from the file path.
func formatHierarchicalData(entries []db.HierarchicalEntry) *mcp.CallToolResult {
	var output strings.Builder
	output.WriteString("📚 **Audiobook Library**\n\n")

	if len(entries) == 0 {
		output.WriteString("No audiobooks found.")
	} else {
		for i, entry := range entries {
			prefix := "├──"
			if i == len(entries)-1 {
				prefix = "└──"
			}
			name := filepath.Base(entry.FilePath)
			fmt.Fprintf(&output, "%s %s (%d chunks)\n", prefix, name, entry.ChunkCount)
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

// filterHierarchicalData filters entries by path substring matches
// (case-insensitive). Both authorFilter and bookFilter must match
// independently — an entry is included only if the file path contains
// both substrings (when non-empty).
func filterHierarchicalData(entries []db.HierarchicalEntry, authorFilter, bookFilter string) []db.HierarchicalEntry {
	if authorFilter == "" && bookFilter == "" {
		return entries
	}

	authorFilter = strings.ToLower(authorFilter)
	bookFilter = strings.ToLower(bookFilter)

	var filtered []db.HierarchicalEntry
	for _, entry := range entries {
		path := strings.ToLower(entry.FilePath)
		authorMatch := authorFilter == "" || strings.Contains(path, authorFilter)
		bookMatch := bookFilter == "" || strings.Contains(path, bookFilter)
		if authorMatch && bookMatch {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
