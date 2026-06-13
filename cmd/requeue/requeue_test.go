package requeue

import (
	"context"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
)

type fakeRequeuer struct {
	jobs   []db.JobMatch
	failed []db.JobMatch
	called string // which mutate method ran
}

func (f *fakeRequeuer) FindJobs(_ context.Context, _ string) ([]db.JobMatch, error) {
	return f.jobs, nil
}
func (f *fakeRequeuer) FindFailedJobs(_ context.Context) ([]db.JobMatch, error) {
	return f.failed, nil
}
func (f *fakeRequeuer) RequeueJobs(_ context.Context, _ string) ([]string, error) {
	f.called = "requeue"
	return pathsOf(f.jobs), nil
}
func (f *fakeRequeuer) RequeueFailed(_ context.Context) ([]string, error) {
	f.called = "failed"
	return pathsOf(f.failed), nil
}
func (f *fakeRequeuer) ReembedJobs(_ context.Context, _ string) ([]string, error) {
	f.called = "reembed"
	return pathsOf(f.jobs), nil
}

func pathsOf(ms []db.JobMatch) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.FilePath
	}
	return out
}

func TestRun_DryRunDoesNotMutate(t *testing.T) {
	f := &fakeRequeuer{jobs: []db.JobMatch{{ID: "1", FilePath: "/b/Dune.m4b", Status: "done"}}}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Dune", options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.called != "" {
		t.Errorf("dry-run must not mutate, but %q was called", f.called)
	}
	if !strings.Contains(out.String(), "dry-run") || !strings.Contains(out.String(), "Dune.m4b") {
		t.Errorf("preview output missing dry-run/match:\n%s", out.String())
	}
}

func TestRun_YesReTranscribes(t *testing.T) {
	f := &fakeRequeuer{jobs: []db.JobMatch{{ID: "1", FilePath: "/b/Dune.m4b", Status: "failed"}}}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Dune", options{yes: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.called != "requeue" {
		t.Errorf("want requeue called, got %q", f.called)
	}
}

func TestRun_FailedMode(t *testing.T) {
	f := &fakeRequeuer{failed: []db.JobMatch{{ID: "2", FilePath: "/b/X.m4b", Status: "failed"}}}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "", options{failed: true, yes: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.called != "failed" {
		t.Errorf("want failed called, got %q", f.called)
	}
}

func TestRun_ReembedMode(t *testing.T) {
	f := &fakeRequeuer{jobs: []db.JobMatch{{ID: "3", FilePath: "/b/Y.m4b", Status: "done"}}}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "Y", options{reembed: true, yes: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.called != "reembed" {
		t.Errorf("want reembed called, got %q", f.called)
	}
}

func TestRun_RejectsReembedWithFailed(t *testing.T) {
	f := &fakeRequeuer{}
	err := run(context.Background(), &strings.Builder{}, f, "", options{reembed: true, failed: true})
	if err == nil {
		t.Fatal("expected error combining --reembed and --failed")
	}
}

func TestRun_RequiresTarget(t *testing.T) {
	f := &fakeRequeuer{}
	err := run(context.Background(), &strings.Builder{}, f, "", options{})
	if err == nil {
		t.Fatal("expected error when no substring/--failed/--reembed given")
	}
}

func TestRun_NoMatches(t *testing.T) {
	f := &fakeRequeuer{}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "nope", options{yes: true}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.called != "" {
		t.Error("no matches must not mutate")
	}
	if !strings.Contains(out.String(), "No matching jobs") {
		t.Errorf("want no-matches message, got:\n%s", out.String())
	}
}
