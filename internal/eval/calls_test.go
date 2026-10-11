package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/genai"
	"github.com/jedwards1230/earmark/internal/recipe"
)

const validReply = `{"findings":[{"original_text":"pin name","issue_type":"homophone","suggested_correction":"pen name","confidence":0.9,"anchor_offset":-1,"anchor_occurrence":-1}]}`

// chatReply is an OpenAI chat-completions body with content.
func chatReply(content string) string {
	b, _ := json.Marshal(map[string]any{
		"model": "m",
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
	})
	return string(b)
}

func pinNameChunk() db.EvalChunk {
	c := sampleChunk()
	c.Text = "she wrote under a pin name for years"
	return c
}

// scriptedServer answers each request with the next scripted response (the
// last one repeats) and counts requests.
type scriptedServer struct {
	srv   *httptest.Server
	calls atomic.Int32
}

type httpStep struct {
	status     int
	retryAfter string
	body       string
	hang       bool // block until the request is cancelled
}

func newScriptedServer(t *testing.T, steps ...httpStep) *scriptedServer {
	t.Helper()
	s := &scriptedServer{}
	stop := make(chan struct{})
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(s.calls.Add(1)) - 1
		st := steps[min(n, len(steps)-1)]
		if st.hang {
			select {
			case <-r.Context().Done():
			case <-stop:
			}
			return
		}
		if st.retryAfter != "" {
			w.Header().Set("Retry-After", st.retryAfter)
		}
		w.WriteHeader(st.status)
		_, _ = w.Write([]byte(st.body))
	}))
	t.Cleanup(s.srv.Close)
	t.Cleanup(func() { close(stop) }) // runs first: releases hung handlers so Close returns
	return s
}

// judgeFor builds a judge on the real HTTP client against srv, with retries
// and a recording sleep (no real waiting).
func judgeFor(srv *httptest.Server, p CallPolicy) (*Judge, *[]time.Duration) {
	j := NewJudge(newOpenAIChatClient(chatConfig{BaseURL: srv.URL, Model: "m"}))
	j.SetCallPolicy(p)
	var waits []time.Duration
	j.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	return j, &waits
}

// TestJudgeChunk_RetryBackoffHonoursRetryAfter: a 429 with Retry-After waits
// exactly what the endpoint asked, a 503 without one waits the jittered
// backoff, and the third attempt's reply is used.
func TestJudgeChunk_RetryBackoffHonoursRetryAfter(t *testing.T) {
	s := newScriptedServer(t,
		httpStep{status: 429, retryAfter: "7", body: "slow down"},
		httpStep{status: 503, body: "busy"},
		httpStep{status: 200, body: chatReply(validReply)},
	)
	j, waits := judgeFor(s.srv, CallPolicy{Retries: 4, BackoffBase: 10 * time.Millisecond, BackoffMax: time.Second})
	res, err := j.JudgeChunk(context.Background(), pinNameChunk())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.calls.Load(); got != 3 || res.Attempts != 3 {
		t.Fatalf("calls %d, attempts %d; want 3, 3", got, res.Attempts)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("findings %d, want 1", len(res.Findings))
	}
	if len(*waits) != 2 || (*waits)[0] != 7*time.Second {
		t.Fatalf("waits %v: want Retry-After 7s first", *waits)
	}
	// The 503 was attempt 1: base·2 = 20ms, equal jitter → [10ms, 20ms].
	if w := (*waits)[1]; w < 10*time.Millisecond || w > 20*time.Millisecond {
		t.Errorf("backoff %v outside the equal-jitter band [10ms, 20ms] of attempt 1", w)
	}
}

// TestJudgeChunk_RetryPolicy: what is and is not retried.
func TestJudgeChunk_RetryPolicy(t *testing.T) {
	cases := []struct {
		name      string
		steps     []httpStep
		timeout   time.Duration
		wantCalls int32
		wantErr   func(error) bool
	}{
		{"invalid reply never retried", []httpStep{{status: 200, body: chatReply("I cannot help with that.")}}, 0, 1,
			func(err error) bool { return errors.Is(err, ErrInvalidReply) }},
		{"truncated reply never retried", []httpStep{{status: 200, body: `{"model":"m","choices":[{"message":{"content":"{"},"finish_reason":"length"}]}`}}, 0, 1,
			func(err error) bool { return errors.Is(err, ErrTruncatedResponse) }},
		{"400 not retried", []httpStep{{status: 400, body: "bad"}}, 0, 1,
			func(err error) bool { var se *StatusError; return errors.As(err, &se) && se.Code == 400 }},
		{"5xx retried until exhausted", []httpStep{{status: 500, body: "boom"}}, 0, 3,
			func(err error) bool { var se *StatusError; return errors.As(err, &se) && se.Code == 500 }},
		{"timeout retried", []httpStep{{hang: true}, {status: 200, body: chatReply(validReply)}}, 50 * time.Millisecond, 2,
			func(err error) bool { return err == nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedServer(t, tc.steps...)
			j, _ := judgeFor(s.srv, CallPolicy{Retries: 2, Timeout: tc.timeout, BackoffBase: time.Millisecond})
			_, err := j.JudgeChunk(context.Background(), pinNameChunk())
			if !tc.wantErr(err) {
				t.Fatalf("unexpected error %v", err)
			}
			if got := s.calls.Load(); got != tc.wantCalls {
				t.Errorf("calls %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestJudgeChunk_NoRetryAfterCallerStops: a cancelled caller is never
// retried, however retryable the failure looked.
func TestJudgeChunk_NoRetryAfterCallerStops(t *testing.T) {
	s := newScriptedServer(t, httpStep{hang: true})
	j, _ := judgeFor(s.srv, CallPolicy{Retries: 4, BackoffBase: time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := j.JudgeChunk(ctx, pinNameChunk()); err == nil {
		t.Fatal("want an error")
	}
	if got := s.calls.Load(); got != 1 {
		t.Errorf("calls %d after the caller stopped, want 1", got)
	}
}

func TestRetryAfterParse(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"":                              0,
		"12":                            12 * time.Second,
		"-3":                            0,
		"soon":                          0,
		"Sun, 11 Oct 2026 12:00:30 GMT": 30 * time.Second,
		"Sun, 11 Oct 2026 11:00:00 GMT": 0,
	} {
		if got := retryAfter(in, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

// memCallLog is an in-memory fn_calls with the real cache rule: a row serves
// when it succeeded, is not itself a cache hit, and its model matches.
type memCallLog struct {
	mu      sync.Mutex
	rows    []db.FnCall
	recipes map[string]recipe.Recipe
}

func (m *memCallLog) RegisterRecipe(_ context.Context, r recipe.Recipe) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, err := r.ID()
	if m.recipes == nil {
		m.recipes = map[string]recipe.Recipe{}
	}
	m.recipes[id] = r
	return id, err
}

func (m *memCallLog) LookupFnCache(_ context.Context, k db.FnCacheKey, expected string) (*db.FnCall, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if expected == "" {
		expected = k.ModelAlias
	}
	for i := range m.rows {
		r := m.rows[i]
		if r.Key() == k && r.ErrorClass == "" && !r.CacheHit && genai.SameModel(r.ModelResolved, expected) {
			return &r, true, nil
		}
	}
	return nil, false, nil
}

func (m *memCallLog) InsertFnCall(_ context.Context, c db.FnCall) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ErrorClass == "" && !c.CacheHit {
		for _, r := range m.rows {
			if r.Key() == c.Key() && r.ErrorClass == "" && !r.CacheHit {
				return 0, false, nil
			}
		}
	}
	c.ID = int64(len(m.rows) + 1)
	m.rows = append(m.rows, c)
	return c.ID, true, nil
}

func (m *memCallLog) all() []db.FnCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]db.FnCall(nil), m.rows...)
}

// TestJudgeChunk_CallLogCachesAndServes: the first judgement is logged as a
// judge_chunk row (input with the chunk, path and request params; output with
// the reply and the kept/dropped findings); the same chunk again is served
// from it — no model call — with the findings pointing at the original row,
// and logged as a cache hit. An invalid reply is logged as invalid_reply and
// never served.
func TestJudgeChunk_CallLogCachesAndServes(t *testing.T) {
	log := &memCallLog{}
	first := &fakeChat{resp: validReply}
	j := NewJudge(first)
	j.SetCallLog(log)
	res, err := j.JudgeChunk(context.Background(), pinNameChunk())
	if err != nil {
		t.Fatal(err)
	}
	rows := log.all()
	if len(rows) != 1 || rows[0].Fn != JudgeChunkFn || rows[0].ErrorClass != "" || rows[0].RecipeID == "" {
		t.Fatalf("rows %+v: want one successful judge_chunk row with a recipe", rows)
	}
	var in judgeCallInput
	if err := json.Unmarshal(rows[0].Input, &in); err != nil {
		t.Fatal(err)
	}
	if in.ChunkText != pinNameChunk().Text || in.FilePath != pinNameChunk().FilePath ||
		in.PromptVersion != j.Prompt().Version() || in.BookContext.Source != BookContextPathOnly || in.Request["temperature"] == nil {
		t.Errorf("input %+v lacks the chunk, path, prompt version, book context or request params", in)
	}
	var out judgeCallOutput
	if err := json.Unmarshal(rows[0].Output, &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != validReply || len(out.Kept) != 1 {
		t.Errorf("output %+v: want the raw reply and the kept finding", out)
	}
	if res.FnCallID == nil || *res.FnCallID != rows[0].ID || *res.Findings[0].FnCallID != rows[0].ID {
		t.Errorf("result/finding not pointed at fn_call %d", rows[0].ID)
	}

	second := &fakeChat{resp: "must not be asked"}
	j2 := NewJudge(second)
	j2.SetCallLog(log)
	res2, err := j2.JudgeChunk(context.Background(), pinNameChunk())
	if err != nil {
		t.Fatal(err)
	}
	if second.calls != 0 || !res2.Cached || res2.Attempts != 0 {
		t.Fatalf("served chunk made %d calls (cached %v)", second.calls, res2.Cached)
	}
	if len(res2.Findings) != 1 || *res2.Findings[0].FnCallID != rows[0].ID {
		t.Errorf("cached findings %+v: want one, pointing at the original row", res2.Findings)
	}
	rows = log.all()
	if len(rows) != 2 || !rows[1].CacheHit || rows[1].CachedFrom == nil || *rows[1].CachedFrom != rows[0].ID {
		t.Errorf("want a cache-hit row pointing at row %d, got %+v", rows[0].ID, rows)
	}

	// A different request param is a different input: not served.
	third := &fakeChat{resp: validReply}
	j3 := NewJudge(&tempChat{fakeChat: third, temp: 0.7})
	j3.SetCallLog(log)
	if _, err := j3.JudgeChunk(context.Background(), pinNameChunk()); err != nil {
		t.Fatal(err)
	}
	if third.calls != 1 {
		t.Errorf("a changed temperature was served from cache (%d calls)", third.calls)
	}

	// Invalid replies are logged, never served.
	bad := &fakeChat{resp: "not json"}
	jb := NewJudge(bad)
	jb.SetCallLog(log)
	c := pinNameChunk()
	c.Text = "a different chunk entirely"
	for range 2 {
		if _, err := jb.JudgeChunk(context.Background(), c); !errors.Is(err, ErrInvalidReply) {
			t.Fatalf("want ErrInvalidReply, got %v", err)
		}
	}
	if bad.calls != 2 {
		t.Errorf("an invalid reply was served from cache (%d calls)", bad.calls)
	}
	var invalid int
	for _, r := range log.all() {
		if r.ErrorClass == ErrorClassInvalidReply {
			invalid++
			if len(r.Output) == 0 {
				t.Error("invalid_reply row lost the reply")
			}
		}
	}
	if invalid != 2 {
		t.Errorf("%d invalid_reply rows, want 2", invalid)
	}
}

// tempChat reports a temperature (TemperatureReporter).
type tempChat struct {
	*fakeChat
	temp float64
}

func (c *tempChat) Temperature() *float64 { return &c.temp }

// gaugeChat tracks concurrent calls with an atomic max.
type gaugeChat struct {
	inflight, peak, calls atomic.Int32
	delay                 time.Duration
}

func (g *gaugeChat) Complete(ctx context.Context, _, _ string) (string, error) {
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
	return `{"findings":[]}`, nil
}
func (g *gaugeChat) Model() string { return "gauge" }

// TestJudgeChunks_LimiterBoundsConcurrency: concurrent judging never has more
// calls in flight than the limiter admits, reaches the bound, and returns the
// stats in full.
func TestJudgeChunks_LimiterBoundsConcurrency(t *testing.T) {
	chat := &gaugeChat{delay: 15 * time.Millisecond}
	j := NewJudge(chat)
	var chunks []db.EvalChunk
	for i := range 12 {
		c := sampleChunk()
		c.ChunkID, c.ChunkIndex = fmt.Sprintf("c%d", i), i
		chunks = append(chunks, c)
	}
	var observed atomic.Int32
	_, stats, err := JudgeChunks(context.Background(), j, chunks, NewLimiter(3),
		func(Result, error) { observed.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	if p := chat.peak.Load(); p > 3 || p < 3 {
		t.Errorf("peak in-flight %d, want exactly the bound 3", p)
	}
	if stats.ChunksEvaluated != 12 || observed.Load() != 12 || len(stats.Latencies) != 12 {
		t.Errorf("stats %+v, observed %d", stats, observed.Load())
	}
}

// TestPassClose: status from the chunk counters; failed counts every chunk
// not judged; percentiles nearest-rank.
func TestPassClose(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	stats := RunStats{ChunksEvaluated: 4, ChunksSkipped: 1, ChunksCached: 1, FirstError: "boom",
		Latencies: []time.Duration{ms(40), ms(10), ms(30)}, HasCost: true, CostUSD: 0.5,
		Dropped: map[string]int{DropOverCap: 2}}
	cl := PassClose(stats, 5, nil)
	if cl.Status != db.JudgePassPartial || cl.ChunksFailed != 1 || cl.ChunksCached != 1 || cl.Error == "" {
		t.Errorf("partial close %+v", cl)
	}
	if *cl.LatencyP50MS != 30 || *cl.LatencyP95MS != 40 || *cl.CostUSD != 0.5 || cl.Dropped[DropOverCap] != 2 {
		t.Errorf("p50 %d p95 %d cost %v", *cl.LatencyP50MS, *cl.LatencyP95MS, *cl.CostUSD)
	}
	if cl := PassClose(RunStats{ChunksEvaluated: 5}, 5, nil); cl.Status != db.JudgePassComplete || cl.LatencyP50MS != nil {
		t.Errorf("complete close %+v", cl)
	}
	if cl := PassClose(RunStats{ChunksEvaluated: 5}, 5, context.Canceled); cl.Status != db.JudgePassPartial {
		t.Errorf("a stopped run is never complete: %+v", cl)
	}
	if cl := PassClose(RunStats{ChunksSkipped: 5}, 5, nil); cl.Status != db.JudgePassFailed || cl.ChunksFailed != 5 {
		t.Errorf("failed close %+v", cl)
	}
	if cl := PassClose(RunStats{ChunksEvaluated: 2}, 5, context.Canceled); cl.ChunksFailed != 3 {
		t.Errorf("unreached chunks must count as failed: %+v", cl)
	}
}
