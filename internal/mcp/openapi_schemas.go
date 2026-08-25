package mcp

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// This file derives the OpenAPI `components.schemas` block from the Go types
// that actually marshal into the JSON control API, so the documented shape of a
// payload cannot drift from the shape the server sends.
//
// Why this exists: docs/openapi.yaml used to hand-write every schema. The route
// table (routes.go) was drift-guarded by TestOpenAPISync, but that guard only
// compares *paths* — renaming a field, changing its type, or making it optional
// left the spec quietly describing a payload the server no longer produced.
// That is the failure mode the spec was introduced to remove, one level down.
//
// Division of responsibility, so a reader knows what is authored where:
//
//   - Structure (property names, JSON types, required-ness, nullability,
//     $refs, array/map element types) is DERIVED here from the Go structs and
//     is not editable by hand — TestOpenAPISchemasMatch fails if the committed
//     file disagrees.
//   - Prose and constraints Go cannot express — per-field `description`, `enum`
//     token sets on plain string fields, and the `required` list of REQUEST
//     bodies (which is handler policy, not struct shape) — stay hand-authored in
//     docs/openapi.yaml. Regeneration carries them forward by field name; an
//     annotation for a field that no longer exists is dropped with the field.
//
// Regenerate with:
//
//	go test ./internal/mcp -run TestOpenAPISchemasMatch -update

// payloadRole distinguishes a body the server *sends* from one it *accepts*,
// because a pointer field means something different in each.
//
// In a response, a nil pointer marshals to JSON null, so `*int` is a nullable
// integer. In a request, a pointer is a presence detector: the handlers use nil
// to tell "field omitted" from "field set to the zero value" and reject the
// omission (`field "paused" is required`). JSON null is therefore NOT a legal
// request value, and documenting the field as nullable would be a lie the
// decoder rejects at runtime. Reflection cannot recover that intent, so it is
// stated here.
type payloadRole int

const (
	roleResponse payloadRole = iota
	roleRequest
)

// schemaType binds one OpenAPI component name to the Go type defining its shape.
type schemaType struct {
	Name string
	Type reflect.Type
	Role payloadRole
}

// schemaTypes is the complete set of generated components. Adding a schema to
// docs/openapi.yaml without adding it here (or vice versa) fails
// TestOpenAPISchemasMatch, so the two cannot drift apart.
func schemaTypes() []schemaType {
	return []schemaType{
		{"Error", reflect.TypeOf(errorEnvelope{}), roleResponse},
		{"PauseState", reflect.TypeOf(pauseState{}), roleResponse},
		{"PauseRequest", reflect.TypeOf(pauseRequest{}), roleRequest},
		{"RunRequest", reflect.TypeOf(runRequest{}), roleRequest},
		{"RunnerUpdateRequest", reflect.TypeOf(runnerUpdateRequest{}), roleRequest},
		{"RunnerUpdate", reflect.TypeOf(apiRunnerUpdate{}), roleResponse},
		{"ETA", reflect.TypeOf(apiETA{}), roleResponse},
		{"Endpoint", reflect.TypeOf(apiEndpoint{}), roleResponse},
		{"Server", reflect.TypeOf(apiServer{}), roleResponse},
		{"PipelineLifecycle", reflect.TypeOf(pipelineLifecycle{}), roleResponse},
		{"Status", reflect.TypeOf(apiStatus{}), roleResponse},
	}
}

// refNames maps a Go struct type to the component name it is referenced by, so
// a nested struct becomes a $ref rather than an inlined copy.
func refNames() map[reflect.Type]string {
	m := make(map[reflect.Type]string, len(schemaTypes()))
	for _, st := range schemaTypes() {
		m[st.Type] = st.Name
	}
	return m
}

// generateSchemas builds the full components.schemas map from the Go types.
// Every value is a plain map so it round-trips through YAML identically to the
// committed document.
func generateSchemas() (map[string]any, error) {
	refs := refNames()
	out := make(map[string]any, len(schemaTypes()))
	for _, st := range schemaTypes() {
		s, err := structSchema(st.Type, refs, st.Role)
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", st.Name, err)
		}
		// decodeJSONBody sets DisallowUnknownFields, so an unrecognised key is a
		// 400 rather than being ignored. Documenting that keeps a client from
		// sending a hopeful extra field and getting a rejection the spec did not
		// predict. Responses stay open so adding a field is not a breaking change.
		if st.Role == roleRequest {
			s["additionalProperties"] = false
		}
		out[st.Name] = s
	}
	return out, nil
}

// structSchema renders one struct as an OpenAPI object schema. Field order
// follows declaration order (which is also the order the JSON encoder emits),
// and `required` lists every field without `omitempty` — matching encoding/json:
// a field that is always marshalled is always present in the response.
func structSchema(t reflect.Type, refs map[reflect.Type]string, role payloadRole) (map[string]any, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("want struct, got %s", t.Kind())
	}
	props := make(map[string]any, t.NumField())
	var required []string

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported: never marshalled
			continue
		}
		name, opts, ok := jsonFieldName(f)
		if !ok {
			continue
		}
		schema, err := typeSchema(f.Type, refs, role, opts.omitempty)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", f.Name, err)
		}
		props[name] = schema
		// Required-ness is derived for responses only. What the *server* always
		// sends is a property of the struct (no omitempty ⇒ always marshalled).
		// What a *client* must send is not: every request field is a pointer so
		// the handler can tell "absent" from "zero", and whether absence is an
		// error is handler policy (paused is rejected when nil; version is
		// accepted and clears the request). Reflection cannot see that, so
		// request `required` stays hand-authored — see annotationKeys.
		if role == roleResponse && !opts.omitempty {
			required = append(required, name)
		}
	}

	s := map[string]any{"type": "object"}
	if len(required) > 0 {
		s["required"] = required
	}
	s["properties"] = props
	return s, nil
}

type jsonOpts struct{ omitempty bool }

// jsonFieldName resolves a field's marshalled name and options. A `json:"-"`
// field is skipped; an absent tag falls back to the Go field name, matching
// encoding/json.
func jsonFieldName(f reflect.StructField) (string, jsonOpts, bool) {
	tag, has := f.Tag.Lookup("json")
	if !has {
		return f.Name, jsonOpts{}, true
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "-" && len(parts) == 1 {
		return "", jsonOpts{}, false
	}
	if name == "" {
		name = f.Name
	}
	var o jsonOpts
	for _, p := range parts[1:] {
		if p == "omitempty" {
			o.omitempty = true
		}
	}
	return name, o, true
}

// typeSchema renders a single Go type. Pointers become a nullable union
// (`type: [T, "null"]`) because a nil pointer marshals to JSON null — the
// distinction a consumer has to handle, and the one most likely to be got wrong
// by hand.
func typeSchema(t reflect.Type, refs map[reflect.Type]string, role payloadRole, omitempty bool) (map[string]any, error) {
	if t.Kind() == reflect.Pointer {
		inner, err := typeSchema(t.Elem(), refs, role, omitempty)
		if err != nil {
			return nil, err
		}
		if role == roleRequest {
			// Presence detector, not a nullable value — see payloadRole.
			return inner, nil
		}
		if omitempty {
			// encoding/json omits a nil pointer entirely when omitempty is set, so
			// the field is absent rather than null. Documenting it as nullable
			// would describe a value the server never emits.
			return inner, nil
		}
		return nullable(inner), nil
	}

	if name, ok := refs[t]; ok {
		return map[string]any{"$ref": "#/components/schemas/" + name}, nil
	}

	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Slice, reflect.Array:
		items, err := typeSchema(t.Elem(), refs, role, false)
		if err != nil {
			return nil, err
		}
		arr := map[string]any{"type": "array", "items": items}
		if omitempty {
			return arr, nil
		}
		// A nil slice marshals to null, not []. Without omitempty the field is
		// always present, so null is a value consumers must handle — this is the
		// case hand-written specs most often get wrong.
		return nullable(arr), nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map key must be string, got %s", t.Key().Kind())
		}
		vals, err := typeSchema(t.Elem(), refs, role, false)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": vals}, nil
	case reflect.Struct:
		// A struct with no component name is inlined rather than referenced.
		return structSchema(t, refs, role)
	default:
		return nil, fmt.Errorf("unsupported kind %s", t.Kind())
	}
}

// nullable turns a schema into its nullable form. A $ref cannot carry a sibling
// `type`, so it is wrapped in a oneOf alongside an explicit null.
func nullable(s map[string]any) map[string]any {
	if _, isRef := s["$ref"]; isRef {
		return map[string]any{"oneOf": []any{s, map[string]any{"type": "null"}}}
	}
	if t, ok := s["type"].(string); ok {
		s["type"] = []any{t, "null"}
	}
	return s
}

// schemaNames returns the generated component names in sorted order, for
// deterministic error messages.
func schemaNames() []string {
	names := make([]string, 0, len(schemaTypes()))
	for _, st := range schemaTypes() {
		names = append(names, st.Name)
	}
	sort.Strings(names)
	return names
}
