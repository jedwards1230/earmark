// Package library derives human-friendly (author, title) labels from audiobook
// file paths. Real libraries are not uniformly shaped — one collection root may
// be Author/Title/tracks while another is Author/loose-track-files — so the
// shapes are declared in runtime configuration (LIBRARY_COLLECTIONS), never
// hardcoded. A sensible default handles any path that no collection matches.
package library

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Collection declares one library root and how the paths beneath it are shaped.
//
//	Root:   a path prefix. Absolute ("/books/audio-libation") or relative to
//	        BOOKS_DIR ("audio-libation").
//	Layout: slash-delimited roles for the directory segments BELOW Root, up to
//	        (but excluding) the audio filename. Recognized roles: "author",
//	        "title", "series", and "_" (ignore). If "title" is not one of the
//	        directory roles, the title is parsed from the filename instead — for
//	        layouts where an author directory holds loose track files.
//
// Examples:
//
//	{"root":"audio-libation","layout":"author/title"} → /…/W. Gibson/Neuromancer/01.mp3
//	{"root":"audio-libro","layout":"author"}          → /…/D. Kahneman/Title - Track 1.mp3
type Collection struct {
	Root   string `json:"root"`
	Layout string `json:"layout"`
}

// Resolver maps a book directory + a sample filename to (author, title) using
// the configured collections, with a generic fallback.
type Resolver struct {
	booksDir    string
	collections []compiled
}

type compiled struct {
	root  string // absolute, no trailing slash
	roles []string
}

// NewResolver compiles a set of collections. booksDir resolves relative roots to
// absolute prefixes. Roots are matched longest-first so a more specific root
// wins over a broader one.
func NewResolver(booksDir string, cols []Collection) *Resolver {
	r := &Resolver{booksDir: cleanTrim(booksDir)}
	for _, c := range cols {
		root := c.Root
		if !strings.HasPrefix(root, "/") {
			root = path.Join(r.booksDir, root)
		}
		root = cleanTrim(root)
		var roles []string
		for _, seg := range strings.Split(c.Layout, "/") {
			seg = strings.TrimSpace(seg)
			if seg != "" {
				roles = append(roles, seg)
			}
		}
		r.collections = append(r.collections, compiled{root: root, roles: roles})
	}
	// Longest root first so the most specific collection matches.
	for i := 1; i < len(r.collections); i++ {
		for j := i; j > 0 && len(r.collections[j].root) > len(r.collections[j-1].root); j-- {
			r.collections[j], r.collections[j-1] = r.collections[j-1], r.collections[j]
		}
	}
	return r
}

// ParseCollections builds a Resolver from the LIBRARY_COLLECTIONS JSON value.
// An empty value yields a resolver that always uses the generic fallback.
func ParseCollections(jsonValue, booksDir string) (*Resolver, error) {
	jsonValue = strings.TrimSpace(jsonValue)
	if jsonValue == "" {
		return NewResolver(booksDir, nil), nil
	}
	var cols []Collection
	if err := json.Unmarshal([]byte(jsonValue), &cols); err != nil {
		return nil, fmt.Errorf("parse LIBRARY_COLLECTIONS: %w", err)
	}
	return NewResolver(booksDir, cols), nil
}

// Resolve returns the (author, title) for a book directory. sampleFile is any
// one file path within that directory — used to parse the title from the
// filename for layouts whose directory roles don't include "title".
func (r *Resolver) Resolve(bookDir, sampleFile string) (author, title string) {
	bookDir = cleanTrim(bookDir)

	if c, ok := r.match(bookDir); ok {
		rel := strings.TrimPrefix(bookDir, c.root)
		segs := splitNonEmpty(rel)
		var hasTitle bool
		for i, role := range c.roles {
			if i >= len(segs) {
				break
			}
			switch role {
			case "author":
				author = segs[i]
			case "title":
				title = segs[i]
				hasTitle = true
			}
		}
		if !hasTitle {
			title = titleFromFilename(sampleFile)
		}
		return cleanup(author), cleanup(fallbackTitle(title, bookDir))
	}

	// Generic fallback: no configured collection matched.
	rel := strings.TrimPrefix(bookDir, r.booksDir)
	segs := splitNonEmpty(rel)
	switch {
	case len(segs) >= 2:
		author = segs[len(segs)-2]
		title = segs[len(segs)-1]
	case len(segs) == 1:
		author = segs[0]
		title = titleFromFilename(sampleFile)
	}
	return cleanup(author), cleanup(fallbackTitle(title, bookDir))
}

// match returns the most specific collection whose root is a path-prefix of dir.
func (r *Resolver) match(dir string) (compiled, bool) {
	for _, c := range r.collections {
		if dir == c.root || strings.HasPrefix(dir, c.root+"/") {
			return c, true
		}
	}
	return compiled{}, false
}

// trailingTrack strips a trailing track/part/chapter/disc marker and its number
// (e.g. " - Track 202", " Part 1", " - 01", " CD2") so a per-track filename
// collapses to the book title.
var trailingTrack = regexp.MustCompile(`(?i)[\s_]*[-–—]?[\s_]*(track|part|chapter|chp|pt|disc|cd|vol|volume)?[\s_]*\d+\s*$`)

// titleFromFilename derives a book title from a track filename: drop the
// extension, then strip a trailing track/part marker.
func titleFromFilename(file string) string {
	base := path.Base(file)
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	stripped := trailingTrack.ReplaceAllString(base, "")
	stripped = strings.TrimRight(strings.TrimSpace(stripped), " -–—_")
	if stripped == "" {
		return strings.TrimSpace(base)
	}
	return stripped
}

// fallbackTitle returns title if non-empty, else the book directory's basename.
func fallbackTitle(title, bookDir string) string {
	if strings.TrimSpace(title) != "" {
		return title
	}
	return path.Base(bookDir)
}

func cleanTrim(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	return strings.TrimRight(path.Clean(p), "/")
}

func splitNonEmpty(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cleanup tidies a derived label (collapses whitespace, trims separators).
func cleanup(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "/_-–—")
	return strings.TrimSpace(s)
}
