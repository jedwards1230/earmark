package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	decidepkg "github.com/jedwards1230/earmark/internal/decide"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/systemone"
)

func TestValidate(t *testing.T) {
	ok := options{sample: 10, seed: "s", concurrency: 8, batch: 20}
	if err := validate(ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*options){
		"zero sample":         func(o *options) { o.sample = 0 },
		"huge sample":         func(o *options) { o.sample = db.MaxDecideSample + 1 },
		"empty seed":          func(o *options) { o.seed = "" },
		"concurrency":         func(o *options) { o.concurrency = 0 },
		"concurrency+":        func(o *options) { o.concurrency = 65 },
		"unknown issue":       func(o *options) { o.issueType = "typo" },
		"yes with --sample":   func(o *options) { o.yes, o.sampleSet = true, true },
		"yes with calibrate":  func(o *options) { o.yes, o.calibrate = true, true },
		"limit without yes":   func(o *options) { o.limit = 5 },
		"shard without yes":   func(o *options) { o.shard = "0/2" },
		"accepts without yes": func(o *options) { o.maxAccepts = 5 },
		"bad shard":           func(o *options) { o.yes, o.shard = true, "2/2" },
		"negative limit":      func(o *options) { o.yes, o.limit = true, -1 },
		"zero batch":          func(o *options) { o.batch = 0 },
		"human w/o calibrate": func(o *options) { o.human = []string{"cli"} },
		"yes with rung0-only": func(o *options) { o.yes, o.rung0Only = true, true },
		"yes with dump":       func(o *options) { o.yes, o.dump = true, "/tmp/x.jsonl" },
	} {
		o := ok
		mut(&o)
		if err := validate(o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	o := ok
	o.issueType = decidepkg.IssueHomophone
	if err := validate(o); err != nil {
		t.Errorf("homophone refused: %v", err)
	}
}

// emptyStore is a backlog with nothing in scope.
type emptyStore struct {
	scope db.DecideScope
	work  []db.DecideWorkScope
}

func (s *emptyStore) RegisterRecipe(_ context.Context, r recipe.Recipe) (string, error) {
	return r.ID()
}
func (s *emptyStore) LookupFnCache(context.Context, db.FnCacheKey, string) (*db.FnCall, bool, error) {
	return nil, false, nil
}
func (s *emptyStore) InsertFnCall(context.Context, db.FnCall) (int64, bool, error) {
	return 1, true, nil
}
func (s *emptyStore) DecideSample(_ context.Context, sc db.DecideScope) ([]db.DecideFinding, error) {
	s.scope = sc
	return nil, nil
}
func (s *emptyStore) DecideBacklog(context.Context, db.DecideScope) (map[string]int, error) {
	return map[string]int{}, nil
}
func (s *emptyStore) DecideChunks(context.Context, []db.ChunkKey) (map[db.ChunkKey]*db.DecideChunk, error) {
	return nil, nil
}
func (s *emptyStore) GetTranscriptSegments(context.Context, []string) (map[string][]db.Segment, error) {
	return nil, nil
}
func (s *emptyStore) GetBookRecords(context.Context, []string) (map[string]db.BookRecord, error) {
	return nil, nil
}
func (s *emptyStore) DecideWork(_ context.Context, w db.DecideWorkScope) ([]db.DecideFinding, error) {
	s.work = append(s.work, w)
	return nil, nil
}
func (s *emptyStore) ApplyDecisions(context.Context, string, []db.DecisionEvent) (db.ApplyResult, error) {
	panic("nothing to write")
}
func (s *emptyStore) ApplyRecheckDecisions(context.Context, string, []db.DecisionEvent) (db.ApplyResult, error) {
	panic("nothing to write")
}

type noAsker struct{}

func (noAsker) Decide(context.Context, systemone.Request) (*systemone.Response, error) {
	panic("no finding, no call")
}

func TestRunPassesScope(t *testing.T) {
	store := &emptyStore{}
	var out bytes.Buffer
	o := options{sample: 7, seed: "q4", book: "Dune", issueType: decidepkg.IssueHomophone, concurrency: 2, calibrate: true, human: []string{"mcp", "cli"}, batch: 20}
	if err := resolvedRun(context.Background(), &out, store, noAsker{}, o); err != nil {
		t.Fatal(err)
	}
	want := db.DecideScope{Sample: 7, Seed: "q4", Book: "Dune", IssueType: decidepkg.IssueHomophone, Calibrate: true, HumanActors: []string{"mcp", "cli"}}
	if !reflect.DeepEqual(store.scope, want) {
		t.Errorf("scope %+v, want %+v", store.scope, want)
	}
	if !strings.Contains(out.String(), "No findings in scope.") || !strings.Contains(out.String(), "calibration") {
		t.Errorf("output:\n%s", out.String())
	}

	out.Reset()
	o.json = true
	if err := resolvedRun(context.Background(), &out, store, noAsker{}, o); err != nil {
		t.Fatal(err)
	}
	var rep decidepkg.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || !rep.DryRun || !rep.Calibrate || rep.Findings != 0 {
		t.Errorf("json report %+v, err %v", rep, err)
	}

	if err := resolvedRun(context.Background(), &out, store, noAsker{}, options{sample: 0, seed: "s", concurrency: 1, batch: 20}); err == nil {
		t.Error("invalid options ran")
	}

	// --yes walks the work list with the shard and filters.
	out.Reset()
	if err := resolvedRun(context.Background(), &out, store, noAsker{}, options{sample: 100, seed: "s", concurrency: 1, batch: 20,
		yes: true, shard: "1/4", book: "Dune", issueType: decidepkg.IssueHomophone, limit: 10}); err != nil {
		t.Fatal(err)
	}
	if len(store.work) != 1 || store.work[0].Shard != 1 || store.work[0].Shards != 4 || store.work[0].Limit != 10 ||
		store.work[0].Book != "Dune" || store.work[0].RetryReason != decidepkg.ReasonJevUnavailable {
		t.Errorf("work scope %+v", store.work)
	}
	if !strings.Contains(out.String(), "run (--yes)") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestParseShard(t *testing.T) {
	for in, want := range map[string][2]int{"": {0, 0}, "0/1": {0, 1}, "3/4": {3, 4}} {
		a, b, err := parseShard(in)
		if err != nil || a != want[0] || b != want[1] {
			t.Errorf("%q = %d/%d, %v", in, a, b, err)
		}
	}
	for _, bad := range []string{"4/4", "-1/4", "1/0", "1", "a/b", "1/4x", " 1/4"} {
		if _, _, err := parseShard(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// registerSpy is emptyStore with a register hook, so a test can see whether a
// run registered a recipe.
type registerSpy struct {
	emptyStore
	registered int
}

func (s *registerSpy) RegisterRecipe(ctx context.Context, r recipe.Recipe) (string, error) {
	s.registered++
	return s.emptyStore.RegisterRecipe(ctx, r)
}

// --rung0-only runs without an asker and registers no recipe; --dump writes
// the file (empty for an empty scope).
func TestRunRung0OnlyDump(t *testing.T) {
	store := &registerSpy{}
	path := filepath.Join(t.TempDir(), "r0.jsonl")
	var out bytes.Buffer
	o := options{sample: 5, seed: "s", concurrency: 1, batch: 20, rung0Only: true, dump: path}
	if err := resolvedRun(context.Background(), &out, store, nil, o); err != nil {
		t.Fatal(err)
	}
	if store.registered != 0 {
		t.Errorf("rung-0 replay registered %d recipes", store.registered)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) != 0 {
		t.Errorf("dump %q, %v", b, err)
	}
	if !strings.Contains(out.String(), "rung 0 only") {
		t.Errorf("output:\n%s", out.String())
	}

	// A dump into a missing directory fails before any work.
	o.dump = filepath.Join(t.TempDir(), "missing", "r0.jsonl")
	if err := resolvedRun(context.Background(), &out, store, nil, o); err == nil || !strings.Contains(err.Error(), "--dump") {
		t.Errorf("bad dump path: %v", err)
	}
}

type failingSample struct{ emptyStore }

func (failingSample) DecideSample(context.Context, db.DecideScope) ([]db.DecideFinding, error) {
	return nil, errors.New("boom")
}

// A failed run leaves no partial dump behind.
func TestRunDumpRemovedOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r0.jsonl")
	o := options{sample: 5, seed: "s", concurrency: 1, batch: 20, rung0Only: true, dump: path}
	if err := resolvedRun(context.Background(), &bytes.Buffer{}, &failingSample{}, nil, o); err == nil {
		t.Fatal("store error swallowed")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial dump left behind: %v", err)
	}
}

// modelSpy records the recipes a run registers.
type modelSpy struct {
	emptyStore
	recipes []recipe.Recipe
}

func (s *modelSpy) RegisterRecipe(ctx context.Context, r recipe.Recipe) (string, error) {
	s.recipes = append(s.recipes, r)
	return s.emptyStore.RegisterRecipe(ctx, r)
}

// The resolved decide model (a model setting or --model, CONTRACT §2.20)
// flows into the registered recipe, the report, and step_runs' args; the
// zero model is the built-in one.
func TestRunUsesResolvedModel(t *testing.T) {
	for _, tc := range []struct {
		model     decidepkg.Model
		wantAlias string
	}{
		{decidepkg.Model{}, decidepkg.ShouldApplyModel},
		{decidepkg.Model{Alias: "clef-1.0.0", Expected: "typesafe/clef-1.0.0"}, "clef-1.0.0"},
	} {
		store := &modelSpy{}
		var out bytes.Buffer
		o := options{sample: 5, seed: "s", concurrency: 1, batch: 20, json: true, model: tc.model}
		if err := resolvedRun(context.Background(), &out, store, noAsker{}, o); err != nil {
			t.Fatal(err)
		}
		if len(store.recipes) != 1 || store.recipes[0].ModelAlias != tc.wantAlias {
			t.Fatalf("registered %+v, want model %s", store.recipes, tc.wantAlias)
		}
		if tc.model.Expected != "" && store.recipes[0].ModelResolved != tc.model.Expected {
			t.Errorf("model_resolved %q, want the pin %q", store.recipes[0].ModelResolved, tc.model.Expected)
		}
		var rep decidepkg.Report
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil || rep.Model != tc.wantAlias {
			t.Errorf("report model %q (err %v), want %s", rep.Model, err, tc.wantAlias)
		}
	}
	if got := argsSummary(options{sample: 5, seed: "s", overrideEndpoint: "clef", overrideModel: "clef-1.0.0"}); !strings.Contains(got, "--endpoint clef --model clef-1.0.0") {
		t.Errorf("args %q do not record the override", got)
	}
}

// The bake-off runs `decide --model clef:27b` as a dry run in every mode
// (sample, calibrate --human, rung-0 replay) and as --yes: the resolved model
// reaches the report (and the recipe whenever one is registered) in each.
// Overrides never write model_settings: run() has no settings store at all,
// and ResolveStep only reads (db.TestResolveStep).
func TestRunModelInEveryMode(t *testing.T) {
	clef := decidepkg.Model{Alias: "clef:27b"}
	modes := map[string]options{
		"sample":       {sample: 84, seed: "bake", concurrency: 1, batch: 20, json: true, model: clef},
		"calibrate":    {sample: 84, seed: "bake", concurrency: 1, batch: 20, json: true, calibrate: true, human: []string{"mcp", "cli"}, model: clef},
		"rung0-only":   {sample: 84, seed: "bake", concurrency: 1, batch: 20, json: true, rung0Only: true, model: clef},
		"yes (--live)": {sample: 100, seed: "s", concurrency: 1, batch: 20, json: true, yes: true, limit: 10, model: clef},
	}
	for name, o := range modes {
		t.Run(name, func(t *testing.T) {
			store := &modelSpy{}
			var out bytes.Buffer
			var asker decidepkg.Asker = noAsker{}
			if o.rung0Only {
				asker = nil
			}
			if err := resolvedRun(context.Background(), &out, store, asker, o); err != nil {
				t.Fatal(err)
			}
			var rep decidepkg.Report
			if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
				t.Fatalf("report: %v\n%s", err, out.String())
			}
			if rep.Model != "clef:27b" {
				t.Errorf("report model %q", rep.Model)
			}
			for _, r := range store.recipes {
				if r.ModelAlias != "clef:27b" {
					t.Errorf("registered a %s recipe", r.ModelAlias)
				}
			}
			if o.rung0Only != (len(store.recipes) == 0) {
				t.Errorf("registered %d recipes (rung0-only %v)", len(store.recipes), o.rung0Only)
			}
		})
	}
}

// settingsRows is a model_settings reader serving fixed rows.
type settingsRows []db.ModelSetting

func (r settingsRows) ListModelSettings(context.Context) ([]db.ModelSetting, error) { return r, nil }

func policyConfig() *config.Config {
	return &config.Config{
		AIEndpoints: []config.AIEndpoint{
			{ID: "litellm-jev", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000/typesafe", Model: "jev-1.13.0"},
			{ID: "litellm-s1", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000", Model: "clef:27b"},
		},
		AIRoles: &config.AIRoles{Decide: "litellm-jev"},
		Models:  &config.ModelRegistry{Steps: map[string]config.ModelPin{}},
	}
}

// `decide --model clef:27b` decides under Clef's policy in every mode — the
// dry runs and --yes resolve alike, so a dry run's outcomes are the write's —
// and --policy layers over it for the run.
func TestResolvePolicyInEveryMode(t *testing.T) {
	modes := map[string]options{
		"sample":     {sample: 84, seed: "bake", concurrency: 1, batch: 20, json: true},
		"calibrate":  {sample: 84, seed: "bake", concurrency: 1, batch: 20, json: true, calibrate: true},
		"rung0-only": {sample: 84, seed: "bake", concurrency: 1, batch: 20, json: true, rung0Only: true},
		"yes":        {sample: 100, seed: "s", concurrency: 1, batch: 20, json: true, yes: true, limit: 10},
	}
	for name, o := range modes {
		t.Run(name, func(t *testing.T) {
			o.overrideEndpoint, o.overrideModel = "litellm-s1", "clef:27b"
			_, o, err := resolve(context.Background(), settingsRows(nil), policyConfig(), o)
			if err != nil {
				t.Fatal(err)
			}
			if o.model.Alias != "clef:27b" || o.policy.Preset != decidepkg.PresetClef {
				t.Fatalf("resolved model %+v policy %+v", o.model, o.policy)
			}
			store := &modelSpy{}
			var out bytes.Buffer
			var asker decidepkg.Asker = noAsker{}
			if o.rung0Only {
				asker = nil
			}
			if err := resolvedRun(context.Background(), &out, store, asker, o); err != nil {
				t.Fatal(err)
			}
			var rep decidepkg.Report
			if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
				t.Fatalf("report: %v\n%s", err, out.String())
			}
			if rep.PolicySource != "preset clef" || rep.Policy[config.ParamRejectRequiresLowercase] != true ||
				rep.Policy[config.ParamRejectFloor] != 0.1 {
				t.Errorf("report policy %v (%s)", rep.Policy, rep.PolicySource)
			}
			for _, r := range store.recipes {
				if ev, _ := r.Params["apply_evidence"].([]string); len(ev) != 1 || ev[0] != decidepkg.EvidenceASINVerbatim {
					t.Errorf("registered recipe apply_evidence %v", r.Params["apply_evidence"])
				}
			}
		})
	}

	// A model_settings row's params, then --policy, layer over the preset.
	rows := settingsRows{{Step: recipe.StepDecide, EndpointID: "litellm-s1", Model: "clef:27b",
		Params: map[string]any{config.ParamRejectFloor: 0.05}}}
	o := options{sample: 5, seed: "s", concurrency: 1, batch: 20}
	_, got, err := resolve(context.Background(), rows, policyConfig(), o)
	if err != nil || got.policy.Params.RejectFloor != 0.05 || got.policy.Source != decidepkg.PolicySourceDB {
		t.Fatalf("row policy %+v, %v", got.policy, err)
	}
	o.policyFlags = []string{"apply_p=0.9"}
	_, got, err = resolve(context.Background(), rows, policyConfig(), o)
	if err != nil || got.policy.Params.ApplyP != 0.9 || got.policy.Params.RejectFloor != 0.05 ||
		got.policy.SourceLabel() != "override (apply_p) + db (reject_floor) over preset clef" {
		t.Fatalf("override policy %+v (%s), %v", got.policy, got.policy.SourceLabel(), err)
	}
	if !strings.Contains(argsSummary(o), "--policy apply_p=0.9") {
		t.Errorf("args %q do not record --policy", argsSummary(o))
	}

	// Bad values fail before anything runs.
	for _, flags := range [][]string{{"apply_p=0.1"}, {"bogus=1"}, {"reject_floor=0.5"}} {
		o.policyFlags = flags
		if _, _, err := resolve(context.Background(), rows, policyConfig(), o); err == nil {
			t.Errorf("--policy %v accepted", flags)
		}
	}
}

// resolvedRun is run with the policy resolve would give the options' model
// (its preset, else the default) when the test set none.
func resolvedRun(ctx context.Context, out io.Writer, store decidepkg.WriteStore, asker decidepkg.Asker, o options) error {
	if o.policy.Source == "" {
		o.policy = decidepkg.Policy{Params: decidepkg.DefaultShouldApplyParams(), Source: decidepkg.PolicySourceDefault}
		if name, p, ok := decidepkg.PresetFor(modelOrDefault(o.model)); ok {
			o.policy = decidepkg.Policy{Params: p, Source: decidepkg.PolicySourcePreset, Preset: name}
		}
	}
	return run(ctx, out, store, asker, o)
}

// run refuses options whose policy was never resolved, rather than falling
// back to a policy other than the model's.
func TestRunNeedsResolvedPolicy(t *testing.T) {
	o := options{sample: 5, seed: "s", concurrency: 1, batch: 20, model: decidepkg.Model{Alias: "clef:27b"}}
	err := run(context.Background(), &bytes.Buffer{}, &modelSpy{}, noAsker{}, o)
	if err == nil || !strings.Contains(err.Error(), "not resolved") {
		t.Errorf("unresolved policy: %v", err)
	}
}
