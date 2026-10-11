package eval

// Postgres proofs of `earmark eval --rejudge` (CONTRACT §2.15 "Re-judge"):
// a killed run resumes from fn_calls without paying again and without
// inserting a finding twice, and an invalid reply fails its chunk. Skipped
// unless EARMARK_TEST_DATABASE_URL points at a server the test may create and
// drop databases on (see internal/db/migrate_integration_test.go).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
)

func rejudgeIntegrationDB(t *testing.T) (*db.DB, *pgx.Conn) {
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
		DatabaseURL: u.String(), ChunkSize: 512,
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

// seedRejudge inserts n done transcripts of k chunks; chunk j of transcript i
// reads "she wrote under a pin name i j".
func seedRejudge(t *testing.T, conn *pgx.Conn, n, k int) []string {
	t.Helper()
	ctx := context.Background()
	var ids []string
	for i := range n {
		path := fmt.Sprintf("/books/Author/Book %d/01.m4b", i)
		var job, tid string
		if err := conn.QueryRow(ctx, `INSERT INTO transcription_jobs (file_path, checksum, status)
			VALUES ($1, $2, 'done') RETURNING id`, path, fmt.Sprintf("cs%d", i)).Scan(&job); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx, `INSERT INTO transcripts (job_id, file_path, checksum, language,
			duration_seconds, segments, raw_text, model_name, created_at)
			VALUES ($1, $2, $3, 'en', 60, '[]', 'x', 'm', now() + $4 * interval '1 second') RETURNING id`,
			job, path, fmt.Sprintf("cs%d", i), i).Scan(&tid); err != nil {
			t.Fatal(err)
		}
		for j := range k {
			if _, err := conn.Exec(ctx, `INSERT INTO transcript_chunks (transcript_id, file_path, chunk_index,
				start_sec, end_sec, text, embedding)
				VALUES ($1, $2, $3, $3 * 10, $3 * 10 + 10, $4, array_fill(0.1, ARRAY[768])::vector)`,
				tid, path, j, fmt.Sprintf("she wrote under a pin name %d %d", i, j)); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, tid)
	}
	return ids
}

const pinPenReply = `{"findings":[{"original_text":"pin name","issue_type":"homophone","suggested_correction":"pen name","confidence":0.9,"anchor_offset":-1,"anchor_occurrence":-1}]}`

// killChat answers every chunk with one finding; after `stopAfter` calls it
// fires `stop` (the run's cancel) once the reply is returned. A reply for a
// chunk containing `bad` is not JSON.
type killChat struct {
	calls     atomic.Int32
	stopAfter int32
	stop      func()
	bad       string
}

func (c *killChat) Complete(_ context.Context, _, user string) (string, error) {
	n := c.calls.Add(1)
	if c.stop != nil && n == c.stopAfter {
		defer c.stop()
	}
	if c.bad != "" && strings.Contains(user, c.bad) {
		return "not JSON at all", nil
	}
	return pinPenReply, nil
}
func (c *killChat) Model() string { return "it-judge" }

func newITJudge(d *db.DB, chat evalpkg.ChatClient) *evalpkg.Judge {
	j := evalpkg.NewJudge(chat)
	j.SetCallLog(d)
	return j
}

// crashOnCommit is the database of a process that dies before its pass
// commits: every model call is logged (fn_calls), nothing else lands.
type crashOnCommit struct{ *db.DB }

func (crashOnCommit) CommitJudgePass(context.Context, db.JudgePassCommit) (db.JudgePassResult, error) {
	return db.JudgePassResult{}, errors.New("killed")
}

func countQ(t *testing.T, conn *pgx.Conn, q string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// TestIntegrationRejudgeResumeAfterKill: a run killed after two paid calls
// (its pass never committed) leaves the pass 'running' and both replies in
// fn_calls. The resumed run pays only for the chunks never answered, serves
// the two from cache, completes every pass and inserts each finding once; a
// third run selects nothing.
func TestIntegrationRejudgeResumeAfterKill(t *testing.T) {
	d, conn := rejudgeIntegrationDB(t)
	ids := seedRejudge(t, conn, 2, 3)
	var out strings.Builder

	ctx, cancel := context.WithCancel(context.Background())
	chat1 := &killChat{stopAfter: 2, stop: cancel}
	err := runRejudge(ctx, &out, crashOnCommit{d}, newITJudge(d, chat1), rejudgeOptions{write: true, concurrency: 1})
	if err == nil {
		t.Fatal("killed run reported success")
	}
	if got := countQ(t, conn, `SELECT count(*) FROM judge_passes WHERE status = 'running'`); got != 1 {
		t.Fatalf("%d running passes after the kill, want 1 (the evidence)", got)
	}
	if got := countQ(t, conn, `SELECT count(*) FROM fn_calls WHERE fn = 'judge_chunk' AND error_class IS NULL AND NOT cache_hit`); got != 2 {
		t.Fatalf("%d paid judge_chunk rows, want 2", got)
	}
	if got := countQ(t, conn, `SELECT count(*) FROM transcript_findings`); got != 0 {
		t.Fatalf("%d findings landed from a run that died before committing", got)
	}

	chat2 := &killChat{}
	out.Reset()
	if err := runRejudge(context.Background(), &out, d, newITJudge(d, chat2), rejudgeOptions{write: true, concurrency: 3}); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	if got := chat2.calls.Load(); got != 4 {
		t.Errorf("resume paid for %d chunks, want 4 (6 minus the 2 answered before the kill)", got)
	}
	if got := countQ(t, conn, `SELECT coalesce(sum(chunks_cached), 0) FROM judge_passes WHERE status = 'complete'`); got != 2 {
		t.Errorf("%d chunks served from cache, want 2", got)
	}
	if got := countQ(t, conn, `SELECT count(*) FROM fn_calls WHERE cache_hit`); got != 2 {
		t.Errorf("%d cache-hit rows, want 2", got)
	}
	for _, id := range ids {
		if got := countQ(t, conn, `SELECT count(*) FROM judge_passes WHERE transcript_id = $1 AND status = 'complete'`, id); got != 1 {
			t.Errorf("transcript %s has %d complete passes", id, got)
		}
	}
	if got := countQ(t, conn, `SELECT count(*) FROM transcript_findings WHERE judge_pass_id IS NOT NULL AND fn_call_id IS NOT NULL`); got != 6 {
		t.Errorf("%d findings with lineage, want 6 (one per chunk)", got)
	}
	if got := countQ(t, conn, `SELECT count(*) - count(DISTINCT (chunk_id, original_text, suggested_correction)) FROM transcript_findings`); got != 0 {
		t.Errorf("%d duplicate findings", got)
	}
	if got := countQ(t, conn, `SELECT count(*) FROM run_metrics WHERE eval_finished_at IS NOT NULL`); got != 2 {
		t.Errorf("%d latched jobs, want 2", got)
	}
	if got := countQ(t, conn, `SELECT count(*) FROM step_runs WHERE step = 'eval_rejudge'`); got != 2 {
		t.Errorf("%d eval_rejudge step runs, want 2", got)
	}

	chat3 := &killChat{}
	out.Reset()
	if err := runRejudge(context.Background(), &out, d, newITJudge(d, chat3), rejudgeOptions{write: true, concurrency: 3}); err != nil {
		t.Fatal(err)
	}
	if chat3.calls.Load() != 0 || !strings.Contains(out.String(), "No transcript needs re-judging") {
		t.Errorf("a finished re-judge selected work again (%d calls):\n%s", chat3.calls.Load(), out.String())
	}
}

// TestIntegrationRejudgeStopAndResumeNoDoubleInsert: a run stopped mid-way
// (SIGINT) commits its in-flight pass as partial with the findings judged so
// far; the resumed run re-serves those chunks from cache and records them as
// already_recorded instead of inserting them again.
func TestIntegrationRejudgeStopAndResumeNoDoubleInsert(t *testing.T) {
	d, conn := rejudgeIntegrationDB(t)
	seedRejudge(t, conn, 1, 3)
	var out strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	if err := runRejudge(ctx, &out, d, newITJudge(d, &killChat{stopAfter: 2, stop: cancel}),
		rejudgeOptions{write: true, concurrency: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want a cancelled run, got %v", err)
	}
	if got := countQ(t, conn, `SELECT count(*) FROM judge_passes WHERE status = 'partial' AND findings_kept = 2 AND chunks_failed = 1`); got != 1 {
		t.Fatalf("want one partial pass with 2 findings:\n%s", out.String())
	}
	if got := countQ(t, conn, `SELECT count(*) FROM run_metrics WHERE eval_failed_at IS NOT NULL OR eval_finished_at IS NOT NULL`); got != 0 {
		t.Errorf("a stop wrote the run_metrics latch or a failure record")
	}

	chat := &killChat{}
	out.Reset()
	if err := runRejudge(context.Background(), &out, d, newITJudge(d, chat), rejudgeOptions{write: true, concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if chat.calls.Load() != 1 {
		t.Errorf("resume paid for %d chunks, want 1", chat.calls.Load())
	}
	if got := countQ(t, conn, `SELECT count(*) FROM transcript_findings`); got != 3 {
		t.Errorf("%d findings, want 3 (no double insert)", got)
	}
	if got := countQ(t, conn, `SELECT count(*) FROM judge_passes WHERE status = 'complete' AND findings_kept = 1
		AND (dropped->>'already_recorded')::int = 2 AND chunks_cached = 2`); got != 1 {
		t.Errorf("resumed pass not recorded as 1 kept + 2 already_recorded from cache:\n%s", out.String())
	}
}

// TestIntegrationRejudgeInvalidReply: a chunk whose reply is not JSON fails:
// the pass is partial (never complete), the reply is logged as invalid_reply
// (never served), the job gets a failure record, and the next run selects the
// transcript again and asks for that chunk again.
func TestIntegrationRejudgeInvalidReply(t *testing.T) {
	d, conn := rejudgeIntegrationDB(t)
	seedRejudge(t, conn, 1, 3)
	var out strings.Builder
	err := runRejudge(context.Background(), &out, d, newITJudge(d, &killChat{bad: "pin name 0 1"}),
		rejudgeOptions{write: true, concurrency: 2})
	if err == nil {
		t.Fatal("a run that completed nothing must fail")
	}
	if got := countQ(t, conn, `SELECT count(*) FROM judge_passes WHERE status = 'partial' AND chunks_failed = 1 AND chunks_judged = 2`); got != 1 {
		t.Fatalf("want one partial pass with one failed chunk:\n%s", out.String())
	}
	if got := countQ(t, conn, `SELECT count(*) FROM fn_calls WHERE error_class = 'invalid_reply' AND output IS NOT NULL`); got != 1 {
		t.Errorf("%d invalid_reply rows, want 1 (with the reply kept)", got)
	}
	if got := countQ(t, conn, `SELECT count(*) FROM run_metrics WHERE eval_failed_at IS NOT NULL AND eval_finished_at IS NULL`); got != 1 {
		t.Errorf("the job was not recorded as failed")
	}
	chat := &killChat{}
	out.Reset()
	if err := runRejudge(context.Background(), &out, d, newITJudge(d, chat), rejudgeOptions{write: true, concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if chat.calls.Load() != 1 {
		t.Errorf("re-run paid for %d chunks, want 1 (only the invalid one)", chat.calls.Load())
	}
	if got := countQ(t, conn, `SELECT count(*) FROM judge_passes WHERE status = 'complete'`); got != 1 {
		t.Errorf("re-run did not complete the transcript")
	}
}

// TestIntegrationRejudgeExpectRecipe: --expect-recipe refuses a judge whose
// recipe differs, before any call or pass.
func TestIntegrationRejudgeExpectRecipe(t *testing.T) {
	d, conn := rejudgeIntegrationDB(t)
	seedRejudge(t, conn, 1, 1)
	other, err := d.RegisterRecipe(context.Background(), newITJudge(d, &killChat{}).Recipe())
	if err != nil {
		t.Fatal(err)
	}
	chat := &modelChat{killChat: &killChat{}, model: "another-judge"}
	var out strings.Builder
	if err := runRejudge(context.Background(), &out, d, newITJudge(d, chat),
		rejudgeOptions{write: true, concurrency: 1, expectRecipe: other}); !errors.Is(err, ErrRecipeMismatch) {
		t.Fatalf("want ErrRecipeMismatch, got %v", err)
	}
	if chat.calls.Load() != 0 || countQ(t, conn, `SELECT count(*) FROM judge_passes`) != 0 {
		t.Error("a refused run called the model or opened a pass")
	}
}

type modelChat struct {
	*killChat
	model string
}

func (m *modelChat) Model() string { return m.model }

// TestIntegrationBackfillRecordsJudgePasses: --backfill-unevaluated --write
// records each transcript as a judge pass, its findings with judge_pass_id
// and fn_call_id, and every judge call in fn_calls; a re-run judges from
// cache and inserts nothing twice.
func TestIntegrationBackfillRecordsJudgePasses(t *testing.T) {
	d, conn := rejudgeIntegrationDB(t)
	seedRejudge(t, conn, 2, 2)
	var out strings.Builder
	cfg := &config.Config{ChunkSize: 512}
	chat := &killChat{}
	if err := runBackfill(context.Background(), &out, d, newITJudge(d, chat), cfg,
		backfillOptions{mode: backfillUnevaluated, write: true}); err != nil {
		t.Fatalf("backfill: %v\n%s", err, out.String())
	}
	if got := countQ(t, conn, `SELECT count(*) FROM judge_passes WHERE status = 'complete' AND findings_kept = 2 AND step_run_id IS NOT NULL`); got != 2 {
		t.Errorf("%d complete passes with 2 findings, want 2:\n%s", got, out.String())
	}
	if got := countQ(t, conn, `SELECT count(*) FROM transcript_findings WHERE judge_pass_id IS NOT NULL AND fn_call_id IS NOT NULL`); got != 4 {
		t.Errorf("%d findings with lineage, want 4", got)
	}
	if got := countQ(t, conn, `SELECT count(*) FROM fn_calls WHERE fn = 'judge_chunk' AND NOT cache_hit`); got != 4 {
		t.Errorf("%d judge_chunk calls logged, want 4", got)
	}

	// Re-judge the same transcripts (as --backfill-eval-errors would): every
	// chunk comes from cache and nothing is inserted twice.
	if _, err := conn.Exec(context.Background(), `UPDATE run_metrics SET eval_finished_at = NULL`); err != nil {
		t.Fatal(err)
	}
	chat2 := &killChat{}
	out.Reset()
	if err := runBackfill(context.Background(), &out, d, newITJudge(d, chat2), cfg,
		backfillOptions{mode: backfillUnevaluated, write: true}); err != nil {
		t.Fatal(err)
	}
	if chat2.calls.Load() != 0 {
		t.Errorf("re-run paid for %d chunks, want 0 (cached)", chat2.calls.Load())
	}
	if got := countQ(t, conn, `SELECT count(*) FROM transcript_findings`); got != 4 {
		t.Errorf("%d findings after the re-run, want 4", got)
	}
}
