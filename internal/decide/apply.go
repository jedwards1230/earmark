package decide

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/runs"
)

// DefaultWriteBatch is how many transcripts one --yes write transaction covers.
const DefaultWriteBatch = 20

// workPage is how many findings one compute phase decides before writing.
const workPage = 500

// WriteStore is what a --yes run needs (*db.DB implements it): the dry run's
// reads, the work list, and the two decision writes.
type WriteStore interface {
	RunStore
	DecideWork(ctx context.Context, s db.DecideWorkScope) ([]db.DecideFinding, error)
	ApplyDecisions(ctx context.Context, recipeID string, evs []db.DecisionEvent) (db.ApplyResult, error)
	ApplyRecheckDecisions(ctx context.Context, recipeID string, evs []db.DecisionEvent) (db.ApplyResult, error)
}

// WorkCounter is the optional WriteStore extension that sizes a --yes run up
// front (*db.DB implements it): the progress total. Without it, or when the
// count fails, the run reports done with no total.
type WorkCounter interface {
	DecideWorkCount(ctx context.Context, s db.DecideWorkScope) (int, error)
}

// workCountTimeout bounds the up-front count: it is a progress nicety, never
// worth delaying the run for.
const workCountTimeout = 10 * time.Second

// ApplyOptions configures a --yes run.
type ApplyOptions struct {
	Book, IssueType string
	// Shard / Shards partition the work by transcript (Shards 0 = all).
	Shard, Shards int
	// Limit caps the findings decided in this run (0 = all in scope).
	Limit int
	// Batch is the transcripts per write transaction (0 = DefaultWriteBatch).
	Batch int
	// MaxAccepts caps the findings moved to accepted in this run (0 = no
	// cap). Each accept re-embeds its whole transcript on the next rebuild;
	// this bounds that work. Applies past the cap are not written — they stay
	// undecided and are decided by the next run.
	MaxAccepts  int
	Concurrency int
	Params      ShouldApplyParams
	// Progress, when set, records the run's live progress (CONTRACT §1.10).
	Progress *runs.Run
}

// WriteStats counts what a --yes run wrote.
type WriteStats struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Held     int `json:"held"`
	// Re-check of findings another decide recipe accepted: Kept (apply or
	// hold, the fix stays; holds are listed for review as KeptHeld) and
	// Reverted (reject: accepted → rejected counts in Rejected, applied →
	// reverted here).
	Kept     int `json:"kept"`
	KeptHeld int `json:"kept_held"`
	Reverted int `json:"reverted"`
	// Skipped: locked by another writer, no longer in scope, or the chunk
	// changed between compute and write. Decided again next run.
	Skipped int `json:"skipped"`
	// Reanchor: not written (chunk_changed / anchor_missing are reanchor's).
	Reanchor int `json:"reanchor"`
	// Capped: applies not written because MaxAccepts was reached.
	Capped int `json:"capped"`
	// Stopped names why the run ended early ("" = scope exhausted).
	Stopped string `json:"stopped,omitempty"`
}

// CurrentRecipe is the decide recipe this configuration would decide under,
// for current_recipes. ok=false when AI_ROLES.decide is unbound.
func CurrentRecipe(cfg *config.Config) (recipe.Recipe, bool, error) {
	if _, ok, err := ClientFromConfig(cfg); !ok || err != nil {
		return recipe.Recipe{}, ok, err
	}
	f, err := ShouldApplyFn(DefaultShouldApplyParams())
	if err != nil {
		return recipe.Recipe{}, true, err
	}
	return f.Recipe(""), true, nil
}

// Apply decides every finding in scope and writes the decisions (CONTRACT
// §2.19 "earmark decide --yes"). The decide recipe is registered once. Then,
// page by page in finding-id order: a compute phase with no transaction —
// rung 0, evidence and the model calls (cached in fn_calls) — and a write
// phase, one short transaction per Batch transcripts (db.ApplyDecisions for
// proposed findings, db.ApplyRecheckDecisions for findings another decide
// recipe accepted). No network I/O happens while a lock is held.
func Apply(ctx context.Context, store WriteStore, asker Asker, o ApplyOptions) (*Report, error) {
	if o.Concurrency < 1 {
		return nil, errors.New("decide: concurrency must be >= 1")
	}
	if o.Limit < 0 || o.MaxAccepts < 0 || o.Batch < 0 {
		return nil, errors.New("decide: limit, max-accepts and batch must be >= 0")
	}
	batch := o.Batch
	if batch == 0 {
		batch = DefaultWriteBatch
	}
	ev, err := NewEvaluator(store, asker, o.Params)
	if err != nil {
		return nil, err
	}
	recipeID, err := store.RegisterRecipe(ctx, ev.Recipe())
	if err != nil {
		return nil, fmt.Errorf("register decide recipe: %w", err)
	}
	rec := o.Progress
	rec.SetRecipe(recipeID, ShouldApplyModel)

	ws := &WriteStats{}
	var all []*item
	scope := db.DecideWorkScope{
		RecipeID: recipeID, RetryReason: ReasonJevUnavailable, Book: o.Book, IssueType: o.IssueType,
		Shard: o.Shard, Shards: o.Shards,
	}
	if rec != nil {
		rec.Phase("counting work")
		if total, ok := workTotal(ctx, store, scope, o.Limit); ok {
			rec.SetTotal(total)
		}
	}
	for page := 1; ws.Stopped == ""; page++ {
		scope.Limit = workPage
		if o.Limit > 0 {
			if left := o.Limit - len(all); left < scope.Limit {
				scope.Limit = left
			}
			if scope.Limit == 0 {
				ws.Stopped = "limit"
				break
			}
		}
		rec.Phase(fmt.Sprintf("loading page %d", page))
		work, err := store.DecideWork(ctx, scope)
		if err != nil {
			return nil, err
		}
		if len(work) == 0 {
			break
		}
		scope.After = work[len(work)-1].ID

		rec.Phase(fmt.Sprintf("page %d: loading chunks + rung-0", page))
		items, err := prepare(ctx, store, work, o.Params)
		if err != nil {
			return nil, err
		}
		rec.Phase(fmt.Sprintf("page %d: %s", page, askingPhase))
		evaluateAll(ctx, ev, items, o.Concurrency, rec)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before := *ws
		if err := writeItems(ctx, store, recipeID, items, batch, o.MaxAccepts, ws, pageRecorder{rec, page}); err != nil {
			return nil, err
		}
		rec.Count("skipped", int64(ws.Skipped-before.Skipped))
		rec.Count("capped", int64(ws.Capped-before.Capped))
		for _, it := range items {
			it.input, it.chunk, it.sentences = Input{}, nil, nil // the report needs finding + outcome only
		}
		all = append(all, items...)
	}

	rep := buildReport(RunOptions{Scope: db.DecideScope{Sample: o.Limit}}, recipeID, all, nil)
	rep.DryRun = false
	rep.Write = ws
	return rep, nil
}

// workTotal sizes the run: the work list's count, capped by limit. ok=false
// when the store cannot count or the count failed; the total then stays
// unknown (--limit alone is not used: the scope may be smaller).
func workTotal(ctx context.Context, store WriteStore, scope db.DecideWorkScope, limit int) (int64, bool) {
	wc, ok := store.(WorkCounter)
	if !ok {
		return 0, false
	}
	cctx, cancel := context.WithTimeout(ctx, workCountTimeout)
	defer cancel()
	n, err := wc.DecideWorkCount(cctx, scope)
	if err != nil {
		return 0, false
	}
	if limit > 0 && n > limit {
		n = limit
	}
	return int64(n), true
}

// pageRecorder labels the write phase of one page on the run.
type pageRecorder struct {
	rec  *runs.Run
	page int
}

func (p pageRecorder) batch(i, n int) {
	p.rec.Phase(fmt.Sprintf("page %d: writing batch %d/%d", p.page, i, n))
}

// eventOf is the decision event an outcome records; ok=false for an outcome
// that is not a decision (re-anchor needed).
func eventOf(recipeID string, o Outcome) (db.DecisionEvent, bool) {
	if classOf(o) == ClassReanchor {
		return db.DecisionEvent{}, false
	}
	e := db.DecisionEvent{
		FindingID: o.FindingID, RecipeID: recipeID, Outcome: o.Decision, Reason: o.Reason,
		P: o.P, ChunkTextSHA256: o.ChunkHash,
	}
	if o.Rung0.Pass {
		e.Evidence = o.Evidence
	}
	if o.FnCallID != 0 {
		id := o.FnCallID
		e.FnCallID = &id
	}
	return e, true
}

// writeItems writes items' decisions, Batch transcripts per transaction. The
// phase is recorded before each transaction, never inside one.
func writeItems(ctx context.Context, store WriteStore, recipeID string, items []*item, batch, maxAccepts int,
	ws *WriteStats, pr pageRecorder) error {
	byTranscript := map[string][]*item{}
	var tids []string
	for _, it := range items {
		t := it.finding.TranscriptID
		if _, ok := byTranscript[t]; !ok {
			tids = append(tids, t)
		}
		byTranscript[t] = append(byTranscript[t], it)
	}
	sort.Strings(tids)

	nBatches := (len(tids) + batch - 1) / batch
	for start := 0; start < len(tids); start += batch {
		pr.batch(start/batch+1, nBatches)
		var fresh, recheck []db.DecisionEvent
		recheckHold := map[string]bool{}
		for _, t := range tids[start:min(start+batch, len(tids))] {
			for _, it := range byTranscript[t] {
				e, ok := eventOf(recipeID, it.outcome)
				if !ok {
					ws.Reanchor++
					continue
				}
				if it.finding.PatchState == patch.StateProposed {
					fresh = append(fresh, e)
				} else {
					recheck = append(recheck, e)
					recheckHold[e.FindingID] = e.Outcome == db.OutcomeHold
				}
			}
		}
		if maxAccepts > 0 {
			fresh = capAccepts(fresh, maxAccepts-ws.Accepted, ws)
		}
		if len(fresh) > 0 {
			res, err := store.ApplyDecisions(ctx, recipeID, fresh)
			if err != nil {
				return err
			}
			ws.Accepted += len(res.Accepted)
			ws.Rejected += len(res.Rejected)
			ws.Held += len(res.Held)
			ws.Skipped += len(res.Skipped)
		}
		if len(recheck) > 0 {
			res, err := store.ApplyRecheckDecisions(ctx, recipeID, recheck)
			if err != nil {
				return err
			}
			ws.Rejected += len(res.Rejected)
			ws.Reverted += len(res.Reverted)
			ws.Kept += len(res.Kept)
			ws.Skipped += len(res.Skipped)
			for _, id := range res.Kept {
				if recheckHold[id] {
					ws.KeptHeld++
				}
			}
		}
	}
	if ws.Capped > 0 {
		ws.Stopped = "max-accepts"
	}
	return nil
}

// capAccepts drops apply decisions past room, counting them in ws.Capped.
// Dropped findings stay undecided (no event), so the next run decides them.
func capAccepts(evs []db.DecisionEvent, room int, ws *WriteStats) []db.DecisionEvent {
	out := evs[:0:0]
	for _, e := range evs {
		if e.Outcome == db.OutcomeApply {
			if room <= 0 {
				ws.Capped++
				continue
			}
			room--
		}
		out = append(out, e)
	}
	return out
}

func (w *WriteStats) print(p func(string, ...any)) {
	p("\nwritten: accepted %d · rejected %d · held %d · re-check kept %d (held for review %d) · re-check reverted %d\n",
		w.Accepted, w.Rejected, w.Held, w.Kept, w.KeptHeld, w.Reverted)
	p("not written: skipped %d (locked, moved or chunk changed; decided next run) · reanchor needed %d · over --max-accepts %d\n",
		w.Skipped, w.Reanchor, w.Capped)
	if w.Stopped != "" {
		p("stopped early: %s reached — run again to continue\n", w.Stopped)
	}
}
