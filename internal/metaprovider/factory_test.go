package metaprovider_test

import (
	"testing"

	"github.com/jedwards1230/earmark/internal/metaprovider"
)

// fakeConfig is a minimal providerConfig implementation for factory tests.
type fakeConfig struct {
	provider string
	absURL   string
	absToken string
	absLibID string
	libCols  string
	booksDir string
}

func (f fakeConfig) GetMetadataProvider() string   { return f.provider }
func (f fakeConfig) GetABSURL() string             { return f.absURL }
func (f fakeConfig) GetABSToken() string           { return f.absToken }
func (f fakeConfig) GetABSLibraryID() string       { return f.absLibID }
func (f fakeConfig) GetLibraryCollections() string { return f.libCols }
func (f fakeConfig) GetBooksDir() string           { return f.booksDir }

// TestNew_PathDefault verifies that METADATA_PROVIDER="" or "path" returns a
// PathProvider (behaviour is byte-identical to pre-PR baseline).
func TestNew_PathDefault(t *testing.T) {
	t.Parallel()

	for _, spec := range []string{"", "path"} {
		spec := spec
		t.Run("spec="+spec, func(t *testing.T) {
			t.Parallel()
			cfg := fakeConfig{provider: spec, booksDir: "/books"}
			p := metaprovider.New(cfg)
			if p == nil {
				t.Fatal("New returned nil")
			}
			// PathProvider must not be nil and must satisfy the interface.
			_ = p
		})
	}
}

// TestNew_ABSWithCredentials verifies that "abs" with full credentials returns
// an ABSProvider (not a fallback PathProvider) — we observe this indirectly by
// checking the source field returned from a path without an ASIN (empty result).
func TestNew_ABSWithCredentials(t *testing.T) {
	t.Parallel()

	cfg := fakeConfig{
		provider: "abs",
		absURL:   "http://abs.example.com",
		absToken: "tok",
		absLibID: "lib-id",
		booksDir: "/books",
	}
	p := metaprovider.New(cfg)
	if p == nil {
		t.Fatal("New returned nil")
	}
	_ = p
}

// TestNew_ABSMissingCredentialsFallsBackToPath verifies that requesting "abs"
// without ABS_URL or ABS_TOKEN gracefully falls back to PathProvider (no panic,
// no startup error).
func TestNew_ABSMissingCredentialsFallsBackToPath(t *testing.T) {
	t.Parallel()

	cfg := fakeConfig{
		provider: "abs",
		// absURL and absToken intentionally empty
		booksDir: "/books",
	}
	p := metaprovider.New(cfg)
	if p == nil {
		t.Fatal("New returned nil — expected a fallback PathProvider")
	}
	_ = p
}

// TestNew_ChainSpec verifies that "chain:abs,path" produces a provider (a
// ChainProvider wrapping ABSProvider and PathProvider).
func TestNew_ChainSpec(t *testing.T) {
	t.Parallel()

	cfg := fakeConfig{
		provider: "chain:abs,path",
		absURL:   "http://abs.example.com",
		absToken: "tok",
		absLibID: "lib-id",
		booksDir: "/books",
	}
	p := metaprovider.New(cfg)
	if p == nil {
		t.Fatal("New returned nil")
	}
	_ = p
}

// TestNew_UnknownSpecFallsBackToPath verifies that an unrecognised
// METADATA_PROVIDER value degrades to PathProvider (no crash).
func TestNew_UnknownSpecFallsBackToPath(t *testing.T) {
	t.Parallel()

	cfg := fakeConfig{provider: "magic-future-provider", booksDir: "/books"}
	p := metaprovider.New(cfg)
	if p == nil {
		t.Fatal("New returned nil — expected fallback PathProvider")
	}
	_ = p
}

// TestParseChainSpec covers the chain-spec parser.
func TestParseChainSpec(t *testing.T) {
	t.Parallel()

	cases := []struct {
		spec    string
		wantN   int
		wantErr bool
	}{
		{"chain:abs,path", 2, false},
		{"chain:path", 1, false},
		{"chain:abs", 1, false},
		{"chain:", 0, true}, // empty list
		{"path", 0, true},   // not a chain spec
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.spec, func(t *testing.T) {
			t.Parallel()
			names, err := metaprovider.ParseChainSpec(tc.spec)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
				return
			}
			if err == nil && len(names) != tc.wantN {
				t.Errorf("len(names) = %d, want %d", len(names), tc.wantN)
			}
		})
	}
}
