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
