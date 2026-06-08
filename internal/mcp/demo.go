package mcp

import (
	"context"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
)

// demoDB is an in-memory DBInterface implementation that serves synthetic data
// for the status dashboard. It lets the dashboard render with no Postgres,
// which is what makes local UI work and AI-agent visual verification cheap
// (see CLAUDE.md "Visual Verification"). It is never wired into production
// code paths — only `lil-whisper mcp --demo` constructs it.
type demoDB struct{}

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

// GetServiceStatus returns a believable mid-run snapshot: an active runner, a
// healthy backlog, and a couple of failures so every dashboard state renders.
func (demoDB) GetServiceStatus(context.Context) (*db.QueueStats, error) {
	heartbeat := time.Now().Add(-12 * time.Second)
	return &db.QueueStats{
		Pending:       42,
		Claimed:       1,
		Done:          317,
		Failed:        2,
		Transcripts:   317,
		Chunks:        18452,
		RunnerActive:  true,
		RunnerID:      "demo-runner",
		LastHeartbeat: &heartbeat,
	}, nil
}

// GetRecentJobs returns a fixed set of synthetic jobs covering every status
// (claimed/done/failed/pending) so the activity table exercises each badge and
// the error row. File paths are generic placeholders, not real library paths.
func (demoDB) GetRecentJobs(_ context.Context, limit int) ([]db.RecentJob, error) {
	if limit <= 0 {
		limit = 15
	}
	now := time.Now()
	failErr := "ffmpeg: unsupported codec in chapter 3; file skipped"
	jobs := []db.RecentJob{
		{ID: "demo-1", FilePath: "/books/Author One/A Long Title/01.m4b", Status: "claimed", UpdatedAt: now.Add(-12 * time.Second)},
		{ID: "demo-2", FilePath: "/books/Author Two/Another Book/Another Book.m4b", Status: "done", UpdatedAt: now.Add(-3 * time.Minute)},
		{ID: "demo-3", FilePath: "/books/Author Three/Short Stories/Short Stories.mp3", Status: "failed", UpdatedAt: now.Add(-9 * time.Minute), Error: &failErr},
		{ID: "demo-4", FilePath: "/books/Author Four/The Sequel/The Sequel.m4b", Status: "done", UpdatedAt: now.Add(-22 * time.Minute)},
		{ID: "demo-5", FilePath: "/books/Author Five/A Classic/A Classic.m4b", Status: "pending", UpdatedAt: now.Add(-31 * time.Minute)},
		{ID: "demo-6", FilePath: "/books/Author Six/A Novella/A Novella.m4b", Status: "done", UpdatedAt: now.Add(-48 * time.Minute)},
	}
	if limit < len(jobs) {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

// StartDemoDashboard starts the HTTP transport (status dashboard + /mcp +
// /health + /readyz) backed by synthetic data, with no database connection.
// Intended for local UI iteration and AI-agent visual verification only.
func StartDemoDashboard(addr string) error {
	if addr == "" {
		addr = ":8081"
	}
	srv := NewMCPServer(demoDB{}, &config.Config{MCPHTTPAddr: addr})
	srv.logger.Info("Starting DEMO dashboard (synthetic data, no database)", "address", addr)
	return srv.StartHTTP(addr)
}
