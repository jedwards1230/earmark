package mcp

import "github.com/jedwards1230/earmark/internal/db"

// This file defines the structured-output payloads for the MCP tools. Each tool
// returns BOTH a human-readable text rendering (the long-standing format, kept as
// the spec-required back-compat fallback in Content[0]) AND a machine-readable
// structuredContent object built from the underlying typed DB rows.
//
// The MCP output-schema spec requires the top-level structured payload to be a
// JSON object (mcp-go's WithOutputSchema always forces type:"object"), so even the
// search tools — which conceptually return a list — wrap their slice in a small
// object with a `results` field plus a count.

// SearchResultsOutput is the structured payload for the three chunk-returning
// tools: semantic_search_audiobooks, text_search_audiobooks, and
// get_chunk_context. `kind` records how the rows were ranked ("semantic",
// "trigram", or "context") so a consumer doesn't misread a trigram rank as a
// cosine-similarity score. `query` echoes the search term (empty for context).
type SearchResultsOutput struct {
	Kind    string                        `json:"kind"`
	Query   string                        `json:"query,omitempty"`
	Count   int                           `json:"count"`
	Results []db.SearchResultWithMetadata `json:"results"`
}

// BookEntry is one book row in the list_books structured output. It mirrors the
// rendered inventory line: provider-derived author/title plus the per-book
// track/duration/word/chunk aggregates. JSON tags are added here (db.BookSummary
// itself carries none) so the structured payload has stable, camelCase keys.
type BookEntry struct {
	Dir             string   `json:"dir"`
	Author          string   `json:"author"`
	Title           string   `json:"title"`
	Total           int      `json:"total"`
	Pending         int      `json:"pending"`
	Claimed         int      `json:"claimed"`
	Done            int      `json:"done"`
	Failed          int      `json:"failed"`
	DurationSeconds *float64 `json:"durationSeconds,omitempty"`
	WordCount       *int     `json:"wordCount,omitempty"`
	EmbedChunkCount *int     `json:"embedChunkCount,omitempty"`
}

// LibraryTotalsOutput mirrors db.LibraryTotals with explicit JSON tags for the
// structured payload (the db type carries none).
type LibraryTotalsOutput struct {
	TotalBooks       int `json:"totalBooks"`
	FullyTranscribed int `json:"fullyTranscribed"`
	WithPending      int `json:"withPending"`
}

// ListBooksOutput is the structured payload for list_books: the page of books,
// the whole-library totals (summary line), and the pagination footer fields.
// `nextOffset` is non-nil only when another page exists.
type ListBooksOutput struct {
	Format     string              `json:"format"`
	Books      []BookEntry         `json:"books"`
	Totals     LibraryTotalsOutput `json:"totals"`
	Total      int                 `json:"total"`
	Offset     int                 `json:"offset"`
	NextOffset *int                `json:"nextOffset,omitempty"`
}

// TrackRef is one track in a transcript track-chooser response (when a book has
// multiple tracks and the caller must pick one by trackID).
type TrackRef struct {
	TrackID  string `json:"trackID"`
	FilePath string `json:"filePath"`
	Status   string `json:"status"`
}

// TranscriptSegment is one timestamped segment in a get_transcript page.
type TranscriptSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// TranscriptOutput is the structured payload for get_transcript. It is one of two
// shapes, distinguished by `kind`:
//
//   - kind="transcript": a page of timestamped segments for a single track, with
//     pagination footer fields. `tracks` is empty.
//   - kind="trackChooser": the book has multiple tracks; `tracks` lists them so
//     the caller can re-call with a trackID. The segment/pagination fields are
//     zero.
type TranscriptOutput struct {
	Kind string `json:"kind"`

	// Single-track page (kind="transcript").
	FilePath        string              `json:"filePath,omitempty"`
	Language        string              `json:"language,omitempty"`
	ModelName       string              `json:"modelName,omitempty"`
	DurationSeconds float64             `json:"durationSeconds,omitempty"`
	Segments        []TranscriptSegment `json:"segments,omitempty"`
	Offset          int                 `json:"offset"`
	Limit           int                 `json:"limit"`
	TotalSegments   int                 `json:"totalSegments"`
	NextOffset      *int                `json:"nextOffset,omitempty"`

	// Track chooser (kind="trackChooser").
	Book   string     `json:"book,omitempty"`
	Tracks []TrackRef `json:"tracks,omitempty"`
}
