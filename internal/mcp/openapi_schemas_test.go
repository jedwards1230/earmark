package mcp

import (
	"bytes"
	"flag"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// updateSpec regenerates docs/openapi.yaml's components.schemas block instead of
// asserting against it:
//
//	go test ./internal/mcp -run TestOpenAPISchemasMatch -update
var updateSpec = flag.Bool("update", false, "rewrite docs/openapi.yaml's generated schemas")

// TestOpenAPISchemasMatch is the field-level drift guard.
//
// TestOpenAPISync (openapi_sync_test.go) proves the spec documents exactly the
// registered *routes*. That leaves the payloads unguarded: renaming a field on
// apiStatus, changing its type, or adding an omitempty would keep every route
// test green while the spec described a body the server no longer sends. This
// test closes that gap by deriving the schemas from the Go types and requiring
// the committed document to match.
//
// Only structure is compared. Annotations that Go types cannot express — prose
// descriptions, and enum constraints like state: ["ready"|"offline"|…] on a
// plain string field — stay hand-authored in the YAML and are carried across
// regeneration by field name (see annotationKeys), so running with -update never
// silently deletes documentation or a validation constraint.
func TestOpenAPISchemasMatch(t *testing.T) {
	generated, err := generateSchemas()
	if err != nil {
		t.Fatalf("generate schemas: %v", err)
	}

	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}

	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	committed := committedSchemas(t, spec)

	// Annotations live in the YAML; carry them onto the freshly-derived structure
	// so the comparison is about shape alone.
	merged := mergeAnnotations(generated, committed)

	// Compare like with like. The committed side has been through YAML, so its
	// lists are []any and its scalars are YAML's types; normalising the generated
	// side the same way keeps the diff about content rather than Go types.
	merged = normalizeYAML(t, merged)

	if *updateSpec {
		writeSchemas(t, specPath, raw, merged)
		t.Logf("rewrote %s (%d schemas)", specPath, len(merged))
		return
	}

	if reflect.DeepEqual(merged, committed) {
		return
	}

	// Report the smallest useful thing: which schemas differ, and how.
	for _, name := range schemaNames() {
		got, want := merged[name], committed[name]
		if reflect.DeepEqual(got, want) {
			continue
		}
		if want == nil {
			t.Errorf("schema %q is generated from Go but missing from %s", name, specPath)
			continue
		}
		t.Errorf("schema %q in %s does not match the Go type it documents\n  from Go:   %s\n  committed: %s",
			name, specPath, mustYAML(got), mustYAML(want))
	}
	for name := range committed {
		if _, ok := merged[name]; !ok {
			t.Errorf("schema %q is in %s but has no Go type in schemaTypes()", name, specPath)
		}
	}
	t.Logf("regenerate with: go test ./internal/mcp -run TestOpenAPISchemasMatch -update")
}

// normalizeYAML round-trips a value through YAML so both sides of the comparison
// have the types yaml.Unmarshal produces ([]any for sequences, map[string]any
// for mappings). Without this, a []string and a []any with identical contents
// compare unequal and every schema looks like it drifted.
func normalizeYAML(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("normalize marshal: %v", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(b, &out); err != nil {
		t.Fatalf("normalize unmarshal: %v", err)
	}
	return out
}

func committedSchemas(t *testing.T, spec map[string]any) map[string]any {
	t.Helper()
	comps, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no components block", specPath)
	}
	schemas, ok := comps["schemas"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no components.schemas block", specPath)
	}
	return schemas
}

// annotationKeys are the schema keywords authored by hand in docs/openapi.yaml
// rather than derived from Go. They describe or constrain a value beyond what
// its Go type says — a `state string` field is documented with the closed set of
// tokens it can hold, which reflection cannot recover — so regeneration carries
// them forward instead of overwriting them.
//
// Everything NOT on this list is structural and comes from Go: type, required,
// properties, items, additionalProperties, $ref, oneOf. Editing those by hand in
// the YAML is what this test exists to catch.
var annotationKeys = []string{
	"description", "title", "enum", "format", "default",
	"example", "examples", "pattern", "minimum", "maximum", "readOnly",
}

// requestOnlyAnnotationKeys are carried forward for REQUEST schemas only.
//
// `required` is derived for responses (no omitempty ⇒ always marshalled) but is
// handler policy for requests, so it is hand-authored there. Carrying it for
// both would let the committed file overwrite the derived value and silently
// mask real drift — adding omitempty to a response field would stop being
// caught, which is exactly what mutation-testing this guard turned up.
var requestOnlyAnnotationKeys = []string{"required"}

// requestSchemaNames is the set of components whose bodies the server accepts
// rather than sends.
func requestSchemaNames() map[string]bool {
	m := map[string]bool{}
	for _, st := range schemaTypes() {
		if st.Role == roleRequest {
			m[st.Name] = true
		}
	}
	return m
}

// mergeAnnotations copies the hand-authored keywords from the committed document
// onto the generated structure, matching on schema name and property name. A
// field that no longer exists in Go loses its annotations along with the field,
// which is the intended behaviour: they described something the server no longer
// sends.
func mergeAnnotations(generated, committed map[string]any) map[string]any {
	requests := requestSchemaNames()
	out := make(map[string]any, len(generated))
	for name, gen := range generated {
		g, _ := gen.(map[string]any)
		c, _ := committed[name].(map[string]any)
		keys := annotationKeys
		if requests[name] {
			keys = append(append([]string{}, annotationKeys...), requestOnlyAnnotationKeys...)
		}
		out[name] = mergeSchemaAnnotations(g, c, keys)
	}
	return out
}

func mergeSchemaAnnotations(gen, committed map[string]any, keys []string) map[string]any {
	if gen == nil {
		return nil
	}
	out := make(map[string]any, len(gen)+1)
	for k, v := range gen {
		out[k] = v
	}
	if committed == nil {
		return out
	}
	for _, k := range keys {
		if v, ok := committed[k]; ok {
			out[k] = v
		}
	}
	genProps, gok := out["properties"].(map[string]any)
	comProps, cok := committed["properties"].(map[string]any)
	if !gok || !cok {
		return out
	}
	merged := make(map[string]any, len(genProps))
	for field, gv := range genProps {
		gm, _ := gv.(map[string]any)
		cm, _ := comProps[field].(map[string]any)
		merged[field] = mergeSchemaAnnotations(gm, cm, annotationKeys)
	}
	out["properties"] = merged
	return out
}

// writeSchemas rewrites ONLY the `components.schemas` block, splicing the
// freshly-marshalled YAML into the file textually and leaving every other byte
// untouched.
//
// It deliberately does NOT round-trip the whole document through the YAML
// encoder. Doing that reflows the hand-authored parts — folded scalars (`>`)
// become literal blocks, key order is re-sorted, and a 600-line spec turns into
// a 600-line diff — which buries the actual schema change and destroys prose
// formatting the author chose. The generated block is machine-owned; the rest of
// the file is not.
func writeSchemas(t *testing.T, path string, raw []byte, schemas map[string]any) {
	t.Helper()

	var body bytes.Buffer
	enc := yaml.NewEncoder(&body)
	enc.SetIndent(2)
	if err := enc.Encode(schemas); err != nil {
		t.Fatalf("encode schemas: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("close encoder: %v", err)
	}

	lines := strings.Split(string(raw), "\n")
	start, end := schemasBlockRange(t, lines)

	// Indent the generated mapping to sit under "  schemas:".
	var indented []string
	for _, l := range strings.Split(strings.TrimRight(body.String(), "\n"), "\n") {
		if l == "" {
			indented = append(indented, l)
			continue
		}
		indented = append(indented, "    "+l)
	}

	out := append([]string{}, lines[:start+1]...) // through the "  schemas:" line
	out = append(out, indented...)
	out = append(out, lines[end:]...)

	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// schemasBlockRange locates the "  schemas:" key under "components:" and returns
// its line index plus the index of the first line after its block (the next
// sibling key, or EOF). Indentation is the delimiter, which is why the file is
// asserted to be 2-space indented YAML with no tabs.
func schemasBlockRange(t *testing.T, lines []string) (start, end int) {
	t.Helper()
	inComponents := false
	start = -1
	for i, l := range lines {
		if l == "components:" {
			inComponents = true
			continue
		}
		if inComponents && strings.HasPrefix(l, "  schemas:") {
			start = i
			break
		}
		// A new top-level key before we found schemas means components ended.
		if inComponents && l != "" && !strings.HasPrefix(l, " ") {
			inComponents = false
		}
	}
	if start < 0 {
		t.Fatalf("could not find the \"  schemas:\" block under \"components:\"")
	}
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "\t") {
			t.Fatalf("line %d is tab-indented; this splice assumes 2-space YAML", i+1)
		}
		// Any line indented 2 or fewer spaces ends the schemas block.
		if len(l)-len(strings.TrimLeft(l, " ")) <= 2 {
			return start, i
		}
	}
	return start, len(lines)
}

func mustYAML(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "<unmarshalable>"
	}
	return "\n" + string(b)
}

// TestSchemaTypesCoverSpec asserts the generated set and the documented set are
// the same set, so a schema cannot be added to one and forgotten in the other.
func TestSchemaTypesCoverSpec(t *testing.T) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	committed := committedSchemas(t, spec)

	var documented []string
	for name := range committed {
		documented = append(documented, name)
	}
	sort.Strings(documented)

	if got := schemaNames(); !reflect.DeepEqual(got, documented) {
		t.Errorf("generated schemas != documented schemas\n  schemaTypes(): %v\n  %s:  %v",
			got, specPath, documented)
	}
}
