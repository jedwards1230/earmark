package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
)

// hungSearchDB is a DB whose text search never returns on its own: it blocks
// until its context ends, the way a search waiting on a drained pool did. It
// records whether that context carried a deadline.
type hungSearchDB struct {
	SimpleMockDB
	hadDeadline chan bool
}

func (m *hungSearchDB) TextSearch(ctx context.Context, _ string, _ int) ([]db.SearchResultWithMetadata, error) {
	_, ok := ctx.Deadline()
	m.hadDeadline <- ok
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestToolCallsRunUnderTimeout is the MCP half of the 2026-11-01 pool-deadlock
// regression: a tool call stuck on the database must fail once
// MCP_TOOL_TIMEOUT passes instead of waiting (and holding what it holds)
// forever. It drives a real in-memory tools/call through the server
// NewMCPServer builds, so the middleware wiring is exercised, not just the
// middleware function.
func TestToolCallsRunUnderTimeout(t *testing.T) {
	mock := &hungSearchDB{hadDeadline: make(chan bool, 1)}
	srv := NewMCPServer(mock, &config.Config{MCPToolTimeout: 100 * time.Millisecond})
	session := connectCapabilitiesInMemory(t, srv.server)

	// The test's own bound is far looser than the tool timeout: hitting it
	// means the call was never cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "text_search_audiobooks",
		Arguments: map[string]any{"query": "dragons"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("tool call took %v; the 100ms tool timeout did not cancel it", elapsed)
	}
	if !<-mock.hadDeadline {
		t.Error("the tool handler's context carried no deadline")
	}
	if !res.IsError {
		t.Fatal("a timed-out search returned a non-error result")
	}
	if text := resultText(res); !strings.Contains(text, context.DeadlineExceeded.Error()) {
		t.Errorf("result text = %q, want it to report the deadline", text)
	}
}

// TestToolTimeoutMiddlewareScope pins that only tools/call is bounded: other
// methods (initialize, tools/list, …) get the context they arrived with.
func TestToolTimeoutMiddlewareScope(t *testing.T) {
	cases := []struct {
		method       string
		wantDeadline bool
	}{
		{method: "tools/call", wantDeadline: true},
		{method: "tools/list", wantDeadline: false},
		{method: "initialize", wantDeadline: false},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			var got bool
			next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				_, got = ctx.Deadline()
				return nil, nil
			}
			h := toolTimeoutMiddleware(time.Minute, func(string, ...any) {})(next)
			if _, err := h(context.Background(), tc.method, &mcp.CallToolRequest{}); err != nil {
				t.Fatalf("handler: %v", err)
			}
			if got != tc.wantDeadline {
				t.Errorf("deadline set = %v, want %v", got, tc.wantDeadline)
			}
		})
	}
}

// TestToolTimeoutMiddlewareCancelsOnDeadline: the handler sees its context end
// with DeadlineExceeded once the timeout passes.
func TestToolTimeoutMiddlewareCancelsOnDeadline(t *testing.T) {
	next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	h := toolTimeoutMiddleware(20*time.Millisecond, func(string, ...any) {})(next)
	_, err := h(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "x"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestToolTimeoutMiddlewareWarnsOnlyOnOwnTimeout pins when the "exceeded its
// deadline" warning fires: only for a call that FAILED after the middleware's
// own deadline expired. A call that succeeds as the deadline passes, or that
// ran out of a shorter deadline its caller set, is not misattributed.
func TestToolTimeoutMiddlewareWarnsOnlyOnOwnTimeout(t *testing.T) {
	const own = 20 * time.Millisecond
	// waitThen blocks until the handler's context ends, then returns the
	// given outcome.
	waitThen := func(res mcp.Result, err error) mcp.MethodHandler {
		return func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
			<-ctx.Done()
			return res, err
		}
	}
	cases := []struct {
		name     string
		next     mcp.MethodHandler
		parent   time.Duration // 0 = no caller deadline
		wantWarn bool
	}{
		{name: "error result after own deadline", next: waitThen(&mcp.CallToolResult{IsError: true}, nil), wantWarn: true},
		{name: "protocol error after own deadline", next: waitThen(nil, context.DeadlineExceeded), wantWarn: true},
		{name: "success as own deadline passes", next: waitThen(&mcp.CallToolResult{}, nil), wantWarn: false},
		{name: "caller's shorter deadline", next: waitThen(&mcp.CallToolResult{IsError: true}, nil), parent: 5 * time.Millisecond, wantWarn: false},
		{
			name: "fast failure, no deadline involved",
			next: func(context.Context, string, mcp.Request) (mcp.Result, error) {
				return &mcp.CallToolResult{IsError: true}, nil
			},
			wantWarn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var warned []string
			warn := func(msg string, _ ...any) { warned = append(warned, msg) }
			ctx := context.Background()
			if tc.parent > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parent)
				defer cancel()
			}
			h := toolTimeoutMiddleware(own, warn)(tc.next)
			_, _ = h(ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "x"}})
			if got := len(warned) > 0; got != tc.wantWarn {
				t.Errorf("warned = %v (%q), want %v", got, warned, tc.wantWarn)
			}
		})
	}
}
