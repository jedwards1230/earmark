package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jedwards1230/earmark/internal/predict"
)

// statsCollector is a prometheus.Collector that reads QueueStats + the ETA model
// at scrape time and emits the current-state gauges. Reading on scrape keeps the
// gauges fresh with no background goroutine.
type statsCollector struct {
	src     StatsSource
	timeout time.Duration

	// metric descriptors (names are load-bearing; CONTRACT §2.16)
	jobs              *prometheus.Desc // earmark_jobs{status}
	embedBacklog      *prometheus.Desc // earmark_embed_backlog
	evalCoverage      *prometheus.Desc // earmark_eval_coverage_ratio
	lastHeartbeatSecs *prometheus.Desc // earmark_runner_last_heartbeat_seconds
	runnerAvailable   *prometheus.Desc // earmark_runner_available
	etaWork           *prometheus.Desc // earmark_eta_work_seconds
	etaCalendar       *prometheus.Desc // earmark_eta_calendar_seconds
}

func newStatsCollector(src StatsSource, timeout time.Duration) *statsCollector {
	return &statsCollector{
		src:     src,
		timeout: timeout,
		jobs: prometheus.NewDesc(
			"earmark_jobs",
			"Current transcription_jobs count by status (pending|claimed|done|failed).",
			[]string{"status"}, nil),
		embedBacklog: prometheus.NewDesc(
			"earmark_embed_backlog",
			"Completed transcripts with no chunks yet (the embed worker's needs-embedding set).",
			nil, nil),
		evalCoverage: prometheus.NewDesc(
			"earmark_eval_coverage_ratio",
			"Fraction of done jobs that have been judged (run_metrics.eval_finished_at non-NULL / done jobs); 0 when no done jobs.",
			nil, nil),
		lastHeartbeatSecs: prometheus.NewDesc(
			"earmark_runner_last_heartbeat_seconds",
			"Seconds since the runner's last CLAIM-ACTIVITY (it only stamps a heartbeat while a job is claimed — there is no idle heartbeat). NOT emitted when there is no claim/completion history. Cannot distinguish 'idle, queue empty' from 'down': pair with earmark_jobs{status=pending|claimed}>0 before alerting.",
			nil, nil),
		runnerAvailable: prometheus.NewDesc(
			"earmark_runner_available",
			"1 when the runner host is available for transcription (gpu-arbiter not gaming), 0 when gaming/evicting. Only emitted when a runner_availability signal has been observed.",
			nil, nil),
		etaWork: prometheus.NewDesc(
			"earmark_eta_work_seconds",
			"Estimated busy-seconds to process the remaining chunks (work-time ETA, internal/predict). Only emitted when there is remaining work and rate history.",
			nil, nil),
		etaCalendar: prometheus.NewDesc(
			"earmark_eta_calendar_seconds",
			"Estimated wall-clock seconds to drain the queue accounting for runner availability (calendar ETA). Only emitted when availability history exists.",
			nil, nil),
	}
}

// Describe sends the static descriptors.
func (c *statsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.jobs
	ch <- c.embedBacklog
	ch <- c.evalCoverage
	ch <- c.lastHeartbeatSecs
	ch <- c.runnerAvailable
	ch <- c.etaWork
	ch <- c.etaCalendar
}

// Collect reads current state and emits the gauges. A read error emits nothing
// (the scrape simply omits earmark gauges that cycle) rather than failing — a DB
// hiccup must not break the whole /metrics scrape.
func (c *statsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	st, err := c.src.GetServiceStatus(ctx)
	if err != nil || st == nil {
		return
	}

	g := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}

	g(c.jobs, float64(st.Pending), "pending")
	g(c.jobs, float64(st.Claimed), "claimed")
	g(c.jobs, float64(st.Done), "done")
	g(c.jobs, float64(st.Failed), "failed")
	g(c.embedBacklog, float64(st.EmbedBacklog))

	// Eval coverage ratio: done jobs with eval_finished_at / done jobs. 0 when no
	// done jobs (avoids divide-by-zero). EvalCoverageDone is provided by the DB.
	if st.Done > 0 {
		g(c.evalCoverage, float64(st.EvalCoverageDone)/float64(st.Done))
	} else {
		g(c.evalCoverage, 0)
	}

	// Heartbeat age — ONLY emitted when there is real claim/completion activity to
	// measure (LatestActivity non-nil). When the queue is drained or paused there
	// is no heartbeat, so emitting a multi-day age would be a lie an alert would
	// misread as "runner down" (CONTRACT §1.7). Absent gauge = "no recent activity
	// to measure," which is the honest signal.
	if st.LatestActivity != nil {
		age := time.Since(*st.LatestActivity).Seconds()
		if age < 0 {
			age = 0
		}
		g(c.lastHeartbeatSecs, age)
	}

	// Runner availability: only emit when an availability signal exists.
	if st.RunnerAvailableKnown {
		v := 0.0
		if st.RunnerAvailable {
			v = 1
		}
		g(c.runnerAvailable, v)
	}

	// ETA gauges from the empirical model. Emit work-seconds only when there is
	// real work; calendar-seconds only when the calendar estimate is known.
	if in, perr := c.src.GetPredictInputs(ctx); perr == nil {
		est := predict.Compute(in)
		if est.HasWork {
			g(c.etaWork, est.WorkSeconds)
		}
		if est.CalendarKnown {
			g(c.etaCalendar, est.CalendarSeconds)
		}
	}
}
