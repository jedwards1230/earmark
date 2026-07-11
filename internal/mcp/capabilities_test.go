package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/earmark/internal/config"
)

// connectCapabilitiesInMemory wires a client to srv over an in-memory
// transport (net.Pipe under the hood — no sockets, no stdio) and returns the
// connected client session. The caller must close the session.
func connectCapabilitiesInMemory(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	if _, err := srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "capabilities-test-client", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// TestInitializeCapabilities runs a real in-memory MCP `initialize` handshake
// against the server NewMCPServer builds, and asserts the negotiated
// capabilities carry the parity contract this migration must preserve
// (see docs/projects/mcp-official-sdk-migration-plan.md, parity rule 3):
//
//   - `tools` MUST be present and truthy — earmark registers 5 tools, and the
//     SDK auto-derives {"listChanged":true} from them since
//     ServerOptions.Capabilities is left nil in NewMCPServer.
//   - `resources` MUST be ABSENT — earmark registers no MCP resources today,
//     and the SDK correctly omits the key entirely rather than stamping a
//     falsy `resources: {}` (which ContextForge's federation gate,
//     `if capabilities.get("resources")`, would treat as "no resources" —
//     the exact trap a hand-set Capabilities could reintroduce).
//
// The full capabilities JSON is logged (via -v) so the owner can quote it in
// the migration digest.
func TestInitializeCapabilities(t *testing.T) {
	srv := NewMCPServer(&SimpleMockDB{}, &config.Config{})
	session := connectCapabilitiesInMemory(t, srv.server)

	result := session.InitializeResult()
	if result == nil {
		t.Fatal("InitializeResult() is nil after a successful Connect")
	}

	raw, err := json.MarshalIndent(result.Capabilities, "", "  ")
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	t.Logf("initialize capabilities:\n%s", raw)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal capabilities: %v", err)
	}

	toolsRaw, ok := m["tools"]
	if !ok {
		t.Fatal("capabilities.tools is absent — want present (5 tools registered)")
	}
	var toolsObj map[string]any
	if err := json.Unmarshal(toolsRaw, &toolsObj); err != nil {
		t.Fatalf("unmarshal capabilities.tools: %v", err)
	}
	if len(toolsObj) == 0 {
		t.Errorf("capabilities.tools is an empty object %s — want a truthy object (e.g. {\"listChanged\":true}); a bare {} is Python-falsy and would break ContextForge federation", toolsRaw)
	}

	if resourcesRaw, ok := m["resources"]; ok {
		t.Errorf("capabilities.resources is present (%s) — want ABSENT (earmark registers no resources); a falsy `resources: {}` breaks ContextForge's `if capabilities.get(\"resources\")` federation gate", resourcesRaw)
	}
}
