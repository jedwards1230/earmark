// Package metrics exposes a narrow Prometheus surface for earmark (CONTRACT
// §2.16). It is deliberately gauges/counters only — NO per-job series (high-
// cardinality per-job history belongs in Postgres/Grafana, not Prometheus).
//
// The metric NAMES here are load-bearing: the homelab-k8s companion PR (alert
// rules, dashboards) depends on them verbatim. Do not rename without updating
// CONTRACT §2.16 and that PR.
//
// Design:
//   - Gauges that reflect current state (queue depth, backlog, coverage, ETA,
//     runner liveness/availability) are produced by a custom prometheus.Collector
//     (statsCollector) that reads QueueStats + the ETA model AT SCRAPE TIME, so
//     they never go stale and need no refresh goroutine.
//   - Counters (jobs failed) and the stage-duration histogram are registered and
//     incremented at the pipeline event-emit sites.
package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/predict"
)

// StatsSource is the read seam the scrape-time collector needs. *db.DB satisfies
// it. Kept minimal + an interface so the collector is unit-testable with a fake.
type StatsSource interface {
	GetServiceStatus(ctx context.Context) (*db.QueueStats, error)
	GetPredictInputs(ctx context.Context) (predict.Inputs, error)
}

// Registry bundles the earmark metrics: a dedicated prometheus.Registry (so the
// surface is explicit and test-isolated), the event-driven counters/histogram,
// and the scrape-time collector. Build it with New, mount Handler() on /metrics,
// and call the Record* methods from the event-emit sites.
type Registry struct {
	reg *prometheus.Registry

	jobsFailedTotal    prometheus.Counter
	jobsCompletedTotal prometheus.Counter
	stageDuration      *prometheus.HistogramVec
}

// New builds the earmark metric registry backed by src for the scrape-time
// gauges. scrapeTimeout bounds each scrape's DB read (0 → 2s default).
func New(src StatsSource, scrapeTimeout time.Duration) *Registry {
	if scrapeTimeout <= 0 {
		scrapeTimeout = 2 * time.Second
	}
	reg := prometheus.NewRegistry()

	r := &Registry{
		reg: reg,
		jobsFailedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "earmark_jobs_failed_total",
			Help: "Total transcription jobs observed failing (Go-side: stale-claim attempt-cap failures).",
		}),
		jobsCompletedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "earmark_jobs_completed_total",
			Help: "Total embed-stage completions observed by the Go worker (best-effort; the runner owns the job 'done' transition, so this counts Go-observable embed finishes, not the runner's mark-done).",
		}),
		stageDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "earmark_stage_duration_seconds",
			Help: "Per-stage processing duration in seconds, observed at the Go-emitted finish events (labels: stage).",
			// Buckets span sub-second embeds to multi-hour transcribes.
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600, 7200},
		}, []string{"stage"}),
	}

	reg.MustRegister(r.jobsFailedTotal, r.jobsCompletedTotal, r.stageDuration)
	reg.MustRegister(newStatsCollector(src, scrapeTimeout))
	// Standard Go runtime + process collectors for baseline observability.
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	return r
}

// Handler returns the /metrics HTTP handler for this registry.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// RecordStageFinish observes a stage's duration (on a Go-emitted finish event)
// and bumps the completed counter for embed finishes. Safe to call on a nil
// Registry (no-op) so emit sites needn't nil-check.
func (r *Registry) RecordStageFinish(stage string, d time.Duration) {
	if r == nil {
		return
	}
	r.stageDuration.WithLabelValues(stage).Observe(d.Seconds())
	if stage == db.StageEmbed {
		r.jobsCompletedTotal.Inc()
	}
}

// RecordJobFailed increments the failed-jobs counter (Go-observable failures).
// Safe on a nil Registry.
func (r *Registry) RecordJobFailed() {
	if r == nil {
		return
	}
	r.jobsFailedTotal.Inc()
}
