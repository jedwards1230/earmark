package eval

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	// Pinned: the GenAI semantic conventions are still "development", so
	// earmark follows exactly semconv v1.40.0 (gen_ai.provider.name, not the
	// older gen_ai.system) and treats its own earmark.* attributes as
	// authoritative (CONTRACT §2.16).
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// scopeName is the instrumentation scope of the judge's spans and metrics.
const scopeName = "github.com/jedwards1230/earmark/internal/eval"

// judgeFn is the fn label / earmark.fn attribute of the judge's model calls
// (earmark_model_calls_total{fn,model,outcome}).
const judgeFn = "judge"

// Model-call outcomes (earmark_model_calls_total{outcome}).
const (
	outcomeOK       = "ok"
	outcomeError    = "error"
	outcomeFallback = "fallback"
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

// The tracer and counter are looked up from the OpenTelemetry globals on each
// call (once per model request, which takes seconds), so they always follow
// whatever internal/telemetry — or a test — installed last; no-op until then.
func tracer() trace.Tracer { return otel.Tracer(scopeName) }

func countModelCall(ctx context.Context, model, outcome string) {
	c, err := otel.Meter(scopeName).Int64Counter("earmark_model_calls",
		metric.WithDescription("Model calls by LLM function, requested model and outcome (ok, error, fallback)."))
	if err != nil {
		otel.Handle(err)
		return
	}
	c.Add(ctx, 1, metric.WithAttributes(
		attribute.String("fn", judgeFn),
		attribute.String("model", model),
		attribute.String("outcome", outcome)))
}

// startChatSpan opens the gen_ai client span for one judge call. Prompt and
// completion content are never recorded.
func startChatSpan(ctx context.Context, model string, ep Endpoint, c chunkRef) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameChat,
		semconv.GenAIRequestModel(model),
		// openAIChatClient always sends temperature 0.
		semconv.GenAIRequestTemperature(0),
		attrStep.String("propose"),
		attrFn.String(judgeFn),
		attrTranscriptID.String(c.transcriptID),
		attrChunkID.String(c.chunkID),
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
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(semconv.ErrorType(err))
	} else {
		if comp.ResolvedModel != "" {
			span.SetAttributes(semconv.GenAIResponseModel(comp.ResolvedModel))
			if expected != "" && comp.ResolvedModel != expected {
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
