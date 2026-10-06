package prunechunks

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

func TestClassify(t *testing.T) {
	exp := []string{"a", "b"}
	sh := func(pairs ...any) []db.StoredChunkHash {
		var out []db.StoredChunkHash
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, db.StoredChunkHash{ChunkIndex: pairs[i].(int), SHA256: pairs[i+1].(string)})
		}
		return out
	}
	tests := []struct {
		name   string
		stored []db.StoredChunkHash
		want   layout
		tail   int
	}{
		{"exact", sh(0, "a", 1, "b"), layoutMatch, 0},
		{"rebuilt head, old tail", sh(0, "a", 1, "b", 2, "x", 3, "y"), layoutOrphans, 2},
		// Embedded under a different chunking: the extra rows are real coverage.
		{"different chunking", sh(0, "p", 1, "q", 2, "r"), layoutDrift, 1},
		{"head mismatch only", sh(0, "a", 1, "z"), layoutDrift, 0},
		{"hole in head plus tail", sh(0, "a", 2, "x"), layoutDrift, 1},
		{"short", sh(0, "a"), layoutShort, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, tail := classify(exp, tc.stored)
			if got != tc.want || tail != tc.tail {
				t.Errorf("classify = (%v, %d), want (%v, %d)", got, tail, tc.want, tc.tail)
			}
		})
	}
}

type fakePruner struct {
	transcripts []*db.Transcript
	stored      map[string][]db.StoredChunkHash
	pruned      map[string]int // transcript → keep
	pages       int
}

func (f *fakePruner) GetChunkedTranscripts(_ context.Context, after db.TranscriptCursor, limit int) ([]*db.Transcript, error) {
	f.pages++
	start := 0
	if after.ID != "" {
		for i, t := range f.transcripts {
			if t.ID == after.ID {
				start = i + 1
			}
		}
	}
	out := f.transcripts[start:min(start+limit, len(f.transcripts))]
	return out, nil
}

func (f *fakePruner) GetStoredChunkHashes(_ context.Context, id string) ([]db.StoredChunkHash, error) {
	return f.stored[id], nil
}

func (f *fakePruner) PruneChunks(_ context.Context, id string, keep int) (db.PruneResult, error) {
	if f.pruned == nil {
		f.pruned = map[string]int{}
	}
	f.pruned[id] = keep
	n := 0
	for _, s := range f.stored[id] {
		if s.ChunkIndex >= keep {
			n++
		}
	}
	return db.PruneResult{Chunks: n, Findings: 1}, nil
}

// newFixture builds three one-chunk transcripts: one matching, one with an
// orphan tail, one embedded under a different chunking.
func newFixture(t *testing.T) *fakePruner {
	t.Helper()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := &fakePruner{stored: map[string][]db.StoredChunkHash{}}
	for i, id := range []string{"t-match", "t-orphan", "t-drift"} {
		tr := &db.Transcript{ID: id, FilePath: "/books/" + id + ".m4b", RawText: "hello world " + id,
			CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		exp, err := expectedHashes(tr, 512)
		if err != nil || len(exp) != 1 {
			t.Fatalf("fixture re-chunk: %v (%d chunks)", err, len(exp))
		}
		f.transcripts = append(f.transcripts, tr)
		switch id {
		case "t-match":
			f.stored[id] = []db.StoredChunkHash{{ChunkIndex: 0, SHA256: exp[0]}}
		case "t-orphan":
			f.stored[id] = []db.StoredChunkHash{{ChunkIndex: 0, SHA256: exp[0]}, {ChunkIndex: 1, SHA256: "old"}, {ChunkIndex: 2, SHA256: "old2"}}
		case "t-drift":
			f.stored[id] = []db.StoredChunkHash{{ChunkIndex: 0, SHA256: "other"}, {ChunkIndex: 1, SHA256: "other2"}}
		}
	}
	return f
}

func TestRun_DryRunNeverPrunes(t *testing.T) {
	f := newFixture(t)
	var out strings.Builder
	if err := run(context.Background(), &out, f, 512, options{pageSize: 2}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(f.pruned) != 0 {
		t.Errorf("dry-run pruned %v", f.pruned)
	}
	s := out.String()
	for _, want := range []string{"1 match, 1 with orphans (2 orphan chunk(s)), 1 drift", "(dry-run) pass --yes"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

// TestRun_YesPrunesOnlyOrphans: only the clean-rebuild signature is pruned,
// against the re-chunk's length; a drifted transcript is never touched.
func TestRun_YesPrunesOnlyOrphans(t *testing.T) {
	f := newFixture(t)
	var out strings.Builder
	if err := run(context.Background(), &out, f, 512, options{yes: true, pageSize: 2}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(f.pruned) != 1 || f.pruned["t-orphan"] != 1 {
		t.Errorf("want only t-orphan pruned to keep=1, got %v", f.pruned)
	}
	if !strings.Contains(out.String(), "Pruned 2 chunk(s); retired 1 finding(s)") {
		t.Errorf("summary missing:\n%s", out.String())
	}
	// 3 transcripts at pageSize 2: a full page, then a short one ends the walk.
	if f.pages != 2 {
		t.Errorf("want 2 page queries, got %d", f.pages)
	}
}
