package mcp

// Postgres proof of the "Running now" panel (CONTRACT §1.10): real step_runs
// rows → db.StepRuns → the rendered /runs/data fragment, the Decide card's
// link, and GET /api/v1/runs. Skipped unless EARMARK_TEST_DATABASE_URL.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/db"
)

func TestIntegrationRunsPanel(t *testing.T) {
	d, conn := modelsIntegrationDB(t)
	ctx := context.Background()

	// A live decide --yes, 40% through, asking the model.
	total, done, cost := int64(29_000), int64(11_600), 2.5
	live, err := d.StartStepRun(ctx, db.StepRunStart{Step: "decide", Mode: db.StepRunWrite, Model: itJev,
		Args: "--yes --limit 29000", Host: "earmark-mcp-0", Total: &total})
	require.NoError(t, err)
	require.NoError(t, d.UpdateStepRun(ctx, live, db.StepRunProgress{RecipeID: strings.Repeat("d1", 32), Total: &total,
		Done: &done, Counters: map[string]int64{"apply": 4000, "hold": 5000, "reject": 2600}, CostUSD: &cost,
		Message: "page 24: asking " + itJev}))
	_, err = conn.Exec(ctx, `UPDATE step_runs SET started_at = now() - interval '10 minutes' WHERE id = $1`, live)
	require.NoError(t, err)

	// A scan whose exec was killed: running, heartbeat 5 minutes old.
	stale, err := d.StartStepRun(ctx, db.StepRunStart{Step: "scan", Mode: db.StepRunWrite, Model: itJev, Args: "--sample 500"})
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `UPDATE step_runs SET started_at = now() - interval '30 minutes',
	                                             heartbeat_at = now() - interval '5 minutes' WHERE id = $1`, stale)
	require.NoError(t, err)

	// A finished dry run.
	fin, err := d.StartStepRun(ctx, db.StepRunStart{Step: "decide", Mode: db.StepRunDryRun})
	require.NoError(t, err)
	n := int64(200)
	require.NoError(t, d.FinishStepRun(ctx, fin, db.StepRunDone, db.StepRunProgress{Total: &n, Done: &n,
		Counters: map[string]int64{"apply": 60}}, ""))

	srv := NewMCPServer(d, modelsITConfig(true))
	srv.endpointProber = demoEndpointProber{}
	srv.gatewayProber = demoGatewayProber{}
	mux := srv.buildMux()
	get := func(path string) string {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, w.Code, path)
		return w.Body.String()
	}

	out := get("/runs/data")
	liveCard := cut(t, out, `id="run-`+strconv.FormatInt(live, 10)+`"`, "</dl>")
	for _, want := range []string{"decide", "write", "● running", "page 24: asking " + itJev,
		"11,600 / 29,000 · 40%", `style="width:40%"`, "apply 4,000 · hold 5,000 · reject 2,600", "ETA ", "$2.5000", "earmark-mcp-0"} {
		assert.Contains(t, liveCard, want, "live card")
	}
	staleCard := cut(t, out, `id="run-`+strconv.FormatInt(stale, 10)+`"`, "</dl>")
	for _, want := range []string{"⚠ stale", "last heartbeat 5m ago", "dot amber", "stale-note"} {
		assert.Contains(t, staleCard, want, "stale card")
	}
	assert.NotContains(t, staleCard, "ETA", "a stale run has no ETA")
	recent := cut(t, out, `id="runs-recent-title"`, "</table>")
	assert.Contains(t, recent, "✓ done")
	assert.Contains(t, recent, "dry run")

	// The Models page: the Decide card links to the live run (not the stale
	// scan); the Scan card links to its stale run, marked stale.
	models := get("/servers/data")
	assert.Contains(t, roleCardHTML(t, models, "decide"), `href="#run-`+strconv.FormatInt(live, 10)+`"`)
	scanCard := roleCardHTML(t, models, "scan")
	assert.Contains(t, scanCard, `href="#run-`+strconv.FormatInt(stale, 10)+`"`)
	assert.Contains(t, scanCard, "· stale")

	// The API: same rows, stale computed at read time.
	var api apiRuns
	require.NoError(t, json.Unmarshal([]byte(get("/api/v1/runs")), &api))
	require.Len(t, api.Active, 2)
	assert.Equal(t, "running", api.Active[0].Status)
	assert.InDelta(t, 0.4, *api.Active[0].Progress, 1e-9)
	assert.NotNil(t, api.Active[0].ETASecs)
	assert.Equal(t, "stale", api.Active[1].Status)
	assert.True(t, api.Active[1].Stale)
	require.Len(t, api.Recent, 1)
	assert.Equal(t, 120, api.StaleAfterSeconds)
}

// cut returns out from the first `from` to the next `to` after it.
func cut(t *testing.T, out, from, to string) string {
	t.Helper()
	i := strings.Index(out, from)
	require.GreaterOrEqual(t, i, 0, "%s not rendered", from)
	j := strings.Index(out[i:], to)
	require.Positive(t, j, "%s after %s", to, from)
	return out[i : i+j]
}
