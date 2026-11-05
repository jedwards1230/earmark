package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	decidepkg "github.com/jedwards1230/earmark/internal/decide"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// Fixture pilot models: opaque strings, not the deployment's pins.
const (
	fixA = "org-a/asr-a@1111111111111111111111111111111111111111"
	fixB = "org-b/asr-b@2222222222222222222222222222222222222222"
)

func audioOpts() options {
	return options{sample: 100, seed: "s", concurrency: defaultConcurrency, batch: 20, audioVote: true}
}

func TestValidateAudioVote(t *testing.T) {
	for name, mut := range map[string]func(*options){
		"dry run":                  func(*options) {},
		"yes":                      func(o *options) { o.yes = true },
		"dry run with limit":       func(o *options) { o.limit = 2000 },
		"dry run with max-accepts": func(o *options) { o.maxAccepts = 500 },
		"dry run with shard":       func(o *options) { o.shard = "0/4" },
		"yes with all":             func(o *options) { o.yes, o.limit, o.maxAccepts, o.shard, o.batch = true, 2000, 500, "1/4", 50 },
		"book, issue, json":        func(o *options) { o.book, o.issueType, o.json = "Dune", decidepkg.IssueHomophone, true },
		"sample and seed":          func(o *options) { o.sample, o.sampleSet, o.seed = 5, true, "q4" },
		"sample zero":              func(o *options) { o.sample, o.sampleSet = 0, true },
		"flag config":              func(o *options) { o.audioRule, o.audioModels = "asr_vote@v2", []string{fixA, fixB} },
	} {
		o := audioOpts()
		mut(&o)
		if err := validate(o); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	for name, mut := range map[string]func(*options){
		"calibrate":             func(o *options) { o.calibrate = true },
		"rung0-only":            func(o *options) { o.rung0Only = true },
		"dump":                  func(o *options) { o.dump = "/tmp/x.jsonl" },
		"human":                 func(o *options) { o.human = []string{"cli"} },
		"endpoint":              func(o *options) { o.overrideEndpoint = "e" },
		"model":                 func(o *options) { o.overrideModel = "clef:27b" },
		"policy":                func(o *options) { o.policyFlags = []string{"apply_p=0.9"} },
		"concurrency":           func(o *options) { o.concurrencySet = true },
		"negative sample":       func(o *options) { o.sample, o.sampleSet = -1, true },
		"yes with sample":       func(o *options) { o.yes, o.sampleSet = true, true },
		"models without rule":   func(o *options) { o.audioModels = []string{fixA, fixB} },
		"rule without models":   func(o *options) { o.audioRule = "asr_vote@v2" },
		"bad shard":             func(o *options) { o.shard = "4/4" },
		"audio-model alone":     func(o *options) { o.audioVote, o.audioModels = false, []string{fixA} },
		"audio-rule alone":      func(o *options) { o.audioVote, o.audioRule = false, "r" },
		"limit without yes/avs": func(o *options) { o.audioVote, o.limit = false, 5 },
	} {
		o := audioOpts()
		mut(&o)
		if err := validate(o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAudioConfig(t *testing.T) {
	file := &config.AudioVoteConfig{Rule: "asr_vote@v2", Models: []config.AudioVoteModel{
		{ModelID: "org-c/asr-c", Revision: "3"}, {ModelID: "org-d/asr-d", Revision: "4"},
	}}
	withFile := &config.Config{Models: &config.ModelRegistry{AudioVote: file}}
	got, err := audioConfig(withFile, audioOpts())
	if err != nil || got.Rule != file.Rule || len(got.Models) != 2 || got.Models[0].ModelID != "org-c/asr-c" {
		t.Errorf("file block: %+v, %v", got, err)
	}
	// Flags replace the file block wholesale.
	o := audioOpts()
	o.audioRule, o.audioModels = "asr_vote@v9", []string{fixA, fixB}
	got, err = audioConfig(withFile, o)
	if err != nil || got.Rule != "asr_vote@v9" || len(got.Models) != 2 || got.Models[0].ModelID != "org-a/asr-a" ||
		got.Models[1].Revision != "2222222222222222222222222222222222222222" {
		t.Errorf("flags: %+v, %v", got, err)
	}
	// Absent: a clear error naming both ways to configure it.
	if _, err := audioConfig(&config.Config{Models: &config.ModelRegistry{}}, audioOpts()); err == nil ||
		!strings.Contains(err.Error(), "MODELS_FILE") || !strings.Contains(err.Error(), "--audio-model") {
		t.Errorf("absent config: %v", err)
	}
	o.audioModels = []string{fixA}
	if _, err := audioConfig(withFile, o); err == nil {
		t.Error("one --audio-model accepted")
	}
	o.audioModels = []string{fixA, "no-revision"}
	if _, err := audioConfig(withFile, o); err == nil {
		t.Error("a model without @revision accepted")
	}
}

// emptyAudioStore has no held finding with a vote.
type emptyAudioStore struct {
	registered int
}

func (s *emptyAudioStore) DecideChunks(context.Context, string, []db.ChunkKey) (map[db.ChunkKey]*db.DecideChunk, error) {
	return nil, nil
}
func (s *emptyAudioStore) GetTranscriptSegments(context.Context, []string) (map[string][]db.Segment, error) {
	return nil, nil
}
func (s *emptyAudioStore) GetBookRecords(context.Context, []string) (map[string]db.BookRecord, error) {
	return nil, nil
}
func (s *emptyAudioStore) ApplyDecisions(context.Context, string, []db.DecisionEvent) (db.ApplyResult, error) {
	return db.ApplyResult{}, nil
}
func (s *emptyAudioStore) ApplyRecheckDecisions(context.Context, string, []db.DecisionEvent) (db.ApplyResult, error) {
	return db.ApplyResult{}, nil
}
func (s *emptyAudioStore) RegisterRecipe(_ context.Context, r recipe.Recipe) (string, error) {
	s.registered++
	return r.ID()
}
func (s *emptyAudioStore) AudioVoteWork(context.Context, db.AudioVoteScope) ([]db.AudioVoteFinding, error) {
	return nil, nil
}
func (s *emptyAudioStore) CurrentASRVotes(context.Context, []string, string, []config.AudioVoteModel) (map[string][]db.ASRVote, error) {
	return nil, nil
}
func (s *emptyAudioStore) ListCurrentRecipes(context.Context) ([]db.CurrentRecipe, error) {
	return nil, nil
}
func (s *emptyAudioStore) LatestDecisions(context.Context, []string) (map[string]db.LatestDecision, error) {
	return nil, nil
}

func TestRunAudio(t *testing.T) {
	o := audioOpts()
	o.audioRule, o.audioModels = "asr_vote@v2", []string{fixA, fixB}
	cfg, err := audioConfig(nil, o)
	if err != nil {
		t.Fatal(err)
	}
	store := &emptyAudioStore{}
	var out bytes.Buffer
	if err := runAudio(context.Background(), &out, store, cfg, o); err != nil {
		t.Fatal(err)
	}
	if store.registered != 0 || !strings.Contains(out.String(), "decide --audio-vote dry run · recipe ") ||
		!strings.Contains(out.String(), "org-a/asr-a@") {
		t.Errorf("dry run registered %d recipes; output:\n%s", store.registered, out.String())
	}
	o.yes, o.json = true, true
	out.Reset()
	if err := runAudio(context.Background(), &out, store, cfg, o); err != nil {
		t.Fatal(err)
	}
	var rep decidepkg.AudioVoteReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || rep.DryRun || rep.Write == nil || len(rep.RecipeID) != 64 || store.registered != 1 {
		t.Errorf("--yes --json: %v %+v (registered %d)", err, rep, store.registered)
	}
	if got := argsSummary(o); got != "--audio-vote --yes" {
		t.Errorf("args %q", got)
	}
	o.yes = false
	o.limit = 5
	if got := argsSummary(o); got != "--audio-vote --limit 5" {
		t.Errorf("args %q", got)
	}
	o.audioVote = false
	o.audioRule, o.audioModels = "", nil
	if err := runAudio(context.Background(), &out, store, cfg, o); err == nil {
		t.Error("runAudio without --audio-vote ran")
	}
}
