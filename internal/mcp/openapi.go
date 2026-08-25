package mcp

import (
	"net/http"

	"github.com/jedwards1230/earmark/docs"
)

// handleOpenAPISpec serves GET /api/v1/openapi.yaml — the embedded OpenAPI 3.1
// document describing the JSON control API, so a deployed instance is
// self-describing rather than pointing callers at a repo file. The bytes are
// written verbatim from the //go:embed of docs/openapi.yaml (no templating, no
// envelope), which is what lets a client checksum the contract it was served.
//
// Unauthenticated on purpose: the contract is public, and a non-browser tool
// must be able to fetch it with no token before it can obtain one.
func (s *MCPServer) handleOpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(docs.OpenAPISpec)
}
