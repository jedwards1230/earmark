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
	d := trackData{
		Title: "B", Author: "A", BackDir: "/books/audio-libation/A/B", DurationPtr: &dur,
		Detail: &db.TrackDetail{
			ID: "t1", FilePath: "/books/audio-libation/A/B/01.m4b", Status: "done",
			UpdatedAt: time.Now(), HasTranscript: true, Language: "en", DurationSeconds: dur,
			ModelName:  "large-v3",
			AudioCodec: &codec, AudioChannels: &channels, AudioBytes: &bytesN, EmbedTotalTokens: &tot,
			Segments: []db.Segment{
				{ID: 0, Start: 0, End: 4.2, Text: "Chapter one.", Speaker: &spk},
				{ID: 1, Start: 4.2, End: 9.8, Text: "She waited.", Speaker: &spk},
			},
			Chunks: []db.ChunkRow{{ChunkIndex: 0, StartSec: 0, EndSec: 90.4, CharCount: 512, Speaker: &spk}},
		},
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
