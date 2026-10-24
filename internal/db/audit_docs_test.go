package db

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	auditContractPath = "../../docs/CONTRACT.md"
	auditSpecPath     = "../../docs/openapi.yaml"
	auditSpecRoute    = "/api/v1/audit/{source}"
)

var backticked = regexp.MustCompile("`([a-z_0-9]+)`")

// auditTokens returns the distinct backticked identifiers in s, sorted.
func auditTokens(s string) []string {
	var out []string
	for _, m := range backticked.FindAllStringSubmatch(s, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// auditSorted returns a sorted copy of in.
func auditSorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// TestAuditDocsMatchRegistry is the drift guard for the registry's two public
// descriptions: docs/openapi.yaml (the parameters of the audit route) and the
// CONTRACT §2.12.2 table (one row per source). Adding a column or a filter, or
// excluding one, without updating both fails here.
func TestAuditDocsMatchRegistry(t *testing.T) {
	sources := AuditSources()
	var names []string
	filterSources := map[string][]string{} // filter name -> sources that have it
	for _, s := range sources {
		names = append(names, s.Name)
		for _, f := range s.Filters {
			filterSources[f.Name] = append(filterSources[f.Name], s.Name)
		}
	}

	t.Run("openapi", func(t *testing.T) {
		raw, err := os.ReadFile(auditSpecPath)
		if err != nil {
			t.Fatal(err)
		}
		var spec struct {
			Paths map[string]map[string]struct {
				Parameters []struct {
					Name        string         `yaml:"name"`
					In          string         `yaml:"in"`
					Description string         `yaml:"description"`
					Schema      map[string]any `yaml:"schema"`
				} `yaml:"parameters"`
			} `yaml:"paths"`
		}
		if err := yaml.Unmarshal(raw, &spec); err != nil {
			t.Fatal(err)
		}
		op, ok := spec.Paths[auditSpecRoute]["get"]
		if !ok {
			t.Fatalf("%s documents no GET %s", auditSpecPath, auditSpecRoute)
		}

		query := map[string]string{} // documented query parameter -> description
		for _, p := range op.Parameters {
			switch p.In {
			case "query":
				query[p.Name] = p.Description
			case "path":
				if p.Name != "source" {
					t.Errorf("unexpected path parameter %q", p.Name)
					continue
				}
				var enum []string
				if raw, ok := p.Schema["enum"].([]any); ok {
					for _, e := range raw {
						enum = append(enum, fmt.Sprint(e))
					}
				}
				if !slices.Equal(auditSorted(enum), auditSorted(names)) {
					t.Errorf("source enum = %v, registry sources = %v", enum, names)
				}
			}
		}
		for _, paging := range []string{"after", "limit"} {
			if _, ok := query[paging]; !ok {
				t.Errorf("query parameter %q is not documented", paging)
			}
			delete(query, paging)
		}
		for name, have := range filterSources {
			desc, ok := query[name]
			if !ok {
				t.Errorf("filter %q (sources %v) is not a documented query parameter", name, have)
				continue
			}
			var mentioned []string
			for _, tok := range auditTokens(desc) {
				if slices.Contains(names, tok) {
					mentioned = append(mentioned, tok)
				}
			}
			if !slices.Equal(mentioned, auditSorted(have)) {
				t.Errorf("parameter %q describes sources %v, the registry gives it to %v", name, mentioned, auditSorted(have))
			}
			delete(query, name)
		}
		for name := range query {
			t.Errorf("query parameter %q is documented but is no source's filter", name)
		}
	})

	t.Run("contract", func(t *testing.T) {
		raw, err := os.ReadFile(auditContractPath)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(raw)
		start := strings.Index(doc, "#### 2.12.2")
		end := strings.Index(doc, "### 2.13")
		if start < 0 || end < start {
			t.Fatal("CONTRACT.md has no §2.12.2 section")
		}
		section := doc[start:end]

		if want := fmt.Sprintf("cut to its first %d characters", auditClip); !strings.Contains(strings.Join(strings.Fields(section), " "), want) {
			t.Errorf("§2.12.2 does not say text is cut to its first %d characters", auditClip)
		}

		rows := map[string][]string{} // source -> cells after the name
		for _, line := range strings.Split(section, "\n") {
			if !strings.HasPrefix(line, "| `") {
				continue
			}
			cells := strings.Split(strings.Trim(line, "| "), "|")
			if len(cells) == 4 {
				rows[auditTokens(cells[0])[0]] = cells[1:]
			}
		}
		clippedMark := regexp.MustCompile("`([a-z_0-9]+)`†")
		for _, s := range sources {
			cells, ok := rows[s.Name]
			if !ok {
				t.Errorf("§2.12.2 has no table row for %s", s.Name)
				continue
			}
			columns, excluded, filters := cells[0], cells[1], cells[2]

			var wantCols, wantFilters, wantClipped []string
			for _, c := range s.Columns {
				wantCols = append(wantCols, c.Name)
				if c.Clip > 0 {
					wantClipped = append(wantClipped, c.Name)
				}
			}
			for _, f := range s.Filters {
				wantFilters = append(wantFilters, f.Name)
			}
			if got := auditTokens(columns); !slices.Equal(got, auditSorted(wantCols)) {
				t.Errorf("%s: contract columns %v != registry columns %v", s.Name, got, auditSorted(wantCols))
			}
			var gotClipped []string
			for _, m := range clippedMark.FindAllStringSubmatch(columns, -1) {
				gotClipped = append(gotClipped, m[1])
			}
			if !slices.Equal(auditSorted(gotClipped), auditSorted(wantClipped)) {
				t.Errorf("%s: contract marks %v as cut (†), registry clips %v", s.Name, auditSorted(gotClipped), auditSorted(wantClipped))
			}
			if got := auditTokens(excluded); !slices.Equal(got, auditSorted(auditExcluded[s.Name])) {
				t.Errorf("%s: contract excludes %v, expected exclusions are %v", s.Name, got, auditSorted(auditExcluded[s.Name]))
			}
			for _, x := range auditExcluded[s.Name] {
				if slices.Contains(auditTokens(columns), x) {
					t.Errorf("%s: excluded column %q is listed as returned", s.Name, x)
				}
			}
			if got := auditTokens(filters); !slices.Equal(got, auditSorted(wantFilters)) {
				t.Errorf("%s: contract filters %v != registry filters %v", s.Name, got, auditSorted(wantFilters))
			}
		}
		if len(rows) != len(sources) {
			t.Errorf("§2.12.2 has %d source rows, the registry %d sources", len(rows), len(sources))
		}
	})
}
