package db

// Postgres integration tests for migration 14's finding_labels and the label
// import (CONTRACT §2.15 "Labels and scorecards"). Skipped unless
// EARMARK_TEST_DATABASE_URL is set (see migrate_integration_test.go).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	lbJob        = "00000000-0000-0000-0000-00000000b001"
	lbTranscript = "00000000-0000-0000-0000-00000000b101"
	lbChunk0     = "00000000-0000-0000-0000-00000000b200"
	lbChunk1     = "00000000-0000-0000-0000-00000000b201"
	lbChunkGone  = "00000000-0000-0000-0000-00000000b2ff"
	lbChunk0Text = "then ganema said she would wait by the gate"
	lbChunk1Text = "and left the the hall"
	lbOldChunk1  = "and left the hall at once"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// seedLabelChunks writes one transcript with two chunks, a finding on each
// (one on the current text of chunk 0, one archived on an older revision of
// chunk 1), and a legacy finding with no chunk hash. Returns the finding ids.
func seedLabelChunks(t *testing.T, conn *pgx.Conn) (current, archived, legacy string) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES ($1, '/b/LB/01.m4b', 'lb', 'done');
		`, lbJob); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name)
		VALUES ($1, $2, '/b/LB/01.m4b', 'lb', 'en', 60, '[]', 'x', 'parakeet')`, lbTranscript, lbJob); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO transcript_chunks (id, transcript_id, file_path, chunk_index, start_sec, end_sec, text, source_text, embedding)
		VALUES ($1, $3, '/b/LB/01.m4b', 0, 0, 30, $4, $4, array_fill(0.1, ARRAY[768])::vector),
		       ($2, $3, '/b/LB/01.m4b', 1, 30, 60, 'and left the hall', $5, array_fill(0.2, ARRAY[768])::vector)`,
		lbChunk0, lbChunk1, lbTranscript, lbChunk0Text, lbChunk1Text); err != nil {
		t.Fatal(err)
	}
	ins := func(chunk string, idx int, hash *string, orig, corr, state string) string {
		var id string
		if err := conn.QueryRow(ctx, `
			INSERT INTO transcript_findings (transcript_id, file_path, chunk_id, chunk_index, start_sec, end_sec,
			       original_text, issue_type, suggested_correction, confidence, model, chunk_text_sha256, patch_state,
			       superseded_at)
			VALUES ($1, '/b/LB/01.m4b', $2, $3, 0, 30, $4, 'misheard_proper_noun', $5, 0.9, 'm', $6, $7,
			        CASE WHEN $7 = 'superseded' THEN now() END)
			RETURNING id`, lbTranscript, chunk, idx, orig, corr, hash, state).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	h0, hOld := sha(lbChunk0Text), sha(lbOldChunk1)
	current = ins(lbChunk0, 0, &h0, "ganema", "Ghanima", "proposed")
	archived = ins(lbChunk1, 1, &hOld, "left the hall", "left the hall", "superseded")
	legacy = ins(lbChunk1, 1, nil, "the the", "the", "proposed")
	return current, archived, legacy
}

// TestIntegrationFindingCorrectionKeyMatchesFindings: the function the label
// table generates its key with is the 13 expression exactly — on a C-locale
// database, with every normalisation case the findings test covers.
func TestIntegrationFindingCorrectionKeyMatchesFindings(t *testing.T) {
	ctx := context.Background()
	dbURL := newTestDatabaseCLocale(t)
	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	conn := connect(t, dbURL)
	seedFindingEvents(t, conn, 0)
	cases := []struct{ hash, orig, corr *string }{}
	s := func(v string) *string { return &v }
	for _, c := range [][3]string{
		{"h1", "ganema", "Ghanima"}, {"h1", " Ganema", "  GHANIMA "}, {"h1", "ÉMILE", "ÉMILE ZOLA"},
		{"h1", "the\tcat", "the  cat\n"}, {"h1", "the\u00a0cat", "a\u2009b"}, {"h1", "émile", "Émile"},
		{"h2", "ab", "c"}, {"h2", "a", "bc"}, {"h3", "x\ufeffy", "z\u3000w"},
	} {
		cases = append(cases, struct{ hash, orig, corr *string }{s(c[0]), s(c[1]), s(c[2])})
	}
	cases = append(cases, struct{ hash, orig, corr *string }{s("h1"), s("the the"), nil})
	cases = append(cases, struct{ hash, orig, corr *string }{nil, s("ganema"), s("Ghanima")})
	for _, c := range cases {
		var fromRow, fromFn *string
		if err := conn.QueryRow(ctx, `INSERT INTO transcript_findings (transcript_id, file_path, chunk_index, start_sec,
			end_sec, original_text, issue_type, suggested_correction, confidence, model, chunk_text_sha256)
			VALUES ($1, '/b/FE/01.m4b', 0, 0, 30, $2, 'misheard_word', $3, 0.5, 'm', $4)
			RETURNING correction_key, finding_correction_key($4, $2, $3)`, feTranscript, c.orig, c.corr, c.hash).
			Scan(&fromRow, &fromFn); err != nil {
			t.Fatal(err)
		}
		if strOr(fromRow) != strOr(fromFn) || (fromRow == nil) != (fromFn == nil) {
			t.Errorf("(%v, %q, %v): findings key %q, finding_correction_key %q", strOr(c.hash), *c.orig, strOr(c.corr), strOr(fromRow), strOr(fromFn))
		}
	}
}

// TestIntegrationFindingLabelsAppendOnly: UPDATE, DELETE and TRUNCATE are
// refused; the reset path (DROP) still clears the table.
func TestIntegrationFindingLabelsAppendOnly(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	current, _, _ := seedLabelChunks(t, conn)
	if _, err := d.ImportLabels(ctx, LabelImport{Set: "s1", Labeler: "human:t",
		Items: []LabelItem{{FindingID: current, Label: LabelGood}}}); err != nil {
		t.Fatal(err)
	}
	isRestrict := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23001"
	}
	for _, sql := range []string{
		`UPDATE finding_labels SET label = 'junk'`,
		`DELETE FROM finding_labels`,
		`TRUNCATE finding_labels`,
	} {
		if _, err := conn.Exec(ctx, sql); !isRestrict(err) {
			t.Errorf("%s: err = %v, want restrict_violation", sql, err)
		}
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_labels`); n != 1 {
		t.Errorf("%d labels after refused writes, want 1", n)
	}
	if _, err := conn.Exec(ctx, `DROP TABLE finding_labels CASCADE`); err != nil {
		t.Errorf("DROP (the reset path) must still work: %v", err)
	}
}

// TestIntegrationFindingLabelsMigrationDownUp: 14 rolls back cleanly and
// re-applies.
func TestIntegrationFindingLabelsMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	dbURL, _ := migratedTestDB(t)
	conn := connect(t, dbURL)
	objects := `SELECT count(*) FROM pg_class WHERE relname IN
		('finding_labels', 'finding_labels_effective', 'recipe_scorecard', 'decide_scorecard')`
	fns := `SELECT count(*) FROM pg_proc WHERE proname IN
		('finding_correction_key', 'finding_labels_dedupe_key', 'finding_labels_append_only')`
	if countRows(t, conn, objects) != 4 || countRows(t, conn, fns) != 3 {
		t.Fatal("migration 14 objects missing after Up")
	}
	if _, err := providerFor(t, dbURL).DownTo(ctx, 13); err != nil {
		t.Fatalf("down to 13: %v", err)
	}
	if n, m := countRows(t, conn, objects), countRows(t, conn, fns); n != 0 || m != 0 {
		t.Errorf("after Down: %d relations, %d functions left", n, m)
	}
	if _, err := providerFor(t, dbURL).UpTo(ctx, 14); err != nil {
		t.Fatalf("up to 14 again: %v", err)
	}
	if countRows(t, conn, objects) != 4 || countRows(t, conn, fns) != 3 {
		t.Error("migration 14 objects missing after Up/Down/Up")
	}
}

// TestIntegrationImportLabels: resolution by finding, by hash and by chunk
// span; a changed chunk, a stale context, a disagreeing finding and an
// unknown revision are unresolved and never stored; re-import is a no-op;
// a dry run writes nothing.
func TestIntegrationImportLabels(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)
	current, archived, legacy := seedLabelChunks(t, conn)
	day := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	items := []LabelItem{
		// 0: by finding — texts taken from the finding.
		{SourceRef: "F", FindingID: current, Label: LabelGood, Reason: "Ghanima", LabelledAt: day},
		// 1: by chunk span, same fix as 0 under another casing — same key.
		{SourceRef: "C", ChunkID: lbChunk0, Original: "GANEMA ", Correction: "ghanima", Label: LabelGood, Reason: "Ghanima"},
		// 2: by hash of an archived finding's (older) revision.
		{SourceRef: "H", ChunkID: lbChunk1, ChunkTextSHA256: sha(lbOldChunk1), Original: "left the hall",
			Correction: "left the hall", Label: LabelJunk},
		// 3: legacy finding (no hash): pinned to the finding.
		{SourceRef: "L", FindingID: legacy, Label: LabelPartial},
		// 4: span not in the current chunk text (the chunk changed since).
		{SourceRef: "X1", ChunkID: lbChunk1, Original: "at once", Correction: "at one", Label: LabelGood},
		// 5: span present but the labelled context is not.
		{SourceRef: "X2", ChunkID: lbChunk1, Original: "left", Correction: "lift", Context: "and left the hall at once",
			Label: LabelGood},
		// 6: hash nobody has seen.
		{SourceRef: "X3", ChunkID: lbChunk1, ChunkTextSHA256: sha("never"), Original: "left", Correction: "lift", Label: LabelGood},
		// 7: finding whose texts disagree with the item.
		{SourceRef: "X4", FindingID: current, Original: "gamma", Correction: "Ghanima", Label: LabelGood},
		// 8: chunk gone.
		{SourceRef: "X5", ChunkID: lbChunkGone, Original: "x", Correction: "y", Label: LabelGood},
		// 9: hash given that is not the finding's.
		{SourceRef: "X6", FindingID: current, ChunkTextSHA256: sha(lbOldChunk1), Label: LabelGood},
		// 10: invalid label.
		{SourceRef: "X7", FindingID: current, Label: "maybe"},
		// 11: context present in the text column (corrected surface).
		{SourceRef: "T", ChunkID: lbChunk1, Original: "the the", Correction: "the", Context: "left the hall",
			Label: LabelGood},
		// 12: repeat of 1 inside the same file.
		{SourceRef: "C2", ChunkID: lbChunk0, Original: "ganema", Correction: "Ghanima", Label: LabelGood, Reason: "Ghanima"},
	}
	_ = archived
	imp := LabelImport{Set: "bakeoff-test", Labeler: "opus:test", Items: items, DryRun: true}
	dry, err := d.ImportLabels(ctx, imp)
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_labels`); n != 0 {
		t.Fatalf("dry run wrote %d labels", n)
	}
	wantUnresolved := map[string]string{
		"X1": UnresolvedSpanNotInChunk, "X2": UnresolvedContextNotInChunk, "X3": UnresolvedUnknownRevision,
		"X4": UnresolvedFindingTextMismatch, "X5": UnresolvedChunkNotFound, "X6": UnresolvedHashMismatch,
		"X7": UnresolvedInvalid,
	}
	got := map[string]string{}
	for _, u := range dry.UnresolvedItems {
		got[u.SourceRef] = u.Reason
	}
	for ref, want := range wantUnresolved {
		if got[ref] != want {
			t.Errorf("%s: unresolved %q, want %q", ref, got[ref], want)
		}
	}
	if len(got) != len(wantUnresolved) {
		t.Errorf("unresolved %v, want exactly %v", got, wantUnresolved)
	}
	// 0 and 1 are the same fix with the same reason → one row; 12 repeats 1.
	if dry.Inserted != 4 || dry.Skipped != 2 || dry.Unresolved != 7 {
		t.Errorf("dry run inserted %d skipped %d unresolved %d, want 4/2/7", dry.Inserted, dry.Skipped, dry.Unresolved)
	}
	if dry.ByResolution[LabelResolvedFinding] != 2 || dry.ByResolution[LabelResolvedChunk] != 3 || dry.ByResolution[LabelResolvedHash] != 1 {
		t.Errorf("by resolution %v", dry.ByResolution)
	}

	imp.DryRun = false
	rep, err := d.ImportLabels(ctx, imp)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Inserted != dry.Inserted || rep.Skipped != dry.Skipped || rep.Unresolved != dry.Unresolved {
		t.Errorf("import %+v differs from its dry run %+v", rep, dry)
	}
	// The label for item 0 keys exactly like the finding.
	if n := countRows(t, conn, `SELECT count(*) FROM finding_labels l JOIN transcript_findings f
		ON f.correction_key = l.correction_key WHERE f.id = $1`, current); n != 1 {
		t.Errorf("%d labels share the finding's correction_key, want 1", n)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_labels WHERE finding_id = $1
		AND correction_key IS NULL AND label = 'partial'`, legacy); n != 1 {
		t.Error("the legacy finding's label must be pinned to its id")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_labels WHERE source_ref = 'H'
		AND resolved_by = 'hash' AND chunk_text_sha256 = $1`, sha(lbOldChunk1)); n != 1 {
		t.Error("the archived revision's label must keep its own hash")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM finding_labels WHERE labelled_at = '2026-10-10'`); n != 1 {
		t.Error("labelled_at from the item must be stored")
	}

	again, err := d.ImportLabels(ctx, imp)
	if err != nil {
		t.Fatal(err)
	}
	if again.Inserted != 0 || again.Skipped != rep.Inserted+rep.Skipped {
		t.Errorf("re-import inserted %d skipped %d, want 0/%d", again.Inserted, again.Skipped, rep.Inserted+rep.Skipped)
	}
	// A changed label is a new row, and the newest is in force.
	newer := LabelImport{Set: "bakeoff-test", Labeler: "opus:test", Items: []LabelItem{
		{FindingID: current, Label: LabelJunk, LabelledAt: day.Add(48 * time.Hour)}}}
	if r, err := d.ImportLabels(ctx, newer); err != nil || r.Inserted != 1 {
		t.Fatalf("relabel: %+v, %v", r, err)
	}
	var label string
	if err := conn.QueryRow(ctx, `SELECT e.label FROM finding_labels_effective e
		JOIN transcript_findings f ON f.correction_key = e.correction_key WHERE f.id = $1`, current).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != LabelJunk {
		t.Errorf("label in force %q, want the newer junk", label)
	}
	// A human label outranks a newer model label.
	human := LabelImport{Set: "review", Labeler: "human:t", Items: []LabelItem{
		{FindingID: current, Label: LabelGood, LabelledAt: day}}}
	if _, err := d.ImportLabels(ctx, human); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT e.label FROM finding_labels_effective e
		JOIN transcript_findings f ON f.correction_key = e.correction_key WHERE f.id = $1`, current).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != LabelGood {
		t.Errorf("label in force %q, want the human's good", label)
	}

	// --require-hash refuses span-only matches.
	strict := LabelImport{Set: "strict", Labeler: "opus:test", RequireHash: true, DryRun: true, Items: []LabelItem{
		{SourceRef: "S", ChunkID: lbChunk0, Original: "ganema", Correction: "Ghanima", Label: LabelGood}}}
	sr, err := d.ImportLabels(ctx, strict)
	if err != nil {
		t.Fatal(err)
	}
	if sr.Unresolved != 1 || sr.UnresolvedItems[0].Reason != UnresolvedNoHash {
		t.Errorf("require-hash: %+v", sr)
	}
}
