package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/asr"
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
		{Name: "gpu-1", Host: "gpu-1", Model: parakeet, Role: "primary"},
		{Name: "gpu-2", Host: "gpu-2", Model: parakeet, Role: "fallback"},
		{Name: "gpu-3", Model: parakeet, Role: "fallback"}, // never seen
	}
	obs := &db.ServerObservation{
		LiveRunners: []db.LiveRunner{
			{ClaimedBy: "asr-runner-gpu-1", ClaimedCount: 1, LastHeartbeat: now.Add(-20 * time.Second),
				CurrentFile: "/books/Author/Book/01 - Chapter 1.mp3"},
		},
		Hosts: []db.HostMetrics{
			{Host: "gpu-1", ASRModel: &parakeet, ComputeType: &bf16, JobsDone: 300, LastFinished: tp(-3 * time.Minute), AvgProcessingSeconds: fp(487.5)},
			{Host: "gpu-2", ASRModel: &parakeet, JobsDone: 12, LastFinished: tp(-6 * time.Hour)},
			{Host: "gpu-9", JobsDone: 5, LastFinished: tp(-48 * time.Hour)}, // unconfigured
		},
	}

	views := buildServerViews(configured, obs, nil, now, stale)
	byName := map[string]serverView{}
	for _, v := range views {
		byName[v.Name] = v
	}

	// gpu-1: fresh live claim → TRANSCRIBING, observed model + size, file in sub.
	d := byName["gpu-1"]
	if d.State.Label != "TRANSCRIBING" {
		t.Errorf("gpu-1 state = %q, want TRANSCRIBING", d.State.Label)
	}
	if !strings.Contains(d.State.Sub, "01 - Chapter 1.mp3") {
		t.Errorf("gpu-1 sub = %q, want it to name the in-flight file", d.State.Sub)
	}
	if d.Model != parakeet || d.ModelSource != "observed" || d.ModelSize != "0.6B" || d.ComputeMode != "bfloat16" {
		t.Errorf("gpu-1 model fields wrong: %+v", d)
	}
	if !d.Configured || d.Role != "primary" {
		t.Errorf("gpu-1 should be configured primary: %+v", d)
	}

	// gpu-2: history but no live claim → IDLE.
	if l := byName["gpu-2"]; l.State.Label != "IDLE" || !strings.Contains(l.State.Sub, "last active") {
		t.Errorf("gpu-2 state = %q sub = %q, want IDLE/last active", l.State.Label, l.State.Sub)
	}

	// gpu-3: configured, never observed → NOT SEEN, model falls back to configured.
	p := byName["gpu-3"]
	if p.State.Label != "NOT SEEN" {
		t.Errorf("gpu-3 state = %q, want NOT SEEN", p.State.Label)
	}
	if p.Model != parakeet || p.ModelSource != "configured" {
		t.Errorf("gpu-3 model should be configured fallback: %+v", p)
	}

	// gpu-9: observed but unconfigured → present, Configured=false.
	m, ok := byName["gpu-9"]
	if !ok {
		t.Fatal("unconfigured host gpu-9 should still be rendered")
	}
	if m.Configured {
		t.Errorf("gpu-9 should be Configured=false")
	}
	if m.State.Label != "IDLE" {
		t.Errorf("gpu-9 state = %q, want IDLE", m.State.Label)
	}
}

func TestBuildServerViews_StaleClaim(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	stale := 30 * time.Minute
	configured := []config.ASRServer{{Name: "gpu-1", Role: "primary"}}
	obs := &db.ServerObservation{
		LiveRunners: []db.LiveRunner{
			{ClaimedBy: "asr-runner-gpu-1", ClaimedCount: 1, LastHeartbeat: now.Add(-2 * time.Hour)},
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
			{ClaimedBy: "asr-runner-gpu-1", ClaimedCount: 1, LastHeartbeat: now.Add(-10 * time.Second)},
		},
		Hosts: []db.HostMetrics{
			{Host: "gpu-1", ASRModel: &parakeet, JobsDone: 9, LastFinished: tp(-time.Minute)},
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
	configured := []config.ASRServer{{Name: "gpu-1", GPUArbiterURL: "http://x/busy"}}
	obs := &db.ServerObservation{
		LiveRunners: []db.LiveRunner{{ClaimedBy: "asr-runner-gpu-1", LastHeartbeat: now.Add(-10 * time.Second), CurrentFile: "/b/01.m4b"}},
	}
	probes := map[string]arbiterStatus{"gpu-1": {Reachable: true, State: "gaming"}}
	views := buildServerViews(configured, obs, probes, now, 30*time.Minute)
	if views[0].State.Label != "TRANSCRIBING" {
		t.Fatalf("want TRANSCRIBING (live claim beats probe), got %q", views[0].State.Label)
	}
}

// capByKey indexes a server's badge strip by capability key for assertions.
func capByKey(v serverView) map[string]capBadge {
	m := map[string]capBadge{}
	for _, b := range v.Caps {
		m[b.Key] = b
	}
	return m
}

// TestBuildServerViews_BackendDescriptor covers the Phase-2 resolution: family/
// runtime/caps observed-wins-over-configured, the config-only "expected" path,
// the unknown (neither) path, and skipped-reason carry-through on a declined cap.
func TestBuildServerViews_BackendDescriptor(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	stale := 30 * time.Minute
	tp := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	fp := func(v float64) *float64 { return &v }
	sp := func(s string) *string { return &s }

	parakeet := "nvidia/parakeet-tdt-0.6b-v3"
	whisper := "whisper-large-v3"

	configured := []config.ASRServer{
		// gpu-1: config declares Parakeet/CUDA with context_biasing=true, but the
		// observed run reports a DIFFERENT family/runtime and DECLINES biasing with
		// a reason — observed must win and the reason must surface.
		{Name: "gpu-1", Host: "gpu-1", Role: "primary",
			Family: "config-parakeet", Runtime: "config-cuda", Model: parakeet,
			Capabilities: asr.Capabilities{asr.CapContextBiasing: true, asr.CapWordTimestamps: true}},
		// mac-1: config-only descriptor (no observed run) → "configured" source.
		{Name: "mac-1", Host: "mac-1", Role: "fallback",
			Family: asr.FamilyWhisper, Runtime: asr.RuntimeWhisperCPP, Model: whisper,
			Capabilities: asr.Capabilities{asr.CapWordTimestamps: true, asr.CapContextBiasing: false}},
		// bare-1: no descriptor anywhere → unknown/blank, must still render cleanly.
		{Name: "bare-1", Host: "bare-1", Role: "fallback", Model: parakeet},
	}
	obs := &db.ServerObservation{
		Hosts: []db.HostMetrics{
			{Host: "gpu-1", ASRModel: &parakeet, JobsDone: 100, LastFinished: tp(-time.Minute),
				AvgProcessingSeconds: fp(98),
				ASRFamily:            sp(asr.FamilyNeMoParakeet), ASRRuntime: sp(asr.RuntimeNeMoCUDA),
				CapsApplied: asr.Capabilities{
					asr.CapWordTimestamps: true,
					asr.CapContextBiasing: false,
				},
				CapsSkippedReason:  map[string]string{"context_biasing": "parakeet-tdt timestamps break under boosting"},
				MeanWordConfidence: fp(0.91)},
			// mac-1 has NO observed run; only the configured descriptor applies.
			{Host: "bare-1", ASRModel: &parakeet, JobsDone: 3, LastFinished: tp(-2 * time.Hour)},
		},
	}

	byName := map[string]serverView{}
	for _, v := range buildServerViews(configured, obs, nil, now, stale) {
		byName[v.Name] = v
	}

	// gpu-1: observed family/runtime win over config; known canonical ids; mean conf.
	g := byName["gpu-1"]
	if g.Family != asr.FamilyNeMoParakeet || g.FamilySource != "observed" || !g.FamilyKnown {
		t.Errorf("gpu-1 family: got %q/%q known=%v, want observed nemo-parakeet/known", g.Family, g.FamilySource, g.FamilyKnown)
	}
	if g.Runtime != asr.RuntimeNeMoCUDA || g.RuntimeSource != "observed" || !g.RuntimeKnown {
		t.Errorf("gpu-1 runtime: got %q/%q known=%v, want observed nemo-cuda/known", g.Runtime, g.RuntimeSource, g.RuntimeKnown)
	}
	if g.CapsSource != "observed" {
		t.Errorf("gpu-1 caps source = %q, want observed", g.CapsSource)
	}
	if g.MeanConfidence == nil || *g.MeanConfidence != 0.91 {
		t.Errorf("gpu-1 mean conf = %v, want 0.91", g.MeanConfidence)
	}
	gc := capByKey(g)
	if b := gc["word_timestamps"]; !b.Applied || b.Label != "words" {
		t.Errorf("gpu-1 words badge = %+v, want applied/words", b)
	}
	if b := gc["context_biasing"]; b.Applied || b.Reason == "" || !strings.Contains(b.Reason, "timestamps break") {
		t.Errorf("gpu-1 bias badge = %+v, want declined with reason", b)
	}

	// mac-1: config-only → "configured" source for family/runtime/caps; no reason.
	m := byName["mac-1"]
	if m.Family != asr.FamilyWhisper || m.FamilySource != "configured" {
		t.Errorf("mac-1 family: got %q/%q, want configured whisper", m.Family, m.FamilySource)
	}
	if m.Runtime != asr.RuntimeWhisperCPP || m.RuntimeSource != "configured" {
		t.Errorf("mac-1 runtime: got %q/%q, want configured whisper.cpp", m.Runtime, m.RuntimeSource)
	}
	if m.CapsSource != "configured" {
		t.Errorf("mac-1 caps source = %q, want configured", m.CapsSource)
	}
	if b := capByKey(m)["context_biasing"]; b.Applied || b.Reason != "" {
		t.Errorf("mac-1 bias badge = %+v, want declined with NO reason (config has no run)", b)
	}
	if m.MeanConfidence != nil {
		t.Errorf("mac-1 mean conf = %v, want nil (no observed run)", m.MeanConfidence)
	}

	// bare-1: no descriptor anywhere → all blank, renders cleanly (back-compat).
	b := byName["bare-1"]
	if b.Family != "" || b.Runtime != "" || len(b.Caps) != 0 || b.CapsSource != "" {
		t.Errorf("bare-1 should have no descriptor data, got %+v", b)
	}
	if b.FamilyKnown || b.RuntimeKnown {
		t.Errorf("bare-1 known flags should be false")
	}
}

// TestBuildCapBadges checks strip ordering, applied/declined states, reason
// attachment only on declined caps, and the empty input → nil contract.
func TestBuildCapBadges(t *testing.T) {
	if got := buildCapBadges(nil, nil); got != nil {
		t.Errorf("nil caps → nil, got %+v", got)
	}
	caps := asr.Capabilities{
		asr.CapContextBiasing: false,
		asr.CapWordTimestamps: true, // out of map order; strip order must be stable
	}
	reasons := map[string]string{"context_biasing": "declined here"}
	badges := buildCapBadges(caps, reasons)
	if len(badges) != 2 {
		t.Fatalf("want 2 badges, got %d: %+v", len(badges), badges)
	}
	// word_timestamps sorts before context_biasing in the strip order.
	if badges[0].Key != "word_timestamps" || !badges[0].Applied {
		t.Errorf("badge[0] = %+v, want applied word_timestamps first", badges[0])
	}
	if badges[1].Key != "context_biasing" || badges[1].Applied || badges[1].Reason != "declined here" {
		t.Errorf("badge[1] = %+v, want declined context_biasing with reason", badges[1])
	}
	// An applied cap must NOT carry a reason even if one is supplied.
	applied := buildCapBadges(asr.Capabilities{asr.CapWordTimestamps: true},
		map[string]string{"word_timestamps": "should be ignored"})
	if applied[0].Reason != "" {
		t.Errorf("applied cap should carry no reason, got %q", applied[0].Reason)
	}
}

// TestGroupByFamily checks first-seen-order bucketing, the trailing unknown
// bucket, and the multiFamily toggle (≤1 family → flat render).
func TestGroupByFamily(t *testing.T) {
	views := []serverView{
		{Name: "a", Family: asr.FamilyNeMoParakeet, FamilyKnown: true},
		{Name: "b", Family: asr.FamilyWhisper},
		{Name: "c", Family: asr.FamilyNeMoParakeet, FamilyKnown: true}, // joins a's group
		{Name: "d"}, // no family → unknown bucket
	}
	groups := groupByFamily(views)
	if len(groups) != 3 {
		t.Fatalf("want 3 groups (parakeet, whisper, unknown), got %d: %+v", len(groups), groups)
	}
	if groups[0].Family != asr.FamilyNeMoParakeet || len(groups[0].Servers) != 2 || !groups[0].Known {
		t.Errorf("group[0] wrong: %+v", groups[0])
	}
	if groups[2].Family != "" || groups[2].Label != "unknown" {
		t.Errorf("group[2] should be the unknown bucket: %+v", groups[2])
	}
	if !multiFamily(groups) {
		t.Errorf("3 families → multiFamily true")
	}
	// Single family → flat render.
	single := groupByFamily([]serverView{{Name: "x", Family: asr.FamilyWhisper}})
	if multiFamily(single) {
		t.Errorf("one family → multiFamily false")
	}
}

func TestArbiterRawToStatus(t *testing.T) {
	used, total := 7338, 32607
	raw := arbiterRaw{
		State:  "gaming",
		Claims: []string{"steam:413150"},
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
