package eval

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
)

// seededReader records the seed it was asked for.
type seededReader struct {
	fakeReader
	gotSeed  string
	gotLimit int
}

func (s *seededReader) SampleEvalChunksSeeded(_ context.Context, limit int, seed string) ([]db.EvalChunk, error) {
	s.gotSeed, s.gotLimit = seed, limit
	return s.chunks, nil
}

// TestRun_SeedSelectsSeededSample: --seed routes the sample through the seeded
// reader; a seed without a sample size, or with a reader that cannot sample
// by seed, is an error rather than a silent unseeded sample.
func TestRun_SeedSelectsSeededSample(t *testing.T) {
	r := &seededReader{fakeReader: fakeReader{chunks: []db.EvalChunk{sampleChunk()}}}
	_, stats, err := Run(context.Background(), r, NewJudge(&fakeChat{resp: `{"findings":[]}`}), &fakeWriter{},
		RunOptions{Sample: 7, Seed: "q4"})
	if err != nil {
		t.Fatal(err)
	}
	if r.gotSeed != "q4" || r.gotLimit != 7 || stats.ChunksEvaluated != 1 {
		t.Errorf("seeded reader got seed %q limit %d, evaluated %d", r.gotSeed, r.gotLimit, stats.ChunksEvaluated)
	}

	if _, _, err := Run(context.Background(), r, NewJudge(&fakeChat{}), &fakeWriter{}, RunOptions{Seed: "q4", Book: "x"}); err == nil {
		t.Error("seed without a sample size accepted")
	}
	if _, _, err := Run(context.Background(), fakeReader{}, NewJudge(&fakeChat{}), &fakeWriter{}, RunOptions{Sample: 3, Seed: "q4"}); err == nil {
		t.Error("seed accepted by a reader that cannot sample by seed")
	}
}

// TestRun_ObserveSeesEveryChunk: the Observe hook is called once per chunk,
// in order, including a chunk the judge failed on — the --dump harness must
// account for every call it paid for.
func TestRun_ObserveSeesEveryChunk(t *testing.T) {
	chat := &scriptedChat{steps: []step{
		{resp: oneFinding},
		{err: errors.New("connection reset by peer")},
		{resp: oneFinding},
	}}
	var seen []string
	var errs int
	_, _, err := Run(context.Background(), fakeReader{chunks: threeChunks()}, NewJudge(chat), &fakeWriter{},
		RunOptions{Book: "Book", Observe: func(r Result, err error) {
			seen = append(seen, r.Chunk.ChunkID)
			if err != nil {
				errs++
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{"a", "b", "c"}) || errs != 1 {
		t.Errorf("observed %v with %d errors, want [a b c] with 1", seen, errs)
	}
}

// TestJudgeChunk_RecordsDrops: every finding the judge returned is either a
// row or a Dropped entry with its reason, and RunStats counts the reasons.
func TestJudgeChunk_RecordsDrops(t *testing.T) {
	t.Setenv("EVAL_MIN_CONFIDENCE", "0.6")
	t.Setenv("EVAL_MAX_FINDINGS_PER_CHUNK", "1")
	resp := `{"findings":[
		{"original_text":"","issue_type":"misheard_word","suggested_correction":"x","confidence":0.9},
		{"original_text":"fox","issue_type":"misheard_word","suggested_correction":"","confidence":0.9},
		{"original_text":"french","issue_type":"misheard_word","suggested_correction":"French","confidence":0.9},
		{"original_text":"fox","issue_type":"misheard_word","suggested_correction":"folks","confidence":0.3},
		{"original_text":"quick","issue_type":"misheard_word","suggested_correction":"quack","confidence":0.7},
		{"original_text":"lazy","issue_type":"misheard_word","suggested_correction":"hazy","confidence":0.8}
	]}`
	_, stats, err := Run(context.Background(), fakeReader{chunks: []db.EvalChunk{sampleChunk()}},
		NewJudge(&fakeChat{resp: resp}), &fakeWriter{}, RunOptions{Book: "b"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		DropEmptySpan: 1, DropEmptyCorrection: 1, DropCosmeticOnly: 1,
		DropBelowMinConfidence: 1, DropOverCap: 1,
	}
	for k, v := range want {
		if stats.Dropped[k] != v {
			t.Errorf("Dropped[%s] = %d, want %d (all: %v)", k, stats.Dropped[k], v, stats.Dropped)
		}
	}
	if stats.FindingsFound != 1 {
		t.Errorf("FindingsFound = %d, want 1", stats.FindingsFound)
	}

	res, err := NewJudge(&fakeChat{resp: resp}).JudgeChunk(context.Background(), sampleChunk())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(res.Findings) + len(res.Dropped); got != 6 {
		t.Errorf("kept + dropped = %d, want all 6 returned findings accounted for", got)
	}
	for _, d := range res.Dropped {
		if d.Reason == DropOverCap && d.OriginalText != "quick" {
			t.Errorf("cap dropped %q, want the lower-confidence \"quick\"", d.OriginalText)
		}
	}
}

// TestCompleteWithModel_ReportsUsageAndCost: token usage and the gateway's
// x-litellm-response-cost reach the Result; a bad or missing header is "no
// cost", never 0 reported as a price.
func TestCompleteWithModel_ReportsUsageAndCost(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		wantCost     bool
		want         float64
	}{
		{"reported", "0.000123", true, 0.000123},
		{"absent", "", false, 0},
		{"garbage", "n/a", false, 0},
		{"negative", "-1", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.header != "" {
					w.Header().Set("x-litellm-response-cost", tc.header)
				}
				_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"{\"findings\":[]}"},` +
					`"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":34}}`))
			}))
			defer srv.Close()
			c, err := ResolveChatClient(fakeSource{ok: true, ep: EvalEndpoint{BaseURL: srv.URL + "/v1", Model: "m"}})
			if err != nil {
				t.Fatal(err)
			}
			res, err := NewJudge(c).JudgeChunk(context.Background(), sampleChunk())
			if err != nil {
				t.Fatal(err)
			}
			u := res.Usage
			if !u.HasUsage || u.InputTokens != 1200 || u.OutputTokens != 34 {
				t.Errorf("usage = %+v", u)
			}
			if u.HasCost != tc.wantCost || u.CostUSD != tc.want {
				t.Errorf("cost = %v/%v, want %v/%v", u.CostUSD, u.HasCost, tc.want, tc.wantCost)
			}
		})
	}
}
