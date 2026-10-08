package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"
)

var (
	fnKey = FnCacheKey{
		Fn:           "should_apply",
		PromptSHA256: "00ff",
		ModelAlias:   "jev-1.13.0",
		InputSHA256:  strings.Repeat("a1", 32),
	}
	fnLookupColumns = []string{
		"id", "fn", "prompt_version", "prompt_sha256", "model_alias", "model_resolved",
		"model_revision", "recipe_id", "input_sha256", "input", "output", "latency_ms",
		"input_tokens", "output_tokens", "cost_usd", "created_at",
	}
)

func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock
}

func ptr[T any](v T) *T { return &v }

func anyArgs(n int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	return args
}

// TestLookupFnCacheSQL pins the serve rule at the source: only successful,
// non-cached rows with an output, answered by the expected model, and the
// WHERE repeats the partial index's predicate so the index is usable.
func TestLookupFnCacheSQL(t *testing.T) {
	sql := norm(lookupFnCacheSQL)
	for _, want := range []string{
		"WHERE fn = $1 AND prompt_sha256 = $2 AND model_alias = $3 AND input_sha256 = $4",
		"error_class IS NULL AND NOT cache_hit",
		"output IS NOT NULL",
		"model_resolved = $5",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("lookupFnCacheSQL is missing %q:\n%s", want, sql)
		}
	}
	if !strings.HasPrefix(strings.ToUpper(sql), "SELECT") {
		t.Errorf("lookupFnCacheSQL must be read-only:\n%s", sql)
	}
}

// TestInsertFnCallSQLNeverOverwrites: log rows are immutable — a duplicate
// cache key is dropped, never updated.
func TestInsertFnCallSQLNeverOverwrites(t *testing.T) {
	sql := norm(insertFnCallSQL)
	if !strings.Contains(sql, "ON CONFLICT (fn, prompt_sha256, model_alias, input_sha256) WHERE error_class IS NULL AND NOT cache_hit DO NOTHING") {
		t.Errorf("insertFnCallSQL must DO NOTHING on the cache index:\n%s", sql)
	}
	if strings.Contains(strings.ToUpper(sql), "DO UPDATE") {
		t.Errorf("insertFnCallSQL updates on conflict:\n%s", sql)
	}
}

func TestLookupFnCache(t *testing.T) {
	created := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	t.Run("hit returns the row", func(t *testing.T) {
		mock := newMockPool(t)
		mock.ExpectQuery("FROM fn_calls").
			WithArgs(fnKey.Fn, fnKey.PromptSHA256, fnKey.ModelAlias, fnKey.InputSHA256, "jev-1.13.0").
			WillReturnRows(pgxmock.NewRows(fnLookupColumns).AddRow(
				int64(42), fnKey.Fn, "decide@v1", fnKey.PromptSHA256, fnKey.ModelAlias, "jev-1.13.0",
				"", strings.Repeat("b", 64), fnKey.InputSHA256, []byte(`{"x":1}`), []byte(`{"noul":0.99}`),
				ptr(812), ptr(391), ptr(67), ptr(0.00001642), created))
		got, ok, err := lookupFnCache(context.Background(), mock, fnKey, "")
		if err != nil || !ok {
			t.Fatalf("lookupFnCache = %v, %v, %v", got, ok, err)
		}
		if got.ID != 42 || string(got.Output) != `{"noul":0.99}` || string(got.Input) != `{"x":1}` ||
			*got.InputTokens != 391 || *got.CostUSD != 0.00001642 || !got.CreatedAt.Equal(created) {
			t.Errorf("row = %+v", got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("expected model overrides the alias", func(t *testing.T) {
		mock := newMockPool(t)
		// A row answered by another model is filtered by the query ($5), so
		// the expected model must be what is bound — not the alias.
		mock.ExpectQuery("FROM fn_calls").
			WithArgs(fnKey.Fn, fnKey.PromptSHA256, fnKey.ModelAlias, fnKey.InputSHA256, "typesafe/jev-1.13.0").
			WillReturnError(pgx.ErrNoRows)
		got, ok, err := lookupFnCache(context.Background(), mock, fnKey, "typesafe/jev-1.13.0")
		if err != nil || ok || got != nil {
			t.Fatalf("miss = %v, %v, %v", got, ok, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("query error is wrapped", func(t *testing.T) {
		mock := newMockPool(t)
		boom := errors.New("boom")
		mock.ExpectQuery("FROM fn_calls").WithArgs(anyArgs(5)...).WillReturnError(boom)
		if _, ok, err := lookupFnCache(context.Background(), mock, fnKey, ""); !errors.Is(err, boom) || ok {
			t.Fatalf("err = %v, ok = %v", err, ok)
		}
	})

	t.Run("invalid key never queries", func(t *testing.T) {
		mock := newMockPool(t)
		bad := fnKey
		bad.InputSHA256 = strings.ToUpper(fnKey.InputSHA256)
		if _, _, err := lookupFnCache(context.Background(), mock, bad, ""); err == nil {
			t.Fatal("uppercase input_sha256 accepted")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}

func validFnCall() FnCall {
	return FnCall{
		Fn:            fnKey.Fn,
		PromptVersion: "decide@v1",
		PromptSHA256:  fnKey.PromptSHA256,
		ModelAlias:    fnKey.ModelAlias,
		ModelResolved: "jev-1.13.0",
		RecipeID:      strings.Repeat("b", 64),
		InputSHA256:   fnKey.InputSHA256,
		Input:         json.RawMessage(`{"x":1}`),
		Output:        json.RawMessage(`{"noul":0.99}`),
		LatencyMS:     ptr(812),
		InputTokens:   ptr(391),
		OutputTokens:  ptr(67),
		CostUSD:       ptr(0.00001642),
	}
}

func TestInsertFnCall(t *testing.T) {
	t.Run("success inserts and returns the id", func(t *testing.T) {
		mock := newMockPool(t)
		c := validFnCall()
		out := string(c.Output)
		mock.ExpectQuery("INSERT INTO fn_calls").
			WithArgs(c.Fn, c.PromptVersion, c.PromptSHA256, c.ModelAlias, c.ModelResolved,
				"", c.RecipeID, c.InputSHA256, `{"x":1}`, &out, "",
				c.LatencyMS, c.InputTokens, c.OutputTokens, c.CostUSD, false, (*int64)(nil)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(7)))
		id, inserted, err := insertFnCall(context.Background(), mock, c)
		if err != nil || !inserted || id != 7 {
			t.Fatalf("insertFnCall = %d, %v, %v", id, inserted, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("conflict on the cache key is not an error", func(t *testing.T) {
		mock := newMockPool(t)
		mock.ExpectQuery("INSERT INTO fn_calls").WithArgs(anyArgs(17)...).WillReturnError(pgx.ErrNoRows)
		id, inserted, err := insertFnCall(context.Background(), mock, validFnCall())
		if err != nil || inserted || id != 0 {
			t.Fatalf("conflict = %d, %v, %v", id, inserted, err)
		}
	})

	t.Run("error row binds NULL output and its class", func(t *testing.T) {
		mock := newMockPool(t)
		c := validFnCall()
		c.Output, c.ModelResolved, c.ErrorClass = nil, "", "rate_limited"
		c.InputTokens, c.OutputTokens, c.CostUSD = nil, nil, nil
		mock.ExpectQuery("INSERT INTO fn_calls").
			WithArgs(c.Fn, c.PromptVersion, c.PromptSHA256, c.ModelAlias, "",
				"", c.RecipeID, c.InputSHA256, `{"x":1}`, (*string)(nil), "rate_limited",
				c.LatencyMS, (*int)(nil), (*int)(nil), (*float64)(nil), false, (*int64)(nil)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(8)))
		if _, inserted, err := insertFnCall(context.Background(), mock, c); err != nil || !inserted {
			t.Fatalf("error row = %v, %v", inserted, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("cache hit binds cached_from", func(t *testing.T) {
		mock := newMockPool(t)
		c := validFnCall()
		c.CacheHit, c.CachedFrom = true, ptr(int64(7))
		out := string(c.Output)
		mock.ExpectQuery("INSERT INTO fn_calls").
			WithArgs(c.Fn, c.PromptVersion, c.PromptSHA256, c.ModelAlias, c.ModelResolved,
				"", c.RecipeID, c.InputSHA256, `{"x":1}`, &out, "",
				c.LatencyMS, c.InputTokens, c.OutputTokens, c.CostUSD, true, c.CachedFrom).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(9)))
		if id, inserted, err := insertFnCall(context.Background(), mock, c); err != nil || !inserted || id != 9 {
			t.Fatalf("cache-hit row = %d, %v, %v", id, inserted, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("query error is wrapped", func(t *testing.T) {
		mock := newMockPool(t)
		boom := errors.New("boom")
		mock.ExpectQuery("INSERT INTO fn_calls").WithArgs(anyArgs(17)...).WillReturnError(boom)
		if _, _, err := insertFnCall(context.Background(), mock, validFnCall()); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})

	invalid := map[string]func(*FnCall){
		"empty fn":               func(c *FnCall) { c.Fn = "" },
		"empty prompt version":   func(c *FnCall) { c.PromptVersion = "" },
		"empty model alias":      func(c *FnCall) { c.ModelAlias = "" },
		"short input hash":       func(c *FnCall) { c.InputSHA256 = "abc" },
		"empty input":            func(c *FnCall) { c.Input = nil },
		"success without output": func(c *FnCall) { c.Output = nil },
		"cache hit without source": func(c *FnCall) {
			c.CacheHit = true
		},
		"source without cache hit": func(c *FnCall) { c.CachedFrom = ptr(int64(1)) },
	}
	for name, mutate := range invalid {
		t.Run("rejects "+name, func(t *testing.T) {
			mock := newMockPool(t)
			c := validFnCall()
			mutate(&c)
			if _, _, err := insertFnCall(context.Background(), mock, c); err == nil {
				t.Fatal("accepted")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}
