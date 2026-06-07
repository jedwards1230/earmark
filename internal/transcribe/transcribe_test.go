package transcribe

import (
	"context"
	"os"
	"testing"
)

// fakeInserter implements JobInserter for unit tests.
type fakeInserter struct {
	inserted map[string]bool // checksum -> present
	nextID   string
}

func (f *fakeInserter) InsertJobIfAbsent(_ context.Context, _, checksum string) (string, bool, error) {
	if f.inserted[checksum] {
		return f.nextID, false, nil
	}
	f.inserted[checksum] = true
	return f.nextID, true, nil
}

func TestComputeChecksum(t *testing.T) {
	tmp, err := os.CreateTemp("", "transcribe_test_*.bin")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	content := []byte("hello world")
	if _, err := tmp.Write(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	_ = tmp.Close()

	sum1, err := ComputeChecksum(tmp.Name())
	if err != nil {
		t.Fatalf("ComputeChecksum: %v", err)
	}
	if sum1 == "" {
		t.Fatal("expected non-empty checksum")
	}

	// Idempotent
	sum2, err := ComputeChecksum(tmp.Name())
	if err != nil {
		t.Fatalf("ComputeChecksum (2nd): %v", err)
	}
	if sum1 != sum2 {
		t.Errorf("checksums differ: %q vs %q", sum1, sum2)
	}
}

func TestComputeChecksum_Nonexistent(t *testing.T) {
	_, err := ComputeChecksum("/nonexistent/path/file.m4b")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestEnqueueJob_Created(t *testing.T) {
	tmp, err := os.CreateTemp("", "enqueue_test_*.bin")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString("data"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = tmp.Close()

	ins := &fakeInserter{inserted: make(map[string]bool), nextID: "job-uuid-1"}
	id, created, err := EnqueueJob(context.Background(), tmp.Name(), ins)
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	if !created {
		t.Error("expected job to be created")
	}
	if id != "job-uuid-1" {
		t.Errorf("unexpected job id: %q", id)
	}
}

func TestEnqueueJob_AlreadyExists(t *testing.T) {
	tmp, err := os.CreateTemp("", "enqueue_existing_*.bin")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString("data"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = tmp.Close()

	// Pre-populate the fake inserter
	sum, _ := ComputeChecksum(tmp.Name())
	ins := &fakeInserter{inserted: map[string]bool{sum: true}, nextID: "job-uuid-2"}
	_, created, err := EnqueueJob(context.Background(), tmp.Name(), ins)
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
	if created {
		t.Error("expected job to already exist")
	}
}
