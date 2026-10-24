package decide

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/systemone"
)

// routeAsker answers every call with resp/err and appends its name to *log.
type routeAsker struct {
	name string
	resp *systemone.Response
	err  error
	mu   *sync.Mutex
	log  *[]string
}

func (a routeAsker) Decide(context.Context, systemone.Request) (*systemone.Response, error) {
	a.mu.Lock()
	*a.log = append(*a.log, a.name)
	a.mu.Unlock()
	return a.resp, a.err
}

// Until a call has returned, calls go to the cold asker; after it, to the warm
// one — whatever the first call returned.
func TestColdStartRouting(t *testing.T) {
	answer := &systemone.Response{Model: "clef:27b"}
	tests := []struct {
		name    string
		coldErr error
		want    []string
	}{
		{"first call answered", nil, []string{"cold", "warm", "warm"}},
		{"first call timed out", &systemone.TimeoutError{Err: context.DeadlineExceeded}, []string{"cold", "warm", "warm"}},
		{"first call refused", &systemone.ServerError{Code: 503}, []string{"cold", "warm", "warm"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var log []string
			a := &coldStartAsker{
				cold: routeAsker{"cold", answer, tc.coldErr, &mu, &log},
				warm: routeAsker{"warm", answer, nil, &mu, &log},
			}
			for range 3 {
				resp, err := a.Decide(context.Background(), systemone.Request{Model: "clef:27b"})
				if len(log) == 1 { // the first call: its reply and error come back as they were
					if !errors.Is(err, tc.coldErr) || (tc.coldErr == nil && resp != answer) {
						t.Errorf("first call returned %v, %v", resp, err)
					}
				}
			}
			if !slices.Equal(log, tc.want) {
				t.Errorf("calls went to %v, want %v", log, tc.want)
			}
		})
	}
}

// The first calls of a run are made together, before any has returned: all of
// them — not just the first — get the cold timeout. The next one, after the
// first has returned, does not.
func TestColdStartBurst(t *testing.T) {
	for _, n := range []int{1, 4, 8} {
		t.Run(fmt.Sprintf("%d in flight", n), func(t *testing.T) {
			var mu sync.Mutex
			var log []string
			arrived := make(chan struct{}, n)
			release := make(chan struct{})
			a := &coldStartAsker{
				cold: askerFunc(func(context.Context, systemone.Request) (*systemone.Response, error) {
					mu.Lock()
					log = append(log, "cold")
					mu.Unlock()
					arrived <- struct{}{}
					<-release
					return &systemone.Response{}, nil
				}),
				warm: routeAsker{"warm", &systemone.Response{}, nil, &mu, &log},
			}
			var wg sync.WaitGroup
			for range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := a.Decide(context.Background(), systemone.Request{}); err != nil {
						t.Error(err)
					}
				}()
			}
			for range n {
				select {
				case <-arrived:
				case <-time.After(5 * time.Second):
					t.Fatalf("only some of the %d calls reached the cold asker", n)
				}
			}
			close(release)
			wg.Wait()
			if _, err := a.Decide(context.Background(), systemone.Request{}); err != nil {
				t.Fatal(err)
			}
			want := make([]string, 0, n+1)
			for range n {
				want = append(want, "cold")
			}
			want = append(want, "warm")
			if !slices.Equal(log, want) {
				t.Errorf("calls went to %v, want %v", log, want)
			}
		})
	}
}

type askerFunc func(context.Context, systemone.Request) (*systemone.Response, error)

func (f askerFunc) Decide(ctx context.Context, req systemone.Request) (*systemone.Response, error) {
	return f(ctx, req)
}

// coldModelServer is a System One endpoint in front of a model that has to be
// loaded: every request waits for the load, which starts with the first one and
// takes loadTime; after that each is answered at once, unless hang is set.
type coldModelServer struct {
	loadTime time.Duration
	hang     atomic.Bool
	calls    atomic.Int32

	once     sync.Once
	loadedAt time.Time
}

func (s *coldModelServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body) // so a client that gives up cancels r.Context()
		s.once.Do(func() { s.loadedAt = time.Now().Add(s.loadTime) })
		if d := time.Until(s.loadedAt); d > 0 {
			time.Sleep(d)
		}
		if s.hang.Load() {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		noul(0.97, "clef:27b")(w, systemone.Request{})
	}))
	t.Cleanup(srv.Close)
	return srv
}

var askReq = systemone.Request{
	Model: "clef:27b", State: "s",
	Questions: map[string]systemone.Question{questionKey: shouldApplyQuestion},
}

// The failure this exists for, end to end over HTTP: a model whose load takes
// longer than the call timeout times out every call of a cold start when
// asked directly, and none of them behind the cold-start asker — whose
// ordinary timeout then applies again once the first call has returned.
func TestColdStartOverHTTP(t *testing.T) {
	const (
		callTimeout = 50 * time.Millisecond  // stands in for systemone.DefaultTimeout
		loadTime    = 400 * time.Millisecond // longer than callTimeout, as 15.4 s is longer than 15 s
		coldTimeout = 10 * time.Second
		inFlight    = 4
	)
	tests := []struct {
		name string
		wrap bool
		// wantTimeouts is how many of the burst's calls time out.
		wantTimeouts int
		// wantRequests is every request the endpoint sees: the burst, plus —
		// wrapped — the two follow-up calls below. Never one more: the
		// cold-start asker makes no warm-up request of its own.
		wantRequests int
	}{
		{"asked directly, a cold start times out every call", false, inFlight, inFlight},
		{"behind the cold-start asker, none does", true, 0, inFlight + 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := &coldModelServer{loadTime: loadTime}
			srv := model.start(t)
			client, err := systemone.New(srv.URL, "", systemone.WithTimeout(callTimeout))
			if err != nil {
				t.Fatal(err)
			}
			var asker Asker = client
			if tc.wrap {
				asker = withColdStart(client, coldTimeout)
			}

			var timeouts atomic.Int32
			var wg sync.WaitGroup
			for range inFlight {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := asker.Decide(context.Background(), askReq)
					var te *systemone.TimeoutError
					if errors.As(err, &te) {
						timeouts.Add(1)
					} else if err != nil {
						t.Errorf("call failed with %v, want an answer or a timeout", err)
					}
				}()
			}
			wg.Wait()
			if got := int(timeouts.Load()); got != tc.wantTimeouts {
				t.Errorf("%d of %d cold-start calls timed out, want %d", got, inFlight, tc.wantTimeouts)
			}

			// Warm now: answered at once under the ordinary timeout...
			if tc.wrap {
				if _, err := asker.Decide(context.Background(), askReq); err != nil {
					t.Errorf("a warm call failed: %v", err)
				}
				// ...and a model that then stops answering is held to that
				// timeout, not to the cold one.
				model.hang.Store(true)
				start := time.Now()
				_, err := asker.Decide(context.Background(), askReq)
				var te *systemone.TimeoutError
				if !errors.As(err, &te) || time.Since(start) > coldTimeout/2 {
					t.Errorf("a hung warm call returned %v after %v, want a timeout at the ordinary limit", err, time.Since(start))
				}
			}
			if got := int(model.calls.Load()); got != tc.wantRequests {
				t.Errorf("the endpoint saw %d requests, want %d", got, tc.wantRequests)
			}
		})
	}
}

// What the cold-start asker adds to a run is nothing but time: the same
// requests reach the endpoint and the same rows reach fn_calls, with the same
// cache keys, and every decision, reason, p and recipe is the same. No
// warm-up request exists to be recorded, cached or counted.
func TestColdStartChangesNothingButTheTimeout(t *testing.T) {
	type observed struct {
		calls    int
		requests []string // sorted: the order of concurrent calls is not fixed
		rows     []db.FnCacheKey
		report   *Report
		recipes  int
	}
	run := func(wrap bool) observed {
		t.Helper()
		store := runFixture()
		inner := runAsker()
		var requests []string
		var mu sync.Mutex
		var asker Asker = askerFunc(func(ctx context.Context, req systemone.Request) (*systemone.Response, error) {
			mu.Lock()
			requests = append(requests, req.Model+"\x00"+req.State)
			mu.Unlock()
			return inner.Decide(ctx, req)
		})
		if wrap {
			asker = &coldStartAsker{cold: asker, warm: asker}
		}
		rep, err := DryRun(context.Background(), store, asker, RunOptions{
			Scope: db.DecideScope{Sample: 50, Seed: "s"}, Concurrency: 3, Params: DefaultShouldApplyParams(),
		})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(requests)
		var keys []db.FnCacheKey
		for _, r := range store.rows {
			keys = append(keys, r.Key())
		}
		slices.SortFunc(keys, func(a, b db.FnCacheKey) int { return cmp.Compare(a.InputSHA256, b.InputSHA256) })
		return observed{calls: inner.calls, requests: requests, rows: keys, report: rep, recipes: len(store.recipes)}
	}

	plain, wrapped := run(false), run(true)
	if plain.calls != 7 || wrapped.calls != plain.calls {
		t.Errorf("model calls: plain %d, wrapped %d, want 7 each (no warm-up call)", plain.calls, wrapped.calls)
	}
	if !slices.Equal(plain.requests, wrapped.requests) {
		t.Errorf("the requests differ: plain asked %d, wrapped %d (sorted, compared byte for byte)", len(plain.requests), len(wrapped.requests))
	}
	if len(wrapped.rows) != 7 || !reflect.DeepEqual(plain.rows, wrapped.rows) {
		t.Errorf("fn_calls rows (cache keys) differ: plain %d, wrapped %d, want the same 7", len(plain.rows), len(wrapped.rows))
	}
	if plain.recipes != wrapped.recipes {
		t.Errorf("registered %d recipes, want %d", wrapped.recipes, plain.recipes)
	}
	p, w := plain.report, wrapped.report
	if p.RecipeID != w.RecipeID || !reflect.DeepEqual(p.Outcomes, w.Outcomes) || !reflect.DeepEqual(p.Reasons, w.Reasons) ||
		!reflect.DeepEqual(p.Policy, w.Policy) || p.PolicySource != w.PolicySource ||
		p.Jev.Asked != w.Jev.Asked || p.Jev.Answered != w.Jev.Answered || p.Jev.Unavailable != w.Jev.Unavailable ||
		p.ConflictAsks != w.ConflictAsks || !reflect.DeepEqual(p.Grouping, w.Grouping) {
		t.Errorf("the report differs:\nplain   %+v\nwrapped %+v", p, w)
	}
}

func TestAskerFromConfig(t *testing.T) {
	ep := func(model string) *config.Config {
		return &config.Config{
			AIEndpoints: []config.AIEndpoint{{ID: "s1", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://gw:4000", Model: model}},
			AIRoles:     &config.AIRoles{Decide: "s1"},
		}
	}
	if a, ok, err := AskerFromConfig(&config.Config{}); a != nil || ok || err != nil {
		t.Errorf("unbound role: %v ok=%v err=%v", a, ok, err)
	}
	if a, ok, err := AskerFromConfig(ep("jev-latest")); a != nil || !ok || err == nil {
		t.Errorf("moving alias: %v ok=%v err=%v", a, ok, err)
	}
	a, ok, err := AskerFromConfig(ep("clef:27b"))
	if !ok || err != nil {
		t.Fatalf("clef: ok=%v err=%v", ok, err)
	}
	if _, wrapped := a.(*coldStartAsker); !wrapped {
		t.Errorf("a decide run's asker is %T, not cold-start safe", a)
	}
}

// The cold timeout is longer than a model load: measured 15.4 s for clef:27b
// (a 22 GB load) against the 15 s ordinary timeout.
func TestColdStartTimeoutOutlastsAModelLoad(t *testing.T) {
	const measuredLoad = 15400 * time.Millisecond
	if ColdStartTimeout < 2*measuredLoad {
		t.Errorf("ColdStartTimeout %s is less than twice the measured %s model load", ColdStartTimeout, measuredLoad)
	}
	if ColdStartTimeout <= systemone.DefaultTimeout {
		t.Errorf("ColdStartTimeout %s does not exceed the ordinary %s", ColdStartTimeout, systemone.DefaultTimeout)
	}
}
