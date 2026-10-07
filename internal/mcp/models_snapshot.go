package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// modelsSnapshotTTL is how long the Models page reuses its DB aggregates. The
// fragment polls every 5 s; the aggregates (stale_work counts, findings
// GROUP BY, provenance groups) recompute at most every 30 s regardless of how
// many viewers or API callers there are.
const modelsSnapshotTTL = 30 * time.Second

// modelsSnapshotTimeout bounds one refresh (five queries, sequential).
const modelsSnapshotTimeout = 3 * time.Second

// asrProvenanceGroupLimit is how many runner-build groups the page shows.
const asrProvenanceGroupLimit = 8

// modelsSnapshot is the cached DB evidence behind the Models page and the
// roles[] array of GET /api/v1/status.
type modelsSnapshot struct {
	At        time.Time
	Activity  db.ModelActivity
	Recipes   []db.CurrentRecipe
	Stale     map[string]int64 // step → count; a missing key = not tracked
	Findings  []db.FindingsModelCount
	ASRGroups []db.ASRProvenanceGroup
}

// modelsSnapshotCache holds the last GOOD snapshot. Holding mu across the
// refresh is deliberate: it is the single-flight (concurrent pollers wait for
// one refresh rather than each issuing it), and a refresh takes tens of ms.
type modelsSnapshotCache struct {
	mu      sync.Mutex
	last    *modelsSnapshot // last good snapshot; kept when a refresh fails
	lastErr error           // error of the most recent refresh (nil after a success)
	retryAt time.Time       // after a failure, no new attempt before this
	ttl     time.Duration
	now     func() time.Time
}

func newModelsSnapshotCache(ttl time.Duration) *modelsSnapshotCache {
	return &modelsSnapshotCache{ttl: ttl, now: time.Now}
}

// get returns the cached snapshot, refreshing it when older than the TTL. On a
// refresh error it returns the last good snapshot (nil if there never was one)
// together with the error, and waits a full TTL before retrying so a broken DB
// is not hammered every 5 s per viewer.
func (c *modelsSnapshotCache) get(ctx context.Context, d DBInterface) (*modelsSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.lastErr != nil {
		if now.Before(c.retryAt) {
			return c.last, c.lastErr
		}
	} else if c.last != nil && now.Sub(c.last.At) < c.ttl {
		return c.last, nil
	}

	// Detach from the request's cancellation: a viewer closing the tab must not
	// turn into a recorded failure that blocks every other viewer for a TTL.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelsSnapshotTimeout)
	defer cancel()
	snap, err := loadModelsSnapshot(rctx, d)
	if err != nil {
		c.lastErr = err
		c.retryAt = now.Add(c.ttl)
		return c.last, err
	}
	snap.At = now
	c.last, c.lastErr = snap, nil
	return snap, nil
}

// loadModelsSnapshot runs the five aggregate queries sequentially.
func loadModelsSnapshot(ctx context.Context, d DBInterface) (*modelsSnapshot, error) {
	var (
		s   modelsSnapshot
		err error
	)
	if s.Activity, err = d.GetModelActivity(ctx); err != nil {
		return nil, fmt.Errorf("models snapshot: %w", err)
	}
	if s.Recipes, err = d.ListCurrentRecipes(ctx); err != nil {
		return nil, fmt.Errorf("models snapshot: %w", err)
	}
	if s.Stale, err = d.StaleItemCounts(ctx); err != nil {
		return nil, fmt.Errorf("models snapshot: %w", err)
	}
	if s.Findings, err = d.FindingsByModel(ctx); err != nil {
		return nil, fmt.Errorf("models snapshot: %w", err)
	}
	if s.ASRGroups, err = d.ASRProvenanceGroups(ctx, asrProvenanceGroupLimit); err != nil {
		return nil, fmt.Errorf("models snapshot: %w", err)
	}
	return &s, nil
}
