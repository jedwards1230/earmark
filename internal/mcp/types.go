package mcp

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/library"

	"github.com/mark3labs/mcp-go/mcp"
)

// textResult wraps plain text in the MCP CallToolResult shape used everywhere in
// this package.
func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent{Type: "text", Text: s}},
	}
}

// fmtHMS renders a second count as "Hh MMm SSs" (em dash when unknown), matching
// the dashboard's humanizeSeconds style for the library list.
func fmtHMS(secs *float64) string {
	if secs == nil {
		return "—"
	}
	total := int64(*secs + 0.5)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// mmss renders seconds as mm:ss (or hh:mm:ss past an hour) for segment markers.
func mmss(secs float64) string {
	total := int64(secs + 0.5)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// intOrDash renders an optional count, em dash when unknown (no run_metrics row).
func intOrDash(n *int) string {
	if n == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *n)
}

// formatBookList renders the library inventory for the list_books tool: one line
// per book with author/title, track progress, duration, word count, and chunk
// count. A book with no run_metrics yet shows em dashes / 0 for its aggregates.
func formatBookList(books []db.BookSummary, total, offset int, resolver *library.Resolver) *mcp.CallToolResult {
	if len(books) == 0 {
		return textResult("No books found.")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Library: %d book(s)", total)
	if offset > 0 || len(books) < total {
		fmt.Fprintf(&b, " (showing %d–%d)", offset+1, offset+len(books))
	}
	b.WriteString("\n\n")

	for _, bk := range books {
		author, title := resolver.Resolve(bk.Dir, bk.SamplePath)
		if title == "" {
			title = filepath.Base(bk.Dir)
		}
		if author == "" {
			author = "Unknown"
		}
		fmt.Fprintf(&b, "**%s** by %s\n", title, author)
		fmt.Fprintf(&b, "  tracks: %d/%d done", bk.Done, bk.Total)
		if bk.Pending > 0 {
			fmt.Fprintf(&b, ", %d pending", bk.Pending)
		}
		if bk.Claimed > 0 {
			fmt.Fprintf(&b, ", %d in progress", bk.Claimed)
		}
		if bk.Failed > 0 {
			fmt.Fprintf(&b, ", %d failed", bk.Failed)
		}
		fmt.Fprintf(&b, " | duration: %s | words: %s | chunks: %s\n",
			fmtHMS(bk.DurationSeconds), intOrDash(bk.WordCount), intOrDash(bk.EmbedChunkCount))
		fmt.Fprintf(&b, "  dir: %s\n\n", bk.Dir)
	}

	if offset+len(books) < total {
		fmt.Fprintf(&b, "Showing %d of %d books. Next page: offset=%d.\n", offset+len(books), total, offset+len(books))
	}

	return textResult(strings.TrimRight(b.String(), "\n"))
}

// formatTrackChooser lists a multi-track book's tracks so the caller can pick one
// (by trackID) for get_transcript.
func formatTrackChooser(book string, tracks []db.RecentJob) *mcp.CallToolResult {
	var b strings.Builder
	fmt.Fprintf(&b, "%q has %d tracks. Call get_transcript again with one of these trackID values:\n\n", book, len(tracks))
	for _, t := range tracks {
		fmt.Fprintf(&b, "  • %s  [%s]  trackID=%s\n", filepath.Base(t.FilePath), t.Status, t.ID)
	}
	return textResult(strings.TrimRight(b.String(), "\n"))
}

// formatTranscriptPage renders a page of a track's transcript as timestamped
// segments. raw_text can be hundreds of thousands of characters, so segments are
// paginated by offset/limit with a footer pointing at the next page.
func formatTranscriptPage(d *db.TrackDetail, offset, limit int) *mcp.CallToolResult {
	// Defensive: the caller checks for nil, but guard here too so the helper is
	// safe to reuse. A nil detail has no transcript to render.
	if d == nil {
		return mcp.NewToolResultError("no transcript available")
	}
	totalSegs := len(d.Segments)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 50
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Transcript: %s\n", d.FilePath)
	fmt.Fprintf(&b, "Language: %s | Model: %s | Duration: %s\n\n",
		d.Language, d.ModelName, fmtHMS(&d.DurationSeconds))

	if offset >= totalSegs {
		fmt.Fprintf(&b, "(offset %d is past the end — %d segments total)", offset, totalSegs)
		return textResult(b.String())
	}

	end := offset + limit
	if end > totalSegs {
		end = totalSegs
	}
	for _, seg := range d.Segments[offset:end] {
		fmt.Fprintf(&b, "[%s → %s] %s\n", mmss(seg.Start), mmss(seg.End), strings.TrimSpace(seg.Text))
	}

	b.WriteString("\n")
	if end < totalSegs {
		fmt.Fprintf(&b, "Showing segments %d–%d of %d. Next page: offset=%d.", offset+1, end, totalSegs, end)
	} else {
		fmt.Fprintf(&b, "Showing segments %d–%d of %d (end of transcript).", offset+1, end, totalSegs)
	}
	return textResult(b.String())
}

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
