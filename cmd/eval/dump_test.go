package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	evalpkg "github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/recipe"
)

func TestRun_SeedAndDumpFlags(t *testing.T) {
	t.Run("seed passes through", func(t *testing.T) {
		f := &fakeRunner{}
		var out strings.Builder
		called := false
		if err := run(context.Background(), &out, f, "", options{sample: 5, seed: "q4",
			observe: func(evalpkg.Result, error) { called = true }}); err != nil {
			t.Fatal(err)
		}
		if f.gotOpts.Seed != "q4" || f.gotOpts.Sample != 5 || f.gotOpts.Observe == nil {
			t.Errorf("opts = %+v", f.gotOpts)
		}
		f.gotOpts.Observe(evalpkg.Result{}, nil)
		if !called {
			t.Error("Observe is not the dump hook")
		}
		if !strings.Contains(out.String(), `seed "q4"`) {
			t.Errorf("report does not name the seed:\n%s", out.String())
		}
	})
	for _, tc := range []struct {
		name string
		book string
		o    options
	}{
		{"seed without sample", "Dune", options{seed: "q4"}},
		{"dump with write", "", options{sample: 5, dump: "/tmp/x.jsonl", write: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{}
			var out strings.Builder
			if err := run(context.Background(), &out, f, tc.book, tc.o); err == nil {
				t.Fatal("accepted")
			}
			if f.called {
				t.Error("runner called")
			}
		})
	}
}

func TestRun_ReportsDrops(t *testing.T) {
	f := &fakeRunner{stats: evalpkg.RunStats{ChunksEvaluated: 2,
		Dropped: map[string]int{evalpkg.DropBelowMinConfidence: 2, evalpkg.DropCosmeticOnly: 5, evalpkg.DropOverCap: 2}}}
	var out strings.Builder
	if err := run(context.Background(), &out, f, "", options{sample: 2}); err != nil {
		t.Fatal(err)
	}
	if want := "Dropped before recording: cosmetic_only 5, below_min_confidence 2, over_cap 2."; !strings.Contains(out.String(), want) {
		t.Errorf("missing %q in:\n%s", want, out.String())
	}
}

// TestDumper: one JSON line per chunk with ids, prompt identity, usage, cost,
// kept and dropped findings; unreported usage/cost are null, not zero; a
// failed chunk carries its (bounded) error.
func TestDumper(t *testing.T) {
	var buf bytes.Buffer
	rec := recipe.Recipe{PromptVersion: "judge@v9", PromptSHA256: "abc", StepVersion: 3}
	d := newDumper(&buf, "earmark-judge", rec)
	corr := "folks"
	d.observe(evalpkg.Result{
		Chunk:         db.EvalChunk{ChunkID: "c1", TranscriptID: "t1", FilePath: "/b/x.m4b", ChunkIndex: 4, Text: "héllo fox"},
		ResolvedModel: "anthropic/claude-haiku-5-5",
		Usage:         evalpkg.Usage{InputTokens: 1500, OutputTokens: 40, HasUsage: true, CostUSD: 0.002, HasCost: true},
		Elapsed:       1500 * time.Millisecond,
		Findings:      []db.Finding{{OriginalText: "fox", SuggestedCorrection: &corr, IssueType: "misheard_word", Confidence: 0.8}},
		Dropped:       []evalpkg.Dropped{{Reason: evalpkg.DropCosmeticOnly, OriginalText: "a", SuggestedCorrection: "A", IssueType: "misheard_word", Confidence: 0.9}},
	}, nil)
	d.observe(evalpkg.Result{Chunk: db.EvalChunk{ChunkID: "c2"}}, errors.New(strings.Repeat("x", 1000)))
	if d.err != nil {
		t.Fatal(d.err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2:\n%s", len(lines), buf.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]any{
		"chunk_id": "c1", "transcript_id": "t1", "chunk_index": 4.0, "text_chars": 9.0,
		"prompt_version": "judge@v9", "prompt_sha256": "abc", "step_version": 3.0, "model": "earmark-judge",
		"resolved_model": "anthropic/claude-haiku-5-5", "prompt_tokens": 1500.0, "completion_tokens": 40.0,
		"cost_usd": 0.002, "latency_ms": 1500.0,
	} {
		if first[k] != v {
			t.Errorf("%s = %v, want %v", k, first[k], v)
		}
	}
	if fs := first["findings"].([]any); len(fs) != 1 || fs[0].(map[string]any)["correction"] != "folks" {
		t.Errorf("findings = %v", first["findings"])
	}
	if ds := first["dropped"].([]any); len(ds) != 1 || ds[0].(map[string]any)["reason"] != evalpkg.DropCosmeticOnly {
		t.Errorf("dropped = %v", first["dropped"])
	}

	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"prompt_tokens", "completion_tokens", "cost_usd"} {
		if v, ok := second[k]; !ok || v != nil {
			t.Errorf("%s = %v (present %v), want null", k, v, ok)
		}
	}
	if fs, ok := second["findings"].([]any); !ok || len(fs) != 0 {
		t.Errorf("failed chunk findings = %v, want []", second["findings"])
	}
	if e, _ := second["error"].(string); e == "" || len([]rune(e)) > maxDumpErrorRunes+1 {
		t.Errorf("error = %d runes, want 1..%d", len([]rune(e)), maxDumpErrorRunes+1)
	}
}
