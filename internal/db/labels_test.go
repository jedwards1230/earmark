package db

import (
	"strings"
	"testing"
)

func TestNormSpan(t *testing.T) {
	for in, want := range map[string]string{
		"  Ganema  ":           "ganema",
		"the\tcat\n sat":       "the cat sat",
		"the\u00a0cat\u3000x":  "the cat x",
		"ÉMILE":                "émile",
		"a\ufeffb":             "a b",
		"":                     "",
		"already normal words": "already normal words",
	} {
		if got := normSpan(in); got != want {
			t.Errorf("normSpan(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLabelImportValidate(t *testing.T) {
	ok := LabelImport{Set: "bakeoff-2026-10-qw2", Labeler: "opus:bakeoff-2026-10"}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid import refused: %v", err)
	}
	for _, bad := range []LabelImport{
		{Set: "", Labeler: "human:j"},
		{Set: "has space", Labeler: "human:j"},
		{Set: "-dash", Labeler: "human:j"},
		{Set: "s", Labeler: "justin"},
		{Set: "s", Labeler: "Human:j"},
		{Set: "s", Labeler: "human:"},
		{Set: "s", Labeler: "human:a\vb"},
		{Set: strings.Repeat("s", 129), Labeler: "human:j"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// TestResolveLabel: the pure resolution rules (the integration test covers
// the queries feeding them).
func TestResolveLabel(t *testing.T) {
	s := func(v string) *string { return &v }
	hash, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	finding := labelTarget{findingID: s("f1"), findingChunk: s("c1"), findingSHA: s(hash),
		findingOrig: s("ganema"), findingCorr: s("Ghanima"), findingIssue: s("misheard_proper_noun"),
		chunkID: s("c1"), chunkSHA: s(hash), chunkSource: s("then ganema said"), chunkText: s("then Ghanima said")}
	chunk := labelTarget{chunkID: s("c1"), chunkSHA: s(hash), chunkSource: s("then ganema said"), chunkText: s("then Ghanima said")}
	atCurrent := []chunkFinding{{id: "f1", sha: hash, original: "ganema", correction: "Ghanima"}}
	atOther := []chunkFinding{{id: "f2", sha: other, original: "ganema", correction: "Ghanima"}}
	twoAtCurrent := []chunkFinding{atCurrent[0], {id: "f3", sha: hash, original: "Ganema", correction: "ghanima"}}
	tests := []struct {
		name        string
		it          LabelItem
		t           labelTarget
		onChunk     []chunkFinding
		match       bool
		wantBy      string
		wantReason  string
		wantSHA     string
		wantFinding string
	}{
		{"by finding", LabelItem{FindingID: "f1"}, finding, nil, false, LabelResolvedFinding, "", hash, "f1"},
		{"by finding, texts agree modulo case/space", LabelItem{FindingID: "f1", Original: " GANEMA", Correction: "ghanima "}, finding, nil, false, LabelResolvedFinding, "", hash, "f1"},
		{"finding text mismatch", LabelItem{FindingID: "f1", Original: "gamma"}, finding, nil, false, "", UnresolvedFindingTextMismatch, "", ""},
		{"finding hash mismatch", LabelItem{FindingID: "f1", ChunkTextSHA256: other}, finding, nil, false, "", UnresolvedHashMismatch, "", ""},
		{"finding missing, no chunk", LabelItem{FindingID: "f9"}, labelTarget{}, nil, false, "", UnresolvedFindingNotFound, "", ""},
		{"finding missing, falls back to the chunk's findings", LabelItem{FindingID: "f9", ChunkID: "c1", Original: "ganema", Correction: "Ghanima"}, chunk, atCurrent, false, LabelResolvedFinding, "", hash, "f1"},
		{"no target", LabelItem{Original: "x"}, labelTarget{}, nil, false, "", UnresolvedNoTarget, "", ""},
		{"no hash: the findings' current revision", LabelItem{ChunkID: "c1", Original: "Ganema", Correction: "ghanima"}, chunk, atCurrent, false, LabelResolvedFinding, "", hash, "f1"},
		{"no hash: several findings, no single finding id", LabelItem{ChunkID: "c1", Original: "ganema", Correction: "Ghanima"}, chunk, twoAtCurrent, false, LabelResolvedFinding, "", hash, ""},
		{"no hash: the findings saw another revision", LabelItem{ChunkID: "c1", Original: "ganema", Correction: "Ghanima"}, chunk, atOther, true, "", UnresolvedChunkChanged, "", ""},
		{"no hash: findings, chunk gone", LabelItem{ChunkID: "c1", Original: "ganema", Correction: "Ghanima"}, labelTarget{}, atOther, false, "", UnresolvedChunkNotFound, "", ""},
		{"no hash: the findings' revision, context missing", LabelItem{ChunkID: "c1", Original: "ganema", Correction: "Ghanima", Context: "now ganema"}, chunk, atCurrent, false, "", UnresolvedContextNotInChunk, "", ""},
		{"no hash, no finding: off by default", LabelItem{ChunkID: "c1", Original: "ganema", Correction: "x"}, chunk, atCurrent, false, "", UnresolvedNoRecordedRevision, "", ""},
		{"no hash, no finding: current chunk opt-in", LabelItem{ChunkID: "c1", Original: "ganema", Correction: "x"}, chunk, nil, true, LabelResolvedChunk, "", hash, ""},
		{"opt-in: span only in the corrected text", LabelItem{ChunkID: "c1", Original: "Ghanima", Correction: "x"}, chunk, nil, true, "", UnresolvedSpanNotInChunk, "", ""},
		{"opt-in: context in the corrected text", LabelItem{ChunkID: "c1", Original: "ganema", Correction: "x", Context: "then Ghanima"}, chunk, nil, true, LabelResolvedChunk, "", hash, ""},
		{"current hash, span present", LabelItem{ChunkID: "c1", ChunkTextSHA256: hash, Original: "ganema", Correction: "x"}, chunk, nil, false, LabelResolvedHash, "", hash, ""},
		{"current hash, span absent", LabelItem{ChunkID: "c1", ChunkTextSHA256: hash, Original: "nowhere", Correction: "x"}, chunk, nil, false, "", UnresolvedSpanNotInChunk, "", ""},
		{"known older hash", LabelItem{ChunkID: "c1", ChunkTextSHA256: other, Original: "x", Correction: "y"}, chunk, atOther, false, LabelResolvedHash, "", other, ""},
		{"known hash, chunk gone", LabelItem{ChunkID: "c1", ChunkTextSHA256: other, Original: "x", Correction: "y"}, labelTarget{}, atOther, false, LabelResolvedHash, "", other, ""},
		{"unknown hash", LabelItem{ChunkID: "c1", ChunkTextSHA256: other, Original: "x", Correction: "y"}, chunk, nil, false, "", UnresolvedUnknownRevision, "", ""},
		{"chunk gone", LabelItem{ChunkID: "c1", Original: "x", Correction: "y"}, labelTarget{}, nil, true, "", UnresolvedChunkNotFound, "", ""},
		{"empty original", LabelItem{ChunkID: "c1", Original: " ", Correction: "y"}, chunk, nil, false, "", UnresolvedInvalid, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, reason, _ := resolveLabel(tt.it, tt.t, tt.onChunk, tt.match)
			if reason != tt.wantReason {
				t.Fatalf("reason %q, want %q", reason, tt.wantReason)
			}
			if reason != "" {
				return
			}
			if r.by != tt.wantBy || strOr(r.sha) != tt.wantSHA || strOr(r.findingID) != tt.wantFinding {
				t.Errorf("resolved by %q sha %q finding %q, want %q %q %q", r.by, strOr(r.sha), strOr(r.findingID),
					tt.wantBy, tt.wantSHA, tt.wantFinding)
			}
			if tt.it.FindingID == "f1" && (r.original != "ganema" || r.correctn != "Ghanima" || r.issueType != "misheard_proper_noun") {
				t.Errorf("a finding-resolved label must carry the finding's texts: %+v", r)
			}
		})
	}
}

func TestValidLabelItem(t *testing.T) {
	for _, bad := range []LabelItem{
		{Label: "maybe"},
		{Label: LabelGood, FindingID: "nope"},
		{Label: LabelGood, ChunkID: "nope"},
		{Label: LabelGood, ChunkTextSHA256: strings.Repeat("A", 64)},
		{Label: LabelGood, Reason: strings.Repeat("x", 2001)},
	} {
		if validLabelItem(bad) == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if err := validLabelItem(LabelItem{Label: LabelUnsure, FindingID: "22498907-495B-400F-98EF-2208BC832AA3"}); err != nil {
		t.Errorf("upper-case uuid refused: %v", err)
	}
}

func TestCalibrationBuckets(t *testing.T) {
	got := calibration([]bucketRow{{Bucket: 2, Good: 3, Junk: 1}, {Bucket: 5, Partial: 2}, {Bucket: 9, Good: 1}}, false)
	if len(got) != pBuckets || got[0].Lo != 0 || got[4].Hi != 1 {
		t.Fatalf("buckets %+v", got)
	}
	if got[1].Rate == nil || *got[1].Rate != 0.75 || got[4].Rate == nil || *got[4].Rate != 0 || got[0].Rate != nil {
		t.Errorf("rates %+v", got)
	}
	h := calibration([]bucketRow{{Bucket: 3, Accepted: 1, Rejected: 3}}, true)
	if h[2].Rate == nil || *h[2].Rate != 0.25 {
		t.Errorf("human rate %+v", h[2])
	}
}
