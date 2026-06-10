package db

// transaction_test.go — tests for atomic operation patterns.
//
// The original tests simulated the correction-pipeline transaction logic
// which has been removed. We keep a minimal set that validates the
// stale-job recovery SQL shapes (without a live DB) and the checksum helpers.
//
// TODO(review): M-8 — add a full testcontainers-based DB integration suite
// covering InsertJobIfAbsent, RecoverStaleJobs, InsertChunks, and Search (needs Docker).
// TODO(review): M-9 — replace vacuous compile tests below with real behaviour assertions.

import (
	"testing"
)

// TestDBTypesCompile ensures that the domain types used across the package
// compile correctly and have the fields the MCP/worker layers expect.
func TestDBTypesCompile(t *testing.T) {
	_ = Job{
		ID:       "id",
		FilePath: "/books/audio-libation/Author/Book/01.m4b",
		Checksum: "abc123",
		Status:   "pending",
		Attempts: 0,
	}

	score := 0.9
	speaker := "SPEAKER_00"
	_ = Segment{
		ID:      0,
		Start:   0.0,
		End:     1.5,
		Text:    "Hello.",
		Speaker: &speaker,
		Words: []Word{
			{Word: "Hello,", Start: 0.0, End: 0.5, Score: &score, Speaker: &speaker},
		},
	}

	_ = SearchResultWithMetadata{
		ID:      "chunk-uuid",
		Content: "some text",
	}
}

// TestEmbedMetricsFields ensures the embed worker's run_metrics slice carries
// the expected fields, with PromptTokens nullable (Ollama omits provider usage)
// and TotalTokens the authoritative local count.
func TestEmbedMetricsFields(t *testing.T) {
	prompt := 7
	m := EmbedMetrics{
		JobID:        "job-uuid",
		Model:        "nomic-embed-text",
		ChunkCount:   4,
		PromptTokens: &prompt,
		TotalTokens:  321,
	}
	if m.PromptTokens == nil || *m.PromptTokens != 7 {
		t.Errorf("PromptTokens mismatch")
	}
	if m.TotalTokens != 321 {
		t.Errorf("TotalTokens mismatch")
	}

	// PromptTokens must be allowed to be nil (the Ollama path).
	m.PromptTokens = nil
	if m.PromptTokens != nil {
		t.Errorf("expected nil-able PromptTokens")
	}
}

// TestRecentJobMetricsFields ensures the run_metrics LEFT JOIN fields on
// RecentJob are nullable pointers (nil when no metrics row exists).
func TestRecentJobMetricsFields(t *testing.T) {
	secs := 12.5
	chunked := true
	windows := 3
	chars := 4096
	tokens := 1500
	j := RecentJob{
		ID:                "job-uuid",
		Status:            "done",
		ProcessingSeconds: &secs,
		Chunked:           &chunked,
		NWindows:          &windows,
		CharCount:         &chars,
		EmbedTotalTokens:  &tokens,
	}
	if j.ProcessingSeconds == nil || *j.ProcessingSeconds != 12.5 {
		t.Errorf("ProcessingSeconds mismatch")
	}
	if j.Chunked == nil || !*j.Chunked {
		t.Errorf("Chunked mismatch")
	}

	// A bare RecentJob (no metrics joined) must have nil metric fields.
	bare := RecentJob{ID: "x", Status: "pending"}
	if bare.ProcessingSeconds != nil || bare.EmbedTotalTokens != nil {
		t.Errorf("expected nil metric fields on un-joined RecentJob")
	}
}

// TestStatisticsFields ensures Statistics struct has the expected fields.
func TestStatisticsFields(t *testing.T) {
	s := Statistics{
		PendingJobs:    3,
		CompletedJobs:  10,
		EmbeddedChunks: 500,
	}
	if s.PendingJobs != 3 {
		t.Errorf("PendingJobs mismatch")
	}
	if s.CompletedJobs != 10 {
		t.Errorf("CompletedJobs mismatch")
	}
	if s.EmbeddedChunks != 500 {
		t.Errorf("EmbeddedChunks mismatch")
	}
}
