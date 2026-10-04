package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/log"
)

// errChat fails every Complete call with err — a judge endpoint that is down,
// or (with a DeadlineExceeded-wrapping err) one whose 120s per-request client
// timeout fires on every chunk.
type errChat struct{ err error }

func (c errChat) Complete(context.Context, string, string) (string, error) { return "", c.err }
func (c errChat) Model() string                                            { return "err-judge" }

// errClientTimeout mimics what net/http returns when http.Client.Timeout fires:
// it satisfies errors.Is(err, context.DeadlineExceeded) even though the CALLER's
// context is still live. Treating it as "the caller cancelled" is what used to
// abort the whole transcript on one slow chunk.
var errClientTimeout = fmt.Errorf("chat request: Post \"http://judge/v1/chat/completions\": %w (Client.Timeout exceeded while awaiting headers)",
	context.DeadlineExceeded)

// TestEvalTranscript_JudgeErrorDoesNotLatch is the latch-only-on-success
// regression (Phase 0a item 2). When the judge fails, eval_finished_at must NOT
// be written: a latched transcript is never re-judged, so a failure latched as
// "done" is lost for good. Before the fix the gated eval pass wrote the latch
// with ChunksEvaluated=len(chunks) whatever the judge did.
func TestEvalTranscript_JudgeErrorDoesNotLatch(t *testing.T) {
	for name, chat := range map[string]eval.ChatClient{
		"endpoint down":        errChat{err: errors.New("connection refused")},
		"per-request timeouts": errChat{err: errClientTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			fdb := &fakeDB{}
			w := &Worker{
				ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"),
				judge: eval.NewJudge(chat), evalGatesEmbed: true,
			}
			cfg := &config.Config{ChunkSize: 8, EvalGatesEmbed: true}
			tr := &db.Transcript{
				ID: "tid-judgefail", JobID: "job-judgefail", FilePath: "/b/a/t/ch.mp3",
				RawText: "Hello world this is a fairly long test transcript with plenty of words to chunk.",
			}

			require.NoError(t, w.evalTranscript(cfg, tr))
			require.Len(t, fdb.evalMetrics, 1, "exactly one outcome record (the failure) per judge run")
			for _, m := range fdb.evalMetrics {
				require.True(t, m.FinishedAt.IsZero(),
					"judge failed on every chunk → eval_finished_at must NOT be latched (got %v, chunks=%d skipped=%d)",
					m.FinishedAt, m.Chunks, m.Skipped)
			}
		})
	}
}

const longTranscript = "Hello world this is a fairly long test transcript with plenty of words so the token chunker emits multiple chunks for the judge to evaluate one by one."

const oneFindingJSON = `{"findings":[{"original_text":"hello world","issue_type":"misheard_word","suggested_correction":"hello word","confidence":0.9}]}`

// A partial judge run (one chunk fails) persists the surviving findings, records
// a failure instead of the latch, and logs an eval/error event.
func TestEvalTranscript_PartialFailureRecordsFailure(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{
		ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"),
		judge: eval.NewJudge(&flakyChat{resp: oneFindingJSON}), evalGatesEmbed: true,
	}
	tr := &db.Transcript{ID: "tid-partial", JobID: "job-partial", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}

	require.NoError(t, w.evalTranscript(&config.Config{ChunkSize: 8, EvalGatesEmbed: true}, tr))

	require.Len(t, fdb.evalMetrics, 1, "exactly one outcome record per judge run")
	m := fdb.evalMetrics[0]
	require.True(t, m.Failed(), "a partial run must not latch")
	require.True(t, m.FinishedAt.IsZero())
	require.False(t, m.FailedAt.IsZero(), "failure must be timestamped (eval_failed_at)")
	require.Equal(t, 1, m.FailedChunks)
	require.Equal(t, "job-partial", m.JobID)
	require.Contains(t, m.Error, "1 of")
	require.Contains(t, m.Error, "transient judge glitch")
	require.NotEmpty(t, fdb.findings, "surviving chunks' findings are still persisted")

	var errEvents int
	for _, e := range fdb.events {
		if e.Stage == db.StageEval && e.Event == db.EventError {
			errEvents++
			require.Equal(t, 1, e.Detail["skipped"])
		}
	}
	require.Equal(t, 1, errEvents, "a partial run logs one eval/error event")
}

// A complete run latches and clears nothing else: one success record.
func TestEvalTranscript_CompleteRunLatches(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{
		ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"),
		judge: eval.NewJudge(workerFakeChat{resp: oneFindingJSON}), evalGatesEmbed: true,
	}
	tr := &db.Transcript{ID: "tid-ok", JobID: "job-ok", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}
	require.NoError(t, w.evalTranscript(&config.Config{ChunkSize: 8, EvalGatesEmbed: true}, tr))
	require.Len(t, fdb.evalMetrics, 1)
	require.False(t, fdb.evalMetrics[0].Failed())
	require.False(t, fdb.evalMetrics[0].FinishedAt.IsZero(), "complete run → latch")
	require.Zero(t, fdb.evalMetrics[0].Skipped)
}

// Findings that could not be stored make the run a failure: latching would mark
// the transcript judged while its findings are gone.
func TestEvalTranscript_PersistFailureDoesNotLatch(t *testing.T) {
	fdb := &fakeDB{findingsErr: errors.New("findings table down")}
	w := &Worker{
		ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"),
		judge: eval.NewJudge(workerFakeChat{resp: oneFindingJSON}), evalGatesEmbed: true,
	}
	tr := &db.Transcript{ID: "tid-pf", JobID: "job-pf", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}
	require.NoError(t, w.evalTranscript(&config.Config{ChunkSize: 8, EvalGatesEmbed: true}, tr))
	require.Len(t, fdb.evalMetrics, 1)
	m := fdb.evalMetrics[0]
	require.True(t, m.Failed())
	require.Contains(t, m.Error, "persist findings")
	require.Positive(t, m.FailedChunks)
}

// cancelChat cancels the worker's context on the first call, like a shutdown
// arriving mid-judge.
type cancelChat struct{ cancel context.CancelFunc }

func (c cancelChat) Complete(context.Context, string, string) (string, error) {
	c.cancel()
	return "", context.Canceled
}
func (cancelChat) Model() string { return "cancel-judge" }

// A shutdown mid-judge records NOTHING: not the latch, not a failure. The job
// stays in the eval pass's selection and is simply judged after restart.
func TestEvalTranscript_ShutdownRecordsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fdb := &fakeDB{}
	w := &Worker{
		ctx: ctx, db: fdb, log: log.NewLogger("worker-test"),
		judge: eval.NewJudge(cancelChat{cancel: cancel}), evalGatesEmbed: true,
	}
	tr := &db.Transcript{ID: "tid-stop", JobID: "job-stop", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}
	require.ErrorIs(t, w.evalTranscript(&config.Config{ChunkSize: 8, EvalGatesEmbed: true}, tr), context.Canceled)
	require.Empty(t, fdb.evalMetrics, "shutdown must not latch or record a failure")
	require.Empty(t, fdb.findings)
}

// Ungated inline eval (EVAL_IN_PIPELINE=true, EVAL_GATES_EMBED=false): a judge
// failure never blocks the embed, and the transcript is left unlatched with a
// failure record so `earmark eval --backfill-unevaluated` re-judges it.
func TestProcessTranscript_JudgeFailureEmbedsButDoesNotLatch(t *testing.T) {
	fdb := &fakeDB{}
	w := &Worker{
		ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"),
		judge: eval.NewJudge(errChat{err: errors.New("judge down")}),
	}
	tr := &db.Transcript{ID: "tid-ug", JobID: "job-ug", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}
	require.NoError(t, w.processTranscript(&config.Config{ChunkSize: 8}, tr))
	require.NotEmpty(t, fdb.chunks, "judge failure must not block the embed")
	require.Len(t, fdb.evalMetrics, 1)
	require.True(t, fdb.evalMetrics[0].Failed(), "judge failure must not latch")
	require.Equal(t, len(fdb.chunks), fdb.evalMetrics[0].FailedChunks)
}

// If the judge outcome (latch or failure record) cannot be written, the job is
// still unattempted in the DB and will be re-selected. evalTranscript must say
// so, and the gated loop must back off instead of draining a "full" batch of
// the same job in a hot loop.
func TestEvalTranscript_OutcomeWriteFailureIsReported(t *testing.T) {
	for name, chat := range map[string]eval.ChatClient{
		"latch write fails":   workerFakeChat{resp: `{"findings":[]}`},
		"failure write fails": errChat{err: errors.New("judge down")},
	} {
		t.Run(name, func(t *testing.T) {
			fdb := &fakeDB{evalMetErr: errors.New("run_metrics down")}
			w := &Worker{
				ctx: context.Background(), db: fdb, log: log.NewLogger("worker-test"),
				judge: eval.NewJudge(chat), evalGatesEmbed: true,
			}
			tr := &db.Transcript{ID: "tid-ow", JobID: "job-ow", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}
			err := w.evalTranscript(&config.Config{ChunkSize: 8, EvalGatesEmbed: true}, tr)
			require.ErrorIs(t, err, errEvalOutcomeNotRecorded)
		})
	}
}

func TestStart_GatedBacksOffWhenOutcomeNotRecorded(t *testing.T) {
	fdb := &fakeDB{
		evalMetErr: errors.New("run_metrics down"),
		// Not scripted: every eval-pass call returns this one transcript — a FULL
		// batch at batch size 1, which would normally mean "drain immediately".
		transcripts: []*db.Transcript{{ID: "tid-loop", JobID: "job-loop", FilePath: "/b/x.mp3", RawText: longTranscript}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{
		done: make(chan struct{}), ctx: ctx, cancel: cancel, db: fdb, log: log.NewLogger("worker-test"),
		judge: eval.NewJudge(workerFakeChat{resp: `{"findings":[]}`}), evalGatesEmbed: true,
	}
	go w.Start(&config.Config{ChunkSize: 8, EmbedBatchSize: 1, EvalGatesEmbed: true})
	time.Sleep(200 * time.Millisecond)
	w.Stop()
	require.LessOrEqual(t, fdb.evalCallCount(), 2,
		"an unrecorded eval outcome must back off for the poll interval, not re-judge in a hot loop")
}
