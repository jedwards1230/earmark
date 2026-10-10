package scan

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
	scanpkg "github.com/jedwards1230/earmark/internal/scan"
	"github.com/jedwards1230/earmark/internal/systemone"
)

// fakeStore is an in-memory Store: fn_calls with the cache semantics, a fixed
// candidate list (keyset-paged), and a record of every chunk_scan insert.
type fakeStore struct {
	mu      sync.Mutex
	cands   []db.ScanCandidate
	calls   []db.FnCall
	scans   []db.ChunkScan
	scopes  []db.ScanScope
	outcome db.ChunkScanOutcome
}

func (f *fakeStore) RegisterRecipe(_ context.Context, r recipe.Recipe) (string, error) { return r.ID() }

func (f *fakeStore) LookupFnCache(_ context.Context, k db.FnCacheKey, _ string) (*db.FnCall, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.calls {
		if c := f.calls[i]; c.Key() == k && c.ErrorClass == "" && !c.CacheHit {
			return &c, true, nil
		}
	}
	return nil, false, nil
}

func (f *fakeStore) InsertFnCall(_ context.Context, c db.FnCall) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c.ID = int64(len(f.calls) + 1)
	f.calls = append(f.calls, c)
	return c.ID, true, nil
}

func (f *fakeStore) ScanCandidates(_ context.Context, s db.ScanScope) ([]db.ScanCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = append(f.scopes, s)
	start := 0
	if s.After.TranscriptID != "" {
		for i, c := range f.cands {
			if c.Cursor() == s.After {
				start = i + 1
			}
		}
	}
	end := len(f.cands)
	n := s.Limit
	if s.Sample > 0 {
		n = s.Sample
	}
	if n > 0 && start+n < end {
		end = start + n
	}
	return append([]db.ScanCandidate(nil), f.cands[start:end]...), nil
}

func (f *fakeStore) InsertChunkScan(_ context.Context, s db.ChunkScan) (db.ChunkScanOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans = append(f.scans, s)
	if f.outcome != "" {
		return f.outcome, nil
	}
	return db.ChunkScanInserted, nil
}

func (f *fakeStore) successCalls() int {
	n := 0
	for _, c := range f.calls {
		if c.ErrorClass == "" && !c.CacheHit {
			n++
		}
	}
	return n
}

// replyFor answers every request: quality score 3 (quality 4), unless the
// state mentions "garbage" (score 0, garbled) or "broken" (dialogue missing).
func replyFor(state string) string {
	score, garbled, dialogue := "3", "0.05", `"dialogue":{"type":"noul","noul":0.6},`
	switch {
	case strings.Contains(state, "garbage"):
		score, garbled = "0", "0.95"
	case strings.Contains(state, "broken"):
		dialogue = ""
	}
	return `{"model":"jev-1.13.0","answers":{
		"needs_fix":{"type":"noul","noul":0.7},
		"quality":{"type":"score","score":` + score + `,"confidence":0.9},
		"boilerplate":{"type":"noul","noul":0.01},
		"garbled":{"type":"noul","noul":` + garbled + `},` + dialogue + `
		"issue_type":{"type":"choice","choice":"misheard_word","probabilities":{"misheard_word":0.7,"none":0.3}}},
		"usage":{"input_tokens":1000,"output_tokens":50}}`
}

// fakeSystemOne is an httptest System One that counts requests.
func fakeSystemOne(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req systemone.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || r.URL.Path != "/v1/systemone" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(replyFor(req.State)))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newScanner(t *testing.T, url string, store *fakeStore) scanpkg.Scanner {
	t.Helper()
	client, err := systemone.New(url, "key")
	if err != nil {
		t.Fatal(err)
	}
	f, err := scanpkg.NewFn(scanpkg.DefaultModel, "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	return scanpkg.Scanner{Fn: f, Client: client, Store: store}
}

func cands(texts ...string) []db.ScanCandidate {
	out := make([]db.ScanCandidate, len(texts))
	for i, s := range texts {
		out[i] = db.ScanCandidate{TranscriptID: "00000000-0000-0000-0000-000000000001", ChunkIndex: i,
			Text: s, TextSHA256: strings.Repeat("c", 64), ContextBefore: []string{"prev"}}
	}
	return out
}

// TestRunDryRunWritesNoChunkScan: the default run calls the model (and logs
// the calls in fn_calls) but writes nothing to chunk_scan; the --yes run that
// follows is served entirely from the cache and writes every usable answer.
func TestRunDryRunWritesNoChunkScan(t *testing.T) {
	srv, hits := fakeSystemOne(t)
	store := &fakeStore{cands: cands("a clean chunk", "pure garbage here", "a broken reply", "another clean one")}
	sc := newScanner(t, srv.URL, store)

	var out strings.Builder
	o := options{sample: 10, seed: "s", concurrency: 3, context: 2}
	if err := run(context.Background(), &out, store, sc, o); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(store.scans) != 0 {
		t.Fatalf("dry run wrote %d chunk_scan rows", len(store.scans))
	}
	if store.successCalls() != 3 || hits.Load() != 4 {
		t.Errorf("success calls %d, requests %d; want 3 and 4 (one reply is broken)", store.successCalls(), hits.Load())
	}
	got := out.String()
	for _, want := range []string{"(dry-run)", "invalid_reply 1", "mean quality 3.00", "misheard_word 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run report missing %q:\n%s", want, got)
		}
	}
	if s := store.scopes[0]; s.Sample != 10 || s.Seed != "s" || s.ContextSegments != 2 || s.RecipeID == "" {
		t.Errorf("scope = %+v", s)
	}

	before := hits.Load()
	out.Reset()
	o.yes, o.json = true, true
	if err := run(context.Background(), &out, store, sc, o); err != nil {
		t.Fatalf("--yes run: %v", err)
	}
	// Only the broken reply (an error, never cached) is asked again.
	if hits.Load() != before+1 {
		t.Errorf("--yes made %d requests, want 1 (the rest are cached)", hits.Load()-before)
	}
	if len(store.scans) != 3 {
		t.Fatalf("wrote %d rows, want 3", len(store.scans))
	}
	var rep Report
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatalf("--json output: %v\n%s", err, out.String())
	}
	if rep.DryRun || rep.Written != 3 || rep.Cached != 3 || rep.Errors["invalid_reply"] != 1 || rep.Garbled != 1 ||
		rep.OtherRecipe != 0 || rep.QualityHist["4"] != 2 || rep.QualityHist["1"] != 1 {
		t.Errorf("report = %+v", rep)
	}
	for _, s := range store.scans {
		if s.RecipeID != rep.RecipeID || s.FnCallID == 0 || s.ChunkTextSHA256 != strings.Repeat("c", 64) {
			t.Errorf("row = %+v", s)
		}
	}
}

// TestRunWalksPagesAndHonoursLimit: without --sample the run pages by
// keyset until --limit chunks are visited.
func TestRunWalksPagesAndHonoursLimit(t *testing.T) {
	srv, _ := fakeSystemOne(t)
	texts := make([]string, 7)
	for i := range texts {
		texts[i] = "chunk " + string(rune('a'+i))
	}
	store := &fakeStore{cands: cands(texts...)}
	var out strings.Builder
	if err := run(context.Background(), &out, store, newScanner(t, srv.URL, store),
		options{limit: 5, concurrency: 2, context: 2, yes: true}); err != nil {
		t.Fatal(err)
	}
	if len(store.scans) != 5 {
		t.Errorf("scanned %d, want 5 (--limit)", len(store.scans))
	}
	// Unlimited: every page until the store runs dry.
	store2 := &fakeStore{cands: cands(texts...)}
	if err := run(context.Background(), &out, store2, newScanner(t, srv.URL, store2),
		options{concurrency: 2, context: 2, yes: true}); err != nil {
		t.Fatal(err)
	}
	if len(store2.scans) != 7 {
		t.Errorf("scanned %d, want all 7", len(store2.scans))
	}
}

// TestRunRoutePrefixedReplyKeepsTheRecipe: a gateway that reports the model
// with its route prefix ("typesafe/jev-1.13.0") answers under the run's own
// recipe — no other_recipe, no MODELS_FILE pin needed.
func TestRunRoutePrefixedReplyKeepsTheRecipe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req systemone.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, _ = w.Write([]byte(strings.Replace(replyFor(req.State), `"model":"jev-1.13.0"`, `"model":"typesafe/jev-1.13.0"`, 1)))
	}))
	t.Cleanup(srv.Close)
	store := &fakeStore{cands: cands("one", "two")}
	var out strings.Builder
	if err := run(context.Background(), &out, store, newScanner(t, srv.URL, store),
		options{sample: 2, concurrency: 2, context: 2, yes: true, json: true}); err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.OtherRecipe != 0 || rep.Fallback != 0 || rep.Written != 2 {
		t.Errorf("report = %+v", rep)
	}
	for _, s := range store.scans {
		if s.RecipeID != rep.RecipeID {
			t.Errorf("row recipe %s, want the run's %s", s.RecipeID, rep.RecipeID)
		}
	}
}

func TestRunReportsChangedChunks(t *testing.T) {
	srv, _ := fakeSystemOne(t)
	store := &fakeStore{cands: cands("x"), outcome: db.ChunkScanChanged}
	var out strings.Builder
	if err := run(context.Background(), &out, store, newScanner(t, srv.URL, store),
		options{sample: 1, concurrency: 1, context: 2, yes: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Wrote 0, already present 0, skipped 1") {
		t.Errorf("report:\n%s", out.String())
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	for name, o := range map[string]options{
		"negative sample":  {sample: -1, concurrency: 1},
		"zero concurrency": {concurrency: 0},
		"too much context": {concurrency: 1, context: db.MaxScanContextSegments + 1},
		"negative limit":   {concurrency: 1, limit: -1},
	} {
		if err := validate(o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// stepRunStore is a fakeStore that is also a runs.Store, recording the
// step_runs writes — or failing every one of them when broken.
type stepRunStore struct {
	*fakeStore
	broken   bool
	srMu     sync.Mutex
	start    db.StepRunStart
	status   string
	final    db.StepRunProgress
	finished int
}

func (s *stepRunStore) StartStepRun(_ context.Context, st db.StepRunStart) (int64, error) {
	if s.broken {
		return 0, errors.New(`relation "step_runs" does not exist`)
	}
	s.srMu.Lock()
	defer s.srMu.Unlock()
	s.start = st
	return 7, nil
}

func (s *stepRunStore) UpdateStepRun(context.Context, int64, db.StepRunProgress) error {
	if s.broken {
		return errors.New("connection reset")
	}
	return nil
}

func (s *stepRunStore) FinishStepRun(_ context.Context, _ int64, status string, p db.StepRunProgress, _ string) error {
	if s.broken {
		return errors.New("connection reset")
	}
	s.srMu.Lock()
	defer s.srMu.Unlock()
	s.status, s.final = status, p
	s.finished++
	return nil
}

// TestRunRecordsStepRun: a scan run against a store that records step runs
// leaves one closed row with the run's counters; a store whose every step_runs
// write fails changes nothing about the scan (CONTRACT §1.10: recording never
// fails the step).
func TestRunRecordsStepRun(t *testing.T) {
	srv, _ := fakeSystemOne(t)
	o := options{sample: 10, seed: "s", concurrency: 3, context: 2, yes: true, json: true}

	ok := &stepRunStore{fakeStore: &fakeStore{cands: cands("a clean chunk", "pure garbage here", "a broken reply", "another clean one")}}
	var out1 strings.Builder
	if err := run(context.Background(), &out1, ok, newScanner(t, srv.URL, ok.fakeStore), o); err != nil {
		t.Fatalf("run: %v", err)
	}
	if ok.start.Step != "scan" || ok.start.Mode != db.StepRunWrite || ok.start.Total == nil || *ok.start.Total != 10 ||
		!strings.Contains(ok.start.Args, "--sample 10") {
		t.Errorf("start = %+v", ok.start)
	}
	if ok.finished != 1 || ok.status != db.StepRunDone || *ok.final.Done != 4 || ok.final.Counters["scanned"] != 3 ||
		ok.final.Counters["errors"] != 1 || ok.final.Counters["written"] != 3 || ok.final.RecipeID == "" {
		t.Errorf("finish = %s %d %+v", ok.status, ok.finished, ok.final)
	}

	broken := &stepRunStore{fakeStore: &fakeStore{cands: cands("a clean chunk", "pure garbage here", "a broken reply", "another clean one")}, broken: true}
	var out2 strings.Builder
	if err := run(context.Background(), &out2, broken, newScanner(t, srv.URL, broken.fakeStore), o); err != nil {
		t.Fatalf("a broken step_runs store failed the scan: %v", err)
	}
	if len(broken.scans) != 3 {
		t.Errorf("wrote %d chunk_scan rows with recording broken, want 3", len(broken.scans))
	}
	var r1, r2 Report
	if err := json.Unmarshal([]byte(out1.String()), &r1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out2.String()), &r2); err != nil {
		t.Fatal(err)
	}
	if r1.Chunks != r2.Chunks || r1.Written != r2.Written || r1.Scanned != r2.Scanned {
		t.Errorf("recording changed the scan: %+v vs %+v", r1, r2)
	}
}
