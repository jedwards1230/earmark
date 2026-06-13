// Package asr defines the ASR backend capability vocabulary shared by the
// earmark Go service and the (out-of-process, Python) ASR runners.
//
// It is a deliberately tiny **leaf** package: no DB, HTTP, or config imports, so
// it is trivially unit-testable and importable by both internal/config and
// internal/mcp without an import cycle. The vocabulary it defines is the
// authoritative mirror of docs/CONTRACT.md §2.13.
//
// Two design rules from the contract are encoded here:
//
//   - Capability KEYS are a CLOSED enum (the five Cap* constants). Parsing a
//     capability map drops unknown keys with a warning (forward-compat: an older
//     binary ignores keys a newer one introduces, rather than erroring).
//   - family / runtime ids are OPEN strings. earmark does not gatekeep which
//     families exist; KnownFamily / KnownRuntime exist purely so the dashboard
//     can render a nicer label for the recommended canonical ids, while unknown
//     values render verbatim.
package asr

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/jedwards1230/earmark/internal/log"
)

var logger = log.NewLogger("asr")

// Capability is one key in the closed capability enum (CONTRACT §2.13). It is a
// named string so call sites read as asr.CapWordTimestamps rather than a bare
// literal, but it serializes to/from JSON as a plain string.
type Capability string

// The closed capability enum. These are the ONLY valid capability keys; any
// other key is dropped by ParseCapabilities with a warning.
const (
	// CapWordTimestamps — per-word start/end timestamps in segments[].words.
	CapWordTimestamps Capability = "word_timestamps"
	// CapContextBiasing — word-boosting / context biasing from book_metadata.bias_terms.
	CapContextBiasing Capability = "context_biasing"
	// CapDiarization — speaker labels (segments[].speaker, words[].speaker).
	CapDiarization Capability = "diarization"
	// CapConfidenceScores — per-word confidence (words[].score).
	CapConfidenceScores Capability = "confidence_scores"
	// CapLanguageDetection — auto language id vs a fixed language.
	CapLanguageDetection Capability = "language_detection"
)

// allCapabilities is the closed set, in a stable order for AllCapabilities.
var allCapabilities = []Capability{
	CapWordTimestamps,
	CapContextBiasing,
	CapDiarization,
	CapConfidenceScores,
	CapLanguageDetection,
}

// capabilitySet is the membership lookup for IsKnownCapability / parsing.
var capabilitySet = func() map[Capability]struct{} {
	m := make(map[Capability]struct{}, len(allCapabilities))
	for _, c := range allCapabilities {
		m[c] = struct{}{}
	}
	return m
}()

// AllCapabilities returns the closed capability enum in a stable order. The
// returned slice is a fresh copy callers may mutate freely.
func AllCapabilities() []Capability {
	out := make([]Capability, len(allCapabilities))
	copy(out, allCapabilities)
	return out
}

// IsKnownCapability reports whether key is one of the closed enum capabilities.
func IsKnownCapability(key string) bool {
	_, ok := capabilitySet[Capability(key)]
	return ok
}

// Capabilities is a validated capability map: every key is a member of the
// closed enum. Construct it via ParseCapabilities (which drops unknown keys) so
// the invariant holds; it serializes to/from JSON as a plain {string: bool}
// object compatible with the run_metrics caps_* JSONB columns and the
// ASR_CAPABILITIES env value.
type Capabilities map[Capability]bool

// ParseCapabilities validates a raw {string: bool} capability map against the
// closed enum, returning a Capabilities containing only the recognized keys.
// Unrecognized keys are dropped and logged at WARN once (forward-compat per
// CONTRACT §2.13) — this is best-effort and never errors. A nil/empty input
// returns a nil Capabilities.
//
// context is a short label for the warning (e.g. "ASR_CAPABILITIES" or
// "caps_applied") so an operator can see where a bad key came from.
func ParseCapabilities(raw map[string]bool, context string) Capabilities {
	if len(raw) == 0 {
		return nil
	}
	out := make(Capabilities, len(raw))
	var unknown []string
	for k, v := range raw {
		if IsKnownCapability(k) {
			out[Capability(k)] = v
		} else {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		logger.Warn("dropping unknown ASR capability keys",
			"context", context, "keys", strings.Join(unknown, ","))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseCapabilitiesJSON parses a JSON object of {string: bool} into validated
// Capabilities, dropping unknown keys with a warning (see ParseCapabilities).
// Empty/whitespace input returns (nil, nil). Malformed JSON returns an error so
// the caller can decide whether to warn-and-degrade (config does) — it never
// panics.
func ParseCapabilitiesJSON(raw, context string) (Capabilities, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var m map[string]bool
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return ParseCapabilities(m, context), nil
}

// recommended canonical family ids (CONTRACT §2.13). These are RECOMMENDATIONS,
// not a closed set — a runner may report any family string.
const (
	FamilyNeMoParakeet  = "nemo-parakeet"
	FamilyNeMoCanary    = "nemo-canary"
	FamilyGraniteSpeech = "granite-speech"
	FamilyWhisper       = "whisper"
)

// recommended canonical runtime ids (CONTRACT §2.13). Recommendations, not closed.
const (
	RuntimeNeMoCUDA       = "nemo-cuda"
	RuntimeParakeetMLX    = "parakeet-mlx"
	RuntimeParakeetCPP    = "parakeet.cpp"
	RuntimeWhisperCPPSYCL = "whisper.cpp-sycl"
	RuntimeWhisperCPP     = "whisper.cpp"
	RuntimeOpenVINO       = "openvino"
)

// knownFamilies / knownRuntimes back the Known* label helpers. Membership is
// case-insensitive on the trimmed value.
var knownFamilies = map[string]struct{}{
	FamilyNeMoParakeet:  {},
	FamilyNeMoCanary:    {},
	FamilyGraniteSpeech: {},
	FamilyWhisper:       {},
}

var knownRuntimes = map[string]struct{}{
	RuntimeNeMoCUDA:       {},
	RuntimeParakeetMLX:    {},
	RuntimeParakeetCPP:    {},
	RuntimeWhisperCPPSYCL: {},
	RuntimeWhisperCPP:     {},
	RuntimeOpenVINO:       {},
}

// KnownFamily reports whether id is one of the recommended canonical family ids.
// It is purely cosmetic (the dashboard uses it to decide whether to show a
// curated label/icon); an unknown family is valid and rendered verbatim.
// Matching is case-insensitive on the trimmed value.
func KnownFamily(id string) bool {
	_, ok := knownFamilies[strings.ToLower(strings.TrimSpace(id))]
	return ok
}

// KnownRuntime reports whether id is one of the recommended canonical runtime
// ids. Cosmetic only; an unknown runtime is valid and rendered verbatim.
// Matching is case-insensitive on the trimmed value.
func KnownRuntime(id string) bool {
	_, ok := knownRuntimes[strings.ToLower(strings.TrimSpace(id))]
	return ok
}
