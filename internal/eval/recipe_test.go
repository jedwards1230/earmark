package eval

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// reportingChat is a fakeChat that also reports which model served the reply.
type reportingChat struct {
	fakeChat
	resolved string
}

func (r *reportingChat) CompleteWithModel(ctx context.Context, system, user string) (Completion, error) {
	content, err := r.Complete(ctx, system, user)
	return Completion{Content: content, ResolvedModel: r.resolved}, err
}

const anchoredFinding = `{"findings":[{"original_text":"ganema","issue_type":"misheard_proper_noun",` +
	`"suggested_correction":"ghanima","confidence":0.9,"anchor_offset":0,"anchor_occurrence":0}]}`

// TestJudgeRecipe: the judge's current recipe carries what it asks for, what
// is expected to answer, the prompt identity, and its output-shaping params.
func TestJudgeRecipe(t *testing.T) {
	j := NewJudge(&fakeChat{model: "earmark-judge"})
	r := j.Recipe()
	if r.Step != recipe.StepPropose || r.StepVersion != proposeStepVersion {
		t.Errorf("step = %s v%d", r.Step, r.StepVersion)
	}
	if r.ModelAlias != "earmark-judge" || r.ModelResolved != "earmark-judge" {
		t.Errorf("unpinned judge: alias %q resolved %q, want both earmark-judge", r.ModelAlias, r.ModelResolved)
	}
	if r.PromptVersion != judgePromptVersion || r.PromptSHA256 != judgePromptSHA256() {
		t.Errorf("prompt identity = %s/%s", r.PromptVersion, r.PromptSHA256)
	}
	for _, k := range []string{"temperature", "min_confidence", "max_findings_per_chunk"} {
		if _, ok := r.Params[k]; !ok {
			t.Errorf("params missing %q: %v", k, r.Params)
		}
	}
	if err := r.Validate(); err != nil {
		t.Error(err)
	}

	j.SetModelPin(ModelPin{ExpectedModel: "anthropic/claude-haiku-4-5-20251001", Revision: "20251001"})
	p := j.Recipe()
	if p.ModelResolved != "anthropic/claude-haiku-4-5-20251001" || p.ModelRevision != "20251001" {
		t.Errorf("pinned judge: resolved %q revision %q", p.ModelResolved, p.ModelRevision)
	}
	if mustRecipeID(t, p) == mustRecipeID(t, r) {
		t.Error("a registry pin did not change the recipe")
	}
}

// TestJudgeChunkStampsWhatAnswered: every finding carries the recipe that
// actually produced it. When the endpoint reports the expected model, that is
// the current recipe; when a fallback answers, it is a different one.
func TestJudgeChunkStampsWhatAnswered(t *testing.T) {
	const expected = "anthropic/claude-haiku-4-5-20251001"
	tests := []struct {
		name        string
		resolved    string
		wantCurrent bool
		wantModel   string
	}{
		{"expected model answered", expected, true, expected},
		{"endpoint reported nothing", "", true, expected},
		{"fallback answered", "gemini/gemini-2.5-flash", false, "gemini/gemini-2.5-flash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chat := &reportingChat{fakeChat: fakeChat{model: "earmark-judge", resp: anchoredFinding}, resolved: tt.resolved}
			j := NewJudge(chat)
			j.SetModelPin(ModelPin{ExpectedModel: expected})
			res, err := j.JudgeChunk(context.Background(), db.EvalChunk{
				ChunkID: "c", TranscriptID: "t", FilePath: "/b/x.m4b", Text: "ganema said",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Findings) != 1 {
				t.Fatalf("got %d findings", len(res.Findings))
			}
			f := res.Findings[0]
			if f.Recipe == nil {
				t.Fatal("finding has no recipe")
			}
			if f.Recipe.ModelResolved != tt.wantModel || f.Recipe.ModelAlias != "earmark-judge" {
				t.Errorf("recipe alias/resolved = %q/%q, want earmark-judge/%q",
					f.Recipe.ModelAlias, f.Recipe.ModelResolved, tt.wantModel)
			}
			isCurrent := mustRecipeID(t, *f.Recipe) == mustRecipeID(t, j.Recipe())
			if isCurrent != tt.wantCurrent {
				t.Errorf("finding recipe is current = %v, want %v", isCurrent, tt.wantCurrent)
			}
		})
	}
}

// TestBuildPromptBytesUnchanged: moving the user message into
// userPromptTemplate (so it can be hashed) must not change a byte sent.
func TestBuildPromptBytesUnchanged(t *testing.T) {
	c := db.EvalChunk{FilePath: "/books/A/B/01.m4b", StartSec: 12.345, EndSec: 678.9, Text: "the span text"}
	// The pre-recipe buildPrompt, verbatim.
	var b strings.Builder
	fmt.Fprintf(&b, "Book/track: %s\n", c.FilePath)
	fmt.Fprintf(&b, "Span time: %.1fs–%.1fs\n\n", c.StartSec, c.EndSec)
	b.WriteString("Transcript span:\n")
	b.WriteString(c.Text)

	sys, user := buildPrompt(c)
	if user != b.String() {
		t.Errorf("user prompt changed:\n got %q\nwant %q", user, b.String())
	}
	if sys != systemPrompt {
		t.Error("system prompt is not systemPrompt")
	}
}

// TestJudgePromptVersionPinned forces a conscious version bump: if the system
// prompt, user template or response schema changes, the hash changes, and this
// fails until judgePromptVersion is bumped and the new pair recorded here.
func TestJudgePromptVersionPinned(t *testing.T) {
	pinned := map[string]string{
		"judge@v1": "3ed9ca07704aa769150724f487a7c35012c170419eb8fe33596b3a4e59fc927b",
	}
	want, ok := pinned[judgePromptVersion]
	if !ok {
		t.Fatalf("judgePromptVersion %q has no pinned hash; add %q", judgePromptVersion, judgePromptSHA256())
	}
	if got := judgePromptSHA256(); got != want {
		t.Errorf("judge prompt changed (sha256 %s) but judgePromptVersion is still %q — "+
			"bump it and pin the new hash", got, judgePromptVersion)
	}
}

func mustRecipeID(t *testing.T, r recipe.Recipe) string {
	t.Helper()
	id, err := r.ID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
