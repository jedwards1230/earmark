package log

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestJSONLogCarriesTraceIDs: a JSON record logged with a context inside a
// span carries that span's trace_id and span_id; one logged outside a span
// carries neither.
func TestJSONLogCarriesTraceIDs(t *testing.T) {
	orig := jsonFormat
	jsonFormat = true
	defer func() { jsonFormat = orig }()
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(nil)

	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ctx, span := tp.Tracer("t").Start(context.Background(), "op")
	defer span.End()

	l := NewLogger("eval")
	l.InfoContext(ctx, "inside")
	l.Info("outside")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines: %q", len(lines), buf.String())
	}
	var in, out map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &in); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &out); err != nil {
		t.Fatal(err)
	}
	sc := span.SpanContext()
	if in["trace_id"] != sc.TraceID().String() || in["span_id"] != sc.SpanID().String() {
		t.Errorf("inside span: trace_id=%v span_id=%v, want %s/%s", in["trace_id"], in["span_id"], sc.TraceID(), sc.SpanID())
	}
	if in["module"] != "eval" {
		t.Errorf("module = %v, want eval (WithAttrs must survive the trace wrapper)", in["module"])
	}
	if _, ok := out["trace_id"]; ok {
		t.Error("record outside a span carries a trace_id")
	}
}

// TestSetOutputRedirectsExistingLoggers: MCP stdio mode redirects logging to
// stderr after package-level loggers already exist; they must follow.
func TestSetOutputRedirectsExistingLoggers(t *testing.T) {
	for _, js := range []bool{false, true} {
		orig := jsonFormat
		jsonFormat = js
		l := NewLogger("early") // created before the redirect
		jsonFormat = orig

		var buf bytes.Buffer
		SetOutput(&buf)
		l.Info("after redirect")
		SetOutput(nil)
		if !strings.Contains(buf.String(), "after redirect") {
			t.Errorf("json=%v: logger created before SetOutput did not follow it (got %q)", js, buf.String())
		}
	}
}
