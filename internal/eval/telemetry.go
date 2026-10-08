package eval

import (
	"context"
	"errors"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	// Pinned: the GenAI semantic conventions are still "development", so
	// earmark follows exactly semconv v1.40.0 (gen_ai.provider.name, not the
	// older gen_ai.system) and treats its own earmark.* attributes as
	// authoritative (CONTRACT §2.16).
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/jedwards1230/earmark/internal/genai"
)

// scopeName is the instrumentation scope of the judge's spans and metrics.
const scopeName = "github.com/jedwards1230/earmark/internal/eval"

// judgeFn is the fn label / earmark.fn attribute of the judge's model calls
// (earmark_model_calls_total{fn,model,outcome}).
const judgeFn = "judge"

// Model-call outcomes (earmark_model_calls_total{outcome}).
const (
	outcomeOK       = genai.OutcomeOK
	outcomeError    = genai.OutcomeError
	outcomeFallback = genai.OutcomeFallback
)

// earmark.* span attributes. Book, chunk and recipe ids live on spans and
// logs, never on metric labels (the cardinality rule, CONTRACT §2.16).
const (
	attrRecipeID     = attribute.Key("earmark.recipe_id")
	attrTranscriptID = attribute.Key("earmark.transcript_id")
	attrChunkID      = attribute.Key("earmark.chunk_id")
	attrStep         = attribute.Key("earmark.step")
	attrFn           = attribute.Key("earmark.fn")
)

// The tracer is looked up from the OpenTelemetry globals on each call (once
// per model request, which takes seconds), so it always follows whatever
// internal/telemetry — or a test — installed last; no-op until then. The
// earmark_model_calls counter is the shared one in internal/genai.
func tracer() trace.Tracer { return otel.Tracer(scopeName) }

func countModelCall(ctx context.Context, model, outcome string) {
	genai.CountModelCall(ctx, judgeFn, model, outcome)
}

// startChatSpan opens the gen_ai client span for one judge call. temperature
// is what the client sends; nil (omitted from the request) omits the
// gen_ai.request.temperature attribute. Prompt and completion content are
// never recorded.
func startChatSpan(ctx context.Context, model string, temperature *float64, ep Endpoint, c chunkRef) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameChat,
		semconv.GenAIRequestModel(model),
		attrStep.String("propose"),
		attrFn.String(judgeFn),
		attrTranscriptID.String(c.transcriptID),
		attrChunkID.String(c.chunkID),
	}
	if temperature != nil {
		attrs = append(attrs, semconv.GenAIRequestTemperature(*temperature))
	}
	if ep.Provider != "" {
		attrs = append(attrs, semconv.GenAIProviderNameKey.String(ep.Provider))
	}
	if ep.Host != "" {
		attrs = append(attrs, semconv.ServerAddress(ep.Host))
		if ep.Port > 0 {
			attrs = append(attrs, semconv.ServerPort(ep.Port))
		}
	}
	return tracer().Start(ctx, "chat "+model,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
}

// chunkRef carries the ids a judge span is tagged with.
type chunkRef struct {
	transcriptID string
	chunkID      string
}

// endChatSpan records the call's result on span and counts it: the resolved
// model, token usage, the recipe that stamped it, and the error status.
func endChatSpan(ctx context.Context, span trace.Span, model, expected string, comp Completion, recipeID string, err error) {
	defer span.End()
	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
		// Never err.Error(): an upstream error body can echo the request —
		// the prompt and the chunk text (CONTRACT §2.16). Only a bounded
		// classification goes on the span; the full error is still returned
		// to the caller and logged there.
		class := errorClass(err)
		span.SetStatus(codes.Error, "chat call failed: "+class)
		span.SetAttributes(semconv.ErrorTypeKey.String(class))
	} else {
		if comp.ResolvedModel != "" {
			span.SetAttributes(semconv.GenAIResponseModel(comp.ResolvedModel))
			if expected != "" && !SameModel(comp.ResolvedModel, expected) {
				outcome = outcomeFallback
			}
		}
		if comp.HasUsage {
			span.SetAttributes(
				semconv.GenAIUsageInputTokens(comp.InputTokens),
				semconv.GenAIUsageOutputTokens(comp.OutputTokens))
		}
		if recipeID != "" {
			span.SetAttributes(attrRecipeID.String(recipeID))
		}
	}
	countModelCall(ctx, model, outcome)
}

// errorClass reduces a chat error to a bounded, content-free label for
// error.type: the HTTP status code ("422"), one of the unusable-reply classes
// ("thinking_only", "empty", "truncated", "refusal"), else the shared
// classification (genai.ErrorClass): "timeout", "canceled" or semconv's
// "_OTHER".
func errorClass(err error) string {
	var se *StatusError
	switch {
	case errors.As(err, &se):
		return strconv.Itoa(se.Code)
	case errors.Is(err, ErrThinkingOnlyResponse):
		return "thinking_only"
	case errors.Is(err, ErrEmptyResponse):
		return "empty"
	case errors.Is(err, ErrTruncatedResponse):
		return "truncated"
	case errors.Is(err, ErrRefusalResponse):
		return "refusal"
	default:
		return genai.ErrorClass(err)
	}
}

// SameModel compares model ids ignoring a router's route prefix and case
// (genai.SameModel). Shared with the dashboard's Models page (internal/mcp).
func SameModel(a, b string) bool { return genai.SameModel(a, b) }
