package mcp

import "net/http"

// apiRoute describes one route of the JSON control API (/api/v1/*).
//
// The apiRoutes table below is the single source of truth for both
// registerAPIRoutes and the OpenAPI sync test (openapi_sync_test.go), so
// docs/openapi.yaml cannot silently drift from the registered surface: adding a
// route here without documenting it (or vice versa) fails TestOpenAPISync.
type apiRoute struct {
	Method  string
	Pattern string
	// mutating selects the auth wrapper: true → requireToken, which demands
	// "Authorization: Bearer <CONTROL_API_TOKEN>" (401 when absent/wrong) and
	// fails closed with 503 when no token is configured on the server. false →
	// registered bare; reads are deliberately unauthenticated (§2.12).
	mutating bool
	// handler is the method expression for the route's handler, bound to the
	// receiver at registration time.
	handler func(*MCPServer, http.ResponseWriter, *http.Request)
}

// apiRoutes is the complete JSON control API surface (CONTRACT §2.12).
//
// Deliberately scoped: the htmx dashboard routes (the page shells, their
// "/…/data" fragments, and the "POST /actions/*" mutations) return HTML
// fragments, not JSON, and are NOT part of this contract — they are excluded
// from both this table and docs/openapi.yaml on purpose. Likewise the probes
// (/health, /healthz, /readyz), /metrics, /static/*, and the MCP transport at
// /mcp, none of which are control-API operations.
//
// Keep in sync with docs/openapi.yaml — TestOpenAPISync enforces that both
// directions match exactly.
var apiRoutes = []apiRoute{
	// Read-only routes — unauthenticated; the pipeline snapshot is non-sensitive
	// and the spec is the public contract.
	{Method: "GET", Pattern: "/api/v1/status", handler: (*MCPServer).handleAPIStatus},
	{Method: "GET", Pattern: "/api/v1/pipeline/pause", handler: (*MCPServer).handleAPIPauseGet},
	{Method: "GET", Pattern: "/api/v1/openapi.yaml", handler: (*MCPServer).handleOpenAPISpec},

	// Mutating routes — bearer-token guarded, fail-closed (503) when
	// CONTROL_API_TOKEN is unset.
	{Method: "PUT", Pattern: "/api/v1/pipeline/pause", mutating: true, handler: (*MCPServer).handleAPIPausePut},
	{Method: "POST", Pattern: "/api/v1/pipeline/run", mutating: true, handler: (*MCPServer).handleAPIRun},
	{Method: "DELETE", Pattern: "/api/v1/pipeline/run", mutating: true, handler: (*MCPServer).handleAPIRunClear},
	{Method: "POST", Pattern: "/api/v1/runner/update", mutating: true, handler: (*MCPServer).handleAPIRunnerUpdate},
}

// registerAPIRoutes registers every route in apiRoutes on mux. Mutating routes
// are wrapped in requireToken; read routes are registered bare. Each pattern is
// a specific method+path, so none of them conflict with the "/" catch-all page
// handler or the method-less "/mcp" handler.
func (s *MCPServer) registerAPIRoutes(mux *http.ServeMux) {
	for _, rt := range apiRoutes {
		fn := rt.handler
		h := func(w http.ResponseWriter, r *http.Request) { fn(s, w, r) }
		if rt.mutating {
			h = s.requireToken(h)
		}
		mux.HandleFunc(rt.Method+" "+rt.Pattern, h)
	}
}
