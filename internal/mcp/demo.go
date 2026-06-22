package mcp

import (
	"cmp"
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/asr"
	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/predict"
)

// doneRatio is the fraction of a book's tracks that are done (0 when no tracks),
// used by the demo to mirror the real transcribed-first ORDER BY.
func doneRatio(b db.BookSummary) float64 {
	if b.Total == 0 {
		return 0
	}
	return float64(b.Done) / float64(b.Total)
}

// demoDB is an in-memory DBInterface implementation that serves synthetic data
// for the status dashboard. It lets the dashboard render with no Postgres,
// which is what makes local UI work and AI-agent visual verification cheap
// (see CLAUDE.md "Visual Verification"). It is never wired into production
// code paths — only `earmark mcp --demo` constructs it.
//
// scenario selects which state to render so every UI path is visually testable:
//
//	active (default) — live runner, healthy backlog, a couple of failures
//	empty            — fresh install: zero counts, runner never seen
//	stale            — runner heartbeat hours old with work waiting → STALLED
//	failed           — failures including a long multi-line error string
//	multibackend     — three ASR families (Parakeet/Whisper/Canary) on three
//	                   hosts so the Servers page's Family/Runtime columns,
//	                   capability badges, and skipped-reason tooltips render
type demoDB struct {
	scenario string
	paused   *bool // heap-backed so value-receiver SetPaused can mutate it
	runLimit *int  // bounded-run counter for the control API (nil = unlimited)
}

func (demoDB) Ping(context.Context) error { return nil }

func (demoDB) Search(context.Context, string, int, float64) ([]db.SearchResultWithMetadata, error) {
	return nil, nil
}

func (demoDB) TextSearch(context.Context, string, int) ([]db.SearchResultWithMetadata, error) {
	return nil, nil
}

// TextSearchInBook returns synthetic per-book search hits so the book-detail
// search box is exercisable with no database: two matching chunk rows within
// the given dir (timestamps inline). An empty query yields nothing.
func (demoDB) TextSearchInBook(_ context.Context, dir, query string, _ int) ([]db.SearchResultWithMetadata, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	return []db.SearchResultWithMetadata{
		{ID: "s1", FilePath: dir + "/01.m4b", ChunkIndex: 3, StartSec: 182.4, EndSec: 271.0,
			Content: "…and there, in the " + query + ", everything changed at once."},
		{ID: "s2", FilePath: dir + "/04.m4b", ChunkIndex: 12, StartSec: 1820.0, EndSec: 1905.5,
			Content: "She returned to the " + query + " one final time before dawn."},
	}, nil
}

// SearchInBook returns synthetic book-scoped semantic hits so the scoped
// semantic_search tool is exercisable with no database (mirrors TextSearchInBook).
func (demoDB) SearchInBook(_ context.Context, query, dir string, _ int, _ float64) ([]db.SearchResultWithMetadata, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	return []db.SearchResultWithMetadata{
		{ID: "v1", FilePath: dir + "/01.m4b", ChunkIndex: 5, StartSec: 305.0, EndSec: 372.5,
			Similarity: 0.82, Content: "A passage closely related to " + query + " in meaning."},
		{ID: "v2", FilePath: dir + "/02.m4b", ChunkIndex: 9, StartSec: 940.0, EndSec: 1012.0,
			Similarity: 0.74, Content: "Another semantically similar moment about " + query + "."},
	}, nil
}

func (demoDB) GetChunkContext(context.Context, string, int) ([]db.SearchResultWithMetadata, error) {
	return nil, nil
}

// Requeue actions are no-ops in demo mode (no real DB) but succeed so the
// dashboard buttons are exercisable; the fixture data is unchanged on refresh.
func (demoDB) RequeueByID(_ context.Context, id string) (string, error) { return id, nil }

func (demoDB) RequeueFailed(context.Context) ([]string, error) { return []string{"demo"}, nil }

func (demoDB) RequeueByDir(_ context.Context, dir string) ([]string, error) {
	return []string{dir}, nil
}

// GetFailedJobs returns synthetic failed jobs (with full errors, attempts, and
// runner) for the failures view. Empty/healthy scenarios have none.
func (d demoDB) GetFailedJobs(context.Context) ([]db.FailedJob, error) {
	if d.scenario == "empty" || d.scenario == "stale" {
		return nil, nil
	}
	now := time.Now()
	cudaErr := "Traceback (most recent call last):\n" +
		"  File \"runner.py\", line 412, in transcribe\n" +
		"    result = model.transcribe(audio_path)\n" +
		"RuntimeError: CUDA out of memory. Tried to allocate 2.40 GiB " +
		"(GPU 0; 31.49 GiB total; 28.12 GiB allocated; 1.05 GiB free)"
	codecErr := "ffmpeg: unsupported codec in chapter 3; file skipped"
	runner := "asr-runner-gpu-1"
	return []db.FailedJob{
		{ID: "f1", FilePath: "/books/audio-libation/Author Seven/An Epic/An Epic.m4b",
			Error: &cudaErr, Attempts: 3, ClaimedBy: &runner, UpdatedAt: now.Add(-15 * time.Second)},
		{ID: "f2", FilePath: "/books/audio-libro/Some Author/Short Stories - Track 3.mp3",
			Error: &codecErr, Attempts: 1, ClaimedBy: &runner, UpdatedAt: now.Add(-9 * time.Minute)},
	}, nil
}

// demoASRServers is the synthetic ASR_SERVERS registry for the demo: a primary
// (gpu-1, GPU ready), a fallback (gpu-2, GPU busy/gaming), and an offline
// one (gpu-3), so the Servers page shows every gpu-arbiter readiness state. The
// gpuArbiterUrl values are demo sentinels routed by demoGPUProber, not real
// endpoints (the demo swaps in a static prober, so no network call is made).
var demoASRServers = []config.ASRServer{
	{Name: "gpu-1", Host: "gpu-1", Model: "nvidia/parakeet-tdt-0.6b-v3", Role: "primary", GPUArbiterURL: "http://demo/gpu-1/ready"},
	{Name: "gpu-2", Host: "gpu-2", Model: "nvidia/parakeet-tdt-0.6b-v3", Role: "fallback", GPUArbiterURL: "http://demo/gpu-2/busy"},
	{Name: "gpu-3", Host: "gpu-3", Model: "nvidia/parakeet-tdt-0.6b-v3", Role: "fallback", GPUArbiterURL: "http://demo/gpu-3/offline"},
}

// demoMultibackendServers is the ASR_SERVERS registry for DEMO_SCENARIO=multibackend:
// three different families on three hosts so the Family/Runtime columns and the
// capability badges render with no DB. Generic placeholder names only (gpu-1 /
// mac-1 / cpu-1) per the shareable-repo rule. The configured capabilities here are
// the *declared* expectation; GetServerObservation reports the *applied* truth
// (observed wins), which is what makes the requested-vs-applied story legible.
var demoMultibackendServers = []config.ASRServer{
	// Parakeet primary: declares word timestamps but context_biasing false (TDT
	// timestamps break under boosting) — the canonical honest-degradation case.
	{Name: "gpu-1", Host: "gpu-1", Role: "primary",
		Family: asr.FamilyNeMoParakeet, Runtime: asr.RuntimeNeMoCUDA,
		Model: "nvidia/parakeet-tdt-0.6b-v3",
		Capabilities: asr.Capabilities{
			asr.CapWordTimestamps: true,
			asr.CapContextBiasing: false,
			asr.CapDiarization:    false,
		}},
	// Whisper fallback: word timestamps, no biasing, no diarization.
	{Name: "mac-1", Host: "mac-1", Role: "fallback",
		Family: asr.FamilyWhisper, Runtime: asr.RuntimeWhisperCPP,
		Model: "whisper-large-v3",
		Capabilities: asr.Capabilities{
			asr.CapWordTimestamps: true,
			asr.CapContextBiasing: false,
			asr.CapDiarization:    false,
		}},
	// Canary server: AED model that *does* apply context biasing.
	{Name: "cpu-1", Host: "cpu-1", Role: "fallback",
		Family: asr.FamilyNeMoCanary, Runtime: asr.RuntimeNeMoCUDA,
		Model: "nvidia/canary-1b",
		Capabilities: asr.Capabilities{
			asr.CapWordTimestamps:   true,
			asr.CapContextBiasing:   true,
			asr.CapConfidenceScores: true,
		}},
}

// demoServersFor returns the ASR_SERVERS registry for a scenario: the
// multibackend scenario uses the three-family set (no gpu-arbiter probes — the
// story there is families/caps, not readiness); every other scenario uses the
// single-family readiness set.
func demoServersFor(scenario string) []config.ASRServer {
	if scenario == "multibackend" {
		return demoMultibackendServers
	}
	return demoASRServers
}

// demoAIEndpoints is the synthetic AI endpoint registry for the demo: an
// embeddings endpoint (Ollama, bound to the embeddings role, ready) and a chat
// endpoint (vLLM, bound to eval, offline) so the Models/Services page shows both
// types, the role badge, options, and two health states. baseURLs are generic
// placeholders only (per the shareable-repo rule); the demo swaps in a static
// prober (demoEndpointProber) keyed by a sentinel in the URL, so no network call
// is made.
var demoAIEndpoints = []config.AIEndpoint{
	{ID: "embed-1", Type: config.AIEndpointTypeEmbeddings, Backend: config.AIBackendOllama,
		BaseURL: "http://ollama.example.com:11434/v1", Model: "nomic-embed-text"},
	{ID: "eval-1", Type: config.AIEndpointTypeChat, Backend: config.AIBackendVLLM,
		BaseURL: "http://vllm.example.com:8000/v1", Model: "Qwen2.5-7B-Instruct",
		Options: map[string]string{"temperature": "0", "max_tokens": "256"}},
}

var demoAIRoles = &config.AIRoles{Embeddings: "embed-1", Eval: "eval-1"}

// demoEndpointProber is a static endpointProber for the demo: the embeddings
// endpoint is ready, the eval (chat) endpoint is offline, routed by a substring
// of the base URL so the page renders both states with no network call.
type demoEndpointProber struct{}

func (demoEndpointProber) Probe(_ context.Context, baseURL, _ string) endpointProbe {
	if strings.Contains(baseURL, "vllm") {
		return endpointProbe{Probed: true, State: epStateOffline}
	}
	return endpointProbe{Probed: true, State: epStateReady}
}

// demoGPUProber is a static gpuProber for the demo, routing by a sentinel in the
// URL so the page renders ready / busy / offline without any network call.
type demoGPUProber struct{}

func (demoGPUProber) Probe(_ context.Context, url string) arbiterStatus {
	ip := func(n int) *int { return &n }
	bp := func(b bool) *bool { return &b }
	switch {
	case strings.Contains(url, "busy"):
		return arbiterStatus{Reachable: true, State: "gaming", Claims: []string{"steam:413150"},
			ASRRunning: bp(false), VRAMUsedMB: ip(7338), VRAMTotalMB: ip(32607)}
	case strings.Contains(url, "offline"):
		return arbiterStatus{Reachable: false}
	default: // ready
		return arbiterStatus{Reachable: true, State: "available",
			ASRRunning: bp(true), VRAMUsedMB: ip(512), VRAMTotalMB: ip(32607)}
	}
}

// GetServerObservation returns synthetic runner activity for the Servers page.
// active/failed: gpu-1 transcribing, gpu-2 idle (fallback), plus an
// unconfigured host (gpu-9, a model with no size token). stale: gpu-1's
// claim heartbeat is hours old → STALLED. empty: nothing observed, so the
// configured servers render as NOT SEEN.
func (d demoDB) GetServerObservation(context.Context) (*db.ServerObservation, error) {
	now := time.Now()
	parakeet := "nvidia/parakeet-tdt-0.6b-v3"
	bf16, f16, int8 := "bfloat16", "float16", "int8"
	whisper := "whisper-large-v3"
	fp := func(v float64) *float64 { return &v }
	tp := func(t time.Time) *time.Time { return &t }
	sp := func(s string) *string { return &s }

	switch d.scenario {
	case "empty":
		return &db.ServerObservation{}, nil
	case "multibackend":
		// Three families A/B'd on the same library. Observed caps_applied is the
		// ground truth (wins over the configured declaration): gpu-1 (Parakeet)
		// *declined* context_biasing with a reason (the honest-degradation surface);
		// mac-1 (Whisper) applied words only; cpu-1 (Canary) applied biasing and
		// reports a mean word confidence (Parakeet TDT emits none → NULL).
		canary := "nvidia/canary-1b"
		nemoFam, whisperFam, canaryFam := asr.FamilyNeMoParakeet, asr.FamilyWhisper, asr.FamilyNeMoCanary
		nemoRt, whisperRt := asr.RuntimeNeMoCUDA, asr.RuntimeWhisperCPP
		conf := 0.93
		return &db.ServerObservation{
			LiveRunners: []db.LiveRunner{
				{ClaimedBy: "asr-runner-gpu-1", ClaimedCount: 1, LastHeartbeat: now.Add(-9 * time.Second),
					CurrentFile: "/books/Author One/A Long Title/01.m4b"},
			},
			Hosts: []db.HostMetrics{
				{Host: "gpu-1", ASRModel: &parakeet, ComputeType: &bf16, JobsDone: 412,
					LastFinished: tp(now.Add(-9 * time.Second)), AvgProcessingSeconds: fp(98.0),
					ASRFamily: &nemoFam, ASRRuntime: &nemoRt,
					CapsApplied: asr.Capabilities{
						asr.CapWordTimestamps: true,
						asr.CapContextBiasing: false,
						asr.CapDiarization:    false,
					},
					CapsSkippedReason: map[string]string{
						"context_biasing": "parakeet-tdt timestamps break under boosting",
					}},
				{Host: "mac-1", ASRModel: &whisper, ComputeType: &int8, JobsDone: 57,
					LastFinished: tp(now.Add(-2 * time.Hour)), AvgProcessingSeconds: fp(1840.0),
					ASRFamily: &whisperFam, ASRRuntime: &whisperRt,
					CapsApplied: asr.Capabilities{
						asr.CapWordTimestamps: true,
						asr.CapContextBiasing: false,
						asr.CapDiarization:    false,
					}},
				{Host: "cpu-1", ASRModel: &canary, ComputeType: &f16, JobsDone: 31,
					LastFinished: tp(now.Add(-40 * time.Minute)), AvgProcessingSeconds: fp(2710.0),
					ASRFamily: &canaryFam, ASRRuntime: &nemoRt,
					CapsApplied: asr.Capabilities{
						asr.CapWordTimestamps:   true,
						asr.CapContextBiasing:   true,
						asr.CapConfidenceScores: true,
					},
					MeanWordConfidence: &conf},
			},
		}, nil
	case "stale":
		return &db.ServerObservation{
			LiveRunners: []db.LiveRunner{
				{ClaimedBy: "asr-runner-gpu-1", ClaimedCount: 1, LastHeartbeat: now.Add(-2 * time.Hour),
					CurrentFile: "/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]/01 - Chapter 1.mp3"},
			},
			Hosts: []db.HostMetrics{
				{Host: "gpu-1", ASRModel: &parakeet, ComputeType: &bf16, JobsDone: 120,
					LastFinished: tp(now.Add(-2 * time.Hour)), AvgProcessingSeconds: fp(498.0)},
			},
		}, nil
	default: // active / failed
		return &db.ServerObservation{
			LiveRunners: []db.LiveRunner{
				{ClaimedBy: "asr-runner-gpu-1", ClaimedCount: 1, LastHeartbeat: now.Add(-12 * time.Second),
					CurrentFile: "/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]/01 - Chapter 1.mp3"},
			},
			Hosts: []db.HostMetrics{
				{Host: "gpu-1", ASRModel: &parakeet, ComputeType: &bf16, JobsDone: 311,
					LastFinished: tp(now.Add(-3 * time.Minute)), AvgProcessingSeconds: fp(487.5)},
				{Host: "gpu-2", ASRModel: &parakeet, ComputeType: &f16, JobsDone: 24,
					LastFinished: tp(now.Add(-6 * time.Hour)), AvgProcessingSeconds: fp(902.0)},
				// Unconfigured host: not in demoASRServers; whisper-large-v3 has no
				// param-size token, so the Size column shows an em dash.
				{Host: "gpu-9", ASRModel: &whisper, ComputeType: sp(int8), JobsDone: 5,
					LastFinished: tp(now.Add(-48 * time.Hour)), AvgProcessingSeconds: fp(1840.0)},
			},
		}, nil
	}
}

// GetFindingsSummary returns a synthetic eval-layer findings rollup for the
// /findings page so it renders with no database (CONTRACT §2.15). Empty
// scenarios show the fresh-install zero state; everything else shows a
// representative spread of issue types and confidence buckets.
func (d demoDB) GetFindingsSummary(context.Context) (*db.FindingsSummary, error) {
	if d.scenario == "empty" {
		return &db.FindingsSummary{}, nil
	}
	mean := 0.61
	return &db.FindingsSummary{
		TotalFindings:    37,
		MeanConfidence:   &mean,
		HighConfidence:   11,
		MediumConfidence: 18,
		LowConfidence:    8,
		ByIssueType: []db.IssueTypeCount{
			{IssueType: "misheard_proper_noun", Count: 15},
			{IssueType: "number_artifact", Count: 9},
			{IssueType: "homophone", Count: 7},
			{IssueType: "misheard_word", Count: 4},
			{IssueType: "repeated_text", Count: 2},
		},
		ByBook: []db.BookFindings{
			{BookDir: "/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]",
				FilePath: "/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]/Project Hail Mary.m4b",
				Count:    21, MeanConfidence: 0.66, TopIssueType: "misheard_proper_noun"},
			{BookDir: "/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]",
				FilePath: "/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]/01 - Chapter 1.mp3",
				Count:    16, MeanConfidence: 0.54, TopIssueType: "number_artifact"},
		},
	}, nil
}

// GetEvalChunksForBook / SampleEvalChunks return synthetic chunks so the
// "run eval" dashboard actions are exercisable with no database. The demo judge
// (a static fake chat client wired in StartDemoDashboard) turns these into
// findings; here we just hand back a couple of plausible chunks.
func (demoDB) GetEvalChunksForBook(_ context.Context, dir string, limit int) ([]db.EvalChunk, error) {
	chunks := demoEvalChunks(dir)
	if limit > 0 && len(chunks) > limit {
		chunks = chunks[:limit]
	}
	return chunks, nil
}

func (demoDB) SampleEvalChunks(_ context.Context, limit int) ([]db.EvalChunk, error) {
	chunks := demoEvalChunks("/books/Author One/A Long Title")
	if limit > 0 && len(chunks) > limit {
		chunks = chunks[:limit]
	}
	return chunks, nil
}

// InsertFindings is a no-op in the demo: the synthetic GetFindingsSummary is
// fixed, so a demo eval run reports "evaluated N chunks" without changing the
// rollup. The button + async indicator are what we're verifying, not persistence.
func (demoDB) InsertFindings(context.Context, []db.Finding) error { return nil }

// ClearFindings is a no-op in the demo (the synthetic rollup is fixed), but it
// reports a plausible row count so the "clear findings" button's confirm +
// re-render path is exercisable with no database. The empty scenario has no
// findings to clear, so it reports 0.
func (d demoDB) ClearFindings(context.Context, string) (int64, error) {
	if d.scenario == "empty" {
		return 0, nil
	}
	return 37, nil // matches the demo TotalFindings
}

// ListFindings returns synthetic individual finding rows for the /findings
// worklist and the per-book Book section. The book dirs MUST match
// GetFindingsSummary.ByBook above so the worklist's book links resolve to the
// same demo books the per-book roll-up names. An empty (fresh-install) scenario
// returns nil; a non-empty dir scopes to one of the two demo books.
func (d demoDB) ListFindings(_ context.Context, dir string, limit int) ([]db.FindingRow, error) {
	if d.scenario == "empty" {
		return nil, nil
	}
	ci0, ci1, ci2 := 0, 4, 7
	corr1 := "Arecibo"
	corr2 := "three hundred"
	corr3 := "pen name"
	phmDir := "/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]"
	phmFile := phmDir + "/Project Hail Mary.m4b" // == demoBooks SamplePath → track 0 (⚑ matches)
	duneDir := "/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]"
	duneFile := duneDir + "/01 - Chapter 1.mp3" // == demoBooks SamplePath → track 0 (⚑ matches)
	// JobIDs are aligned to real demo track ids ("<dir>#<n>") so the finding
	// "Where" deep-jump (/track?id=…&t=…) lands on the matching demo book's track
	// 0 (an even index → demoDB.GetTrackDetail returns a full transcript), exercising
	// the deep-jump end-to-end against a real demo book (D4).
	job1, job2 := phmDir+"#0", duneDir+"#0"
	ci3, ci4 := 11, 18
	all := []db.FindingRow{
		{ID: "f1", FilePath: phmFile,
			BookDir: phmDir, JobID: &job1, ChunkIndex: &ci0,
			StartSec: 73.5, EndSec: 81.0, OriginalText: "auto sebo",
			IssueType: "misheard_proper_noun", SuggestedCorrection: &corr1, Confidence: 0.92},
		// Two more occurrences of the SAME correction at different timestamps — the
		// canonical recurring misheard proper noun. The /findings worklist collapses
		// these three into one ×3 group with a "show all locations" expander, which is
		// what makes the dedup feature visible in --demo.
		{ID: "f1b", FilePath: phmFile,
			BookDir: phmDir, JobID: &job1, ChunkIndex: &ci3,
			StartSec: 1450.0, EndSec: 1456.2, OriginalText: "auto sebo",
			IssueType: "misheard_proper_noun", SuggestedCorrection: &corr1, Confidence: 0.88},
		{ID: "f1c", FilePath: phmFile,
			BookDir: phmDir, JobID: &job1, ChunkIndex: &ci4,
			StartSec: 2980.5, EndSec: 2986.0, OriginalText: "auto sebo",
			IssueType: "misheard_proper_noun", SuggestedCorrection: &corr1, Confidence: 0.79},
		{ID: "f2", FilePath: phmFile,
			BookDir: phmDir, JobID: &job1, ChunkIndex: &ci1,
			StartSec: 612.0, EndSec: 618.4, OriginalText: "free hundred",
			IssueType: "number_artifact", SuggestedCorrection: &corr2, Confidence: 0.71},
		{ID: "f3", FilePath: duneFile,
			BookDir: duneDir, JobID: &job2, ChunkIndex: &ci2,
			StartSec: 145.2, EndSec: 150.9, OriginalText: "pin name",
			IssueType: "homophone", SuggestedCorrection: &corr3, Confidence: 0.55},
	}

	var out []db.FindingRow
	for _, f := range all {
		if dir == "" || f.BookDir == strings.TrimRight(strings.TrimSpace(dir), "/") {
			out = append(out, f)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetFindingsCountByBook returns the per-book findings counts for the library
// ⚑ column. The dirs and counts MUST match GetFindingsSummary.ByBook above so the
// library column agrees with the per-book roll-up (PHM 21, Dune 16); books with
// no findings are simply absent (looked up → 0). Empty on a fresh install.
func (d demoDB) GetFindingsCountByBook(context.Context) (map[string]int, error) {
	if d.scenario == "empty" {
		return map[string]int{}, nil
	}
	return map[string]int{
		"/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]": 21,
		"/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]":          16,
	}, nil
}

// demoEvalRun builds an eval runner backed by a static fake chat client, so the
// "run eval" dashboard buttons are exercisable in --demo with no network call
// (the demo's vLLM endpoint is intentionally offline). It runs the real
// eval.Run orchestration over the demo chunks, just with a canned judge reply.
func demoEvalRun(database DBInterface) evalRunFunc {
	judge := eval.NewJudge(demoFakeChat{})
	return func(ctx context.Context, opts eval.RunOptions) (eval.RunStats, error) {
		_, stats, err := eval.Run(ctx, database, judge, database, opts)
		return stats, err
	}
}

// demoFakeChat is a static eval.ChatClient: it returns one plausible finding per
// chunk so a demo run "completes" deterministically without a real endpoint.
type demoFakeChat struct{}

func (demoFakeChat) Model() string { return "demo-judge" }
func (demoFakeChat) Complete(context.Context, string, string) (string, error) {
	return `{"findings":[{"original_text":"sea shells","issue_type":"homophone","suggested_correction":"seashells","confidence":0.62}]}`, nil
}

// demoEvalChunks builds two synthetic chunks under the given book dir.
func demoEvalChunks(dir string) []db.EvalChunk {
	return []db.EvalChunk{
		{ChunkID: "demo-chunk-1", TranscriptID: "demo-tr-1", TranscriptionRunID: "demo-job-1",
			FilePath: dir + "/01.m4b", ChunkIndex: 0, StartSec: 0, EndSec: 30,
			Text: "the quick brown fox jumps over the lazy dog"},
		{ChunkID: "demo-chunk-2", TranscriptID: "demo-tr-1", TranscriptionRunID: "demo-job-1",
			FilePath: dir + "/01.m4b", ChunkIndex: 1, StartSec: 30, EndSec: 60,
			Text: "she sells sea shells by the sea shore"},
	}
}

// SetPaused flips the in-memory demo pause flag so the toggle is exercisable.
func (d demoDB) SetPaused(_ context.Context, paused bool, _ string) error {
	if d.paused != nil {
		*d.paused = paused
	}
	return nil
}

func (d demoDB) isPaused() bool { return d.paused != nil && *d.paused }

// GetControl reports the demo control state. SetRunLimit is a no-op in the demo
// (value receiver) — the control API isn't exercised against the demo fixture.
func (d demoDB) GetControl(context.Context) (bool, *int, error) {
	return d.isPaused(), d.runLimit, nil
}

func (d demoDB) SetRunLimit(context.Context, *int, string) error { return nil }

// GetPipelinePhase reports a per-scenario coordinator phase so the read-only
// phase badge renders a representative state with no database: the live "active"
// scenario is mid-transcribe, the crashed-runner "stale" scenario is in the
// analyze phase, and every other scenario is idle (the default).
func (d demoDB) GetPipelinePhase(context.Context) (string, error) {
	switch d.scenario {
	case "active":
		return db.PhaseTranscribe, nil
	case "stale":
		return db.PhaseAnalyze, nil
	default:
		return db.PhaseIdle, nil
	}
}

// GetServiceStatus returns a synthetic snapshot for the selected scenario.
func (d demoDB) GetServiceStatus(context.Context) (*db.QueueStats, error) {
	now := time.Now()
	var q *db.QueueStats
	switch d.scenario {
	case "empty":
		q = &db.QueueStats{}
	case "stale":
		hb := now.Add(-2 * time.Hour) // older than the 30m stale window
		emb := now.Add(-2 * time.Hour)
		avg := 0.0
		tok := int64(0)
		libDur := 432000.0
		libWords := int64(7_800_000)
		q = &db.QueueStats{
			Pending: 5, Claimed: 1, Done: 120, Failed: 0,
			Transcripts: 120, Chunks: 7431, EmbedBacklog: 0, LastEmbedAt: &emb,
			TotalJobs: 126, DoneLastHour: 0, // stalled → no recent completions → ETA "—"
			RunnerActive: true, RunnerID: "demo-runner", LastHeartbeat: &hb,
			// run_metrics exist but predate this stall; avg over zero-duration → "—".
			AvgProcessingSeconds: &avg, TotalEmbedTokens: &tok,
			TotalDurationSeconds: &libDur, TotalWords: &libWords, BooksFullyDone: 18, BooksTotal: 22,
		}
	case "failed":
		hb := now.Add(-40 * time.Second)
		emb := now.Add(-20 * time.Minute) // no embed for 20m + backlog → genuine stall
		avg := 624.0
		tok := int64(2_140_500)
		libDur := 295200.0
		libWords := int64(4_120_000)
		q = &db.QueueStats{
			Pending: 3, Claimed: 1, Done: 88, Failed: 7,
			Transcripts: 88, Chunks: 5120, EmbedBacklog: 14, LastEmbedAt: &emb, // backlog + stale embed → exercises the stall warning
			TotalJobs: 99, DoneLastHour: 4,
			RunnerActive: true, RunnerID: "demo-runner", LastHeartbeat: &hb,
			AvgProcessingSeconds: &avg, TotalEmbedTokens: &tok,
			TotalDurationSeconds: &libDur, TotalWords: &libWords, BooksFullyDone: 12, BooksTotal: 15,
		}
	default: // active
		hb := now.Add(-12 * time.Second)
		emb := now.Add(-30 * time.Second) // embeds landing → backlog is normal catch-up, no stall
		avg := 487.5
		tok := int64(6_820_400)
		libDur := 1_188_000.0
		libWords := int64(12_400_000)
		q = &db.QueueStats{
			Pending: 42, Claimed: 1, Done: 317, Failed: 2,
			Transcripts: 317, Chunks: 18452, EmbedBacklog: 3, LastEmbedAt: &emb,
			TotalJobs: 362, DoneLastHour: 22,
			RunnerActive: true, RunnerID: "demo-runner", LastHeartbeat: &hb,
			AvgProcessingSeconds: &avg, TotalEmbedTokens: &tok,
			TotalDurationSeconds: &libDur, TotalWords: &libWords, BooksFullyDone: 41, BooksTotal: 52,
		}
	}
	q.Paused = d.isPaused()
	q.RunLimit = d.runLimit
	return q, nil
}

// GetPredictInputs returns synthetic ETA inputs so the demo dashboard renders a
// populated ETA. The "stale"/"empty" scenarios omit availability history (→
// work-time fallback label); "active"/"failed" supply a healthy availability
// fraction (→ a calendar estimate).
func (d demoDB) GetPredictInputs(context.Context) (predict.Inputs, error) {
	switch d.scenario {
	case "empty":
		return predict.Inputs{}, nil
	case "stale":
		// Work known but no availability history → labeled work-time estimate.
		return predict.Inputs{
			RemainingChunks: 6 * 320,
			Rates: predict.Rates{
				TranscribeSecPerChunk: 7.1, EmbedSecPerChunk: 0.6,
				EvalSecPerChunk: 0, EvalKnown: false,
			},
			AvailabilityFraction: 0, // no windows → calendar unknown
		}, nil
	default: // active / failed — full estimate incl. eval + a calendar figure
		return predict.Inputs{
			RemainingChunks: 43 * 360,
			Rates: predict.Rates{
				TranscribeSecPerChunk: 7.1, EmbedSecPerChunk: 0.6,
				EvalSecPerChunk: 1.4, EvalKnown: true,
			},
			AvailabilityFraction: 0.45,
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

	// Synthetic run_metrics for 'done' jobs (nil on others — em-dash in the UI).
	procFast, procSlow := 92.0, 1340.0
	chunkedT, chunkedF := true, false
	win := 14
	tok1, tok2, tok3 := 18240, 4120, 26500
	chars1 := 612000

	jobs := []db.RecentJob{
		{ID: "demo-1", FilePath: "/books/Author One/A Long Title/01.m4b", Status: "claimed", UpdatedAt: now.Add(-12 * time.Second)},
		{ID: "demo-2", FilePath: "/books/Author Two/Another Book/Another Book.m4b", Status: "done", UpdatedAt: now.Add(-3 * time.Minute),
			ProcessingSeconds: &procFast, Chunked: &chunkedF, CharCount: &chars1, EmbedTotalTokens: &tok1},
		{ID: "demo-3", FilePath: "/books/Author Three/Short Stories/Short Stories.mp3", Status: "failed", UpdatedAt: now.Add(-9 * time.Minute), Error: &shortErr},
		{ID: "demo-4", FilePath: "/books/Author Four/The Sequel/The Sequel.m4b", Status: "done", UpdatedAt: now.Add(-22 * time.Minute),
			ProcessingSeconds: &procSlow, Chunked: &chunkedT, NWindows: &win, EmbedTotalTokens: &tok3},
		{ID: "demo-5", FilePath: "/books/Author Five/A Classic/A Classic.m4b", Status: "pending", UpdatedAt: now.Add(-31 * time.Minute)},
		{ID: "demo-6", FilePath: "/books/Author Six/A Novella/A Novella.m4b", Status: "done", UpdatedAt: now.Add(-48 * time.Minute),
			ProcessingSeconds: &procFast, Chunked: &chunkedF, EmbedTotalTokens: &tok2},
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

// demoBooks is a fixed synthetic library used by the library view in demo mode.
// The dirs and sample paths intentionally mix two collection shapes — an
// author/title layout (audio-libation) and an author-only layout where the
// title lives in the filename (audio-libro) — so the config-driven resolver is
// visibly exercised (matching demoCollections below).
func demoBooks() []db.BookSummary {
	now := time.Now()
	// fp/ip build nilable pointers inline so the demo can mix populated and NULL
	// per-book aggregates (a book with no run_metrics → em dash, like prod).
	fp := func(v float64) *float64 { return &v }
	ip := func(v int) *int { return &v }
	return []db.BookSummary{
		// audio-libro: author dir holds loose track files; title from filename.
		// Pending-only → no done tracks → all aggregates NULL (em dash).
		{Dir: "/books/audio-libro/Daniel Kahneman",
			SamplePath: "/books/audio-libro/Daniel Kahneman/Thinking Fast and Slow - Track 1.mp3",
			Total:      202, Pending: 202, LastUpdated: now.Add(-21 * time.Hour)},
		// audio-libation: author/title dirs. Fully done with aggregates populated.
		{Dir: "/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]",
			SamplePath: "/books/audio-libation/Andy Weir/Project Hail Mary [B08GB58KD5]/Project Hail Mary.m4b",
			Total:      1, Done: 1, LastUpdated: now.Add(-3 * time.Minute),
			DurationSeconds: fp(58320), WordCount: ip(124800), EmbedChunkCount: ip(412)},
		{Dir: "/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]",
			SamplePath: "/books/audio-libation/Frank Herbert/Dune [B0011UGNDG]/01 - Chapter 1.mp3",
			Total:      24, Done: 22, Claimed: 1, Pending: 1, LastUpdated: now.Add(-12 * time.Second),
			DurationSeconds: fp(75600), WordCount: ip(198400), EmbedChunkCount: ip(640)},
		// Done tracks exist but predate run_metrics → counts NULL, duration set.
		{Dir: "/books/audio-libation/Cixin Liu/The Three-Body Problem",
			SamplePath: "/books/audio-libation/Cixin Liu/The Three-Body Problem/01 - Part 1.mp3",
			Total:      16, Done: 14, Failed: 2, LastUpdated: now.Add(-9 * time.Minute),
			DurationSeconds: fp(46800)},
		// audio-custom: single-file book in an author dir (pending → em dashes).
		{Dir: "/books/audio-custom/George Orwell",
			SamplePath: "/books/audio-custom/George Orwell/1984.m4b",
			Total:      1, Pending: 1, LastUpdated: now.Add(-2 * time.Hour)},
		// Kahneman's *Noise* with a numeric ASIN containing "1984" — the regression
		// case for the book-resolution collision: book="1984" must NOT match this.
		{Dir: "/books/audio-libation/Daniel Kahneman/Noise [1984832069]",
			SamplePath: "/books/audio-libation/Daniel Kahneman/Noise [1984832069]/Noise.m4b",
			Total:      1, Done: 1, LastUpdated: now.Add(-5 * time.Hour),
			DurationSeconds: fp(50400), WordCount: ip(110000), EmbedChunkCount: ip(360)},
	}
}

// demoCollections mirrors a realistic LIBRARY_COLLECTIONS so the demo resolver
// derives author/title the same way production does.
const demoCollections = `[
	{"root":"audio-libation","layout":"author/title"},
	{"root":"audio-libro","layout":"author"},
	{"root":"audio-custom","layout":"author"}
]`

// GetBookSummaries serves the synthetic library, honoring status/query/paging so
// the filter and pagination controls are exercisable with no database.
func (d demoDB) GetBookSummaries(_ context.Context, f db.BookFilter) ([]db.BookSummary, int, error) {
	if d.scenario == "empty" {
		return nil, 0, nil
	}
	books := demoBooks()
	filtered := books[:0:0]
	for _, b := range books {
		if f.Query != "" {
			hay := strings.ToLower(b.Dir + " " + b.SamplePath)
			if !strings.Contains(hay, strings.ToLower(f.Query)) {
				continue
			}
		}
		switch f.Status {
		case "pending":
			if b.Pending == 0 {
				continue
			}
		case "claimed":
			if b.Claimed == 0 {
				continue
			}
		case "done":
			if b.Done == 0 {
				continue
			}
		case "failed":
			if b.Failed == 0 {
				continue
			}
		case "queued":
			// Books with remaining work: at least one pending or claimed track.
			if b.Pending == 0 && b.Claimed == 0 {
				continue
			}
		}
		filtered = append(filtered, b)
	}
	// Mirror the real query's ORDER BY for each sort mode.
	switch f.Sort {
	case "activity":
		// Most-recently-updated first (activity feed).
		sort.SliceStable(filtered, func(i, j int) bool {
			return filtered[i].LastUpdated.After(filtered[j].LastUpdated)
		})
	case "queue":
		// Active-first: (claimed>0) DESC, claimed DESC, pending DESC,
		// last_updated ASC (longest-waiting first), book_dir.
		sort.SliceStable(filtered, func(i, j int) bool {
			bi, bj := filtered[i], filtered[j]
			iActive := bi.Claimed > 0
			jActive := bj.Claimed > 0
			if iActive != jActive {
				return iActive // active first
			}
			if bi.Claimed != bj.Claimed {
				return bi.Claimed > bj.Claimed
			}
			if bi.Pending != bj.Pending {
				return bi.Pending > bj.Pending
			}
			if !bi.LastUpdated.Equal(bj.LastUpdated) {
				return bi.LastUpdated.Before(bj.LastUpdated) // oldest first
			}
			return bi.Dir < bj.Dir
		})
	default:
		// Default: transcribed-first order.
		sort.SliceStable(filtered, func(i, j int) bool {
			ri, rj := doneRatio(filtered[i]), doneRatio(filtered[j])
			if ri != rj {
				return ri > rj
			}
			return filtered[i].Done > filtered[j].Done
		})
	}
	total := len(filtered)
	lim := f.Limit
	if lim <= 0 {
		lim = 20
	}
	start := f.Offset
	if start > total {
		start = total
	}
	end := start + lim
	if end > total {
		end = total
	}
	return filtered[start:end], total, nil
}

// GetLibraryTotals computes whole-library counts from the synthetic books, so the
// list_books summary line is exercisable with no database. Honors the query
// (author) filter the same way GetBookSummaries does.
func (d demoDB) GetLibraryTotals(_ context.Context, query string) (db.LibraryTotals, error) {
	if d.scenario == "empty" {
		return db.LibraryTotals{}, nil
	}
	var t db.LibraryTotals
	for _, b := range demoBooks() {
		if query != "" {
			hay := strings.ToLower(b.Dir + " " + b.SamplePath)
			if !strings.Contains(hay, strings.ToLower(query)) {
				continue
			}
		}
		t.TotalBooks++
		// "Fully transcribed" = every track done (no pending/claimed/failed).
		if b.Done == b.Total && b.Total > 0 {
			t.FullyTranscribed++
		} else {
			t.WithPending++
		}
	}
	return t, nil
}

// GetBookTracks returns synthetic tracks for a demo book directory. Track 0 uses
// the book's real SamplePath so the book page's resolver derives the same
// author/title as the library list; later tracks vary the trailing number.
func (d demoDB) GetBookTracks(_ context.Context, dir string) ([]db.RecentJob, error) {
	now := time.Now()
	var out []db.RecentJob
	for _, b := range demoBooks() {
		if b.Dir != dir {
			continue
		}
		n := b.Total
		if n > 8 {
			n = 8 // cap demo expansion
		}
		for i := 0; i < n; i++ {
			status := "pending"
			switch {
			case i < b.Done:
				status = "done"
			case i < b.Done+b.Failed:
				status = "failed"
			}
			fp := b.SamplePath
			if i > 0 {
				fp = renumber(b.SamplePath, i+1)
			}
			rj := db.RecentJob{
				ID:        dir + "#" + strconv.Itoa(i),
				FilePath:  fp,
				Status:    status,
				UpdatedAt: now.Add(-time.Duration(i) * time.Minute),
			}
			// Populate per-track detail only for 'done' tracks, and only on every
			// other one — so the book-detail view shows both populated cells AND
			// em-dashes (most real transcripts have no run_metrics row). Pending /
			// failed / odd-index done tracks render em-dashes, mirroring prod.
			if status == "done" && i%2 == 0 {
				dur := 1800.0 + float64(i)*123.0
				proc := 95.0 + float64(i)*40.0
				words := 14200 + i*900
				codec := "aac"
				channels := 2
				chunks := 36 + i*4
				rj.DurationSeconds = &dur
				rj.ProcessingSeconds = &proc
				rj.WordCount = &words
				rj.AudioCodec = &codec
				rj.AudioChannels = &channels
				rj.EmbedChunkCount = &chunks
			}
			out = append(out, rj)
		}
	}
	return out, nil
}

// GetTrackDetail returns a synthetic per-track detail for the /track page. A
// trailing "#0" (or an id ending in an even index) is treated as a done track
// with a full transcript + chunks; an odd index is a pending track with no
// transcript (exercising the "not transcribed yet" state). The id format mirrors
// demoDB.GetBookTracks ("<dir>#<n>").
func (d demoDB) GetTrackDetail(_ context.Context, jobID string) (*db.TrackDetail, error) {
	now := time.Now()
	// Recover the synthetic track index from the "<dir>#<n>" id; default 0.
	idx := 0
	if h := strings.LastIndex(jobID, "#"); h >= 0 {
		if n, err := strconv.Atoi(jobID[h+1:]); err == nil {
			idx = n
		}
	}
	fp := jobID
	if h := strings.LastIndex(jobID, "#"); h >= 0 {
		fp = jobID[:h] + "/track.m4b"
	}

	det := &db.TrackDetail{
		ID: jobID, FilePath: fp, UpdatedAt: now.Add(-time.Duration(idx) * time.Minute),
		Attempts: 1,
	}

	// Odd index → pending track with no transcript (graceful empty state).
	if idx%2 == 1 {
		det.Status = "pending"
		return det, nil
	}

	// Even index → done track with full detail.
	det.Status = "done"
	det.HasTranscript = true
	det.Language = "en"
	det.DurationSeconds = 1830 + float64(idx)*120
	spk := 1
	det.SpeakerCount = &spk
	det.ModelName = "nvidia/parakeet-tdt-0.6b-v3"
	det.TranscriptAt = now.Add(-time.Duration(idx) * time.Minute)

	bytes := int64(48_300_000)
	channels := 2
	rate := 44100
	codec := "aac"
	format := "m4b"
	proc := 95.0 + float64(idx)*30
	compute := "bfloat16"
	host := "asr-runner-gpu-1"
	chunkedF := false
	words := 14200 + idx*800
	chars := 84000 + idx*4000
	segCount := 3
	embModel := "nomic-embed-text"
	embChunks := 36 + idx*4
	promptTok := 0
	totalTok := 18240 + idx*1200
	det.AudioBytes = &bytes
	det.AudioChannels = &channels
	det.AudioSampleRate = &rate
	det.AudioCodec = &codec
	det.AudioFormat = &format
	det.ProcessingSeconds = &proc
	det.ASRModel = &det.ModelName
	det.ComputeType = &compute
	det.RunnerHost = &host
	det.Chunked = &chunkedF
	det.WordCount = &words
	det.CharCount = &chars
	det.SegmentCount = &segCount
	det.EmbedModel = &embModel
	det.EmbedChunkCount = &embChunks
	det.EmbedPromptTokens = &promptTok
	det.EmbedTotalTokens = &totalTok

	spk0 := "SPEAKER_00"
	// Generate 72 segments so the reader's "load more" pagination (P7,
	// segmentPageSize=30) is visibly exercised: 3 pages (30 + 30 + 12).
	lines := []string{
		"The morning light fell across the desk.",
		"She had been waiting for this moment longer than she cared to admit.",
		"Outside, the rain had finally stopped, and the city began to stir.",
		"A single thought kept returning, unbidden, to the front of her mind.",
		"He closed the book and looked out toward the harbor.",
	}
	const nSegs = 72
	det.Segments = make([]db.Segment, nSegs)
	for i := 0; i < nSegs; i++ {
		start := float64(i) * 5.4
		det.Segments[i] = db.Segment{
			ID: i, Start: start, End: start + 5.0,
			Text: lines[i%len(lines)], Speaker: &spk0,
		}
	}
	det.Chunks = []db.ChunkRow{
		{ChunkIndex: 0, StartSec: 0.0, EndSec: 90.4, CharCount: 512, Speaker: &spk0},
		{ChunkIndex: 1, StartSec: 88.1, EndSec: 182.7, CharCount: 498, Speaker: &spk0},
		{ChunkIndex: 2, StartSec: 180.0, EndSec: 274.5, CharCount: 530, Speaker: &spk0},
	}
	return det, nil
}

// renumber replaces the last run of digits in a path's filename with n (demo
// helper, so sibling track names look plausible).
func renumber(p string, n int) string {
	end := strings.LastIndex(p, ".")
	if end < 0 {
		end = len(p)
	}
	name, ext := p[:end], p[end:]
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	if i == len(name) {
		return name + " " + strconv.Itoa(n) + ext
	}
	return name[:i] + strconv.Itoa(n) + ext
}

// StartDemoDashboard starts the HTTP transport (status dashboard + /mcp +
// /health + /readyz) backed by synthetic data, with no database connection.
// Intended for local UI iteration and AI-agent visual verification only.
// Set DEMO_SCENARIO=empty|stale|failed|active|multibackend to render a state.
func StartDemoDashboard(addr string) error {
	if addr == "" {
		addr = ":8081"
	}
	scenario := os.Getenv("DEMO_SCENARIO")
	if scenario == "" {
		scenario = "active"
	}
	// The "empty" scenario models a fresh install: no eval role bound, so the
	// /findings page shows the honest "Eval endpoint not configured" state and
	// the per-book "run eval" button is hidden. Every other scenario keeps the
	// eval role bound so the trigger surface renders.
	aiRoles := demoAIRoles
	// #nosec G101 - demo fixture string, not a real credential
	demoEvalToken := "demo-eval-token"
	if scenario == "empty" {
		aiRoles = &config.AIRoles{Embeddings: "embed-1"} // no Eval binding
		demoEvalToken = ""
	}
	cfg := &config.Config{
		MCPHTTPAddr:        addr,
		StaleJobTimeout:    30 * time.Minute,
		BooksDir:           "/books",
		LibraryCollections: demoCollections,
		// Honor CONTROL_API_TOKEN so the control-API mutations are exercisable
		// against the demo (otherwise they fail closed with 503). The eval-run
		// actions also fail closed without a token, so default one in for the demo
		// (unless the operator set their own) so the buttons are clickable.
		ControlAPIToken: cmp.Or(os.Getenv("CONTROL_API_TOKEN"), demoEvalToken),
		ASRServers:      demoServersFor(scenario),
		AIEndpoints:     demoAIEndpoints,
		AIRoles:         aiRoles,
	}
	srv := NewMCPServer(demoDB{scenario: scenario, paused: new(bool)}, cfg)
	// Swap the real HTTP probers for the static demo ones so the readiness states
	// render without any network call.
	srv.prober = demoGPUProber{}
	srv.endpointProber = demoEndpointProber{}
	// Swap the live chat client for a static fake judge so clicking "run eval"
	// exercises the trigger + async indicator with no network call (the demo
	// vLLM endpoint is intentionally offline). Only when the scenario configured
	// an eval endpoint in the first place.
	if srv.eval.configured {
		srv.eval.run = demoEvalRun(srv.db)
	}
	srv.logger.Info("Starting DEMO dashboard (synthetic data, no database)",
		"address", addr, "scenario", scenario)
	return srv.StartHTTP(addr)
}
