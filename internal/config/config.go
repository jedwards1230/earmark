// Package config loads and validates service configuration from environment
// variables (or an optional .env file). All canonical env var names are defined
// in the CONTRACT.md document — do not rename them without updating that file.
package config

import (
	"encoding/json"
	"fmt"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/asr"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/joho/godotenv"
)

var logger = log.NewLogger("config")

// ASRServer is one entry in the static ASR_SERVERS registry — the set of
// transcription servers (Python ASR runners) the operator declares as part of
// the deployment. The Go service does NOT route work to these (the runner
// claims jobs itself, FOR UPDATE SKIP LOCKED); the list exists purely so the
// dashboard can show a configured-but-idle server (e.g. a declared fallback)
// rather than only servers it has happened to observe in run_metrics.
//
// Live status is matched against observed data by Match (a case-insensitive
// substring tested against both transcription_jobs.claimed_by and
// run_metrics.runner_host). Match defaults to Name when omitted — e.g. a server
// named "gpu-1" matches claimed_by "asr-runner-gpu-1" and runner_host
// "gpu-1".
type ASRServer struct {
	Name  string `json:"name"`            // display name, e.g. "gpu-1"
	Host  string `json:"host,omitempty"`  // host/address (informational), e.g. "gpu-1"
	Model string `json:"model,omitempty"` // expected ASR model, e.g. "nvidia/parakeet-tdt-0.6b-v3"
	Role  string `json:"role,omitempty"`  // "primary" | "fallback" (free-form; informational)
	Match string `json:"match,omitempty"` // token matched against claimed_by/runner_host; defaults to Name

	// GPUArbiterURL is an optional gpu-arbiter /status endpoint (e.g.
	// "http://gpu-1:48750/status") the dashboard polls for live
	// readiness: reachable + GPU available → ready; reachable + GPU in a game →
	// busy (the fallback signal); unreachable → offline. Empty → readiness is
	// inferred from job activity only (idle/not-seen). See internal/mcp/servers.go.
	GPUArbiterURL string `json:"gpuArbiterUrl,omitempty"`

	// Backend descriptor (CONTRACT §2.13) — all optional, all back-compat. These
	// declare the *expected* shape of the backend; the runner's run_metrics rows
	// are the ground truth the dashboard prefers when present (observed > config).
	//
	// Family / Runtime are FREE-FORM strings (earmark does not gatekeep which
	// families/runtimes exist); see asr.KnownFamily for the recommended canonical
	// ids used only for nicer labels.
	Family  string `json:"family,omitempty"`  // e.g. "nemo-parakeet", "whisper"
	Runtime string `json:"runtime,omitempty"` // e.g. "nemo-cuda", "whisper.cpp-sycl"

	// Capabilities is the declared/advertised capability map. Its keys are the
	// CLOSED asr.* enum (§2.13); unknown keys are dropped with a warning during
	// LoadConfig (forward-compat). Nil → "unknown" on the dashboard.
	Capabilities asr.Capabilities `json:"capabilities,omitempty"`

	// Languages is an optional ISO-639-1 code set the backend supports. Omitted →
	// unknown. Modeled as a string set rather than a boolean capability because
	// "which languages" is the useful fact (§2.13).
	Languages []string `json:"languages,omitempty"`
}

// ─── AI endpoint registry (CONTRACT §2.14) ─────────────────────────────────────
//
// The registry decouples "what AI endpoint exists" from "which function uses
// it". The operator can swap an embeddings backend (e.g. Ollama on one host →
// vLLM on another) by changing config, not code. AIRoles binds a function name
// (e.g. "embeddings") to an endpoint id.

// AIEndpointType distinguishes an embeddings endpoint from a chat/generation
// endpoint. Only these two values are valid; LoadConfig rejects others.
type AIEndpointType string

const (
	AIEndpointTypeEmbeddings AIEndpointType = "embeddings"
	AIEndpointTypeChat       AIEndpointType = "chat"
)

// AIBackend names the provider's wire protocol. All three speak the
// OpenAI-compatible REST API; the distinction is used only for the dashboard
// label and the health-probe path — there is no behavioral difference yet.
type AIBackend string

const (
	AIBackendOllama AIBackend = "ollama"
	AIBackendVLLM   AIBackend = "vllm"
	AIBackendOpenAI AIBackend = "openai-compat" // generic fallback
)

// AIEndpoint is one entry in the AI_ENDPOINTS registry.
type AIEndpoint struct {
	ID      string         `json:"id"`      // unique within this deployment
	Type    AIEndpointType `json:"type"`    // "embeddings" | "chat"
	Backend AIBackend      `json:"backend"` // "ollama" | "vllm" | "openai-compat"
	BaseURL string         `json:"baseURL"` // OpenAI-compatible base, no trailing slash
	Model   string         `json:"model"`   // model id passed to the API
	// Options are backend-specific key/value pairs (all strings on the wire).
	// Known keys: temperature, max_tokens, top_p. Unknown keys are forwarded
	// as-is so future backends don't require code changes.
	Options map[string]string `json:"options,omitempty"`
}

// AIRoles binds function names to endpoint IDs. The "embeddings" role is
// required when AI_ROLES is set; "eval" is optional (no eval layer when absent).
type AIRoles struct {
	Embeddings string `json:"embeddings"` // id of the embeddings endpoint (required)
	Eval       string `json:"eval"`       // id of a chat endpoint for the eval layer; "" = disabled
}

// validAIType reports whether t is a recognized endpoint type.
func validAIType(t AIEndpointType) bool {
	return t == AIEndpointTypeEmbeddings || t == AIEndpointTypeChat
}

// validAIBackend reports whether b is a recognized backend adapter.
func validAIBackend(b AIBackend) bool {
	return b == AIBackendOllama || b == AIBackendVLLM || b == AIBackendOpenAI
}

// MatchToken is the lower-cased substring used to attribute observed runner
// activity to this server. Falls back to the (lower-cased) name.
func (a ASRServer) MatchToken() string {
	t := a.Match
	if t == "" {
		t = a.Name
	}
	return strings.ToLower(strings.TrimSpace(t))
}

// asrServerJSON is the wire shape of one ASR_SERVERS entry. It mirrors ASRServer
// but takes capabilities as a raw {string: bool} map so unknown capability keys
// can be dropped via asr.ParseCapabilities (the closed-enum + forward-compat
// rule of CONTRACT §2.13) before they reach the typed asr.Capabilities field.
type asrServerJSON struct {
	Name          string          `json:"name"`
	Host          string          `json:"host"`
	Model         string          `json:"model"`
	Role          string          `json:"role"`
	Match         string          `json:"match"`
	GPUArbiterURL string          `json:"gpuArbiterUrl"`
	Family        string          `json:"family"`
	Runtime       string          `json:"runtime"`
	Capabilities  map[string]bool `json:"capabilities"`
	Languages     []string        `json:"languages"`
}

// parseASRServers decodes the ASR_SERVERS JSON array and validates each entry's
// capability map against the closed asr enum (unknown keys warn+drop). A JSON
// syntax error is returned to the caller so it can warn-and-degrade; a bad
// capability key never errors (it is dropped) — best-effort, never blocks startup.
func parseASRServers(raw string) ([]ASRServer, error) {
	var wire []asrServerJSON
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil, err
	}
	servers := make([]ASRServer, len(wire))
	for i, w := range wire {
		caps := asr.ParseCapabilities(w.Capabilities, "ASR_SERVERS["+w.Name+"].capabilities")
		servers[i] = ASRServer{
			Name:          w.Name,
			Host:          w.Host,
			Model:         w.Model,
			Role:          w.Role,
			Match:         w.Match,
			GPUArbiterURL: w.GPUArbiterURL,
			Family:        w.Family,
			Runtime:       w.Runtime,
			Capabilities:  caps,
			Languages:     w.Languages,
		}
	}
	return servers, nil
}

// parseAIEndpoints decodes the AI_ENDPOINTS JSON array and validates every
// entry. Unlike ASR_SERVERS (cosmetic, warn-and-degrade), the AI endpoint
// registry gates a critical path (embeddings), so any error here is fatal —
// the caller fails startup rather than silently embedding into the void.
func parseAIEndpoints(raw string) ([]AIEndpoint, error) {
	var eps []AIEndpoint
	if err := json.Unmarshal([]byte(raw), &eps); err != nil {
		return nil, fmt.Errorf("AI_ENDPOINTS is not valid JSON: %w", err)
	}
	seen := map[string]bool{}
	for i, ep := range eps {
		where := fmt.Sprintf("AI_ENDPOINTS[%d]", i)
		if ep.ID == "" {
			return nil, fmt.Errorf("%s: id is required", where)
		}
		if seen[ep.ID] {
			return nil, fmt.Errorf("%s: duplicate id %q", where, ep.ID)
		}
		seen[ep.ID] = true
		if !validAIType(ep.Type) {
			return nil, fmt.Errorf("%s (%q): invalid type %q (want %q or %q)",
				where, ep.ID, ep.Type, AIEndpointTypeEmbeddings, AIEndpointTypeChat)
		}
		if !validAIBackend(ep.Backend) {
			return nil, fmt.Errorf("%s (%q): invalid backend %q (want %q, %q, or %q)",
				where, ep.ID, ep.Backend, AIBackendOllama, AIBackendVLLM, AIBackendOpenAI)
		}
		if err := validateBaseURL(ep.BaseURL); err != nil {
			return nil, fmt.Errorf("%s (%q): %w", where, ep.ID, err)
		}
		if ep.Model == "" {
			return nil, fmt.Errorf("%s (%q): model is required", where, ep.ID)
		}
	}
	return eps, nil
}

// parseAIRoles decodes the AI_ROLES JSON object.
func parseAIRoles(raw string) (*AIRoles, error) {
	var roles AIRoles
	if err := json.Unmarshal([]byte(raw), &roles); err != nil {
		return nil, fmt.Errorf("AI_ROLES is not valid JSON: %w", err)
	}
	return &roles, nil
}

// validateBaseURL requires a parseable http/https URL with a host, so a
// mis-typed endpoint fails at startup rather than at first embed.
func validateBaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("baseURL is required")
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return fmt.Errorf("baseURL %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("baseURL %q must be http:// or https://", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("baseURL %q has no host", raw)
	}
	return nil
}

// validateAIRoles checks that the role bindings resolve to endpoints of the
// right type. Called after both AI_ENDPOINTS and AI_ROLES are parsed.
func validateAIRoles(roles *AIRoles, eps []AIEndpoint) error {
	byID := map[string]AIEndpoint{}
	for _, ep := range eps {
		byID[ep.ID] = ep
	}
	if roles.Embeddings == "" {
		return fmt.Errorf("AI_ROLES.embeddings is required when AI_ENDPOINTS is set")
	}
	emb, ok := byID[roles.Embeddings]
	if !ok {
		return fmt.Errorf("AI_ROLES.embeddings %q does not match any AI_ENDPOINTS id", roles.Embeddings)
	}
	if emb.Type != AIEndpointTypeEmbeddings {
		return fmt.Errorf("AI_ROLES.embeddings %q resolves to a %q endpoint, want %q",
			roles.Embeddings, emb.Type, AIEndpointTypeEmbeddings)
	}
	if roles.Eval != "" {
		ev, ok := byID[roles.Eval]
		if !ok {
			return fmt.Errorf("AI_ROLES.eval %q does not match any AI_ENDPOINTS id", roles.Eval)
		}
		if ev.Type != AIEndpointTypeChat {
			return fmt.Errorf("AI_ROLES.eval %q resolves to a %q endpoint, want %q",
				roles.Eval, ev.Type, AIEndpointTypeChat)
		}
	}
	return nil
}

// legacyEmbeddingsID is the synthetic endpoint id used when AI_ENDPOINTS is
// absent and the deployment relies on the deprecated EMBEDDINGS_* vars.
const legacyEmbeddingsID = "_legacy"

// Config holds all runtime configuration for the earmark Go service.
// Field names mirror the canonical env var names from CONTRACT.md §2.4.
type Config struct {
	// DATABASE_URL — PostgreSQL DSN (required).
	// Example: postgres://earmark:<pass>@earmark-pg-rw.earmark:5432/earmark
	DatabaseURL string

	// BOOKS_DIR — read-only NFS mount of the audiobook library.
	// Default: /books (matches the Kubernetes volumeMount path).
	BooksDir string

	// EMBEDDINGS_BASE_URL — OpenAI-compatible base URL for the embeddings
	// endpoint (Ollama). Default: http://ollama:11434/v1
	EmbeddingsBaseURL string

	// EMBEDDINGS_MODEL — embedding model to use. Must produce 768-dim vectors.
	// Default: nomic-embed-text (CONTRACT §2.3).
	EmbeddingsModel string

	// MCP_HTTP_ADDR — address for the streamable-HTTP MCP server.
	// Default: :8081 (CONTRACT §2.2).
	MCPHTTPAddr string

	// STALE_JOB_TIMEOUT — how long a claimed job may be silent before the Go
	// service resets it to pending. Default: 30m (CONTRACT §1.3).
	StaleJobTimeout time.Duration

	// ChunkSize — target token count per transcript chunk. Default: 512.
	// Overlap is 64 tokens (implementation constant in the chunker).
	ChunkSize int

	// LIBRARY_COLLECTIONS — optional JSON describing how each library root is
	// shaped, so author/title labels are derived from configuration rather than
	// hardcoded path assumptions. Empty → a generic fallback is used.
	// Example: [{"root":"audio-libation","layout":"author/title"},
	//           {"root":"audio-libro","layout":"author"}]
	// See internal/library for the layout grammar.
	LibraryCollections string

	// CONTROL_API_TOKEN — bearer token required on the mutating control-API
	// endpoints (PUT /api/v1/pipeline/pause, POST|DELETE /api/v1/pipeline/run).
	// Empty → those endpoints fail closed (503), so the pipeline can never be
	// paused or driven by an unauthenticated caller. Read endpoints need no token.
	ControlAPIToken string

	// METADATA_PROVIDER — which MetadataProvider to use.
	// "path" (default) — PathProvider: derives author/title from the directory
	//   layout defined by LIBRARY_COLLECTIONS. Byte-identical to pre-PR behaviour.
	// "abs"             — ABSProvider: looks up the book in Audiobookshelf by ASIN.
	//   Requires ABS_URL and ABS_TOKEN. Falls back to path if they are unset.
	// "chain:abs,path"  — ChainProvider: tries ABS first, then path. Recommended
	//   production value when ABS credentials are available.
	MetadataProvider string

	// ABS_URL — base URL of the Audiobookshelf instance reachable in-cluster,
	// e.g. "http://audiobookshelf.media:13378". Required when METADATA_PROVIDER
	// includes "abs". Empty → ABS provider degrades to path.
	ABSURL string

	// ABS_TOKEN — bearer token for the ABS REST API.
	// Populate from 1Password (Audiobookshelf item, api_token field).
	// Required when METADATA_PROVIDER includes "abs".
	ABSToken string

	// ABS_LIBRARY_ID — UUID of the ABS books library to search.
	// Default: "c749dc72-87a0-4889-8714-227eddb25900" (the homelab books library).
	ABSLibraryID string

	// ASRServers — optional JSON array describing the transcription servers
	// (ASR runners) declared for this deployment, so the dashboard can show a
	// configured-but-idle server (e.g. a fallback) instead of only servers it
	// has observed. Empty → the Servers page lists only observed runners.
	// Example: [{"name":"gpu-1","host":"gpu-1",
	//            "model":"nvidia/parakeet-tdt-0.6b-v3","role":"primary"}]
	// This does NOT influence job routing — the runner claims work itself.
	ASRServers []ASRServer

	// AIEndpoints — the AI endpoint registry (CONTRACT §2.14), parsed from
	// AI_ENDPOINTS. When AI_ENDPOINTS is absent, LoadConfig synthesizes a single
	// "_legacy" embeddings endpoint from EMBEDDINGS_BASE_URL / EMBEDDINGS_MODEL,
	// so existing deployments keep working with no config change. Always non-empty
	// after LoadConfig (it has at least the embeddings endpoint).
	AIEndpoints []AIEndpoint

	// AIRoles — role→endpoint-id bindings, parsed from AI_ROLES. When the legacy
	// path is used it is synthesized as {Embeddings:"_legacy"}. Always non-nil
	// after LoadConfig.
	AIRoles *AIRoles

	// Debug enables verbose structured logging.
	Debug bool

	// DebugDBReset drops and recreates all tables on startup (development only).
	DebugDBReset bool
}

// LoadConfig reads the environment (preceded by an optional .env file) and
// returns a validated Config. An error is returned for required vars that are
// absent or for values that fail to parse.
func LoadConfig() (*Config, error) {
	logger.Info("Loading configuration...")

	if err := godotenv.Load(); err != nil {
		logger.Debug("No .env file found", "error", err)
	} else {
		logger.Debug("Loaded .env file")
	}

	cfg := &Config{}

	// Required
	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	// Optional with defaults
	cfg.BooksDir = getEnvOrDefault("BOOKS_DIR", "/books")
	cfg.EmbeddingsBaseURL = getEnvOrDefault("EMBEDDINGS_BASE_URL", "http://ollama:11434/v1")
	cfg.EmbeddingsModel = getEnvOrDefault("EMBEDDINGS_MODEL", "nomic-embed-text")
	cfg.MCPHTTPAddr = getEnvOrDefault("MCP_HTTP_ADDR", ":8081")
	cfg.LibraryCollections = os.Getenv("LIBRARY_COLLECTIONS")
	cfg.ControlAPIToken = os.Getenv("CONTROL_API_TOKEN")
	cfg.MetadataProvider = getEnvOrDefault("METADATA_PROVIDER", "path")
	cfg.ABSURL = os.Getenv("ABS_URL")
	cfg.ABSToken = os.Getenv("ABS_TOKEN")
	cfg.ABSLibraryID = getEnvOrDefault("ABS_LIBRARY_ID", "c749dc72-87a0-4889-8714-227eddb25900")

	var err error

	staleStr := getEnvOrDefault("STALE_JOB_TIMEOUT", "30m")
	cfg.StaleJobTimeout, err = time.ParseDuration(staleStr)
	if err != nil {
		return nil, fmt.Errorf("STALE_JOB_TIMEOUT %q: %w", staleStr, err)
	}

	if cs := os.Getenv("CHUNK_SIZE"); cs != "" {
		cfg.ChunkSize, err = strconv.Atoi(cs)
		if err != nil {
			return nil, fmt.Errorf("CHUNK_SIZE %q: %w", cs, err)
		}
	} else {
		cfg.ChunkSize = 512
	}

	// ASR_SERVERS is best-effort, like LIBRARY_COLLECTIONS: a parse error logs a
	// warning and degrades to "no configured servers" (the page then lists only
	// observed runners) rather than blocking startup on a cosmetic list.
	if raw := strings.TrimSpace(os.Getenv("ASR_SERVERS")); raw != "" {
		servers, err := parseASRServers(raw)
		if err != nil {
			logger.Warn("ASR_SERVERS is not valid JSON; ignoring", "error", err)
		} else {
			cfg.ASRServers = servers
		}
	}

	// AI endpoint registry (CONTRACT §2.14). Priority:
	//   1. AI_ENDPOINTS + AI_ROLES — the structured registry (fail-closed on error).
	//   2. EMBEDDINGS_BASE_URL + EMBEDDINGS_MODEL — synthesized "_legacy" endpoint.
	// Unlike ASR_SERVERS, a malformed AI_ENDPOINTS is fatal: embeddings is the
	// critical path and a silent degrade would cause invisible embed failures.
	if err := cfg.loadAIRegistry(); err != nil {
		return nil, err
	}

	cfg.Debug = parseBoolEnv("DEBUG")
	cfg.DebugDBReset = parseBoolEnv("DEBUG_DB_RESET")

	return cfg, nil
}

// loadAIRegistry populates AIEndpoints + AIRoles from AI_ENDPOINTS / AI_ROLES,
// or synthesizes the legacy single-embeddings-endpoint registry from the
// deprecated EMBEDDINGS_* vars. After it returns nil, AIEndpoints is non-empty
// and AIRoles is non-nil with a resolvable Embeddings binding.
func (c *Config) loadAIRegistry() error {
	rawEndpoints := strings.TrimSpace(os.Getenv("AI_ENDPOINTS"))
	if rawEndpoints == "" {
		// Legacy path: synthesize one embeddings endpoint from EMBEDDINGS_*.
		c.AIEndpoints = []AIEndpoint{{
			ID:      legacyEmbeddingsID,
			Type:    AIEndpointTypeEmbeddings,
			Backend: AIBackendOllama,
			BaseURL: c.EmbeddingsBaseURL,
			Model:   c.EmbeddingsModel,
		}}
		c.AIRoles = &AIRoles{Embeddings: legacyEmbeddingsID}
		logger.Debug("AI_ENDPOINTS absent; using legacy EMBEDDINGS_* as the _legacy endpoint")
		return nil
	}

	eps, err := parseAIEndpoints(rawEndpoints)
	if err != nil {
		return err
	}
	if len(eps) == 0 {
		return fmt.Errorf("AI_ENDPOINTS is an empty array; set at least an embeddings endpoint or unset it to use EMBEDDINGS_*")
	}

	rawRoles := strings.TrimSpace(os.Getenv("AI_ROLES"))
	if rawRoles == "" {
		return fmt.Errorf("AI_ROLES is required when AI_ENDPOINTS is set")
	}
	roles, err := parseAIRoles(rawRoles)
	if err != nil {
		return err
	}
	if err := validateAIRoles(roles, eps); err != nil {
		return err
	}

	c.AIEndpoints = eps
	c.AIRoles = roles
	return nil
}

// EmbeddingsEndpoint returns the endpoint bound to the "embeddings" role.
// It is always present after LoadConfig (the legacy synth guarantees it), but
// returns ok=false defensively if the registry was constructed by hand without
// a valid binding.
func (c *Config) EmbeddingsEndpoint() (AIEndpoint, bool) {
	return c.endpointForRole(roleEmbeddings)
}

// RoleForEndpoint returns the AI_ROLES key bound to an endpoint id, or "" when
// the endpoint is registered but unbound. Used by the dashboard to label which
// role an endpoint serves.
func (c *Config) RoleForEndpoint(id string) string {
	if c.AIRoles == nil {
		return ""
	}
	switch id {
	case c.AIRoles.Embeddings:
		return roleEmbeddings
	case c.AIRoles.Eval:
		return roleEval
	}
	return ""
}

const (
	roleEmbeddings = "embeddings"
	roleEval       = "eval"
)

// endpointForRole resolves a role name to its bound endpoint.
func (c *Config) endpointForRole(role string) (AIEndpoint, bool) {
	if c.AIRoles == nil {
		return AIEndpoint{}, false
	}
	var id string
	switch role {
	case roleEmbeddings:
		id = c.AIRoles.Embeddings
	case roleEval:
		id = c.AIRoles.Eval
	}
	if id == "" {
		return AIEndpoint{}, false
	}
	for _, ep := range c.AIEndpoints {
		if ep.ID == id {
			return ep, true
		}
	}
	return AIEndpoint{}, false
}

// MaskSecret redacts all but the length of a secret string for logging.
func MaskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) > 8 {
		return "********"
	}
	n := len(secret)
	out := make([]byte, n)
	for i := range out {
		out[i] = '*'
	}
	return string(out)
}

// PrintEnvVars logs the current configuration at DEBUG level.
func (c *Config) PrintEnvVars() {
	logger.Debug("=== Current Configuration ===")
	logger.Debug("Debug", "value", c.Debug)
	logger.Debug("Debug DB Reset", "value", c.DebugDBReset)
	logger.Debug("Database URL", "value", MaskSecret(c.DatabaseURL))
	logger.Debug("Books Dir", "value", c.BooksDir)
	logger.Debug("Embeddings Base URL", "value", c.EmbeddingsBaseURL)
	logger.Debug("Embeddings Model", "value", c.EmbeddingsModel)
	logger.Debug("MCP HTTP Addr", "value", c.MCPHTTPAddr)
	logger.Debug("Library Collections", "value", c.LibraryCollections)
	logger.Debug("Control API Token", "value", MaskSecret(c.ControlAPIToken))
	logger.Debug("Stale Job Timeout", "value", c.StaleJobTimeout)
	logger.Debug("Chunk Size", "value", c.ChunkSize)
	logger.Debug("Metadata Provider", "value", c.MetadataProvider)
	logger.Debug("ABS URL", "value", c.ABSURL)
	logger.Debug("ABS Token", "value", MaskSecret(c.ABSToken))
	logger.Debug("ABS Library ID", "value", c.ABSLibraryID)
	logger.Debug("ASR Servers", "count", len(c.ASRServers))
	logger.Debug("AI Endpoints", "count", len(c.AIEndpoints))
	if emb, ok := c.EmbeddingsEndpoint(); ok {
		logger.Debug("Embeddings endpoint (resolved)", "id", emb.ID, "model", emb.Model, "baseURL", emb.BaseURL)
	}
}

// GetMetadataProvider satisfies metaprovider.providerConfig.
func (c *Config) GetMetadataProvider() string { return c.MetadataProvider }

// GetABSURL satisfies metaprovider.providerConfig.
func (c *Config) GetABSURL() string { return c.ABSURL }

// GetABSToken satisfies metaprovider.providerConfig.
func (c *Config) GetABSToken() string { return c.ABSToken }

// GetABSLibraryID satisfies metaprovider.providerConfig.
func (c *Config) GetABSLibraryID() string { return c.ABSLibraryID }

// GetLibraryCollections satisfies metaprovider.providerConfig.
func (c *Config) GetLibraryCollections() string { return c.LibraryCollections }

// GetBooksDir satisfies metaprovider.providerConfig.
func (c *Config) GetBooksDir() string { return c.BooksDir }

// getEnvOrDefault returns the env var value or defaultValue when unset/empty.
func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// parseBoolEnv returns true for "1" or "true", false for everything else.
func parseBoolEnv(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true"
}
