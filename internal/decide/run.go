package decide

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/fn"
	"github.com/jedwards1230/earmark/internal/genai"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/systemone"
)

// RunStore is the slice of the database a decide dry run needs (*db.DB
// implements it). Apart from fn.Store's RegisterRecipe and InsertFnCall —
// the recipe row and the model-call log the later full run reuses — every
// method is a read.
type RunStore interface {
	fn.Store
	DecideSample(ctx context.Context, s db.DecideScope) ([]db.DecideFinding, error)
	DecideBacklog(ctx context.Context, s db.DecideScope) (map[string]int, error)
	DecideChunks(ctx context.Context, keys []db.ChunkKey) (map[db.ChunkKey]*db.DecideChunk, error)
	GetTranscriptSegments(ctx context.Context, ids []string) (map[string][]db.Segment, error)
	GetBookRecords(ctx context.Context, bookDirs []string) (map[string]db.BookRecord, error)
}

// DefaultSweep is the phonetic_min_sim values calibration re-runs rung 0 at.
var DefaultSweep = []float64{0.55, 0.60, 0.67, 0.75, 0.85}

// RunOptions configures a dry run.
type RunOptions struct {
	Scope       db.DecideScope
	Concurrency int
	Params      ShouldApplyParams
	// Sweep is the thresholds calibration re-runs rung 0 at (nil = DefaultSweep).
	Sweep []float64
}

// ClientFromConfig builds the System One client for AI_ROLES.decide. ok is
// false when the role is unbound. The endpoint must ask for the model
// should_apply is pinned to: the recipe names that model, so another one would
// make every answer a fallback.
func ClientFromConfig(cfg *config.Config) (client *systemone.Client, ok bool, err error) {
	ep, ok := cfg.DecideEndpoint()
	if !ok {
		return nil, false, nil
	}
	if !genai.SameModel(ep.Model, ShouldApplyModel) {
		return nil, true, fmt.Errorf("decide endpoint %q asks for model %q; should_apply is pinned to %s", ep.ID, ep.Model, ShouldApplyModel)
	}
	var opts []systemone.Option
	if p, set := cfg.ModelPin(recipe.StepDecide).FloatParam(config.ParamUSDPerMTokIn); set {
		opts = append(opts, systemone.WithUSDPerMTokIn(p))
	}
	client, err = systemone.New(ep.BaseURL, ep.APIKey, opts...)
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

// item is one sampled finding with everything Evaluate needs, kept for the
// calibration sweep.
type item struct {
	finding   db.DecideFinding
	input     Input
	chunk     *db.DecideChunk // nil when the chunk is gone
	outcome   Outcome
	sentences []RecordSentence
}

// DryRun samples findings, decides each — rung 0, evidence, should_apply —
// and reports what a full run would do. It writes nothing but the decide
// recipe row and the fn_calls rows the model calls leave (so a later full run
// is served from cache): no decision events, no state changes.
func DryRun(ctx context.Context, store RunStore, asker Asker, o RunOptions) (*Report, error) {
	if o.Concurrency < 1 {
		return nil, errors.New("decide: concurrency must be >= 1")
	}
	ev, err := NewEvaluator(store, asker, o.Params)
	if err != nil {
		return nil, err
	}
	recipeID, err := store.RegisterRecipe(ctx, ev.Recipe())
	if err != nil {
		return nil, fmt.Errorf("register decide recipe: %w", err)
	}
	sample, err := store.DecideSample(ctx, o.Scope)
	if err != nil {
		return nil, err
	}
	backlog, err := store.DecideBacklog(ctx, o.Scope)
	if err != nil {
		return nil, err
	}
	items, err := prepare(ctx, store, sample, o.Params)
	if err != nil {
		return nil, err
	}
	evaluateAll(ctx, ev, items, o.Concurrency)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rep := buildReport(o, recipeID, items, backlog)
	if o.Scope.Calibrate {
		sweep := o.Sweep
		if sweep == nil {
			sweep = DefaultSweep
		}
		rep.Calibration = calibrate(items, o.Params, sweep)
	}
	return rep, nil
}

// prepare groups the sample by transcript, loads chunks, segments and book
// records in bounded batches, and runs rung 0 per chunk (Check + Dedupe over
// the chunk's sampled findings, its other proposed findings, and its
// accepted/applied overlay).
func prepare(ctx context.Context, store RunStore, sample []db.DecideFinding, p ShouldApplyParams) ([]*item, error) {
	byTranscript := map[string][]db.DecideFinding{}
	for _, f := range sample {
		byTranscript[f.TranscriptID] = append(byTranscript[f.TranscriptID], f)
	}
	tids := make([]string, 0, len(byTranscript))
	for t := range byTranscript {
		tids = append(tids, t)
	}
	sort.Strings(tids)

	var items []*item
	for start := 0; start < len(tids); start += db.MaxDecideTranscriptBatch {
		batch := tids[start:min(start+db.MaxDecideTranscriptBatch, len(tids))]
		segs, err := store.GetTranscriptSegments(ctx, batch)
		if err != nil {
			return nil, err
		}
		var keys []db.ChunkKey
		dirSet := map[string]bool{}
		for _, t := range batch {
			for _, f := range byTranscript[t] {
				k := db.ChunkKey{TranscriptID: f.TranscriptID, ChunkIndex: f.ChunkIndex}
				if !slices.Contains(keys, k) {
					keys = append(keys, k)
				}
				dirSet[filepath.Dir(f.FilePath)] = true
			}
		}
		chunks := map[db.ChunkKey]*db.DecideChunk{}
		for k0 := 0; k0 < len(keys); k0 += db.MaxDecideChunkBatch {
			part, err := store.DecideChunks(ctx, keys[k0:min(k0+db.MaxDecideChunkBatch, len(keys))])
			if err != nil {
				return nil, err
			}
			for k, c := range part {
				chunks[k] = c
			}
		}
		dirs := make([]string, 0, len(dirSet))
		for d := range dirSet {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		records := map[string]db.BookRecord{}
		for d0 := 0; d0 < len(dirs); d0 += db.MaxDecideBookBatch {
			part, err := store.GetBookRecords(ctx, dirs[d0:min(d0+db.MaxDecideBookBatch, len(dirs))])
			if err != nil {
				return nil, err
			}
			for k, r := range part {
				records[k] = r
			}
		}

		for _, t := range batch {
			items = append(items, prepareTranscript(byTranscript[t], chunks, segs[t], records, p)...)
		}
	}
	return items, nil
}

func prepareTranscript(fs []db.DecideFinding, chunks map[db.ChunkKey]*db.DecideChunk,
	segs []db.Segment, records map[string]db.BookRecord, p ShouldApplyParams) []*item {
	byChunk := map[db.ChunkKey][]db.DecideFinding{}
	var order []db.ChunkKey
	for _, f := range fs {
		k := db.ChunkKey{TranscriptID: f.TranscriptID, ChunkIndex: f.ChunkIndex}
		if _, seen := byChunk[k]; !seen {
			order = append(order, k)
		}
		byChunk[k] = append(byChunk[k], f)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].ChunkIndex < order[j].ChunkIndex })

	var out []*item
	for _, k := range order {
		chunk := chunks[k]
		verdicts := chunkVerdicts(byChunk[k], chunk, p.PhoneticMinSim)
		for _, f := range byChunk[k] {
			var rec *db.BookRecord
			if r, ok := records[filepath.Dir(f.FilePath)]; ok {
				rec = &r
			}
			it := &item{finding: f, chunk: chunk, sentences: RecordSentences(rec)}
			v := verdicts[f.ID]
			it.input = Input{Candidate: candidateOf(f, chunk), Verdict: &v, Segments: segs, Record: rec}
			if chunk != nil {
				it.input.Chunk = ChunkWindow{StartSec: chunk.StartSec, EndSec: chunk.EndSec}
			}
			out = append(out, it)
		}
	}
	return out
}

// chunkVerdicts runs rung 0 for the sampled findings of one chunk, with the
// chunk's other proposed findings as dedupe competitors. A missing chunk
// fails every finding chunk_changed.
func chunkVerdicts(sampled []db.DecideFinding, chunk *db.DecideChunk, threshold float64) map[string]Verdict {
	out := make(map[string]Verdict, len(sampled))
	if chunk == nil {
		for _, f := range sampled {
			out[f.ID] = Verdict{Reason: ReasonChunkChanged, Evidence: "chunk no longer exists"}
		}
		return out
	}
	cands := make([]Candidate, 0, len(sampled)+len(chunk.Competitors))
	seen := map[string]bool{}
	for _, f := range sampled {
		cands = append(cands, candidateOf(f, chunk))
		seen[f.ID] = true
	}
	for _, f := range chunk.Competitors {
		if !seen[f.ID] {
			cands = append(cands, candidateOf(f, chunk))
		}
	}
	verdicts := Rung0(cands, chunk.Overlay, Params{SoundAlikeThreshold: threshold})
	for i, c := range cands {
		if seen[c.FindingID] {
			out[c.FindingID] = verdicts[i]
		}
	}
	return out
}

func candidateOf(f db.DecideFinding, chunk *db.DecideChunk) Candidate {
	c := Candidate{
		FindingID:   f.ID,
		IssueType:   f.IssueType,
		Original:    f.Original,
		Replacement: f.Replacement,
		Confidence:  f.Confidence,
		Anchor:      patch.Anchor{OriginalText: f.Original, Offset: f.AnchorOffset, Occurrence: f.AnchorOccurrence},
		ChunkHash:   f.ChunkTextSHA256,
	}
	if chunk != nil {
		c.ChunkText = chunk.Text
	}
	return c
}

// evaluateAll runs Evaluate over items with at most n calls in flight.
// Rung-0 failures return without a call, so they never wait for a slot.
func evaluateAll(ctx context.Context, ev *Evaluator, items []*item, n int) {
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, it := range items {
		if !it.input.Verdict.Pass {
			it.outcome = ev.Evaluate(ctx, it.input)
			continue
		}
		if ctx.Err() != nil {
			continue // DryRun returns ctx.Err(); no call is made
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			continue // DryRun returns ctx.Err(); no call is made
		}
		wg.Add(1)
		go func(it *item) {
			defer wg.Done()
			defer func() { <-sem }()
			it.outcome = ev.Evaluate(ctx, it.input)
		}(it)
	}
	wg.Wait()
}
