package eval

import (
	"context"
	"errors"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
)

// scriptedChat returns a per-call response or error, advancing through `steps`
// on each Complete. It lets a test inject a transient error on one chunk while
// the others succeed.
type scriptedChat struct {
	steps []step
	n     int
}

type step struct {
	resp string
	err  error
}

func (s *scriptedChat) Complete(_ context.Context, _, _ string) (string, error) {
	st := s.steps[s.n]
	s.n++
	return st.resp, st.err
}
func (s *scriptedChat) Model() string { return "scripted-judge" }

func threeChunks() []db.EvalChunk {
	out := make([]db.EvalChunk, 3)
	for i := range out {
		out[i] = db.EvalChunk{
			ChunkID:      string(rune('a' + i)),
			TranscriptID: "tr",
			FilePath:     "/books/Author/Book/01.m4b",
			Text:         "span",
		}
	}
	return out
}

const oneFinding = `{"findings":[{"original_text":"span","issue_type":"misheard_word","suggested_correction":"spawn","confidence":0.8}]}`

// A transient judge error mid-stream must NOT discard the run: the bad chunk is
// skipped (counted) and the surrounding successes are still collected.
func TestRun_TransientJudgeErrorSkipsChunkAndContinues(t *testing.T) {
	chat := &scriptedChat{steps: []step{
		{resp: oneFinding}, // chunk a → 1 finding
		{err: errors.New("connection reset by peer")}, // chunk b → transient error
		{resp: oneFinding},                            // chunk c → 1 finding
	}}
	reader := fakeReader{chunks: threeChunks()}
	writer := &fakeWriter{}

	findings, stats, err := Run(context.Background(), reader, NewJudge(chat), writer, RunOptions{Book: "Book", Write: true})
	if err != nil {
		t.Fatalf("transient error must not abort the run, got: %v", err)
	}
	if len(findings) != 2 {
		t.Fatalf("want 2 findings (skipped chunk excluded), got %d", len(findings))
	}
	if stats.ChunksEvaluated != 2 || stats.ChunksSkipped != 1 {
		t.Errorf("stats = evaluated %d / skipped %d; want 2 / 1", stats.ChunksEvaluated, stats.ChunksSkipped)
	}
	if !stats.Persisted || len(writer.inserted) != 2 {
		t.Errorf("partial findings should still persist: persisted=%v inserted=%d", stats.Persisted, len(writer.inserted))
	}
}

// A cancelled context aborts the run, but returns the findings collected so far
// rather than discarding them.
func TestRun_ContextCancelAbortsButReturnsPartial(t *testing.T) {
	chat := &scriptedChat{steps: []step{
		{resp: oneFinding},      // chunk a → 1 finding
		{err: context.Canceled}, // chunk b → cancellation
		{resp: oneFinding},      // chunk c → never reached
	}}
	reader := fakeReader{chunks: threeChunks()}
	writer := &fakeWriter{}

	findings, stats, err := Run(context.Background(), reader, NewJudge(chat), writer, RunOptions{Book: "Book", Write: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding (collected before cancel), got %d", len(findings))
	}
	if stats.FindingsFound != 1 {
		t.Errorf("stats.FindingsFound = %d, want 1", stats.FindingsFound)
	}
	// On abort we do not persist (the caller asked to stop).
	if stats.Persisted || len(writer.inserted) != 0 {
		t.Errorf("aborted run must not persist: persisted=%v inserted=%d", stats.Persisted, len(writer.inserted))
	}
}
