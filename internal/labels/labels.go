// Package labels parses finding-label files for `earmark labels import`
// (CONTRACT §2.15 "Labels and scorecards") into db.LabelItem values. It reads
// files only; resolving items against the database and storing them is
// db.ImportLabels.
//
// Formats:
//
//   - jsonl — the canonical schema, one object per line (CanonicalItem).
//   - bakeoff — the October 2026 bake-off label dumps: a .json file or the
//     ```json appendix blocks of a verdict*.md report. Any array of objects
//     carrying a "label" is read, at any depth, except under "legend"; the
//     report's "Date: YYYY-MM-DD" is the default labelled_at.
//   - bakeoff-key — judge_items_key.json (blind items keyed by "bid") plus a
//     separate labels file (--labels): the jlabels.py dict
//     ("J000":("B","reason"),) or a JSON object {"J000": ["B", "reason"]}.
package labels

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// Formats.
const (
	FormatAuto       = "auto"
	FormatJSONL      = "jsonl"
	FormatBakeoff    = "bakeoff"
	FormatBakeoffKey = "bakeoff-key"
)

// Formats lists the accepted --format values.
var Formats = []string{FormatAuto, FormatJSONL, FormatBakeoff, FormatBakeoffKey}

// Options steer a parse.
type Options struct {
	Format string
	// LabelsFile is the bakeoff-key labels file.
	LabelsFile string
	// LabelledAt is the default labelled_at of items that carry none; it
	// overrides a date found in the file.
	LabelledAt time.Time
}

// Parsed is a parsed label file.
type Parsed struct {
	Format string
	Items  []db.LabelItem
	// FileDate is the "Date:" a bakeoff report carries (zero if none).
	FileDate time.Time
	// Unlabelled counts bakeoff-key items with no entry in the labels file.
	Unlabelled int
}

// CanonicalItem is one line of the canonical JSONL schema. A target is
// finding_id, or chunk_id + original + correction (+ chunk_text_sha256 to
// pin the revision; without it the import checks original — and context —
// against the current chunk text). Unknown fields are an error.
type CanonicalItem struct {
	ID              string `json:"id,omitempty"`
	FindingID       string `json:"finding_id,omitempty"`
	ChunkID         string `json:"chunk_id,omitempty"`
	ChunkTextSHA256 string `json:"chunk_text_sha256,omitempty"`
	Original        string `json:"original,omitempty"`
	Correction      string `json:"correction,omitempty"`
	IssueType       string `json:"issue_type,omitempty"`
	Context         string `json:"context,omitempty"`
	// Label is good|junk|partial|unsure (or G|B|P|U).
	Label  string `json:"label"`
	Reason string `json:"reason,omitempty"`
	// LabelledAt is RFC 3339 or YYYY-MM-DD.
	LabelledAt string `json:"labelled_at,omitempty"`
}

// NormalizeLabel maps a label spelling to its db value: good/G, junk/B/bad,
// partial/P, unsure/U (case-insensitive).
func NormalizeLabel(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "good", "g":
		return db.LabelGood, nil
	case "junk", "b", "bad":
		return db.LabelJunk, nil
	case "partial", "p":
		return db.LabelPartial, nil
	case "unsure", "u":
		return db.LabelUnsure, nil
	}
	return "", fmt.Errorf("unknown label %q (want good|junk|partial|unsure or G|B|P|U)", s)
}

// ParseTime reads RFC 3339 or a bare date (midnight UTC).
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("time %q: want RFC 3339 or YYYY-MM-DD", s)
	}
	return t, nil
}

// DetectFormat picks a format from the file name: .jsonl → jsonl, .md →
// bakeoff, .json → bakeoff-key with a labels file, else bakeoff.
func DetectFormat(path, labelsFile string) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonl", ".ndjson":
		return FormatJSONL, nil
	case ".md":
		return FormatBakeoff, nil
	case ".json":
		if labelsFile != "" {
			return FormatBakeoffKey, nil
		}
		return FormatBakeoff, nil
	}
	return "", fmt.Errorf("cannot tell the format of %s; pass --format (%s)", path, strings.Join(Formats[1:], ", "))
}

// ParseFile reads and parses path.
func ParseFile(path string, opts Options) (Parsed, error) {
	data, err := os.ReadFile(filepath.Clean(path)) // #nosec G304 -- operator-supplied import path
	if err != nil {
		return Parsed{}, err
	}
	format := opts.Format
	if format == "" || format == FormatAuto {
		if format, err = DetectFormat(path, opts.LabelsFile); err != nil {
			return Parsed{}, err
		}
	}
	var labelsData []byte
	if format == FormatBakeoffKey {
		if opts.LabelsFile == "" {
			return Parsed{}, errors.New("bakeoff-key needs --labels <file> (jlabels.py or a JSON object)")
		}
		if labelsData, err = os.ReadFile(filepath.Clean(opts.LabelsFile)); err != nil { // #nosec G304 -- operator-supplied --labels path
			return Parsed{}, err
		}
	}
	return Parse(format, data, labelsData, opts.LabelledAt)
}

// Parse parses data in format. labelsData is the bakeoff-key labels file;
// defaultAt (non-zero) is the labelled_at of items that carry none.
func Parse(format string, data, labelsData []byte, defaultAt time.Time) (Parsed, error) {
	var (
		p   Parsed
		err error
	)
	p.Format = format
	switch format {
	case FormatJSONL:
		p.Items, err = parseJSONL(data)
	case FormatBakeoff:
		p.Items, p.FileDate, err = parseBakeoff(data)
	case FormatBakeoffKey:
		p.Items, p.Unlabelled, err = parseBakeoffKey(data, labelsData)
	default:
		return p, fmt.Errorf("unknown format %q (%s)", format, strings.Join(Formats, ", "))
	}
	if err != nil {
		return p, err
	}
	at := defaultAt
	if at.IsZero() {
		at = p.FileDate
	}
	for i := range p.Items {
		if p.Items[i].LabelledAt.IsZero() {
			p.Items[i].LabelledAt = at
		}
	}
	return p, nil
}

func parseJSONL(data []byte) ([]db.LabelItem, error) {
	var out []db.LabelItem
	for n, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var c CanonicalItem
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		it, err := c.item()
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		out = append(out, it)
	}
	return out, nil
}

func (c CanonicalItem) item() (db.LabelItem, error) {
	label, err := NormalizeLabel(c.Label)
	if err != nil {
		return db.LabelItem{}, err
	}
	it := db.LabelItem{
		SourceRef: strings.TrimSpace(c.ID), FindingID: strings.TrimSpace(c.FindingID),
		ChunkID: strings.TrimSpace(c.ChunkID), ChunkTextSHA256: strings.TrimSpace(c.ChunkTextSHA256),
		Original: c.Original, Correction: c.Correction, IssueType: strings.TrimSpace(c.IssueType),
		Context: c.Context, Label: label, Reason: strings.TrimSpace(c.Reason),
	}
	if c.LabelledAt != "" {
		if it.LabelledAt, err = ParseTime(c.LabelledAt); err != nil {
			return db.LabelItem{}, err
		}
	}
	return it, nil
}

var (
	fenceRe    = regexp.MustCompile("(?s)```json[ \t]*\r?\n(.*?)\r?\n```")
	fileDateRe = regexp.MustCompile(`(?m)^Date:\s*(\d{4}-\d{2}-\d{2})`)
)

// parseBakeoff reads a bake-off .json dump or the ```json blocks of a
// verdict*.md report.
func parseBakeoff(data []byte) ([]db.LabelItem, time.Time, error) {
	var at time.Time
	var blocks [][]byte
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		blocks = [][]byte{trimmed}
	} else {
		if m := fileDateRe.FindSubmatch(data); m != nil {
			at, _ = time.Parse(time.DateOnly, string(m[1]))
		}
		for _, m := range fenceRe.FindAllSubmatch(data, -1) {
			blocks = append(blocks, m[1])
		}
		if len(blocks) == 0 {
			return nil, at, errors.New("no ```json block found")
		}
	}
	var out []db.LabelItem
	for i, b := range blocks {
		var v any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, at, fmt.Errorf("json block %d: %w", i+1, err)
		}
		items, err := walkBakeoff(v, "$")
		if err != nil {
			return nil, at, fmt.Errorf("json block %d: %w", i+1, err)
		}
		out = append(out, items...)
	}
	if len(out) == 0 {
		return nil, at, errors.New("no labelled items found (want arrays of objects carrying \"label\")")
	}
	return out, at, nil
}

// walkBakeoff collects the label objects under v: an array whose objects
// carry "label" is a label list; objects are searched key by key (sorted, so
// the order is stable), skipping "legend".
func walkBakeoff(v any, path string) ([]db.LabelItem, error) {
	switch x := v.(type) {
	case []any:
		var out []db.LabelItem
		for i, e := range x {
			obj, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if _, has := obj["label"]; !has {
				continue
			}
			it, err := bakeoffItem(obj)
			if err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", path, i, err)
			}
			out = append(out, it)
		}
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			if k != "legend" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var out []db.LabelItem
		for _, k := range keys {
			items, err := walkBakeoff(x[k], path+"."+k)
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
		}
		return out, nil
	}
	return nil, nil
}

// bakeoffItem maps one bake-off label object. Field spellings differ by
// report: id|bid, chunk_id|chunk, original|orig,
// correction|replacement|corr; issue_type is a string or a per-arm map.
func bakeoffItem(obj map[string]any) (db.LabelItem, error) {
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := obj[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	label, err := NormalizeLabel(str("label"))
	if err != nil {
		return db.LabelItem{}, err
	}
	return db.LabelItem{
		SourceRef:       str("id", "bid", "finding_id"),
		FindingID:       str("finding_id"),
		ChunkID:         str("chunk_id", "chunk"),
		ChunkTextSHA256: str("chunk_text_sha256"),
		Original:        str("original", "orig"),
		Correction:      str("correction", "replacement", "corr"),
		IssueType:       issueType(obj["issue_type"], obj["types"]),
		Context:         str("context"),
		Label:           label,
		Reason:          strings.TrimSpace(str("reason")),
	}, nil
}

// issueType reads a bake-off issue_type: a string, or a per-arm map of a
// string or a list of strings. Arms that disagree yield "".
func issueType(vs ...any) string {
	seen := map[string]bool{}
	var add func(v any)
	add = func(v any) {
		switch x := v.(type) {
		case string:
			if x != "" {
				seen[x] = true
			}
		case []any:
			for _, e := range x {
				add(e)
			}
		case map[string]any:
			for _, e := range x {
				add(e)
			}
		}
	}
	for _, v := range vs {
		add(v)
	}
	if len(seen) != 1 {
		return ""
	}
	for k := range seen {
		return k
	}
	return ""
}

// jlabelsLineRe matches one jlabels.py entry: "J000":("B","reason"),
var jlabelsLineRe = regexp.MustCompile(`^\s*"([^"]+)"\s*:\s*\(\s*"([A-Za-z]+)"\s*,\s*("(?:[^"\\]|\\.)*")\s*\)\s*,?\s*$`)

// parseLabelMap reads a bakeoff-key labels file: the jlabels.py dict, or a
// JSON object of id → [label, reason].
func parseLabelMap(data []byte) (map[string][2]string, error) {
	out := map[string][2]string{}
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '{' {
		var m map[string][]string
		if err := json.Unmarshal(t, &m); err != nil {
			return nil, fmt.Errorf("labels file: want a JSON object of id → [label, reason]: %w", err)
		}
		for id, v := range m {
			if len(v) == 0 || len(v) > 2 {
				return nil, fmt.Errorf("labels %s: want [label, reason]", id)
			}
			reason := ""
			if len(v) == 2 {
				reason = v[1]
			}
			out[id] = [2]string{v[0], reason}
		}
		return out, nil
	}
	for n, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		switch {
		case l == "", strings.HasPrefix(l, "#"), l == "}", strings.HasSuffix(l, "= {") || strings.HasSuffix(l, "={"):
			continue
		}
		m := jlabelsLineRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("labels line %d: not a \"ID\":(\"L\",\"reason\") entry: %q", n+1, l)
		}
		reason, err := strconv.Unquote(m[3])
		if err != nil {
			return nil, fmt.Errorf("labels line %d: reason: %w", n+1, err)
		}
		if _, dup := out[m[1]]; dup {
			return nil, fmt.Errorf("labels line %d: %s labelled twice", n+1, m[1])
		}
		out[m[1]] = [2]string{m[2], reason}
	}
	return out, nil
}

// parseBakeoffKey joins judge_items_key.json with its labels file by id.
func parseBakeoffKey(data, labelsData []byte) ([]db.LabelItem, int, error) {
	labelMap, err := parseLabelMap(labelsData)
	if err != nil {
		return nil, 0, err
	}
	var key []map[string]any
	if err := json.Unmarshal(data, &key); err != nil {
		return nil, 0, fmt.Errorf("key file: want a JSON array of items: %w", err)
	}
	var out []db.LabelItem
	unlabelled := 0
	for i, obj := range key {
		id, _ := obj["bid"].(string)
		if id == "" {
			id, _ = obj["id"].(string)
		}
		if id == "" {
			return nil, 0, fmt.Errorf("key item %d has no bid/id", i)
		}
		lr, ok := labelMap[id]
		if !ok {
			unlabelled++
			continue
		}
		obj["label"], obj["reason"] = lr[0], lr[1]
		obj["id"] = id
		it, err := bakeoffItem(obj)
		if err != nil {
			return nil, 0, fmt.Errorf("key item %s: %w", id, err)
		}
		out = append(out, it)
	}
	return out, unlabelled, nil
}
