package mcp

import (
	"encoding/json"
	"fmt"
	"strconv"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolArgs is the map-based argument accessor every tool handler uses in
// place of mcp-go's CallToolRequest.RequireString/GetString/GetInt/GetFloat/
// GetBool. The official SDK's low-level ToolHandler hands the raw JSON
// arguments as req.Params.Arguments (json.RawMessage); parseArgs unmarshals
// them into this map ONCE per call, and the accessor methods below reproduce
// the same default + coercion semantics the mcp-go helpers had (JSON numbers
// decode as float64).
type toolArgs map[string]any

// parseArgs unmarshals req's raw JSON arguments into a toolArgs map. A
// nil/empty payload decodes to a nil map — every lookup misses, matching
// mcp-go's CallToolRequest.GetArguments() returning nil for a request with no
// arguments.
func parseArgs(req *mcp.CallToolRequest) (toolArgs, error) {
	if len(req.Params.Arguments) == 0 {
		return nil, nil
	}
	var m toolArgs
	if err := json.Unmarshal(req.Params.Arguments, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// requireString returns a string argument by key, erroring only if the key is
// absent or not a string — mirroring mcp-go's CallToolRequest.RequireString.
// An empty string VALUE is not itself an error (mcp-go never checked for
// emptiness either).
func (a toolArgs) requireString(key string) (string, error) {
	v, ok := a[key]
	if !ok {
		return "", fmt.Errorf("required argument %q not found", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q is not a string", key)
	}
	return s, nil
}

// getString returns a string argument by key, or def if absent/wrong type.
func (a toolArgs) getString(key, def string) string {
	if v, ok := a[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// getInt returns an int argument by key, or def if absent/unparseable. JSON
// numbers decode as float64 (the common case); a numeric string is also
// accepted, mirroring mcp-go's GetInt.
func (a toolArgs) getInt(key string, def int) int {
	if v, ok := a[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			if i, err := strconv.Atoi(n); err == nil {
				return i
			}
		}
	}
	return def
}

// getFloat returns a float64 argument by key, or def if absent/unparseable.
func (a toolArgs) getFloat(key string, def float64) float64 {
	if v, ok := a[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case string:
			if f, err := strconv.ParseFloat(n, 64); err == nil {
				return f
			}
		}
	}
	return def
}

// getBool returns a bool argument by key, or def if absent/unparseable.
func (a toolArgs) getBool(key string, def bool) bool {
	if v, ok := a[key]; ok {
		switch b := v.(type) {
		case bool:
			return b
		case string:
			if parsed, err := strconv.ParseBool(b); err == nil {
				return parsed
			}
		case int:
			return b != 0
		case float64:
			return b != 0
		}
	}
	return def
}
