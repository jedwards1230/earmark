package scan

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/systemone"
)

func f64(v float64) *float64 { return &v }

// goodAnswers is a complete, well-typed answer set.
func goodAnswers() map[string]systemone.Answer {
	return map[string]systemone.Answer{
		QNeedsFix:    {Type: systemone.TypeNoul, Noul: f64(0.2)},
		QQuality:     {Type: systemone.TypeScore, Score: f64(3.5), Confidence: f64(0.8)},
		QBoilerplate: {Type: systemone.TypeNoul, Noul: f64(0.05)},
		QGarbled:     {Type: systemone.TypeNoul, Noul: f64(0.01)},
		QDialogue:    {Type: systemone.TypeNoul, Noul: f64(0.7)},
		QIssueType: {Type: systemone.TypeChoice, Choice: "homophone",
			Probabilities: map[string]float64{"homophone": 0.6, IssueNone: 0.4}, Confidence: f64(0.6)},
	}
}

func TestMapAnswers(t *testing.T) {
	r, err := MapAnswers(goodAnswers())
	if err != nil {
		t.Fatalf("MapAnswers: %v", err)
	}
	if r.PNeedsFix != 0.2 || r.Quality != 4.5 || *r.QualityConfidence != 0.8 || r.PBoilerplate != 0.05 ||
		r.PGarbled != 0.01 || r.PDialogue != 0.7 || r.IssueType != "homophone" || r.IssueProbs[IssueNone] != 0.4 {
		t.Errorf("mapped = %+v", r)
	}
	// Score 0 is quality 1, score 4 is quality 5; a missing confidence is NULL.
	a := goodAnswers()
	a[QQuality] = systemone.Answer{Type: systemone.TypeScore, Score: f64(0)}
	if r, err := MapAnswers(a); err != nil || r.Quality != 1 || r.QualityConfidence != nil {
		t.Errorf("score 0 = %+v, %v", r, err)
	}
	a[QQuality] = systemone.Answer{Type: systemone.TypeScore, Score: f64(4)}
	if r, err := MapAnswers(a); err != nil || r.Quality != 5 {
		t.Errorf("score 4 = %+v, %v", r, err)
	}
}

// TestMapAnswersMissingQuestionFails: every one of the six questions is
// required — dropping any one is an error, never a partial row.
func TestMapAnswersMissingQuestionFails(t *testing.T) {
	for _, q := range []string{QNeedsFix, QQuality, QBoilerplate, QGarbled, QDialogue, QIssueType} {
		t.Run(q, func(t *testing.T) {
			a := goodAnswers()
			delete(a, q)
			_, err := MapAnswers(a)
			var me *MappingError
			if !errors.As(err, &me) {
				t.Fatalf("missing %s: err = %v, want *MappingError", q, err)
			}
			if !strings.Contains(err.Error(), q) {
				t.Errorf("error %q does not name %s", err, q)
			}
		})
	}
}

func TestMapAnswersRejectsMistyped(t *testing.T) {
	cases := map[string]func(map[string]systemone.Answer){
		"noul answered as choice": func(a map[string]systemone.Answer) {
			a[QGarbled] = systemone.Answer{Type: systemone.TypeChoice, Choice: "x"}
		},
		"noul without value": func(a map[string]systemone.Answer) { a[QDialogue] = systemone.Answer{Type: systemone.TypeNoul} },
		"noul outside [0,1]": func(a map[string]systemone.Answer) {
			a[QNeedsFix] = systemone.Answer{Type: systemone.TypeNoul, Noul: f64(1.2)}
		},
		"noul NaN": func(a map[string]systemone.Answer) {
			a[QNeedsFix] = systemone.Answer{Type: systemone.TypeNoul, Noul: f64(math.NaN())}
		},
		"quality as noul": func(a map[string]systemone.Answer) {
			a[QQuality] = systemone.Answer{Type: systemone.TypeNoul, Noul: f64(0.5)}
		},
		"quality without score": func(a map[string]systemone.Answer) { a[QQuality] = systemone.Answer{Type: systemone.TypeScore} },
		"quality above top level": func(a map[string]systemone.Answer) {
			a[QQuality] = systemone.Answer{Type: systemone.TypeScore, Score: f64(4.01)}
		},
		"quality negative": func(a map[string]systemone.Answer) {
			a[QQuality] = systemone.Answer{Type: systemone.TypeScore, Score: f64(-0.1)}
		},
		"quality confidence > 1": func(a map[string]systemone.Answer) {
			a[QQuality] = systemone.Answer{Type: systemone.TypeScore, Score: f64(2), Confidence: f64(2)}
		},
		"issue as noul": func(a map[string]systemone.Answer) {
			a[QIssueType] = systemone.Answer{Type: systemone.TypeNoul, Noul: f64(0.5)}
		},
		"issue unknown label": func(a map[string]systemone.Answer) {
			a[QIssueType] = systemone.Answer{Type: systemone.TypeChoice, Choice: "other", Probabilities: map[string]float64{"other": 1}}
		},
		"issue without probabilities": func(a map[string]systemone.Answer) {
			a[QIssueType] = systemone.Answer{Type: systemone.TypeChoice, Choice: IssueNone}
		},
		"issue probability for unknown label": func(a map[string]systemone.Answer) {
			a[QIssueType] = systemone.Answer{Type: systemone.TypeChoice, Choice: IssueNone, Probabilities: map[string]float64{"run_on": 1}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := goodAnswers()
			mutate(a)
			if _, err := MapAnswers(a); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestDecodeRoundTripsAndRejectsGarbage(t *testing.T) {
	raw, err := json.Marshal(output{Answers: goodAnswers()})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := Decode(raw); err != nil || r.Quality != 4.5 {
		t.Fatalf("Decode = %+v, %v", r, err)
	}
	for _, bad := range []string{`not json`, `{}`, `{"answers":{}}`, `[1]`} {
		if _, err := Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("Decode(%s) accepted", bad)
		}
	}
}

func TestQuestionsAreValidAndCoverEveryIssueType(t *testing.T) {
	qs := Questions()
	if len(qs) != 6 {
		t.Fatalf("%d questions, want 6", len(qs))
	}
	labels, ok := qs[QIssueType].Criteria.(map[string]string)
	if !ok || len(labels) != len(IssueTypes) {
		t.Fatalf("issue_type labels = %v", qs[QIssueType].Criteria)
	}
	for _, it := range IssueTypes {
		if labels[it] == "" {
			t.Errorf("issue type %q has no label", it)
		}
	}
	if levels, ok := qs[QQuality].Criteria.([]string); !ok || len(levels) != 5 {
		t.Errorf("quality levels = %v, want 5", qs[QQuality].Criteria)
	}
	for name, q := range qs {
		if !strings.Contains(q.Instructions, "TEXT TO JUDGE") {
			t.Errorf("%s instructions do not scope the judgement to the judged text", name)
		}
	}
	// Pinned: any change to a question, a label or the state layout changes
	// the prompt hash — and so the recipe and every cache key. If this fails,
	// the change must be deliberate: bump PromptVersion with it.
	if got := PromptSHA256(); got != "8dcd1f7eaaff2f2c09bf86c62d0d61cb726cb61639ba3762710226abd03ff135" {
		t.Errorf("prompt hash = %s; the scan prompt changed — bump PromptVersion and re-pin", got)
	}
}

func TestStateLabelsContextAsNotJudged(t *testing.T) {
	s := State(Input{Text: "the judged text", ContextBefore: []string{"seg a", "seg b"}, ContextAfter: []string{"seg c"}})
	iBefore, iText, iAfter := strings.Index(s, "seg b"), strings.Index(s, "the judged text"), strings.Index(s, "seg c")
	if iBefore < 0 || iText < 0 || iAfter < 0 || iBefore >= iText || iText >= iAfter {
		t.Fatalf("state order wrong:\n%s", s)
	}
	if strings.Count(s, "context only — do not judge") != 2 || !strings.Contains(s, "=== TEXT TO JUDGE ===") {
		t.Errorf("context not labelled:\n%s", s)
	}
	bare := State(Input{Text: "only text"})
	if strings.Contains(bare, "CONTEXT") {
		t.Errorf("empty context rendered a section:\n%s", bare)
	}
}

func TestNewFnRefusesUnpinnedModel(t *testing.T) {
	if _, err := NewFn("jev-latest", "", "", 2); err == nil {
		t.Error("jev-latest accepted")
	}
	f, err := NewFn(DefaultModel, "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	r := f.Recipe("")
	if r.Step != recipe.StepScan || r.PromptVersion != PromptVersion || r.Params["context_segments"] != 2 || r.Params["fn"] != FnName {
		t.Errorf("recipe = %+v", r)
	}
	g, _ := NewFn(DefaultModel, "", "", 1)
	if a, _ := f.Recipe("").ID(); func() bool { b, _ := g.Recipe("").ID(); return a == b }() {
		t.Error("a different context width must be a different recipe")
	}
}

// TestQualityIndexArithmetic: per recipe, library = all chunks, the two
// sides split by ASIN; the mean of 1..5 is normalized to 0..1; empty scopes
// have no series.
func TestQualityIndexArithmetic(t *testing.T) {
	got := QualityIndex([]db.QualityGroup{
		{RecipeID: "r1", ASINMatched: true, Chunks: 2, QualitySum: 10},  // mean 5   → 1
		{RecipeID: "r1", ASINMatched: false, Chunks: 2, QualitySum: 2},  // mean 1   → 0
		{RecipeID: "r2", ASINMatched: false, Chunks: 4, QualitySum: 12}, // mean 3   → 0.5
		{RecipeID: "r3", ASINMatched: true, Chunks: 0, QualitySum: 0},   // empty    → none
	})
	want := []QualityPoint{
		{Scope: ScopeASINMatched, Recipe: "r1", Value: 1},
		{Scope: ScopeLibrary, Recipe: "r1", Value: 0.5}, // (10+2)/4 = 3 → 0.5
		{Scope: ScopeUnmatched, Recipe: "r1", Value: 0},
		{Scope: ScopeLibrary, Recipe: "r2", Value: 0.5},
		{Scope: ScopeUnmatched, Recipe: "r2", Value: 0.5},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Scope != want[i].Scope || got[i].Recipe != want[i].Recipe || math.Abs(got[i].Value-want[i].Value) > 1e-12 {
			t.Errorf("point %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Weighted, not a mean of means: 1 chunk at 5 and 3 at 1 → mean 2 → 0.25.
	w := QualityIndex([]db.QualityGroup{
		{RecipeID: "r", ASINMatched: true, Chunks: 1, QualitySum: 5},
		{RecipeID: "r", ASINMatched: false, Chunks: 3, QualitySum: 3},
	})
	for _, p := range w {
		if p.Scope == ScopeLibrary && math.Abs(p.Value-0.25) > 1e-12 {
			t.Errorf("library = %v, want 0.25 (chunk-weighted)", p.Value)
		}
	}
	if QualityIndex(nil) != nil {
		t.Error("no scans must publish no series")
	}
}

// ─── Scanner end to end against a fake System One ──────────────────────────

// memStore is an in-memory fn.Store with the fn_calls cache semantics.
type memStore struct {
	mu    sync.Mutex
	calls []db.FnCall
}

func (m *memStore) RegisterRecipe(_ context.Context, r recipe.Recipe) (string, error) {
	return r.ID()
}

func (m *memStore) LookupFnCache(_ context.Context, k db.FnCacheKey, _ string) (*db.FnCall, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.calls {
		c := m.calls[i]
		if c.Key() == k && c.ErrorClass == "" && !c.CacheHit {
			return &c, true, nil
		}
	}
	return nil, false, nil
}

func (m *memStore) InsertFnCall(_ context.Context, c db.FnCall) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.ID = int64(len(m.calls) + 1)
	m.calls = append(m.calls, c)
	return c.ID, true, nil
}

const goodReply = `{"model":"jev-1.13.0","answers":{
	"needs_fix":{"type":"noul","noul":0.9},
	"quality":{"type":"score","score":1,"confidence":0.7,"probabilities":{"0":0.1,"1":0.7,"2":0.1,"3":0.05,"4":0.05}},
	"boilerplate":{"type":"noul","noul":0.02},
	"garbled":{"type":"noul","noul":0.1},
	"dialogue":{"type":"noul","noul":0.3},
	"issue_type":{"type":"choice","choice":"misheard_proper_noun","confidence":0.8,
	  "probabilities":{"misheard_proper_noun":0.8,"none":0.2}}},
	"usage":{"input_tokens":1000,"output_tokens":60}}`

func TestScannerEndToEnd(t *testing.T) {
	var (
		mu    sync.Mutex
		hits  int
		reply = goodReply
		seen  systemone.Request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits++
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&seen)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	client, err := systemone.New(srv.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFn(DefaultModel, "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	store := &memStore{}
	sc := Scanner{Fn: f, Client: client, Store: store}
	cand := db.ScanCandidate{TranscriptID: "t", ChunkIndex: 3, Text: "liet kines walked",
		TextSHA256: strings.Repeat("a", 64), ContextBefore: []string{"before"}, ContextAfter: []string{"after"}}

	r, meta, err := sc.Scan(context.Background(), cand)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if r.Quality != 2 || r.PNeedsFix != 0.9 || r.IssueType != "misheard_proper_noun" || meta.CacheHit || meta.RecipeID == "" {
		t.Errorf("result = %+v meta = %+v", r, meta)
	}
	if r.CostUSD <= 0 {
		t.Errorf("cost = %v, want the input-token estimate", r.CostUSD)
	}
	if seen.Model != DefaultModel || len(seen.Questions) != 6 || !strings.Contains(seen.State, "liet kines walked") ||
		!strings.Contains(seen.State, "before") {
		t.Errorf("request = %+v", seen)
	}
	row := r.Row(cand, meta)
	if row.ChunkTextSHA256 != cand.TextSHA256 || row.FnCallID != meta.CallID || row.Quality != 2 {
		t.Errorf("row = %+v", row)
	}

	// The same chunk again is served from fn_calls: no request, same answer.
	r2, meta2, err := sc.Scan(context.Background(), cand)
	if err != nil || !meta2.CacheHit || r2.Quality != r.Quality || r2.CostUSD != 0 {
		t.Fatalf("second scan = %+v %+v %v", r2, meta2, err)
	}
	if hits != 1 {
		t.Errorf("%d requests, want 1 (the second is cached)", hits)
	}

	// A reply missing a question is a decode error: logged as an error row,
	// never cached, no result.
	mu.Lock()
	reply = strings.Replace(goodReply, `"dialogue":{"type":"noul","noul":0.3},`, "", 1)
	mu.Unlock()
	other := cand
	other.Text = "another chunk"
	if _, _, err := sc.Scan(context.Background(), other); err == nil {
		t.Fatal("a reply missing dialogue was accepted")
	}
	last := store.calls[len(store.calls)-1]
	if last.ErrorClass != "invalid_reply" || last.Output != nil {
		t.Errorf("failed call logged as %+v", last)
	}
}

// TestScannerMappingFailureIsNotCached: a reply System One's decoder accepts
// but the mapping cannot use (here: a choice with no probabilities) is logged
// as an error, so the next run asks again instead of failing from cache.
func TestScannerMappingFailureIsNotCached(t *testing.T) {
	bad := strings.Replace(goodReply,
		`"issue_type":{"type":"choice","choice":"misheard_proper_noun","confidence":0.8,
	  "probabilities":{"misheard_proper_noun":0.8,"none":0.2}}`,
		`"issue_type":{"type":"choice","choice":"none"}`, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(bad))
	}))
	defer srv.Close()
	client, _ := systemone.New(srv.URL, "")
	f, _ := NewFn(DefaultModel, "", "", 2)
	store := &memStore{}
	sc := Scanner{Fn: f, Client: client, Store: store}
	cand := db.ScanCandidate{TranscriptID: "t", Text: "x", TextSHA256: strings.Repeat("b", 64)}
	if _, _, err := sc.Scan(context.Background(), cand); err == nil {
		t.Fatal("unmappable reply accepted")
	}
	if len(store.calls) != 1 || store.calls[0].ErrorClass != "invalid_reply" {
		t.Fatalf("calls = %+v", store.calls)
	}
}
