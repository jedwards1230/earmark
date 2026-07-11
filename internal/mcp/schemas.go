package mcp

import (
	"encoding/json"
	"fmt"

	jsonschema "github.com/google/jsonschema-go/jsonschema"
)

// This file hand-authors the *jsonschema.Schema InputSchema for each of the 5
// MCP tools, matching the property names/types/descriptions/required set/
// defaults of the pre-migration mcp-go option-builder DSL (mcp.WithString,
// mcp.WithNumber, mcp.WithBoolean, mcp.Required, mcp.DefaultNumber/DefaultBool)
// 1:1. It also derives each tool's OutputSchema by reflecting the existing
// structured-output struct (results.go), which mcp-go did via
// WithOutputSchema[T]().

// mustDefault marshals a Go literal default value into the json.RawMessage
// jsonschema.Schema.Default expects. v is always a compile-time Go literal
// here, so Marshal cannot fail.
func mustDefault(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("mustDefault(%v): %v", v, err))
	}
	return b
}

// stringProp builds a required-or-optional string property (required-ness is
// controlled by the schema's top-level Required list, not the property itself).
func stringProp(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: desc}
}

// numberProp builds a number property carrying a default value, mirroring
// mcp.WithNumber(..., mcp.DefaultNumber(def)).
func numberProp(desc string, def float64) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "number", Description: desc, Default: mustDefault(def)}
}

// numberPropNoDefault builds a number property with no default (e.g. `snippet`,
// which is omitted/0 by default with no schema-level default value in mcp-go).
func numberPropNoDefault(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "number", Description: desc}
}

// boolProp builds a boolean property carrying a default value, mirroring
// mcp.WithBoolean(..., mcp.DefaultBool(def)).
func boolProp(desc string, def bool) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: desc, Default: mustDefault(def)}
}

// objectSchema builds the top-level object schema AddTool requires (type
// "object" is mandatory on the low-level registration path).
func objectSchema(required []string, props map[string]*jsonschema.Schema) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:       "object",
		Properties: props,
		Required:   required,
	}
}

// semanticSearchSchema: query (required), book, threshold (default 0.3),
// limit (default 10), snippet (no default).
func semanticSearchSchema() *jsonschema.Schema {
	return objectSchema([]string{"query"}, map[string]*jsonschema.Schema{
		"query":     stringProp("The search query to find relevant content"),
		"book":      stringProp("Optional: restrict the search to one book (a title or directory substring, e.g. \"Project Hail Mary\"). Omit to search the entire library. Run list_books to see available titles."),
		"threshold": numberProp("Similarity threshold (0.0-1.0, default: 0.3)", 0.3),
		"limit":     numberProp("Maximum number of results to return (default: 10)", 10),
		"snippet": numberPropNoDefault("Optional: cap each hit's quoted text to roughly this many characters to keep iterative searches cheap. " +
			"Omit to return the full ~400-word chunk (default). A semantic hit has no sub-chunk match position, so the " +
			"snippet is a leading PREVIEW of the chunk, not a centered excerpt — use get_chunk_context for the full " +
			"surrounding text."),
	})
}

// textSearchSchema: query (required), book, limit (default 10), snippet (no default).
func textSearchSchema() *jsonschema.Schema {
	return objectSchema([]string{"query"}, map[string]*jsonschema.Schema{
		"query": stringProp("The search query to find exact text matches"),
		"book":  stringProp("Optional: restrict the search to one book (a title or directory substring, e.g. \"Dune\"). Omit to search the entire library. Run list_books to see available titles."),
		"limit": numberProp("Maximum number of results to return (default: 10)", 10),
		"snippet": numberPropNoDefault("Optional: cap each hit's quoted text to roughly this many characters to keep iterative searches cheap. " +
			"Omit to return the full ~400-word chunk (default). When set, the excerpt is CENTERED on the literal query " +
			"match within the chunk. Use get_chunk_context for the full surrounding text."),
	})
}

// listBooksSchema: no required fields; author, format, limit (default 50),
// offset (default 0).
func listBooksSchema() *jsonschema.Schema {
	return objectSchema(nil, map[string]*jsonschema.Schema{
		"author": stringProp("Optional: filter to books whose path/author matches this substring (case-insensitive)."),
		"format": stringProp("Output shape: \"flat\" (default) — one entry per book; or \"tree\" — group books by author. " +
			"Both list the same books with the same metadata; tree only changes the grouping."),
		"limit":  numberProp("Maximum number of books to return (default: 50)", 50),
		"offset": numberProp("Pagination offset into the book list (default: 0)", 0),
	})
}

// getTranscriptSchema: no required fields; book, trackID, offset (default 0),
// limit (default 50), includeWordTimestamps (default false).
func getTranscriptSchema() *jsonschema.Schema {
	return objectSchema(nil, map[string]*jsonschema.Schema{
		"book":    stringProp("A book title or directory substring to read (e.g. \"Project Hail Mary\"). Either this or trackID is required."),
		"trackID": stringProp("A specific track/job id (UUID) to read. Takes precedence over `book`. Use when a book has multiple tracks."),
		"offset":  numberProp("Segment offset to start the page at (default: 0)", 0),
		"limit":   numberProp("Number of segments to return per page (default: 50)", 50),
		"includeWordTimestamps": boolProp("Include per-word timestamps (word/start/end, plus score/speaker when available) on each "+
			"returned segment. Default false — omitted to keep the response small; enable it only when you need "+
			"word-level timing (e.g. \"exactly when was X said\").", false),
	})
}

// getChunkContextSchema: chunkID (required), contextWindow (default 1).
func getChunkContextSchema() *jsonschema.Schema {
	return objectSchema([]string{"chunkID"}, map[string]*jsonschema.Schema{
		"chunkID": stringProp("The chunk UUID returned in the `ID` field of semantic_search_audiobooks / " +
			"text_search_audiobooks results."),
		"contextWindow": numberProp("Number of chunks before and after to include (default: 1, i.e. ~3 chunks; clamped to 0–50)", 1),
	})
}

// outputSchemaFor reflects T into a *jsonschema.Schema for a tool's
// OutputSchema, the equivalent of mcp-go's mcp.WithOutputSchema[T](). T is
// always one of the fixed structured-output types in results.go, so
// inference cannot fail in practice; a failure here would be a programming
// error caught immediately by any test that builds the server.
func outputSchemaFor[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("outputSchemaFor[%T]: %v", *new(T), err))
	}
	return s
}
