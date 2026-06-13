package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
)

func TestModelSize(t *testing.T) {
	cases := map[string]string{
		"nvidia/parakeet-tdt-0.6b-v3": "0.6B",
		"nemo-canary-1b":              "1B",
		"some-model-7B-instruct":      "7B",
		"whisper-large-v3":            "", // no param-size token
		"":                            "",
	}
	for in, want := range cases {
		if got := modelSize(in); got != want {
			t.Errorf("modelSize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildServerViews_States(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	stale := 30 * time.Minute
	parakeet := "nvidia/parakeet-tdt-0.6b-v3"
	bf16 := "bfloat16"
	tp := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	fp := func(v float64) *float64 { return &v }

	configured := []config.ASRServer{
		{Name: "desktop-1", Host: "192.168.8.10", Model: parakeet, Role: "primary"},
		{Name: "linux-1", Host: "192.168.8.31", Model: parakeet, Role: "fallback"},
		{Name: "pi-1", Model: parakeet, Role: "fallback"}, // never seen
	}
	obs := &db.ServerObservation{
		LiveRunners: []db.LiveRunner{
			{ClaimedBy: "asr-runner-desktop-1", ClaimedCount: 1, LastHeartbeat: now.Add(-20 * time.Second),
				CurrentFile: "/books/Author/Book/01 - Chapter 1.mp3"},
		},
		Hosts: []db.HostMetrics{
			{Host: "desktop-1", ASRModel: &parakeet, ComputeType: &bf16, JobsDone: 300, LastFinished: tp(-3 * time.Minute), AvgProcessingSeconds: fp(487.5)},
			{Host: "linux-1", ASRModel: &parakeet, JobsDone: 12, LastFinished: tp(-6 * time.Hour)},
			{Host: "mini-1", JobsDone: 5, LastFinished: tp(-48 * time.Hour)}, // unconfigured
		},
	}

	views := buildServerViews(configured, obs, nil, now, stale)
	byName := map[string]serverView{}
	for _, v := range views {
		byName[v.Name] = v
	}

	// desktop-1: fresh live claim → TRANSCRIBING, observed model + size, file in sub.
	d := byName["desktop-1"]
	if d.State.Label != "TRANSCRIBING" {
		t.Errorf("desktop-1 state = %q, want TRANSCRIBING", d.State.Label)
	}
	if !strings.Contains(d.State.Sub, "01 - Chapter 1.mp3") {
		t.Errorf("desktop-1 sub = %q, want it to name the in-flight file", d.State.Sub)
	}
	if d.Model != parakeet || d.ModelSource != "observed" || d.ModelSize != "0.6B" || d.ComputeMode != "bfloat16" {
		t.Errorf("desktop-1 model fields wrong: %+v", d)
	}
	if !d.Configured || d.Role != "primary" {
		t.Errorf("desktop-1 should be configured primary: %+v", d)
	}

	// linux-1: history but no live claim → IDLE.
	if l := byName["linux-1"]; l.State.Label != "IDLE" || !strings.Contains(l.State.Sub, "last active") {
		t.Errorf("linux-1 state = %q sub = %q, want IDLE/last active", l.State.Label, l.State.Sub)
	}

	// pi-1: configured, never observed → NOT SEEN, model falls back to configured.
	p := byName["pi-1"]
	if p.State.Label != "NOT SEEN" {
		t.Errorf("pi-1 state = %q, want NOT SEEN", p.State.Label)
	}
	if p.Model != parakeet || p.ModelSource != "configured" {
		t.Errorf("pi-1 model should be configured fallback: %+v", p)
	}

	// mini-1: observed but unconfigured → present, Configured=false.
	m, ok := byName["mini-1"]
	if !ok {
		t.Fatal("unconfigured host mini-1 should still be rendered")
	}
	if m.Configured {
		t.Errorf("mini-1 should be Configured=false")
	}
	if m.State.Label != "IDLE" {
		t.Errorf("mini-1 state = %q, want IDLE", m.State.Label)
	}
}

func TestBuildServerViews_StaleClaim(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	stale := 30 * time.Minute
	configured := []config.ASRServer{{Name: "desktop-1", Role: "primary"}}
	obs := &db.ServerObservation{
		LiveRunners: []db.LiveRunner{
			{ClaimedBy: "asr-runner-desktop-1", ClaimedCount: 1, LastHeartbeat: now.Add(-2 * time.Hour)},
		},
	}
	views := buildServerViews(configured, obs, nil, now, stale)
	if len(views) != 1 || views[0].State.Label != "STALLED" {
		t.Fatalf("want one STALLED view, got %+v", views)
	}
	if !strings.Contains(views[0].State.Sub, "stale") {
		t.Errorf("stalled sub = %q, want it to mention staleness", views[0].State.Sub)
	}
}

func TestBuildServerViews_UnconfiguredRunnerPairsHost(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	parakeet := "nvidia/parakeet-tdt-0.6b-v3"
	tp := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	obs := &db.ServerObservation{
		LiveRunners: []db.LiveRunner{
			{ClaimedBy: "asr-runner-desktop-1", ClaimedCount: 1, LastHeartbeat: now.Add(-10 * time.Second)},
		},
		Hosts: []db.HostMetrics{
			{Host: "desktop-1", ASRModel: &parakeet, JobsDone: 9, LastFinished: tp(-time.Minute)},
		},
	}
	// No configured servers: the live runner must pair with the host whose name is
	// a substring of claimed_by, so its model shows and the host isn't double-rendered.
	views := buildServerViews(nil, obs, nil, now, 30*time.Minute)
	if len(views) != 1 {
		t.Fatalf("want a single merged unconfigured view, got %d: %+v", len(views), views)
	}
	v := views[0]
	if v.Configured {
		t.Errorf("should be unconfigured")
	}
	if v.State.Label != "TRANSCRIBING" || v.Model != parakeet || v.JobsDone != 9 {
		t.Errorf("merged view wrong: %+v", v)
	}
}

func TestBuildServerViews_GPUReadiness(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	stale := 30 * time.Minute
	ip := func(n int) *int { return &n }
	bp := func(b bool) *bool { return &b }

	configured := []config.ASRServer{
		{Name: "ready-1", GPUArbiterURL: "http://x/ready"},
		{Name: "busy-1", GPUArbiterURL: "http://x/busy"},
		{Name: "offline-1", GPUArbiterURL: "http://x/offline"},
		{Name: "rundown-1", GPUArbiterURL: "http://x/rundown"},
	}
	probes := map[string]arbiterStatus{
		"ready-1":   {Reachable: true, State: "available", ASRRunning: bp(true), VRAMUsedMB: ip(512), VRAMTotalMB: ip(32607)},
		"busy-1":    {Reachable: true, State: "gaming", Claims: []string{"steam:440"}, ASRRunning: bp(false), VRAMUsedMB: ip(7338), VRAMTotalMB: ip(32607)},
		"offline-1": {Reachable: false},
		// reachable + available but the runner unit is down → not usable → BUSY
		"rundown-1": {Reachable: true, State: "available", ASRRunning: bp(false)},
	}

	byName := map[string]serverView{}
	for _, v := range buildServerViews(configured, &db.ServerObservation{}, probes, now, stale) {
		byName[v.Name] = v
	}

	if v := byName["ready-1"]; v.State.Label != "READY" || v.State.Token != "ready" || v.State.Dot != "green" {
		t.Errorf("ready-1 = %+v, want READY/green", v.State)
	}
	if v := byName["ready-1"]; !v.Probed || !v.Reachable || v.GPUState != "available" {
		t.Errorf("ready-1 probe fields wrong: %+v", v)
	}
	if v := byName["busy-1"]; v.State.Label != "BUSY" || v.State.Dot != "amber" {
		t.Errorf("busy-1 = %+v, want BUSY/amber", v.State)
	}
	if v := byName["busy-1"]; !strings.Contains(v.State.Sub, "game mode") || !strings.Contains(v.State.Sub, "steam:440") {
		t.Errorf("busy-1 sub = %q, want game mode + claim", v.State.Sub)
	}
	if v := byName["offline-1"]; v.State.Label != "OFFLINE" || v.State.Dot != "grey" {
		t.Errorf("offline-1 = %+v, want OFFLINE/grey", v.State)
	}
	if v := byName["rundown-1"]; v.State.Label != "BUSY" || !strings.Contains(v.State.Sub, "asr-runner stopped") {
		t.Errorf("rundown-1 = %+v, want BUSY (runner stopped)", v.State)
	}
}

func TestBuildServerViews_LiveClaimBeatsProbe(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	// A fresh live claim must win even if gpu-arbiter says gaming: if the runner
	// holds a job, it is plainly transcribing.
	configured := []config.ASRServer{{Name: "desktop-1", GPUArbiterURL: "http://x/busy"}}
	obs := &db.ServerObservation{
		LiveRunners: []db.LiveRunner{{ClaimedBy: "asr-runner-desktop-1", LastHeartbeat: now.Add(-10 * time.Second), CurrentFile: "/b/01.m4b"}},
	}
	probes := map[string]arbiterStatus{"desktop-1": {Reachable: true, State: "gaming"}}
	views := buildServerViews(configured, obs, probes, now, 30*time.Minute)
	if views[0].State.Label != "TRANSCRIBING" {
		t.Fatalf("want TRANSCRIBING (live claim beats probe), got %q", views[0].State.Label)
	}
}

func TestArbiterRawToStatus(t *testing.T) {
	used, total := 7338, 32607
	raw := arbiterRaw{
		State:  "gaming",
		Claims: []string{"steam:2215200"},
		Units: []struct {
			Unit    string `json:"unit"`
			Running bool   `json:"running"`
		}{
			{Unit: "ollama.service", Running: false},
			{Unit: "asr-runner.service", Running: false},
		},
		VRAMUsedMB:  &used,
		VRAMTotalMB: &total,
	}
	st := raw.toStatus()
	if !st.Reachable || st.State != "gaming" {
		t.Fatalf("unexpected: %+v", st)
	}
	if st.ASRRunning == nil || *st.ASRRunning {
		t.Errorf("ASRRunning should be non-nil false, got %v", st.ASRRunning)
	}
	if st.ready() {
		t.Errorf("gaming state must not be ready()")
	}
	// available + runner up → ready
	up := arbiterStatus{Reachable: true, State: "available", ASRRunning: boolPtrT(true)}
	if !up.ready() {
		t.Errorf("available + runner up should be ready()")
	}
}

func boolPtrT(b bool) *bool { return &b }
