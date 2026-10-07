package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// refreshCache is a stale-while-revalidate cache for one expensive value (a DB
// aggregate, an upstream probe). It never makes a request wait on a refresh
// once a value exists:
//
//   - A fresh value is returned immediately.
//   - A value older than ttl is STILL returned immediately, and one background
//     refresh is started (single-flight: concurrent callers never start a
//     second one).
//   - Only the very first load (no value yet) waits, and that wait honors the
//     caller's ctx — a cancelled request returns ctx.Err() without abandoning
//     the shared load for other callers.
//
// A failed refresh keeps the last good value, records the error (returned
// alongside the value until a refresh succeeds), logs once, and is not retried
// for retryAfter, so a broken dependency is not hammered on every poll.
//
// The load runs on its own context (timeout-bounded, detached from any request)
// so one viewer closing a tab can't turn into a recorded failure for everyone,
// and so every background goroutine has a bounded lifetime.
type refreshCache[T any] struct {
	load       func(ctx context.Context) (T, error)
	ttl        time.Duration
	timeout    time.Duration
	retryAfter time.Duration
	// firstRetryAfter replaces retryAfter while no value has ever loaded, so a
	// long-TTL cache (stale counts, 5 min) recovers quickly from a failed
	// first load instead of showing "unavailable" for a full TTL.
	firstRetryAfter time.Duration
	now             func() time.Time
	onError         func(err error, haveLastGood bool) // called once per failed refresh; may be nil

	mu         sync.Mutex
	last       *cached[T] // last good value; nil until the first success
	lastErr    error      // error of the most recent refresh (nil after a success)
	retryAt    time.Time  // after a failure, no new refresh before this
	refreshing bool
	done       chan struct{} // closed when the in-flight refresh finishes
}

// cached is one immutable cache entry: the value and when it was loaded.
type cached[T any] struct {
	Val T
	At  time.Time
}

func newRefreshCache[T any](ttl, timeout time.Duration, load func(context.Context) (T, error)) *refreshCache[T] {
	return &refreshCache[T]{load: load, ttl: ttl, timeout: timeout, retryAfter: ttl, firstRetryAfter: ttl, now: time.Now}
}

// get returns the cached entry (nil when none has ever loaded) and the error of
// the most recent refresh, per the policy above.
func (c *refreshCache[T]) get(ctx context.Context) (*cached[T], error) {
	c.mu.Lock()
	now := c.now()
	canRefresh := !c.refreshing && (c.lastErr == nil || !now.Before(c.retryAt))
	if c.last != nil {
		if canRefresh && now.Sub(c.last.At) >= c.ttl {
			c.startLocked()
		}
		last, err := c.last, c.lastErr
		c.mu.Unlock()
		return last, err
	}
	// No value yet: start (or join) the first load and wait for it, bounded by ctx.
	if !c.refreshing {
		if !canRefresh {
			err := c.lastErr
			c.mu.Unlock()
			return nil, err
		}
		c.startLocked()
	}
	done := c.done
	c.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, c.lastErr
}

// startLocked launches one background refresh. c.mu must be held.
func (c *refreshCache[T]) startLocked() {
	c.refreshing = true
	c.done = make(chan struct{})
	go c.refresh(c.done)
}

func (c *refreshCache[T]) refresh(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	v, err := c.safeLoad(ctx)

	c.mu.Lock()
	now := c.now()
	if err != nil {
		c.lastErr = err
		wait := c.retryAfter
		if c.last == nil {
			wait = c.firstRetryAfter
		}
		c.retryAt = now.Add(wait)
	} else {
		c.last = &cached[T]{Val: v, At: now}
		c.lastErr = nil
	}
	haveLast := c.last != nil
	onError := c.onError
	c.refreshing = false
	close(done)
	c.mu.Unlock()

	if err != nil && onError != nil {
		onError(err, haveLast)
	}
}

// safeLoad runs the load, converting a panic into an error. The load runs on
// a background goroutine, outside net/http's per-request recover, so an
// unrecovered panic here would take the whole process down.
func (c *refreshCache[T]) safeLoad(ctx context.Context) (v T, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("refresh panicked: %v", p)
		}
	}()
	return c.load(ctx)
}
