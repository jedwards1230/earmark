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

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/log"
)

// ChatClient is the small abstraction over the chat-LLM endpoint the judge
// needs. Implemented by openAIChatClient (OpenAI-compatible /v1/chat/completions)
// and faked in tests. Keeping the judge behind this interface means swapping the
// endpoint binding to #48's AI endpoint registry is a one-line change at the
// resolution point (see ResolveChatClient), not a rewrite of the judge.
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
}

// NewJudge constructs a Judge backed by the given chat client.
func NewJudge(chat ChatClient) *Judge {
	return &Judge{chat: chat, logger: log.NewLogger("eval")}
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

// optionalStr maps an empty string to nil (NULL), else a pointer to the value.
func optionalStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
