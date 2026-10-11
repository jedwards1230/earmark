package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// fakeRejudgeDB is an in-memory rejudgeDB: n transcripts of k chunks each.
type fakeRejudgeDB struct {
	mu       sync.Mutex
	targets  []db.RejudgeTarget
	chunks   map[string][]db.EvalChunk
	opened   []db.JudgePassOpen
	commits  []db.JudgePassCommit
	selected int
}

func newFakeRejudgeDB(n, k int) *fakeRejudgeDB {
	f := &fakeRejudgeDB{chunks: map[string][]db.EvalChunk{}}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		path := fmt.Sprintf("/books/Book%d/01.m4b", i)
		f.targets = append(f.targets, db.RejudgeTarget{TranscriptID: id, JobID: "j" + id, FilePath: path,
			CreatedAt: base.Add(time.Duration(i) * time.Second)})
		for c := range k {
			f.chunks[id] = append(f.chunks[id], db.EvalChunk{ChunkID: fmt.Sprintf("%s-c%d", id, c), TranscriptID: id,
				FilePath: path, ChunkIndex: c, Text: fmt.Sprintf("chunk %d of transcript %d", c, i)})
		}
	}
	return f
}

func (f *fakeRejudgeDB) RejudgeTargets(_ context.Context, q db.RejudgeQuery) ([]db.RejudgeTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selected++
	var out []db.RejudgeTarget
	for _, t := range f.targets {
		if q.After.ID != "" && !t.CreatedAt.After(q.After.CreatedAt) {
			continue
		}
		out = append(out, t)
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeRejudgeDB) GetEvalChunksForTranscript(_ context.Context, id string) ([]db.EvalChunk, error) {
	return f.chunks[id], nil
}

func (f *fakeRejudgeDB) GetRecipeFindingKeys(context.Context, string, string) (map[db.FindingKey]bool, error) {
	return nil, nil
}

func (f *fakeRejudgeDB) RegisterRecipe(_ context.Context, r recipe.Recipe) (string, error) {
	return r.ID()
}

func (f *fakeRejudgeDB) RecipesEquivalent(_ context.Context, a, b string) (bool, error) {
	return a == b, nil
}

func (f *fakeRejudgeDB) OpenJudgePass(_ context.Context, p db.JudgePassOpen) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, p)
	return int64(len(f.opened)), nil
}

func (f *fakeRejudgeDB) CommitJudgePass(_ context.Context, c db.JudgePassCommit) (db.JudgePassResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, c)
	return db.JudgePassResult{Inserted: len(c.Findings)}, nil
}

// gaugeChat counts calls and the peak number in flight (atomic max).
type gaugeChat struct {
	inflight, peak, calls atomic.Int32
	delay                 time.Duration
	resp                  func(user string) string
}

func (g *gaugeChat) Complete(ctx context.Context, _, user string) (string, error) {
	n := g.inflight.Add(1)
	defer g.inflight.Add(-1)
	g.calls.Add(1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-time.After(g.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if g.resp != nil {
		return g.resp(user), nil
	}
	return `{"findings":[]}`, nil
}
func (g *gaugeChat) Model() string { return "gauge-judge" }

// TestRejudge_ExpectRecipeRefusal: --expect-recipe naming another recipe
// refuses before any model call, pass or step run.
func TestRejudge_ExpectRecipeRefusal(t *testing.T) {
	fdb := newFakeRejudgeDB(2, 2)
	chat := &gaugeChat{}
	var out strings.Builder
	err := runRejudge(context.Background(), &out, fdb, evalpkg.NewJudge(chat),
		rejudgeOptions{write: true, concurrency: 4, expectRecipe: strings.Repeat("ab", 32)})
	if !errors.Is(err, ErrRecipeMismatch) {
		t.Fatalf("want ErrRecipeMismatch, got %v", err)
	}
	if chat.calls.Load() != 0 || len(fdb.opened) != 0 || fdb.selected != 0 {
		t.Errorf("refusal still worked: %d calls, %d passes, %d selections", chat.calls.Load(), len(fdb.opened), fdb.selected)
	}

	// The current recipe passes the check.
	j := evalpkg.NewJudge(chat)
	id, _ := j.Recipe().ID()
	if err := runRejudge(context.Background(), &out, fdb, j,
		rejudgeOptions{write: true, concurrency: 4, expectRecipe: id}); err != nil {
		t.Fatalf("matching --expect-recipe refused: %v", err)
	}
}

// TestRejudge_ConcurrencyBoundSharedAcrossTranscripts: one bound over every
// transcript's chunks — never exceeded, and reached even though no single
// transcript has that many chunks (so it is not per-transcript).
func TestRejudge_ConcurrencyBoundSharedAcrossTranscripts(t *testing.T) {
	fdb := newFakeRejudgeDB(8, 3)
	chat := &gaugeChat{delay: 25 * time.Millisecond}
	var out strings.Builder
	if err := runRejudge(context.Background(), &out, fdb, evalpkg.NewJudge(chat),
		rejudgeOptions{write: true, concurrency: 5, pageSize: 3}); err != nil {
		t.Fatal(err)
	}
	if p := chat.peak.Load(); p > 5 {
		t.Errorf("peak in-flight %d exceeds --concurrency 5", p)
	} else if p <= 3 {
		t.Errorf("peak in-flight %d: the bound is not shared across transcripts (3 chunks each)", p)
	}
	if chat.calls.Load() != 24 || len(fdb.commits) != 8 {
		t.Errorf("%d calls, %d commits; want 24, 8", chat.calls.Load(), len(fdb.commits))
	}
	for _, c := range fdb.commits {
		if c.Close.Status != db.JudgePassComplete || c.Metrics == nil || c.Metrics.Failed() {
			t.Errorf("commit %+v: want complete with the run_metrics latch", c.Close)
		}
	}
}

// TestRejudge_DryRunWritesNoPass: without --write no pass is opened or
// committed, and the output says the calls are still paid.
func TestRejudge_DryRunWritesNoPass(t *testing.T) {
	fdb := newFakeRejudgeDB(3, 2)
	chat := &gaugeChat{}
	var out strings.Builder
	if err := runRejudge(context.Background(), &out, fdb, evalpkg.NewJudge(chat),
		rejudgeOptions{concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if len(fdb.opened) != 0 || len(fdb.commits) != 0 {
		t.Errorf("dry run wrote %d passes, %d commits", len(fdb.opened), len(fdb.commits))
	}
	if chat.calls.Load() != 6 {
		t.Errorf("%d calls, want 6", chat.calls.Load())
	}
	if s := out.String(); !strings.Contains(s, "PAID model calls") || !strings.Contains(s, "would be complete") {
		t.Errorf("dry-run output:\n%s", s)
	}
}

// TestRejudge_InvalidReplyFailsThePass: a chunk whose reply does not parse
// FAILS — the pass is partial, never complete, and the latch is not written.
func TestRejudge_InvalidReplyFailsThePass(t *testing.T) {
	fdb := newFakeRejudgeDB(1, 3)
	chat := &gaugeChat{resp: func(user string) string {
		if strings.Contains(user, "chunk 1 of") {
			return "sorry, no JSON today"
		}
		return `{"findings":[]}`
	}}
	var out strings.Builder
	err := runRejudge(context.Background(), &out, fdb, evalpkg.NewJudge(chat), rejudgeOptions{write: true, concurrency: 2})
	if err == nil {
		t.Fatal("a run that completed nothing must fail")
	}
	if len(fdb.commits) != 1 {
		t.Fatalf("%d commits", len(fdb.commits))
	}
	c := fdb.commits[0]
	if c.Close.Status != db.JudgePassPartial || c.Close.ChunksFailed != 1 || c.Close.ChunksJudged != 2 ||
		!strings.Contains(c.Close.Error, "not valid findings JSON") {
		t.Errorf("close %+v: want partial, 1 failed", c.Close)
	}
	if c.Metrics == nil || !c.Metrics.Failed() {
		t.Errorf("metrics %+v: a partial pass must record a failure, not the latch", c.Metrics)
	}
}

// TestRejudge_LimitAndStop: --limit bounds the transcripts attempted; a
// cancelled run closes its in-flight passes without the latch.
func TestRejudge_LimitAndStop(t *testing.T) {
	fdb := newFakeRejudgeDB(5, 1)
	var out strings.Builder
	if err := runRejudge(context.Background(), &out, fdb, evalpkg.NewJudge(&gaugeChat{}),
		rejudgeOptions{write: true, concurrency: 2, limit: 2}); err != nil {
		t.Fatal(err)
	}
	if len(fdb.opened) != 2 {
		t.Errorf("--limit 2 opened %d passes", len(fdb.opened))
	}

	fdb = newFakeRejudgeDB(2, 4)
	ctx, cancel := context.WithCancel(context.Background())
	chat := &gaugeChat{delay: time.Second}
	go func() {
		for chat.calls.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	err := runRejudge(ctx, &out, fdb, evalpkg.NewJudge(chat), rejudgeOptions{write: true, concurrency: 2})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want a cancelled run, got %v", err)
	}
	if len(fdb.commits) != len(fdb.opened) || len(fdb.commits) == 0 {
		t.Fatalf("%d opened, %d closed: every opened pass must be closed on a stop", len(fdb.opened), len(fdb.commits))
	}
	for _, c := range fdb.commits {
		if c.Close.Status == db.JudgePassComplete || c.Metrics != nil {
			t.Errorf("stopped pass %+v (metrics %v): never complete, latch untouched", c.Close, c.Metrics)
		}
	}
}

func TestParseShard(t *testing.T) {
	for in, want := range map[string][2]int{"": {0, 0}, "0/1": {0, 1}, "3/4": {3, 4}} {
		s, n, err := parseShard(in)
		if err != nil || s != want[0] || n != want[1] {
			t.Errorf("parseShard(%q) = %d/%d, %v", in, s, n, err)
		}
	}
	for _, bad := range []string{"4/4", "-1/2", "1/0", "1/2x", "a/b", "01/2"} {
		if _, _, err := parseShard(bad); err == nil {
			t.Errorf("parseShard(%q) accepted", bad)
		}
	}
}

func TestRejudgeOptionsFrom(t *testing.T) {
	if _, err := rejudgeOptionsFrom(options{concurrency: 0}, ""); err == nil {
		t.Error("--concurrency 0 accepted")
	}
	ro, err := rejudgeOptionsFrom(options{concurrency: 8, shard: "1/4", limit: 3, write: true}, "Dune")
	if err != nil || ro.shard != 1 || ro.shards != 4 || ro.book != "Dune" ||
		ro.args != `--rejudge --book "Dune" --shard 1/4 --limit 3 --concurrency 8 --write` {
		t.Errorf("options %+v, %v", ro, err)
	}
	if validateCallFlags(options{callTimeout: 0}) == nil || validateCallFlags(options{callTimeout: time.Second, retries: -1}) == nil {
		t.Error("bad call flags accepted")
	}
}
