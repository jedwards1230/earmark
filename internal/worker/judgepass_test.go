package worker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/log"
)

// passDB is a fakeDB that records judge passes (as db.DB does), so the
// in-pipeline judge writes its runs through them (CONTRACT §2.15 "Lineage").
type passDB struct {
	*fakeDB
	pmu       sync.Mutex
	opened    []db.JudgePassOpen
	commits   []db.JudgePassCommit
	commitErr error // returned by the first commit only
}

func (p *passDB) OpenJudgePass(_ context.Context, o db.JudgePassOpen) (int64, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.opened = append(p.opened, o)
	return int64(len(p.opened)), nil
}

func (p *passDB) CommitJudgePass(_ context.Context, c db.JudgePassCommit) (db.JudgePassResult, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.commits = append(p.commits, c)
	if err := p.commitErr; err != nil {
		p.commitErr = nil
		return db.JudgePassResult{}, err
	}
	return db.JudgePassResult{Inserted: len(c.Findings)}, nil
}

var _ eval.PassStore = (*passDB)(nil)

func passWorker(ctx context.Context, pdb *passDB, chat eval.ChatClient) *Worker {
	return &Worker{ctx: ctx, db: pdb, log: log.NewLogger("worker-test"), judge: eval.NewJudge(chat), evalGatesEmbed: true}
}

// A complete in-pipeline run opens its pass before judging and commits the
// findings, the closing record and the latch together — nothing through the
// plain InsertFindings / UpsertEvalMetrics writes.
func TestEvalTranscript_RecordsJudgePass(t *testing.T) {
	pdb := &passDB{fakeDB: &fakeDB{}}
	w := passWorker(context.Background(), pdb, workerFakeChat{resp: oneFindingJSON})
	tr := &db.Transcript{ID: "tid-pass", JobID: "job-pass", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}
	require.NoError(t, w.evalTranscript(&config.Config{ChunkSize: 8, EvalGatesEmbed: true}, tr))

	require.Len(t, pdb.opened, 1)
	require.Equal(t, "tid-pass", pdb.opened[0].TranscriptID)
	require.Positive(t, pdb.opened[0].ChunksTotal)
	require.Len(t, pdb.commits, 1)
	c := pdb.commits[0]
	require.Equal(t, db.JudgePassComplete, c.Close.Status)
	require.NotEmpty(t, c.Findings)
	require.NotNil(t, c.Metrics)
	require.False(t, c.Metrics.Failed(), "complete pass carries the latch")
	require.Empty(t, pdb.findings, "findings go through the pass commit")
	require.Empty(t, pdb.evalMetrics, "the latch goes through the pass commit")
}

// A shutdown mid-judge on the gated eval pass stores no findings and no
// latch, but closes the opened pass as failed instead of leaving it running.
func TestEvalTranscript_ShutdownClosesPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pdb := &passDB{fakeDB: &fakeDB{}}
	w := passWorker(ctx, pdb, cancelChat{cancel: cancel})
	tr := &db.Transcript{ID: "tid-stop", JobID: "job-stop", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}
	require.ErrorIs(t, w.evalTranscript(&config.Config{ChunkSize: 8, EvalGatesEmbed: true}, tr), context.Canceled)
	require.Len(t, pdb.opened, 1)
	require.Len(t, pdb.commits, 1, "the opened pass is closed")
	require.Equal(t, db.JudgePassFailed, pdb.commits[0].Close.Status)
	require.Empty(t, pdb.commits[0].Findings)
	require.Nil(t, pdb.commits[0].Metrics)
	require.Empty(t, pdb.evalMetrics)
}

// A pass commit that fails falls back to the plain writes (findings, then
// the outcome record) and closes the pass as failed.
func TestEvalTranscript_PassCommitFailureFallsBack(t *testing.T) {
	pdb := &passDB{fakeDB: &fakeDB{}, commitErr: errors.New("tx aborted")}
	w := passWorker(context.Background(), pdb, workerFakeChat{resp: oneFindingJSON})
	tr := &db.Transcript{ID: "tid-fb", JobID: "job-fb", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}
	require.NoError(t, w.evalTranscript(&config.Config{ChunkSize: 8, EvalGatesEmbed: true}, tr))
	require.Len(t, pdb.commits, 2, "the failed commit, then the failed-status close")
	require.Equal(t, db.JudgePassFailed, pdb.commits[1].Close.Status)
	require.NotEmpty(t, pdb.findings, "fallback stores the findings")
	require.Len(t, pdb.evalMetrics, 1, "fallback records the outcome")
}

// stopSecondChat answers the first chunk, then shuts the worker down on the
// second call — a shutdown arriving mid-judge after some work was done.
type stopSecondChat struct {
	cancel context.CancelFunc
	calls  *int
}

func (c stopSecondChat) Complete(_ context.Context, _, user string) (string, error) {
	*c.calls++
	if *c.calls == 1 {
		return echoFinding(oneFindingJSON, user), nil
	}
	c.cancel()
	return "", context.Canceled
}
func (stopSecondChat) Model() string { return "stop-judge" }

// A shutdown on the gated eval pass closes the pass through closeStoppedPass:
// partial, with the findings of the chunk judged before the stop, and no
// run_metrics write (the job stays unlatched and unfailed).
func TestEvalTranscript_StopClosesPassWithJudgedFindings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pdb := &passDB{fakeDB: &fakeDB{}}
	calls := 0
	w := passWorker(ctx, pdb, stopSecondChat{cancel: cancel, calls: &calls})
	tr := &db.Transcript{ID: "tid-stop2", JobID: "job-stop2", FilePath: "/b/a/t/ch.mp3", RawText: longTranscript}
	require.ErrorIs(t, w.evalTranscript(&config.Config{ChunkSize: 8, EvalGatesEmbed: true}, tr), context.Canceled)
	require.Len(t, pdb.opened, 1)
	require.Len(t, pdb.commits, 1, "the stopped pass is closed exactly once")
	c := pdb.commits[0]
	require.Equal(t, db.JudgePassPartial, c.Close.Status)
	require.Equal(t, 1, c.Close.ChunksJudged)
	require.NotEmpty(t, c.Findings, "the findings judged before the stop are stored")
	require.Nil(t, c.Metrics)
	require.Empty(t, pdb.evalMetrics)
}
