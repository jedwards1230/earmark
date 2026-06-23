package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
)

func TestEmbeddingDimensionConstant(t *testing.T) {
	// The CONTRACT mandates VECTOR(768) for nomic-embed-text.
	// This test guards against accidental changes to the constant.
	const expected = 768
	if EmbeddingDimension != expected {
		t.Errorf("EmbeddingDimension = %d, want %d — update CONTRACT.md before changing this", EmbeddingDimension, expected)
	}
}

func TestNewEmbeddings(t *testing.T) {
	cfg := &config.Config{
		EmbeddingsBaseURL: "http://ollama:11434/v1",
		EmbeddingsModel:   "nomic-embed-text",
	}
	e := NewEmbeddings(cfg)
	if e == nil {
		t.Fatal("NewEmbeddings returned nil")
	}
	if e.model != "nomic-embed-text" {
		t.Errorf("expected model nomic-embed-text, got %q", e.model)
	}
	if !e.prefixed {
		t.Error("expected nomic-embed-text to want task prefixes")
	}
}

func TestModelWantsTaskPrefixes(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"nomic-embed-text", true},
		{"nomic-embed-text:latest", true},
		{"NOMIC-EMBED-TEXT", true}, // case-insensitive
		{"bge-m3", false},
		{"text-embedding-3-small", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := modelWantsTaskPrefixes(tt.model); got != tt.want {
				t.Errorf("modelWantsTaskPrefixes(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestEmbedDocuments_EmptyInput(t *testing.T) {
	e := NewEmbeddings(&config.Config{
		EmbeddingsBaseURL: "http://ollama:11434/v1",
		EmbeddingsModel:   "nomic-embed-text",
	})
	result, err := e.EmbedDocuments(nil)
	if err != nil {
		t.Errorf("unexpected error for nil input: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result for nil input, got %v", result)
	}

	result2, err2 := e.EmbedDocuments([]string{})
	if err2 != nil {
		t.Errorf("unexpected error for empty slice: %v", err2)
	}
	if result2 != nil {
		t.Errorf("expected nil result for empty slice, got %v", result2)
	}
}

// embeddingRequest is the subset of the OpenAI embeddings request body the stub
// server inspects: Input is `[]string` on the wire for our calls.
type embeddingRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

// stubServer returns an httptest server that records the Input it received and
// answers with one zero 768-vector per input. captured is populated on each call.
func stubServer(t *testing.T, captured *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		var req embeddingRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decoding request body %q: %v", body, err)
		}
		*captured = req.Input

		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{
				"object":    "embedding",
				"index":     i,
				"embedding": make([]float32, EmbeddingDimension),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   data,
			"model":  req.Model,
			"usage":  map[string]int{"prompt_tokens": 0, "total_tokens": 0},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPrefixDivergence is the core guard: the document path MUST emit
// "search_document: <text>" and the query path MUST emit "search_query: <text>".
// They must NOT collapse to the same prefix.
func TestPrefixDivergence(t *testing.T) {
	var captured []string
	srv := stubServer(t, &captured)

	e := NewEmbeddings(&config.Config{
		EmbeddingsBaseURL: srv.URL + "/v1",
		EmbeddingsModel:   "nomic-embed-text",
	})

	t.Run("document path prefixes search_document", func(t *testing.T) {
		captured = nil
		if _, err := e.EmbedDocuments([]string{"the default mode network", "alpha waves"}); err != nil {
			t.Fatalf("EmbedDocuments: %v", err)
		}
		want := []string{
			"search_document: the default mode network",
			"search_document: alpha waves",
		}
		if len(captured) != len(want) {
			t.Fatalf("captured %d inputs, want %d", len(captured), len(want))
		}
		for i := range want {
			if captured[i] != want[i] {
				t.Errorf("document input[%d] = %q, want %q", i, captured[i], want[i])
			}
		}
	})

	t.Run("query path prefixes search_query", func(t *testing.T) {
		captured = nil
		if _, err := e.EmbedQuery("default mode network"); err != nil {
			t.Fatalf("EmbedQuery: %v", err)
		}
		want := []string{"search_query: default mode network"}
		if len(captured) != 1 {
			t.Fatalf("captured %d inputs, want 1", len(captured))
		}
		if captured[0] != want[0] {
			t.Errorf("query input = %q, want %q", captured[0], want[0])
		}
	})

	t.Run("same text diverges by role", func(t *testing.T) {
		const text = "the hippocampus consolidates memory"

		captured = nil
		if _, err := e.EmbedDocuments([]string{text}); err != nil {
			t.Fatalf("EmbedDocuments: %v", err)
		}
		doc := captured[0]

		captured = nil
		if _, err := e.EmbedQuery(text); err != nil {
			t.Fatalf("EmbedQuery: %v", err)
		}
		query := captured[0]

		if doc == query {
			t.Fatalf("document and query inputs must diverge, both = %q", doc)
		}
		if doc != documentPrefix+text {
			t.Errorf("document = %q, want %q", doc, documentPrefix+text)
		}
		if query != queryPrefix+text {
			t.Errorf("query = %q, want %q", query, queryPrefix+text)
		}
	})
}

// TestNoPrefixForNonNomicModel locks in the gating: a non-nomic model sends the
// text verbatim, with no task-instruction prefix on either side.
func TestNoPrefixForNonNomicModel(t *testing.T) {
	var captured []string
	srv := stubServer(t, &captured)

	e := NewEmbeddings(&config.Config{
		EmbeddingsBaseURL: srv.URL + "/v1",
		EmbeddingsModel:   "bge-m3",
	})
	if e.prefixed {
		t.Fatal("bge-m3 must not be prefixed")
	}

	captured = nil
	if _, err := e.EmbedDocuments([]string{"raw passage"}); err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	if captured[0] != "raw passage" {
		t.Errorf("document input = %q, want verbatim %q", captured[0], "raw passage")
	}

	captured = nil
	if _, err := e.EmbedQuery("raw query"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if captured[0] != "raw query" {
		t.Errorf("query input = %q, want verbatim %q", captured[0], "raw query")
	}
}
