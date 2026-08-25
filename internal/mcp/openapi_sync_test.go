package mcp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jedwards1230/earmark/docs"
	"github.com/jedwards1230/earmark/internal/config"
)

// specPath is the OpenAPI document, relative to this package directory.
const specPath = "../../docs/openapi.yaml"

// httpMethods are the OpenAPI operation keys treated as routes. Other keys under
// a path item (notably "parameters") are skipped.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"patch": true, "head": true, "options": true, "trace": true,
}

// TestOpenAPISync asserts docs/openapi.yaml documents exactly the routes
// registered in apiRoutes — no undocumented routes, no phantom spec paths.
// It is the drift guard: adding, removing, or renaming a control-API route
// without updating the spec (or vice versa) fails this test.
//
// Patterns are compared literally: every control-API route is a fixed path with
// no Go 1.22 "{name...}" wildcard, so no canonicalization step is needed. Add
// one (mapping "{name...}" → "{name}", which is all OpenAPI can express) if a
// wildcard route is ever introduced.
func TestOpenAPISync(t *testing.T) {
	// Routes registered by the code.
	codeRoutes := make(map[string]bool, len(apiRoutes))
	for _, rt := range apiRoutes {
		codeRoutes[rt.Method+" "+rt.Pattern] = true
	}

	// Operations documented by the spec.
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	specRoutes := make(map[string]bool)
	for path, item := range spec.Paths {
		for key := range item {
			if !httpMethods[strings.ToLower(key)] {
				continue
			}
			specRoutes[strings.ToUpper(key)+" "+path] = true
		}
	}

	if missing := routeDiff(codeRoutes, specRoutes); len(missing) > 0 {
		t.Errorf("routes registered in code but missing from %s:\n  %s",
			specPath, strings.Join(missing, "\n  "))
	}
	if phantom := routeDiff(specRoutes, codeRoutes); len(phantom) > 0 {
		t.Errorf("routes documented in %s but not registered in code:\n  %s",
			specPath, strings.Join(phantom, "\n  "))
	}
}

// TestAPIRoutesRegistered asserts every route in apiRoutes actually resolves on
// a mux built by registerAPIRoutes — so the table cannot drift from the mux
// either (a spec-and-table pair that agree with each other but not with the
// server would still be a lie).
func TestAPIRoutesRegistered(t *testing.T) {
	s := NewMCPServer(&SimpleMockDB{}, &config.Config{ControlAPIToken: testToken})
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	for _, rt := range apiRoutes {
		t.Run(rt.Method+" "+rt.Pattern, func(t *testing.T) {
			req := httptest.NewRequest(rt.Method, rt.Pattern, nil)
			h, pattern := mux.Handler(req)
			if h == nil || pattern == "" {
				t.Fatalf("no handler registered for %s %s", rt.Method, rt.Pattern)
			}
			if want := rt.Method + " " + rt.Pattern; pattern != want {
				t.Errorf("matched pattern = %q, want %q", pattern, want)
			}
		})
	}
}

// TestOpenAPISpecServedVerbatim asserts the spec endpoint honours its two
// contract claims: it needs no token (a tool must be able to read the contract
// before it holds one), and the body is the embedded document byte-for-byte, so
// a client can checksum what the running build was compiled against.
func TestOpenAPISpecServedVerbatim(t *testing.T) {
	// A token IS configured, to prove reads stay open regardless.
	h := muxWithToken(&SimpleMockDB{}, testToken)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/openapi.yaml", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	if !bytes.Equal(w.Body.Bytes(), docs.OpenAPISpec) {
		t.Errorf("body is not the embedded spec verbatim (%d bytes served, %d embedded)",
			w.Body.Len(), len(docs.OpenAPISpec))
	}
}

// routeDiff returns the sorted keys present in a but not in b.
func routeDiff(a, b map[string]bool) []string {
	var only []string
	for k := range a {
		if !b[k] {
			only = append(only, k)
		}
	}
	sort.Strings(only)
	return only
}
