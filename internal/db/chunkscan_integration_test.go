package db

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"testing"

	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/recipe"
)

const (
	csT1 = "00000000-0000-0000-0000-0000000000c1" // a book with a catalogue record
	csT2 = "00000000-0000-0000-0000-0000000000c2" // a book without one
)

func seedChunkScan(t *testing.T, d *DB) {
	t.Helper()
	segs, _ := json.Marshal([]Segment{
		{ID: 0, Start: 0, End: 10, Text: "seg zero"},
		{ID: 1, Start: 10, End: 20, Text: "seg one"},
		{ID: 2, Start: 20, End: 30, Text: "seg two"},
		{ID: 3, Start: 30, End: 40, Text: "seg three"},
		{ID: 4, Start: 40, End: 50, Text: "seg four"},
	})
	_, err := d.pool.Exec(context.Background(), `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('00000000-0000-0000-0000-0000000000a1', '/b/Dune/01.m4b', 'cs1', 'done'),
		  ('00000000-0000-0000-0000-0000000000a2', '/b/Other/01.m4b', 'cs2', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
		                         segments, raw_text, model_name) VALUES
		  ('`+csT1+`', '00000000-0000-0000-0000-0000000000a1', '/b/Dune/01.m4b', 'cs1', 'en', 50, '`+string(segs)+`'::jsonb, 'x', 'm'),
		  ('`+csT2+`', '00000000-0000-0000-0000-0000000000a2', '/b/Other/01.m4b', 'cs2', 'en', 10, '[]', 'x', 'm');
		INSERT INTO transcript_chunks (transcript_id, file_path, chunk_index, start_sec, end_sec, text, source_text, embedding) VALUES
		  ('`+csT1+`', '/b/Dune/01.m4b', 0,  0, 20, 'seg zero seg one',   NULL, array_fill(0.1, ARRAY[768])::vector),
		  ('`+csT1+`', '/b/Dune/01.m4b', 1, 20, 30, 'Seg Two (fixed)',    'seg two', array_fill(0.1, ARRAY[768])::vector),
		  ('`+csT1+`', '/b/Dune/01.m4b', 2, 30, 50, 'seg three seg four', NULL, array_fill(0.1, ARRAY[768])::vector),
		  ('`+csT2+`', '/b/Other/01.m4b', 0, 0, 10, 'other book text',    NULL, array_fill(0.1, ARRAY[768])::vector);
		INSERT INTO book_metadata (book_dir, title, asin) VALUES ('/b/Dune', 'Dune', 'B00ASIN'), ('/b/Other', 'Other', NULL);`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func scanRecipe(prompt string) recipe.Recipe {
	return recipe.Recipe{Step: recipe.StepScan, StepVersion: 1, CodeVersion: "test", ModelAlias: "jev-1.13.0",
		ModelResolved: "jev-1.13.0", PromptVersion: "scan_chunk@v1", PromptSHA256: prompt,
		Params: map[string]any{"fn": "scan_chunk", "context_segments": 2}}
}

func scanRow(c ScanCandidate, rid string, quality, boiler float64) ChunkScan {
	return ChunkScan{TranscriptID: c.TranscriptID, ChunkIndex: c.ChunkIndex, ChunkTextSHA256: c.TextSHA256,
		RecipeID: rid, PNeedsFix: 0.5, Quality: quality, PBoilerplate: boiler, PGarbled: 0, PDialogue: 0,
		IssueType: "none", IssueProbs: map[string]float64{"none": 1}}
}

// TestIntegrationChunkScan: candidates carry pristine text, its Postgres hash
// and segment context; the write re-checks the hash and is idempotent; the
// stale_work scan arm lists exactly the chunks with no current-recipe scan of
// their current text; the quality groups count current text only, cut
// boilerplate and split on ASIN. Skipped unless EARMARK_TEST_DATABASE_URL.
func TestIntegrationChunkScan(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()
	seedChunkScan(t, d)

	cur := scanRecipe("p1")
	rid, err := d.RegisterRecipe(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}

	all, err := d.ScanCandidates(ctx, ScanScope{RecipeID: rid, ContextSegments: 2})
	if err != nil {
		t.Fatalf("ScanCandidates: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("%d candidates, want 4", len(all))
	}
	mid := all[1] // csT1 chunk 1 (20-30 s)
	if mid.TranscriptID != csT1 || mid.ChunkIndex != 1 || mid.Text != "seg two" || mid.TextSHA256 != patch.ChunkHash("seg two") {
		t.Errorf("chunk 1 = %+v (pristine text and its Go hash expected)", mid)
	}
	if !slices.Equal(mid.ContextBefore, []string{"seg zero", "seg one"}) || !slices.Equal(mid.ContextAfter, []string{"seg three", "seg four"}) {
		t.Errorf("context = %v | %v", mid.ContextBefore, mid.ContextAfter)
	}
	if c, _ := d.ScanCandidates(ctx, ScanScope{ContextSegments: 1}); len(c) != 4 ||
		!slices.Equal(c[1].ContextBefore, []string{"seg one"}) || len(c[0].ContextBefore) != 0 {
		t.Errorf("one segment of context: %+v", c)
	}
	if c, _ := d.ScanCandidates(ctx, ScanScope{Book: "other"}); len(c) != 1 || c[0].TranscriptID != csT2 {
		t.Errorf("--book other = %+v", c)
	}
	// A seeded sample is reproducible; keyset pages cover everything once.
	s1, _ := d.ScanCandidates(ctx, ScanScope{Sample: 2, Seed: "x"})
	s2, _ := d.ScanCandidates(ctx, ScanScope{Sample: 2, Seed: "x"})
	if len(s1) != 2 || s1[0].ChunkID != s2[0].ChunkID || s1[1].ChunkID != s2[1].ChunkID {
		t.Errorf("seeded sample not reproducible: %v vs %v", s1, s2)
	}
	p1, _ := d.ScanCandidates(ctx, ScanScope{Limit: 3})
	p2, _ := d.ScanCandidates(ctx, ScanScope{Limit: 3, After: p1[len(p1)-1].Cursor()})
	if len(p1) != 3 || len(p2) != 1 {
		t.Errorf("pages = %d + %d, want 3 + 1", len(p1), len(p2))
	}

	// No current scan recipe yet: the scan arm reports nothing.
	if n := countStale(t, d, "scan"); n != 0 {
		t.Errorf("scan arm without a current recipe = %d, want 0", n)
	}
	if err := d.SetCurrentRecipes(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if n := countStale(t, d, "scan"); n != 4 {
		t.Errorf("unscanned chunks stale = %d, want 4", n)
	}

	// Writes: quality 5 (asin book), 3 (asin book), 1 + boilerplate (asin
	// book), 2 (other book).
	rows := []ChunkScan{scanRow(all[0], rid, 5, 0), scanRow(all[1], rid, 3, 0.1),
		scanRow(all[2], rid, 1, 0.9), scanRow(all[3], rid, 2, 0)}
	for i, r := range rows {
		if got, err := d.InsertChunkScan(ctx, r); err != nil || got != ChunkScanInserted {
			t.Fatalf("insert %d = %v, %v", i, got, err)
		}
	}
	if got, err := d.InsertChunkScan(ctx, rows[0]); err != nil || got != ChunkScanExists {
		t.Errorf("re-insert = %v, %v; want exists", got, err)
	}
	stale := rows[0]
	stale.ChunkTextSHA256 = patch.ChunkHash("text the chunk no longer has")
	if got, err := d.InsertChunkScan(ctx, stale); err != nil || got != ChunkScanChanged {
		t.Errorf("stale-hash insert = %v, %v; want changed", got, err)
	}
	if n := countStale(t, d, "scan"); n != 0 {
		t.Errorf("after scanning everything stale = %d, want 0", n)
	}
	if c, _ := d.ScanCandidates(ctx, ScanScope{RecipeID: rid}); len(c) != 0 {
		t.Errorf("scanned chunks are still candidates: %+v", c)
	}

	// Quality groups: current text only, boilerplate cut, ASIN split.
	groups, err := d.QualityGroups(ctx, 0.5)
	if err != nil {
		t.Fatalf("QualityGroups: %v", err)
	}
	want := map[bool]QualityGroup{
		true:  {RecipeID: rid, ASINMatched: true, Chunks: 2, QualitySum: 8},
		false: {RecipeID: rid, ASINMatched: false, Chunks: 1, QualitySum: 2},
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	for _, g := range groups {
		w := want[g.ASINMatched]
		if g.RecipeID != w.RecipeID || g.Chunks != w.Chunks || math.Abs(g.QualitySum-w.QualitySum) > 1e-9 {
			t.Errorf("group = %+v, want %+v", g, w)
		}
	}

	// A rebuild changes chunk 0's text: its scan no longer counts and it is
	// stale again; a new current recipe makes every chunk stale.
	if _, err := d.pool.Exec(ctx, `UPDATE transcript_chunks SET text = 'rebuilt text'
		WHERE transcript_id = $1 AND chunk_index = 0`, csT1); err != nil {
		t.Fatal(err)
	}
	if n := countStale(t, d, "scan"); n != 1 {
		t.Errorf("after a text change stale = %d, want 1", n)
	}
	if g, _ := d.QualityGroups(ctx, 0.5); len(g) != 2 || g[1].Chunks != 1 {
		t.Errorf("groups after rebuild = %+v (chunk 0's scan must drop out)", g)
	}
	if err := d.SetCurrentRecipes(ctx, scanRecipe("p2")); err != nil {
		t.Fatal(err)
	}
	if n := countStale(t, d, "scan"); n != 4 {
		t.Errorf("after a recipe change stale = %d, want 4", n)
	}
	var latest string
	if err := d.pool.QueryRow(ctx, `SELECT coalesce(recipe_id, '') FROM stale_work
		WHERE step = 'scan' AND row_id = (SELECT id FROM transcript_chunks WHERE transcript_id = $1 AND chunk_index = 1)`,
		csT1).Scan(&latest); err != nil || latest != rid {
		t.Errorf("stale scan row recipe = %q, %v; want the latest scan's %s", latest, err, rid)
	}

	// A requeue deletes the transcript; its scans go with it.
	if _, err := d.pool.Exec(ctx, `DELETE FROM transcripts WHERE id = $1`, csT2); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM chunk_scan WHERE transcript_id = $1`, csT2).Scan(&left); err != nil || left != 0 {
		t.Errorf("scans left after transcript delete = %d, %v", left, err)
	}
}

func countStale(t *testing.T, d *DB, step string) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(context.Background(), `SELECT count(*) FROM stale_work WHERE step = $1`, step).Scan(&n); err != nil {
		t.Fatalf("count stale %s: %v", step, err)
	}
	return n
}
