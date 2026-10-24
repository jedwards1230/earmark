package mcp

import (
	"context"
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

func TestBuildTrackRows(t *testing.T) {
	last := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	rows := buildTrackRows([]db.ModelTrack{
		{ModelAlias: "clef:27b", Apply: 1_903, Hold: 38_385, Reject: 7_675, Duplicate: 415, Last: last},
		{ModelAlias: "luna", PassesComplete: 4_287, PassesPartial: 41, PassesFailed: 7, PassesLegacy: 3, PassesRunning: 1},
		{ModelAlias: "jev", Apply: 0, Undone: 12}, // decided, all of it since undone
		{ModelAlias: ""},
	})
	require.Len(t, rows, 4)

	clef := rows[0]
	assert.Equal(t, "clef:27b", clef.Alias)
	assert.False(t, clef.HasPasses)
	assert.True(t, clef.HasDecisions)
	assert.Equal(t, []string{"1,903", "38,385", "7,675", "415", "0"}, []string{clef.Apply, clef.Hold, clef.Reject, clef.Duplicate, clef.Undone})
	assert.Equal(t, []string{"—", "—", "—", "—"}, []string{clef.Passes, clef.Complete, clef.Incomplete, clef.Legacy},
		"a model that never judged shows a dash, not a zero")
	assert.True(t, clef.Last.Equal(last))

	luna := rows[1]
	assert.True(t, luna.HasPasses)
	assert.False(t, luna.HasDecisions)
	assert.Equal(t, "4,339", luna.Passes, "every status, running included")
	assert.Equal(t, "all judge passes, including 1 still running", luna.PassesNote)
	assert.Equal(t, "4,287", luna.Complete)
	assert.Equal(t, "48", luna.Incomplete)
	assert.Equal(t, "41 partial · 7 failed", luna.IncompleteNote)
	assert.Equal(t, "3", luna.Legacy)
	assert.Equal(t, []string{"—", "—", "—", "—", "—"}, []string{luna.Apply, luna.Hold, luna.Reject, luna.Duplicate, luna.Undone})

	jev := rows[2]
	assert.True(t, jev.HasDecisions, "undone-only still decided")
	assert.Equal(t, []string{"0", "0", "0", "0", "12"}, []string{jev.Apply, jev.Hold, jev.Reject, jev.Duplicate, jev.Undone})

	assert.True(t, rows[3].NoAlias)
	assert.Equal(t, noAliasLabel, rows[3].Alias)
	assert.Empty(t, buildTrackRows(nil))
}

// trackDB serves a canned track record (or error), and records the deadline.
type trackDB struct {
	SimpleMockDB
	rows     []db.ModelTrack
	err      error
	deadline time.Time
	hasDL    bool
}

func (d *trackDB) ModelTrackRecord(ctx context.Context) ([]db.ModelTrack, error) {
	d.deadline, d.hasDL = ctx.Deadline()
	return d.rows, d.err
}

type slowTrackDB struct{ SimpleMockDB }

func (*slowTrackDB) ModelTrackRecord(ctx context.Context) ([]db.ModelTrack, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func serveTrackRecord(t *testing.T, d DBInterface) (int, string) {
	t.Helper()
	srv := NewMCPServer(d, &config.Config{})
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/track-record", nil))
	return w.Code, w.Body.String()
}

func TestTrackRecordData(t *testing.T) {
	last := time.Now().Add(-3 * time.Hour)
	d := &trackDB{rows: []db.ModelTrack{
		{ModelAlias: "clef:27b", Apply: 1_903, Hold: 38_385, Reject: 7_675, Duplicate: 415, Last: last},
		{ModelAlias: "luna<b>", PassesComplete: 4_287, PassesPartial: 41, PassesFailed: 7, PassesRunning: 1},
	}}
	code, out := serveTrackRecord(t, d)
	require.Equal(t, http.StatusOK, code)

	assert.True(t, d.hasDL, "the read runs under a deadline")
	assert.WithinDuration(t, time.Now().Add(trackRecordQueryTimeout), d.deadline, 2*time.Second)

	assert.Contains(t, out, `id="track-record-title">Track record</h2>`)
	assert.Contains(t, out, `<th scope="colgroup" colspan="4" class="group">Judge passes</th>`)
	assert.Contains(t, out, `<th scope="colgroup" colspan="5" class="group">Decisions</th>`)
	for _, h := range []string{"Passes", "Complete", "Partial / failed", "Legacy", "Apply", "Hold", "Reject", "Duplicate", "Undone"} {
		assert.Contains(t, out, ">"+h+"</th>", h)
	}
	assert.Contains(t, out, `<th scope="row" class="mono">clef:27b</th>`)
	assert.Contains(t, out, ">38,385<")
	assert.Contains(t, out, `title="41 partial · 7 failed"`)
	assert.Contains(t, out, `title="all judge passes, including 1 still running"`)
	assert.Contains(t, out, "3h ago")
	assert.NotContains(t, out, "luna<b>", "an alias is text, never markup")
	assert.Contains(t, out, "luna&lt;b&gt;")
	// One row per alias, plus the two header rows.
	assert.Equal(t, 4, strings.Count(out, "<tr>"))
}

func TestTrackRecordDataStates(t *testing.T) {
	t.Run("nothing recorded", func(t *testing.T) {
		code, out := serveTrackRecord(t, &trackDB{})
		assert.Equal(t, http.StatusOK, code)
		assert.Contains(t, out, "no judge passes or decisions recorded yet")
		assert.NotContains(t, out, "<table")
	})
	t.Run("a failed read is 200 unavailable, never the page's connection banner", func(t *testing.T) {
		code, out := serveTrackRecord(t, &trackDB{err: errors.New(`relation "judge_passes" does not exist`)})
		assert.Equal(t, http.StatusOK, code)
		assert.Contains(t, out, "track record unavailable")
		assert.NotContains(t, out, "judge_passes", "the database error stays in the log")
	})
	t.Run("a read that outlives its deadline renders unavailable", func(t *testing.T) {
		// The handler's own deadline is asserted in TestTrackRecordData; a
		// shorter one from the request keeps this test fast.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		srv := NewMCPServer(&slowTrackDB{}, &config.Config{})
		w := httptest.NewRecorder()
		srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/track-record", nil).WithContext(ctx))
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "track record unavailable")
	})
}

func TestTrackRecordRegionIsOnTheModelsPage(t *testing.T) {
	srv := NewMCPServer(&SimpleMockDB{}, &config.Config{})
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers", nil))
	require.Equal(t, http.StatusOK, w.Code)
	out := w.Body.String()
	assert.Contains(t, out, `id="track-record-region"`)
	assert.Contains(t, out, `hx-get="/servers/track-record"`)
	// Outside the polled Models region (a 5 s poll must not replace it), and
	// after the runner-update form, which stays under ASR runners.
	region := strings.Index(out, `id="track-record-region"`)
	assert.Greater(t, region, strings.Index(out, `id="runner-update-title"`))
	assert.Less(t, region, strings.Index(out, `class="about-page"`))
	modelsRegionEnd := strings.Index(out, `id="runner-update-title"`)
	assert.Greater(t, region, modelsRegionEnd)

	post := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/servers/track-record", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, post.Code, "GET only")
}

// TestDemoTrackRecord: every demo scenario renders the section through the
// real builder; empty has nothing, snapshot-error degrades only this section.
func TestDemoTrackRecord(t *testing.T) {
	for _, sc := range []string{"active", "idle", "failed", "empty", "snapshot-error"} {
		t.Run(sc, func(t *testing.T) {
			srv := newDemoServer(":0", sc)
			w := httptest.NewRecorder()
			srv.buildMux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/servers/track-record", nil))
			require.Equal(t, http.StatusOK, w.Code)
			out := w.Body.String()
			switch sc {
			case "empty":
				assert.Contains(t, out, "no judge passes or decisions recorded yet")
			case "snapshot-error":
				assert.Contains(t, out, "track record unavailable")
			default:
				assert.Contains(t, out, "clef:27b")
				assert.Contains(t, out, "38,385")
				assert.Contains(t, out, noAliasLabel)
			}
		})
	}
}
