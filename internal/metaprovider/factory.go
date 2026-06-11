package metaprovider

import (
	"fmt"
	"strings"

	"github.com/jedwards1230/lil-whisper/internal/log"
)

// providerConfig is the slice of the service config that the factory needs.
// Using an interface keeps the factory decoupled from the concrete config type
// and makes it straightforward to test with a small struct.
type providerConfig interface {
	GetMetadataProvider() string
	GetABSURL() string
	GetABSToken() string
	GetABSLibraryID() string
	GetLibraryCollections() string
	GetBooksDir() string
}

var factoryLog = log.NewLogger("metaprovider-factory")

// New builds the MetadataProvider requested by cfg.GetMetadataProvider().
//
// Accepted METADATA_PROVIDER values:
//   - "path"            — PathProvider only (default; byte-identical to pre-PR behaviour)
//   - "abs"             — ABSProvider only (warns + falls back to PathProvider if
//     ABS_URL or ABS_TOKEN are empty)
//   - "chain:abs,path"  — ChainProvider that tries ABSProvider first, then PathProvider
//     (the recommended production value)
//
// If ABS is requested but ABS_URL or ABS_TOKEN are unset, New logs a warning
// and substitutes PathProvider for the ABS slot — so the binary starts cleanly
// and degrades gracefully instead of hard-failing startup.
func New(cfg providerConfig) MetadataProvider {
	spec := strings.TrimSpace(cfg.GetMetadataProvider())
	if spec == "" {
		spec = "path"
	}

	pathP := NewPathProvider(cfg.GetLibraryCollections(), cfg.GetBooksDir())

	switch {
	case spec == "path":
		return pathP

	case spec == "abs":
		absP := buildABS(cfg, pathP)
		return absP

	case strings.HasPrefix(spec, "chain:"):
		// chain:<p1>,<p2>,... — parse the ordered list
		rest := strings.TrimPrefix(spec, "chain:")
		names := strings.Split(rest, ",")
		providers := make([]MetadataProvider, 0, len(names))
		for _, name := range names {
			name = strings.TrimSpace(name)
			switch name {
			case "abs":
				providers = append(providers, buildABS(cfg, pathP))
			case "path":
				providers = append(providers, pathP)
			default:
				factoryLog.Warn("unknown provider in chain spec, skipping", "name", name)
			}
		}
		if len(providers) == 0 {
			factoryLog.Warn("chain spec produced no providers, falling back to path", "spec", spec)
			return pathP
		}
		return NewChainProvider(providers...)

	default:
		factoryLog.Warn("unknown METADATA_PROVIDER value, falling back to path", "value", spec)
		return pathP
	}
}

// buildABS constructs an ABSProvider if credentials are present, or logs a
// warning and returns fallback (typically PathProvider) when they are not.
func buildABS(cfg providerConfig, fallback MetadataProvider) MetadataProvider {
	url := strings.TrimSpace(cfg.GetABSURL())
	token := strings.TrimSpace(cfg.GetABSToken())
	if url == "" || token == "" {
		factoryLog.Warn("ABS provider requested but ABS_URL/ABS_TOKEN not set; falling back to path provider")
		return fallback
	}
	libraryID := strings.TrimSpace(cfg.GetABSLibraryID())
	if libraryID == "" {
		factoryLog.Warn("ABS_LIBRARY_ID not set; falling back to path provider")
		return fallback
	}
	factoryLog.Info("ABS metadata provider configured",
		"url", url, "library_id", libraryID)
	return NewABSProvider(url, token, libraryID, nil)
}

// ParseChainSpec parses a "chain:p1,p2" spec into provider names, returning an
// error when the spec prefix is present but the list is empty. Used by config
// validation.
func ParseChainSpec(spec string) ([]string, error) {
	if !strings.HasPrefix(spec, "chain:") {
		return nil, fmt.Errorf("not a chain spec: %q", spec)
	}
	rest := strings.TrimPrefix(spec, "chain:")
	var names []string
	for _, n := range strings.Split(rest, ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("chain spec has no providers: %q", spec)
	}
	return names, nil
}
