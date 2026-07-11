package mcp

import (
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file holds the small CallToolResult builders every tool handler in
// tools.go/types.go shares — the official-SDK equivalents of mcp-go's
// mcp.NewToolResultError / mcp.NewToolResultStructured.

// errorResult builds a tool-level error CallToolResult: IsError=true with a
// text message. Handlers return this alongside a NIL Go error — a non-nil Go
// error would surface as a protocol-level error instead of a tool result the
// calling model can read and react to (the IsError:true convention this
// migration must preserve).
func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// structuredResult builds a successful CallToolResult carrying BOTH a
// human-readable text fallback (Content[0], the spec-required back-compat
// representation) and the typed StructuredContent value.
func structuredResult(structured any, text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: structured,
	}
}
