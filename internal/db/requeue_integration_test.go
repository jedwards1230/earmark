package db

// Postgres integration tests for requeue-as-archive (CONTRACT §1.4, §2.17):
// migration 00005's foreign key and orphan cleanup, the superseded state, and
// every requeue entry point superseding findings in the same transaction as the
// transcript delete. Skipped unless EARMARK_TEST_DATABASE_URL is set (see
// migrate_integration_test.go).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	rqJobA        = "00000000-0000-0000-0000-00000000000a"
	rqJobB        = "00000000-0000-0000-0000-00000000000b"
	rqTranscriptA = "00000000-0000-0000-0000-0000000000a1"
	rqTranscriptB = "00000000-0000-0000-0000-0000000000b1"
)

// seedRequeueRows writes two done, judged, corrected tracks: book A (job A,
// failed so --failed selects it too) with findings in every pre-requeue state
// including a human correction, and book B with one proposed finding that no
// requeue of A may touch.
func seedRequeueRows(t *testing.T, dbURL string) {
	t.Helper()
	_, err := connect(t, dbURL).Exec(context.Background(), `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('`+rqJobA+`', '/b/Author/A/01.m4b', 'c1', 'failed'),
		  ('`+rqJobB+`', '/b/Author/B/01.m4b', 'c2', 'done');
		INSERT INTO run_metrics (job_id, embed_model, eval_finished_at) VALUES
		  ('`+rqJobA+`', 'nomic-embed-text', now()),
		  ('`+rqJobB+`', 'nomic-embed-text', now());
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
		                         segments, raw_text, model_name) VALUES
		  ('`+rqTranscriptA+`', '`+rqJobA+`', '/b/Author/A/01.m4b', 'c1', 'en', 60, '[]', 'ganema said', 'parakeet'),
		  ('`+rqTranscriptB+`', '`+rqJobB+`', '/b/Author/B/01.m4b', 'c2', 'en', 60, '[]', 'the the cat', 'parakeet');
		INSERT INTO transcript_chunks (transcript_id, file_path, chunk_index, start_sec, end_sec,
		                               text, source_text, embedding) VALUES
		  ('`+rqTranscriptA+`', '/b/Author/A/01.m4b', 0, 0, 30, 'Ghanima said', 'ganema said', array_fill(0.1, ARRAY[768])::vector),
		  ('`+rqTranscriptA+`', '/b/Author/A/01.m4b', 1, 30, 60, 'and left',    'and left',    array_fill(0.2, ARRAY[768])::vector),
		  ('`+rqTranscriptB+`', '/b/Author/B/01.m4b', 0, 0, 60, 'the the cat',  NULL,          array_fill(0.3, ARRAY[768])::vector);
		INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec, original_text,
		                                 issue_type, suggested_correction, confidence, model, origin, patch_state) VALUES
		  ('`+rqTranscriptA+`', '/b/Author/A/01.m4b', 0, 30, 'ganema', 'misheard_proper_noun', 'Ghanima', 0.9, 'gemma3:12b', 'judge', 'proposed'),
		  ('`+rqTranscriptA+`', '/b/Author/A/01.m4b', 0, 30, 'ganema', 'misheard_proper_noun', 'Ghanima', 0.9, 'gemma3:12b', 'judge', 'accepted'),
		  ('`+rqTranscriptA+`', '/b/Author/A/01.m4b', 0, 30, 'ganema', 'misheard_proper_noun', 'Ghanima', 0.9, 'gemma3:12b', 'judge', 'applied'),
		  ('`+rqTranscriptA+`', '/b/Author/A/01.m4b', 0, 30, 'said',   'misheard_word',        'sad',     0.4, 'gemma3:12b', 'judge', 'rejected'),
		  ('`+rqTranscriptA+`', '/b/Author/A/01.m4b', 0, 30, 'said',   'misheard_word',        'sad',     0.4, 'gemma3:12b', 'judge', 'reverted'),
		  ('`+rqTranscriptA+`', '/b/Author/A/01.m4b', 0, 30, 'said',   'misheard_word',        'sad',     0.4, 'gemma3:12b', 'judge', 'stale'),
		  ('`+rqTranscriptA+`', '/b/Author/A/01.m4b', 30, 60, 'left',  'misheard_word',        'leapt',   1,   'manual',     'human', 'accepted'),
		  ('`+rqTranscriptB+`', '/b/Author/B/01.m4b', 0, 60, 'the the', 'repeated_text',       'the',     0.9, 'gemma3:12b', 'judge', 'proposed');
	`)
	if err != nil {
		t.Fatalf("seed requeue rows: %v", err)
	}
}

func migratedTestDB(t *testing.T) (string, *DB) {
	t.Helper()
	dbURL := newTestDatabase(t)
	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return dbURL, &DB{pool: pool, log: testLog()}
}

func countRows(t *testing.T, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// TestIntegrationRequeueSupersedesFindings: every requeue entry point — the CLI
// substring, --failed, the dashboard per-track and per-book buttons — archives
// the replaced transcript's findings as superseded (human decisions included,
// none deleted), drops the transcript, its chunks and its eval latch, and
// leaves other books alone. The re-transcribed track is then judged fresh: it
// has no finding keys and an empty correction overlay.
func TestIntegrationRequeueSupersedesFindings(t *testing.T) {
	integrationServer(t)
	entryPoints := map[string]func(ctx context.Context, d *DB) error{
		"RequeueJobs (CLI substring)": func(ctx context.Context, d *DB) error {
			_, err := d.RequeueJobs(ctx, "Author/A")
			return err
		},
		"RequeueFailed (--failed, retry-failed button)": func(ctx context.Context, d *DB) error {
			_, err := d.RequeueFailed(ctx)
			return err
		},
		"RequeueByID (dashboard row)": func(ctx context.Context, d *DB) error {
			_, err := d.RequeueByID(ctx, rqJobA)
			return err
		},
		"RequeueByDir (book page)": func(ctx context.Context, d *DB) error {
			_, err := d.RequeueByDir(ctx, "/b/Author/A")
			return err
		},
	}
	for name, requeue := range entryPoints {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dbURL, d := migratedTestDB(t)
			seedRequeueRows(t, dbURL)
			conn := connect(t, dbURL)

			if err := requeue(ctx, d); err != nil {
				t.Fatalf("requeue: %v", err)
			}

			if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings WHERE file_path LIKE '/b/Author/A/%'`); n != 7 {
				t.Errorf("book A has %d findings after requeue, want all 7 kept", n)
			}
			if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
				WHERE file_path LIKE '/b/Author/A/%'
				  AND (patch_state <> 'superseded' OR superseded_at IS NULL OR transcript_id IS NOT NULL)`); n != 0 {
				t.Errorf("%d book-A findings not archived (want superseded, stamped, transcript_id NULL)", n)
			}
			if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
				WHERE origin = 'human' AND patch_state = 'superseded'`); n != 1 {
				t.Errorf("human correction superseded count = %d, want 1", n)
			}
			if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
				WHERE transcript_id = $1 AND patch_state = 'proposed' AND superseded_at IS NULL`, rqTranscriptB); n != 1 {
				t.Error("book B's finding was touched by a requeue of book A")
			}
			if n := countRows(t, conn, `SELECT count(*) FROM transcripts WHERE job_id = $1`, rqJobA); n != 0 {
				t.Error("book A's transcript survived the requeue")
			}
			if n := countRows(t, conn, `SELECT count(*) FROM transcript_chunks WHERE file_path LIKE '/b/Author/A/%'`); n != 0 {
				t.Error("book A's chunks survived the requeue")
			}
			if n := countRows(t, conn, `SELECT count(*) FROM run_metrics WHERE job_id = $1`, rqJobA); n != 0 {
				t.Error("book A's run_metrics (eval latch) survived the requeue")
			}
			if n := countRows(t, conn, `SELECT count(*) FROM transcription_jobs WHERE id = $1 AND status = 'pending'`, rqJobA); n != 1 {
				t.Error("book A's job is not pending")
			}

			// The runner re-transcribes: a NEW transcript for the same job. It is
			// judged fresh — no finding keys to dedupe against, nothing to replay.
			const newTranscript = "00000000-0000-0000-0000-0000000000a2"
			if _, err := conn.Exec(ctx, `INSERT INTO transcripts (id, job_id, file_path, checksum, language,
				duration_seconds, segments, raw_text, model_name)
				VALUES ($1, $2, '/b/Author/A/01.m4b', 'c1', 'en', 60, '[]', 'Ghanima said', 'parakeet')`,
				newTranscript, rqJobA); err != nil {
				t.Fatal(err)
			}
			keys, err := d.GetFindingKeys(ctx, newTranscript)
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) != 0 {
				t.Errorf("new transcript inherits %d finding keys, want 0", len(keys))
			}
			overlay, _, err := d.GetCorrectionOverlay(ctx, newTranscript)
			if err != nil {
				t.Fatal(err)
			}
			if len(overlay) != 0 {
				t.Errorf("new transcript replays %d superseded corrections, want 0", len(overlay))
			}

			// Superseded rows still list for audit (NULL transcript_id reads as "").
			rows, err := d.ListCorrections(ctx, CorrectionFilter{States: []string{"superseded"}, Limit: 50})
			if err != nil {
				t.Fatalf("list superseded: %v", err)
			}
			if len(rows) != 7 {
				t.Errorf("listed %d superseded findings, want 7", len(rows))
			}
		})
	}
}

// TestIntegrationFindingsTranscriptFK: the foreign key refuses a finding for a
// transcript that does not exist, and a transcript delete keeps its findings
// with transcript_id set to NULL. The CHECK admits 'superseded' only.
func TestIntegrationFindingsTranscriptFK(t *testing.T) {
	ctx := context.Background()
	dbURL, _ := migratedTestDB(t)
	seedRequeueRows(t, dbURL)
	conn := connect(t, dbURL)

	_, err := conn.Exec(ctx, `INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec,
		original_text, issue_type, confidence, model)
		VALUES ('00000000-0000-0000-0000-0000000000ff', '/b/x.m4b', 0, 1, 'x', 'other', 0.5, 'm')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("insert for a missing transcript: err = %v, want foreign_key_violation (23503)", err)
	}

	if _, err := conn.Exec(ctx, `DELETE FROM transcripts WHERE id = $1`, rqTranscriptB); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE file_path = '/b/Author/B/01.m4b' AND transcript_id IS NULL`); n != 1 {
		t.Errorf("finding of a deleted transcript: %d rows with NULL transcript_id, want 1 (ON DELETE SET NULL)", n)
	}

	if _, err := conn.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'superseded'
		WHERE file_path = '/b/Author/B/01.m4b'`); err != nil {
		t.Errorf("CHECK refuses superseded: %v", err)
	}
	_, err = conn.Exec(ctx, `UPDATE transcript_findings SET patch_state = 'bogus' WHERE file_path = '/b/Author/B/01.m4b'`)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("CHECK admits an unknown state: err = %v", err)
	}
}

// TestIntegrationMigrationArchivesOrphans: migrating a database that already
// holds orphaned findings (from requeues before the foreign key) archives them
// as superseded with a NULL transcript_id and adds a VALIDATED foreign key. It
// also rebuilds the patch_state CHECK from the list it finds — a state another
// migration added (here simulated as 'unanchorable') survives — and its Down
// reverts cleanly.
func TestIntegrationMigrationArchivesOrphans(t *testing.T) {
	ctx := context.Background()
	dbURL := newTestDatabase(t)
	applyLegacyInitialize(t, dbURL)
	seedLegacyRows(t, dbURL)
	conn := connect(t, dbURL)
	if _, err := conn.Exec(ctx, `INSERT INTO transcript_findings (transcript_id, file_path, start_sec, end_sec,
		original_text, issue_type, confidence, model, patch_state)
		VALUES ('00000000-0000-0000-0000-0000000000ff', '/b/gone.m4b', 0, 1, 'x', 'other', 0.5, 'm', 'accepted')`); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgx.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = sqlDB.Close() })
	provider, err := newMigrationProvider(sqlDB, testLog())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 3); err != nil {
		t.Fatalf("up to 3: %v", err)
	}
	// A sibling migration widens the CHECK before 00005 runs.
	if _, err := conn.Exec(ctx, `ALTER TABLE transcript_findings DROP CONSTRAINT transcript_findings_patch_state_valid;
		ALTER TABLE transcript_findings ADD CONSTRAINT transcript_findings_patch_state_valid
		  CHECK (patch_state IN ('proposed','accepted','rejected','applied','stale','reverted','unanchorable'))`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(itCtx(t), dbURL, testLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE file_path = '/b/gone.m4b' AND transcript_id IS NULL AND patch_state = 'superseded'`); n != 1 {
		t.Error("orphaned finding was not archived as superseded with a NULL transcript_id")
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE patch_state = 'proposed' AND transcript_id IS NOT NULL`); n != 5 {
		t.Errorf("%d live findings untouched, want the 5 seeded ones", n)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM pg_constraint
		WHERE conname = 'transcript_findings_transcript_id_fkey' AND convalidated AND confdeltype = 'n'`); n != 1 {
		t.Error("foreign key missing, not validated, or not ON DELETE SET NULL")
	}
	var def string
	if err := conn.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'transcript_findings_patch_state_valid'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"proposed", "accepted", "rejected", "applied", "stale", "reverted", "unanchorable", "superseded"} {
		if !strings.Contains(def, "'"+s+"'") {
			t.Errorf("patch_state CHECK lost %q: %s", s, def)
		}
	}

	// Down: superseded → stale, CHECK without superseded, FK gone; the column
	// stays nullable because the archived orphan has no transcript.
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("down to 3: %v", err)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM transcript_findings
		WHERE file_path = '/b/gone.m4b' AND patch_state = 'stale' AND stale_reason = 'superseded'`); n != 1 {
		t.Error("Down did not map superseded to stale")
	}
	if err := conn.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'transcript_findings_patch_state_valid'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(def, "superseded") || !strings.Contains(def, "unanchorable") {
		t.Errorf("Down CHECK = %s", def)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM pg_constraint WHERE conname = 'transcript_findings_transcript_id_fkey'`); n != 0 {
		t.Error("Down kept the foreign key")
	}
}
