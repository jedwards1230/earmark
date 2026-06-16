package mcp

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
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

func TestHumanizeSeconds(t *testing.T) {
	cases := []struct {
		secs float64
		want string
	}{
		{0, "0s"},
		{5, "5s"},          // sub-minute
		{59, "59s"},        // sub-minute boundary
		{60, "1m0s"},       // exact minute — seconds component kept, not dropped
		{65, "1m5s"},       // minutes + seconds
		{3600, "1h0m0s"},   // exact hour — m/s kept for an unambiguous breakdown
		{3725, "1h2m5s"},   // hours + minutes + seconds (no dropped component)
		{3661.5, "1h1m2s"}, // fractional input rounds to whole seconds (61.5s → 1m2s)
		{2.4, "2s"},        // fractional rounds down
		{2.5, "3s"},        // fractional rounds up
	}
	for _, c := range cases {
		if got := humanizeSeconds(c.secs); got != c.want {
			t.Errorf("humanizeSeconds(%v) = %q, want %q", c.secs, got, c.want)
		}
	}
}

// TestPipelineStateDerivation verifies the unified pipeline state never
// contradicts itself: RUNNING only when a fresh runner is connected, IDLE when
// enabled-but-no/stale-runner, PAUSED when the flag is set.
func TestPipelineStateDerivation(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	stale := 30 * time.Minute

	// Fresh heartbeat, not paused → RUNNING (green).
	recent := now.Add(-10 * time.Second)
	d := newStatusData(&db.QueueStats{RunnerActive: true, RunnerID: "r1", LastHeartbeat: &recent}, nil, now, stale, "")
	if d.StateLabel != "RUNNING" || d.DotClass != "green" {
		t.Errorf("fresh runner = (%q,%q), want (RUNNING,green)", d.StateLabel, d.DotClass)
	}

	// Old heartbeat, RunnerActive, but NO work waiting → IDLE (drained), not RUNNING.
	old := now.Add(-2 * time.Hour)
	d = newStatusData(&db.QueueStats{RunnerActive: true, LastHeartbeat: &old}, nil, now, stale, "")
	if d.StateLabel != "IDLE" {
		t.Errorf("stale runner, no work = StateLabel %q, want IDLE", d.StateLabel)
	}
	if !strings.Contains(d.SubText, "stale") {
		t.Errorf("stale runner SubText = %q, want it to mention stale", d.SubText)
	}

	// Old heartbeat, RunnerActive, AND work waiting → STALLED (red) — an incident.
	d = newStatusData(&db.QueueStats{RunnerActive: true, LastHeartbeat: &old, Pending: 5, Claimed: 1}, nil, now, stale, "")
	if d.StateLabel != "STALLED" || d.DotClass != "red" {
		t.Errorf("stale runner with work = (%q,%q), want (STALLED,red)", d.StateLabel, d.DotClass)
	}

	// Not paused, no runner ever seen → IDLE "no runner connected".
	d = newStatusData(&db.QueueStats{Pending: 4069}, nil, now, stale, "")
	if d.StateLabel != "IDLE" || d.DotClass != "blue" {
		t.Errorf("no-runner = (%q,%q), want (IDLE,blue)", d.StateLabel, d.DotClass)
	}
	if !strings.Contains(d.SubText, "no runner") {
		t.Errorf("no-runner SubText = %q, want it to mention no runner", d.SubText)
	}

	// Paused wins regardless of runner liveness.
	d = newStatusData(&db.QueueStats{Paused: true, RunnerActive: true, LastHeartbeat: &recent}, nil, now, stale, "")
	if d.StateLabel != "PAUSED" || d.DotClass != "amber" {
		t.Errorf("paused = (%q,%q), want (PAUSED,amber)", d.StateLabel, d.DotClass)
	}
}

// TestStatusFragmentRendersIdleNotRunning is the regression guard for the
// reported contradiction: an enabled pipeline with no live runner must render
// IDLE (not "RUNNING — claiming jobs").
func TestStatusFragmentRendersIdleNotRunning(t *testing.T) {
	now := time.Now()
	data := newStatusData(
		&db.QueueStats{Pending: 4069, Chunks: 12345}, // not paused, no runner
		nil, now, 30*time.Minute, "",
	)
	var buf bytes.Buffer
	if err := statusFragmentTmpl.Execute(&buf, data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "IDLE") {
		t.Errorf("expected IDLE state:\n%s", out)
	}
	if strings.Contains(out, "RUNNING") {
		t.Error("must NOT claim RUNNING when no runner is connected")
	}
	if !strings.Contains(out, "12,345") {
		t.Error("counts should render with thousands separators")
	}
	if !strings.Contains(out, "updated ") {
		t.Error("fragment should carry an 'updated' recency stamp")
	}
}

// TestStatusFragmentRendersStalled verifies a crashed runner with work waiting
// renders the loud STALLED state, not a calm IDLE.
func TestStatusFragmentRendersStalled(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	data := newStatusData(
		&db.QueueStats{RunnerActive: true, LastHeartbeat: &old, Pending: 5, Claimed: 1},
		nil, now, 30*time.Minute, "",
	)
	var buf bytes.Buffer
	if err := statusFragmentTmpl.Execute(&buf, data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "STALLED") || !strings.Contains(out, "state-stalled") {
		t.Errorf("expected a STALLED banner:\n%s", out)
	}
}

func TestBackfillProgressDerivation(t *testing.T) {
	now := time.Now()
	// Active backfill: 317/362 done, 22/hr → finite ETA.
	d := newStatusData(&db.QueueStats{
		Pending: 42, Claimed: 1, Done: 317, Failed: 2, TotalJobs: 362, DoneLastHour: 22,
	}, nil, now, 30*time.Minute, "")
	if !d.ShowProgress {
		t.Fatal("ShowProgress should be true when TotalJobs > 0")
	}
	if d.DonePct != 87 {
		t.Errorf("DonePct = %d, want 87 (317*100/362)", d.DonePct)
	}
	for _, want := range []string{"317", "362", "87%"} {
		if !strings.Contains(d.ProgressText, want) {
			t.Errorf("ProgressText %q missing %q", d.ProgressText, want)
		}
	}
	if !strings.Contains(d.ThroughputText, "22") {
		t.Errorf("ThroughputText = %q, want it to mention 22", d.ThroughputText)
	}
	if d.ETAText == "" || d.ETAText == "—" {
		t.Errorf("ETAText = %q, want a finite estimate", d.ETAText)
	}

	// Zero throughput → ETA "—", no panic.
	d = newStatusData(&db.QueueStats{Pending: 100, Done: 10, TotalJobs: 110, DoneLastHour: 0}, nil, now, 30*time.Minute, "")
	if d.ETAText != "—" {
		t.Errorf("zero-throughput ETAText = %q, want —", d.ETAText)
	}

	// Empty install → no progress shown, no divide-by-zero.
	d = newStatusData(&db.QueueStats{}, nil, now, 30*time.Minute, "")
	if d.ShowProgress {
		t.Error("ShowProgress should be false on an empty install")
	}
}

func TestHumanizeETA(t *testing.T) {
	if got := humanizeETA(0); got != "—" {
		t.Errorf("humanizeETA(0) = %q, want —", got)
	}
	if got := humanizeETA(0.5); got != "<1h left" {
		t.Errorf("humanizeETA(0.5) = %q, want <1h left", got)
	}
	if got := humanizeETA(5); got != "~5h left" {
		t.Errorf("humanizeETA(5) = %q, want ~5h left", got)
	}
	if got := humanizeETA(168); got != "~7.0 days left" {
		t.Errorf("humanizeETA(168) = %q, want ~7.0 days left", got)
	}
}

// TestErrorRowIsExpandable verifies a job error renders inside a <details>
// expander (the full traceback is reachable), and is still HTML-escaped.
func TestCodecLabel(t *testing.T) {
	aac := "aac"
	mp3 := "mp3"
	empty := ""
	one, two, six := 1, 2, 6
	cases := []struct {
		name     string
		codec    *string
		channels *int
		want     string
	}{
		{"codec+stereo", &aac, &two, "aac · stereo"},
		{"codec+mono", &mp3, &one, "mp3 · mono"},
		{"codec+multichannel", &aac, &six, "aac · 6ch"},
		{"codec only (channels nil)", &aac, nil, "aac"},
		{"channels only (codec nil)", nil, &two, "stereo"},
		{"both nil", nil, nil, "—"},
		{"empty codec string falls back", &empty, nil, "—"},
	}
	for _, c := range cases {
		if got := codecLabel(c.codec, c.channels); got != c.want {
			t.Errorf("%s: codecLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCommafy64(t *testing.T) {
	cases := map[int64]string{0: "0", 412: "412", 6_820_400: "6,820,400", -2500: "-2,500"}
	for in, want := range cases {
		if got := commafy64(in); got != want {
			t.Errorf("commafy64(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestBookFragmentRendersDetailAndEmDash renders bookFragmentTmpl with one
// fully-populated 'done' track and one 'pending' track whose detail fields are
// all nil, and asserts both the populated cells (duration, words, "aac ·
// stereo", chunk count) AND the em-dashes for the NULL track appear — the
// load-bearing "never assume the joined row exists" requirement.
func TestBookFragmentRendersDetailAndEmDash(t *testing.T) {
	dur := 1830.0
	proc := 95.5
	words := 14200
	codec := "aac"
	channels := 2
	chunks := 36

	d := bookData{
		Dir: "/books/audio-libation/A/B", DirQuery: "x", Title: "B", Author: "A", Total: 2, Done: 1, Pending: 1,
		Tracks: []db.RecentJob{
			{ID: "t1", FilePath: "/books/audio-libation/A/B/01.m4b", Status: "done", UpdatedAt: time.Now(),
				DurationSeconds: &dur, ProcessingSeconds: &proc, WordCount: &words,
				AudioCodec: &codec, AudioChannels: &channels, EmbedChunkCount: &chunks},
			{ID: "t2", FilePath: "/books/audio-libation/A/B/02.m4b", Status: "pending", UpdatedAt: time.Now()},
		},
	}
	var buf bytes.Buffer
	if err := bookFragmentTmpl.Execute(&buf, d); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"<th title=\"audio duration\">Duration</th>",
		"<th title=\"embedded chunks\">Chunks</th>",
		"30m30s",       // duration 1830s
		"1m36s",        // proc 95.5s rounds to 96s
		"14,200",       // words
		"aac · stereo", // codec + channels
		">36<",         // chunk count cell
		"—",            // em-dash for the pending track's NULL detail
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in book fragment output:\n%s", want, out)
		}
	}
}

// TestBookFragmentRendersFindings renders bookFragmentTmpl with a per-book
// findings summary + two individual rows and asserts the Findings section shows
// the confidence %, issue type, the "original → corrected" correction, the ⚑
// per-track flag cell, and the #book-findings anchor. It also asserts the em-dash
// empty-state when a book has no findings, mirroring
// TestBookFragmentRendersDetailAndEmDash.
func TestBookFragmentRendersFindings(t *testing.T) {
	dir := "/books/audio-libation/A/B"
	job := "job-1"
	ci := 4
	corr1 := "Arecibo"
	summary := &db.BookFindings{
		BookDir: dir, FilePath: dir + "/01.m4b", Count: 2,
		MeanConfidence: 0.73, TopIssueType: "misheard_proper_noun",
	}
	findings := []db.FindingRow{
		{ID: "f1", FilePath: dir + "/01.m4b", BookDir: dir, JobID: &job, ChunkIndex: &ci,
			StartSec: 73.5, EndSec: 81.0, OriginalText: "auto sebo",
			IssueType: "misheard_proper_noun", SuggestedCorrection: &corr1, Confidence: 0.92},
		{ID: "f2", FilePath: dir + "/01.m4b", BookDir: dir,
			StartSec: 612.0, EndSec: 618.4, OriginalText: "free hundred",
			IssueType: "number_artifact", SuggestedCorrection: nil, Confidence: 0.71},
	}
	d := bookData{
		Dir: dir, DirQuery: "x", Title: "B", Author: "A", Total: 1, Done: 1,
		Tracks: []db.RecentJob{
			{ID: "t1", FilePath: dir + "/01.m4b", Status: "done", UpdatedAt: time.Now()},
		},
		FindingsSummary: summary,
		Findings:        findings,
		FindingsByTrack: map[string]int{dir + "/01.m4b": 2},
		ControlEnabled:  false, // clear-book-findings button must NOT render
	}
	var buf bytes.Buffer
	if err := bookFragmentTmpl.Execute(&buf, d); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`id="book-findings"`,    // findings section anchor
		"2 suspected errors",    // summary count line
		"73%",                   // mean conf 0.73 → 73%
		"misheard_proper_noun",  // top issue + finding issue type
		"92%",                   // finding confidence
		"auto sebo",             // original span
		"Arecibo",               // suggested correction
		"&#9873; 2",             // ⚑ 2 flag cell on the tracks table
		`href="#book-findings"`, // flag cell links to the findings section
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in book fragment output:\n%s", want, out)
		}
	}
	// f2 has a nil correction → em-dash via strPtr.
	if !strings.Contains(out, "free hundred &#8594; —") {
		t.Errorf("expected em-dash empty correction for f2:\n%s", out)
	}
	// ControlEnabled is false → no clear-book-findings button.
	if strings.Contains(out, "clear book findings") {
		t.Error("clear-book-findings button must not render when ControlEnabled is false")
	}

	// Empty-state: a book with no findings renders the em-dash/empty notice and no
	// findings table.
	dEmpty := bookData{Dir: dir, DirQuery: "x", Title: "B", Author: "A", Total: 1, Done: 1,
		Tracks: []db.RecentJob{{ID: "t1", FilePath: dir + "/01.m4b", Status: "done", UpdatedAt: time.Now()}}}
	var bufE bytes.Buffer
	if err := bookFragmentTmpl.Execute(&bufE, dEmpty); err != nil {
		t.Fatalf("execute (empty): %v", err)
	}
	outE := bufE.String()
	if !strings.Contains(outE, "No findings recorded for this book") {
		t.Errorf("expected per-book findings empty-state:\n%s", outE)
	}
	if !strings.Contains(outE, "—") {
		t.Errorf("expected em-dash in empty Flags cell:\n%s", outE)
	}
}

// TestFindingsFragmentRendersWorklistAndBookLinks renders findingsFragmentTmpl
// with a populated summary (per-book roll-up) AND the individual worklist rows,
// and asserts: the per-book table name is now a `/book?dir=` link (not plain
// text), the "Findings (worklist)" section renders its rows (confidence, issue,
// original → correction), and each worklist row links to its book.
func TestFindingsFragmentRendersWorklistAndBookLinks(t *testing.T) {
	dir := "/books/Author One/A Long Title"
	mean := 0.66
	job := "job-1"
	corr := "Arecibo"
	d := findingsData{
		RenderedAt: "2026-01-01 00:00:00 UTC",
		Summary: &db.FindingsSummary{
			TotalFindings: 2, MeanConfidence: &mean, HighConfidence: 1, MediumConfidence: 1,
			ByBook: []db.BookFindings{
				{BookDir: dir, FilePath: dir + "/01.m4b", Count: 2, MeanConfidence: 0.66, TopIssueType: "misheard_proper_noun"},
			},
		},
		Findings: []db.FindingRow{
			{ID: "f1", FilePath: dir + "/01.m4b", BookDir: dir, JobID: &job, StartSec: 73.5, EndSec: 81.0,
				OriginalText: "auto sebo", IssueType: "misheard_proper_noun", SuggestedCorrection: &corr, Confidence: 0.92},
		},
	}
	var buf bytes.Buffer
	if err := findingsFragmentTmpl.Execute(&buf, d); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`href="/book?dir=`,          // per-book table name is a link now
		"Findings (worklist)",       // worklist section title
		"auto sebo &#8594; Arecibo", // original → correction
		"92%",                       // worklist row confidence
		"misheard_proper_noun",      // issue type
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in findings fragment output:\n%s", want, out)
		}
	}
}

// TestBookFragmentFindingsXSSEscaping verifies html/template auto-escapes the
// model-generated finding text (original span, suggested correction, issue type)
// in the book fragment — findings are LLM output, so this is the same XSS guard as
// TestTrackFragmentXSSEscaping applies to transcript segments.
func TestBookFragmentFindingsXSSEscaping(t *testing.T) {
	dir := "/books/A/B"
	malicious := `<script>alert(1)</script>`
	maliciousCorr := `"><img src=x onerror=alert(1)>`
	corr := maliciousCorr
	d := bookData{
		Dir: dir, DirQuery: "x", Title: "B", Author: "A", Total: 1, Done: 1,
		Tracks: []db.RecentJob{{ID: "t1", FilePath: dir + "/01.m4b", Status: "done", UpdatedAt: time.Now()}},
		Findings: []db.FindingRow{
			{ID: "f1", FilePath: dir + "/01.m4b", BookDir: dir, StartSec: 1, EndSec: 2,
				OriginalText: malicious, IssueType: malicious, SuggestedCorrection: &corr, Confidence: 0.5},
		},
	}
	var buf bytes.Buffer
	if err := bookFragmentTmpl.Execute(&buf, d); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("finding text: raw <script> must not appear — html/template must escape it")
	}
	if strings.Contains(out, `"><img src=x onerror=alert(1)>`) {
		t.Error("finding correction: raw attribute-injection payload must not appear")
	}
	if !strings.Contains(out, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("expected HTML-escaped finding text in output")
	}
}

// TestTrackFragmentRendersDetail renders trackFragmentTmpl for a done track with
// transcript + chunks and asserts the 3 panels, the timestamped reader (mm:ss),
// and the chunk list all appear, plus the back-to-book link.
func TestTrackFragmentRendersDetail(t *testing.T) {
	spk := "SPEAKER_00"
	codec := "aac"
	channels := 2
	bytesN := int64(48300000)
	tot := 18240
	dur := 1830.0
	segs := []db.Segment{
		{ID: 0, Start: 0, End: 4.2, Text: "Chapter one.", Speaker: &spk},
		{ID: 1, Start: 4.2, End: 9.8, Text: "She waited.", Speaker: &spk},
	}
	d := trackData{
		Title: "B", Author: "A", BackDir: "/books/audio-libation/A/B", DurationPtr: &dur,
		Detail: &db.TrackDetail{
			ID: "t1", FilePath: "/books/audio-libation/A/B/01.m4b", Status: "done",
			UpdatedAt: time.Now(), HasTranscript: true, Language: "en", DurationSeconds: dur,
			ModelName:  "large-v3",
			AudioCodec: &codec, AudioChannels: &channels, AudioBytes: &bytesN, EmbedTotalTokens: &tot,
			Segments: segs,
			Chunks:   []db.ChunkRow{{ChunkIndex: 0, StartSec: 0, EndSec: 90.4, CharCount: 512, Speaker: &spk}},
		},
		// Mirror handleTrackData: first page of segments, no "load more" (2 < 30).
		TotalSegments: len(segs), PageSegments: segs, HasMore: false,
	}
	var buf bytes.Buffer
	if err := trackFragmentTmpl.Execute(&buf, d); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"href=\"/book?dir=",                         // back-to-book link
		">Audio<", ">Transcription<", ">Embedding<", // 3 panels
		"large-v3",              // model
		"18,240",                // total tokens
		"[00:00 &#8594; 00:04]", // reader timestamp (mm:ss)
		"Chapter one.",          // segment text
		"Time range",            // chunk table header
		"01:30",                 // chunk end 90.4s → 1m30s → "01:30"
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in track fragment:\n%s", want, out)
		}
	}
}

// TestTrackFragmentNotTranscribedYet renders the pending-track path: no
// transcript → the graceful "not transcribed yet" state plus em-dashes in the
// panels, and no reader/chunk-table.
func TestTrackFragmentNotTranscribedYet(t *testing.T) {
	d := trackData{
		Title: "B", Author: "A", BackDir: "/books/audio-libation/A/B",
		Detail: &db.TrackDetail{
			ID: "t2", FilePath: "/books/audio-libation/A/B/02.m4b", Status: "pending",
			UpdatedAt: time.Now(), HasTranscript: false,
		},
	}
	var buf bytes.Buffer
	if err := trackFragmentTmpl.Execute(&buf, d); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Not transcribed yet") {
		t.Errorf("expected graceful 'Not transcribed yet' state:\n%s", out)
	}
	if !strings.Contains(out, "<dd>—</dd>") {
		t.Error("expected em-dashes in the panels for a no-metrics pending track")
	}
	if strings.Contains(out, "seg-text") || strings.Contains(out, "Time range") {
		t.Error("pending track must not render the reader or chunk table")
	}
}

func TestTrackFragmentNotTranscribedYet_EmDashCount(t *testing.T) {
	d := trackData{
		Detail: &db.TrackDetail{ID: "t3", FilePath: "/a/b/c.m4b", Status: "claimed"},
	}
	var buf bytes.Buffer
	if err := trackFragmentTmpl.Execute(&buf, d); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if n := strings.Count(buf.String(), "—"); n < 15 {
		t.Errorf("expected many em-dashes for a no-data track, got %d", n)
	}
}

func TestTimestamp(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "00:00"}, {4.2, "00:04"}, {65, "01:05"}, {3661, "1:01:01"}, {-5, "00:00"},
	}
	for _, c := range cases {
		if got := timestamp(c.in); got != c.want {
			t.Errorf("timestamp(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanizeBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"}, {1024, "1.0 KB"}, {1536, "1.5 KB"}, {48300000, "46.1 MB"},
	}
	for _, c := range cases {
		if got := humanizeBytes(c.in); got != c.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTrackFragmentXSSEscaping verifies that html/template auto-escapes
// DB-sourced transcript segment text and speaker names, preventing XSS from
// malicious content stored in the database.
func TestTrackFragmentXSSEscaping(t *testing.T) {
	maliciousText := `<script>alert(1)</script>`
	maliciousSpeaker := `"><img src=x onerror=alert(1)>`
	chunkSpeaker := maliciousSpeaker
	dur := 10.0
	seg := db.Segment{ID: 0, Start: 0, End: 5, Text: maliciousText, Speaker: &maliciousSpeaker}
	d := trackData{
		Title: "B", Author: "A", BackDir: "%2Fbooks%2FA%2FB", DurationPtr: &dur,
		TotalSegments: 1, PageSegments: []db.Segment{seg},
		Detail: &db.TrackDetail{
			ID: "t-xss", FilePath: "/books/A/B/01.m4b", Status: "done",
			UpdatedAt: time.Now(), HasTranscript: true, DurationSeconds: dur,
			Chunks: []db.ChunkRow{{ChunkIndex: 0, StartSec: 0, EndSec: 10, CharCount: 26, Speaker: &chunkSpeaker}},
		},
	}
	var buf bytes.Buffer
	if err := trackFragmentTmpl.Execute(&buf, d); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	// Raw script tags and unquoted attribute injections must not appear.
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("segment text: raw <script> tag must not appear in output — html/template must escape it")
	}
	if strings.Contains(out, `"><img src=x onerror=alert(1)>`) {
		t.Error("segment speaker: raw attribute-injection payload must not appear in output — html/template must escape it")
	}

	// The escaped forms must be present, proving the content is rendered (not dropped).
	if !strings.Contains(out, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("segment text: expected HTML-escaped form &lt;script&gt; in output")
	}
	if !strings.Contains(out, "&gt;&lt;img src=x onerror=alert(1)&gt;") {
		t.Error("segment speaker / chunk speaker: expected HTML-escaped form in output")
	}
}

func TestPaginateSegments(t *testing.T) {
	mk := func(n int) []db.Segment {
		s := make([]db.Segment, n)
		for i := range s {
			s[i] = db.Segment{ID: i}
		}
		return s
	}
	cases := []struct {
		name        string
		total       int
		offset      int
		wantLen     int
		wantHasMore bool
		wantNext    int
	}{
		{"first page of many", 72, 0, 30, true, 30},
		{"second page", 72, 30, 30, true, 60},
		{"last partial page", 72, 60, 12, false, 72},
		{"exact one page", 30, 0, 30, false, 30},
		{"fewer than a page", 5, 0, 5, false, 5},
		{"empty", 0, 0, 0, false, 0},
		{"offset past end clamps", 10, 99, 0, false, 10},
		{"negative offset clamps", 40, -5, 30, true, 30},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			page, hasMore, next := paginateSegments(mk(c.total), c.offset)
			if len(page) != c.wantLen {
				t.Errorf("len = %d, want %d", len(page), c.wantLen)
			}
			if hasMore != c.wantHasMore {
				t.Errorf("hasMore = %v, want %v", hasMore, c.wantHasMore)
			}
			if next != c.wantNext {
				t.Errorf("next = %d, want %d", next, c.wantNext)
			}
		})
	}
}

// TestTrackReaderPaginates renders the track fragment with >segmentPageSize
// segments and asserts only the first page is rendered plus a "load more"
// button, and the second-page (segments fragment) renders the remainder.
func TestTrackReaderPaginates(t *testing.T) {
	const total = 72
	segs := make([]db.Segment, total)
	for i := range segs {
		segs[i] = db.Segment{ID: i, Start: float64(i), End: float64(i) + 1, Text: "seg"}
	}
	page, hasMore, next := paginateSegments(segs, 0)
	d := trackData{
		IDQuery: "abc",
		Detail: &db.TrackDetail{
			ID: "t1", FilePath: "/a/b/c.m4b", Status: "done", HasTranscript: true,
			Segments: segs,
		},
		TotalSegments: total, PageSegments: page, HasMore: hasMore, NextOffset: next,
	}
	var buf bytes.Buffer
	if err := trackFragmentTmpl.Execute(&buf, d); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if n := strings.Count(out, `class="seg"`); n != segmentPageSize {
		t.Errorf("rendered %d segments on first page, want %d", n, segmentPageSize)
	}
	if !strings.Contains(out, `/track/segments?id=abc&offset=30`) {
		t.Errorf("expected load-more button to /track/segments offset=30:\n%s", out)
	}
	if !strings.Contains(out, "72 segments") {
		t.Error("header should show the FULL segment count, not just the page")
	}

	// Second page via the standalone segments fragment.
	page2, hasMore2, next2 := paginateSegments(segs, 30)
	var buf2 bytes.Buffer
	if err := segmentsFragmentTmpl.Execute(&buf2, segmentsData{Segments: page2, HasMore: hasMore2, NextOffset: next2, IDQuery: "abc"}); err != nil {
		t.Fatalf("execute segments: %v", err)
	}
	out2 := buf2.String()
	if n := strings.Count(out2, `class="seg"`); n != segmentPageSize {
		t.Errorf("second page rendered %d segments, want %d", n, segmentPageSize)
	}
	if !strings.Contains(out2, "offset=60") {
		t.Error("second page should chain to offset=60")
	}
}

func TestErrorRowIsExpandable(t *testing.T) {
	evil := "Traceback line 1\n<script>alert(1)</script>\nRuntimeError: boom"
	data := newStatusData(&db.QueueStats{TotalJobs: 1, Failed: 1}, []db.RecentJob{
		{ID: "x", FilePath: "/books/a/b.m4b", Status: "failed", Error: &evil},
	}, time.Now(), 30*time.Minute, "")
	var buf bytes.Buffer
	if err := statusFragmentTmpl.Execute(&buf, data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "<details class=\"error-row\"") || !strings.Contains(out, "<summary>") {
		t.Errorf("error should render in a <details> expander:\n%s", out)
	}
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("error text must be HTML-escaped, not raw")
	}
	if !strings.Contains(out, "RuntimeError: boom") {
		t.Error("the full error text should be present (not clamped away)")
	}
}
