package eval

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// Judge passes (CONTRACT §2.15 "Lineage"): a path that judges a whole
// transcript — `earmark eval --rejudge`, the --backfill-* modes and the
// in-pipeline worker — opens a judge_passes row as 'running' before its first
// model call and closes it, with the findings and the run_metrics latch, in
// one transaction. Book and --sample runs judge a selection of chunks, not a
// transcript, and record no pass.

// PassStore is where judge passes are written (*db.DB implements it). It
// writes judge_passes, transcript_findings and the run_metrics eval slice —
// never the transcript tables.
type PassStore interface {
	OpenJudgePass(ctx context.Context, p db.JudgePassOpen) (int64, error)
	CommitJudgePass(ctx context.Context, c db.JudgePassCommit) (db.JudgePassResult, error)
}

// PassTarget is the transcript a pass judges.
type PassTarget struct {
	TranscriptID string
	JobID        string
	FilePath     string
}

// OpenPass opens a 'running' pass for t under the judge's current recipe.
func (j *Judge) OpenPass(ctx context.Context, store PassStore, t PassTarget, chunks int, stepRunID int64) (int64, error) {
	return store.OpenJudgePass(ctx, db.JudgePassOpen{
		TranscriptID: t.TranscriptID, JobID: t.JobID, FilePath: t.FilePath,
		Recipe: j.Recipe(), StepRunID: stepRunID, ChunksTotal: chunks,
	})
}

// PassClose turns one transcript's judging (stats over its total chunks;
// stop is the error that stopped it early, nil when it ran to the end) into
// the pass's closing record: complete when every chunk was judged, partial
// when some were, failed when none were. A chunk counts as failed whether its
// call failed, its reply was invalid, or it was never reached.
func PassClose(stats RunStats, total int, stop error) db.JudgePassClose {
	judged := stats.ChunksEvaluated
	failed := max(total-judged, 0)
	cl := db.JudgePassClose{
		ChunksTotal:  total,
		ChunksJudged: judged,
		ChunksFailed: failed,
		ChunksCached: stats.ChunksCached,
		Dropped:      stats.Dropped,
	}
	switch {
	case failed == 0 && stop == nil:
		cl.Status = db.JudgePassComplete
	case judged > 0:
		cl.Status = db.JudgePassPartial
	default:
		cl.Status = db.JudgePassFailed
	}
	switch {
	case stop != nil:
		cl.Error = fmt.Sprintf("stopped after %d of %d chunks: %v", judged, total, stop)
	case failed > 0:
		cl.Error = fmt.Sprintf("%d of %d chunks failed: %s", failed, total, stats.FirstError)
	}
	if stats.HasUsage {
		in, out := stats.InputTokens, stats.OutputTokens
		cl.InputTokens, cl.OutputTokens = &in, &out
	}
	if stats.HasCost {
		c := stats.CostUSD
		cl.CostUSD = &c
	}
	cl.LatencyP50MS = percentileMS(stats.Latencies, 50)
	cl.LatencyP95MS = percentileMS(stats.Latencies, 95)
	for _, bc := range stats.BookContexts {
		if bc.SHA256 != "" {
			cl.BookContextSHA = bc.SHA256
		}
	}
	return cl
}

// percentileMS is the nearest-rank p-th percentile of ds in milliseconds; nil
// when ds is empty.
func percentileMS(ds []time.Duration, p int) *int {
	if len(ds) == 0 {
		return nil
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	rank := (p*len(s) + 99) / 100 // ceil(p/100 · n)
	ms := int(s[max(rank, 1)-1].Milliseconds())
	return &ms
}
