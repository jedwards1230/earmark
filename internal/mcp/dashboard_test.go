package mcp

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/db"
)

func TestCommafy(t *testing.T) {
	cases := map[int]string{0: "0", 42: "42", 1000: "1,000", 18452: "18,452", 1234567: "1,234,567", -2500: "-2,500"}
	for in, want := range cases {
		if got := commafy(in); got != want {
			t.Errorf("commafy(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanizeSince(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s ago"},
		{90 * time.Second, "1m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
		{-1 * time.Second, "just now"},
	}
	for _, c := range cases {
		if got := humanizeSince(c.d); got != c.want {
			t.Errorf("humanizeSince(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestNewDashboardData_RunnerStaleness(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	stale := 30 * time.Minute

	// Fresh heartbeat → active, not stale.
	recent := now.Add(-10 * time.Second)
	d := newDashboardData(&db.QueueStats{RunnerActive: true, LastHeartbeat: &recent}, nil, now, stale, "")
	if d.RunnerStale {
		t.Error("recent heartbeat should not be stale")
	}
	if d.HeartbeatRel != "10s ago" {
		t.Errorf("HeartbeatRel = %q, want %q", d.HeartbeatRel, "10s ago")
	}

	// Old heartbeat + still 'claimed' → stale (the crashed-runner case).
	old := now.Add(-2 * time.Hour)
	d = newDashboardData(&db.QueueStats{RunnerActive: true, LastHeartbeat: &old}, nil, now, stale, "")
	if !d.RunnerStale {
		t.Error("2h-old heartbeat with RunnerActive should be stale")
	}

	// Fresh install → Empty.
	d = newDashboardData(&db.QueueStats{}, nil, now, stale, "")
	if !d.Empty {
		t.Error("zero stats with no runner should be Empty")
	}
	if d.RunnerStale {
		t.Error("empty state should not be stale")
	}
}

// TestFragmentRendersStaleRunner asserts the stale-runner path renders the
// "stale" label (not a green "active" dot) — the misleading-output case.
func TestFragmentRendersStaleRunner(t *testing.T) {
	now := time.Now()
	old := now.Add(-90 * time.Minute)
	data := newDashboardData(
		&db.QueueStats{RunnerActive: true, RunnerID: "r1", LastHeartbeat: &old, Chunks: 12345},
		nil, now, 30*time.Minute, "",
	)
	var buf bytes.Buffer
	if err := fragmentTmpl.Execute(&buf, data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "stale — last seen") {
		t.Errorf("stale runner not rendered:\n%s", out)
	}
	if !strings.Contains(out, "dot amber") {
		t.Error("stale runner should use the amber dot, not green")
	}
	if !strings.Contains(out, "12,345") {
		t.Error("counts should be rendered with thousands separators")
	}
	if !strings.Contains(out, "updated ") {
		t.Error("fragment should carry an 'updated' recency stamp")
	}
}
