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
//     provenance. Cheap (a few ms each; judge findings ≈ 0.6 s at 46k rows);
//     refreshed every 30 s, 3 s timeout.
//   - fnRoles — the decide/scan call evidence: one aggregate over the fn_calls
//     log. Its own cache and budget (60 s TTL, 30 s timeout): it scans the
//     whole call log (~0.8 s at 150k rows, growing with every decide run), so
//     sharing the snapshot's 3 s budget made it time out on every refresh.
//   - staleSnapshot — per-step stale_work counts. The view seq-scans chunks
//     and findings (EXPLAIN ANALYZE ≈ 1.6 s on production), so it gets its own
//     5 min TTL and 30 s timeout — the same budget as the ingest pod's
//     earmark_stale_items refresh. A slow or failed stale count never marks the
//     other sections unavailable.
const (
	modelsSnapshotTTL     = 30 * time.Second
	modelsSnapshotTimeout = 3 * time.Second
	fnRolesTTL            = 60 * time.Second
	fnRolesTimeout        = 30 * time.Second
	staleSnapshotTTL      = 5 * time.Minute
	staleSnapshotTimeout  = 30 * time.Second
)

// The Models page also needs the queue stats (coverage, embed backlog, chunk
// total, last embed, runner version). GetServiceStatus counts every row of
// transcript_chunks (COUNT(*) and MAX(created_at), a full scan of the
// largest table) and, run inline on every 5 s poll, pushed the fragment past
// its 5 s htmx timeout on a large library. The page reads them through a
// third stale-while-revalidate cache instead: refreshed every 15 s with a
// 30 s budget, the first load waited for at most queueStatsFirstWait (the
// cards render "—" meanwhile). The Pipeline page and GET /api/v1/status keep
// reading them live; a runner-update POST re-renders with live stats so the
// request it just made shows at once.
const (
	queueStatsTTL       = 15 * time.Second
	queueStatsTimeout   = 30 * time.Second
	queueStatsFirstWait = 2 * time.Second
)

// asrProvenanceGroupLimit is how many runner-build groups the page shows.
const asrProvenanceGroupLimit = 8

// modelsSnapshot is the cheap DB evidence behind the Models page and the
// roles[] array of GET /api/v1/status.
type modelsSnapshot struct {
	Activity  db.ModelActivity
	Recipes   []db.CurrentRecipe
	Findings  []db.FindingsModelCount
	FnRoles   []db.FnRoleActivity
	ASRGroups []db.ASRProvenanceGroup
	// FnRolesErr is the decide/scan call-evidence query's error. That query
	// scans the fn_calls log, so it has its own cache (modelsCaches.fnRoles) and
	// is best-effort: a failure marks only the Decide and Scan cards unknown,
	// never the whole snapshot. modelsCaches.read fills FnRoles and FnRolesErr.
	FnRolesErr error
}

// fnRole returns the call evidence for step, nil when the step has no current
// recipe.
func (s *modelsSnapshot) fnRole(step string) *db.FnRoleActivity {
	for i := range s.FnRoles {
		if s.FnRoles[i].Step == step {
			return &s.FnRoles[i]
		}
	}
	return nil
}

// currentRecipe returns step's current recipe, nil when it has none.
func (s *modelsSnapshot) currentRecipe(step string) *db.CurrentRecipe {
	for i := range s.Recipes {
		if s.Recipes[i].Step == step {
			return &s.Recipes[i]
		}
	}
	return nil
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

// modelsCaches holds the page's snapshot caches.
type modelsCaches struct {
	models  *refreshCache[modelsSnapshot]
	fnRoles *refreshCache[[]db.FnRoleActivity]
	stale   *refreshCache[staleSnapshot]
	queue   *refreshCache[*db.QueueStats]
}

func newModelsCaches(d DBInterface, logWarn func(msg string, args ...any)) modelsCaches {
	c := modelsCaches{
		models: newRefreshCache(modelsSnapshotTTL, modelsSnapshotTimeout,
			func(ctx context.Context) (modelsSnapshot, error) { return loadModelsSnapshot(ctx, d) }),
		fnRoles: newRefreshCache(fnRolesTTL, fnRolesTimeout, d.FnRoleActivity),
		stale: newRefreshCache(staleSnapshotTTL, staleSnapshotTimeout,
			func(ctx context.Context) (staleSnapshot, error) { return loadStaleSnapshot(ctx, d) }),
		queue: newRefreshCache(queueStatsTTL, queueStatsTimeout, d.GetServiceStatus),
	}
	// A failed FIRST stale count retries after the aggregates' TTL rather than
	// leaving the counts "unavailable" for the full 5 min.
	c.stale.firstRetryAfter = modelsSnapshotTTL
	c.fnRoles.firstRetryAfter = modelsSnapshotTTL
	if logWarn != nil {
		c.models.onError = func(err error, haveLast bool) {
			logWarn("models: snapshot refresh failed; serving last good counts", "error", err, "have_last_good", haveLast)
		}
		c.fnRoles.onError = func(err error, haveLast bool) {
			logWarn("models: fn_calls activity refresh failed; Decide/Scan cards unknown without a last good", "error", err, "have_last_good", haveLast)
		}
		c.stale.onError = func(err error, haveLast bool) {
			logWarn("models: stale-count refresh failed; serving last good counts", "error", err, "have_last_good", haveLast)
		}
		c.queue.onError = func(err error, haveLast bool) {
			logWarn("models: queue-stats refresh failed; serving last good stats", "error", err, "have_last_good", haveLast)
		}
	}
	return c
}

// queueStats returns the cached queue stats, nil while the first load is
// still running past wait or when no load has succeeded (the cards then show
// "—"). After the first load it never waits.
func (c modelsCaches) queueStats(ctx context.Context, wait time.Duration) (*db.QueueStats, error) {
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	v, err := c.queue.get(wctx)
	if v == nil {
		return nil, err
	}
	return v.Val, err
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

// fnRoleActivity returns the cached decide/scan call evidence. A refresh that
// failed after a good one keeps serving the last good value (logged by onError);
// with none, the error is returned and the Decide and Scan cards read unknown.
func (c modelsCaches) fnRoleActivity(ctx context.Context) ([]db.FnRoleActivity, error) {
	v, err := c.fnRoles.get(ctx)
	if v != nil {
		return v.Val, nil
	}
	if err == nil {
		err = errors.New("no value")
	}
	return nil, fmt.Errorf("models snapshot: %w", err)
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
		snap := m.Val // a copy: the cached entry is shared and immutable
		snap.FnRoles, snap.FnRolesErr = c.fnRoleActivity(ctx)
		e.Snap, e.SnapAt = &snap, m.At
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
