package mcp

import (
	"context"
	"os"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
)

// demoDB is an in-memory DBInterface implementation that serves synthetic data
// for the status dashboard. It lets the dashboard render with no Postgres,
// which is what makes local UI work and AI-agent visual verification cheap
// (see CLAUDE.md "Visual Verification"). It is never wired into production
// code paths — only `lil-whisper mcp --demo` constructs it.
//
// scenario selects which state to render so every UI path is visually testable:
//
//	active (default) — live runner, healthy backlog, a couple of failures
//	empty            — fresh install: zero counts, runner never seen
//	stale            — runner claimed a job but its heartbeat is hours old
//	failed           — failures including a long multi-line error string
type demoDB struct{ scenario string }

func (demoDB) Ping(context.Context) error { return nil }

func (demoDB) Search(context.Context, string, int, float64) ([]db.SearchResultWithMetadata, error) {
	return nil, nil
}

func (demoDB) TextSearch(context.Context, string, int) ([]db.SearchResultWithMetadata, error) {
	return nil, nil
}

func (demoDB) GetHierarchicalData(context.Context) ([]db.HierarchicalEntry, error) {
	return nil, nil
}

func (demoDB) GetChunkContext(context.Context, string, int) ([]db.SearchResultWithMetadata, error) {
	return nil, nil
}

// Requeue actions are no-ops in demo mode (no real DB) but succeed so the
// dashboard buttons are exercisable; the fixture data is unchanged on refresh.
func (demoDB) RequeueByID(_ context.Context, id string) (string, error) { return id, nil }

func (demoDB) RequeueFailed(context.Context) ([]string, error) { return []string{"demo"}, nil }

// GetServiceStatus returns a synthetic snapshot for the selected scenario.
func (d demoDB) GetServiceStatus(context.Context) (*db.QueueStats, error) {
	now := time.Now()
	switch d.scenario {
	case "empty":
		return &db.QueueStats{}, nil
	case "stale":
		hb := now.Add(-2 * time.Hour) // older than the 30m stale window
		return &db.QueueStats{
			Pending: 5, Claimed: 1, Done: 120, Failed: 0,
			Transcripts: 120, Chunks: 7431,
			RunnerActive: true, RunnerID: "demo-runner", LastHeartbeat: &hb,
		}, nil
	case "failed":
		hb := now.Add(-40 * time.Second)
		return &db.QueueStats{
			Pending: 3, Claimed: 1, Done: 88, Failed: 7,
			Transcripts: 88, Chunks: 5120,
			RunnerActive: true, RunnerID: "demo-runner", LastHeartbeat: &hb,
		}, nil
	default: // active
		hb := now.Add(-12 * time.Second)
		return &db.QueueStats{
			Pending: 42, Claimed: 1, Done: 317, Failed: 2,
			Transcripts: 317, Chunks: 18452,
			RunnerActive: true, RunnerID: "demo-runner", LastHeartbeat: &hb,
		}, nil
	}
}

// GetRecentJobs returns a synthetic job list for the selected scenario. File
// paths are generic placeholders, not real library paths.
func (d demoDB) GetRecentJobs(_ context.Context, limit int) ([]db.RecentJob, error) {
	if limit <= 0 {
		limit = 15
	}
	if d.scenario == "empty" {
		return nil, nil
	}
	now := time.Now()
	shortErr := "ffmpeg: unsupported codec in chapter 3; file skipped"
	longErr := "Traceback (most recent call last):\n" +
		"  File \"runner.py\", line 412, in transcribe\n" +
		"    result = model.transcribe(audio_path)\n" +
		"RuntimeError: CUDA out of memory. Tried to allocate 2.40 GiB " +
		"(GPU 0; 31.49 GiB total capacity; 28.12 GiB already allocated; 1.05 GiB free)"

	jobs := []db.RecentJob{
		{ID: "demo-1", FilePath: "/books/Author One/A Long Title/01.m4b", Status: "claimed", UpdatedAt: now.Add(-12 * time.Second)},
		{ID: "demo-2", FilePath: "/books/Author Two/Another Book/Another Book.m4b", Status: "done", UpdatedAt: now.Add(-3 * time.Minute)},
		{ID: "demo-3", FilePath: "/books/Author Three/Short Stories/Short Stories.mp3", Status: "failed", UpdatedAt: now.Add(-9 * time.Minute), Error: &shortErr},
		{ID: "demo-4", FilePath: "/books/Author Four/The Sequel/The Sequel.m4b", Status: "done", UpdatedAt: now.Add(-22 * time.Minute)},
		{ID: "demo-5", FilePath: "/books/Author Five/A Classic/A Classic.m4b", Status: "pending", UpdatedAt: now.Add(-31 * time.Minute)},
		{ID: "demo-6", FilePath: "/books/Author Six/A Novella/A Novella.m4b", Status: "done", UpdatedAt: now.Add(-48 * time.Minute)},
	}
	if d.scenario == "failed" {
		// Lead with a long, multi-line error to exercise the bounded error row.
		jobs = append([]db.RecentJob{
			{ID: "demo-0", FilePath: "/books/Author Seven/An Epic/An Epic.m4b", Status: "failed", UpdatedAt: now.Add(-15 * time.Second), Error: &longErr},
		}, jobs...)
	}
	if limit < len(jobs) {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

// StartDemoDashboard starts the HTTP transport (status dashboard + /mcp +
// /health + /readyz) backed by synthetic data, with no database connection.
// Intended for local UI iteration and AI-agent visual verification only.
// Set DEMO_SCENARIO=empty|stale|failed|active to render a specific state.
func StartDemoDashboard(addr string) error {
	if addr == "" {
		addr = ":8081"
	}
	scenario := os.Getenv("DEMO_SCENARIO")
	if scenario == "" {
		scenario = "active"
	}
	cfg := &config.Config{MCPHTTPAddr: addr, StaleJobTimeout: 30 * time.Minute}
	srv := NewMCPServer(demoDB{scenario: scenario}, cfg)
	srv.logger.Info("Starting DEMO dashboard (synthetic data, no database)",
		"address", addr, "scenario", scenario)
	return srv.StartHTTP(addr)
}
