// Package fn wraps a call to a decision model as a pure function: the same
// (function, prompt, pinned model, input) always yields the same output, so a
// successful call is recorded once in fn_calls and served from there after
// (CONTRACT §1.9 "fn_calls").
//
// Invoke hashes the canonical input, serves a cached row when one exists, and
// otherwise makes the call, registers its recipe, logs the row, records a
// gen_ai span and counts earmark_model_calls_total. Nothing here records the
// input, the prompt or the output on a span, a metric or a log line: they are
// book content and live only in the fn_calls row.
package fn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/genai"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// scopeName is the instrumentation scope of the function spans.
const scopeName = "github.com/jedwards1230/earmark/internal/fn"

// Telemetry identity of the one backend today, TypeSafe System One. Semconv
// has no decision operation, so gen_ai.operation.name is a custom value.
const (
	operationName = "systemone"
	providerName  = "typesafe"
)

// earmark.* span attributes (CONTRACT §2.16).
const (
	attrFn       = attribute.Key("earmark.fn")
	attrStep     = attribute.Key("earmark.step")
	attrRecipeID = attribute.Key("earmark.recipe_id")
	attrCacheHit = attribute.Key("earmark.cache_hit")
)

// Fn describes one pure function.
type Fn struct {
	// Name is the function (fn_calls.fn, the fn metric label).
	Name string
	// Step is the pipeline step it serves (a recipe step, e.g. "decide").
	Step string
	// PromptVersion and PromptSHA256 identify the question set: the version
	// label and the hash of the exact instructions and criteria sent.
	PromptVersion string
	PromptSHA256  string
	// ModelAlias is the pinned model asked for (e.g. "jev-1.13.0").
	// ExpectedModel is the model that should answer; "" = ModelAlias.
	ModelAlias    string
	ExpectedModel string
	// Revision pins the model further when the provider exposes one.
	Revision string
	// StepVersion is bumped when earmark's logic for the function changes.
	StepVersion int
	// Params are recorded in the recipe (thresholds, question options…).
	Params map[string]any
}

// Validate refuses a function that cannot be cached safely: a missing
// identity field, an unknown step, or a model that is not a pinned version
// (config.PinnedModel: "jev-latest", "jev-preview" and "*-latest" are refused).
func (f Fn) Validate() error {
	switch {
	case strings.TrimSpace(f.Name) == "":
		return errors.New("fn: name is required")
	case !recipe.KnownStep(f.Step):
		return fmt.Errorf("fn %s: unknown step %q", f.Name, f.Step)
	case f.PromptVersion == "" || f.PromptSHA256 == "":
		return fmt.Errorf("fn %s: prompt version and sha256 are required", f.Name)
	}
	if err := config.PinnedModel(f.ModelAlias); err != nil {
		return fmt.Errorf("fn %s: %w", f.Name, err)
	}
	if f.ExpectedModel != "" {
		if err := config.PinnedModel(f.ExpectedModel); err != nil {
			return fmt.Errorf("fn %s: expected model: %w", f.Name, err)
		}
	}
	return nil
}

// New returns f after validating it.
func New(f Fn) (Fn, error) {
	if err := f.Validate(); err != nil {
		return Fn{}, err
	}
	return f, nil
}

// expected is the model a reply must come from to be served from cache.
func (f Fn) expected() string {
	if f.ExpectedModel != "" {
		return f.ExpectedModel
	}
	return f.ModelAlias
}

// Recipe is the recipe of a call answered by resolved ("" = the expected
// model). The function name is a param, so two functions of one step never
// share a recipe.
func (f Fn) Recipe(resolved string) recipe.Recipe {
	if resolved == "" {
		resolved = f.expected()
	}
	params := make(map[string]any, len(f.Params)+1)
	for k, v := range f.Params {
		params[k] = v
	}
	params["fn"] = f.Name
	return recipe.Recipe{
		Step:          f.Step,
		StepVersion:   f.StepVersion,
		CodeVersion:   recipe.CodeVersion(),
		ModelAlias:    f.ModelAlias,
		ModelResolved: resolved,
		ModelRevision: f.Revision,
		PromptVersion: f.PromptVersion,
		PromptSHA256:  f.PromptSHA256,
		Params:        params,
	}
}

// CanonicalInput is input's canonical JSON and its lowercase hex sha256.
// input is marshalled, decoded generically and re-marshalled with
// recipe.MarshalCanonical, so every object's keys are sorted (struct fields
// included), whitespace is dropped, HTML is not escaped and numbers keep their
// literal text: a struct and a map with the same JSON hash the same.
func CanonicalInput(input any) ([]byte, string, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, "", fmt.Errorf("fn input: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, "", fmt.Errorf("fn input: %w", err)
	}
	canon, err := recipe.MarshalCanonical(generic)
	if err != nil {
		return nil, "", fmt.Errorf("fn input: %w", err)
	}
	sum := sha256.Sum256(canon)
	return canon, hex.EncodeToString(sum[:]), nil
}

// Result is what a call returned.
type Result struct {
	// Output is the function's JSON output (stored in fn_calls.output).
	Output json.RawMessage
	// Model is the model that answered (the reply's "model").
	Model        string
	InputTokens  *int
	OutputTokens *int
	CostUSD      *float64
}

// Meta describes how a result was obtained.
type Meta struct {
	// CallID is this call's fn_calls row (0 when a concurrent identical call
	// already holds the cache row and this one was dropped).
	CallID      int64
	InputSHA256 string
	RecipeID    string
	CacheHit    bool
	CachedFrom  int64 // the row served, on a cache hit
	// Fallback is true when a model other than the expected one answered: the
	// result is returned and stored, but never served from cache.
	Fallback bool
	Latency  time.Duration
}

// Store is the slice of the database Invoke needs (*db.DB implements it).
type Store interface {
	RegisterRecipe(ctx context.Context, r recipe.Recipe) (string, error)
	LookupFnCache(ctx context.Context, key db.FnCacheKey, expectedModel string) (*db.FnCall, bool, error)
	InsertFnCall(ctx context.Context, c db.FnCall) (int64, bool, error)
}

// Call makes the model request. It must return a non-empty Result.Model and
// Output on success.
type Call func(ctx context.Context) (Result, error)

// Invoke runs f on input: served from fn_calls when a cached row exists,
// otherwise by call. Every invocation is logged as its own row. A call error
// is logged with its class (genai.ErrorClass) and returned; it is never
// cached.
func (f Fn) Invoke(ctx context.Context, store Store, input any, call Call) (Result, Meta, error) {
	if err := f.Validate(); err != nil {
		return Result{}, Meta{}, err
	}
	canon, sum, err := CanonicalInput(input)
	if err != nil {
		return Result{}, Meta{}, err
	}
	key := db.FnCacheKey{Fn: f.Name, PromptSHA256: f.PromptSHA256, ModelAlias: f.ModelAlias, InputSHA256: sum}
	start := time.Now()

	cached, hit, err := store.LookupFnCache(ctx, key, f.expected())
	if err != nil {
		return Result{}, Meta{InputSHA256: sum}, err
	}
	if hit {
		return f.serveCached(ctx, store, canon, sum, cached, start)
	}
	return f.callModel(ctx, store, canon, sum, call, start)
}

func (f Fn) startSpan(ctx context.Context, cacheHit bool) (context.Context, trace.Span) {
	return otel.Tracer(scopeName).Start(ctx, operationName+" "+f.ModelAlias,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.GenAIOperationNameKey.String(operationName),
			semconv.GenAIProviderNameKey.String(providerName),
			semconv.GenAIRequestModel(f.ModelAlias),
			attrFn.String(f.Name),
			attrStep.String(f.Step),
			attrCacheHit.Bool(cacheHit)))
}

func (f Fn) row(canon []byte, sum string) db.FnCall {
	return db.FnCall{
		Fn:            f.Name,
		PromptVersion: f.PromptVersion,
		PromptSHA256:  f.PromptSHA256,
		ModelAlias:    f.ModelAlias,
		ModelRevision: f.Revision,
		InputSHA256:   sum,
		Input:         canon,
	}
}

func latencyMS(d time.Duration) *int {
	ms := int(d.Milliseconds())
	return &ms
}

// serveCached logs a cache-hit row pointing at the served row and returns its
// output. No model request is made, so the row carries no tokens and no cost.
func (f Fn) serveCached(ctx context.Context, store Store, canon []byte, sum string, src *db.FnCall, start time.Time) (Result, Meta, error) {
	ctx, span := f.startSpan(ctx, true)
	defer span.End()
	span.SetAttributes(semconv.GenAIResponseModel(src.ModelResolved))
	if src.RecipeID != "" {
		span.SetAttributes(attrRecipeID.String(src.RecipeID))
	}
	meta := Meta{InputSHA256: sum, RecipeID: src.RecipeID, CacheHit: true, CachedFrom: src.ID, Latency: time.Since(start)}
	r := f.row(canon, sum)
	r.ModelResolved, r.ModelRevision, r.RecipeID = src.ModelResolved, src.ModelRevision, src.RecipeID
	r.Output, r.CacheHit, r.CachedFrom = src.Output, true, &src.ID
	r.LatencyMS = latencyMS(meta.Latency)
	id, _, err := store.InsertFnCall(ctx, r)
	genai.CountModelCall(ctx, f.Name, f.ModelAlias, genai.OutcomeCached)
	if err != nil {
		failSpan(span, err)
		return Result{}, meta, err
	}
	meta.CallID = id
	return Result{Output: src.Output, Model: src.ModelResolved}, meta, nil
}

// callModel makes the request and logs it.
func (f Fn) callModel(ctx context.Context, store Store, canon []byte, sum string, call Call, start time.Time) (Result, Meta, error) {
	ctx, span := f.startSpan(ctx, false)
	defer span.End()
	res, err := call(ctx)
	meta := Meta{InputSHA256: sum, Latency: time.Since(start)}
	if err == nil {
		err = res.check()
	}
	r := f.row(canon, sum)
	r.LatencyMS = latencyMS(meta.Latency)

	if err != nil {
		class := genai.ErrorClass(err)
		r.ErrorClass = class
		id, _, logErr := store.InsertFnCall(ctx, r)
		meta.CallID = id
		failSpan(span, err)
		genai.CountModelCall(ctx, f.Name, f.ModelAlias, genai.OutcomeError)
		return Result{}, meta, errors.Join(err, logErr)
	}

	span.SetAttributes(semconv.GenAIResponseModel(res.Model))
	if res.InputTokens != nil {
		span.SetAttributes(semconv.GenAIUsageInputTokens(*res.InputTokens))
	}
	if res.OutputTokens != nil {
		span.SetAttributes(semconv.GenAIUsageOutputTokens(*res.OutputTokens))
	}
	outcome := genai.OutcomeOK
	if !genai.SameModel(res.Model, f.expected()) {
		// Store but never serve: errored rows sit outside the cache index,
		// so this reply never takes the slot from the right model's.
		meta.Fallback, outcome, r.ErrorClass = true, genai.OutcomeFallback, db.ErrorClassModelFallback
	}
	defer genai.CountModelCall(ctx, f.Name, f.ModelAlias, outcome)

	rid, err := store.RegisterRecipe(ctx, f.Recipe(res.Model))
	if err != nil {
		failSpan(span, err)
		return Result{}, meta, err
	}
	meta.RecipeID = rid
	span.SetAttributes(attrRecipeID.String(rid))
	r.ModelResolved, r.RecipeID, r.Output = res.Model, rid, res.Output
	r.InputTokens, r.OutputTokens, r.CostUSD = res.InputTokens, res.OutputTokens, res.CostUSD
	id, _, err := store.InsertFnCall(ctx, r)
	if err != nil {
		failSpan(span, err)
		return Result{}, meta, err
	}
	meta.CallID = id
	return res, meta, nil
}

// errBadResult is a call that "succeeded" without a model or an output.
type errBadResult struct{ reason string }

func (e *errBadResult) Error() string      { return "fn: call result " + e.reason }
func (e *errBadResult) ErrorClass() string { return "invalid_reply" }

func (r Result) check() error {
	switch {
	case strings.TrimSpace(r.Model) == "":
		return &errBadResult{reason: "has no model"}
	case len(r.Output) == 0:
		return &errBadResult{reason: "has no output"}
	case !json.Valid(r.Output):
		return &errBadResult{reason: "output is not valid JSON"}
	}
	return nil
}

// failSpan marks span failed with a bounded class only — never err.Error(),
// which may echo the request.
func failSpan(span trace.Span, err error) {
	class := genai.ErrorClass(err)
	span.SetStatus(codes.Error, "call failed: "+class)
	span.SetAttributes(semconv.ErrorTypeKey.String(class))
}
