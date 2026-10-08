package scan

import (
	"sort"

	"github.com/jedwards1230/earmark/internal/db"
)

// BoilerplateCut is the p_boilerplate above which a chunk is left out of the
// quality index: credits, ads and chapter announcements say nothing about
// transcription quality.
const BoilerplateCut = 0.5

// Quality index scopes (earmark_quality_index{scope}). Low cardinality by
// design: never a book, ASIN or chunk id (CONTRACT §2.16).
const (
	ScopeLibrary     = "library"      // every scanned chunk
	ScopeASINMatched = "asin_matched" // chunks of books with a catalogue (ASIN) record
	ScopeUnmatched   = "unmatched"    // chunks of books without one
)

// QualityPoint is one earmark_quality_index series value.
type QualityPoint struct {
	Scope  string
	Recipe string
	Value  float64 // mean quality normalized to 0..1: (mean − 1) / 4
}

// QualityIndex turns the per-recipe, per-ASIN-side sums into index values:
// for each recipe, the mean quality (1..5) of its scanned chunks, normalized
// to 0..1, over the whole library and over each side of the ASIN split. A
// scope with no chunks has no series (not a 0). Sorted by recipe, then scope.
func QualityIndex(groups []db.QualityGroup) []QualityPoint {
	type acc struct {
		n   int64
		sum float64
	}
	byRecipe := map[string]map[string]*acc{}
	add := func(recipe, scope string, n int64, sum float64) {
		if byRecipe[recipe] == nil {
			byRecipe[recipe] = map[string]*acc{}
		}
		a := byRecipe[recipe][scope]
		if a == nil {
			a = &acc{}
			byRecipe[recipe][scope] = a
		}
		a.n += n
		a.sum += sum
	}
	for _, g := range groups {
		if g.Chunks <= 0 {
			continue
		}
		add(g.RecipeID, ScopeLibrary, g.Chunks, g.QualitySum)
		side := ScopeUnmatched
		if g.ASINMatched {
			side = ScopeASINMatched
		}
		add(g.RecipeID, side, g.Chunks, g.QualitySum)
	}
	var out []QualityPoint
	for recipe, scopes := range byRecipe {
		for scope, a := range scopes {
			out = append(out, QualityPoint{Scope: scope, Recipe: recipe, Value: normalize(a.sum / float64(a.n))})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Recipe != out[j].Recipe {
			return out[i].Recipe < out[j].Recipe
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

// normalize maps a 1..5 mean onto 0..1, clamped.
func normalize(mean float64) float64 {
	v := (mean - 1) / 4
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}
