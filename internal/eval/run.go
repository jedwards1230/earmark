package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jedwards1230/earmark/internal/db"
)

// RunOptions selects what the judge evaluates. Exactly one of Book / Sample
// drives the chunk set:
//   - Book != ""  → evaluate the chunks under that book directory prefix.
//   - Sample > 0  → evaluate a random sample of that many chunks library-wide.
//
// Limit caps the per-book chunk count (ignored in sample mode, where Sample is
// the cap). Write persists the findings; when false the run is a dry-run and
// nothing is inserted (mirrors `requeue`'s --yes gate).
type RunOptions struct {
	Book   string
	Sample int
	Limit  int
	Write  bool
}

// RunStats summarizes a judge run for the operator-facing report.
type RunStats struct {
	ChunksEvaluated int // chunks the judge successfully evaluated
	ChunksSkipped   int // chunks skipped due to a transient judge error
	FindingsFound   int
	Persisted       bool
	// FirstError is the first per-chunk judge error's message ("" when none) —
	// recorded as run_metrics.eval_error when the run fails.
	FirstError string
	// ResolvedModel lists the distinct models the endpoint reported serving
	// the run, comma-joined in first-seen order ("" when none reported). More
	// than one means a router fell back mid-run.
	ResolvedModel string
}

// addResolved folds one chunk's resolved model into s.ResolvedModel.
func (s *RunStats) addResolved(model string) {
	if model == "" {
		return
	}
	for _, m := range strings.Split(s.ResolvedModel, ",") {
		if m == model {
			return
		}
	}
	if s.ResolvedModel == "" {
		s.ResolvedModel = model
		return
	}
	s.ResolvedModel += "," + model
}

// Complete reports whether every chunk was judged (none skipped). This is the
// latch-only-on-success rule: a run is "done" — eligible to write
// eval_finished_at — only when the judge evaluated every chunk. A partial run
// still yields its findings, but leaves the job re-judgeable.
func (s RunStats) Complete() bool { return s.ChunksSkipped == 0 }

// Run fetches the selected chunks (read-only), judges each, collects the
// findings, and — only when opts.Write — persists them in one insert-only
// transaction. It NEVER mutates the transcript tables; the only write it can
// perform is the InsertFindings call, and only under the Write gate.
//
// A transient judge error on one chunk (endpoint glitch, timeout on a single
// request) does NOT discard the run: the chunk is skipped (logged, counted in
// ChunksSkipped) and the run continues, so a mid-stream blip can't silently wipe
// the advisory findings already collected — partial results beat nothing for
// quality observability. This mirrors the malformed-response soft-fail in
// JudgeChunk. Only cancellation/expiry of the CALLER's context aborts
// (returning the findings collected so far plus the context error), since
// continuing past that is pointless and the caller asked to stop.
//
// Note the chat client's own per-request timeout surfaces as an error that
// satisfies errors.Is(err, context.DeadlineExceeded) while ctx is still live.
// That is a per-chunk failure, not a stop request, so it is skipped like any
// other — the decision is made on ctx.Err(), never on the error's shape.
//
// The returned findings are always populated (so a dry-run can print exactly
// what it would record); Persisted reports whether they were written.
func Run(ctx context.Context, reader ChunkReader, judge *Judge, writer FindingWriter, opts RunOptions) ([]db.Finding, RunStats, error) {
	chunks, err := selectChunks(ctx, reader, opts)
	if err != nil {
		return nil, RunStats{}, err
	}
	return runChunks(ctx, judge, writer, chunks, opts.Write)
}

// RunOnChunks judges an already-resolved set of chunks (e.g. the in-memory
// chunks the embed worker produces before embedding, per the batched-pipeline
// design) and, when write is true, persists the findings. It shares the
// per-chunk judging + soft-fail semantics with Run; it just skips the reader.
// Used by the in-pipeline eval path so eval can run on chunks that are not yet
// in transcript_chunks (their UUIDs are caller-assigned and consistent with the
// rows the worker subsequently inserts).
func RunOnChunks(ctx context.Context, judge *Judge, writer FindingWriter, chunks []db.EvalChunk, write bool) ([]db.Finding, RunStats, error) {
	return runChunks(ctx, judge, writer, chunks, write)
}

// runChunks is the shared judge-collect-persist core behind Run and RunOnChunks.
// A transient per-chunk judge error skips that chunk (logged) and the run
// continues; a cancelled/expired context aborts but returns the findings
// gathered so far. Findings are persisted only under the write gate.
func runChunks(ctx context.Context, judge *Judge, writer FindingWriter, chunks []db.EvalChunk, write bool) ([]db.Finding, RunStats, error) {
	var all []db.Finding
	var stats RunStats
	for _, c := range chunks {
		res, jerr := judge.JudgeChunk(ctx, c)
		if jerr != nil {
			// The caller cancelled (or its deadline passed): a deliberate stop.
			// Abort, but return the findings gathered so far.
			if ctx.Err() != nil {
				stats.FindingsFound = len(all)
				return all, stats, fmt.Errorf("judge run stopped: %w", errors.Join(ctx.Err(), jerr))
			}
			// A transient per-chunk judge error (network glitch, single-request
			// client timeout): skip this chunk and keep going.
			judge.logger.Warn("skipping chunk due to judge error",
				"chunk_id", c.ChunkID, "error", jerr)
			stats.ChunksSkipped++
			if stats.FirstError == "" {
				stats.FirstError = jerr.Error()
			}
			continue
		}
		stats.ChunksEvaluated++
		stats.addResolved(res.ResolvedModel)
		all = append(all, res.Findings...)
	}

	stats.FindingsFound = len(all)

	if write && len(all) > 0 {
		if err := writer.InsertFindings(ctx, all); err != nil {
			return all, stats, fmt.Errorf("persist findings: %w", err)
		}
		stats.Persisted = true
	}
	return all, stats, nil
}

// selectChunks resolves the chunk set for a run from the options.
func selectChunks(ctx context.Context, reader ChunkReader, opts RunOptions) ([]db.EvalChunk, error) {
	if opts.Sample > 0 {
		return reader.SampleEvalChunks(ctx, opts.Sample)
	}
	if opts.Book != "" {
		return reader.GetEvalChunksForBook(ctx, opts.Book, opts.Limit)
	}
	return nil, fmt.Errorf("nothing to evaluate: pass a book or --sample N")
}
