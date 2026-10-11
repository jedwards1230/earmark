package decide

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/fn"
	"github.com/jedwards1230/earmark/internal/genai"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/systemone"
	"github.com/jedwards1230/earmark/internal/tokenizer"
)

// The prompt hash (question + state layout) is the cache key's prompt
// component: any edit to the instructions, criteria or state labels must show
// up here, together with a bump of ShouldApplyPromptVersion.
func TestShouldApplyPromptPinned(t *testing.T) {
	const want = "ead8aff28d2ba53fdc1c2aa68dd6c2089142a7f23e554beab17eafb1533e168d"
	if ShouldApplyPromptSHA256 != want {
		t.Errorf("ShouldApplyPromptSHA256 = %s, want %s — bump ShouldApplyPromptVersion when the question changes", ShouldApplyPromptSHA256, want)
	}
	if ShouldApplyPromptVersion != "should_apply@v2" || ShouldApplyModel != "jev-1.13.0" || ShouldApplyStepVersion != 3 {
		t.Errorf("identity changed: %s %s step %d", ShouldApplyPromptVersion, ShouldApplyModel, ShouldApplyStepVersion)
	}
	// An earlier prompt's hash must never come back: its cached answers would
	// be served for this prompt's states.
	for version, old := range map[string]string{
		"should_apply@v1": "53b1fdd5811e8f49d3c938c500c747536ef89f6e4bc859bff05ec81a6f754a5f",
		// An undeployed v2 draft, measured live in dry runs (its answers are
		// in fn_calls).
		"should_apply@v2 draft": "a9e3774cb9a83e60baf5d70f42e9f01bde73d94581a051b33c12e8746e098699",
	} {
		if ShouldApplyPromptSHA256 == old {
			t.Errorf("prompt hash equals %s's", version)
		}
	}
}

// A state-layout change alone (no question edit) must change the prompt hash.
func TestShouldApplyPromptHashCoversLayout(t *testing.T) {
	base := shouldApplyPromptSHA256(shouldApplyQuestion, stateLayout)
	if base != ShouldApplyPromptSHA256 {
		t.Fatalf("helper %s != package hash %s", base, ShouldApplyPromptSHA256)
	}
	for i := range stateLayout {
		l := slices.Clone(stateLayout)
		l[i] += "x"
		if shouldApplyPromptSHA256(shouldApplyQuestion, l) == base {
			t.Errorf("changing layout entry %q kept the hash", stateLayout[i])
		}
	}
	q := shouldApplyQuestion
	q.Instructions += " "
	if shouldApplyPromptSHA256(q, stateLayout) == base {
		t.Error("changing the instructions kept the hash")
	}
}

func TestBuildStateGolden(t *testing.T) {
	c := Context{
		Original:  "the dish at [[auto sebo]] picked",
		Corrected: "the dish at [[Arecibo]] picked",
		Before:    "previous chunk words",
	}
	recs := []RecordSentence{{Field: "chapter", Text: "The Arecibo Message"}}
	want := "ISSUE TYPE:\nmisheard_proper_noun\n\n" +
		"ORIGINAL:\nthe dish at [[auto sebo]] picked\n\n" +
		"PROPOSED:\nthe dish at [[Arecibo]] picked\n\n" +
		"BEFORE:\nprevious chunk words\n\n" +
		"AFTER:\n(none)\n\n" +
		"BOOK REFERENCE:\n- chapter: The Arecibo Message\n"
	if got := BuildState(IssueMisheardProperNoun, c, recs); got != want {
		t.Errorf("state:\n%s\nwant:\n%s", got, want)
	}
	if got := BuildState(IssueMisheardWord, c, nil); !strings.HasSuffix(got, "BOOK REFERENCE:\n(none)\n") {
		t.Errorf("empty reference: %q", got)
	}
}

// The apply rule's two keys — p and text evidence — must stay independent:
// the state never tells the model whether the evidence gate passed.
func TestStateOmitsEvidence(t *testing.T) {
	var seen systemone.Request
	j := &fakeJev{}
	j.reply = func(w http.ResponseWriter, req systemone.Request) {
		seen = req
		noul(0.5, "jev-1.13.0")(w, req)
	}
	e, _ := newTestEvaluator(t, j)
	const stutter = "he said the the cat sat"
	repeat := Input{
		Candidate: cand("f2", IssueRepeatedText, stutter, "the the cat", "the cat", 0.9),
		Chunk:     ChunkWindow{StartSec: 0, EndSec: 2},
		Segments:  []db.Segment{seg(0, 2, stutter)},
	}
	for want, in := range map[string]Input{
		EvidenceASINVerbatim: radioInput(IssueMisheardProperNoun, "auto sebo", "Arecibo"),
		EvidenceExactRepeat:  repeat,
	} {
		out := e.Evaluate(context.Background(), in)
		if !out.Asked || out.Evidence != want {
			t.Fatalf("want an asked %s finding, got %+v", want, out)
		}
		for _, leak := range []string{out.Evidence, "evidence", "verbatim", "exact repeat", "soundalike", "sound-alike", "checks"} {
			if strings.Contains(strings.ToLower(seen.State), strings.ToLower(leak)) {
				t.Errorf("state leaks %q (evidence %s):\n%s", leak, out.Evidence, seen.State)
			}
		}
	}
}

func TestDecideBoundaries(t *testing.T) {
	p := DefaultShouldApplyParams()
	tests := []struct {
		prob             float64
		evidence, issue  string
		decision, reason string
	}{
		{0.85, EvidenceASINVerbatim, IssueMisheardProperNoun, DecisionApply, ReasonConfident},
		{0.8499999, EvidenceASINVerbatim, IssueMisheardProperNoun, DecisionHold, ReasonUncertain},
		{1, EvidenceExactRepeat, IssueRepeatedText, DecisionApply, ReasonConfident},
		{0.99, EvidenceNone, IssueMisheardWord, DecisionHold, ReasonNoEvidence},
		{0.99, EvidenceASINVerbatim, IssueDroppedWord, DecisionHold, ReasonTypeCapped},
		{1, EvidenceASINVerbatim, IssueNumberArtifact, DecisionHold, ReasonTypeCapped},
		{0.5, EvidenceASINVerbatim, IssueHomophone, DecisionHold, ReasonUncertain},
		{0.2000001, EvidenceNone, IssueHomophone, DecisionHold, ReasonUncertain},
		{0.20, EvidenceASINVerbatim, IssueHomophone, DecisionReject, ReasonJevReject},
		{0, EvidenceNone, IssueDroppedWord, DecisionReject, ReasonJevReject},
		{math.NaN(), EvidenceASINVerbatim, IssueHomophone, DecisionHold, ReasonJevUnavailable},
		{1.01, EvidenceASINVerbatim, IssueHomophone, DecisionHold, ReasonJevUnavailable},
		{-0.01, EvidenceNone, IssueHomophone, DecisionHold, ReasonJevUnavailable},
		{math.Inf(1), EvidenceASINVerbatim, IssueHomophone, DecisionHold, ReasonJevUnavailable},
	}
	for _, tc := range tests {
		d, r := p.Decide(tc.prob, tc.evidence, tc.issue, "Arecibo")
		if d != tc.decision || r != tc.reason {
			t.Errorf("Decide(%v, %s, %s) = %s/%s, want %s/%s", tc.prob, tc.evidence, tc.issue, d, r, tc.decision, tc.reason)
		}
	}
}

// TestPresetDecisions pins the two built-in policies (CONTRACT §2.19 "Decide
// policy") to the measured evidence: Jev applies at p ≥ 0.85 with either
// evidence kind and rejects at p ≤ 0.20 whatever the case; Clef applies only
// on asin_verbatim and rejects at p ≤ 0.20 only a lowercase replacement or
// p < 0.10.
func TestPresetDecisions(t *testing.T) {
	presets := map[string]string{PresetJev: "jev-1.13.0", PresetClef: "clef:27b"}
	tests := []struct {
		name, preset     string
		prob             float64
		evidence, issue  string
		replacement      string
		decision, reason string
	}{
		// Jev.
		{"jev asin_verbatim at 0.85 applies", PresetJev, 0.85, EvidenceASINVerbatim, IssueMisheardProperNoun, "Arecibo", DecisionApply, ReasonConfident},
		{"jev exact_repeat at 0.85 applies", PresetJev, 0.85, EvidenceExactRepeat, IssueRepeatedText, "the cat", DecisionApply, ReasonConfident},
		{"jev just under 0.85 holds", PresetJev, 0.849, EvidenceASINVerbatim, IssueMisheardProperNoun, "Arecibo", DecisionHold, ReasonUncertain},
		{"jev no evidence holds", PresetJev, 0.99, EvidenceNone, IssueMisheardWord, "cat", DecisionHold, ReasonNoEvidence},
		{"jev capitalised reject still rejects", PresetJev, 0.15, EvidenceNone, IssueMisheardProperNoun, "Arecibo", DecisionReject, ReasonJevReject},
		{"jev lowercase reject rejects", PresetJev, 0.20, EvidenceNone, IssueMisheardWord, "cat", DecisionReject, ReasonJevReject},
		{"jev capped type holds", PresetJev, 0.99, EvidenceASINVerbatim, IssueDroppedWord, "the", DecisionHold, ReasonTypeCapped},
		// Clef.
		{"clef asin_verbatim at 0.85 applies", PresetClef, 0.85, EvidenceASINVerbatim, IssueMisheardProperNoun, "Arecibo", DecisionApply, ReasonConfident},
		{"clef exact_repeat excluded", PresetClef, 0.99, EvidenceExactRepeat, IssueRepeatedText, "the cat", DecisionHold, ReasonEvidenceExcluded},
		{"clef no evidence holds", PresetClef, 0.99, EvidenceNone, IssueMisheardWord, "cat", DecisionHold, ReasonNoEvidence},
		{"clef capped beats evidence", PresetClef, 0.99, EvidenceASINVerbatim, IssueNumberArtifact, "1937", DecisionHold, ReasonTypeCapped},
		{"clef lowercase reject at 0.20 rejects", PresetClef, 0.20, EvidenceNone, IssueMisheardWord, "cat", DecisionReject, ReasonJevReject},
		{"clef empty replacement counts as lowercase", PresetClef, 0.15, EvidenceNone, IssueRepeatedText, "", DecisionReject, ReasonJevReject},
		{"clef capitalised reject at 0.15 holds", PresetClef, 0.15, EvidenceNone, IssueMisheardProperNoun, "Arecibo", DecisionHold, ReasonCapitalised},
		{"clef capitalised at the floor holds", PresetClef, 0.10, EvidenceASINVerbatim, IssueMisheardProperNoun, "von Braun", DecisionHold, ReasonCapitalised},
		{"clef capitalised under the floor rejects", PresetClef, 0.0999, EvidenceNone, IssueMisheardProperNoun, "Arecibo", DecisionReject, ReasonJevReject},
		{"clef non-ASCII capital holds", PresetClef, 0.15, EvidenceNone, IssueMisheardProperNoun, "Éowyn", DecisionHold, ReasonCapitalised},
		{"clef uncertain band holds", PresetClef, 0.5, EvidenceASINVerbatim, IssueMisheardProperNoun, "Arecibo", DecisionHold, ReasonUncertain},
		{"clef unusable p holds", PresetClef, math.NaN(), EvidenceASINVerbatim, IssueMisheardProperNoun, "arecibo", DecisionHold, ReasonJevUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, p, ok := PresetFor(presets[tc.preset])
			if !ok || name != tc.preset {
				t.Fatalf("PresetFor(%s) = %s, %v", presets[tc.preset], name, ok)
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("preset %s invalid: %v", name, err)
			}
			d, r := p.Decide(tc.prob, tc.evidence, tc.issue, tc.replacement)
			if d != tc.decision || r != tc.reason {
				t.Errorf("Decide(%v, %s, %s, %q) = %s/%s, want %s/%s", tc.prob, tc.evidence, tc.issue, tc.replacement,
					d, r, tc.decision, tc.reason)
			}
		})
	}
}

func TestPresetFor(t *testing.T) {
	for model, want := range map[string]string{
		"jev-1.13.0": PresetJev, "typesafe/jev-1.13.0": PresetJev, "JEV-1.14.0": PresetJev,
		"clef:27b": PresetClef, "ollama/clef:27b": PresetClef, "clef-1.0.0": PresetClef,
		"other-1.0.0": "", "jevons-1.0": "", "": "",
	} {
		name, _, ok := PresetFor(model)
		if name != want || ok != (want != "") {
			t.Errorf("PresetFor(%q) = %q, %v; want %q", model, name, ok, want)
		}
	}
	// The Jev preset is the default policy.
	_, jev, _ := PresetFor("jev-1.13.0")
	if !reflect.DeepEqual(jev, DefaultShouldApplyParams()) {
		t.Errorf("jev preset %+v differs from the default %+v", jev, DefaultShouldApplyParams())
	}
}

func TestShouldApplyParamsValidate(t *testing.T) {
	ok := DefaultShouldApplyParams()
	tests := map[string]func(*ShouldApplyParams){
		"reject_p = apply_p":      func(p *ShouldApplyParams) { p.ApplyP, p.RejectP = 0.2, 0.2 },
		"apply_p > 1":             func(p *ShouldApplyParams) { p.ApplyP = 1.1 },
		"reject_p NaN":            func(p *ShouldApplyParams) { p.RejectP = math.NaN() },
		"reject_p < 0":            func(p *ShouldApplyParams) { p.RejectP = -0.1 },
		"phonetic 0":              func(p *ShouldApplyParams) { p.PhoneticMinSim = 0 },
		"no evidence kinds":       func(p *ShouldApplyParams) { p.ApplyEvidence = nil },
		"unknown evidence kind":   func(p *ShouldApplyParams) { p.ApplyEvidence = []string{"asin"} },
		"evidence none":           func(p *ShouldApplyParams) { p.ApplyEvidence = []string{EvidenceNone} },
		"evidence twice":          func(p *ShouldApplyParams) { p.ApplyEvidence = []string{EvidenceExactRepeat, EvidenceExactRepeat} },
		"floor without lowercase": func(p *ShouldApplyParams) { p.RejectFloor = 0.1 },
		"floor above reject_p":    func(p *ShouldApplyParams) { p.RejectRequiresLowercase, p.RejectFloor = true, 0.25 },
		"floor negative":          func(p *ShouldApplyParams) { p.RejectRequiresLowercase, p.RejectFloor = true, -0.01 },
		"floor NaN":               func(p *ShouldApplyParams) { p.RejectRequiresLowercase, p.RejectFloor = true, math.NaN() },
	}
	for name, mut := range tests {
		t.Run(name, func(t *testing.T) {
			p := DefaultShouldApplyParams()
			mut(&p)
			if err := p.Validate(); err == nil {
				t.Errorf("%+v validated", p)
			}
			if _, err := ShouldApplyFn(p); err == nil {
				t.Errorf("ShouldApplyFn accepted %+v", p)
			}
		})
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("default policy invalid: %v", err)
	}
	ok.RejectRequiresLowercase = true
	if err := ok.Validate(); err != nil {
		t.Errorf("lowercase rule without a floor should validate: %v", err)
	}
}

func TestShouldApplyParams(t *testing.T) {
	// Every threshold is a recipe param: changing one changes the recipe.
	base, _ := ShouldApplyFn(DefaultShouldApplyParams())
	baseID := recipeID(t, base.Recipe(""))
	for name, mut := range map[string]func(*ShouldApplyParams){
		"apply_p":                   func(p *ShouldApplyParams) { p.ApplyP = 0.96 },
		"reject_p":                  func(p *ShouldApplyParams) { p.RejectP = 0.1 },
		"phonetic_min_sim":          func(p *ShouldApplyParams) { p.PhoneticMinSim = 0.7 },
		"apply_evidence":            func(p *ShouldApplyParams) { p.ApplyEvidence = []string{EvidenceASINVerbatim} },
		"reject_requires_lowercase": func(p *ShouldApplyParams) { p.RejectRequiresLowercase = true },
		"reject_floor": func(p *ShouldApplyParams) {
			p.RejectRequiresLowercase, p.RejectFloor = true, 0.1
		},
	} {
		p := DefaultShouldApplyParams()
		mut(&p)
		f, err := ShouldApplyFn(p)
		if err != nil {
			t.Fatal(err)
		}
		if recipeID(t, f.Recipe("")) == baseID {
			t.Errorf("%s change kept recipe %s", name, baseID)
		}
	}
	// The order apply_evidence lists its kinds in is not a policy change.
	p := DefaultShouldApplyParams()
	p.ApplyEvidence = []string{EvidenceExactRepeat, EvidenceASINVerbatim}
	if f, _ := ShouldApplyFn(p); recipeID(t, f.Recipe("")) != baseID {
		t.Error("reordering apply_evidence changed the recipe")
	}
	// The two presets are two recipes on one model.
	_, clef, _ := PresetFor("clef:27b")
	fc, _ := ShouldApplyFnFor(clef, Model{Alias: "clef:27b"})
	fd, _ := ShouldApplyFnFor(DefaultShouldApplyParams(), Model{Alias: "clef:27b"})
	if recipeID(t, fc.Recipe("")) == recipeID(t, fd.Recipe("")) {
		t.Error("the clef preset and the default policy share a recipe on clef:27b")
	}
	for _, k := range []string{"evidence_rule", "rung0_version", "apply_p", "reject_p", "phonetic_min_sim",
		"apply_evidence", "reject_requires_lowercase", "reject_floor",
		"context_words", "neighbour_words", "fallback_words", "max_relevant_sentences", "max_relevant_runes",
		"max_sentence_runes", "clip_words"} {
		if _, ok := base.Params[k]; !ok {
			t.Errorf("recipe params lack %s", k)
		}
	}
}

// ─── End to end ──────────────────────────────────────────────────────────────

// memStore is an in-memory fn.Store with the cache semantics of fn_calls.
type memStore struct {
	mu   sync.Mutex
	rows []db.FnCall
}

func (s *memStore) RegisterRecipe(_ context.Context, r recipe.Recipe) (string, error) {
	return r.ID()
}

func recipeID(t *testing.T, r recipe.Recipe) string {
	t.Helper()
	id, err := r.ID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *memStore) LookupFnCache(_ context.Context, k db.FnCacheKey, expected string) (*db.FnCall, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		r := s.rows[i]
		if r.Key() == k && r.ErrorClass == "" && !r.CacheHit && r.Output != nil && genai.SameModel(r.ModelResolved, expected) {
			return &r, true, nil
		}
	}
	return nil, false, nil
}

func (s *memStore) InsertFnCall(_ context.Context, c db.FnCall) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.ID = int64(len(s.rows) + 1)
	s.rows = append(s.rows, c)
	return c.ID, true, nil
}

// fakeJev is a System One endpoint answering every request with reply (a
// function of the request so tests can inspect it).
type fakeJev struct {
	calls atomic.Int32
	reply func(w http.ResponseWriter, req systemone.Request)
}

func (f *fakeJev) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req systemone.Request
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		f.reply(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func noul(p float64, model string) func(http.ResponseWriter, systemone.Request) {
	return func(w http.ResponseWriter, _ systemone.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+model+`","answers":{"should_apply":{"type":"noul","noul":`+
			jsonNum(p)+`}},"usage":{"input_tokens":400,"output_tokens":3}}`)
	}
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

const radioChunk = "the dish at auto sebo picked up the signal"

func radioInput(issue, original, replacement string) Input {
	segs := []db.Segment{seg(0, 2, "previous words"), seg(2, 4, "the dish at auto sebo picked"), seg(4, 6, "up the signal")}
	rec := hailMary()
	return Input{
		Candidate: cand("f1", issue, radioChunk, original, replacement, 0.9),
		Chunk:     ChunkWindow{StartSec: 2, EndSec: 6},
		Segments:  segs,
		Record:    rec,
	}
}

func newTestEvaluator(t *testing.T, j *fakeJev, opts ...systemone.Option) (*Evaluator, *memStore) {
	t.Helper()
	srv := j.server(t)
	client, err := systemone.New(srv.URL, "test-key", opts...)
	if err != nil {
		t.Fatal(err)
	}
	store := &memStore{}
	e, err := NewEvaluator(store, client, DefaultShouldApplyParams())
	if err != nil {
		t.Fatal(err)
	}
	return e, store
}

func TestEvaluateEndToEnd(t *testing.T) {
	ctx := context.Background()

	t.Run("confident with evidence applies, then serves from cache", func(t *testing.T) {
		var seen systemone.Request
		j := &fakeJev{}
		j.reply = func(w http.ResponseWriter, req systemone.Request) {
			seen = req
			noul(0.97, "jev-1.13.0")(w, req)
		}
		e, store := newTestEvaluator(t, j)
		in := radioInput(IssueMisheardProperNoun, "auto sebo", "Arecibo")
		out := e.Evaluate(ctx, in)
		if out.Decision != DecisionApply || out.Reason != ReasonConfident || out.Evidence != EvidenceASINVerbatim ||
			out.P == nil || *out.P != 0.97 || out.Retryable || out.FnCallID == 0 || out.CallRecipeID == "" ||
			out.ChunkHash != in.Candidate.ChunkHash || out.Model != "jev-1.13.0" || !out.Rung0.Pass {
			t.Fatalf("outcome %+v", out)
		}
		if seen.Model != ShouldApplyModel || len(seen.Questions) != 1 || seen.Questions[questionKey].Type != systemone.TypeNoul {
			t.Errorf("request %+v", seen)
		}
		for _, want := range []string{"ORIGINAL:\nthe dish at [[auto sebo]] picked", "PROPOSED:\nthe dish at [[Arecibo]] picked",
			"BEFORE:\nprevious words", "AFTER:\nup the signal", "- chapter: The Arecibo Message"} {
			if !strings.Contains(seen.State, want) {
				t.Errorf("state lacks %q:\n%s", want, seen.State)
			}
		}
		if strings.Contains(seen.State, "lunch") {
			t.Errorf("state carries an unrelated record sentence:\n%s", seen.State)
		}

		again := e.Evaluate(ctx, in)
		if !again.CacheHit || again.Decision != DecisionApply || j.calls.Load() != 1 {
			t.Errorf("second evaluate: %+v, %d calls", again, j.calls.Load())
		}
		if len(store.rows) != 2 {
			t.Errorf("%d fn_calls rows, want 2", len(store.rows))
		}
	})

	t.Run("confident without evidence holds", func(t *testing.T) {
		j := &fakeJev{reply: noul(0.99, "typesafe/jev-1.13.0")}
		e, _ := newTestEvaluator(t, j)
		in := radioInput(IssueMisheardProperNoun, "auto sebo", "Arecibo")
		in.Record = nil
		out := e.Evaluate(ctx, in)
		if out.Decision != DecisionHold || out.Reason != ReasonNoEvidence || out.Evidence != EvidenceNone {
			t.Errorf("outcome %+v", out)
		}
	})

	t.Run("low p rejects", func(t *testing.T) {
		e, _ := newTestEvaluator(t, &fakeJev{reply: noul(0.05, "jev-1.13.0")})
		out := e.Evaluate(ctx, radioInput(IssueMisheardProperNoun, "auto sebo", "Arecibo"))
		if out.Decision != DecisionReject || out.Reason != ReasonJevReject || out.Retryable {
			t.Errorf("outcome %+v", out)
		}
	})

	t.Run("capped issue types never apply", func(t *testing.T) {
		e, _ := newTestEvaluator(t, &fakeJev{reply: noul(1, "jev-1.13.0")})
		out := e.Evaluate(ctx, radioInput(IssueDroppedWord, "the dish", "the Arecibo dish"))
		if out.Decision != DecisionHold || out.Reason != ReasonTypeCapped {
			t.Errorf("dropped_word: %+v", out)
		}
	})

	t.Run("rung-0 failure rejects without a call", func(t *testing.T) {
		j := &fakeJev{reply: noul(1, "jev-1.13.0")}
		e, store := newTestEvaluator(t, j)
		out := e.Evaluate(ctx, radioInput(IssueMisheardProperNoun, "auto sebo", "Zanzibar"))
		if out.Decision != DecisionReject || out.Reason != ReasonNotSoundAlike || out.P != nil || j.calls.Load() != 0 || len(store.rows) != 0 {
			t.Errorf("outcome %+v, %d calls", out, j.calls.Load())
		}
		v := Verdict{Reason: ReasonOverlapDup}
		in := radioInput(IssueMisheardProperNoun, "auto sebo", "Arecibo")
		in.Verdict = &v
		if out := e.Evaluate(ctx, in); out.Decision != DecisionReject || out.Reason != ReasonOverlapDup || j.calls.Load() != 0 {
			t.Errorf("precomputed verdict ignored: %+v", out)
		}
	})
}

// Every way the model can fail to answer is a retryable hold — never apply.
func TestEvaluateFailsClosed(t *testing.T) {
	ctx := context.Background()
	write := func(status int, body string) func(http.ResponseWriter, systemone.Request) {
		return func(w http.ResponseWriter, _ systemone.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}
	cases := map[string]func(http.ResponseWriter, systemone.Request){
		"server error":   write(http.StatusInternalServerError, `{}`),
		"rate limited":   write(http.StatusTooManyRequests, `{}`),
		"unauthorized":   write(http.StatusUnauthorized, `{}`),
		"not json":       write(http.StatusOK, `<html>`),
		"no answer":      write(http.StatusOK, `{"model":"jev-1.13.0","answers":{},"usage":{}}`),
		"wrong type":     write(http.StatusOK, `{"model":"jev-1.13.0","answers":{"should_apply":{"type":"choice","choice":"true"}},"usage":{}}`),
		"field renamed":  write(http.StatusOK, `{"model":"jev-1.13.0","answers":{"should_apply":{"type":"noul","value":0.99}},"usage":{}}`),
		"out of range":   write(http.StatusOK, `{"model":"jev-1.13.0","answers":{"should_apply":{"type":"noul","noul":1.5}},"usage":{}}`),
		"fallback model": noul(0.99, "jev-1.12.0"),
		"alias model":    noul(0.99, "jev-latest"),
		"timeout": func(w http.ResponseWriter, req systemone.Request) {
			time.Sleep(300 * time.Millisecond)
			noul(0.99, "jev-1.13.0")(w, req)
		},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			e, _ := newTestEvaluator(t, &fakeJev{reply: reply}, systemone.WithTimeout(100*time.Millisecond))
			out := e.Evaluate(ctx, radioInput(IssueMisheardProperNoun, "auto sebo", "Arecibo"))
			if out.Decision != DecisionHold || out.Reason != ReasonJevUnavailable || !out.Retryable || out.P != nil || out.ErrorClass == "" || !out.Asked {
				t.Errorf("outcome %+v", out)
			}
		})
	}

	t.Run("cancelled context", func(t *testing.T) {
		e, _ := newTestEvaluator(t, &fakeJev{reply: noul(0.99, "jev-1.13.0")})
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if out := e.Evaluate(cctx, radioInput(IssueMisheardProperNoun, "auto sebo", "Arecibo")); out.Decision != DecisionHold || out.Reason != ReasonJevUnavailable {
			t.Errorf("outcome %+v", out)
		}
	})
}

func TestDecodeShouldApply(t *testing.T) {
	for raw, ok := range map[string]bool{
		`{"model":"m","answers":{"should_apply":{"type":"noul","noul":0.5}}}`:           true,
		`{"model":"m","answers":{"should_apply":{"type":"noul","noul":0.5}},"extra":1}`: false,
		`{"model":"m","answers":{"should_apply":{"type":"noul"}}}`:                      false,
		`{"model":"m","answers":{"other":{"type":"noul","noul":0.5}}}`:                  false,
		`{"model":"m","answers":{"should_apply":{"type":"noul","noul":0.5}}} {}`:        false,
		`{"model":"m","answers":{"should_apply":{"type":"noul","noul":-0.1}}}`:          false,
		`null`: false,
	} {
		if _, err := decodeShouldApply(json.RawMessage(raw)); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", raw, err, ok)
		}
	}
}

// The decide outcome vocabulary is what db.ApplyDecisions validates.
func TestOutcomeVocabularyMatchesDB(t *testing.T) {
	for got, want := range map[string]string{
		DecisionApply: db.OutcomeApply, DecisionHold: db.OutcomeHold, DecisionReject: db.OutcomeReject,
		DecisionDuplicate:    db.OutcomeDuplicate,
		EvidenceASINVerbatim: db.EvidenceASINVerbatim, EvidenceExactRepeat: db.EvidenceExactRepeat, EvidenceNone: db.EvidenceNone,
	} {
		if got != want {
			t.Errorf("decide %q != db %q", got, want)
		}
	}
}

func TestEvaluatorRecipe(t *testing.T) {
	e, _ := newTestEvaluator(t, &fakeJev{reply: noul(0.5, "jev-1.13.0")})
	r := e.Recipe()
	if r.Step != recipe.StepDecide || r.ModelAlias != ShouldApplyModel || r.ModelResolved != ShouldApplyModel ||
		r.PromptSHA256 != ShouldApplyPromptSHA256 || r.Params["fn"] != ShouldApplyName || r.Params["apply_p"] != 0.85 {
		t.Errorf("recipe %+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Error(err)
	}
	if r.StepVersion != ShouldApplyStepVersion {
		t.Errorf("recipe step_version %d, want %d", r.StepVersion, ShouldApplyStepVersion)
	}
}

// Step version 3 (grouping of agreeing findings, conflicts ranked by p) is a
// new decide recipe: the same model, prompt and policy under step 2 has
// another id, so step-2 decisions are re-checked rather than taken as this
// recipe's, and "decided under this recipe" never matches an older run.
func TestStepVersionBumpChangesRecipe(t *testing.T) {
	f, err := ShouldApplyFn(DefaultShouldApplyParams())
	if err != nil {
		t.Fatal(err)
	}
	cur := f.Recipe("")
	old := cur
	old.StepVersion = 2
	curID, err := cur.ID()
	if err != nil {
		t.Fatal(err)
	}
	oldID, err := old.ID()
	if err != nil {
		t.Fatal(err)
	}
	if cur.StepVersion != 3 || curID == oldID {
		t.Errorf("step %d: id %s, step-2 id %s — the bump must mint a new recipe", cur.StepVersion, curID, oldID)
	}
}

// The question is a fixed part of every call's input, so its size is a cost
// guard: v1's question was 225 cl100k tokens as canonical JSON and v2's is
// 161. Growing it past the budget needs a measured reason.
func TestShouldApplyQuestionBudget(t *testing.T) {
	const budget = 170
	canon, _, err := fn.CanonicalInput(map[string]systemone.Question{questionKey: shouldApplyQuestion})
	if err != nil {
		t.Fatal(err)
	}
	n, err := tokenizer.CountTokens(string(canon))
	if err != nil {
		t.Fatal(err)
	}
	if n > budget {
		t.Errorf("should_apply question is %d cl100k tokens, budget %d", n, budget)
	}
}

// staticAsker answers every call with resp (no HTTP, no decode).
type staticAsker struct {
	resp  *systemone.Response
	calls int
	model string
}

func (a *staticAsker) Decide(_ context.Context, req systemone.Request) (*systemone.Response, error) {
	a.calls++
	a.model = req.Model
	return a.resp, nil
}

// The unified route drops a refused answer: the outcome is a retryable hold
// (never an apply, never a panic), and nothing is cached, so the next run
// asks again. Over HTTP the client's strict decode already refuses the
// reply; a reply that reaches the evaluator without the answer is refused
// here too.
func TestEvaluateRefusedAnswerHolds(t *testing.T) {
	ctx := context.Background()
	in := radioInput(IssueMisheardProperNoun, "auto sebo", "Arecibo")

	t.Run("over HTTP, unified-route reply with no answer", func(t *testing.T) {
		j := &fakeJev{}
		j.reply = func(w http.ResponseWriter, _ systemone.Request) {
			_, _ = w.Write([]byte(`{"model":"clef:27b","answers":{},"usage":{"input_tokens":10,"output_tokens":0,"cached_tokens":4,"cache_write_tokens":0}}`))
		}
		e, _ := newTestEvaluator(t, j)
		for i := 0; i < 2; i++ {
			out := e.Evaluate(ctx, in)
			if out.Decision != DecisionHold || out.Reason != ReasonJevUnavailable || !out.Retryable || out.P != nil || out.CacheHit {
				t.Fatalf("outcome %+v", out)
			}
		}
		if j.calls.Load() != 2 {
			t.Errorf("calls = %d: a refused answer must not be cached", j.calls.Load())
		}
	})

	t.Run("asker reply without the answer", func(t *testing.T) {
		a := &staticAsker{resp: &systemone.Response{Model: "clef:27b", Answers: map[string]systemone.Answer{}}}
		e, err := NewEvaluatorFor(&memStore{}, a, DefaultShouldApplyParams(), Model{Alias: "clef:27b"})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			out := e.Evaluate(ctx, in)
			if out.Decision != DecisionHold || out.Reason != ReasonJevUnavailable || !out.Retryable {
				t.Fatalf("outcome %+v", out)
			}
		}
		if a.calls != 2 || a.model != "clef:27b" {
			t.Errorf("calls %d model %q: want 2 uncached calls asking clef:27b", a.calls, a.model)
		}
		if e.Recipe().ModelAlias != "clef:27b" {
			t.Errorf("recipe model %q", e.Recipe().ModelAlias)
		}
	})
}
