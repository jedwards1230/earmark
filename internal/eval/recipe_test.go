package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/log"
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
	if r.PromptVersion != DefaultJudgePrompt.Version() || r.PromptSHA256 != DefaultJudgePrompt.sha256() {
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

	sys, user := buildPrompt(JudgePromptV2, c, pathOnlyContext)
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
		"judge@v2": "27cafa9f7d96a728078ecfb434ef3a48254d0872ce2d8fc65cfe7e48bcaee92f",
		"judge@v3": "233948020f91ccc1de9bb7d147207ba6fcfbf03529f4178432b3cf271c6f2d7d",
	}
	if DefaultJudgePrompt != JudgePromptV2 {
		t.Errorf("default judge prompt = %q, want v2 (v3 is opt-in until the A/B passes)", DefaultJudgePrompt)
	}
	for _, p := range []JudgePrompt{JudgePromptV2, JudgePromptV3} {
		want, ok := pinned[p.Version()]
		if !ok {
			t.Fatalf("judge prompt %q has no pinned hash; add %q", p.Version(), p.sha256())
		}
		if got := p.sha256(); got != want {
			t.Errorf("judge prompt changed (sha256 %s) but its version is still %q — "+
				"bump it and pin the new hash", got, p.Version())
		}
	}
	if DefaultJudgePrompt.sha256() != pinned[DefaultJudgePrompt.Version()] {
		t.Error("judgePromptSHA256 is not the default prompt's hash")
	}
}

// Pinned judge@v2 identity as origin/main built it before judge@v3 existed:
// the recipe id of NewJudge(&fakeChat{model: "earmark-judge"}) with default
// env, and the sha256 of the v2 system prompt.
const (
	pinnedV2RecipeID     = "2c33de239278f459a28b59b1857dd9dafde43bcbc2f75c0a42676e9181ba9c92"
	pinnedV2SystemSHA256 = "68d7f13c0a86ad3401366ae21aa18da11fb08cd2acc3080b17dc6424eb6b55bf"
	pinnedV2UserMessage  = "Book/track: /books/Author/Book/01.m4b\nSpan time: 10.0s–40.0s\n\nTranscript span:\nthe quick brown fox"
)

// TestJudgePromptV2Reproducible: selecting judge@v2 sends exactly what
// origin/main sent and stamps exactly its recipe — even when a book context
// is handed in — so the v2 arm of an A/B run is the old judge.
func TestJudgePromptV2Reproducible(t *testing.T) {
	t.Setenv("EVAL_MIN_CONFIDENCE", "")
	t.Setenv("EVAL_MAX_FINDINGS_PER_CHUNK", "")
	chat := &fakeChat{model: "earmark-judge", resp: `{"findings":[]}`}
	j := NewJudge(chat)
	mustSetPrompt(t, j, JudgePromptV2)

	r := j.Recipe()
	if got := mustRecipeID(t, r); got != pinnedV2RecipeID {
		t.Errorf("v2 recipe id = %s, want %s (params %v)", got, pinnedV2RecipeID, r.Params)
	}
	if r.PromptVersion != "judge@v2" || r.PromptSHA256 != "27cafa9f7d96a728078ecfb434ef3a48254d0872ce2d8fc65cfe7e48bcaee92f" {
		t.Errorf("v2 prompt identity = %s/%s", r.PromptVersion, r.PromptSHA256)
	}
	if _, ok := r.Params["book_context"]; ok {
		t.Errorf("v2 recipe has a book_context param: %v", r.Params)
	}
	sum := sha256.Sum256([]byte(systemPrompt))
	if hex.EncodeToString(sum[:]) != pinnedV2SystemSHA256 {
		t.Error("the judge@v2 system prompt changed")
	}

	res, err := j.JudgeChunkWithBook(context.Background(), sampleChunk(), mustBookContext(t, duneRecord()))
	if err != nil {
		t.Fatal(err)
	}
	if chat.lastSys != systemPrompt || chat.lastUsr != pinnedV2UserMessage {
		t.Errorf("v2 messages changed:\n got %q\nwant %q", chat.lastUsr, pinnedV2UserMessage)
	}
	if res.PromptVersion != "judge@v2" || res.BookContext != pathOnlyContext {
		t.Errorf("v2 result reports %s / %+v", res.PromptVersion, res.BookContext)
	}
}

// TestJudgePromptV3Messages: judge@v3 inserts the Book record block between
// the path and the span time; a path_only context sends v2's user message
// byte for byte (only the system prompt differs).
func TestJudgePromptV3Messages(t *testing.T) {
	c := sampleChunk()
	sys, user := buildPrompt(JudgePromptV3, c, mustBookContext(t, duneRecord()))
	want := "Book/track: /books/Author/Book/01.m4b\n" + wantDuneBlock +
		"Span time: 10.0s–40.0s\n\nTranscript span:\nthe quick brown fox"
	if sys != systemPromptV3 || user != want {
		t.Errorf("v3 user message:\n got %q\nwant %q", user, want)
	}
	sys, user = buildPrompt(JudgePromptV3, c, pathOnlyContext)
	if sys != systemPromptV3 || user != pinnedV2UserMessage {
		t.Errorf("v3 path_only user message = %q, want the v2 one", user)
	}
	if systemPromptV3 == systemPrompt || !strings.Contains(systemPromptV3, "prefer the record's spelling") {
		t.Error("judge@v3 system prompt lacks the book record rule")
	}
	if got := mustBookContext(t, duneRecord()).SHA256; got != "a63b66806f6f2c66607ef97f87af4a75c534d7724868a040661a4ff76a361123" {
		t.Errorf("fixture book_context_sha256 = %s (the rendering changed: bump bookContextVersion)", got)
	}
}

// TestJudgePromptRecipes: v3 (opt-in) records book_context and is a
// different recipe from v2 for the same model and params.
func TestJudgePromptRecipes(t *testing.T) {
	v3 := NewJudge(&fakeChat{model: "earmark-judge"})
	mustSetPrompt(t, v3, JudgePromptV3)
	r3 := v3.Recipe()
	if r3.PromptVersion != "judge@v3" || r3.PromptSHA256 != judgePromptSHA256V3() || r3.Params["book_context"] != "record@v1" {
		t.Errorf("v3 recipe = %s/%s params %v", r3.PromptVersion, r3.PromptSHA256, r3.Params)
	}
	v2 := NewJudge(&fakeChat{model: "earmark-judge"})
	mustSetPrompt(t, v2, JudgePromptV2)
	if mustRecipeID(t, r3) == mustRecipeID(t, v2.Recipe()) {
		t.Error("v2 and v3 share a recipe id")
	}
	if err := r3.Validate(); err != nil {
		t.Error(err)
	}
}

// TestZeroConfigJudgeIsV2: with no flag and no pin param every judge — a
// bare NewJudge, the worker/dashboard's NewJudgeForConfig, and the startup
// current recipe — is exactly origin/main's judge@v2 recipe.
func TestZeroConfigJudgeIsV2(t *testing.T) {
	t.Setenv("EVAL_MIN_CONFIDENCE", "")
	t.Setenv("EVAL_MAX_FINDINGS_PER_CHUNK", "")
	chat := &fakeChat{model: "earmark-judge"}
	for name, j := range map[string]*Judge{
		"NewJudge":                   NewJudge(chat),
		"NewJudgeForConfig nil cfg":  NewJudgeForConfig(chat, nil),
		"NewJudgeForConfig no param": NewJudgeForConfig(chat, &config.Config{Models: &config.ModelRegistry{Steps: map[string]config.ModelPin{}}}),
	} {
		if j.Prompt() != JudgePromptV2 {
			t.Errorf("%s: prompt %s, want v2", name, j.Prompt())
		}
		if got := mustRecipeID(t, j.Recipe()); got != pinnedV2RecipeID {
			t.Errorf("%s: recipe id %s, want origin/main's %s", name, got, pinnedV2RecipeID)
		}
	}
	t.Setenv("EVAL_CHAT_BASE_URL", "http://judge.invalid/v1")
	t.Setenv("EVAL_CHAT_MODEL", "m")
	r, ok, err := CurrentRecipe(&config.Config{})
	if err != nil || !ok || r.PromptVersion != "judge@v2" {
		t.Errorf("startup current recipe = %s (ok=%v err=%v), want judge@v2", r.PromptVersion, ok, err)
	}
}

// TestJudgePromptPinParam: the propose pin's judge_prompt param (a
// model_settings row or MODELS_FILE) selects v3; SetPrompt — what
// --judge-prompt calls — outranks it.
func TestJudgePromptPinParam(t *testing.T) {
	cfg := func(v any) *config.Config {
		return &config.Config{Models: &config.ModelRegistry{Steps: map[string]config.ModelPin{
			recipe.StepPropose: {Params: map[string]any{config.ParamJudgePrompt: v}},
		}}}
	}
	chat := &fakeChat{model: "earmark-judge"}
	for _, tt := range []struct {
		v    any
		want JudgePrompt
	}{
		{"v3", JudgePromptV3},
		{"judge@v3", JudgePromptV3},
		{"v2", JudgePromptV2},
		{"v9", JudgePromptV2}, // invalid (validated out at load): default, logged
		{3, JudgePromptV2},
	} {
		j := NewJudgeForConfig(chat, cfg(tt.v))
		if j.Prompt() != tt.want {
			t.Errorf("judge_prompt %v: prompt %s, want %s", tt.v, j.Prompt(), tt.want)
		}
	}
	j := NewJudgeForConfig(chat, cfg("v3"))
	if r := j.Recipe(); r.PromptVersion != "judge@v3" || r.Params["book_context"] != "record@v1" {
		t.Errorf("pinned v3 recipe = %s %v", r.PromptVersion, r.Params)
	}
	mustSetPrompt(t, j, JudgePromptV2) // --judge-prompt v2 over a v3 pin
	if mustRecipeID(t, j.Recipe()) != mustRecipeID(t, NewJudgeForConfig(chat, nil).Recipe()) {
		t.Error("--judge-prompt v2 over a v3 pin is not the v2 recipe")
	}
}

// TestPinnedPromptWarning: a MODELS_FILE prompt_version pin is checked
// against the prompt the judge runs, including after SetPrompt.
func TestPinnedPromptWarning(t *testing.T) {
	var buf bytes.Buffer
	j := NewJudge(&fakeChat{})
	j.logger = log.Logger{Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))}
	j.pinnedPrompt = "judge@v3"
	j.warnPinnedPrompt()
	if !strings.Contains(buf.String(), "running=judge@v2") {
		t.Errorf("no warning for a v3 pin under v2: %q", buf.String())
	}
	j.warnPinnedPrompt() // the same mismatch again (NewJudgeForConfig, then SetPrompt)
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Errorf("warned %d times for one mismatch: %q", n, buf.String())
	}
	buf.Reset()
	mustSetPrompt(t, j, JudgePromptV3)
	if buf.Len() != 0 {
		t.Errorf("warned although the judge now runs the pinned prompt: %q", buf.String())
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

func TestServedUnexpectedModel(t *testing.T) {
	tests := []struct {
		expected, reported string
		want               bool
	}{
		{"earmark-judge", "", false},
		{"earmark-judge", "earmark-judge", false},
		{"earmark-judge", "anthropic/claude-haiku-4-5-20251001", true}, // alias reported as provider id, unpinned
		{"anthropic/claude-haiku-4-5-20251001", "anthropic/claude-haiku-4-5-20251001", false},
		{"anthropic/claude-haiku-4-5-20251001", "gemini/gemini-2.5-flash", true}, // fallback
	}
	for _, tt := range tests {
		if got := servedUnexpectedModel(tt.expected, tt.reported); got != tt.want {
			t.Errorf("servedUnexpectedModel(%q, %q) = %v, want %v", tt.expected, tt.reported, got, tt.want)
		}
	}
}

// TestJudgeWarnsOnceOnUnexpectedModel: unpinned behind an alias that the
// endpoint reports as a provider id, every finding would be stamped with a
// non-current recipe (stale_work). The judge must say so — once, not per chunk.
func TestJudgeWarnsOnceOnUnexpectedModel(t *testing.T) {
	for _, tt := range []struct {
		name, reported string
		wantWarns      int
	}{
		{"reports the alias", "earmark-judge", 0},
		{"reports a provider id", "anthropic/claude-haiku-4-5-20251001", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			chat := &reportingChat{fakeChat: fakeChat{resp: `{"findings":[]}`, model: "earmark-judge"}, resolved: tt.reported}
			j := NewJudge(chat)
			var buf bytes.Buffer
			j.logger = log.Logger{Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))}
			for range 3 {
				if _, err := j.JudgeChunk(context.Background(), sampleChunk()); err != nil {
					t.Fatal(err)
				}
			}
			if got := strings.Count(buf.String(), "level=WARN"); got != tt.wantWarns {
				t.Errorf("warnings over 3 chunks = %d, want %d:\n%s", got, tt.wantWarns, buf.String())
			}
		})
	}
}

// TestCurrentRecipe_NotConfiguredVsMisconfigured: no eval endpoint at all is a
// quiet "no propose recipe", while a configured-but-invalid one surfaces its
// error so startup can warn instead of silently registering nothing.
func TestCurrentRecipe_NotConfiguredVsMisconfigured(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		model   string
		wantOK  bool
		wantErr bool
	}{
		{"not configured", "", "", false, false},
		{"misconfigured", "ftp://judge.invalid", "m", false, true},
		{"configured", "http://judge.invalid/v1", "m", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EVAL_CHAT_BASE_URL", tc.baseURL)
			t.Setenv("EVAL_CHAT_MODEL", tc.model)
			r, ok, err := CurrentRecipe(&config.Config{})
			if ok != tc.wantOK || (err != nil) != tc.wantErr {
				t.Fatalf("ok=%v err=%v, want ok=%v err=%v", ok, err, tc.wantOK, tc.wantErr)
			}
			if ok && r.Step != recipe.StepPropose {
				t.Errorf("step = %q, want %q", r.Step, recipe.StepPropose)
			}
		})
	}
}
