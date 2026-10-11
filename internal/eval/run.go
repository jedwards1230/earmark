package eval

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
//
// Seed (sample mode only) fixes which chunks the sample picks: the same seed
// over the same library picks the same chunks, so two prompts can be measured
// on identical input. The reader must implement SeededChunkReader.
//
// Observe, when set, is called once per chunk with the judge's result and
// error (nil on success), in chunk order, before the next chunk is judged —
// the hook behind `earmark eval --dump`.
type RunOptions struct {
	Book    string
	Sample  int
	Seed    string
	Limit   int
	Write   bool
	Observe func(Result, error)
}

// SeededChunkReader is the optional ChunkReader extension behind --seed:
// a library sample in an order fixed by seed (db.DB implements it).
type SeededChunkReader interface {
	SampleEvalChunksSeeded(ctx context.Context, limit int, seed string) ([]db.EvalChunk, error)
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
	// Dropped counts, per drop reason (Drop* constants), the findings the
	// judge returned that never became rows. nil when none were dropped.
	Dropped map[string]int
	// PromptVersion is the judge prompt the run used (judge@v2 / judge@v3).
	PromptVersion string
	// BookContexts is the book context each transcript's chunks were judged
	// with, keyed by transcript id: source (asin_record / path_only /
	// record_error) and
	// book_context_sha256 (CONTRACT §2.15 "Book context"). Every transcript
	// the run selected is present, judged or not. A judge pass records it as
	// judge_passes.book_context_sha256.
	BookContexts map[string]BookContext
	// ChunksCached counts the evaluated chunks served from fn_calls (no
	// model call, no cost).
	ChunksCached int
	// ChunksFallback counts the evaluated chunks answered by a model other
	// than the recipe's expected one (Result.Fallback). Their replies are
	// never served from cache, so a run where this is high pays every time —
	// pin the propose step's expected_model.
	ChunksFallback int
	// InputTokens / OutputTokens / CostUSD sum every model call's accounting
	// as the endpoint reported it (failed calls included; HasUsage / HasCost
	// false when no call reported any).
	InputTokens  int64
	OutputTokens int64
	HasUsage     bool
	CostUSD      float64
	HasCost      bool
	// Latencies are the evaluated chunks' model-call latencies (served from
	// cache excluded), in chunk order.
	Latencies []time.Duration
}

// addCall folds one chunk's call accounting into s (judged or failed).
func (s *RunStats) addCall(res Result) {
	if res.Usage.HasUsage {
		s.InputTokens += int64(res.Usage.InputTokens)
		s.OutputTokens += int64(res.Usage.OutputTokens)
		s.HasUsage = true
	}
	if res.Usage.HasCost {
		s.CostUSD += res.Usage.CostUSD
		s.HasCost = true
	}
}

// addDropped folds one chunk's drops into s.Dropped.
func (s *RunStats) addDropped(ds []Dropped) {
	for _, d := range ds {
		if s.Dropped == nil {
			s.Dropped = map[string]int{}
		}
		s.Dropped[d.Reason]++
	}
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
	records := judge.records
	if records == nil {
		// db.DB is both: a judge wired to no record source reads them through
		// the run's chunk reader.
		records, _ = reader.(BookRecordReader)
	}
	return runChunks(ctx, judge, records, writer, chunks, opts.Write, opts.Observe, nil)
}

// RunOnChunks judges an already-resolved set of chunks (e.g. the in-memory
// chunks the embed worker produces before embedding, per the batched-pipeline
// design) and, when write is true, persists the findings. It shares the
// per-chunk judging + soft-fail semantics with Run; it just skips the reader.
// Used by the in-pipeline eval path so eval can run on chunks that are not yet
// in transcript_chunks (their UUIDs are caller-assigned and consistent with the
// rows the worker subsequently inserts).
//
// A book-context prompt reads the chunks' book records through the judge's
// record source (SetBookRecords).
func RunOnChunks(ctx context.Context, judge *Judge, writer FindingWriter, chunks []db.EvalChunk, write bool) ([]db.Finding, RunStats, error) {
	return runChunks(ctx, judge, judge.records, writer, chunks, write, nil, nil)
}

// JudgeChunks judges chunks without persisting anything, like RunOnChunks
// with write=false, but CONCURRENTLY under lim: each chunk's model call
// holds one of lim's slots, and lim may be shared by any number of
// concurrent JudgeChunks calls (one bound over every transcript a run has in
// flight). A nil lim judges serially. observe, when set, is called once per
// chunk (serialized, in completion order). The findings and stats come back
// in chunk order whatever the completion order. A cancelled ctx returns the
// findings of the chunks judged so far with the error.
func JudgeChunks(ctx context.Context, judge *Judge, chunks []db.EvalChunk, lim *Limiter,
	observe func(Result, error)) ([]db.Finding, RunStats, error) {
	return runChunks(ctx, judge, judge.records, nil, chunks, false, observe, lim)
}

// Limiter bounds concurrent judge calls. One Limiter shared by every
// JudgeChunks call of a run is the run's global concurrency bound.
type Limiter struct{ slots chan struct{} }

// NewLimiter returns a limiter admitting n concurrent calls (n < 1 → 1).
func NewLimiter(n int) *Limiter {
	return &Limiter{slots: make(chan struct{}, max(n, 1))}
}

// acquire takes a slot, or returns ctx's error once it is done.
func (l *Limiter) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Limiter) release() { <-l.slots }

// runChunks is the shared judge-collect-persist core behind Run and RunOnChunks.
// A transient per-chunk judge error skips that chunk (logged) and the run
// continues; a cancelled/expired context aborts but returns the findings
// gathered so far. Findings are persisted only under the write gate.
//
// Under a book-context prompt the chunks' book records are read once, before
// the first judge call (GetBookRecords in batches of db.MaxDecideBookBatch
// dirs). A read error, or no record source at all, fails the run before any
// paid call (ErrBookContext) — it never silently judges path_only. A book
// whose own record is unusable (BookContextRecordError) is not judged: each
// of its chunks is skipped with the reason, like a failed judge call, so only
// that book's transcripts record an eval failure.
func runChunks(ctx context.Context, judge *Judge, records BookRecordReader, writer FindingWriter, chunks []db.EvalChunk, write bool,
	observe func(Result, error), lim *Limiter) ([]db.Finding, RunStats, error) {
	var all []db.Finding
	stats := RunStats{PromptVersion: judge.prompt.Version()}
	contexts, err := judge.bookContextsFor(ctx, records, chunks)
	if err != nil {
		return nil, stats, err
	}
	for _, c := range chunks {
		bc := pathOnlyContext
		if v, ok := contexts[filepath.Dir(c.FilePath)]; ok {
			bc = v
		}
		if stats.BookContexts == nil {
			stats.BookContexts = map[string]BookContext{}
		}
		stats.BookContexts[c.TranscriptID] = bc
	}
	judgeOne := func(c db.EvalChunk) (Result, error) {
		bc := stats.BookContexts[c.TranscriptID]
		if bc.err != nil {
			return Result{Chunk: c, PromptVersion: judge.prompt.Version(), BookContext: bc}, bc.err
		}
		return judge.JudgeChunkWithBook(ctx, c, bc)
	}
	fold := func(res Result, jerr error) {
		stats.addCall(res)
		if jerr != nil {
			// A transient per-chunk judge error (network glitch, single-request
			// client timeout, an invalid reply): skip this chunk and keep going.
			judge.logger.Warn("skipping chunk due to judge error",
				"chunk_id", res.Chunk.ChunkID, "error", jerr)
			stats.ChunksSkipped++
			if stats.FirstError == "" {
				stats.FirstError = jerr.Error()
			}
			return
		}
		stats.ChunksEvaluated++
		if res.Fallback {
			stats.ChunksFallback++
		}
		if res.Cached {
			stats.ChunksCached++
		} else {
			stats.Latencies = append(stats.Latencies, res.Elapsed)
		}
		stats.addResolved(res.ResolvedModel)
		stats.addDropped(res.Dropped)
		all = append(all, res.Findings...)
	}

	if lim == nil {
		for _, c := range chunks {
			res, jerr := judgeOne(c)
			if observe != nil {
				observe(res, jerr)
			}
			if jerr != nil && ctx.Err() != nil {
				// The caller cancelled (or its deadline passed): a deliberate stop.
				// Abort, but return the findings gathered so far.
				stats.FindingsFound = len(all)
				return all, stats, fmt.Errorf("judge run stopped: %w", errors.Join(ctx.Err(), jerr))
			}
			fold(res, jerr)
		}
	} else {
		results := make([]Result, len(chunks))
		errs := make([]error, len(chunks))
		started := make([]bool, len(chunks))
		var wg sync.WaitGroup
		var obsMu sync.Mutex
		for i, c := range chunks {
			if lim.acquire(ctx) != nil {
				break
			}
			started[i] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer lim.release()
				res, jerr := judgeOne(c)
				results[i], errs[i] = res, jerr
				if observe != nil {
					obsMu.Lock()
					observe(res, jerr)
					obsMu.Unlock()
				}
			}()
		}
		wg.Wait()
		stopped := ctx.Err()
		for i := range chunks {
			if !started[i] {
				continue
			}
			if errs[i] != nil && stopped != nil {
				stats.addCall(results[i])
				continue // stopped mid-call: neither judged nor a judge failure
			}
			fold(results[i], errs[i])
		}
		if stopped != nil {
			stats.FindingsFound = len(all)
			return all, stats, fmt.Errorf("judge run stopped: %w", stopped)
		}
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

// bookContextsFor reads the book contexts of chunks' books when the judge's
// prompt carries one; nil (every chunk path_only) otherwise. A book-context
// prompt with no record source is an error (ErrBookContext): judging every
// chunk path_only under that prompt's recipe would misstate what it saw.
func (j *Judge) bookContextsFor(ctx context.Context, records BookRecordReader, chunks []db.EvalChunk) (map[string]BookContext, error) {
	if !j.prompt.usesBookContext() || len(chunks) == 0 {
		return nil, nil
	}
	if records == nil {
		return nil, fmt.Errorf("%w: %s needs book records but the judge has no record source", ErrBookContext, j.prompt.Version())
	}
	return bookContexts(ctx, records, chunks)
}

// selectChunks resolves the chunk set for a run from the options.
func selectChunks(ctx context.Context, reader ChunkReader, opts RunOptions) ([]db.EvalChunk, error) {
	if opts.Seed != "" {
		if opts.Sample <= 0 {
			return nil, fmt.Errorf("a seed needs a sample size (--sample N)")
		}
		sr, ok := reader.(SeededChunkReader)
		if !ok {
			return nil, fmt.Errorf("this chunk reader cannot sample by seed")
		}
		return sr.SampleEvalChunksSeeded(ctx, opts.Sample, opts.Seed)
	}
	if opts.Sample > 0 {
		return reader.SampleEvalChunks(ctx, opts.Sample)
	}
	if opts.Book != "" {
		return reader.GetEvalChunksForBook(ctx, opts.Book, opts.Limit)
	}
	return nil, fmt.Errorf("nothing to evaluate: pass a book or --sample N")
}
