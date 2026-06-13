// Package config loads and validates service configuration from environment
// variables (or an optional .env file). All canonical env var names are defined
// in the CONTRACT.md document — do not rename them without updating that file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

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
// named "desktop-1" matches claimed_by "asr-runner-desktop-1" and runner_host
// "desktop-1".
type ASRServer struct {
	Name  string `json:"name"`            // display name, e.g. "desktop-1"
	Host  string `json:"host,omitempty"`  // host/address (informational), e.g. "192.168.8.10"
	Model string `json:"model,omitempty"` // expected ASR model, e.g. "nvidia/parakeet-tdt-0.6b-v3"
	Role  string `json:"role,omitempty"`  // "primary" | "fallback" (free-form; informational)
	Match string `json:"match,omitempty"` // token matched against claimed_by/runner_host; defaults to Name

	// GPUArbiterURL is an optional gpu-arbiter /status endpoint (e.g.
	// "http://192.168.8.10:48750/status") the dashboard polls for live
	// readiness: reachable + GPU available → ready; reachable + GPU in a game →
	// busy (the fallback signal); unreachable → offline. Empty → readiness is
	// inferred from job activity only (idle/not-seen). See internal/mcp/servers.go.
	GPUArbiterURL string `json:"gpuArbiterUrl,omitempty"`
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
	// Example: [{"name":"desktop-1","host":"192.168.8.10",
	//            "model":"nvidia/parakeet-tdt-0.6b-v3","role":"primary"}]
	// This does NOT influence job routing — the runner claims work itself.
	ASRServers []ASRServer

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
		var servers []ASRServer
		if err := json.Unmarshal([]byte(raw), &servers); err != nil {
			logger.Warn("ASR_SERVERS is not valid JSON; ignoring", "error", err)
		} else {
			cfg.ASRServers = servers
		}
	}

	cfg.Debug = parseBoolEnv("DEBUG")
	cfg.DebugDBReset = parseBoolEnv("DEBUG_DB_RESET")

	return cfg, nil
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
