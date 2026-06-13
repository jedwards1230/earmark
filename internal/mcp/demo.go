package mcp

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
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
	runner := "asr-runner-desktop-1"
	return []db.FailedJob{
		{ID: "f1", FilePath: "/books/audio-libation/Author Seven/An Epic/An Epic.m4b",
			Error: &cudaErr, Attempts: 3, ClaimedBy: &runner, UpdatedAt: now.Add(-15 * time.Second)},
		{ID: "f2", FilePath: "/books/audio-libro/Some Author/Short Stories - Track 3.mp3",
			Error: &codecErr, Attempts: 1, ClaimedBy: &runner, UpdatedAt: now.Add(-9 * time.Minute)},
	}, nil
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

// GetServiceStatus returns a synthetic snapshot for the selected scenario.
func (d demoDB) GetServiceStatus(context.Context) (*db.QueueStats, error) {
	now := time.Now()
	var q *db.QueueStats
	switch d.scenario {
	case "empty":
		q = &db.QueueStats{}
	case "stale":
		hb := now.Add(-2 * time.Hour) // older than the 30m stale window
		avg := 0.0
		tok := int64(0)
		libDur := 432000.0
		libWords := int64(7_800_000)
		q = &db.QueueStats{
			Pending: 5, Claimed: 1, Done: 120, Failed: 0,
			Transcripts: 120, Chunks: 7431, EmbedBacklog: 0,
			TotalJobs: 126, DoneLastHour: 0, // stalled → no recent completions → ETA "—"
			RunnerActive: true, RunnerID: "demo-runner", LastHeartbeat: &hb,
			// run_metrics exist but predate this stall; avg over zero-duration → "—".
			AvgProcessingSeconds: &avg, TotalEmbedTokens: &tok,
			TotalDurationSeconds: &libDur, TotalWords: &libWords, BooksFullyDone: 18,
		}
	case "failed":
		hb := now.Add(-40 * time.Second)
		avg := 624.0
		tok := int64(2_140_500)
		libDur := 295200.0
		libWords := int64(4_120_000)
		q = &db.QueueStats{
			Pending: 3, Claimed: 1, Done: 88, Failed: 7,
			Transcripts: 88, Chunks: 5120, EmbedBacklog: 14, // large → exercises the stall warning
			TotalJobs: 99, DoneLastHour: 4,
			RunnerActive: true, RunnerID: "demo-runner", LastHeartbeat: &hb,
			AvgProcessingSeconds: &avg, TotalEmbedTokens: &tok,
			TotalDurationSeconds: &libDur, TotalWords: &libWords, BooksFullyDone: 12,
		}
	default: // active
		hb := now.Add(-12 * time.Second)
		avg := 487.5
		tok := int64(6_820_400)
		libDur := 1_188_000.0
		libWords := int64(12_400_000)
		q = &db.QueueStats{
			Pending: 42, Claimed: 1, Done: 317, Failed: 2,
			Transcripts: 317, Chunks: 18452, EmbedBacklog: 3,
			TotalJobs: 362, DoneLastHour: 22,
			RunnerActive: true, RunnerID: "demo-runner", LastHeartbeat: &hb,
			AvgProcessingSeconds: &avg, TotalEmbedTokens: &tok,
			TotalDurationSeconds: &libDur, TotalWords: &libWords, BooksFullyDone: 41,
		}
	}
	q.Paused = d.isPaused()
	q.RunLimit = d.runLimit
	return q, nil
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
		}
		filtered = append(filtered, b)
	}
	// Mirror the real query's transcribed-first ORDER BY (done-ratio desc, then
	// done count desc) so the demo shows the same ordering as production.
	sort.SliceStable(filtered, func(i, j int) bool {
		ri, rj := doneRatio(filtered[i]), doneRatio(filtered[j])
		if ri != rj {
			return ri > rj
		}
		return filtered[i].Done > filtered[j].Done
	})
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
	host := "asr-runner-desktop-1"
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
// Set DEMO_SCENARIO=empty|stale|failed|active to render a specific state.
func StartDemoDashboard(addr string) error {
	if addr == "" {
		addr = ":8081"
	}
	scenario := os.Getenv("DEMO_SCENARIO")
	if scenario == "" {
		scenario = "active"
	}
	cfg := &config.Config{
		MCPHTTPAddr:        addr,
		StaleJobTimeout:    30 * time.Minute,
		BooksDir:           "/books",
		LibraryCollections: demoCollections,
		// Honor CONTROL_API_TOKEN so the control-API mutations are exercisable
		// against the demo (otherwise they fail closed with 503).
		ControlAPIToken: os.Getenv("CONTROL_API_TOKEN"),
	}
	srv := NewMCPServer(demoDB{scenario: scenario, paused: new(bool)}, cfg)
	srv.logger.Info("Starting DEMO dashboard (synthetic data, no database)",
		"address", addr, "scenario", scenario)
	return srv.StartHTTP(addr)
}
