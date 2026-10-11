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
	tests := []struct {
		name        string
		it          LabelItem
		t           labelTarget
		known, req  bool
		wantBy      string
		wantReason  string
		wantSHA     string
		wantFinding bool
	}{
		{"by finding", LabelItem{FindingID: "f1"}, finding, false, false, LabelResolvedFinding, "", hash, true},
		{"by finding, texts agree modulo case/space", LabelItem{FindingID: "f1", Original: " GANEMA", Correction: "ghanima "}, finding, false, false, LabelResolvedFinding, "", hash, true},
		{"finding text mismatch", LabelItem{FindingID: "f1", Original: "gamma"}, finding, false, false, "", UnresolvedFindingTextMismatch, "", false},
		{"finding hash mismatch", LabelItem{FindingID: "f1", ChunkTextSHA256: other}, finding, false, false, "", UnresolvedHashMismatch, "", false},
		{"finding missing, no chunk", LabelItem{FindingID: "f9"}, labelTarget{}, false, false, "", UnresolvedFindingNotFound, "", false},
		{"finding missing, falls back to chunk", LabelItem{FindingID: "f9", ChunkID: "c1", Original: "ganema", Correction: "x"}, chunk, false, false, LabelResolvedChunk, "", hash, false},
		{"no target", LabelItem{Original: "x"}, labelTarget{}, false, false, "", UnresolvedNoTarget, "", false},
		{"chunk span", LabelItem{ChunkID: "c1", Original: "Ganema", Correction: "Ghanima"}, chunk, false, false, LabelResolvedChunk, "", hash, false},
		{"span only in the corrected text", LabelItem{ChunkID: "c1", Original: "Ghanima", Correction: "x"}, chunk, false, false, "", UnresolvedSpanNotInChunk, "", false},
		{"context in the corrected text", LabelItem{ChunkID: "c1", Original: "ganema", Correction: "x", Context: "then Ghanima"}, chunk, false, false, LabelResolvedChunk, "", hash, false},
		{"context missing", LabelItem{ChunkID: "c1", Original: "ganema", Correction: "x", Context: "now ganema"}, chunk, false, false, "", UnresolvedContextNotInChunk, "", false},
		{"require hash", LabelItem{ChunkID: "c1", Original: "ganema", Correction: "x"}, chunk, false, true, "", UnresolvedNoHash, "", false},
		{"current hash", LabelItem{ChunkID: "c1", ChunkTextSHA256: hash, Original: "nowhere", Correction: "x"}, chunk, false, true, LabelResolvedHash, "", hash, false},
		{"known older hash", LabelItem{ChunkID: "c1", ChunkTextSHA256: other, Original: "x", Correction: "y"}, chunk, true, false, LabelResolvedHash, "", other, false},
		{"known hash, chunk gone", LabelItem{ChunkID: "c1", ChunkTextSHA256: other, Original: "x", Correction: "y"}, labelTarget{}, true, false, LabelResolvedHash, "", other, false},
		{"unknown hash", LabelItem{ChunkID: "c1", ChunkTextSHA256: other, Original: "x", Correction: "y"}, chunk, false, false, "", UnresolvedUnknownRevision, "", false},
		{"chunk gone", LabelItem{ChunkID: "c1", Original: "x", Correction: "y"}, labelTarget{}, false, false, "", UnresolvedChunkNotFound, "", false},
		{"empty original", LabelItem{ChunkID: "c1", Original: " ", Correction: "y"}, chunk, false, false, "", UnresolvedInvalid, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, reason, _ := resolveLabel(tt.it, tt.t, tt.known, tt.req)
			if reason != tt.wantReason {
				t.Fatalf("reason %q, want %q", reason, tt.wantReason)
			}
			if reason != "" {
				return
			}
			if r.by != tt.wantBy || strOr(r.sha) != tt.wantSHA || (r.findingID != nil) != tt.wantFinding {
				t.Errorf("resolved by %q sha %q finding %v, want %q %q %v", r.by, strOr(r.sha), r.findingID != nil,
					tt.wantBy, tt.wantSHA, tt.wantFinding)
			}
			if tt.wantFinding && (r.original != "ganema" || r.correctn != "Ghanima" || r.issueType != "misheard_proper_noun") {
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
