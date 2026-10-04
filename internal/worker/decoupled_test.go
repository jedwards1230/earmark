package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/queue"
)

// judgeServer is an OpenAI-compatible chat endpoint that counts calls, so a
// test can prove whether the worker contacted the judge at all.
func judgeServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"judge","choices":[{"message":{"role":"assistant","content":"{\"findings\":[]}"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// EVAL_IN_PIPELINE=false is the decoupled mode (Phase 0a item 1): even with a
// judge endpoint fully configured, the worker builds no judge and the embed path
// makes NO judge call — embedding never waits on the judge. The control case
// (EVAL_IN_PIPELINE=true, same endpoint) proves the probe would see a call.
func TestEvalInPipelineFalse_EmbedNeverCallsJudge(t *testing.T) {
	for _, tc := range []struct {
		inPipeline bool
		wantCalls  bool
	}{
		{inPipeline: false, wantCalls: false},
		{inPipeline: true, wantCalls: true},
	} {
		t.Run(fmt.Sprintf("EVAL_IN_PIPELINE=%v", tc.inPipeline), func(t *testing.T) {
			srv, calls := judgeServer(t)
			t.Setenv("EVAL_CHAT_BASE_URL", srv.URL+"/v1")
			t.Setenv("EVAL_CHAT_MODEL", "qwen3:8b")

			fdb := &fakeDB{}
			w := NewWorker(&queue.Queue{}, fdb, &config.Config{ChunkSize: 8, EvalInPipeline: tc.inPipeline})
			tr := &db.Transcript{ID: "tid-dec", JobID: "job-dec", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}
			require.NoError(t, w.processTranscript(&config.Config{ChunkSize: 8}, tr))
			require.NotEmpty(t, fdb.chunks, "embedding happens in both modes")

			if !tc.wantCalls {
				require.Nil(t, w.judge, "EVAL_IN_PIPELINE=false must not build a judge")
				require.Zero(t, calls.Load(), "EVAL_IN_PIPELINE=false: the embed path must make no judge call")
				require.Empty(t, fdb.evalMetrics, "nothing latched — the transcript stays for the backfill pass")
				for _, e := range fdb.events {
					require.NotEqual(t, db.StageEval, e.Stage, "no eval events in decoupled mode")
				}
				return
			}
			require.Positive(t, calls.Load(), "control: the inline judge does call the endpoint")
			require.Len(t, fdb.evalMetrics, 1)
		})
	}
}

// The ungated selection is walked in bounded keyset pages within ONE cycle: the
// whole backlog is embedded, but no single query asks for more than
// EMBED_BATCH_SIZE rows.
func TestDrainCompleted_PagesWholeBacklog(t *testing.T) {
	fdb := &fakeDB{}
	for i := range 5 {
		fdb.transcripts = append(fdb.transcripts, &db.Transcript{
			ID: fmt.Sprintf("t%d", i), JobID: fmt.Sprintf("j%d", i),
			FilePath: fmt.Sprintf("/b/a/t/%d.mp3", i), RawText: "Hello world test transcript.",
		})
	}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}

	embedded, ok := w.drainCompleted(&config.Config{ChunkSize: 10, EmbedBatchSize: 2})
	require.True(t, ok)
	require.Equal(t, 5, embedded, "every page is processed within the cycle")
	require.Equal(t, []int{2, 2, 2}, fdb.completedLimits, "each query is bounded by EMBED_BATCH_SIZE")
	require.Equal(t, []string{"", "t1", "t3"},
		[]string{fdb.completedCursors[0].ID, fdb.completedCursors[1].ID, fdb.completedCursors[2].ID},
		"the cursor advances past each page")
}

// Transcripts that keep failing stay in the selection; keyset paging moves past
// them so they can never starve the rest of the backlog (a plain "first N"
// LIMIT would select the same failing head forever).
func TestDrainCompleted_FailingHeadDoesNotStarve(t *testing.T) {
	fdb := &fakeDB{transcripts: []*db.Transcript{
		{ID: "bad0", FilePath: "/b/0.mp3", RawText: ""}, // empty → processTranscript errors
		{ID: "bad1", FilePath: "/b/1.mp3", RawText: ""},
		{ID: "ok2", FilePath: "/b/2.mp3", RawText: "Hello world test transcript."},
		{ID: "ok3", FilePath: "/b/3.mp3", RawText: "Hello world test transcript."},
	}}
	w := &Worker{ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test")}
	embedded, ok := w.drainCompleted(&config.Config{ChunkSize: 10, EmbedBatchSize: 2})
	require.True(t, ok)
	require.Equal(t, 2, embedded, "the healthy transcripts behind a failing page are still embedded")
}

// A transcript that always fails must not be retried in a hot loop: the cycle
// embedded nothing, so the worker sleeps the poll interval.
func TestStart_UngatedFailingTranscriptDoesNotBusyLoop(t *testing.T) {
	fdb := &fakeDB{transcripts: []*db.Transcript{{ID: "bad", FilePath: "/b/x.mp3", RawText: ""}}}
	w := startWorkerWith(fdb)
	time.Sleep(200 * time.Millisecond)
	w.Stop()
	require.LessOrEqual(t, fdb.completedCalls(), 2,
		"a permanently failing transcript must wait out the poll interval, not spin")
}
