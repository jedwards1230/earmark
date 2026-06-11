package metaprovider_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/metaprovider"
)

// stubProvider is a test double for MetadataProvider.
type stubProvider struct {
	result metaprovider.BookMeta
	err    error
	calls  int
}

func (s *stubProvider) Lookup(_ context.Context, _, _ string) (metaprovider.BookMeta, error) {
	s.calls++
	return s.result, s.err
}

// TestChainProvider_FirstHitWins verifies that the chain returns the first
// non-empty result and does not call subsequent providers.
func TestChainProvider_FirstHitWins(t *testing.T) {
	t.Parallel()

	abs := &stubProvider{result: metaprovider.BookMeta{Title: "From ABS", Author: "ABS Author", Source: "abs"}}
	path := &stubProvider{result: metaprovider.BookMeta{Title: "From Path", Author: "Path Author", Source: "path"}}

	chain := metaprovider.NewChainProvider(abs, path)
	got, err := chain.Lookup(context.Background(), "/books/A/B [B0XXXXXXXX]/01.mp3", "01.mp3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Source != "abs" {
		t.Errorf("Source = %q, want %q", got.Source, "abs")
	}
	if path.calls != 0 {
		t.Errorf("path provider called %d times, want 0 (ABS hit should short-circuit)", path.calls)
	}
}

// TestChainProvider_FallsBackOnEmptyResult verifies that when the first provider
// returns empty BookMeta (not-found), the chain tries the next.
func TestChainProvider_FallsBackOnEmptyResult(t *testing.T) {
	t.Parallel()

	abs := &stubProvider{result: metaprovider.BookMeta{}} // empty = not found
	path := &stubProvider{result: metaprovider.BookMeta{Title: "From Path", Author: "Path Author", Source: "path"}}

	chain := metaprovider.NewChainProvider(abs, path)
	got, err := chain.Lookup(context.Background(), "/books/A/B/01.mp3", "01.mp3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Source != "path" {
		t.Errorf("Source = %q, want %q (chain should fall back)", got.Source, "path")
	}
	if abs.calls != 1 {
		t.Errorf("abs provider calls = %d, want 1", abs.calls)
	}
	if path.calls != 1 {
		t.Errorf("path provider calls = %d, want 1", path.calls)
	}
}

// TestChainProvider_FallsBackOnError verifies that when the first provider
// returns an error, the chain logs and falls through to the next provider.
func TestChainProvider_FallsBackOnError(t *testing.T) {
	t.Parallel()

	abs := &stubProvider{err: errors.New("connection refused")}
	path := &stubProvider{result: metaprovider.BookMeta{Title: "From Path", Author: "Path Author", Source: "path"}}

	chain := metaprovider.NewChainProvider(abs, path)
	got, err := chain.Lookup(context.Background(), "/books/A/B [B0XXXXXXXX]/01.mp3", "01.mp3")
	if err != nil {
		t.Fatalf("chain returned error %v, want nil (errors are swallowed)", err)
	}
	if got.Source != "path" {
		t.Errorf("Source = %q, want %q (chain should fall back on error)", got.Source, "path")
	}
}

// TestChainProvider_AllEmpty verifies that when all providers return empty, the
// chain returns empty BookMeta with no error.
func TestChainProvider_AllEmpty(t *testing.T) {
	t.Parallel()

	abs := &stubProvider{result: metaprovider.BookMeta{}}
	path := &stubProvider{result: metaprovider.BookMeta{}}

	chain := metaprovider.NewChainProvider(abs, path)
	got, err := chain.Lookup(context.Background(), "/books/A/B/01.mp3", "01.mp3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Title != "" || got.Author != "" || got.ASIN != "" || len(got.Chapters) != 0 {
		t.Errorf("expected empty BookMeta, got %+v", got)
	}
}

// TestChainProvider_EmptyChain verifies that an empty chain returns empty
// BookMeta with no error (vacuous not-found).
func TestChainProvider_EmptyChain(t *testing.T) {
	t.Parallel()

	chain := metaprovider.NewChainProvider()
	got, err := chain.Lookup(context.Background(), "/books/A/B/01.mp3", "01.mp3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Title != "" || got.Author != "" || got.ASIN != "" || len(got.Chapters) != 0 {
		t.Errorf("expected empty BookMeta, got %+v", got)
	}
}

// TestChainProviderImplementsInterface is a compile-time assertion.
func TestChainProviderImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ metaprovider.MetadataProvider = metaprovider.NewChainProvider()
}
