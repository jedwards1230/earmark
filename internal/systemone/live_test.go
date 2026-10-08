package systemone

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveSmoke calls a real System One endpoint. Skipped unless
//
//	EARMARK_SYSTEMONE_LIVE=1
//	EARMARK_SYSTEMONE_BASE_URL   e.g. http://litellm.llm-gateway.svc:4000/typesafe
//	EARMARK_SYSTEMONE_API_KEY    the bearer key (a LiteLLM virtual key)
//	EARMARK_SYSTEMONE_MODEL      optional, default jev-1.13.0
//
// are set. With EARMARK_SYSTEMONE_RECORD=1 it also writes the raw reply body
// to testdata/live_reply.json — the fixture that pins the documented field
// names (the key is a request header and never reaches the file). The name
// deliberately lacks "Integration": CI runs -run Integration against Postgres.
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("EARMARK_SYSTEMONE_LIVE") != "1" {
		t.Skip("set EARMARK_SYSTEMONE_LIVE=1 (plus _BASE_URL, _API_KEY) to call the live endpoint")
	}
	base, key := os.Getenv("EARMARK_SYSTEMONE_BASE_URL"), os.Getenv("EARMARK_SYSTEMONE_API_KEY")
	if base == "" || key == "" {
		t.Fatal("EARMARK_SYSTEMONE_BASE_URL and EARMARK_SYSTEMONE_API_KEY are required")
	}
	model := os.Getenv("EARMARK_SYSTEMONE_MODEL")
	if model == "" {
		model = "jev-1.13.0"
	}
	rec := &recorder{next: http.DefaultTransport}
	c, err := New(base, key, WithHTTPClient(&http.Client{Timeout: DefaultTimeout, Transport: rec}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ids, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	t.Logf("models: %v", ids)

	req := Request{Model: model, State: "the transcript reads: we sailed past the light house at dawn",
		Questions: map[string]Question{
			"is_compound": {Type: TypeNoul, Instructions: "Is 'light house' a misheard single word?",
				Criteria: map[string]string{"true": "it should be one word", "false": "two words are right"}},
			"kind": {Type: TypeChoice, Instructions: "What kind of error is this?",
				Criteria: map[string]string{"spacing": "a word split in two", "homophone": "a sound-alike word", "none": "no error"}},
			"severity": {Type: TypeScore, Instructions: "How much does it hurt reading?",
				Criteria: []string{"not at all", "a little", "a lot"}},
		}}
	start := time.Now()
	got, err := c.Decide(ctx, req)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	t.Logf("model %s, latency %s, usage %+v, cost %.8f (reported %v)",
		got.Model, time.Since(start), got.Usage, got.CostUSD, got.CostReported)

	if os.Getenv("EARMARK_SYSTEMONE_RECORD") == "1" {
		path := filepath.Join("testdata", "live_reply.json")
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, rec.last, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %s", path)
	}
}

// recorder keeps the last response body it relays.
type recorder struct {
	next http.RoundTripper
	last []byte
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	r.last = b
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}
