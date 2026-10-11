package eval

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"syscall"
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

// TestJudgeChunk_CacheKeyCoversModelIdentity: a new pinned revision or a new
// expected model on the same alias is a cache MISS (never the old model's
// reply served as the new one's), and every call leaves exactly one row.
func TestJudgeChunk_CacheKeyCoversModelIdentity(t *testing.T) {
	log := &memCallLog{}
	judge := func(pin ModelPin) *fakeChat {
		t.Helper()
		chat := &fakeChat{resp: validReply}
		j := NewJudge(chat)
		j.SetModelPin(pin)
		j.SetCallLog(log)
		if _, err := j.JudgeChunk(context.Background(), pinNameChunk()); err != nil {
			t.Fatal(err)
		}
		return chat
	}
	steps := []struct {
		name      string
		pin       ModelPin
		wantCalls int
		wantRows  int
	}{
		{"first call", ModelPin{Revision: "r1"}, 1, 1},
		{"same identity is served", ModelPin{Revision: "r1"}, 0, 2}, // + the cache-hit row
		{"revision bump misses", ModelPin{Revision: "r2"}, 1, 3},
		{"expected model change misses", ModelPin{Revision: "r2", ExpectedModel: "fake-judge-2026"}, 1, 4},
	}
	for _, st := range steps {
		chat := judge(st.pin)
		if chat.calls != st.wantCalls || len(log.all()) != st.wantRows {
			t.Fatalf("%s: %d calls, %d rows; want %d, %d", st.name, chat.calls, len(log.all()), st.wantCalls, st.wantRows)
		}
	}
	var in judgeCallInput
	if err := json.Unmarshal(log.all()[3].Input, &in); err != nil {
		t.Fatal(err)
	}
	if in.ExpectedModel != "fake-judge-2026" || in.ModelRevision != "r2" || in.StepVersion != proposeStepVersion {
		t.Errorf("input identity %+v", in)
	}
}

// TestJudgeChunk_PaidCallNeverDropped: when another row already holds the
// cache slot but does not serve (here: answered by another model), the paid
// reply is still logged — as cache_slot_taken — and the findings point at it.
func TestJudgeChunk_PaidCallNeverDropped(t *testing.T) {
	log := &memCallLog{}
	j := NewJudge(&fakeChat{resp: validReply})
	j.SetCallLog(log)
	if _, err := j.JudgeChunk(context.Background(), pinNameChunk()); err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	log.rows[0].ModelResolved = "some-other-model" // holds the slot, never serves
	log.mu.Unlock()

	chat := &fakeChat{resp: validReply}
	j2 := NewJudge(chat)
	j2.SetCallLog(log)
	res, err := j2.JudgeChunk(context.Background(), pinNameChunk())
	if err != nil {
		t.Fatal(err)
	}
	rows := log.all()
	if chat.calls != 1 || len(rows) != 2 || rows[1].ErrorClass != ErrorClassCacheSlotTaken || len(rows[1].Output) == 0 {
		t.Fatalf("calls %d, rows %+v: want the paid reply logged as cache_slot_taken", chat.calls, rows)
	}
	if res.FnCallID == nil || *res.FnCallID != rows[1].ID || *res.Findings[0].FnCallID != rows[1].ID {
		t.Errorf("findings not pointed at the logged paid call %d", rows[1].ID)
	}
}

// TestTransient: which call failures are retried.
func TestTransient(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"attempt deadline":      {fmt.Errorf("chat request: %w", context.DeadlineExceeded), true},
		"net timeout":           {&url.Error{Op: "Post", URL: "u", Err: timeoutErr{}}, true},
		"connection reset":      {&url.Error{Op: "Post", URL: "u", Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}}, true},
		"connection refused":    {&url.Error{Op: "Post", URL: "u", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, true},
		"EOF on request":        {&url.Error{Op: "Post", URL: "u", Err: io.EOF}, true},
		"reset reading body":    {&BodyReadError{Err: io.ErrUnexpectedEOF}, false},
		"body reset, wrapped":   {fmt.Errorf("judge chunk c: %w", &BodyReadError{Err: syscall.ECONNRESET}), false},
		"x509 unknown CA":       {&url.Error{Op: "Post", URL: "u", Err: x509.UnknownAuthorityError{}}, false},
		"tls verify":            {&url.Error{Op: "Post", URL: "u", Err: &tls.CertificateVerificationError{Err: x509.HostnameError{}}}, false},
		"bad URL":               {&url.Error{Op: "parse", URL: "::", Err: errors.New("missing protocol scheme")}, false},
		"unmarshal of the body": {fmt.Errorf("unmarshal chat response: %w", errors.New("invalid character")), false},
		"caller cancelled":      {context.Canceled, false},
		"temporary DNS failure": {&url.Error{Op: "Post", URL: "u", Err: &net.OpError{Op: "dial",
			Err: &net.DNSError{Err: "server misbehaving", Name: "judge", IsTemporary: true}}}, true},
		"DNS lookup error":   {&url.Error{Op: "Post", URL: "u", Err: &net.DNSError{Err: "read udp: connection refused", Name: "judge"}}, true},
		"DNS no such host":   {&url.Error{Op: "Post", URL: "u", Err: &net.DNSError{Err: "no such host", Name: "judge", IsNotFound: true}}, false},
		"h2 stream reset":    {&url.Error{Op: "Post", URL: "u", Err: errors.New("stream error: stream ID 3; INTERNAL_ERROR; received from peer")}, true},
		"h2 GOAWAY":          {&url.Error{Op: "Post", URL: "u", Err: errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug=""`)}, true},
		"h2 connection lost": {&url.Error{Op: "Post", URL: "u", Err: errors.New("http2: client connection lost")}, true},
	}
	for name, tc := range cases {
		if got := transient(tc.err); got != tc.want {
			t.Errorf("%s: transient = %v, want %v", name, got, tc.want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// reportedChat reports a resolved model (ModelReportingClient).
type reportedChat struct{ resolved string }

func (c reportedChat) Complete(context.Context, string, string) (string, error) {
	return validReply, nil
}
func (c reportedChat) Model() string { return "fake-judge" }
func (c reportedChat) CompleteWithModel(context.Context, string, string) (Completion, error) {
	return Completion{Content: validReply, ResolvedModel: c.resolved}, nil
}

// TestFallbackIsCounted: a reply from another model than the recipe expects
// is flagged and counted (RunStats.ChunksFallback), and its fn_calls row is
// model_fallback — never served, so the next run pays again.
func TestFallbackIsCounted(t *testing.T) {
	log := &memCallLog{}
	j := NewJudge(reportedChat{resolved: "fake-judge-2026-10-01"})
	j.SetCallLog(log)
	_, stats, err := JudgeChunks(context.Background(), j, []db.EvalChunk{pinNameChunk()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ChunksFallback != 1 || log.all()[0].ErrorClass != db.ErrorClassModelFallback {
		t.Fatalf("fallback %d, row %+v", stats.ChunksFallback, log.all()[0])
	}
	_, stats, _ = JudgeChunks(context.Background(), j, []db.EvalChunk{pinNameChunk()}, nil, nil)
	if stats.ChunksCached != 0 || stats.ChunksFallback != 1 {
		t.Errorf("a fallback reply was served from cache: %+v", stats)
	}
	// A reply by the expected model (route prefix aside) is no fallback.
	_, stats, _ = JudgeChunks(context.Background(), NewJudge(reportedChat{resolved: "openrouter/fake-judge"}),
		[]db.EvalChunk{pinNameChunk()}, nil, nil)
	if stats.ChunksFallback != 0 {
		t.Errorf("same model behind a route prefix counted as fallback")
	}
}

// TestJudgeChunk_BodyReadFailureNotRetried: the endpoint answers 200 and the
// connection drops mid-body. That call may be billed with its cost lost, so
// it is not retried (a retry would pay again unseen); the error class says why.
func TestJudgeChunk_BodyReadFailureNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"m","choi`)) // then the handler returns: short body
	}))
	t.Cleanup(srv.Close)
	j, waits := judgeFor(srv, CallPolicy{Retries: 4, BackoffBase: time.Millisecond})
	_, err := j.JudgeChunk(context.Background(), pinNameChunk())
	var bre *BodyReadError
	if !errors.As(err, &bre) {
		t.Fatalf("want a BodyReadError, got %v", err)
	}
	if calls.Load() != 1 || len(*waits) != 0 {
		t.Errorf("%d calls, %d waits: a body-read failure must not be retried", calls.Load(), len(*waits))
	}
	if errorClass(err) != "body_read" {
		t.Errorf("error class %q, want body_read", errorClass(err))
	}
}
