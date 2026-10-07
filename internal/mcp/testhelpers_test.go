package mcp

import "time"

// Test helpers shared across the Models page test files (models_test.go,
// refresh_cache_test.go, gateway_test.go).

// testNow is the fixed clock the Models page builders are tested against.
var testNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// boolp returns a pointer to b (for *bool expectations).
func boolp(b bool) *bool { return &b }

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
