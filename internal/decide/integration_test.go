package decide

// Postgres proof that `earmark decide` is a dry run: selection is
// deterministic and scoped, and a run leaves every table but fn_calls and
// recipes untouched. Skipped unless EARMARK_TEST_DATABASE_URL points at a
// server the test may create and drop databases on (see
// internal/db/migrate_integration_test.go).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

func integrationDB(t *testing.T) (*db.DB, *pgx.Conn) {
	t.Helper()
	admin := os.Getenv("EARMARK_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("EARMARK_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "earmark_it_" + hex.EncodeToString(b[:])
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	d, err := db.New(&config.Config{
		DatabaseURL: u.String(),
		ChunkSize:   512,
		AIEndpoints: []config.AIEndpoint{{ID: "e", Type: config.AIEndpointTypeEmbeddings, Model: "nomic-embed-text"}},
		AIRoles:     &config.AIRoles{Embeddings: "e"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return d, conn
}

const itT = "00000000-0000-0000-0000-0000000000e1"

func seedDecide(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	hash := patch.ChunkHash(runText)
	off := func(s string) int { return patch.Occurrences(runText, s)[0] }
	_, err := conn.Exec(context.Background(), `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('00000000-0000-0000-0000-00000000000a', '/b/PHM/01.m4b', 'c1', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds, segments, raw_text, model_name) VALUES
		  ('`+itT+`', '00000000-0000-0000-0000-00000000000a', '/b/PHM/01.m4b', 'c1', 'en', 6,
		   '[{"id":0,"start":0,"end":3,"text":"the dish at auto sebo picked up the signal","words":[]},
		     {"id":1,"start":3,"end":6,"text":"and the the cat sat","words":[]}]', 'x', 'parakeet');
		INSERT INTO transcript_chunks (id, transcript_id, file_path, chunk_index, start_sec, end_sec, text, source_text, embedding) VALUES
		  ('00000000-0000-0000-0000-0000000000c0', '`+itT+`', '/b/PHM/01.m4b', 0, 0, 6, '`+runText+`', '`+runText+`', array_fill(0.1, ARRAY[768])::vector);
		INSERT INTO book_metadata (book_dir, title, asin, chapters) VALUES
		  ('/b/PHM', 'Project Hail Mary', 'B08GB58KD5', '[{"Index":0,"Title":"The Arecibo Message","StartSec":0,"EndSec":6}]');`)
	if err != nil {
		t.Fatal(err)
	}
	type f struct {
		id, issue, orig, repl, state, origin, decidedBy string
		anchored                                        bool
	}
	for _, r := range []f{
		{"00000000-0000-0000-0000-0000000000f1", IssueMisheardProperNoun, "auto sebo", "Arecibo", "proposed", "judge", "", true},
		{"00000000-0000-0000-0000-0000000000f2", IssueMisheardWord, "dish", "telescope", "proposed", "judge", "", true},
		{"00000000-0000-0000-0000-0000000000f3", IssueMisheardWord, "signal", "signals", "proposed", "judge", "", false},
		{"00000000-0000-0000-0000-0000000000f4", IssueMisheardWord, "picked", "kicked", "proposed", "human", "", true},
		{"00000000-0000-0000-0000-0000000000f5", IssueRepeatedText, "the the cat", "the cat", "accepted", "judge", "mcp:alice", true},
		{"00000000-0000-0000-0000-0000000000f6", IssueMisheardWord, "sat", "sad", "rejected", "judge", "mcp:bob", true},
		{"00000000-0000-0000-0000-0000000000f7", IssueMisheardWord, "up", "op", "rejected", "judge", "cli:carol", true},
	} {
		var offset, h any
		if r.anchored {
			offset, h = off(r.orig), hash
		}
		var by, at any
		if r.decidedBy != "" {
			by, at = r.decidedBy, "2026-01-01T00:00:00Z"
		}
		if _, err := conn.Exec(context.Background(), `
			INSERT INTO transcript_findings (id, transcript_id, file_path, chunk_index, start_sec, end_sec, original_text,
			       issue_type, suggested_correction, confidence, model, patch_state, origin, chunk_text_sha256,
			       anchor_offset, anchor_occurrence, decided_by, decided_at)
			VALUES ($1, $2, '/b/PHM/01.m4b', 0, 0, 6, $3, $4, $5, 0.9, 'judge-model', $6, $7, $8, $9, 0, $10, $11)`,
			r.id, itT, r.orig, r.issue, r.repl, r.state, r.origin, h, offset, by, at); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
}

// snapshot fingerprints every table a dry run must not touch.
func snapshot(t *testing.T, conn *pgx.Conn) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, q := range map[string]string{
		"findings":       `SELECT COALESCE(md5(string_agg(f::text, '|' ORDER BY f.id)), '') FROM transcript_findings f`,
		"finding_events": `SELECT count(*)::text FROM finding_events`,
		"chunks":         `SELECT COALESCE(md5(string_agg(c.id::text || c.text || COALESCE(c.source_text,'') || c.embedding_stale::text, '|' ORDER BY c.id)), '') FROM transcript_chunks c`,
		"transcripts":    `SELECT COALESCE(md5(string_agg(t::text, '|' ORDER BY t.id)), '') FROM transcripts t`,
		"chunk_scan":     `SELECT count(*)::text FROM chunk_scan`,
	} {
		var v string
		if err := conn.QueryRow(context.Background(), q).Scan(&v); err != nil {
			t.Fatalf("snapshot %s: %v", name, err)
		}
		out[name] = v
	}
	return out
}

func TestIntegrationDecideDryRun(t *testing.T) {
	d, conn := integrationDB(t)
	ctx := context.Background()
	seedDecide(t, conn)

	scope := db.DecideScope{Sample: 10, Seed: "q4"}
	first, err := d.DecideSample(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range first {
		ids = append(ids, f.ID[len(f.ID)-2:])
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"f1", "f2"}) {
		t.Fatalf("scope selected %v, want the anchored proposed judge findings f1 f2", ids)
	}
	again, err := d.DecideSample(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if first[i].ID != again[i].ID {
			t.Fatal("same seed, different order")
		}
	}
	if one, err := d.DecideSample(ctx, db.DecideScope{Sample: 1, Seed: "q4"}); err != nil || len(one) != 1 || one[0].ID != first[0].ID {
		t.Errorf("sample of 1 = %v, %v; want the seed order's first", one, err)
	}
	backlog, err := d.DecideBacklog(ctx, scope)
	if err != nil || backlog[IssueMisheardProperNoun] != 1 || backlog[IssueMisheardWord] != 1 || len(backlog) != 2 {
		t.Errorf("backlog %v, %v", backlog, err)
	}

	before := snapshot(t, conn)
	asker := &fakeAsker{p: func(string) (float64, error) { return 0.97, nil }}
	rep, err := DryRun(ctx, d, asker, RunOptions{Scope: scope, Concurrency: 2, Params: DefaultShouldApplyParams()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcomes[DecisionApply] != 1 || rep.Rung0Rejects[ReasonNotSoundAlike] != 1 || asker.calls != 1 {
		t.Errorf("report outcomes %v rung0 %v calls %d", rep.Outcomes, rep.Rung0Rejects, asker.calls)
	}

	cal, err := DryRun(ctx, d, asker, RunOptions{Scope: db.DecideScope{Sample: 10, Seed: "q4", Calibrate: true},
		Concurrency: 2, Params: DefaultShouldApplyParams()})
	if err != nil {
		t.Fatal(err)
	}
	if c := cal.Calibration; c == nil || c.HumanAccepted != 1 || c.HumanRejected != 1 {
		t.Errorf("calibration %+v (only mcp: decisions count as human)", c)
	}

	after := snapshot(t, conn)
	for k, v := range before {
		if after[k] != v {
			t.Errorf("dry run changed %s", k)
		}
	}
	var calls, decideRecipes int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM fn_calls WHERE fn = 'should_apply'`).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM recipes WHERE step = 'decide'`).Scan(&decideRecipes); err != nil {
		t.Fatal(err)
	}
	if calls < 2 || decideRecipes != 1 {
		t.Errorf("fn_calls %d, decide recipes %d", calls, decideRecipes)
	}
}
