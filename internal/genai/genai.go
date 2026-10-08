// Package genai holds the telemetry helpers shared by earmark's model callers —
// the eval judge (internal/eval) and the pure-function wrapper (internal/fn):
// the earmark_model_calls counter, the bounded error classification that goes
// on spans and fn_calls rows, endpoint description for gen_ai span attributes,
// and model-id comparison (CONTRACT §2.16).
//
// Leaf package: OpenTelemetry only, no database or HTTP client.
package genai

import (
	"context"
	"errors"
	"net"
	neturl "net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// scopeName is the instrumentation scope of the shared model-call counter.
// One instrument for every caller: the Prometheus exporter emits no
// otel_scope_* labels, so two scopes registering earmark_model_calls would
// collide on /metrics.
const scopeName = "github.com/jedwards1230/earmark/internal/genai"

// Model-call outcomes (earmark_model_calls_total{outcome}).
const (
	OutcomeOK       = "ok"
	OutcomeError    = "error"
	OutcomeFallback = "fallback"
	// OutcomeCached is a pure-function call served from fn_calls (no model
	// request was made).
	OutcomeCached = "cached"
)

// CountModelCall increments earmark_model_calls_total{fn,model,outcome}. model
// is the requested model. The counter is looked up from the OpenTelemetry
// globals on each call, so it follows whatever internal/telemetry — or a test —
// installed last; a no-op until then.
func CountModelCall(ctx context.Context, fn, model, outcome string) {
	c, err := otel.Meter(scopeName).Int64Counter("earmark_model_calls",
		metric.WithDescription("Model calls by LLM function, requested model and outcome (ok, error, fallback, cached)."))
	if err != nil {
		otel.Handle(err)
		return
	}
	c.Add(ctx, 1, metric.WithAttributes(
		attribute.String("fn", fn),
		attribute.String("model", model),
		attribute.String("outcome", outcome)))
}

// Classifier is implemented by errors that know their own bounded class
// (e.g. a typed client error: "429", "timeout", "invalid_reply").
type Classifier interface {
	ErrorClass() string
}

// ErrorClass reduces an error to a bounded, content-free label for
// error.type and fn_calls.error_class: an error's own class (Classifier),
// "timeout", "canceled", else semconv's "_OTHER". Never err.Error(): an
// upstream error body can echo the request.
func ErrorClass(err error) string {
	var c Classifier
	var ne net.Error
	switch {
	case errors.As(err, &c):
		return c.ErrorClass()
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return semconv.ErrorTypeOther.Value.AsString()
	}
}

// Endpoint describes where a client sends requests, for telemetry
// (gen_ai.provider.name, server.address, server.port).
type Endpoint struct {
	Provider string
	Host     string
	Port     int
}

// RouteProvider is the LiteLLM-style route prefix of a model id
// ("anthropic/…" → "anthropic"), else fallback.
func RouteProvider(model, fallback string) string {
	if i := strings.Index(model, "/"); i > 0 {
		return strings.ToLower(model[:i])
	}
	return fallback
}

// Server is the host and port a base URL addresses; the port defaults by
// scheme. An unparseable URL yields ("", 0).
func Server(baseURL string) (string, int) {
	u, err := neturl.Parse(baseURL)
	if err != nil {
		return "", 0
	}
	if p, err := strconv.Atoi(u.Port()); err == nil {
		return u.Hostname(), p
	}
	switch u.Scheme {
	case "https":
		return u.Hostname(), 443
	case "http":
		return u.Hostname(), 80
	}
	return u.Hostname(), 0
}

// SameModel compares model ids ignoring a router's route prefix and case: a
// registry pin "anthropic/claude-haiku-4-5-20251001" and a LiteLLM response
// reporting "claude-haiku-4-5-20251001" name the same model, so the call is
// not a fallback.
func SameModel(a, b string) bool {
	return BareModel(a) == BareModel(b)
}

// BareModel is a model id lower-cased, trimmed, with any route prefix
// removed — the form SameModel compares.
func BareModel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}
