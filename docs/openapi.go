// Package docs embeds documentation assets that the earmark binary serves at
// runtime, so a deployed instance can expose its own machine-readable contract
// rather than pointing callers at a repo file.
package docs

import _ "embed"

// OpenAPISpec is the OpenAPI 3.1 contract for the JSON control API (/api/v1),
// served verbatim at GET /api/v1/openapi.yaml. docs/openapi.yaml is its single
// source of truth; TestOpenAPISync (internal/mcp) keeps that file in exact sync
// with the registered route table (internal/mcp/routes.go).
//
//go:embed openapi.yaml
var OpenAPISpec []byte
