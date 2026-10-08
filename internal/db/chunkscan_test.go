package db

import (
	"regexp"
	"strings"
	"testing"
)

// TestChunkScanWritesOnlyChunkScan: the scan's one write inserts into
// chunk_scan and nothing else — no UPDATE, DELETE or INSERT into another
// table — re-hashes the chunk's pristine text in the same statement, and is
// idempotent on the unique key. Its reads (candidates, quality groups) write
// nothing at all. The scan's other writes go through internal/fn (fn_calls)
// and RegisterRecipe.
func TestChunkScanWritesOnlyChunkScan(t *testing.T) {
	ins := strings.ToUpper(norm(insertChunkScanSQL))
	inserts := regexp.MustCompile(`INSERT INTO (\w+)`).FindAllStringSubmatch(ins, -1)
	if len(inserts) != 1 || inserts[0][1] != "CHUNK_SCAN" {
		t.Errorf("insertChunkScanSQL inserts into %v, want only chunk_scan", inserts)
	}
	for _, banned := range []string{"UPDATE ", "DELETE ", "DO UPDATE", "TRUNCATE", "ALTER "} {
		if strings.Contains(ins, banned) {
			t.Errorf("insertChunkScanSQL contains %q", banned)
		}
	}
	for _, want := range []string{
		"ON CONFLICT (TRANSCRIPT_ID, CHUNK_INDEX, CHUNK_TEXT_SHA256, RECIPE_ID) DO NOTHING",
		"ENCODE(SHA256(CONVERT_TO(COALESCE(C.SOURCE_TEXT, C.TEXT), 'UTF8')), 'HEX') = $3",
	} {
		if !strings.Contains(ins, want) {
			t.Errorf("insertChunkScanSQL missing %q", want)
		}
	}
	for name, q := range map[string]string{"candidates": scanCandidatesSQL, "quality": qualityGroupsSQL} {
		u := strings.ToUpper(q)
		for _, banned := range []string{"INSERT ", "UPDATE ", "DELETE ", "TRUNCATE", "ALTER "} {
			if strings.Contains(u, banned) {
				t.Errorf("%s query contains %q", name, banned)
			}
		}
	}
}

// TestQualityGroupsUseCurrentTextAndCut: the index counts only scans of a
// chunk's current text, cuts boilerplate by parameter, and splits on the
// book's catalogue record.
func TestQualityGroupsUseCurrentTextAndCut(t *testing.T) {
	q := norm(qualityGroupsSQL)
	for _, want := range []string{
		"encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex') = s.chunk_text_sha256",
		"s.p_boilerplate <= $1",
		"bm.book_dir = regexp_replace(c.file_path, '/[^/]+$', '')",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("qualityGroupsSQL missing %q", want)
		}
	}
}

func TestChunkScanValidate(t *testing.T) {
	good := ChunkScan{
		TranscriptID: "t", ChunkIndex: 0, ChunkTextSHA256: strings.Repeat("a", 64), RecipeID: "r",
		PNeedsFix: 0.1, Quality: 3, PBoilerplate: 0, PGarbled: 1, PDialogue: 0.5,
		IssueType: "none", IssueProbs: map[string]float64{"none": 1},
	}
	if err := good.validate(); err != nil {
		t.Fatalf("good row rejected: %v", err)
	}
	conf := 1.5
	for name, mutate := range map[string]func(*ChunkScan){
		"no recipe":        func(s *ChunkScan) { s.RecipeID = "" },
		"negative index":   func(s *ChunkScan) { s.ChunkIndex = -1 },
		"bad hash":         func(s *ChunkScan) { s.ChunkTextSHA256 = "ABC" },
		"quality 0":        func(s *ChunkScan) { s.Quality = 0 },
		"quality 6":        func(s *ChunkScan) { s.Quality = 6 },
		"probability > 1":  func(s *ChunkScan) { s.PGarbled = 1.01 },
		"confidence > 1":   func(s *ChunkScan) { s.QualityConfidence = &conf },
		"no issue":         func(s *ChunkScan) { s.IssueType = "" },
		"no issue probs":   func(s *ChunkScan) { s.IssueProbs = nil },
		"empty transcript": func(s *ChunkScan) { s.TranscriptID = "" },
	} {
		s := good
		mutate(&s)
		if err := s.validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// TestChunkScanMigration: 00009 creates chunk_scan (cascading with its
// transcript, keyed on chunk text + recipe), adds the scan arm to stale_work
// while restating the earlier arms (00005's superseded filter and 00008's
// decide arm included), and Down restores 00008's view before dropping the table.
func TestChunkScanMigration(t *testing.T) {
	b, err := migrationFiles.ReadFile("migrations/00009_chunk_scan.sql")
	if err != nil {
		t.Fatal(err)
	}
	rawUp, rawDown, ok := strings.Cut(string(b), "-- +goose Down")
	if !ok {
		t.Fatal("no Down section")
	}
	up, down := norm(stripSQLComments(rawUp)), norm(stripSQLComments(rawDown))
	for _, banned := range []string{"ALTER TABLE", "LOCK TABLE", "UPDATE ", "DELETE FROM"} {
		if strings.Contains(strings.ToUpper(up), banned) {
			t.Errorf("00009 Up must only create chunk_scan and replace the view, found %q", banned)
		}
	}
	for _, want := range []string{
		"CREATE TABLE chunk_scan",
		"transcript_id UUID NOT NULL REFERENCES transcripts (id) ON DELETE CASCADE",
		"recipe_id TEXT NOT NULL REFERENCES recipes (recipe_id)",
		"fn_call_id BIGINT REFERENCES fn_calls (id)",
		"CONSTRAINT chunk_scan_unique UNIQUE (transcript_id, chunk_index, chunk_text_sha256, recipe_id)",
		"CHECK (quality BETWEEN 1 AND 5)",
		"CREATE OR REPLACE VIEW stale_work AS",
		"SELECT 'scan', 'transcript_chunks', c.id, ls.recipe_id, cur.recipe_id",
		"JOIN cur ON cur.step = 'scan'",
		"encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex')",
		// earlier arms kept
		"SELECT 'asr'::text AS step",
		"AND f.patch_state <> 'superseded'",
		"SELECT 'embed', 'transcript_chunks'",
		"SELECT 'decide', 'transcript_findings', d.finding_id",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("00009 Up is missing %q", want)
		}
	}
	iView, iDrop := strings.Index(down, "CREATE OR REPLACE VIEW stale_work AS"), strings.Index(down, "DROP TABLE chunk_scan")
	if iView < 0 || iDrop < 0 || iView > iDrop {
		t.Error("00009 Down must restore stale_work before dropping chunk_scan")
	}
	if !strings.Contains(down, "SELECT 'decide', 'transcript_findings'") {
		t.Error("00009 Down must restore 00008's view, decide arm included")
	}
	if strings.Contains(down, "'scan'") {
		t.Error("00009 Down's view still has the scan arm")
	}
	if !strings.Contains(resetSQL, "DROP TABLE    IF EXISTS chunk_scan") {
		t.Error("resetSQL must drop chunk_scan (every migration-created table is reset)")
	}
}
