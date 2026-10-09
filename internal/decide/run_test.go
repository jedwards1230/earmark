package decide

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/systemone"
)

var update = flag.Bool("update", false, "rewrite golden files")

// fakeRunStore serves a fixed backlog. Its only writes are fn.Store's, which
// it counts: a dry run must leave nothing else behind.
type fakeRunStore struct {
	memStore
	sample   []db.DecideFinding
	backlog  map[string]int
	chunks   map[db.ChunkKey]*db.DecideChunk
	segments map[string][]db.Segment
	records  map[string]db.BookRecord

	mu      sync.Mutex
	recipes []recipe.Recipe
	scopes  []db.DecideScope
}

func (s *fakeRunStore) RegisterRecipe(ctx context.Context, r recipe.Recipe) (string, error) {
	s.mu.Lock()
	s.recipes = append(s.recipes, r)
	s.mu.Unlock()
	return s.memStore.RegisterRecipe(ctx, r)
}

func (s *fakeRunStore) DecideSample(_ context.Context, sc db.DecideScope) ([]db.DecideFinding, error) {
	s.scopes = append(s.scopes, sc)
	return s.sample, nil
}

func (s *fakeRunStore) DecideBacklog(context.Context, db.DecideScope) (map[string]int, error) {
	return s.backlog, nil
}

func (s *fakeRunStore) DecideChunks(_ context.Context, keys []db.ChunkKey) (map[db.ChunkKey]*db.DecideChunk, error) {
	if len(keys) > db.MaxDecideChunkBatch {
		return nil, errors.New("over cap")
	}
	out := map[db.ChunkKey]*db.DecideChunk{}
	for _, k := range keys {
		if c, ok := s.chunks[k]; ok {
			out[k] = c
		}
	}
	return out, nil
}

func (s *fakeRunStore) GetTranscriptSegments(_ context.Context, ids []string) (map[string][]db.Segment, error) {
	if len(ids) > db.MaxDecideTranscriptBatch {
		return nil, errors.New("over cap")
	}
	out := map[string][]db.Segment{}
	for _, id := range ids {
		if v, ok := s.segments[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func (s *fakeRunStore) GetBookRecords(_ context.Context, dirs []string) (map[string]db.BookRecord, error) {
	out := map[string]db.BookRecord{}
	for _, d := range dirs {
		if v, ok := s.records[d]; ok {
			out[d] = v
		}
	}
	return out, nil
}

// fakeAsker answers should_apply with p chosen by a marker in the state.
type fakeAsker struct {
	mu    sync.Mutex
	calls int
	p     func(state string) (float64, error)
}

func (a *fakeAsker) Decide(_ context.Context, req systemone.Request) (*systemone.Response, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	p, err := a.p(req.State)
	if err != nil {
		return nil, err
	}
	return &systemone.Response{
		Model:   ShouldApplyModel,
		Answers: map[string]systemone.Answer{questionKey: {Type: systemone.TypeNoul, Noul: &p}},
		Usage:   systemone.Usage{InputTokens: 400, OutputTokens: 3},
		CostUSD: 400 * systemone.DefaultUSDPerMTokIn / 1e6,
	}, nil
}

const (
	runT1   = "00000000-0000-0000-0000-0000000000a1"
	runT2   = "00000000-0000-0000-0000-0000000000a2"
	runText = "the dish at auto sebo picked up the signal and the the cat sat"
	libText = "he walked to the store with too forty dollars"
)

func finding(id, tid, path, issue, chunk, original, replacement string, idx int, conf float64) db.DecideFinding {
	occ := patch.Occurrences(chunk, original)
	off := -1
	if len(occ) > 0 {
		off = occ[0]
	}
	return db.DecideFinding{
		ID: id, TranscriptID: tid, FilePath: path, IssueType: issue, Original: original, Replacement: replacement,
		Confidence: conf, ChunkIndex: idx, AnchorOffset: off, AnchorOccurrence: 0,
		ChunkTextSHA256: patch.ChunkHash(chunk), PatchState: patch.StateProposed,
	}
}

// runFixture is a backlog exercising every outcome class.
func runFixture() *fakeRunStore {
	phm, lib := "/b/PHM/01.m4b", "/b/Libro/01.m4b"
	stale := finding("f-stale", runT1, phm, IssueMisheardWord, runText, "signal", "signals", 1, 0.9)
	stale.ChunkTextSHA256 = strings.Repeat("0", 64)
	sample := []db.DecideFinding{
		finding("f-apply", runT1, phm, IssueMisheardProperNoun, runText, "auto sebo", "Arecibo", 0, 0.9),
		finding("f-repeat", runT1, phm, IssueRepeatedText, runText, "the the cat", "the cat", 0, 0.8),
		finding("f-notsound", runT1, phm, IssueMisheardWord, runText, "dish", "telescope", 0, 0.7),
		stale,
		finding("f-lowp", runT2, lib, IssueMisheardWord, libText, "dollars", "dolors", 0, 0.6),
		finding("f-capped", runT2, lib, IssueNumberArtifact, libText, "too forty", "240", 0, 0.9),
		finding("f-outage", runT2, lib, IssueHomophone, libText, "to the", "two the", 0, 0.5),
	}
	// An unsampled proposed competitor on f-dup's span with a higher judge
	// confidence wins dedupe: f-dup fails overlap_dup, as in a full run.
	comp := finding("c-overlap", runT2, lib, IssueMisheardWord, libText, "walked", "worked", 0, 0.95)
	sample = append(sample, finding("f-dup", runT2, lib, IssueMisheardWord, libText, "walked", "woked", 0, 0.5))
	chunk := func(tid string, idx int, text string, start, end float64, comps ...db.DecideFinding) *db.DecideChunk {
		return &db.DecideChunk{Key: db.ChunkKey{TranscriptID: tid, ChunkIndex: idx}, Text: text, StartSec: start, EndSec: end, Competitors: comps}
	}
	return &fakeRunStore{
		sample:  sample,
		backlog: map[string]int{IssueMisheardProperNoun: 100, IssueMisheardWord: 400, IssueRepeatedText: 50, IssueNumberArtifact: 20, IssueHomophone: 30, IssueDroppedWord: 7},
		chunks: map[db.ChunkKey]*db.DecideChunk{
			{TranscriptID: runT1, ChunkIndex: 0}: chunk(runT1, 0, runText, 0, 6),
			{TranscriptID: runT1, ChunkIndex: 1}: chunk(runT1, 1, "the signal was strong", 6, 8),
			{TranscriptID: runT2, ChunkIndex: 0}: chunk(runT2, 0, libText, 0, 4, comp),
		},
		segments: map[string][]db.Segment{
			runT1: {seg(0, 3, "the dish at auto sebo picked up the signal"), seg(3, 6, "and the the cat sat"), seg(6, 8, "the signal was strong")},
		},
		records: map[string]db.BookRecord{"/b/PHM": *hailMary()},
	}
}

func runAsker() *fakeAsker {
	return &fakeAsker{p: func(state string) (float64, error) {
		switch {
		case strings.Contains(state, "[[Arecibo]]"), strings.Contains(state, "[[the cat]]"), strings.Contains(state, "[[240]]"):
			return 0.97, nil
		case strings.Contains(state, "[[dolors]]"):
			return 0.05, nil
		default:
			return 0, &systemone.ServerError{Code: 503}
		}
	}}
}

func TestDryRun(t *testing.T) {
	store, asker := runFixture(), runAsker()
	rep, err := DryRun(context.Background(), store, asker, RunOptions{
		Scope:       db.DecideScope{Sample: 50, Seed: "s"},
		Concurrency: 3,
		Params:      DefaultShouldApplyParams(),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{DecisionApply: 2, DecisionHold: 2, DecisionReject: 3, ClassReanchor: 1}
	for k, v := range want {
		if rep.Outcomes[k] != v {
			t.Errorf("outcome %s = %d, want %d (all %v)", k, rep.Outcomes[k], v, rep.Outcomes)
		}
	}
	if rep.Rung0Rejects[ReasonNotSoundAlike] != 1 || rep.Rung0Rejects[ReasonOverlapDup] != 1 || rep.Reanchor[ReasonChunkChanged] != 1 {
		t.Errorf("rung-0 %v reanchor %v", rep.Rung0Rejects, rep.Reanchor)
	}
	if rep.Reasons[ReasonConfident] != 2 || rep.Reasons[ReasonTypeCapped] != 1 || rep.Reasons[ReasonJevUnavailable] != 1 || rep.Reasons[ReasonJevReject] != 1 {
		t.Errorf("reasons %v", rep.Reasons)
	}
	if j := rep.Jev; j.Asked != 5 || j.Answered != 4 || j.Unavailable != 1 || j.Errors["503"] != 1 || j.InputTokens != 1600 {
		t.Errorf("jev %+v", j)
	}
	if asker.calls != 5 {
		t.Errorf("%d model calls, want 5 (rung-0 failures make none)", asker.calls)
	}

	// The dry run's only writes: decide recipe registrations and one
	// fn_calls row per model call.
	for _, r := range store.recipes {
		if r.Step != recipe.StepDecide {
			t.Errorf("registered a %s recipe", r.Step)
		}
	}
	if len(store.rows) != 5 {
		t.Errorf("%d fn_calls rows, want 5", len(store.rows))
	}

	// A second run is served from fn_calls: no new model call for the
	// answered findings (the outage is not cached and is asked again).
	rep2, err := DryRun(context.Background(), store, asker, RunOptions{
		Scope: db.DecideScope{Sample: 50, Seed: "s"}, Concurrency: 3, Params: DefaultShouldApplyParams(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if asker.calls != 6 || rep2.Jev.CacheHits != 4 || rep2.Outcomes[DecisionApply] != 2 {
		t.Errorf("rerun: calls %d, cache hits %d, outcomes %v", asker.calls, rep2.Jev.CacheHits, rep2.Outcomes)
	}

	// Projection: per-issue rates × backlog.
	if rep.Projection.Backlog != 607 || rep.Projection.Unprojected != 7 || rep.Projection.CostUSD == nil {
		t.Errorf("projection %+v", rep.Projection)
	}
}

func TestDryRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store, asker := runFixture(), runAsker()
	if _, err := DryRun(ctx, store, asker, RunOptions{Scope: db.DecideScope{Sample: 5, Seed: "s"}, Concurrency: 1, Params: DefaultShouldApplyParams()}); err == nil {
		t.Error("cancelled run returned no error")
	}
	if asker.calls != 0 {
		t.Errorf("cancelled run made %d calls", asker.calls)
	}
}

func TestReportGolden(t *testing.T) {
	p := func(f float64) *float64 { return &f }
	mk := func(issue, decision, reason, evidence string, pass bool, prob *float64, cached bool, lat time.Duration, class string) *item {
		o := Outcome{Decision: decision, Reason: reason, Evidence: evidence, Rung0: Verdict{Pass: pass}, P: prob,
			Asked: pass, CacheHit: cached, Latency: lat, ErrorClass: class}
		if pass && !cached && class == "" {
			o.InputTokens, o.OutputTokens, o.CostUSD = 400, 3, 0.0000168
		}
		return &item{finding: db.DecideFinding{IssueType: issue}, outcome: o}
	}
	items := []*item{
		mk(IssueMisheardProperNoun, DecisionApply, ReasonConfident, EvidenceASINVerbatim, true, p(0.97), false, 800*time.Millisecond, ""),
		mk(IssueMisheardProperNoun, DecisionHold, ReasonNoEvidence, EvidenceNone, true, p(0.99), true, time.Millisecond, ""),
		mk(IssueRepeatedText, DecisionApply, ReasonConfident, EvidenceExactRepeat, true, p(0.96), false, 1200*time.Millisecond, ""),
		mk(IssueMisheardWord, DecisionReject, ReasonJevReject, EvidenceNone, true, p(0.05), false, 900*time.Millisecond, ""),
		mk(IssueMisheardWord, DecisionReject, ReasonNotSoundAlike, EvidenceNone, false, nil, false, 0, ""),
		mk(IssueMisheardWord, DecisionReject, ReasonAnchorMissing, EvidenceNone, false, nil, false, 0, ""),
		mk(IssueHomophone, DecisionHold, ReasonJevUnavailable, EvidenceNone, true, nil, false, 0, "timeout"),
		mk(IssueNumberArtifact, DecisionHold, ReasonTypeCapped, EvidenceNone, true, p(0.98), false, 1000*time.Millisecond, ""),
	}
	rep := buildReport(RunOptions{Scope: db.DecideScope{Sample: 10, Seed: "golden"}}, strings.Repeat("ab", 32), items,
		map[string]int{IssueMisheardProperNoun: 200, IssueMisheardWord: 600, IssueRepeatedText: 50, IssueHomophone: 40, IssueNumberArtifact: 30, IssueDroppedWord: 9})

	wrep := buildReport(RunOptions{Scope: db.DecideScope{Sample: 2000}}, strings.Repeat("ab", 32), items, nil)
	wrep.DryRun = false
	wrep.Write = &WriteStats{Accepted: 2, Rejected: 2, Held: 2, Kept: 3, KeptHeld: 1, Reverted: 1, Skipped: 1, Reanchor: 1, Capped: 4, Stopped: "max-accepts"}
	for name, c := range map[string]struct {
		rep    *Report
		asJSON bool
	}{"report.golden": {rep, false}, "report.json.golden": {rep, true}, "report.write.golden": {wrep, false}} {
		rep, asJSON := c.rep, c.asJSON
		var buf bytes.Buffer
		if err := rep.Print(&buf, asJSON); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", name)
		if *update {
			if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update)", err)
		}
		if buf.String() != string(want) {
			t.Errorf("%s mismatch:\n%s\n--- want ---\n%s", name, buf.String(), want)
		}
	}
}

func TestPercentile(t *testing.T) {
	ds := []time.Duration{5 * time.Millisecond, 1 * time.Millisecond, 3 * time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	if p50, p95 := percentileMS(ds, 0.5), percentileMS(ds, 0.95); *p50 != 3 || *p95 != 5 {
		t.Errorf("p50 %d p95 %d", *p50, *p95)
	}
	if percentileMS(nil, 0.5) != nil {
		t.Error("empty percentile not nil")
	}
	if !slices.Equal(ds, []time.Duration{5 * time.Millisecond, 1 * time.Millisecond, 3 * time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}) {
		t.Error("percentile sorted its input")
	}
}

func TestClientFromConfig(t *testing.T) {
	ep := func(model string) *config.Config {
		return &config.Config{
			AIEndpoints: []config.AIEndpoint{{ID: "jev", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://gw:4000/typesafe", Model: model}},
			AIRoles:     &config.AIRoles{Decide: "jev"},
		}
	}
	if _, ok, err := ClientFromConfig(&config.Config{}); ok || err != nil {
		t.Errorf("unbound role: ok=%v err=%v", ok, err)
	}
	if _, ok, err := ClientFromConfig(ep("jev-1.12.0")); !ok || err == nil {
		t.Errorf("wrong model accepted: ok=%v err=%v", ok, err)
	}
	if c, ok, err := ClientFromConfig(ep("typesafe/jev-1.13.0")); !ok || err != nil || c == nil {
		t.Errorf("pinned model refused: ok=%v err=%v", ok, err)
	}
}

type failingSegments struct{ *fakeRunStore }

func (failingSegments) GetTranscriptSegments(context.Context, []string) (map[string][]db.Segment, error) {
	return nil, errors.New("boom")
}

// A store error stops the run, names the load that failed, and no model
// call is made.
func TestDryRunStoreError(t *testing.T) {
	asker := runAsker()
	_, err := DryRun(context.Background(), failingSegments{runFixture()}, asker,
		RunOptions{Scope: db.DecideScope{Sample: 5, Seed: "s"}, Concurrency: 1, Params: DefaultShouldApplyParams()})
	if err == nil || !strings.Contains(err.Error(), "load segments of 2 transcripts: boom") {
		t.Errorf("err = %v", err)
	}
	if asker.calls != 0 {
		t.Errorf("%d calls after a store error", asker.calls)
	}
}
