package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// installTestProviders points the OpenTelemetry globals at an in-memory span
// exporter and a manual metric reader for the duration of the test.
func installTestProviders(t *testing.T) (*tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prevT, prevM := otel.GetTracerProvider(), otel.GetMeterProvider()
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevT)
		otel.SetMeterProvider(prevM)
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	return spans, reader
}

func spanAttrs(s tracetest.SpanStub) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes {
		out[kv.Key] = kv.Value
	}
	return out
}

// modelCalls returns earmark_model_calls data points as "fn|model|outcome" → n.
func modelCallPoints(t *testing.T, r *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "earmark_model_calls" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				fn, _ := dp.Attributes.Value("fn")
				model, _ := dp.Attributes.Value("model")
				outcome, _ := dp.Attributes.Value("outcome")
				out[fn.AsString()+"|"+model.AsString()+"|"+outcome.AsString()] += dp.Value
			}
		}
	}
	return out
}

// TestJudgeChunk_GenAISpan: one judge call records one gen_ai client span
// (semconv v1.40.0) carrying the requested and resolved model, provider,
// server, token usage and the earmark ids — and never the prompt or the reply.
func TestJudgeChunk_GenAISpan(t *testing.T) {
	spans, reader := installTestProviders(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"anthropic/claude-haiku-4-5-20251001",
			"choices":[{"message":{"role":"assistant","content":"{\"findings\":[]}"}}],
			"usage":{"prompt_tokens":812,"completion_tokens":34}}`))
	}))
	defer srv.Close()

	chat := newOpenAIChatClient(chatConfig{BaseURL: srv.URL + "/v1", Model: "anthropic/claude-haiku-4-5"})
	j := NewJudge(chat)
	c := sampleChunk()
	if _, err := j.JudgeChunk(context.Background(), c); err != nil {
		t.Fatalf("JudgeChunk: %v", err)
	}

	got := spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(got))
	}
	s := got[0]
	if s.Name != "chat anthropic/claude-haiku-4-5" || s.SpanKind != trace.SpanKindClient {
		t.Errorf("span = %q kind %v, want \"chat anthropic/claude-haiku-4-5\" client", s.Name, s.SpanKind)
	}
	a := spanAttrs(s)
	wantRecipe, _ := j.recipeFor("anthropic/claude-haiku-4-5-20251001").ID()
	for k, v := range map[string]any{
		"gen_ai.operation.name":      "chat",
		"gen_ai.provider.name":       "anthropic",
		"gen_ai.request.model":       "anthropic/claude-haiku-4-5",
		"gen_ai.response.model":      "anthropic/claude-haiku-4-5-20251001",
		"gen_ai.usage.input_tokens":  int64(812),
		"gen_ai.usage.output_tokens": int64(34),
		"server.address":             "127.0.0.1",
		"earmark.recipe_id":          wantRecipe,
		"earmark.transcript_id":      c.TranscriptID,
		"earmark.chunk_id":           c.ChunkID,
		"earmark.step":               "propose",
	} {
		v2, ok := a[attribute.Key(k)]
		if !ok {
			t.Errorf("span missing %s", k)
			continue
		}
		if v2.AsInterface() != v {
			t.Errorf("%s = %v, want %v", k, v2.AsInterface(), v)
		}
	}
	for k, v := range a {
		if strings.Contains(v.String(), c.Text) {
			t.Errorf("span attribute %s carries chunk text", k)
		}
		if strings.Contains(string(k), "prompt") || strings.Contains(string(k), "completion") ||
			strings.Contains(string(k), "message") {
			t.Errorf("span records content attribute %s", k)
		}
	}
	if s.Status.Code == codes.Error {
		t.Errorf("status = %v, want unset/ok", s.Status)
	}
	if got := modelCallPoints(t, reader); got["judge|anthropic/claude-haiku-4-5|ok"] != 1 {
		t.Errorf("earmark_model_calls = %v, want judge|…|ok = 1", got)
	}
}

// TestJudgeChunk_GenAISpanError: a failed call marks the span as an error with
// error.type, and counts outcome=error.
func TestJudgeChunk_GenAISpanError(t *testing.T) {
	spans, reader := installTestProviders(t)
	j := NewJudge(&fakeChat{err: errors.New("endpoint down")})
	if _, err := j.JudgeChunk(context.Background(), sampleChunk()); err == nil {
		t.Fatal("JudgeChunk: want error")
	}
	got := spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(got))
	}
	if got[0].Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", got[0].Status)
	}
	if et := spanAttrs(got[0])["error.type"]; et.AsString() != "_OTHER" {
		t.Errorf("error.type = %q, want _OTHER", et.AsString())
	}
	if _, ok := spanAttrs(got[0])["earmark.recipe_id"]; ok {
		t.Error("a failed call stamped no recipe, but the span claims one")
	}
	if pts := modelCallPoints(t, reader); pts["judge|fake-judge|error"] != 1 {
		t.Errorf("earmark_model_calls = %v, want judge|fake-judge|error = 1", pts)
	}
}

// TestJudgeChunk_FallbackOutcome: a reply served by a model other than the
// registry's expected one counts as outcome=fallback.
func TestJudgeChunk_FallbackOutcome(t *testing.T) {
	_, reader := installTestProviders(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"qwen3.8","choices":[{"message":{"content":"{\"findings\":[]}"}}]}`))
	}))
	defer srv.Close()
	j := NewJudge(newOpenAIChatClient(chatConfig{BaseURL: srv.URL + "/v1", Model: "earmark-judge"}))
	j.SetModelPin(ModelPin{ExpectedModel: "anthropic/claude-haiku-4-5-20251001"})
	if _, err := j.JudgeChunk(context.Background(), sampleChunk()); err != nil {
		t.Fatal(err)
	}
	if pts := modelCallPoints(t, reader); pts["judge|earmark-judge|fallback"] != 1 {
		t.Errorf("earmark_model_calls = %v, want judge|earmark-judge|fallback = 1", pts)
	}
}

// TestJudgeChunk_ErrorSpanCarriesNoContent: an upstream validation error that
// echoes the request (FastAPI/vLLM/LiteLLM 422 "input": …) must not put the
// prompt or chunk text on the span — status, attributes or events. The span
// records only the status code class.
func TestJudgeChunk_ErrorSpanCarriesNoContent(t *testing.T) {
	spans, _ := installTestProviders(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":[{"msg":"bad","input":` + string(body) + `}]}`))
	}))
	defer srv.Close()

	j := NewJudge(newOpenAIChatClient(chatConfig{BaseURL: srv.URL + "/v1", Model: "m"}))
	c := sampleChunk()
	c.Text = "ganema said the sietch was quiet"
	_, err := j.JudgeChunk(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), c.Text) {
		t.Fatalf("precondition: the returned error should carry the echoed body, got %v", err)
	}

	got := spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(got))
	}
	s := got[0]
	if s.Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", s.Status)
	}
	if et := spanAttrs(s)["error.type"]; et.AsString() != "422" {
		t.Errorf("error.type = %q, want 422", et.AsString())
	}
	leaks := func(where, v string) {
		if strings.Contains(v, c.Text) || strings.Contains(v, "sietch") {
			t.Errorf("%s carries chunk text: %.120q", where, v)
		}
	}
	leaks("status description", s.Status.Description)
	for k, v := range spanAttrs(s) {
		leaks("attribute "+string(k), v.String())
	}
	for _, e := range s.Events {
		leaks("event "+e.Name, e.Name)
		for _, a := range e.Attributes {
			leaks("event attribute "+string(a.Key), a.Value.String())
		}
	}
}

func TestErrorClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&StatusError{Code: 503, Body: "secret"}, "503"},
		{fmt.Errorf("judge chunk x: %w", &StatusError{Code: 429}), "429"},
		{ErrThinkingOnlyResponse, "thinking_only"},
		{fmt.Errorf("chat request: %w", context.DeadlineExceeded), "timeout"},
		{context.Canceled, "canceled"},
		{errors.New("unmarshal chat response: the text"), "_OTHER"},
	} {
		if got := errorClass(tc.err); got != tc.want {
			t.Errorf("errorClass(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
