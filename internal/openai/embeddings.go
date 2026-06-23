package openai

import (
	"context"
	"fmt"
	"strings"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/sashabaranov/go-openai"
)

// EmbeddingDimension is the fixed vector dimension used for transcript_chunks.
// It must match the EMBEDDINGS_MODEL (nomic-embed-text produces 768-dim vectors).
// Any model change requires a column migration and a full re-embed of all chunks.
const EmbeddingDimension = 768

// Task-instruction prefixes required by nomic-embed-text. The model is trained
// with these prefixes and runs in an undefined regime without them: stored
// passages and search queries each need their own prefix so the document and
// query vectors land in the same learned space. These are nomic-specific — a
// different embeddings model (e.g. bge-m3) would be hurt by them, so prefixing
// is gated on the model name (see prefixForModel).
const (
	// documentPrefix is prepended to every passage stored/indexed in
	// transcript_chunks.embedding.
	documentPrefix = "search_document: "
	// queryPrefix is prepended to every search query before it is embedded.
	queryPrefix = "search_query: "
)

// Embeddings wraps the OpenAI-compatible embeddings client.
// The base URL is configurable so we can point at Ollama
// (http://ollama:11434/v1) instead of api.openai.com.
type Embeddings struct {
	c       *openai.Client
	model   string
	baseURL string
	// prefixed reports whether this model wants nomic-style task prefixes. It is
	// derived from the model name once at construction (see prefixForModel) so the
	// document/query divergence is decided in one place.
	prefixed bool
}

// NewEmbeddings creates an Embeddings client for the endpoint bound to the
// "embeddings" role in the AI endpoint registry (CONTRACT §2.14). After
// config.LoadConfig the registry always has this binding — either from
// AI_ENDPOINTS/AI_ROLES or synthesized from the legacy EMBEDDINGS_* vars — so a
// missing binding only happens when a Config is hand-built in a test; in that
// case we fall back to the flat EmbeddingsBaseURL/EmbeddingsModel fields to
// preserve the previous behavior.
func NewEmbeddings(cfg *config.Config) *Embeddings {
	baseURL, model := cfg.EmbeddingsBaseURL, cfg.EmbeddingsModel
	if emb, ok := cfg.EmbeddingsEndpoint(); ok {
		baseURL, model = emb.BaseURL, emb.Model
	}
	oaiCfg := openai.DefaultConfig("ollama") // key value unused by Ollama but required by the client
	oaiCfg.BaseURL = baseURL
	client := openai.NewClientWithConfig(oaiCfg)
	return &Embeddings{
		c:        client,
		model:    model,
		baseURL:  baseURL,
		prefixed: modelWantsTaskPrefixes(model),
	}
}

// modelWantsTaskPrefixes reports whether the embeddings model is one that
// requires nomic-style task-instruction prefixes. The prefixes are specific to
// the nomic-embed-text family; applying them to a model that was not trained
// with them (e.g. bge-m3) would degrade retrieval, so we gate on the model name.
func modelWantsTaskPrefixes(model string) bool {
	return strings.Contains(strings.ToLower(model), "nomic")
}

// BaseURL returns the configured embeddings endpoint, for diagnostics/logging.
func (e *Embeddings) BaseURL() string { return e.baseURL }

// EmbeddingUsage is the provider-reported token usage for an embeddings call.
// Ollama does not reliably populate these for embeddings (they are frequently
// zero), so callers that need an authoritative count should also tokenize the
// inputs locally; this carries the provider numbers when present.
type EmbeddingUsage struct {
	PromptTokens int
	TotalTokens  int
}

// EmbedDocuments embeds passages for STORAGE/INDEXING — the document side of the
// pipeline (transcript chunks written to transcript_chunks.embedding). For
// nomic-embed-text each input is prefixed with "search_document: ". Returns one
// 768-float32 vector per input; the result length equals len(docs).
//
// This is the document counterpart of EmbedQuery: the two MUST diverge so stored
// vectors and query vectors share nomic's learned space.
func (e *Embeddings) EmbedDocuments(docs []string) ([][]float32, error) {
	vecs, _, err := e.EmbedDocumentsWithUsage(docs)
	return vecs, err
}

// EmbedDocumentsWithUsage is EmbedDocuments plus the provider-reported token
// usage (which Ollama may leave zeroed — see EmbeddingUsage).
func (e *Embeddings) EmbedDocumentsWithUsage(docs []string) ([][]float32, EmbeddingUsage, error) {
	return e.embed(docs, documentPrefix)
}

// EmbedQuery embeds a single SEARCH QUERY — the query side of the pipeline. For
// nomic-embed-text the query is prefixed with "search_query: ". Returns the one
// 768-float32 vector for the query.
//
// This is the query counterpart of EmbedDocuments: see that method for why the
// two prefixes must diverge.
func (e *Embeddings) EmbedQuery(query string) ([]float32, error) {
	vecs, _, err := e.embed([]string{query}, queryPrefix)
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("no embedding returned for query")
	}
	return vecs[0], nil
}

// embed is the shared embedding core. prefix is prepended to each input only
// when this model wants task prefixes (modelWantsTaskPrefixes); otherwise inputs
// are sent verbatim. Callers MUST pass the role-appropriate prefix (document vs
// query) so the two sides of the pipeline never collapse to the same prefix.
func (e *Embeddings) embed(inputs []string, prefix string) ([][]float32, EmbeddingUsage, error) {
	if len(inputs) == 0 {
		return nil, EmbeddingUsage{}, nil
	}

	apiInputs := inputs
	if e.prefixed {
		apiInputs = make([]string, len(inputs))
		for i, in := range inputs {
			apiInputs[i] = prefix + in
		}
	}

	resp, err := e.c.CreateEmbeddings(
		context.Background(),
		openai.EmbeddingRequest{
			Input: apiInputs,
			Model: openai.EmbeddingModel(e.model),
		},
	)
	if err != nil {
		// Include the endpoint URL: an unreachable Ollama or an un-pulled model
		// both surface here, and the URL is what an operator needs to act on.
		return nil, EmbeddingUsage{}, fmt.Errorf("creating embeddings via model %q at %s: %w", e.model, e.baseURL, err)
	}

	if len(resp.Data) == 0 {
		return nil, EmbeddingUsage{}, fmt.Errorf("no embeddings returned for %d inputs", len(inputs))
	}
	if len(resp.Data) != len(inputs) {
		return nil, EmbeddingUsage{}, fmt.Errorf("embedding count mismatch: got %d for %d", len(resp.Data), len(inputs))
	}

	embeddings := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		embeddings[i] = d.Embedding
	}
	usage := EmbeddingUsage{
		PromptTokens: resp.Usage.PromptTokens,
		TotalTokens:  resp.Usage.TotalTokens,
	}
	return embeddings, usage, nil
}
