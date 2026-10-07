package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// The Models page reads two independently cached DB snapshots, both served
// stale-while-revalidate by refreshCache (so a poll never waits on a refresh
// once a value exists):
//
//   - modelsSnapshot — role activity, current recipes, judge findings, ASR
//     provenance. Cheap (a few ms each); refreshed every 30 s, 3 s timeout.
//   - staleSnapshot — per-step stale_work counts. The view seq-scans chunks
//     and findings (EXPLAIN ANALYZE ≈ 1.6 s on production), so it gets its own
//     5 min TTL and 30 s timeout — the same budget as the ingest pod's
//     earmark_stale_items refresh. A slow or failed stale count never marks the
//     other sections unavailable.
const (
	modelsSnapshotTTL     = 30 * time.Second
	modelsSnapshotTimeout = 3 * time.Second
	staleSnapshotTTL      = 5 * time.Minute
	staleSnapshotTimeout  = 30 * time.Second
)

// asrProvenanceGroupLimit is how many runner-build groups the page shows.
const asrProvenanceGroupLimit = 8

// modelsSnapshot is the cheap DB evidence behind the Models page and the
// roles[] array of GET /api/v1/status.
type modelsSnapshot struct {
	Activity  db.ModelActivity
	Recipes   []db.CurrentRecipe
	Findings  []db.FindingsModelCount
	ASRGroups []db.ASRProvenanceGroup
}

// staleSnapshot is the per-step stale_work count map; a missing step key means
// the step has no current recipe (not tracked).
type staleSnapshot struct {
	Counts map[string]int64
}

// loadModelsSnapshot runs the four cheap aggregate queries sequentially.
func loadModelsSnapshot(ctx context.Context, d DBInterface) (modelsSnapshot, error) {
	var (
		s   modelsSnapshot
		err error
	)
	if s.Activity, err = d.GetModelActivity(ctx); err != nil {
		return modelsSnapshot{}, fmt.Errorf("models snapshot: %w", err)
	}
	if s.Recipes, err = d.ListCurrentRecipes(ctx); err != nil {
		return modelsSnapshot{}, fmt.Errorf("models snapshot: %w", err)
	}
	if s.Findings, err = d.FindingsByModel(ctx); err != nil {
		return modelsSnapshot{}, fmt.Errorf("models snapshot: %w", err)
	}
	if s.ASRGroups, err = d.ASRProvenanceGroups(ctx, asrProvenanceGroupLimit); err != nil {
		return modelsSnapshot{}, fmt.Errorf("models snapshot: %w", err)
	}
	return s, nil
}

func loadStaleSnapshot(ctx context.Context, d DBInterface) (staleSnapshot, error) {
	m, err := d.StaleItemCounts(ctx)
	if err != nil {
		return staleSnapshot{}, fmt.Errorf("stale counts: %w", err)
	}
	return staleSnapshot{Counts: m}, nil
}

// modelsCaches holds the page's two snapshot caches.
type modelsCaches struct {
	models *refreshCache[modelsSnapshot]
	stale  *refreshCache[staleSnapshot]
}

func newModelsCaches(d DBInterface, logWarn func(msg string, args ...any)) modelsCaches {
	c := modelsCaches{
		models: newRefreshCache(modelsSnapshotTTL, modelsSnapshotTimeout,
			func(ctx context.Context) (modelsSnapshot, error) { return loadModelsSnapshot(ctx, d) }),
		stale: newRefreshCache(staleSnapshotTTL, staleSnapshotTimeout,
			func(ctx context.Context) (staleSnapshot, error) { return loadStaleSnapshot(ctx, d) }),
	}
	// A failed FIRST stale count retries after the aggregates' TTL rather than
	// leaving the counts "unavailable" for the full 5 min.
	c.stale.firstRetryAfter = modelsSnapshotTTL
	if logWarn != nil {
		c.models.onError = func(err error, haveLast bool) {
			logWarn("models: snapshot refresh failed; serving last good counts", "error", err, "have_last_good", haveLast)
		}
		c.stale.onError = func(err error, haveLast bool) {
			logWarn("models: stale-count refresh failed; serving last good counts", "error", err, "have_last_good", haveLast)
		}
	}
	return c
}

// modelsEvidence is one read of both snapshots, as the builders consume it.
// Snap/Stale are nil when that snapshot has never loaded; the *Err fields
// carry the most recent refresh error (the last good value is still served).
// StalePending is true while the stale counts' first load is still running.
type modelsEvidence struct {
	Snap         *modelsSnapshot
	SnapAt       time.Time
	SnapErr      error
	Stale        *staleSnapshot
	StaleAt      time.Time
	StaleErr     error
	StalePending bool
}

// read returns both snapshots. After their first loads both are served from
// cache without waiting. The aggregates' first load waits up to its own 3 s
// timeout (bounded by ctx); the stale counts' first load (a full-library scan)
// is waited for at most staleWait, then reported as pending while it finishes
// in the background.
func (c modelsCaches) read(ctx context.Context, staleWait time.Duration) modelsEvidence {
	var e modelsEvidence
	m, err := c.models.get(ctx)
	e.SnapErr = err
	if m != nil {
		e.Snap, e.SnapAt = &m.Val, m.At
	}
	sctx, cancel := context.WithTimeout(ctx, staleWait)
	s, err := c.stale.get(sctx)
	if s == nil && errors.Is(err, context.DeadlineExceeded) && sctx.Err() != nil && ctx.Err() == nil {
		e.StalePending, err = true, nil // still counting in the background; not a failure
	}
	cancel()
	e.StaleErr = err
	if s != nil {
		e.Stale, e.StaleAt = &s.Val, s.At
	}
	return e
}
