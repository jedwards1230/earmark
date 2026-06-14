package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
)

// fakeChat returns a canned response (or error) and records the prompts it saw.
type fakeChat struct {
	resp    string
	err     error
	model   string
	lastSys string
	lastUsr string
}

func (f *fakeChat) Complete(_ context.Context, system, user string) (string, error) {
	f.lastSys, f.lastUsr = system, user
	return f.resp, f.err
}
func (f *fakeChat) Model() string {
	if f.model == "" {
		return "fake-judge"
	}
	return f.model
}

func sampleChunk() db.EvalChunk {
	return db.EvalChunk{
		ChunkID:            "chunk-1",
		TranscriptID:       "tr-1",
		TranscriptionRunID: "job-1",
		FilePath:           "/books/Author/Book/01.m4b",
		ChunkIndex:         3,
		StartSec:           10,
		EndSec:             40,
		Text:               "the quick brown fox",
	}
}

func TestJudgeChunk_MapsFindings(t *testing.T) {
	chat := &fakeChat{resp: `{"findings":[{"original_text":"fox","issue_type":"homophone","suggested_correction":"folks","confidence":0.7}]}`}
	j := NewJudge(chat)
	res, err := j.JudgeChunk(context.Background(), sampleChunk())
	if err != nil {
		t.Fatalf("JudgeChunk: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(res.Findings))
	}
	f := res.Findings[0]
	if f.TranscriptID != "tr-1" || f.FilePath != "/books/Author/Book/01.m4b" {
		t.Errorf("addressing not propagated: %+v", f)
	}
	if f.ChunkID == nil || *f.ChunkID != "chunk-1" {
		t.Errorf("chunk id not propagated: %+v", f.ChunkID)
	}
	if f.TranscriptionRunID == nil || *f.TranscriptionRunID != "job-1" {
		t.Errorf("run id (backend attribution) not propagated: %+v", f.TranscriptionRunID)
	}
	if f.Model != "fake-judge" {
		t.Errorf("model not recorded: %q", f.Model)
	}
	if f.SuggestedCorrection == nil || *f.SuggestedCorrection != "folks" {
		t.Errorf("suggested correction not propagated: %+v", f.SuggestedCorrection)
	}
}

func TestJudgeChunk_MalformedResponseIsSoftFailure(t *testing.T) {
	// A garbage response must NOT abort — it yields zero findings, no error.
	chat := &fakeChat{resp: "I cannot help with that."}
	j := NewJudge(chat)
	res, err := j.JudgeChunk(context.Background(), sampleChunk())
	if err != nil {
		t.Fatalf("malformed response should be soft failure, got error: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("want 0 findings for malformed response, got %d", len(res.Findings))
	}
}

// manyFindingsJSON builds a judge response with n findings whose confidence
// descends from 0.99 (finding 0) toward 0, so the highest-confidence findings
// are easy to identify by their original_text marker.
func manyFindingsJSON(n int) string {
	var b strings.Builder
	b.WriteString(`{"findings":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		conf := 0.99 - float64(i)*0.01
		fmt.Fprintf(&b, `{"original_text":"finding-%02d","issue_type":"other","suggested_correction":"","confidence":%.2f}`, i, conf)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestJudgeChunk_CapsPerChunkKeepingHighestConfidence(t *testing.T) {
	// Default cap is 8. Feed 12 findings (confidence 0.99 desc) and expect the
	// top 8 by confidence (finding-00..finding-07) to survive.
	t.Setenv("EVAL_MAX_FINDINGS_PER_CHUNK", "") // exercise the default
	chat := &fakeChat{resp: manyFindingsJSON(12)}
	j := NewJudge(chat)
	res, err := j.JudgeChunk(context.Background(), sampleChunk())
	if err != nil {
		t.Fatalf("JudgeChunk: %v", err)
	}
	if len(res.Findings) != defaultMaxFindingsPerChunk {
		t.Fatalf("want %d findings after cap, got %d", defaultMaxFindingsPerChunk, len(res.Findings))
	}
	// The retained set must be the highest-confidence ones: every kept finding's
	// confidence >= every dropped one's. finding-00 (0.99) must be present;
	// finding-11 (0.88, the lowest) must be gone.
	kept := map[string]bool{}
	for _, f := range res.Findings {
		kept[f.OriginalText] = true
		if f.Confidence < 0.99-float64(defaultMaxFindingsPerChunk-1)*0.01-1e-9 {
			t.Errorf("kept a low-confidence finding %q (conf %.2f) — cap must keep the highest", f.OriginalText, f.Confidence)
		}
	}
	if !kept["finding-00"] {
		t.Error("highest-confidence finding (finding-00) was dropped")
	}
	if kept["finding-11"] {
		t.Error("lowest-confidence finding (finding-11) was kept — should have been truncated")
	}
}

func TestJudgeChunk_CapEnvOverrideAndDisable(t *testing.T) {
	// Override the cap to 2.
	t.Setenv("EVAL_MAX_FINDINGS_PER_CHUNK", "2")
	chat := &fakeChat{resp: manyFindingsJSON(5)}
	if res, err := NewJudge(chat).JudgeChunk(context.Background(), sampleChunk()); err != nil {
		t.Fatalf("JudgeChunk: %v", err)
	} else if len(res.Findings) != 2 {
		t.Fatalf("want 2 findings with cap=2, got %d", len(res.Findings))
	}

	// A negative value disables the cap entirely (all findings retained).
	t.Setenv("EVAL_MAX_FINDINGS_PER_CHUNK", "-1")
	chat2 := &fakeChat{resp: manyFindingsJSON(5)}
	if res, err := NewJudge(chat2).JudgeChunk(context.Background(), sampleChunk()); err != nil {
		t.Fatalf("JudgeChunk: %v", err)
	} else if len(res.Findings) != 5 {
		t.Fatalf("want 5 findings with cap disabled, got %d", len(res.Findings))
	}
}

func TestJudgeChunk_ChatErrorPropagates(t *testing.T) {
	chat := &fakeChat{err: errors.New("endpoint down")}
	j := NewJudge(chat)
	if _, err := j.JudgeChunk(context.Background(), sampleChunk()); err == nil {
		t.Fatal("expected error when chat client fails")
	}
}

// fakeReader / fakeWriter exercise the Run orchestration.
type fakeReader struct{ chunks []db.EvalChunk }

func (f fakeReader) GetEvalChunksForBook(context.Context, string, int) ([]db.EvalChunk, error) {
	return f.chunks, nil
}
func (f fakeReader) SampleEvalChunks(context.Context, int) ([]db.EvalChunk, error) {
	return f.chunks, nil
}

type fakeWriter struct{ inserted []db.Finding }

func (f *fakeWriter) InsertFindings(_ context.Context, findings []db.Finding) error {
	f.inserted = append(f.inserted, findings...)
	return nil
}

func TestRun_DryRunDoesNotPersist(t *testing.T) {
	reader := fakeReader{chunks: []db.EvalChunk{sampleChunk()}}
	chat := &fakeChat{resp: `{"findings":[{"original_text":"fox","issue_type":"other","confidence":0.5}]}`}
	writer := &fakeWriter{}

	findings, stats, err := Run(context.Background(), reader, NewJudge(chat), writer, RunOptions{Book: "Book", Write: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want 1 finding surfaced for preview, got %d", len(findings))
	}
	if stats.Persisted {
		t.Error("dry-run must not persist")
	}
	if len(writer.inserted) != 0 {
		t.Errorf("dry-run wrote %d findings; want 0", len(writer.inserted))
	}
}

func TestRun_WritePersists(t *testing.T) {
	reader := fakeReader{chunks: []db.EvalChunk{sampleChunk()}}
	chat := &fakeChat{resp: `{"findings":[{"original_text":"fox","issue_type":"other","confidence":0.5}]}`}
	writer := &fakeWriter{}

	_, stats, err := Run(context.Background(), reader, NewJudge(chat), writer, RunOptions{Book: "Book", Write: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !stats.Persisted || len(writer.inserted) != 1 {
		t.Errorf("write mode did not persist: persisted=%v inserted=%d", stats.Persisted, len(writer.inserted))
	}
}

func TestRun_RequiresScope(t *testing.T) {
	reader := fakeReader{}
	_, _, err := Run(context.Background(), reader, NewJudge(&fakeChat{}), &fakeWriter{}, RunOptions{})
	if err == nil {
		t.Fatal("expected error when neither book nor sample is given")
	}
}

// fakeSource is a tiny EvalEndpointSource for the resolver tests — no full
// config build, no import cycle.
type fakeSource struct {
	ep EvalEndpoint
	ok bool
}

func (f fakeSource) EvalEndpoint() (EvalEndpoint, bool) { return f.ep, f.ok }

func TestResolveChatClient_MissingConfig(t *testing.T) {
	t.Setenv("EVAL_CHAT_BASE_URL", "")
	t.Setenv("EVAL_CHAT_MODEL", "")
	// neither a registry eval role nor the env-var fallback → clear error.
	if _, err := ResolveChatClient(nil); err == nil {
		t.Fatal("expected error when chat endpoint env vars are unset and no eval role")
	}
}

func TestResolveChatClient_OK(t *testing.T) {
	t.Setenv("EVAL_CHAT_BASE_URL", "http://vllm:8000/v1")
	t.Setenv("EVAL_CHAT_MODEL", "judge-model")
	c, err := ResolveChatClient(nil)
	if err != nil {
		t.Fatalf("ResolveChatClient: %v", err)
	}
	if c.Model() != "judge-model" {
		t.Errorf("model = %q", c.Model())
	}
}

// When the registry binds an eval role, the resolver uses that endpoint and
// IGNORES the EVAL_CHAT_* env vars (registry wins).
func TestResolveChatClient_RegistryRoleWins(t *testing.T) {
	t.Setenv("EVAL_CHAT_BASE_URL", "http://env-fallback:9999/v1")
	t.Setenv("EVAL_CHAT_MODEL", "env-model")
	src := fakeSource{ok: true, ep: EvalEndpoint{
		BaseURL: "http://registry-vllm:8000/v1",
		Model:   "registry-judge",
		Options: map[string]string{"apiKey": "sk-test"},
	}}
	c, err := ResolveChatClient(src)
	if err != nil {
		t.Fatalf("ResolveChatClient: %v", err)
	}
	if c.Model() != "registry-judge" {
		t.Errorf("model = %q; want the registry endpoint's model, not the env fallback", c.Model())
	}
	if oc, ok := c.(*openAIChatClient); !ok {
		t.Fatalf("want *openAIChatClient, got %T", c)
	} else if oc.baseURL != "http://registry-vllm:8000/v1" {
		t.Errorf("baseURL = %q; want the registry endpoint baseURL", oc.baseURL)
	} else if oc.apiKey != "sk-test" {
		t.Errorf("apiKey = %q; want the registry endpoint's options[apiKey]", oc.apiKey)
	}
}

// No eval role bound → fall back to the EVAL_CHAT_* env vars.
func TestResolveChatClient_FallsBackToEnvWhenNoRole(t *testing.T) {
	t.Setenv("EVAL_CHAT_BASE_URL", "http://env-fallback:9000/v1")
	t.Setenv("EVAL_CHAT_MODEL", "env-model")
	src := fakeSource{ok: false}
	c, err := ResolveChatClient(src)
	if err != nil {
		t.Fatalf("ResolveChatClient: %v", err)
	}
	if c.Model() != "env-model" {
		t.Errorf("model = %q; want the env fallback model", c.Model())
	}
}

// A registry eval role missing baseURL/model is an error (don't silently fall
// through to env vars — a bound-but-broken role is a misconfiguration to surface).
func TestResolveChatClient_RegistryRoleIncomplete(t *testing.T) {
	t.Setenv("EVAL_CHAT_BASE_URL", "http://env-fallback:9000/v1")
	t.Setenv("EVAL_CHAT_MODEL", "env-model")
	src := fakeSource{ok: true, ep: EvalEndpoint{BaseURL: "", Model: "x"}}
	if _, err := ResolveChatClient(src); err == nil {
		t.Fatal("expected error when bound eval endpoint is missing baseURL")
	}
}

// SSRF guard still applies to the registry path.
func TestResolveChatClient_RegistryRoleRejectsBadBaseURL(t *testing.T) {
	src := fakeSource{ok: true, ep: EvalEndpoint{BaseURL: "file:///etc/passwd", Model: "judge"}}
	if _, err := ResolveChatClient(src); err == nil {
		t.Fatal("expected error for non-http registry baseURL")
	}
}

// A malformed EVAL_CHAT_BASE_URL must be rejected at resolution time (the SSRF
// consistency guard) — a non-http(s) scheme or a host-less URL can't reach the
// chat endpoint and shouldn't be passed to the HTTP client.
func TestResolveChatClient_RejectsBadBaseURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"non-http scheme", "file:///etc/passwd"},
		{"gopher scheme", "gopher://internal:70/"},
		{"no host", "http:///v1"},
		{"not a url", "://nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EVAL_CHAT_BASE_URL", tc.url)
			t.Setenv("EVAL_CHAT_MODEL", "judge-model")
			if _, err := ResolveChatClient(nil); err == nil {
				t.Fatalf("expected error for bad base URL %q", tc.url)
			}
		})
	}
}

func TestResolveChatClient_AcceptsValidBaseURLs(t *testing.T) {
	for _, u := range []string{"http://vllm:8000/v1", "https://api.example.com/v1"} {
		t.Setenv("EVAL_CHAT_BASE_URL", u)
		t.Setenv("EVAL_CHAT_MODEL", "judge-model")
		if _, err := ResolveChatClient(nil); err != nil {
			t.Errorf("ResolveChatClient(%q): unexpected error %v", u, err)
		}
	}
}
