package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/predict"
)

// fakeSource is a static StatsSource for the collector.
type fakeSource struct {
	stats *db.QueueStats
	in    predict.Inputs
	err   error
}

func (f fakeSource) GetServiceStatus(context.Context) (*db.QueueStats, error) {
	return f.stats, f.err
}
func (f fakeSource) GetPredictInputs(context.Context) (predict.Inputs, error) {
	return f.in, f.err
}

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestRegistry_RegistersAndScrapes(t *testing.T) {
	now := time.Now().Add(-30 * time.Second)
	src := fakeSource{
		stats: &db.QueueStats{
			Pending: 5, Claimed: 1, Done: 100, Failed: 2,
			EmbedBacklog:         3,
			EvalCoverageDone:     40,
			LatestActivity:       &now,
			RunnerAvailable:      true,
			RunnerAvailableKnown: true,
		},
		in: predict.Inputs{
			RemainingChunks:      100,
			Rates:                predict.Rates{TranscribeSecPerChunk: 7, EmbedSecPerChunk: 0.5, EvalSecPerChunk: 1, EvalKnown: true},
			AvailabilityFraction: 0.5,
		},
	}

	r := New(src, time.Second)
	body := scrape(t, r)

	wantContains := []string{
		`earmark_jobs{status="pending"} 5`,
		`earmark_jobs{status="claimed"} 1`,
		`earmark_jobs{status="done"} 100`,
		`earmark_jobs{status="failed"} 2`,
		`earmark_embed_backlog 3`,
		`earmark_eval_coverage_ratio 0.4`,
		`earmark_runner_available 1`,
		`earmark_runner_last_heartbeat_seconds`,
		`earmark_eta_work_seconds`,
		`earmark_eta_calendar_seconds`,
		`earmark_jobs_failed_total 0`,
		`earmark_jobs_completed_total 0`,
	}
	for _, w := range wantContains {
		if !strings.Contains(body, w) {
			t.Errorf("/metrics missing %q\n---\n%s", w, body)
		}
	}
}

// TestHeartbeatGaugeOmittedWhenNoActivity asserts the honesty rule: with no
// claim/completion activity the heartbeat-age gauge is NOT emitted (so an alert
// can't misread a multi-day age as "down" when the truth is "idle, queue empty").
func TestHeartbeatGaugeOmittedWhenNoActivity(t *testing.T) {
	src := fakeSource{
		stats: &db.QueueStats{Done: 0, LatestActivity: nil, RunnerAvailableKnown: false},
		in:    predict.Inputs{},
	}
	r := New(src, time.Second)
	body := scrape(t, r)

	if strings.Contains(body, "earmark_runner_last_heartbeat_seconds ") {
		t.Errorf("heartbeat gauge must be omitted when there is no activity:\n%s", body)
	}
	if strings.Contains(body, "earmark_runner_available ") {
		t.Errorf("runner_available must be omitted until a signal exists:\n%s", body)
	}
	// Coverage with zero done jobs is 0, not NaN.
	if !strings.Contains(body, "earmark_eval_coverage_ratio 0") {
		t.Errorf("eval coverage with zero done jobs should be 0:\n%s", body)
	}
}

func TestRecordCounters(t *testing.T) {
	src := fakeSource{stats: &db.QueueStats{}, in: predict.Inputs{}}
	r := New(src, time.Second)

	r.RecordStageFinish(db.StageEmbed, 2*time.Second) // also bumps completed
	r.RecordStageFinish(db.StageEval, time.Second)    // does not bump completed
	r.RecordJobFailed()
	r.RecordJobFailed()

	body := scrape(t, r)
	if !strings.Contains(body, "earmark_jobs_completed_total 1") {
		t.Errorf("expected completed_total 1 after one embed finish:\n%s", body)
	}
	if !strings.Contains(body, "earmark_jobs_failed_total 2") {
		t.Errorf("expected failed_total 2:\n%s", body)
	}
	if !strings.Contains(body, `earmark_stage_duration_seconds_count{stage="embed"} 1`) {
		t.Errorf("expected one embed stage observation:\n%s", body)
	}
}

// TestNilRegistrySafe confirms the Record* methods are no-ops on a nil Registry.
func TestNilRegistrySafe(t *testing.T) {
	var r *Registry
	r.RecordStageFinish(db.StageEmbed, time.Second)
	r.RecordJobFailed()
}
