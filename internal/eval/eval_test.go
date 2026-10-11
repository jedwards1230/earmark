package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

// fakeChat returns a canned response (or error) and records the prompts it saw.
type fakeChat struct {
	resp    string
	err     error
	model   string
	lastSys string
	lastUsr string
	calls   int
}

func (f *fakeChat) Complete(_ context.Context, system, user string) (string, error) {
	f.calls++
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

// chunkWith is sampleChunk with other text — the judge's pre-filters drop a
// finding whose span is not in the chunk it judged.
func chunkWith(text string) db.EvalChunk {
	c := sampleChunk()
	c.Text = text
	return c
}

// manyFindingsChunk holds every span manyFindingsJSON(n) flags.
func manyFindingsChunk(n int) db.EvalChunk {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("finding-%02d", i)
	}
	return chunkWith(strings.Join(parts, " "))
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

// TestJudgeChunk_InvalidReplyFailsTheChunk: a reply with content that is not
// findings JSON is ErrInvalidReply — the chunk FAILED, never "no findings"
// (which reads as a clean chunk and would latch the transcript, gap 5) — and
// it is not retried even with retries configured.
func TestJudgeChunk_InvalidReplyFailsTheChunk(t *testing.T) {
	chat := &fakeChat{resp: "I cannot help with that."}
	j := NewJudge(chat)
	j.SetCallPolicy(CallPolicy{Retries: 3, BackoffBase: time.Millisecond})
	res, err := j.JudgeChunk(context.Background(), sampleChunk())
	if !errors.Is(err, ErrInvalidReply) {
		t.Fatalf("want ErrInvalidReply, got %v", err)
	}
	if errorClass(err) != ErrorClassInvalidReply {
		t.Errorf("error class %q, want %q", errorClass(err), ErrorClassInvalidReply)
	}
	if len(res.Findings) != 0 || res.Attempts != 1 || chat.calls != 1 {
		t.Fatalf("findings %d, attempts %d, calls %d; want 0, 1, 1 (never retried)", len(res.Findings), res.Attempts, chat.calls)
	}

	// Through Run: the chunk counts as skipped (failed), so the run is not
	// Complete and nothing would latch.
	_, stats, err := Run(context.Background(), fakeReader{chunks: []db.EvalChunk{sampleChunk()}}, j, &fakeWriter{},
		RunOptions{Book: "Book"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ChunksEvaluated != 0 || stats.ChunksSkipped != 1 || stats.Complete() {
		t.Errorf("stats %+v: an invalid reply must fail the chunk", stats)
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
		fmt.Fprintf(&b, `{"original_text":"finding-%02d","issue_type":"misheard_word","suggested_correction":"fix-%02d","confidence":%.2f}`, i, i, conf)
	}
	b.WriteString(`]}`)
	return b.String()
}

// repeatedNameJSON builds a judge response that fixes the same misheard name
// at each of its n occurrences in repeatedNameChunk(n) — one finding per
// occurrence, as the patch path needs — followed by `others` distinct fixes
// of the other-NN spans.
func repeatedNameJSON(n, others int) string {
	text := repeatedNameChunk(n, others).Text
	var b strings.Builder
	b.WriteString(`{"findings":[`)
	off := 0
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		off += strings.Index(text[off:], "auto sebo")
		fmt.Fprintf(&b, `{"original_text":"auto sebo","issue_type":"misheard_proper_noun","suggested_correction":"arecibo","confidence":0.95,"anchor_offset":%d,"anchor_occurrence":%d}`, off, i)
		off++
	}
	for i := 0; i < others; i++ {
		fmt.Fprintf(&b, `,{"original_text":"other-%02d","issue_type":"misheard_word","suggested_correction":"fix-%02d","confidence":0.7}`, i, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

// repeatedNameChunk mentions "auto sebo" n times, then holds `others` other
// spans.
func repeatedNameChunk(n, others int) db.EvalChunk {
	parts := make([]string, 0, n+others)
	for i := 0; i < n; i++ {
		parts = append(parts, "the dish at auto sebo")
	}
	for i := 0; i < others; i++ {
		parts = append(parts, fmt.Sprintf("other-%02d", i))
	}
	return chunkWith(strings.Join(parts, " "))
}

// TestJudgeChunk_PerChunkCap: the default cap leaves room for a misheard name
// repeated through a chunk (each occurrence its own row, as patch anchoring
// needs) alongside other fixes, while a chunk with genuinely many findings is
// still bounded to the highest-confidence ones and the rest are counted as
// over_cap.
func TestJudgeChunk_PerChunkCap(t *testing.T) {
	tests := []struct {
		name        string
		resp        string
		chunk       db.EvalChunk
		wantKept    int
		wantOverCap int
		// wantOccurrences: the auto sebo → arecibo rows must cover these
		// anchor_occurrence values, one row each.
		wantOccurrences int
	}{
		{
			// The bake-off shape that the old cap of 5 broke: one name misheard
			// seven times plus four other fixes. All eleven are kept.
			name:            "repeated name fixes survive",
			resp:            repeatedNameJSON(7, 4),
			chunk:           repeatedNameChunk(7, 4),
			wantKept:        11,
			wantOccurrences: 7,
		},
		{
			// A repeat group larger than the cap is cut like any other
			// overflow (capFindings): bounded, and the cut rows are counted.
			name:            "repeat group larger than the cap is bounded",
			resp:            repeatedNameJSON(defaultMaxFindingsPerChunk+2, 0),
			chunk:           repeatedNameChunk(defaultMaxFindingsPerChunk+2, 0),
			wantKept:        defaultMaxFindingsPerChunk,
			wantOverCap:     2,
			wantOccurrences: defaultMaxFindingsPerChunk,
		},
		{
			name:     "exactly at the cap",
			resp:     manyFindingsJSON(defaultMaxFindingsPerChunk),
			chunk:    manyFindingsChunk(defaultMaxFindingsPerChunk),
			wantKept: defaultMaxFindingsPerChunk,
		},
		{
			name:        "many distinct findings are still bounded",
			resp:        manyFindingsJSON(defaultMaxFindingsPerChunk + 8),
			chunk:       manyFindingsChunk(defaultMaxFindingsPerChunk + 8),
			wantKept:    defaultMaxFindingsPerChunk,
			wantOverCap: 8,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EVAL_MAX_FINDINGS_PER_CHUNK", "") // exercise the default
			t.Setenv("EVAL_MIN_CONFIDENCE", "")
			res, err := NewJudge(&fakeChat{resp: tc.resp}).JudgeChunk(context.Background(), tc.chunk)
			if err != nil {
				t.Fatalf("JudgeChunk: %v", err)
			}
			if len(res.Findings) != tc.wantKept {
				t.Errorf("kept %d findings, want %d (dropped: %+v)", len(res.Findings), tc.wantKept, res.Dropped)
			}
			overCap := 0
			for _, d := range res.Dropped {
				if d.Reason != DropOverCap {
					t.Errorf("unexpected %s drop of %q", d.Reason, d.OriginalText)
					continue
				}
				overCap++
			}
			if overCap != tc.wantOverCap {
				t.Errorf("over_cap drops = %d, want %d", overCap, tc.wantOverCap)
			}

			occ := map[int]bool{}
			minKept := 1.0
			for _, f := range res.Findings {
				minKept = min(minKept, f.Confidence)
				if f.OriginalText == "auto sebo" && f.AnchorOccurrence != nil {
					occ[*f.AnchorOccurrence] = true
				}
			}
			if len(occ) != tc.wantOccurrences {
				t.Errorf("repeated-name rows cover %d occurrences, want %d", len(occ), tc.wantOccurrences)
			}
			// The cap keeps the highest-confidence findings: nothing dropped
			// outranks anything kept.
			for _, d := range res.Dropped {
				if d.Confidence > minKept+1e-9 {
					t.Errorf("dropped %q (conf %.2f) outranks a kept finding (conf %.2f)", d.OriginalText, d.Confidence, minKept)
				}
			}
		})
	}
}

func TestJudgeChunk_CapEnvOverrideAndDisable(t *testing.T) {
	// Override the cap to 2.
	t.Setenv("EVAL_MAX_FINDINGS_PER_CHUNK", "2")
	chat := &fakeChat{resp: manyFindingsJSON(5)}
	if res, err := NewJudge(chat).JudgeChunk(context.Background(), manyFindingsChunk(5)); err != nil {
		t.Fatalf("JudgeChunk: %v", err)
	} else if len(res.Findings) != 2 {
		t.Fatalf("want 2 findings with cap=2, got %d", len(res.Findings))
	}

	// A negative value disables the cap entirely (all findings retained).
	t.Setenv("EVAL_MAX_FINDINGS_PER_CHUNK", "-1")
	chat2 := &fakeChat{resp: manyFindingsJSON(5)}
	if res, err := NewJudge(chat2).JudgeChunk(context.Background(), manyFindingsChunk(5)); err != nil {
		t.Fatalf("JudgeChunk: %v", err)
	} else if len(res.Findings) != 5 {
		t.Fatalf("want 5 findings with cap disabled, got %d", len(res.Findings))
	}
}

func TestJudgeChunk_ConfidenceFloorDropsLow(t *testing.T) {
	// Four findings spanning the floor; all carry corrections so only confidence
	// decides survival. Default floor is 0.6 → keep 0.90 and 0.65, drop 0.55/0.30.
	resp := `{"findings":[
		{"original_text":"cat","issue_type":"misheard_word","suggested_correction":"cot","confidence":0.90},
		{"original_text":"dog","issue_type":"misheard_word","suggested_correction":"dig","confidence":0.65},
		{"original_text":"fish","issue_type":"misheard_word","suggested_correction":"wish","confidence":0.55},
		{"original_text":"bird","issue_type":"misheard_word","suggested_correction":"beard","confidence":0.30}
	]}`
	t.Setenv("EVAL_MIN_CONFIDENCE", "") // exercise the default (0.6)
	res, err := NewJudge(&fakeChat{resp: resp}).JudgeChunk(context.Background(), chunkWith("cat dog fish bird"))
	if err != nil {
		t.Fatalf("JudgeChunk: %v", err)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("want 2 findings above the 0.6 floor, got %d", len(res.Findings))
	}
	for _, f := range res.Findings {
		if f.Confidence < defaultMinConfidence {
			t.Errorf("kept a sub-floor finding %q (conf %.2f)", f.OriginalText, f.Confidence)
		}
	}

	// EVAL_MIN_CONFIDENCE=0 disables the floor → all four survive (the cap is 5).
	t.Setenv("EVAL_MIN_CONFIDENCE", "0")
	res2, err := NewJudge(&fakeChat{resp: resp}).JudgeChunk(context.Background(), chunkWith("cat dog fish bird"))
	if err != nil {
		t.Fatalf("JudgeChunk: %v", err)
	}
	if len(res2.Findings) != 4 {
		t.Fatalf("want 4 findings with floor disabled, got %d", len(res2.Findings))
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
	chat := &fakeChat{resp: `{"findings":[{"original_text":"fox","issue_type":"misheard_word","suggested_correction":"folks","confidence":0.7}]}`}
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
	chat := &fakeChat{resp: `{"findings":[{"original_text":"fox","issue_type":"misheard_word","suggested_correction":"folks","confidence":0.7}]}`}
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

// The bearer token precedence on the registry path: the key resolved from
// apiKeyEnv, then options["apiKey"] (back-compat). EVAL_CHAT_API_KEY belongs to
// the env-var fallback only and never leaks into a registry-bound judge.
func TestResolveChatClient_APIKeyPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		src     EvalEndpointSource
		envKey  string
		wantKey string
	}{
		{
			name:    "resolved key beats options apiKey",
			src:     fakeSource{ok: true, ep: EvalEndpoint{BaseURL: "http://gw:4000/v1", Model: "m", APIKey: "sk-env", Options: map[string]string{"apiKey": "sk-opt"}}},
			envKey:  "sk-fallback",
			wantKey: "sk-env",
		},
		{
			name:    "options apiKey when no resolved key",
			src:     fakeSource{ok: true, ep: EvalEndpoint{BaseURL: "http://gw:4000/v1", Model: "m", Options: map[string]string{"apiKey": "sk-opt"}}},
			envKey:  "sk-fallback",
			wantKey: "sk-opt",
		},
		{
			name:    "registry endpoint with no key sends none",
			src:     fakeSource{ok: true, ep: EvalEndpoint{BaseURL: "http://gw:4000/v1", Model: "m"}},
			envKey:  "sk-fallback",
			wantKey: "",
		},
		{
			name:    "env fallback uses EVAL_CHAT_API_KEY",
			src:     fakeSource{ok: false},
			envKey:  "sk-fallback",
			wantKey: "sk-fallback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EVAL_CHAT_BASE_URL", "http://env-fallback:9000/v1")
			t.Setenv("EVAL_CHAT_MODEL", "env-model")
			t.Setenv("EVAL_CHAT_API_KEY", tc.envKey)
			c, err := ResolveChatClient(tc.src)
			if err != nil {
				t.Fatalf("ResolveChatClient: %v", err)
			}
			oc, ok := c.(*openAIChatClient)
			if !ok {
				t.Fatalf("want *openAIChatClient, got %T", c)
			}
			if oc.apiKey != tc.wantKey {
				t.Errorf("apiKey = %q, want %q", oc.apiKey, tc.wantKey)
			}
		})
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
