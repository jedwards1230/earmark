package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

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
	if err := run(context.Background(), &out, store, noAsker{}, o); err != nil {
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
	if err := run(context.Background(), &out, store, noAsker{}, o); err != nil {
		t.Fatal(err)
	}
	var rep decidepkg.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || !rep.DryRun || !rep.Calibrate || rep.Findings != 0 {
		t.Errorf("json report %+v, err %v", rep, err)
	}

	if err := run(context.Background(), &out, store, noAsker{}, options{sample: 0, seed: "s", concurrency: 1, batch: 20}); err == nil {
		t.Error("invalid options ran")
	}

	// --yes walks the work list with the shard and filters.
	out.Reset()
	if err := run(context.Background(), &out, store, noAsker{}, options{sample: 100, seed: "s", concurrency: 1, batch: 20,
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
