package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/genai"
)

// Pure-function call log and cache (CONTRACT §1.9 "fn_calls"). Every call an
// internal/fn function makes is one fn_calls row. The partial unique index
// fn_calls_cache_key_idx admits one successful, non-cached row per
// FnCacheKey: that row is the cache entry, and inserting with ON CONFLICT DO
// NOTHING is what stops the same input being paid for twice.

// ErrorClassModelFallback is the error_class for a reply from a model other
// than the one asked for (a gateway fallback). The row keeps the output — it
// is stored — but, like any errored row, it sits outside the cache index: it
// is never served and does not take the cache slot from a later reply by the
// right model.
const ErrorClassModelFallback = "model_fallback"

// FnCacheKey is the cache key: the columns of fn_calls_cache_key_idx.
type FnCacheKey struct {
	Fn           string
	PromptSHA256 string
	ModelAlias   string // the pinned model id asked for, e.g. "jev-1.13.0"
	InputSHA256  string // lowercase hex sha256 of the canonical input JSON
}

// FnCall is one fn_calls row. Empty strings and nil pointers are stored as
// NULL. Input and Output are JSON documents.
type FnCall struct {
	ID            int64
	Fn            string
	PromptVersion string
	PromptSHA256  string
	ModelAlias    string
	ModelResolved string // the reply's "model"; "" when the call failed before a reply
	ModelRevision string
	RecipeID      string
	InputSHA256   string
	Input         json.RawMessage
	Output        json.RawMessage // nil on error
	ErrorClass    string          // "" on success
	LatencyMS     *int
	InputTokens   *int
	OutputTokens  *int
	CostUSD       *float64
	CacheHit      bool
	CachedFrom    *int64 // the row a cache hit was served from
	CreatedAt     time.Time
}

// Key returns the row's cache key.
func (c FnCall) Key() FnCacheKey {
	return FnCacheKey{Fn: c.Fn, PromptSHA256: c.PromptSHA256, ModelAlias: c.ModelAlias, InputSHA256: c.InputSHA256}
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (k FnCacheKey) validate() error {
	switch {
	case k.Fn == "":
		return errors.New("fn call: fn is empty")
	case k.PromptSHA256 == "":
		return errors.New("fn call: prompt_sha256 is empty")
	case k.ModelAlias == "":
		return errors.New("fn call: model_alias is empty")
	case !sha256HexRe.MatchString(k.InputSHA256):
		return fmt.Errorf("fn call: input_sha256 %q is not lowercase hex sha256", k.InputSHA256)
	}
	return nil
}

// lookupFnCacheSQL repeats the index predicate so the planner can use
// fn_calls_cache_key_idx. The model_resolved test is the "store but never
// serve" rule: a row answered by any model but the expected one is never
// returned. It compares the bare model id (genai.BareModel: lower-cased, route
// prefix dropped), the same comparison the writer used to call the reply a
// fallback, so "typesafe/jev-1.13.0" serves for "jev-1.13.0".
var lookupFnCacheSQL = `
	SELECT id, fn, prompt_version, prompt_sha256, model_alias,
	       COALESCE(model_resolved, ''), COALESCE(model_revision, ''), COALESCE(recipe_id, ''),
	       input_sha256, input, output, latency_ms, input_tokens, output_tokens,
	       cost_usd::float8, created_at
	  FROM fn_calls
	 WHERE fn = $1 AND prompt_sha256 = $2 AND model_alias = $3 AND input_sha256 = $4
	   AND error_class IS NULL AND NOT cache_hit
	   AND output IS NOT NULL
	   AND lower(regexp_replace(btrim(model_resolved), '^.*/', '')) = $5
`

// LookupFnCache returns the cached successful row for key, if any. A row is
// served only when its model_resolved is the same model as expectedModel (the
// alias when expectedModel is empty; genai.SameModel): a reply from any other
// model is stored but never served.
func (db *DB) LookupFnCache(ctx context.Context, key FnCacheKey, expectedModel string) (*FnCall, bool, error) {
	return lookupFnCache(ctx, db.pool, key, expectedModel)
}

func lookupFnCache(ctx context.Context, q rowScanner, key FnCacheKey, expectedModel string) (*FnCall, bool, error) {
	if err := key.validate(); err != nil {
		return nil, false, err
	}
	if expectedModel == "" {
		expectedModel = key.ModelAlias
	}
	expectedModel = genai.BareModel(expectedModel)
	var (
		c      FnCall
		input  []byte
		output []byte
	)
	err := q.QueryRow(ctx, lookupFnCacheSQL, key.Fn, key.PromptSHA256, key.ModelAlias, key.InputSHA256, expectedModel).
		Scan(&c.ID, &c.Fn, &c.PromptVersion, &c.PromptSHA256, &c.ModelAlias,
			&c.ModelResolved, &c.ModelRevision, &c.RecipeID,
			&c.InputSHA256, &input, &output, &c.LatencyMS, &c.InputTokens, &c.OutputTokens,
			&c.CostUSD, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lookup %s fn cache: %w", key.Fn, err)
	}
	c.Input, c.Output = input, output
	return &c, true, nil
}

// insertFnCallSQL logs one call. ON CONFLICT DO NOTHING targets the partial
// cache index (the predicate must match it for Postgres to infer it): a
// second successful row for a cached key is dropped, never an update — log
// rows are immutable. Errored and cache-hit rows are outside the index and
// always insert.
var insertFnCallSQL = `
	INSERT INTO fn_calls (fn, prompt_version, prompt_sha256, model_alias, model_resolved,
	                      model_revision, recipe_id, input_sha256, input, output, error_class,
	                      latency_ms, input_tokens, output_tokens, cost_usd, cache_hit, cached_from)
	VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), $8, $9::jsonb,
	        $10::jsonb, NULLIF($11, ''), $12, $13, $14, $15, $16, $17)
	ON CONFLICT (fn, prompt_sha256, model_alias, input_sha256)
	    WHERE error_class IS NULL AND NOT cache_hit
	DO NOTHING
	RETURNING id
`

// InsertFnCall logs c and returns its id. inserted is false (and id 0) when
// c is a successful non-cached call whose key already has a cache row — the
// conflict a concurrent identical call produces; the existing row stands.
func (db *DB) InsertFnCall(ctx context.Context, c FnCall) (id int64, inserted bool, err error) {
	return insertFnCall(ctx, db.pool, c)
}

func insertFnCall(ctx context.Context, q rowScanner, c FnCall) (int64, bool, error) {
	if err := c.Key().validate(); err != nil {
		return 0, false, err
	}
	if c.PromptVersion == "" {
		return 0, false, errors.New("fn call: prompt_version is empty")
	}
	if len(c.Input) == 0 {
		return 0, false, errors.New("fn call: input is empty")
	}
	if c.CacheHit != (c.CachedFrom != nil) {
		return 0, false, errors.New("fn call: cached_from must be set exactly when cache_hit is")
	}
	if c.ErrorClass == "" && len(c.Output) == 0 {
		return 0, false, errors.New("fn call: a successful call needs an output")
	}
	var output *string
	if len(c.Output) > 0 {
		s := string(c.Output)
		output = &s
	}
	var id int64
	err := q.QueryRow(ctx, insertFnCallSQL,
		c.Fn, c.PromptVersion, c.PromptSHA256, c.ModelAlias, c.ModelResolved,
		c.ModelRevision, c.RecipeID, c.InputSHA256, string(c.Input), output, c.ErrorClass,
		c.LatencyMS, c.InputTokens, c.OutputTokens, c.CostUSD, c.CacheHit, c.CachedFrom).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert %s fn call: %w", c.Fn, err)
	}
	return id, true, nil
}
