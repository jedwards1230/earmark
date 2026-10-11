package labels

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/db"
)

type fakeImporter struct {
	got db.LabelImport
	rep db.LabelImportReport
}

func (f *fakeImporter) ImportLabels(_ context.Context, imp db.LabelImport) (db.LabelImportReport, error) {
	f.got = imp
	return f.rep, nil
}

const keyFile = "../../internal/labels/testdata/judge_items_key.json"

func baseOpts() Options {
	return Options{Set: "bakeoff-2026-10-qw1", Labeler: "opus:bakeoff-2026-10", Format: "auto",
		LabelsFile: "../../internal/labels/testdata/jlabels.py", LabelledAt: "2026-10-10", ShowUnresolved: 1}
}

func TestOptionsValidate(t *testing.T) {
	assert.NoError(t, baseOpts().validate())
	o := baseOpts()
	o.Labeler = "justin"
	assert.Error(t, o.validate())
	o = baseOpts()
	o.DryRun, o.ParseOnly = true, true
	assert.Error(t, o.validate())
}

func TestImportParseOnly(t *testing.T) {
	o := baseOpts()
	o.ParseOnly = true
	p, err := Parse(keyFile, o)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, Import(context.Background(), &out, nil, keyFile, p, o))
	got := out.String()
	assert.Contains(t, got, "format bakeoff-key · 4 items · set bakeoff-2026-10-qw1 · labeler opus:bakeoff-2026-10")
	assert.Contains(t, got, "labels   good 2 · junk 0 · partial 1 · unsure 1")
	assert.Contains(t, got, "targets  finding 0 · chunk+hash 0 · chunk 4 · 0 with context")
	assert.Contains(t, got, "1 key items have no label (skipped)")
	assert.Contains(t, got, "labelled_at 2026-10-10T00:00:00Z")
	assert.Contains(t, got, "parse only: nothing resolved or written")
}

func TestImportDryRunReport(t *testing.T) {
	o := baseOpts()
	o.DryRun = true
	p, err := Parse(keyFile, o)
	require.NoError(t, err)
	store := &fakeImporter{rep: db.LabelImportReport{DryRun: true, Items: 4, Inserted: 1, Skipped: 1, Unresolved: 2,
		ByResolution: map[string]int{db.LabelResolvedChunk: 2}, ByLabel: map[string]int{db.LabelGood: 2},
		UnresolvedItems: []db.LabelUnresolved{
			{Index: 1, SourceRef: "J004", Reason: db.UnresolvedSpanNotInChunk, Detail: "x"},
			{Index: 2, SourceRef: "J007", Reason: db.UnresolvedChunkNotFound},
		}}}
	var out bytes.Buffer
	require.NoError(t, Import(context.Background(), &out, store, keyFile, p, o))
	assert.True(t, store.got.DryRun)
	assert.Equal(t, "bakeoff-2026-10-qw1", store.got.Set)
	assert.Len(t, store.got.Items, 4)
	got := out.String()
	assert.Contains(t, got, "would insert 1 · skipped 1 (already stored or repeated) · unresolved 2")
	assert.Contains(t, got, "resolved by finding 0 · hash 0 · chunk 2")
	assert.Contains(t, got, "unresolved: chunk_not_found 1 · span_not_in_chunk 1")
	assert.Contains(t, got, "J004  span_not_in_chunk  x")
	assert.Contains(t, got, "… 1 more")

	out.Reset()
	o.JSON = true
	require.NoError(t, Import(context.Background(), &out, store, keyFile, p, o))
	var js struct {
		Parsed ParseSummary         `json:"parsed"`
		Import db.LabelImportReport `json:"import"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &js))
	assert.Equal(t, 4, js.Parsed.Items)
	assert.Equal(t, 2, js.Import.Unresolved)
}

func TestParseBadLabelledAt(t *testing.T) {
	o := baseOpts()
	o.LabelledAt = "last tuesday"
	_, err := Parse(keyFile, o)
	assert.ErrorContains(t, err, "--labelled-at")
}
