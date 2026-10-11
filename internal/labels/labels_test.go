package labels

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/db"
)

// The testdata files are small excerpts of the October 2026 bake-off label
// files (judge_items_key.json + jlabels.py, decide_labels.json and the
// verdict*.md appendices), so the adapters are pinned to the real shapes.

func TestNormalizeLabel(t *testing.T) {
	for in, want := range map[string]string{
		"good": db.LabelGood, "G": db.LabelGood, " g ": db.LabelGood,
		"junk": db.LabelJunk, "B": db.LabelJunk, "bad": db.LabelJunk,
		"partial": db.LabelPartial, "P": db.LabelPartial,
		"unsure": db.LabelUnsure, "U": db.LabelUnsure,
	} {
		got, err := NormalizeLabel(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := NormalizeLabel("maybe")
	assert.Error(t, err)
}

func TestDetectFormat(t *testing.T) {
	for _, c := range []struct{ path, labels, want string }{
		{"a.jsonl", "", FormatJSONL},
		{"verdict.md", "", FormatBakeoff},
		{"decide_labels.json", "", FormatBakeoff},
		{"judge_items_key.json", "jlabels.py", FormatBakeoffKey},
	} {
		got, err := DetectFormat(c.path, c.labels)
		require.NoError(t, err)
		assert.Equal(t, c.want, got, c.path)
	}
	_, err := DetectFormat("labels.txt", "")
	assert.Error(t, err)
}

func TestParseCanonicalJSONL(t *testing.T) {
	p, err := ParseFile("testdata/canonical.jsonl", Options{})
	require.NoError(t, err)
	require.Len(t, p.Items, 3)
	assert.Equal(t, db.LabelItem{SourceRef: "r1", FindingID: "22498907-495b-400f-98ef-2208bc832aa3",
		Label: db.LabelGood, Reason: "Eragon", LabelledAt: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)}, p.Items[0])
	assert.Equal(t, "cline", p.Items[1].Correction)
	assert.Equal(t, db.LabelGood, p.Items[1].Label)
	assert.Len(t, p.Items[1].ChunkTextSHA256, 64)
	assert.True(t, p.Items[1].LabelledAt.IsZero(), "no labelled_at and no default: import time")
	assert.Equal(t, "the pinnis drifted", p.Items[2].Context)
	assert.Equal(t, time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC), p.Items[2].LabelledAt)

	_, err = Parse(FormatJSONL, []byte(`{"label":"good","chunk":"x"}`), nil, time.Time{})
	assert.ErrorContains(t, err, "unknown field", "the canonical schema is strict")
	_, err = Parse(FormatJSONL, []byte("{\"label\":\"good\"}\n{\"label\":\"meh\"}"), nil, time.Time{})
	assert.ErrorContains(t, err, "line 2")
}

func TestParseBakeoffKey(t *testing.T) {
	p, err := ParseFile("testdata/judge_items_key.json", Options{LabelsFile: "testdata/jlabels.py",
		LabelledAt: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	assert.Equal(t, FormatBakeoffKey, p.Format)
	assert.Equal(t, 1, p.Unlabelled, "J008 has no label")
	require.Len(t, p.Items, 4)
	by := map[string]db.LabelItem{}
	for _, it := range p.Items {
		by[it.SourceRef] = it
	}
	j3 := by["J003"]
	assert.Equal(t, "2e7b59d3-d7bb-5c2d-a67d-14d51faf268b", j3.ChunkID)
	assert.Equal(t, "your road of the", j3.Original)
	assert.Equal(t, "yurodivy", j3.Correction)
	assert.Equal(t, db.LabelGood, j3.Label)
	assert.Equal(t, "", j3.IssueType, "arms disagree on the issue type")
	assert.Equal(t, "misheard_proper_noun", by["J007"].IssueType)
	assert.Equal(t, db.LabelPartial, by["J023"].Label)
	assert.Equal(t, "Theremon intended; 'theramon' still misspelt", by["J023"].Reason)
	assert.Equal(t, db.LabelUnsure, by["J004"].Label)
	assert.Equal(t, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), j3.LabelledAt)

	_, err = ParseFile("testdata/judge_items_key.json", Options{Format: FormatBakeoffKey})
	assert.ErrorContains(t, err, "--labels")
}

func TestParseLabelMap(t *testing.T) {
	m, err := parseLabelMap([]byte("LAB = {\n\"J1\":(\"G\",\"a \\\"quoted\\\" name\"),\n}\n"))
	require.NoError(t, err)
	assert.Equal(t, [2]string{"G", `a "quoted" name`}, m["J1"])
	m, err = parseLabelMap([]byte(`{"J1": ["B", "why"], "J2": ["U"]}`))
	require.NoError(t, err)
	assert.Equal(t, [2]string{"U", ""}, m["J2"])
	_, err = parseLabelMap([]byte(`{"J1": ["B", "why"],}`))
	assert.ErrorContains(t, err, "JSON object", "a file starting with { is JSON: its error is reported, not a .py parse error")
	_, err = parseLabelMap([]byte("LAB = {\nJ1: G\n}"))
	assert.ErrorContains(t, err, "line 2", "a line that is not an entry is an error, not skipped")
	_, err = parseLabelMap([]byte("\"J1\":(\"G\",\"a\"),\n\"J1\":(\"B\",\"b\"),"))
	assert.ErrorContains(t, err, "twice")
}

func TestParseBakeoffDecideJSON(t *testing.T) {
	p, err := ParseFile("testdata/decide_labels.json", Options{})
	require.NoError(t, err)
	assert.Equal(t, FormatBakeoff, p.Format)
	require.Len(t, p.Items, 4)
	for _, it := range p.Items {
		assert.NotEmpty(t, it.FindingID)
		assert.Equal(t, it.FindingID, it.SourceRef)
		assert.NotEmpty(t, it.Original)
		assert.NotEmpty(t, it.Correction, "replacement maps to correction")
		assert.True(t, it.LabelledAt.IsZero(), "a .json dump has no Date:")
	}
	// clef_applies sorts before decide_disagreements_top60.
	assert.Equal(t, "0508278b-732f-4f83-bce7-4f2e4ff5f9c4", p.Items[0].FindingID)
}

func TestParseVerdictMarkdown(t *testing.T) {
	tests := []struct {
		file  string
		n     int
		date  string
		check func(t *testing.T, items []db.LabelItem)
	}{
		{"testdata/verdict-excerpt.md", 5, "2026-10-10", func(t *testing.T, items []db.LabelItem) {
			// decide (2 finding labels) sorts before judge (3 chunk labels).
			assert.NotEmpty(t, items[0].FindingID)
			assert.Equal(t, "J000", items[2].SourceRef)
			assert.Equal(t, "castrop", items[2].Original)
			assert.Equal(t, "castro", items[2].Correction)
			assert.Equal(t, db.LabelJunk, items[2].Label)
			assert.Equal(t, "misheard_proper_noun", items[2].IssueType)
		}},
		{"testdata/verdict-qw2-excerpt.md", 3, "2026-10-11", func(t *testing.T, items []db.LabelItem) {
			assert.Equal(t, "Q000", items[0].SourceRef)
			assert.Equal(t, "do you know holden", items[0].Correction)
			assert.Equal(t, "dropped_word", items[0].IssueType)
			for _, it := range items {
				assert.NotEmpty(t, it.Context, it.SourceRef)
				assert.NotEmpty(t, it.ChunkID)
			}
		}},
		{"testdata/verdict-decide-excerpt.md", 2, "2026-10-10", func(t *testing.T, items []db.LabelItem) {
			assert.Equal(t, db.LabelUnsure, items[0].Label, "word labels (good/junk/unsure) map too")
			assert.NotEmpty(t, items[0].FindingID)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			p, err := ParseFile(tt.file, Options{})
			require.NoError(t, err)
			require.Len(t, p.Items, tt.n)
			assert.Equal(t, tt.date, p.FileDate.Format(time.DateOnly))
			for _, it := range p.Items {
				assert.Equal(t, tt.date, it.LabelledAt.Format(time.DateOnly), "the report's Date: is the default labelled_at")
			}
			tt.check(t, p.Items)
		})
	}
	// --labelled-at overrides the report's date.
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	p, err := ParseFile("testdata/verdict-excerpt.md", Options{LabelledAt: at})
	require.NoError(t, err)
	assert.Equal(t, at, p.Items[0].LabelledAt)
}

func TestParseBakeoffRejects(t *testing.T) {
	_, err := Parse(FormatBakeoff, []byte("# report\n\nno json here\n"), nil, time.Time{})
	assert.ErrorContains(t, err, "no ```json block")
	_, err = Parse(FormatBakeoff, []byte(`{"legend": {"label": "G good"}}`), nil, time.Time{})
	assert.ErrorContains(t, err, "no labelled items")
	_, err = Parse(FormatBakeoff, []byte(`[{"finding_id": "x", "label": "maybe"}]`), nil, time.Time{})
	assert.ErrorContains(t, err, "$[0]")
	_, err = Parse("csv", nil, nil, time.Time{})
	assert.True(t, err != nil && strings.Contains(err.Error(), "unknown format"))
}

func TestParseTime(t *testing.T) {
	got, err := ParseTime("2026-10-10")
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), got)
	_, err = ParseTime("10/10/2026")
	assert.Error(t, err)
}
