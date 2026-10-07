package mcp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLoader counts loads, can fail, and can block until released.
type fakeLoader struct {
	calls   atomic.Int32
	fail    atomic.Bool
	release chan struct{} // nil = return immediately
}

func (f *fakeLoader) load(ctx context.Context) (int, error) {
	n := f.calls.Add(1)
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if f.fail.Load() {
		return 0, errors.New("db down")
	}
	return int(n), nil
}

func newTestCache(f *fakeLoader, clock *time.Time) *refreshCache[int] {
	c := newRefreshCache(30*time.Second, time.Second, f.load)
	c.now = func() time.Time { return *clock }
	return c
}

func TestRefreshCache_ServeStaleRefreshInBackground(t *testing.T) {
	clock := testNow
	f := &fakeLoader{}
	c := newTestCache(f, &clock)
	ctx := context.Background()

	e1, err := c.get(ctx) // first load waits
	require.NoError(t, err)
	require.NotNil(t, e1)
	assert.Equal(t, 1, e1.Val)
	assert.Equal(t, testNow, e1.At)

	clock = clock.Add(10 * time.Second) // fresh → no load
	e2, _ := c.get(ctx)
	assert.Same(t, e1, e2)
	assert.EqualValues(t, 1, f.calls.Load())

	clock = clock.Add(25 * time.Second) // stale → old value NOW, refresh behind
	e3, err := c.get(ctx)
	require.NoError(t, err)
	assert.Same(t, e1, e3, "a stale value is served immediately")
	c.waitIdle()
	assert.EqualValues(t, 2, f.calls.Load())
	e4, _ := c.get(ctx)
	assert.Equal(t, 2, e4.Val, "the background refresh landed")
}

func TestRefreshCache_KeepLastGoodAndRetryAfter(t *testing.T) {
	clock := testNow
	f := &fakeLoader{}
	c := newTestCache(f, &clock)
	var logged atomic.Int32
	c.onError = func(error, bool) { logged.Add(1) }
	ctx := context.Background()
	good, _ := c.get(ctx)

	f.fail.Store(true)
	clock = clock.Add(31 * time.Second)
	_, _ = c.get(ctx) // triggers the failing refresh
	c.waitIdle()
	e, err := c.get(ctx)
	require.Error(t, err)
	assert.Same(t, good, e, "keep the last good value on error")
	// onError runs just after done closes, so wait for it rather than racing.
	assert.Eventually(t, func() bool { return logged.Load() == 1 }, 2*time.Second, time.Millisecond,
		"log once per failed refresh, not per poll")

	clock = clock.Add(5 * time.Second) // within retry window: no new load
	_, _ = c.get(ctx)
	c.waitIdle()
	assert.EqualValues(t, 2, f.calls.Load(), "a broken dependency is not re-queried on every poll")
	assert.EqualValues(t, 1, logged.Load())

	f.fail.Store(false)
	clock = clock.Add(30 * time.Second) // retry window over
	_, _ = c.get(ctx)
	c.waitIdle()
	e, err = c.get(ctx)
	require.NoError(t, err)
	assert.NotSame(t, good, e)
	assert.EqualValues(t, 3, f.calls.Load())
}

func TestRefreshCache_NeverGood(t *testing.T) {
	clock := testNow
	f := &fakeLoader{}
	f.fail.Store(true)
	c := newTestCache(f, &clock)
	e, err := c.get(context.Background())
	require.Error(t, err)
	assert.Nil(t, e)
	// Retry window applies to the first load too.
	_, err = c.get(context.Background())
	require.Error(t, err)
	assert.EqualValues(t, 1, f.calls.Load())
}

// The first load honors the caller's ctx, but a cancelled caller does not
// cancel the shared load for others.
func TestRefreshCache_FirstLoadRespectsCtx(t *testing.T) {
	clock := testNow
	f := &fakeLoader{release: make(chan struct{})}
	c := newTestCache(f, &clock)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	e, err := c.get(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, e)

	close(f.release)
	c.waitIdle()
	e, err = c.get(context.Background())
	require.NoError(t, err)
	require.NotNil(t, e, "the load continued after the first caller gave up")
	assert.EqualValues(t, 1, f.calls.Load())
}

// TestRefreshCache_SingleFlight: N concurrent callers past the TTL cause one
// load, and none of them waits for it.
func TestRefreshCache_SingleFlight(t *testing.T) {
	var mu sync.Mutex
	clock := testNow
	f := &fakeLoader{}
	c := newRefreshCache(30*time.Second, time.Second, f.load)
	c.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	first, err := c.get(context.Background())
	require.NoError(t, err)

	f.release = make(chan struct{}) // the next load blocks until released
	mu.Lock()
	clock = clock.Add(time.Minute)
	mu.Unlock()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			e, _ := c.get(context.Background())
			assert.Same(t, first, e, "callers get the stale value without waiting")
		}()
	}
	wg.Wait() // returns while the refresh is still blocked: nobody waited
	assert.EqualValues(t, 2, f.calls.Load(), "exactly one refresh for %d concurrent callers", n)
	close(f.release)
	c.waitIdle()

	// Concurrent FIRST loads also share one load.
	g := &fakeLoader{release: make(chan struct{})}
	c2 := newRefreshCache(30*time.Second, time.Second, g.load)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			e, err := c2.get(context.Background())
			assert.NoError(t, err)
			assert.NotNil(t, e)
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(g.release)
	wg.Wait()
	assert.EqualValues(t, 1, g.calls.Load())
}

// waitIdle blocks until no refresh is in flight (background refreshes make
// "after this get, the value is updated" asynchronous).
func (c *refreshCache[T]) waitIdle() {
	c.mu.Lock()
	refreshing, done := c.refreshing, c.done
	c.mu.Unlock()
	if refreshing {
		<-done
	}
}

// TestRefreshCache_LoadPanicIsRecovered: a panicking load becomes lastErr,
// clears refreshing and closes done (no waiter hangs), and the process lives.
func TestRefreshCache_LoadPanicIsRecovered(t *testing.T) {
	clock := testNow
	calls := 0
	c := newRefreshCache(30*time.Second, time.Second, func(context.Context) (int, error) {
		calls++
		if calls == 2 {
			panic("boom")
		}
		return calls, nil
	})
	c.now = func() time.Time { return clock }
	logged := make(chan error, 1)
	c.onError = func(err error, _ bool) { logged <- err } // runs after done closes

	first, err := c.get(context.Background())
	require.NoError(t, err)

	clock = clock.Add(time.Minute)
	_, _ = c.get(context.Background()) // background refresh panics
	c.waitIdle()
	e, err := c.get(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh panicked: boom")
	assert.Contains(t, err.Error(), "goroutine ", "the stack trace is kept with the error")
	assert.Contains(t, err.Error(), "safeLoad")
	assert.Same(t, first, e, "last good value kept")
	select {
	case lerr := <-logged:
		assert.Contains(t, lerr.Error(), "boom")
	case <-time.After(2 * time.Second):
		t.Fatal("the panic was never reported to onError")
	}
	c.mu.Lock()
	assert.False(t, c.refreshing, "refreshing must be cleared after a panic")
	c.mu.Unlock()

	// A panicking FIRST load must not leave waiters hanging either.
	p := newRefreshCache(30*time.Second, time.Second, func(context.Context) (int, error) { panic("first") })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e2, err := p.get(ctx)
	assert.Nil(t, e2)
	require.Error(t, err)
	assert.NotErrorIs(t, err, context.DeadlineExceeded, "the waiter was released by done, not by its timeout")
}

// TestRefreshCache_FirstFailureRetriesSooner: with no value yet, a failed load
// is retried after firstRetryAfter, not a full TTL.
func TestRefreshCache_FirstFailureRetriesSooner(t *testing.T) {
	clock := testNow
	f := &fakeLoader{}
	f.fail.Store(true)
	c := newRefreshCache(5*time.Minute, time.Second, f.load)
	c.firstRetryAfter = 30 * time.Second
	c.now = func() time.Time { return clock }
	_, _ = c.get(context.Background())
	clock = clock.Add(31 * time.Second)
	f.fail.Store(false)
	e, err := c.get(context.Background())
	require.NoError(t, err)
	require.NotNil(t, e)
	assert.EqualValues(t, 2, f.calls.Load())
}
