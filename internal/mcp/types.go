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

// librarySummaryLine renders the one-line library summary header from the
// whole-library totals: total books, how many are fully transcribed, and how many
// still have pending tracks. Returns "" when there are no books (so the caller can
// skip it for an empty library).
func librarySummaryLine(t db.LibraryTotals) string {
	if t.TotalBooks == 0 {
		return ""
	}
	return fmt.Sprintf("Library: %d books — %d fully transcribed, %d with pending tracks.",
		t.TotalBooks, t.FullyTranscribed, t.WithPending)
}

// formatBookList renders the library inventory for the list_books tool: one line
// per book with author/title, track progress, duration, word count, and chunk
// count. A book with no run_metrics yet shows em dashes / 0 for its aggregates.
// The flat view omits each book's dir line to keep the payload small (the tree
// view keeps it); a leading summary line reports whole-library totals.
func formatBookList(books []db.BookSummary, total, offset int, totals db.LibraryTotals, resolver *library.Resolver) *mcp.CallToolResult {
	if len(books) == 0 {
		return textResult("No books found.")
	}

	var b strings.Builder
	if summary := librarySummaryLine(totals); summary != "" {
		b.WriteString(summary)
		b.WriteString("\n\n")
	}
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
		// Flat view drops the dir line (it ~doubles the payload); use format=tree
		// to see each book's directory.
		fmt.Fprintf(&b, " | duration: %s | words: %s | chunks: %s\n\n",
			fmtHMS(bk.DurationSeconds), intOrDash(bk.WordCount), intOrDash(bk.EmbedChunkCount))
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

// searchKind labels how a result set was ranked, so the formatter can render an
// honest relevance line. Semantic results carry a real cosine-similarity score;
// text (trigram) results are ranked by pg_trgm match — a value the user must not
// read as a semantic-vector score (a literal hit can show a ~1% trigram score).
type searchKind int

const (
	// searchContext is the neutral kind for get_chunk_context, where there is no
	// query-relative relevance to report (the rows are positional neighbours).
	searchContext searchKind = iota
	// searchSemantic ranks by vector cosine similarity (semantic_search_audiobooks).
	searchSemantic
	// searchText ranks by pg_trgm trigram match (text_search_audiobooks).
	searchText
)

// formatSearchResults renders results with no query-relative relevance line —
// used by get_chunk_context, whose rows are positional neighbours, not ranked
// hits. Search tools use formatSearchResultsOpts so they can label relevance and
// honour the snippet window.
func formatSearchResults(results []db.SearchResultWithMetadata) *mcp.CallToolResult {
	return formatSearchResultsOpts(results, searchContext, "", 0)
}

// formatSearchResultsOpts renders search results as MCP text content.
//
//   - kind controls the per-hit relevance line: semantic shows "similarity: NN%"
//     (a real cosine score); text shows "trigram match" (NOT a semantic score —
//     pg_trgm values are tiny for a literal hit and would mislead if labelled
//     "similarity"); context omits the line entirely.
//   - query/snippet drive the optional excerpt window: when snippet > 0 the chunk
//     text is truncated to ~snippet chars. Text search centres the window on the
//     literal query match; semantic search returns a leading preview (no
//     sub-chunk match position exists). Truncated text gets a marker pointing at
//     get_chunk_context for the full surrounding text.
func formatSearchResultsOpts(results []db.SearchResultWithMetadata, kind searchKind, query string, snippet int) *mcp.CallToolResult {
	if len(results) == 0 {
		return textResult("No results found.")
	}

	var output strings.Builder
	fmt.Fprintf(&output, "Found %d result(s):\n\n", len(results))

	for _, result := range results {
		// Format: **Title** by Author
		fmt.Fprintf(&output, "**%s** by %s\n", result.Title, result.Author)

		// Chapter mapping isn't populated yet (a future ABS-integration PR will
		// fill it in), so suppress the misleading "Chapter 0:" prefix whenever
		// there's no real chapter data — index 0 AND empty title.
		chapterPrefix := ""
		if result.ChapterIndex != 0 || strings.TrimSpace(result.ChapterTitle) != "" {
			chapterPrefix = fmt.Sprintf("Chapter %d: %s ", result.ChapterIndex, result.ChapterTitle)
		}

		// Relevance line varies by ranking mechanism.
		switch kind {
		case searchSemantic:
			fmt.Fprintf(&output, "%s(chunk %d/%d, similarity: %d%%)\n",
				chapterPrefix, result.ChunkIndex+1, result.TotalChunks, int(result.Similarity*100))
		case searchText:
			// pg_trgm rank — deliberately NOT shown as a percentage "similarity",
			// which reads like a broken semantic score for literal matches.
			fmt.Fprintf(&output, "%s(chunk %d/%d, ranked by trigram match)\n",
				chapterPrefix, result.ChunkIndex+1, result.TotalChunks)
		default: // searchContext
			fmt.Fprintf(&output, "%s(chunk %d/%d)\n",
				chapterPrefix, result.ChunkIndex+1, result.TotalChunks)
		}

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

		// Format: > Content (optionally windowed to a snippet).
		content := result.Content
		if snippet > 0 {
			if excerpt, truncated := makeSnippet(content, query, snippet, kind); truncated {
				content = excerpt + " …(truncated, use get_chunk_context for full text)"
			} else {
				content = excerpt
			}
		}
		fmt.Fprintf(&output, "> %s\n", content)

		// Add spacing between results
		if result.ID != results[len(results)-1].ID {
			output.WriteString("\n")
		}
	}

	return textResult(output.String())
}

// makeSnippet trims content to roughly max characters. For text search it centres
// the window on the first case-insensitive occurrence of query; for semantic
// search (or when query is absent / not found) it returns a leading window. It
// reports whether the content was actually shortened so the caller can append a
// truncation marker. Trimming is rune-safe and snaps to surrounding spaces so
// words aren't sliced mid-character/mid-word.
func makeSnippet(content, query string, max int, kind searchKind) (string, bool) {
	r := []rune(content)
	if len(r) <= max {
		return content, false
	}

	start := 0
	if kind == searchText && query != "" {
		if idx := strings.Index(strings.ToLower(content), strings.ToLower(query)); idx >= 0 {
			// idx is a byte offset; convert to a rune offset, then centre the window.
			matchRune := len([]rune(content[:idx]))
			start = matchRune - (max-len([]rune(query)))/2
			if start < 0 {
				start = 0
			}
		}
	}
	end := start + max
	if end > len(r) {
		end = len(r)
		start = end - max
		if start < 0 {
			start = 0
		}
	}

	// Snap the start forward to the next space (so we don't begin mid-word) when
	// we're not already at the very beginning.
	if start > 0 {
		for start < end && r[start] != ' ' {
			start++
		}
		for start < end && r[start] == ' ' {
			start++
		}
	}
	// Snap the end back to the previous space for the same reason.
	if end < len(r) {
		for end > start && r[end-1] != ' ' {
			end--
		}
	}
	if start >= end { // degenerate (e.g. a single very long token) → hard cut.
		start, end = 0, max
	}

	excerpt := strings.TrimSpace(string(r[start:end]))
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(r) {
		suffix = "…"
	}
	return prefix + excerpt + suffix, true
}

// authorGroup is one author and their books, used to render the list_books tree.
type authorGroup struct {
	author string
	books  []db.BookSummary
}

// formatBookTree renders the same inventory as formatBookList but grouped by
// author (list_books format=tree). It only regroups the rows list_books already
// produced — no new queries — so author → books, books in their original order.
func formatBookTree(books []db.BookSummary, total, offset int, totals db.LibraryTotals, resolver *library.Resolver) *mcp.CallToolResult {
	if len(books) == 0 {
		return textResult("No books found.")
	}

	// Group books by resolver-derived author, preserving first-seen order.
	var groups []authorGroup
	index := map[string]int{}
	for _, bk := range books {
		author, _ := resolver.Resolve(bk.Dir, bk.SamplePath)
		if author == "" {
			author = "Unknown"
		}
		if i, ok := index[author]; ok {
			groups[i].books = append(groups[i].books, bk)
		} else {
			index[author] = len(groups)
			groups = append(groups, authorGroup{author: author, books: []db.BookSummary{bk}})
		}
	}

	var b strings.Builder
	if summary := librarySummaryLine(totals); summary != "" {
		b.WriteString(summary)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Library: %d book(s) across %d author(s)", total, len(groups))
	if offset > 0 || len(books) < total {
		fmt.Fprintf(&b, " (showing %d–%d)", offset+1, offset+len(books))
	}
	b.WriteString("\n\n")

	for _, g := range groups {
		fmt.Fprintf(&b, "%s\n", g.author)
		for _, bk := range g.books {
			_, title := resolver.Resolve(bk.Dir, bk.SamplePath)
			if title == "" {
				title = filepath.Base(bk.Dir)
			}
			fmt.Fprintf(&b, "  • %s — tracks: %d/%d done", title, bk.Done, bk.Total)
			if bk.Pending > 0 {
				fmt.Fprintf(&b, ", %d pending", bk.Pending)
			}
			if bk.Failed > 0 {
				fmt.Fprintf(&b, ", %d failed", bk.Failed)
			}
			fmt.Fprintf(&b, " | duration: %s | words: %s | chunks: %s\n",
				fmtHMS(bk.DurationSeconds), intOrDash(bk.WordCount), intOrDash(bk.EmbedChunkCount))
			fmt.Fprintf(&b, "    dir: %s\n", bk.Dir)
		}
		b.WriteString("\n")
	}

	if offset+len(books) < total {
		fmt.Fprintf(&b, "Showing %d of %d books. Next page: offset=%d.\n", offset+len(books), total, offset+len(books))
	}

	return textResult(strings.TrimRight(b.String(), "\n"))
}
