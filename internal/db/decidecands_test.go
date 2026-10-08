package db

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

// The decide dry run writes nothing but fn_calls and recipes (CONTRACT
// §2.19): every statement its selection runs must be a plain read — no DML,
// no row locks.
func TestDecideSelectionSQLIsReadOnly(t *testing.T) {
	write := regexp.MustCompile(`(?i)\b(insert|update|delete|merge|truncate|alter|create|drop|grant|for\s+(update|share|no\s+key|key))\b|set_config|nextval`)
	for name, sql := range map[string]string{
		"sample": decideSampleSQL, "backlog": decideBacklogSQL, "chunks": decideChunksSQL,
		"chunk findings": decideChunkFindingsSQL, "segments": transcriptSegmentsSQL, "book records": bookRecordsSQL,
	} {
		s := strings.TrimSpace(sql)
		if !strings.HasPrefix(strings.ToUpper(s), "SELECT") {
			t.Errorf("%s: does not start with SELECT", name)
		}
		if m := write.FindString(s); m != "" {
			t.Errorf("%s: contains %q", name, m)
		}
	}
}

// The sample is deterministic: ordered by md5(id || seed) with id as the
// tie-break, so the same seed over the same rows is the same sample.
func TestDecideSampleSQL(t *testing.T) {
	s := norm(decideSampleSQL)
	for _, want := range []string{
		"f.origin = 'judge'",
		"f.anchor_offset IS NOT NULL",
		"f.chunk_text_sha256 IS NOT NULL",
		"f.patch_state = ANY($1)",
		"(NOT $4 OR f.decided_by LIKE 'mcp:%')",
		"ORDER BY md5(f.id::text || $5), f.id",
		"LIMIT $6",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("sample SQL lacks %q", want)
		}
	}
	if !strings.Contains(norm(decideBacklogSQL), norm(decideScopeWhere)) {
		t.Error("backlog does not count the sample's scope")
	}
}

func TestDecideScope(t *testing.T) {
	ctx := context.Background()
	for _, s := range []DecideScope{{Sample: 0, Seed: "x"}, {Sample: MaxDecideSample + 1, Seed: "x"}, {Sample: 5, Seed: " "}} {
		if _, err := decideSample(ctx, newMockPool(t), s); err == nil {
			t.Errorf("%+v accepted", s)
		}
	}
	if got := (DecideScope{}).states(); len(got) != 1 || got[0] != patch.StateProposed {
		t.Errorf("default states %v", got)
	}
	if got := (DecideScope{Calibrate: true}).states(); len(got) != 3 {
		t.Errorf("calibrate states %v", got)
	}

	mock := newMockPool(t)
	cols := []string{"id", "transcript_id", "file_path", "issue_type", "original_text", "suggested_correction",
		"confidence", "chunk_index", "anchor_offset", "anchor_occurrence", "chunk_text_sha256", "patch_state", "decided_by"}
	mock.ExpectQuery(`ORDER BY md5`).
		WithArgs([]string{"proposed"}, "%Dune%", "homophone", false, "s1", 2).
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("f1", "t1", "/b/Dune/1.m4b", "homophone", "their", "there", 0.9, 3, 10, -1, "aa", "proposed", ""))
	got, err := decideSample(ctx, mock, DecideScope{Sample: 2, Seed: "s1", Book: "Dune", IssueType: "homophone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "f1" || got[0].AnchorOccurrence != -1 || got[0].ChunkIndex != 3 {
		t.Errorf("got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestDecideChunks(t *testing.T) {
	ctx := context.Background()
	if _, err := decideChunks(ctx, newMockPool(t), make([]ChunkKey, MaxDecideChunkBatch+1)); err == nil {
		t.Error("over-cap batch accepted")
	}
	mock := newMockPool(t)
	mock.ExpectQuery(`FROM transcript_chunks`).WithArgs([]string{"t1", "t1"}, []int{0, 1}).
		WillReturnRows(pgxmock.NewRows([]string{"transcript_id", "chunk_index", "text", "start_sec", "end_sec"}).
			AddRow("t1", 0, "the dish at auto sebo", 0.0, 4.0))
	cols := []string{"id", "transcript_id", "file_path", "issue_type", "original_text", "suggested_correction",
		"confidence", "chunk_index", "anchor_offset", "anchor_occurrence", "chunk_text_sha256", "patch_state", "decided_by"}
	mock.ExpectQuery(`FROM transcript_findings`).
		WithArgs([]string{"t1", "t1"}, []int{0, 1}, "proposed", []string{"accepted", "applied"}).
		WillReturnRows(pgxmock.NewRows(cols).
			AddRow("p1", "t1", "/b/x", "misheard_word", "dish", "fish", 0.5, 0, 4, 0, "aa", "proposed", "").
			AddRow("a1", "t1", "/b/x", "misheard_word", "the", "a", 0.5, 0, -1, -1, "", "accepted", "mcp:r").
			AddRow("z1", "t1", "/b/x", "misheard_word", "x", "y", 0.5, 1, 0, 0, "aa", "proposed", ""))
	got, err := decideChunks(ctx, mock, []ChunkKey{{"t1", 0}, {"t1", 1}})
	if err != nil {
		t.Fatal(err)
	}
	c := got[ChunkKey{"t1", 0}]
	if len(got) != 1 || c == nil || c.Text != "the dish at auto sebo" || len(c.Competitors) != 1 || len(c.Overlay) != 1 {
		t.Fatalf("got %+v", got)
	}
	if o := c.Overlay[0]; o.ID != "a1" || o.Anchor.Offset != -1 || o.Correction != "a" || o.ChunkHash != "" {
		t.Errorf("overlay %+v", o)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
