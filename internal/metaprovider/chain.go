package metaprovider

import (
	"context"

	"github.com/jedwards1230/lil-whisper/internal/log"
)

// ChainProvider implements MetadataProvider by trying a list of providers in
// order and returning the first non-empty result.
//
// A result is considered non-empty when at least one of Title, Author, or
// Chapters is non-zero. This lets a provider that returns partial metadata
// (e.g. PathProvider always returns Author+Title) still satisfy the chain,
// while an ABS miss (all-zero BookMeta) is transparent and the next provider
// is tried.
//
// When a provider returns an error, the error is logged and the chain falls
// through to the next provider — a transport hiccup on ABS must never block
// enqueue or search. Errors from all providers are not aggregated into a
// combined error; the last-resort fallback (typically PathProvider) is
// error-free by design.
//
// An empty chain returns empty BookMeta with no error (vacuous not-found).
type ChainProvider struct {
	providers []MetadataProvider
	log       log.Logger
}

// NewChainProvider constructs a ChainProvider from an ordered list of providers.
// Pass providers most-specific first (e.g. ABSProvider, then PathProvider).
func NewChainProvider(providers ...MetadataProvider) *ChainProvider {
	return &ChainProvider{
		providers: providers,
		log:       log.NewLogger("chain-provider"),
	}
}

// Lookup tries each provider in order and returns the first non-empty result.
// Provider errors are logged and swallowed so the chain always falls through.
func (c *ChainProvider) Lookup(ctx context.Context, filePath, sampleName string) (BookMeta, error) {
	for _, p := range c.providers {
		meta, err := p.Lookup(ctx, filePath, sampleName)
		if err != nil {
			c.log.Warn("provider error, trying next", "provider", providerName(p), "file", filePath, "error", err)
			continue
		}
		if isNonEmpty(meta) {
			return meta, nil
		}
	}
	return BookMeta{}, nil
}

// isNonEmpty returns true when a BookMeta carries at least some useful data.
// An all-zero BookMeta from a provider signals "not found in this catalog".
func isNonEmpty(m BookMeta) bool {
	return m.Title != "" || m.Author != "" || len(m.Chapters) > 0
}

// providerName returns a human-readable name for a MetadataProvider, used in
// log messages. Covers the known types; falls back to the Go type string.
func providerName(p MetadataProvider) string {
	switch p.(type) {
	case *ABSProvider:
		return "abs"
	case *PathProvider:
		return "path"
	case *ChainProvider:
		return "chain"
	default:
		return "unknown"
	}
}
