package db

// Postgres proof of the spelling pass's per-book loaders: scoped to one book
// directory with LIKE metacharacters escaped, live findings only, the newest
// full pilot transcript per (track, model), and the caps enforced.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// seedBookTrack inserts a done job + transcript at path, returning the
// transcript id.
func seedBookTrack(t *testing.T, d *DB, path string) string {
	t.Helper()
	ctx := context.Background()
	sum := "it-" + uuid.NewString()
	var jobID, tID string
	if err := d.pool.QueryRow(ctx, `INSERT INTO transcription_jobs (file_path, checksum, status)
		VALUES ($1, $2, 'done') RETURNING id`, path, sum).Scan(&jobID); err != nil {
		t.Fatalf("seed job %s: %v", path, err)
	}
	if err := d.pool.QueryRow(ctx, `INSERT INTO transcripts (job_id, file_path, checksum, language, duration_seconds,
		segments, raw_text, model_name) VALUES ($1, $2, $3, 'en', 40, '[]', 'x', 'm') RETURNING id`,
		jobID, path, sum).Scan(&tID); err != nil {
		t.Fatalf("seed transcript %s: %v", path, err)
	}
	return tID
}

func TestIntegrationSpellingLoaders(t *testing.T) {
	ctx := context.Background()
	_, d := migratedTestDB(t)
	const dir = `/books/50%_off\x`
	in1 := seedBookTrack(t, d, dir+"/01.m4b")
	in2 := seedBookTrack(t, d, dir+"/02.m4b")
	// Would match an unescaped pattern ('%' and '_' are wildcards), and a
	// sub-directory track that is not this book's.
	out1 := seedBookTrack(t, d, `/books/50XYoff\x/01.m4b`)
	out2 := seedBookTrack(t, d, `/books/50%_off\xtra/01.m4b`)
	// A sub-directory is another book (filepath.Dir differs).
	sub := seedBookTrack(t, d, dir+"/CD2/01.m4b")

	finding := func(tID, path, orig, repl, state string) string {
		var id string
		if err := d.pool.QueryRow(ctx, `INSERT INTO transcript_findings (transcript_id, file_path, chunk_index,
			start_sec, end_sec, original_text, issue_type, suggested_correction, confidence, model, patch_state)
			VALUES ($1, $2, 0, 0, 1, $3, 'misheard_proper_noun', $4, 0.9, 'm', $5) RETURNING id::text`,
			tID, path, orig, repl, state).Scan(&id); err != nil {
			t.Fatalf("seed finding: %v", err)
		}
		return id
	}
	want := []string{
		finding(in1, dir+"/01.m4b", "ilmus", "ilmas", "proposed"),
		finding(in1, dir+"/01.m4b", "ilmus", "ilmis", "accepted"),
		finding(in2, dir+"/02.m4b", "ilmus", "ilmas", "applied"),
	}
	rep := want[0]
	finding(in2, dir+"/02.m4b", "ilmus", "ilmos", "rejected")
	finding(out1, `/books/50XYoff\x/01.m4b`, "ilmus", "ilmas", "proposed")
	finding(out2, `/books/50%_off\xtra/01.m4b`, "ilmus", "ilmas", "proposed")
	finding(sub, dir+"/CD2/01.m4b", "ilmus", "ilmas", "proposed")
	dup := finding(in2, dir+"/02.m4b", "ilmus", "ilmas", "proposed")
	if _, err := d.pool.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'duplicate', duplicate_of = $1 WHERE id = $2`, rep, dup); err != nil {
		t.Fatal(err)
	}
	sup := finding(in2, dir+"/02.m4b", "ilmus", "ilmas", "proposed")
	if _, err := d.pool.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'superseded', superseded_at = now() WHERE id = $1`, sup); err != nil {
		t.Fatal(err)
	}

	got, err := d.SpellingFindings(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range got {
		ids = append(ids, f.ID)
		if f.Original != "ilmus" || f.Replacement == "" {
			t.Errorf("finding %+v", f)
		}
	}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("findings %v, want the book's live findings %v", ids, want)
	}
	if _, err := spellingFindings(ctx, d.pool, dir, 2); !errors.Is(err, ErrSpellingCapExceeded) {
		t.Errorf("more findings than the cap: %v, want ErrSpellingCapExceeded", err)
	}

	pilot := func(tID, path, model, rev, scope, text string) {
		if _, err := d.pool.Exec(ctx, `INSERT INTO pilot_transcripts (file_path, production_transcript_id, backend,
			model_id, model_revision, scope, has_word_times, segments, raw_text)
			VALUES ($1, $2, 'b', $3, $4, $5, false, '[]', $6)`, path, tID, model, rev, scope, text); err != nil {
			t.Fatalf("seed pilot: %v", err)
		}
	}
	pilot(in1, dir+"/01.m4b", "granite", "r1", "full", "old revision ilmus")
	pilot(in1, dir+"/01.m4b", "granite", "r2", "full", "ilmas one")
	pilot(in2, dir+"/02.m4b", "granite", "r2", "full", "ilmas two")
	pilot(in1, dir+"/01.m4b", "canary", "r1", "full", "ilmas three")
	pilot(in1, dir+"/01.m4b", "canary", "r1", "findings", "windows only")
	pilot(out1, `/books/50XYoff\x/01.m4b`, "canary", "r1", "full", "other book")
	pilot(sub, dir+"/CD2/01.m4b", "canary", "r1", "full", "sub-directory book")
	texts := map[string][]string{}
	if err := d.StreamPilotText(ctx, dir, func(m, s string) error { texts[m] = append(texts[m], s); return nil }); err != nil {
		t.Fatal(err)
	}
	slices.Sort(texts["granite"])
	if !slices.Equal(texts["granite"], []string{"ilmas one", "ilmas two"}) || !slices.Equal(texts["canary"], []string{"ilmas three"}) || len(texts) != 2 {
		t.Errorf("pilot texts %v (newest full row per track and model, this book only)", texts)
	}
	if err := streamPilotText(ctx, d.pool, dir, 2, func(string, string) error { return nil }); !errors.Is(err, ErrSpellingCapExceeded) {
		t.Errorf("more pilot rows than the cap: %v, want ErrSpellingCapExceeded", err)
	}
	stop := errors.New("stop")
	if err := d.StreamPilotText(ctx, dir, func(string, string) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("callback error %v", err)
	}

	chunk := func(tID, path string, idx int, text, source any) {
		if _, err := d.pool.Exec(ctx, `INSERT INTO transcript_chunks (transcript_id, file_path, chunk_index,
			start_sec, end_sec, text, source_text, embedding) VALUES ($1, $2, $3, 0, 1, $4, $5, array_fill(0.1, ARRAY[768])::vector)`,
			tID, path, idx, text, source); err != nil {
			t.Fatalf("seed chunk: %v", err)
		}
	}
	chunk(in1, dir+"/01.m4b", 0, "corrected ilmas", "pristine ilmus")
	chunk(in2, dir+"/02.m4b", 0, "legacy text", nil)
	chunk(out2, `/books/50%_off\xtra/01.m4b`, 0, "not this book", nil)
	chunk(sub, dir+"/CD2/01.m4b", 0, "sub-directory book", nil)
	var prod []string
	if err := d.StreamProductionText(ctx, dir, func(s string) error { prod = append(prod, s); return nil }); err != nil {
		t.Fatal(err)
	}
	slices.Sort(prod)
	if !slices.Equal(prod, []string{"legacy text", "pristine ilmus"}) {
		t.Errorf("production text %v (source_text, else text; this book only)", prod)
	}
	if err := streamProductionText(ctx, d.pool, dir, 1, func(string) error { return nil }); !errors.Is(err, ErrSpellingCapExceeded) {
		t.Errorf("more chunks than the cap: %v, want ErrSpellingCapExceeded", err)
	}
}
