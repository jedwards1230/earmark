// Package transcribe implements the job-queue producer for transcription jobs.
//
// The Go service no longer performs transcription itself. Instead it:
//  1. Enqueues new audio files into transcription_jobs (dedup by SHA-256 checksum).
//  2. Polls for completed transcripts (status="done") and feeds them into the
//     chunk → embed → pgvector pipeline.
//
// The actual transcription is done by the external Python ASR runner (on the GPU/ASR host).
package transcribe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jedwards1230/earmark/internal/log"
)

var logger = log.NewLogger("transcribe")

// ComputeChecksum returns the SHA-256 hex digest of the file at filePath.
// This is the dedup key used in transcription_jobs.
func ComputeChecksum(filePath string) (string, error) {
	// #nosec G304 — path is from the filesystem monitor, validated by the caller
	f, err := os.Open(filepath.Clean(filePath))
	if err != nil {
		return "", fmt.Errorf("opening file for checksum: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("computing checksum: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// EnqueueJob inserts a new transcription_jobs row for filePath if no row with
// the same checksum already exists (status-agnostic dedup). Returns the job ID,
// or an empty string if the job was already present.
//
// The actual INSERT is delegated to the DB layer to keep SQL in one place.
// This function validates the checksum and delegates to the provided inserter.
func EnqueueJob(ctx context.Context, filePath string, inserter JobInserter) (jobID string, created bool, err error) {
	checksum, err := ComputeChecksum(filePath)
	if err != nil {
		return "", false, fmt.Errorf("checksum %q: %w", filePath, err)
	}

	jobID, created, err = inserter.InsertJobIfAbsent(ctx, filePath, checksum)
	if err != nil {
		return "", false, fmt.Errorf("insert job for %q: %w", filePath, err)
	}

	if created {
		logger.Info("enqueued transcription job", "file", filePath, "job_id", jobID)
	} else {
		logger.Debug("transcription job already exists", "file", filePath, "checksum", checksum)
	}
	return jobID, created, nil
}

// JobInserter is satisfied by the DB layer.
type JobInserter interface {
	// InsertJobIfAbsent inserts a pending transcription_jobs row if no row
	// with checksum already exists. Returns (jobID, true, nil) on insert or
	// (existingJobID, false, nil) if already present.
	InsertJobIfAbsent(ctx context.Context, filePath, checksum string) (jobID string, created bool, err error)
}
