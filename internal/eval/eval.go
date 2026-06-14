// Package eval implements the read-only LLM-as-judge eval layer (CONTRACT
// §2.15, GitHub issue #49).
//
// The judge READS transcript chunks and records suspected transcription errors
// as advisory metadata in transcript_findings. It NEVER edits transcripts: this
// package issues no UPDATE/DELETE/ALTER against transcripts/segments/
// transcript_chunks. The asymmetry is the whole point — a wrong flag is
// harmless, a wrong correction would corrupt the corpus, so corrections
// (suggested_correction) are recorded but never applied.
//
// Cost is operator-bounded: the judge runs on-demand per book or over a random
// sample of N chunks (never every segment of every book), behind a dry-run gate.
package eval

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/log"
)

// defaultMaxFindingsPerChunk bounds how many findings the judge keeps for a
// single chunk. The judge over-flags in practice (one chunk produced 31;
// llama3.2:3b and qwen2.5:7b both averaged ~3/chunk over a 50-chunk sample, but
// the tail is long), so a per-chunk cap keeps the highest-confidence signal and
// drops the noisy remainder. Tunable via EVAL_MAX_FINDINGS_PER_CHUNK; a value
// <= 0 disables the cap.
const defaultMaxFindingsPerChunk = 8

// maxFindingsPerChunk resolves the per-chunk cap from EVAL_MAX_FINDINGS_PER_CHUNK,
// falling back to defaultMaxFindingsPerChunk. A blank/invalid value uses the
// default; an explicit <= 0 disables capping (returned as 0).
func maxFindingsPerChunk() int {
	raw := os.Getenv("EVAL_MAX_FINDINGS_PER_CHUNK")
	if raw == "" {
		return defaultMaxFindingsPerChunk
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultMaxFindingsPerChunk
	}
	if n < 0 {
		return 0
	}
	return n
}

// ChatClient is the small abstraction over the chat-LLM endpoint the judge
// needs. Implemented by openAIChatClient (OpenAI-compatible /v1/chat/completions)
// and faked in tests. Keeping the judge behind this interface means the endpoint
// binding (AI registry vs env-var fallback) is resolved in one place
// (ResolveChatClient) rather than threaded through the judge.
type ChatClient interface {
	// Complete sends a system + user prompt and returns the model's raw text
	// response. The judge is responsible for parsing JSON out of it.
	Complete(ctx context.Context, system, user string) (string, error)
	// Model returns the judge model id, recorded on each finding for
	// attribution.
	Model() string
}

// ChunkReader is the read-only slice of the DB the judge needs to fetch chunks.
// Intentionally read-only — there is no transcript-mutating method here.
type ChunkReader interface {
	GetEvalChunksForBook(ctx context.Context, dir string, limit int) ([]db.EvalChunk, error)
	SampleEvalChunks(ctx context.Context, limit int) ([]db.EvalChunk, error)
}

// FindingWriter is the insert-only slice of the DB the judge writes through.
// Insert-only by construction — no update/delete.
type FindingWriter interface {
	InsertFindings(ctx context.Context, findings []db.Finding) error
}

// Judge runs the LLM-as-judge over chunks and turns its output into findings.
type Judge struct {
	chat   ChatClient
	logger log.Logger
	// maxPerChunk caps findings kept per chunk (highest-confidence first); 0
	// disables the cap. Resolved once from EVAL_MAX_FINDINGS_PER_CHUNK at
	// construction so a single value applies for the whole run.
	maxPerChunk int
}

// NewJudge constructs a Judge backed by the given chat client.
func NewJudge(chat ChatClient) *Judge {
	return &Judge{chat: chat, logger: log.NewLogger("eval"), maxPerChunk: maxFindingsPerChunk()}
}

// Result is the outcome of judging one chunk: the findings derived from it and
// any error (the caller decides whether to abort the run or skip the chunk).
type Result struct {
	Chunk    db.EvalChunk
	Findings []db.Finding
}

// JudgeChunk evaluates a single chunk: build the prompt, call the model, parse
// the response, and map suspected errors into db.Finding rows (attributed to the
// chunk, its transcript, and its run). It performs NO database writes — the
// caller persists via FindingWriter only when not in dry-run.
func (j *Judge) JudgeChunk(ctx context.Context, c db.EvalChunk) (Result, error) {
	system, user := buildPrompt(c)
	raw, err := j.chat.Complete(ctx, system, user)
	if err != nil {
		return Result{Chunk: c}, fmt.Errorf("judge chunk %s: %w", c.ChunkID, err)
	}

	parsed, perr := parseFindings(raw)
	if perr != nil {
		// A malformed judge response is a soft failure: log and treat the chunk
		// as "no findings" rather than aborting the whole run. The judge is
		// advisory; a parse miss costs nothing.
		j.logger.Warn("dropping unparseable judge response",
			"chunk_id", c.ChunkID, "error", perr)
		return Result{Chunk: c}, nil
	}

	parsed = j.capFindings(c, parsed)

	model := j.chat.Model()
	findings := make([]db.Finding, 0, len(parsed))
	chunkID := c.ChunkID
	chunkIndex := c.ChunkIndex
	runID := c.TranscriptionRunID
	for _, p := range parsed {
		findings = append(findings, db.Finding{
			TranscriptID:        c.TranscriptID,
			FilePath:            c.FilePath,
			ChunkID:             &chunkID,
			ChunkIndex:          &chunkIndex,
			StartSec:            c.StartSec,
			EndSec:              c.EndSec,
			OriginalText:        p.OriginalText,
			IssueType:           p.IssueType,
			SuggestedCorrection: optionalStr(p.SuggestedCorrection),
			Confidence:          p.Confidence,
			Model:               model,
			TranscriptionRunID:  optionalStr(runID),
		})
	}
	return Result{Chunk: c, Findings: findings}, nil
}

// capFindings bounds a single chunk's findings to j.maxPerChunk, keeping the
// highest-confidence ones (the judge over-flags, and a wrong flag is only noise
// — so when forced to drop, drop the least-confident). It sorts by confidence
// descending (stable, so equal-confidence findings keep their original order
// for a deterministic result) and truncates. A cap of 0 (disabled) or a set
// already within the cap is returned unchanged. Logs at DEBUG when it truncates.
func (j *Judge) capFindings(c db.EvalChunk, parsed []parsedFinding) []parsedFinding {
	if j.maxPerChunk <= 0 || len(parsed) <= j.maxPerChunk {
		return parsed
	}
	sort.SliceStable(parsed, func(a, b int) bool {
		return parsed[a].Confidence > parsed[b].Confidence
	})
	dropped := len(parsed) - j.maxPerChunk
	j.logger.Debug("capping over-flagged chunk findings",
		"chunk_id", c.ChunkID, "kept", j.maxPerChunk, "dropped", dropped, "cap", j.maxPerChunk)
	return parsed[:j.maxPerChunk]
}

// optionalStr maps an empty string to nil (NULL), else a pointer to the value.
func optionalStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
