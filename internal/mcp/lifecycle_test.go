package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
)

// statsOf is a helper to build a QueueStats value for use in table tests.
func statsOf(pending, claimed, done, total, evalDone, embed int, paused bool) *db.QueueStats {
	return &db.QueueStats{
		Pending:          pending,
		Claimed:          claimed,
		Done:             done,
		TotalJobs:        total,
		EvalCoverageDone: evalDone,
		EmbedBacklog:     embed,
		Paused:           paused,
	}
}

func TestComputePipelineLifecycle_Activity(t *testing.T) {
	tests := []struct {
		name         string
		stats        *db.QueueStats
		phase        string
		arbiter      arbiterStatus
		gpuProbed    bool
		evalEnabled  bool
		wantActivity pipelineActivity
	}{
		{
			name:         "paused takes precedence",
			stats:        statsOf(5, 1, 0, 6, 0, 0, true),
			phase:        db.PhaseTranscribe,
			wantActivity: activityPaused,
		},
		{
			name:         "claimed jobs → transcribing",
			stats:        statsOf(10, 3, 0, 13, 0, 0, false),
			phase:        db.PhaseTranscribe,
			wantActivity: activityTranscribing,
		},
		{
			name:         "analyze phase → evaluating",
			stats:        statsOf(0, 0, 50, 50, 30, 0, false),
			phase:        db.PhaseAnalyze,
			evalEnabled:  true,
			wantActivity: activityEvaluating,
		},
		{
			name:         "embed backlog → embedding",
			stats:        statsOf(0, 0, 50, 50, 50, 3, false),
			phase:        db.PhaseIdle,
			evalEnabled:  true,
			wantActivity: activityEmbedding,
		},
		{
			name:         "eval not complete → evaluating (winding-down path)",
			stats:        statsOf(0, 0, 317, 317, 280, 0, false),
			phase:        db.PhaseIdle,
			evalEnabled:  true,
			wantActivity: activityEvaluating,
		},
		{
			name:         "everything done → idle",
			stats:        statsOf(0, 0, 100, 100, 100, 0, false),
			phase:        db.PhaseIdle,
			evalEnabled:  true,
			wantActivity: activityIdle,
		},
		{
			name:         "eval not in pipeline, all else done → idle",
			stats:        statsOf(0, 0, 100, 100, 0, 0, false),
			phase:        db.PhaseIdle,
			evalEnabled:  false,
			wantActivity: activityIdle,
		},
		{
			name:         "nothing to do, no eval config → idle",
			stats:        statsOf(0, 0, 0, 0, 0, 0, false),
			phase:        db.PhaseIdle,
			evalEnabled:  false,
			wantActivity: activityIdle,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lc := computePipelineLifecycle(tc.stats, tc.phase, tc.arbiter, tc.gpuProbed, tc.evalEnabled)
			if lc.Activity != tc.wantActivity {
				t.Errorf("activity = %q, want %q (fullyDone=%v gpuCommitted=%v evalCoverage=%.2f)",
					lc.Activity, tc.wantActivity, lc.FullyDone, lc.GPUCommitted, lc.EvalCoverage)
			}
		})
	}
}

func TestComputePipelineLifecycle_FullyDone(t *testing.T) {
	tests := []struct {
		name        string
		stats       *db.QueueStats
		evalEnabled bool
		wantDone    bool
	}{
		{
			name:        "all done, eval complete",
			stats:       statsOf(0, 0, 100, 100, 100, 0, false),
			evalEnabled: true,
			wantDone:    true,
		},
		{
			name:        "all done, eval not in pipeline",
			stats:       statsOf(0, 0, 100, 100, 0, 0, false),
			evalEnabled: false,
			wantDone:    true,
		},
		{
			name:        "pending still present",
			stats:       statsOf(5, 0, 95, 100, 95, 0, false),
			evalEnabled: true,
			wantDone:    false,
		},
		{
			name:        "eval not complete",
			stats:       statsOf(0, 0, 100, 100, 80, 0, false),
			evalEnabled: true,
			wantDone:    false,
		},
		{
			name:        "embed backlog remaining",
			stats:       statsOf(0, 0, 100, 100, 100, 2, false),
			evalEnabled: true,
			wantDone:    false,
		},
		{
			name:        "empty queue, no eval → done",
			stats:       statsOf(0, 0, 0, 0, 0, 0, false),
			evalEnabled: false,
			wantDone:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lc := computePipelineLifecycle(tc.stats, db.PhaseIdle, arbiterStatus{}, false, tc.evalEnabled)
			if lc.FullyDone != tc.wantDone {
				t.Errorf("FullyDone = %v, want %v (evalCoverage=%.2f embedBacklog=%d)",
					lc.FullyDone, tc.wantDone, lc.EvalCoverage, lc.EmbedBacklog)
			}
		})
	}
}

func TestComputePipelineLifecycle_EvalCoverage(t *testing.T) {
	tests := []struct {
		name        string
		stats       *db.QueueStats
		evalEnabled bool
		wantPct     float64 // rough check — sentinel vs fraction
	}{
		{
			name:        "eval not enabled → sentinel -1",
			stats:       statsOf(0, 0, 100, 100, 0, 0, false),
			evalEnabled: false,
			wantPct:     -1,
		},
		{
			name:        "eval enabled, 280/317 done",
			stats:       statsOf(0, 0, 317, 317, 280, 0, false),
			evalEnabled: true,
			wantPct:     280.0 / 317.0,
		},
		{
			name:        "eval enabled, none done yet",
			stats:       statsOf(0, 0, 0, 0, 0, 0, false),
			evalEnabled: true,
			wantPct:     0,
		},
		{
			name:        "eval enabled, fully covered",
			stats:       statsOf(0, 0, 100, 100, 100, 0, false),
			evalEnabled: true,
			wantPct:     1.0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lc := computePipelineLifecycle(tc.stats, db.PhaseIdle, arbiterStatus{}, false, tc.evalEnabled)
			if lc.EvalCoverage != tc.wantPct {
				t.Errorf("EvalCoverage = %.4f, want %.4f", lc.EvalCoverage, tc.wantPct)
			}
		})
	}
}

func TestWindingDown(t *testing.T) {
	tests := []struct {
		activity pipelineActivity
		want     bool
	}{
		{activityWindingDown, true},
		{activityEvaluating, true},
		{activityTranscribing, false},
		{activityEmbedding, false},
		{activityIdle, false},
		{activityPaused, false},
	}
	for _, tc := range tests {
		lc := pipelineLifecycle{Activity: tc.activity}
		if got := lc.WindingDown(); got != tc.want {
			t.Errorf("WindingDown() for %q = %v, want %v", tc.activity, got, tc.want)
		}
	}
}

func TestEvalCoverageDisplay(t *testing.T) {
	tests := []struct {
		name        string
		lc          pipelineLifecycle
		done        int
		wantContain string
	}{
		{
			name:        "not in pipeline",
			lc:          pipelineLifecycle{EvalInPipeline: false},
			done:        100,
			wantContain: "not in pipeline",
		},
		{
			name:        "zero done",
			lc:          pipelineLifecycle{EvalInPipeline: true, EvalCoverage: 0},
			done:        0,
			wantContain: "0 / 0",
		},
		{
			name:        "partial coverage",
			lc:          pipelineLifecycle{EvalInPipeline: true, EvalCoverage: 280.0 / 317.0},
			done:        317,
			wantContain: "280",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.lc.EvalCoverageDisplay(tc.done)
			if !containsStr(got, tc.wantContain) {
				t.Errorf("EvalCoverageDisplay(%d) = %q, want it to contain %q", tc.done, got, tc.wantContain)
			}
		})
	}
}

func TestEvalCoveragePct(t *testing.T) {
	tests := []struct {
		name    string
		lc      pipelineLifecycle
		done    int
		wantPct int
	}{
		{
			name:    "not in pipeline → 0",
			lc:      pipelineLifecycle{EvalInPipeline: false},
			done:    100,
			wantPct: 0,
		},
		{
			name:    "sentinel -1 → 0",
			lc:      pipelineLifecycle{EvalInPipeline: true, EvalCoverage: -1},
			done:    100,
			wantPct: 0,
		},
		{
			name:    "88% coverage",
			lc:      pipelineLifecycle{EvalInPipeline: true, EvalCoverage: 0.883},
			done:    317,
			wantPct: 88,
		},
		{
			name:    "100% capped",
			lc:      pipelineLifecycle{EvalInPipeline: true, EvalCoverage: 1.0},
			done:    100,
			wantPct: 100,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.lc.EvalCoveragePct(tc.done)
			if got != tc.wantPct {
				t.Errorf("EvalCoveragePct(%d) = %d, want %d", tc.done, got, tc.wantPct)
			}
		})
	}
}

func TestEmbedReadyPct(t *testing.T) {
	tests := []struct {
		name    string
		lc      pipelineLifecycle
		wantPct int
	}{
		{
			name:    "no tracks → 0 (divide-by-zero guard)",
			lc:      pipelineLifecycle{},
			wantPct: 0,
		},
		{
			name: "raw embedded-ready share matches green segment",
			// total = NotStarted + TranscribedOnly + EvaldOnly + EmbeddedReady
			//       = 20 + 10 + 10 + 60 = 100 → 60%.
			lc:      pipelineLifecycle{NotStarted: 20, TranscribedOnly: 10, EvaldOnly: 10, EmbeddedReady: 60},
			wantPct: 60,
		},
		{
			name: "Transcribing is not double-counted (already in NotStarted)",
			// NotStarted already includes the 5 claimed/transcribing tracks, so the
			// denominator stays 100 → 60% (not diluted to ~57%).
			lc:      pipelineLifecycle{NotStarted: 20, Transcribing: 5, TranscribedOnly: 10, EvaldOnly: 10, EmbeddedReady: 60},
			wantPct: 60,
		},
		{
			name:    "truncating division",
			lc:      pipelineLifecycle{NotStarted: 2, EmbeddedReady: 1},
			wantPct: 33,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lc.EmbedReadyPct(); got != tc.wantPct {
				t.Errorf("EmbedReadyPct() = %d, want %d", got, tc.wantPct)
			}
		})
	}
}

func TestToStatusAllUnits(t *testing.T) {
	// toStatus must collect ALL units — not stop after the first ASR unit —
	// so the dashboard can list every resident model.
	raw := arbiterRaw{
		State: "available",
		Units: []struct {
			Unit    string `json:"unit"`
			Running bool   `json:"running"`
		}{
			{Unit: "asr-runner.service", Running: true},
			{Unit: "ollama.service", Running: true},
			{Unit: "idle-watcher.service", Running: false},
		},
		VRAMUsedMB:  ip(29696),
		VRAMTotalMB: ip(32607),
	}
	st := raw.toStatus()
	if len(st.ResidentUnits) != 3 {
		t.Errorf("ResidentUnits length = %d, want 3 (got %v)", len(st.ResidentUnits), st.ResidentUnits)
	}
	// ASRRunning must be set (from the asr-runner unit).
	if st.ASRRunning == nil || !*st.ASRRunning {
		t.Errorf("ASRRunning should be true, got %v", st.ASRRunning)
	}
	// VRAM should pass through.
	if st.VRAMUsedMB == nil || *st.VRAMUsedMB != 29696 {
		t.Errorf("VRAMUsedMB = %v, want 29696", st.VRAMUsedMB)
	}
}

func TestComputePipelineLifecycle_NilStats(t *testing.T) {
	// Nil stats must not panic; should produce an idle zero-value lifecycle.
	lc := computePipelineLifecycle(nil, db.PhaseIdle, arbiterStatus{}, false, false)
	if lc.Activity != activityIdle {
		t.Errorf("nil stats: activity = %q, want idle", lc.Activity)
	}
	if !lc.FullyDone {
		t.Errorf("nil stats: FullyDone should be true (nothing to do)")
	}
}

// TestDemoLifecycleScenariosRender drives the full /status/data fragment through
// the demo DB + demo GPU prober for the two pipeline-lifecycle scenarios, so the
// "winding down — GPU still working" and "idle · models resident" states stay
// renderable (CLAUDE.md requires every UI path be visually checkable via demo).
func TestDemoLifecycleScenariosRender(t *testing.T) {
	tests := []struct {
		scenario     string
		wantContains []string
		wantAbsent   []string
	}{
		{
			scenario: "winddown",
			wantContains: []string{
				"WINDING DOWN", // relabeled state line
				"Winding down — GPU still working (eval)", // honest subtext
				"active on GPU",   // marker under the Eval stage
				"Models Resident", // GPU resident card
			},
			// Must NOT claim the runner is transcribing — transcribe has drained.
			wantAbsent: []string{"is transcribing"},
		},
		{
			scenario: "idle",
			wantContains: []string{
				"IDLE",
				"safe to walk away",
				"models resident", // the "why is 29 GB occupied while idle" answer
				"Models Resident",
			},
			wantAbsent: []string{"Winding down"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.scenario, func(t *testing.T) {
			cfg := &config.Config{
				ASRServers:     demoServersFor(tc.scenario),
				AIEndpoints:    demoAIEndpoints,
				AIRoles:        demoAIRoles,
				EvalInPipeline: true,
			}
			srv := NewMCPServer(demoDB{scenario: tc.scenario, paused: new(bool)}, cfg)
			srv.prober = demoGPUProber{scenario: tc.scenario}
			srv.endpointProber = demoEndpointProber{}
			h := srv.buildMux()

			req := httptest.NewRequest(http.MethodGet, "/status/data", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			out := w.Body.String()
			for _, want := range tc.wantContains {
				if !containsStr(out, want) {
					t.Errorf("fragment missing %q", want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if containsStr(out, absent) {
					t.Errorf("fragment unexpectedly contains %q", absent)
				}
			}
		})
	}
}

// containsStr is a simple substring helper (avoids importing strings in tests).
func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}
