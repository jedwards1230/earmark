package eval

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/runs"
)

// `earmark eval --rejudge` (CONTRACT §2.15 "Re-judge"): judge every done
// transcript that has stored chunks and no 'complete' judge pass under the
// current judge recipe (or one equivalent to it), keyset-paged, many calls in
// flight. Each chunk is first looked up in fn_calls, so a killed run resumes
// without paying for the chunks it already got answers for. Under --write
// each transcript gets a judge pass opened as 'running' before its first call
// and closed — with its findings and the run_metrics latch — in one
// transaction.

// rejudgeDB is the slice of the database the re-judge needs (*db.DB).
type rejudgeDB interface {
	evalpkg.PassStore
	RejudgeTargets(ctx context.Context, q db.RejudgeQuery) ([]db.RejudgeTarget, error)
	GetEvalChunksForTranscript(ctx context.Context, transcriptID string) ([]db.EvalChunk, error)
	GetRecipeFindingKeys(ctx context.Context, transcriptID, recipeID string) (map[db.FindingKey]bool, error)
	RegisterRecipe(ctx context.Context, r recipe.Recipe) (string, error)
	RecipesEquivalent(ctx context.Context, a, b string) (bool, error)
}

// rejudgeOptions are the knobs of one runRejudge call.
type rejudgeOptions struct {
	write        bool
	expectRecipe string
	concurrency  int // judge calls in flight, across every transcript
	limit        int // transcripts attempted (0 = all)
	book         string
	shard        int
	shards       int // 0 = unsharded
	pageSize     int // 0 → defaultRejudgePageSize
	args         string
}

// defaultRejudgePageSize is the keyset page of transcript identities (no text).
const defaultRejudgePageSize = 64

// commitTimeout bounds a pass's commit, which runs detached from a cancelled
// run so a stop still records what was judged.
const commitTimeout = 30 * time.Second

// ErrRecipeMismatch: --expect-recipe names a recipe the current judge is not.
var ErrRecipeMismatch = errors.New("current judge recipe differs from --expect-recipe")

// validateCallFlags checks the call-policy flags shared by every mode.
func validateCallFlags(o options) error {
	switch {
	case o.callTimeout <= 0:
		return errors.New("--call-timeout must be positive")
	case o.retries < 0:
		return errors.New("--retries must be >= 0")
	}
	return nil
}

// rejudgeOptionsFrom validates the flags of a --rejudge run.
func rejudgeOptionsFrom(o options, book string) (rejudgeOptions, error) {
	if o.concurrency < 1 {
		return rejudgeOptions{}, errors.New("--concurrency must be >= 1")
	}
	if o.limit < 0 {
		return rejudgeOptions{}, errors.New("--limit must be >= 0")
	}
	shard, shards, err := parseShard(o.shard)
	if err != nil {
		return rejudgeOptions{}, err
	}
	ro := rejudgeOptions{write: o.write, expectRecipe: o.expectRecipe, concurrency: o.concurrency,
		limit: o.limit, book: book, shard: shard, shards: shards}
	ro.args = ro.summary()
	return ro, nil
}

// parseShard parses "i/N" (0 ≤ i < N); "" = unsharded.
func parseShard(s string) (shard, shards int, err error) {
	if s == "" {
		return 0, 0, nil
	}
	if _, err := fmt.Sscanf(s, "%d/%d", &shard, &shards); err != nil || fmt.Sprintf("%d/%d", shard, shards) != s ||
		shards < 1 || shard < 0 || shard >= shards {
		return 0, 0, fmt.Errorf("--shard %q must be i/N with 0 <= i < N", s)
	}
	return shard, shards, nil
}

// summary is the step_runs.args line.
func (o rejudgeOptions) summary() string {
	s := "--rejudge"
	if o.book != "" {
		s += fmt.Sprintf(" --book %q", o.book)
	}
	if o.shards > 0 {
		s += fmt.Sprintf(" --shard %d/%d", o.shard, o.shards)
	}
	if o.limit > 0 {
		s += fmt.Sprintf(" --limit %d", o.limit)
	}
	s += fmt.Sprintf(" --concurrency %d", o.concurrency)
	if o.expectRecipe != "" {
		s += " --expect-recipe " + o.expectRecipe
	}
	if o.write {
		s += " --write"
	}
	return s
}

// rejudgeTally is a run's outcome so far. Guarded by its mutex: transcripts
// finish concurrently.
type rejudgeTally struct {
	mu                                            sync.Mutex
	attempted, complete, partial, failed, skipped int
	chunks, cached, chunkErrors, fallback         int
	findings, dupes                               int
	costUSD                                       float64
	hasCost                                       bool
	// warnedFallback: the first fallback reply was reported.
	warnedFallback bool
}

// runRejudge runs `earmark eval --rejudge`. It returns an error — a non-zero
// exit — when --expect-recipe does not match (before any model call), when
// the run is cancelled, when a book record cannot be read (judge@v3), or
// when transcripts were attempted and none completed (a dead endpoint must
// not hide behind exit 0).
func runRejudge(ctx context.Context, out io.Writer, rdb rejudgeDB, judge *evalpkg.Judge, o rejudgeOptions) (err error) {
	var outMu sync.Mutex
	p := func(format string, a ...any) {
		outMu.Lock()
		defer outMu.Unlock()
		_, _ = fmt.Fprintf(out, format, a...)
	}

	cur := judge.Recipe()
	rid, err := rdb.RegisterRecipe(ctx, cur)
	if err != nil {
		return fmt.Errorf("register the judge recipe: %w", err)
	}
	if o.expectRecipe != "" {
		ok, err := rdb.RecipesEquivalent(ctx, o.expectRecipe, rid)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: refusing to start: current %s (%s, %s), expected %s — check `earmark models` for the propose step",
				ErrRecipeMismatch, rid, judge.Model(), cur.PromptVersion, o.expectRecipe)
		}
	}
	pol := judge.CallPolicy()
	p("Re-judging under propose recipe %s (%s, %s): concurrency %d, call timeout %s, retries %d.\n",
		rid, judge.Model(), cur.PromptVersion, o.concurrency, effectiveTimeout(pol), pol.Retries)
	if !o.write {
		p("DRY RUN: this still makes PAID model calls. Every reply is logged and cached in fn_calls (fn %s),\n"+
			"so a later --write run serves these chunks from the cache; no findings, judge_passes or run_metrics are written.\n",
			evalpkg.JudgeChunkFn)
	}

	mode := db.StepRunDryRun
	if o.write {
		mode = db.StepRunWrite
	}
	rec := runs.StartIfStore(ctx, rdb, runs.Spec{
		Step: runs.StepEvalRejudge, Mode: mode, RecipeID: rid, Model: judge.Model(), Args: o.args,
	})
	defer func() { rec.Finish(err) }()
	rec.Phase("asking " + judge.Model())

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		fatalMu sync.Mutex
		fatal   error
	)
	setFatal := func(e error) {
		fatalMu.Lock()
		if fatal == nil {
			fatal = e
		}
		fatalMu.Unlock()
		cancel()
	}

	pageSize := o.pageSize
	if pageSize <= 0 {
		pageSize = defaultRejudgePageSize
	}
	lim := evalpkg.NewLimiter(o.concurrency)
	inflight := make(chan struct{}, o.concurrency) // bounds transcripts (their chunks) in memory
	var tl rejudgeTally
	var wg sync.WaitGroup
	var cursor db.TranscriptCursor
	dispatched := 0
	var walkErr error
walk:
	for {
		page, err := rdb.RejudgeTargets(runCtx, db.RejudgeQuery{RecipeID: rid, Book: o.book,
			Shard: o.shard, Shards: o.shards, After: cursor, Limit: pageSize})
		if err != nil {
			if runCtx.Err() == nil {
				walkErr = fmt.Errorf("select transcripts: %w", err)
			}
			break
		}
		for _, t := range page {
			if o.limit > 0 && dispatched >= o.limit {
				break walk
			}
			select {
			case inflight <- struct{}{}:
			case <-runCtx.Done():
				break walk
			}
			cursor = t.Cursor()
			dispatched++
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-inflight }()
				rejudgeOne(runCtx, p, rdb, judge, rec, lim, &tl, t, rid, o.write, setFatal)
			}()
		}
		if len(page) < pageSize {
			break
		}
	}
	wg.Wait()

	printRejudgeSummary(p, o, &tl)
	switch {
	case fatal != nil:
		return fatal
	case walkErr != nil:
		return walkErr
	case ctx.Err() != nil:
		return fmt.Errorf("re-judge stopped: %w", ctx.Err())
	case tl.attempted > 0 && tl.complete == 0:
		return fmt.Errorf("no transcript completed: %d attempted (%d partial, %d failed) — judge endpoint, API key, or writes broken?",
			tl.attempted, tl.partial, tl.failed)
	}
	return nil
}

func effectiveTimeout(p evalpkg.CallPolicy) time.Duration {
	if p.Timeout <= 0 {
		return evalpkg.DefaultCallTimeout
	}
	return p.Timeout
}

// rejudgeOne judges one transcript and, under write, records its pass.
func rejudgeOne(ctx context.Context, p func(string, ...any), rdb rejudgeDB, judge *evalpkg.Judge, rec *runs.Run,
	lim *evalpkg.Limiter, tl *rejudgeTally, t db.RejudgeTarget, rid string, write bool, setFatal func(error)) {
	name := filepath.Base(t.FilePath)
	chunks, err := rdb.GetEvalChunksForTranscript(ctx, t.TranscriptID)
	if err != nil || len(chunks) == 0 {
		if ctx.Err() != nil {
			return
		}
		tl.add(func(tl *rejudgeTally) { tl.skipped++ })
		rec.Count("transcripts_skipped", 1)
		if err != nil {
			p("  warn %s: read chunks failed (%v); skipped\n", name, err)
		}
		return
	}

	var passID int64
	if write {
		passID, err = judge.OpenPass(ctx, rdb, evalpkg.PassTarget{TranscriptID: t.TranscriptID, JobID: t.JobID,
			FilePath: t.FilePath}, len(chunks), rec.ID())
		if err != nil {
			if ctx.Err() == nil {
				// No pass, no call: never spend unrecorded.
				tl.add(func(tl *rejudgeTally) { tl.skipped++ })
				rec.Count("transcripts_skipped", 1)
				p("  warn %s: open judge pass failed (%v); skipped\n", name, err)
			}
			return
		}
	}

	started := time.Now()
	findings, stats, jerr := evalpkg.JudgeChunks(ctx, judge, chunks, lim, func(res evalpkg.Result, cerr error) {
		outcome := "judged"
		switch {
		case cerr != nil:
			outcome = "failed"
		case res.Cached:
			outcome = "cached"
		}
		rec.Tick(outcome, res.Usage.CostUSD)
	})
	finished := time.Now()
	stopped := jerr != nil && ctx.Err() != nil
	bookErr := jerr != nil && !stopped && errors.Is(jerr, evalpkg.ErrBookContext)
	if bookErr {
		// The book record could not be read: nothing was judged, and judging
		// path-only under this recipe would misstate what the judge saw.
		setFatal(fmt.Errorf("%s: %w", name, jerr))
	}
	cl := evalpkg.PassClose(stats, len(chunks), jerr)

	var res db.JudgePassResult
	if write {
		m := evalMetricsFor(t.JobID, judge.Model(), stats, cl, len(chunks), started, finished)
		c := db.JudgePassCommit{PassID: passID, TranscriptID: t.TranscriptID, Findings: findings, Close: cl}
		if !stopped && !bookErr {
			// A stop is not a judge failure, and a book record that could not
			// be read judged nothing: leave run_metrics as it was for both.
			c.Metrics = &m
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
		res, err = rdb.CommitJudgePass(cctx, c)
		cancel()
		if err != nil {
			tl.add(func(tl *rejudgeTally) { tl.attempted++; tl.failed++ })
			rec.Count("transcripts_failed", 1)
			p("  FAIL %s: commit failed (%v); pass %d left running\n", name, err, passID)
			return
		}
	} else {
		res = dryRunDedupe(ctx, rdb, t.TranscriptID, findings)
	}

	tl.add(func(tl *rejudgeTally) {
		tl.attempted++
		switch cl.Status {
		case db.JudgePassComplete:
			tl.complete++
		case db.JudgePassPartial:
			tl.partial++
		default:
			tl.failed++
		}
		tl.chunks += stats.ChunksEvaluated
		tl.cached += stats.ChunksCached
		tl.chunkErrors += cl.ChunksFailed
		tl.findings += res.Inserted
		tl.dupes += res.Duplicates
		tl.fallback += stats.ChunksFallback
		if stats.ChunksFallback > 0 && !tl.warnedFallback {
			tl.warnedFallback = true
			p("WARNING: %s answered with a model other than the recipe expects (%s); those replies are stored as\n"+
				"model_fallback and NEVER served from cache, so every run pays for them again. If this is the normal\n"+
				"answer for the alias (a dated snapshot id), pin the propose step's expected_model (CONTRACT §2.18).\n",
				name, stats.ResolvedModel)
		}
		tl.costUSD += stats.CostUSD
		tl.hasCost = tl.hasCost || stats.HasCost
	})
	rec.Count("transcripts_"+cl.Status, 1)
	rec.Count("findings", int64(res.Inserted))
	rec.Count("fallback", int64(stats.ChunksFallback))

	verb, tail := "", ""
	if write {
		verb = fmt.Sprintf("pass %d %s", passID, cl.Status)
	} else {
		verb = "[dry-run] would be " + cl.Status
	}
	if cl.Error != "" {
		tail = " — " + cl.Error
	}
	p("  %s %s: %d/%d chunks judged (%d cached), %d new finding(s) (%d already recorded by this recipe), %s%s\n",
		verb, name, cl.ChunksJudged, cl.ChunksTotal, cl.ChunksCached, res.Inserted, res.Duplicates,
		costLabel(stats), tail)
}

// evalMetricsFor is the run_metrics eval slice of a pass: the latch when it
// completed, the failure record otherwise (CONTRACT §1.5).
func evalMetricsFor(jobID, model string, stats evalpkg.RunStats, cl db.JudgePassClose, total int,
	started, finished time.Time) db.EvalMetrics {
	m := db.EvalMetrics{JobID: jobID, StartedAt: started, Model: model, ResolvedModel: stats.ResolvedModel,
		Chunks: stats.ChunksEvaluated, Skipped: cl.ChunksFailed}
	if cl.Status == db.JudgePassComplete {
		m.FinishedAt = finished
		return m
	}
	m.FailedAt, m.FailedChunks = finished, cl.ChunksFailed
	m.Error = cl.Error
	if m.Error == "" {
		m.Error = fmt.Sprintf("%d of %d chunks failed", cl.ChunksFailed, total)
	}
	return m
}

// dryRunDedupe reports what a commit would insert: a finding already
// recorded for the transcript by its OWN recipe (or one equivalent to it) is a
// duplicate, as in db.CommitJudgePass — a fallback answer's findings are
// checked against the fallback's recipe. Best-effort: a read error counts that
// recipe's findings as new.
func dryRunDedupe(ctx context.Context, rdb rejudgeDB, transcriptID string, findings []db.Finding) db.JudgePassResult {
	var res db.JudgePassResult
	keys := map[string]map[db.FindingKey]bool{}
	for _, f := range findings {
		rid := ""
		if f.Recipe != nil {
			rid, _ = f.Recipe.ID()
		}
		if rid == "" {
			res.Inserted++
			continue
		}
		if _, ok := keys[rid]; !ok {
			k, err := rdb.GetRecipeFindingKeys(ctx, transcriptID, rid)
			if err != nil || k == nil {
				k = map[db.FindingKey]bool{}
			}
			keys[rid] = k
		}
		key := db.KeyOf(f)
		if keys[rid][key] {
			res.Duplicates++
			continue
		}
		keys[rid][key] = true
		res.Inserted++
	}
	return res
}

func costLabel(s evalpkg.RunStats) string {
	if !s.HasCost {
		return "cost not reported"
	}
	return fmt.Sprintf("$%.4f", s.CostUSD)
}

func (tl *rejudgeTally) add(f func(*rejudgeTally)) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	f(tl)
}

func printRejudgeSummary(p func(string, ...any), o rejudgeOptions, tl *rejudgeTally) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if tl.attempted == 0 && tl.skipped == 0 {
		p("No transcript needs re-judging under this recipe.\n")
		return
	}
	cost := "cost not reported"
	if tl.hasCost {
		cost = fmt.Sprintf("$%.4f", tl.costUSD)
	}
	p("\nRe-judge: %d transcript(s) attempted — %d complete, %d partial, %d failed; %d skipped.\n",
		tl.attempted, tl.complete, tl.partial, tl.failed, tl.skipped)
	p("%d chunk(s) judged (%d served from fn_calls), %d failed; %d new finding(s), %d already recorded by this recipe; %s.\n",
		tl.chunks, tl.cached, tl.chunkErrors, tl.findings, tl.dupes, cost)
	if tl.fallback > 0 {
		p("WARNING: %d chunk(s) were answered by a model other than the recipe expects (model_fallback: stored, never cached).\n",
			tl.fallback)
	}
	if o.limit > 0 && tl.attempted+tl.skipped >= o.limit {
		p("Stopped at --limit %d.\n", o.limit)
	}
	if !o.write {
		p("(dry-run) nothing written but fn_calls; pass --write to record the passes and findings (answered chunks are served from cache).\n")
	}
}
