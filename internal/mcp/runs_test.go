package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
)

var runsNow = time.Date(2026, 10, 9, 22, 0, 0, 0, time.UTC)

func testRun(mut func(*db.StepRun)) db.StepRun {
	r := db.StepRun{ID: 7, Step: "decide", Mode: db.StepRunWrite, Status: db.StepRunRunning,
		StartedAt: runsNow.Add(-10 * time.Minute), HeartbeatAt: runsNow.Add(-time.Second), ObservedAt: runsNow,
		Total: i64(1000), Done: i64(250), Message: "asking jev-1.13.0"}
	if mut != nil {
		mut(&r)
	}
	return r
}

func TestBuildRunView(t *testing.T) {
	for _, tc := range []struct {
		name               string
		run                db.StepRun
		status, eta, done  string
		pct                int
		hasTotal, stale    bool
		elapsed, modeLabel string
	}{
		{name: "live with total", run: testRun(nil), status: "running", pct: 25, hasTotal: true,
			done: "250", elapsed: "10m0s", eta: "30m0s", modeLabel: "write"},
		{name: "no total", run: testRun(func(r *db.StepRun) { r.Total = nil }), status: "running",
			done: "250", elapsed: "10m0s", modeLabel: "write"},
		{name: "stale: no ETA, elapsed stops at the last heartbeat",
			run:    testRun(func(r *db.StepRun) { r.HeartbeatAt = runsNow.Add(-3 * time.Minute) }),
			status: "stale", stale: true, pct: 25, hasTotal: true, done: "250", elapsed: "7m0s", modeLabel: "write"},
		{name: "done dry run",
			run: testRun(func(r *db.StepRun) {
				f := runsNow.Add(-5 * time.Minute)
				r.FinishedAt, r.Status, r.Mode, r.Done = &f, db.StepRunDone, db.StepRunDryRun, i64(1000)
			}),
			status: "done", pct: 100, hasTotal: true, done: "1,000", elapsed: "5m0s", modeLabel: "dry run"},
		{name: "done past total clamps the bar", run: testRun(func(r *db.StepRun) { r.Done = i64(1200) }),
			status: "running", pct: 100, hasTotal: true, done: "1,200", elapsed: "10m0s", modeLabel: "write"},
		{name: "no progress yet: no ETA", run: testRun(func(r *db.StepRun) { r.Done = nil }),
			status: "running", pct: 0, hasTotal: true, done: "0", elapsed: "10m0s", modeLabel: "write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := buildRunView(tc.run)
			assert.Equal(t, tc.status, v.Status)
			assert.Equal(t, tc.stale, v.Stale)
			assert.Equal(t, tc.pct, v.Pct)
			assert.Equal(t, tc.hasTotal, v.HasTotal)
			assert.Equal(t, tc.done, v.DoneText)
			assert.Equal(t, tc.eta, v.ETA)
			assert.Equal(t, tc.elapsed, v.Elapsed)
			assert.Equal(t, tc.modeLabel, v.ModeLabel)
		})
	}
}

func TestSortedCounts(t *testing.T) {
	got := sortedCounts(map[string]int64{"errors": 1, "reject": 3, "apply": 5, "cached": 2, "hold": 4, "chunk_errors": 9})
	var keys []string
	for _, c := range got {
		keys = append(keys, c.Key)
	}
	assert.Equal(t, []string{"apply", "hold", "reject", "cached", "chunk errors", "errors"}, keys)
}

func TestLinkRoleRuns(t *testing.T) {
	staleDecide := testRun(func(r *db.StepRun) { r.ID, r.HeartbeatAt = 9, runsNow.Add(-time.Hour) })
	liveDecide := testRun(func(r *db.StepRun) { r.ID = 8 })
	revert := testRun(func(r *db.StepRun) { r.ID, r.Step = 5, "decide_revert" })
	backfill := testRun(func(r *db.StepRun) { r.ID, r.Step, r.Total = 6, "eval_backfill", nil })
	roles := []roleCard{{Key: "decide"}, {Key: "scan"}, {Key: "judge"}}
	linkRoleRuns(roles, []db.StepRun{staleDecide, liveDecide, revert, backfill})

	require.NotNil(t, roles[0].ActiveRun)
	assert.Equal(t, int64(8), roles[0].ActiveRun.ID, "a live run wins over a newer stale one")
	assert.Equal(t, "decide (write) · 250 / 1,000 · asking jev-1.13.0", roles[0].ActiveRun.Text)
	assert.Nil(t, roles[1].ActiveRun)
	require.NotNil(t, roles[2].ActiveRun)
	assert.Equal(t, int64(6), roles[2].ActiveRun.ID)
	assert.Equal(t, "eval backfill (write) · 250 · asking jev-1.13.0", roles[2].ActiveRun.Text)

	only := []roleCard{{Key: "decide"}}
	linkRoleRuns(only, []db.StepRun{staleDecide})
	require.NotNil(t, only[0].ActiveRun)
	assert.True(t, only[0].ActiveRun.Stale)
	assert.Contains(t, only[0].ActiveRun.Text, "· stale")
}

type runsErrDB struct{ SimpleMockDB }

func (*runsErrDB) StepRuns(context.Context, int, int) (db.StepRunList, error) {
	return db.StepRunList{}, errors.New(`relation "step_runs" does not exist`)
}

// slowRunsDB blocks until the request's deadline: the handler must not.
type slowRunsDB struct{ SimpleMockDB }

func (*slowRunsDB) StepRuns(ctx context.Context, _, _ int) (db.StepRunList, error) {
	<-ctx.Done()
	return db.StepRunList{}, ctx.Err()
}

// TestRunsData_FailureRendersUnavailable: a failed or hung runs read renders
// 200 "unavailable" (never the page-wide connection-lost banner), and a hung
// one is cut at runsQueryTimeout, inside the region's 5 s htmx timeout.
func TestRunsData_FailureRendersUnavailable(t *testing.T) {
	for name, d := range map[string]DBInterface{"error": &runsErrDB{}, "hang": &slowRunsDB{}} {
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer(d, &config.Config{})
			w := httptest.NewRecorder()
			start := time.Now()
			srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs/data", nil))
			assert.Less(t, time.Since(start), runsQueryTimeout+time.Second)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), "run status unavailable")
		})
	}
	// The Models page still renders, without role links.
	srv := NewMCPServer(&runsErrDB{}, &config.Config{})
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/data", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "<dt>running</dt>")
}

func TestAPIRuns(t *testing.T) {
	srv := NewMCPServer(&SimpleMockDB{}, &config.Config{})
	mux := srv.buildMux()
	for _, bad := range []string{"0", "101", "x"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/runs?recent="+bad, nil))
		assert.Equal(t, http.StatusBadRequest, w.Code, bad)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"active":[],"recent":[],"staleAfterSeconds":120}`, w.Body.String())

	w = httptest.NewRecorder()
	NewMCPServer(&runsErrDB{}, &config.Config{}).buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestAPIRunFrom(t *testing.T) {
	a := apiRunFrom(testRun(nil))
	assert.Equal(t, "running", a.Status)
	require.NotNil(t, a.Progress)
	assert.InDelta(t, 0.25, *a.Progress, 1e-9)
	require.NotNil(t, a.ETASecs)
	assert.InDelta(t, 1800, *a.ETASecs, 1e-6)
	assert.NotNil(t, a.Counters, "counters is always an object")

	s := apiRunFrom(testRun(func(r *db.StepRun) { r.HeartbeatAt = runsNow.Add(-5 * time.Minute) }))
	assert.Equal(t, "stale", s.Status)
	assert.True(t, s.Stale)
	assert.Nil(t, s.ETASecs)
}

// TestDemoRunsPanel: every demo scenario renders the panel through the real
// builders; active shows a live and a stale run, empty shows nothing.
func TestDemoRunsPanel(t *testing.T) {
	for _, sc := range []string{"active", "empty", "stale", "failed", "idle", "batch-analyze"} {
		t.Run(sc, func(t *testing.T) {
			srv := newDemoServer(":0", sc)
			w := httptest.NewRecorder()
			srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs/data", nil))
			require.Equal(t, http.StatusOK, w.Code)
			out := w.Body.String()
			switch sc {
			case "active", "batch-analyze":
				assert.Contains(t, out, "● running")
				assert.Contains(t, out, "12,400 / 29,000 · 42%")
				assert.Contains(t, out, "⚠ stale")
			case "empty":
				assert.Contains(t, out, "Nothing running")
				assert.Contains(t, out, "No runs recorded yet")
			case "stale":
				assert.Contains(t, out, "⚠ stale")
				assert.NotContains(t, out, "● running")
			case "failed":
				assert.Contains(t, out, "total unknown")
				assert.Contains(t, out, "✗ failed")
			case "idle":
				assert.Contains(t, out, "Nothing running")
				assert.Contains(t, out, "✓ done")
			}
			// The status API carries the same runs.
			w = httptest.NewRecorder()
			srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil))
			require.Equal(t, http.StatusOK, w.Code)
			var api apiRuns
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &api))
			assert.Equal(t, strings.Count(out, `class="panel server-card run-card`), len(api.Active))
		})
	}
}
