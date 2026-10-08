package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The documented request and reply shapes (CONTRACT §2.14, the TypeSafe
// System One contract). Every case below is built from these.
var (
	noulQ   = Question{Type: TypeNoul, Instructions: "Should the correction apply?", Criteria: map[string]string{"true": "yes", "false": "no"}}
	choiceQ = Question{Type: TypeChoice, Instructions: "Which issue?", Criteria: map[string]string{"x": "first", "y": "second"}}
	scoreQ  = Question{Type: TypeScore, Instructions: "How sure?", Criteria: []string{"low", "mid", "high"}}

	docNoul   = `{"type":"noul","noul":0.99}`
	docChoice = `{"type":"choice","choice":"x","probabilities":{"x":0.85,"y":0.15},"confidence":0.82}`
	docScore  = `{"type":"score","score":0.95,"confidence":0.86,"legend":{"0":"low","1":"mid","2":"high"},"probabilities":{"0":0.07,"1":0.9,"2":0.03}}`
)

func reply(answers string) string {
	return `{"model":"jev-1.13.0","answers":{` + answers + `},"usage":{"input_tokens":391,"output_tokens":67}}`
}

func TestDecodeResponseDocumentedShapes(t *testing.T) {
	req := Request{Model: "jev-1.13.0", State: "s", Questions: map[string]Question{"a": noulQ, "b": choiceQ, "c": scoreQ}}
	got, err := decodeResponse([]byte(reply(`"a":`+docNoul+`,"b":`+docChoice+`,"c":`+docScore)), req)
	if err != nil {
		t.Fatalf("documented reply rejected: %v", err)
	}
	if *got.Answers["a"].Noul != 0.99 || got.Answers["b"].Choice != "x" || got.Answers["b"].Probabilities["y"] != 0.15 ||
		*got.Answers["c"].Score != 0.95 || got.Answers["c"].Legend["2"] != "high" || *got.Answers["c"].Confidence != 0.86 {
		t.Errorf("decoded = %+v", got.Answers)
	}
	if got.Model != "jev-1.13.0" || got.Usage.InputTokens != 391 || got.Usage.OutputTokens != 67 {
		t.Errorf("model/usage = %q %+v", got.Model, got.Usage)
	}
}

func TestDecodeResponseStrict(t *testing.T) {
	one := func(q Question) Request {
		return Request{Model: "jev-1.13.0", State: "s", Questions: map[string]Question{"q": q}}
	}
	tests := []struct {
		name string
		req  Request
		body string
		ok   bool
	}{
		{"noul ok", one(noulQ), reply(`"q":` + docNoul), true},
		{"noul without criteria ok", one(Question{Type: TypeNoul, Instructions: "i"}), reply(`"q":` + docNoul), true},
		{"extra answers dropped", one(noulQ), reply(`"q":` + docNoul + `,"other":` + docNoul), true},
		{"score at top level", one(scoreQ), reply(`"q":{"type":"score","score":2}`), true},
		{"missing answer", one(noulQ), reply(`"other":` + docNoul), false},
		{"type mismatch", one(noulQ), reply(`"q":` + docChoice), false},
		{"noul missing", one(noulQ), reply(`"q":{"type":"noul"}`), false},
		{"noul above 1", one(noulQ), reply(`"q":{"type":"noul","noul":1.01}`), false},
		{"noul negative", one(noulQ), reply(`"q":{"type":"noul","noul":-0.1}`), false},
		{"choice missing", one(choiceQ), reply(`"q":{"type":"choice","probabilities":{"x":1}}`), false},
		{"choice not a label", one(choiceQ), reply(`"q":{"type":"choice","choice":"z"}`), false},
		{"probability above 1", one(choiceQ), reply(`"q":{"type":"choice","choice":"x","probabilities":{"x":1.5}}`), false},
		{"confidence above 1", one(choiceQ), reply(`"q":{"type":"choice","choice":"x","confidence":2}`), false},
		{"score missing", one(scoreQ), reply(`"q":{"type":"score","confidence":0.5}`), false},
		{"score above levels", one(scoreQ), reply(`"q":{"type":"score","score":2.5}`), false},
		{"score negative", one(scoreQ), reply(`"q":{"type":"score","score":-1}`), false},
		{"no model", one(noulQ), `{"answers":{"q":` + docNoul + `}}`, false},
		{"negative usage", one(noulQ), `{"model":"m","answers":{"q":` + docNoul + `},"usage":{"input_tokens":-1}}`, false},
		{"not json", one(noulQ), `<html>`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeResponse([]byte(tt.body), tt.req)
			if tt.ok && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if !tt.ok {
				var de *DecodeError
				if !errors.As(err, &de) {
					t.Fatalf("err = %v, want *DecodeError", err)
				}
				if de.ErrorClass() != "invalid_reply" {
					t.Errorf("class = %q", de.ErrorClass())
				}
			}
		})
	}
}

func TestValidateRequest(t *testing.T) {
	good := Request{Model: "jev-1.13.0", State: "s", Questions: map[string]Question{"q": noulQ}}
	if err := validateRequest(good); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	labels := map[string]string{}
	for i := range 256 {
		labels[string(rune('a'+i%26))+strings.Repeat("x", i)] = "d"
	}
	for name, q := range map[string]Question{
		"unknown type":         {Type: "vibe", Instructions: "i"},
		"no instructions":      {Type: TypeNoul},
		"choice no labels":     {Type: TypeChoice, Instructions: "i", Criteria: map[string]string{}},
		"choice 256 labels":    {Type: TypeChoice, Instructions: "i", Criteria: labels},
		"choice list criteria": {Type: TypeChoice, Instructions: "i", Criteria: []string{"a"}},
		"score one level":      {Type: TypeScore, Instructions: "i", Criteria: []string{"only"}},
		"noul bad key":         {Type: TypeNoul, Instructions: "i", Criteria: map[string]string{"maybe": "?"}},
	} {
		if err := validateRequest(Request{Model: "m", Questions: map[string]Question{"q": q}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validateRequest(Request{Questions: good.Questions}); err == nil {
		t.Error("request without model accepted")
	}
	if err := validateRequest(Request{Model: "m"}); err == nil {
		t.Error("request without questions accepted")
	}
}

// ─── HTTP client ─────────────────────────────────────────────────────────────

func newTestClient(t *testing.T, h http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/typesafe/", "sk-test", opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var noulReq = Request{Model: "jev-1.13.0", State: "the state", Questions: map[string]Question{"q": noulQ}}

func TestDecideSendsModelAndKey(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set(CostHeader, "0.0000171")
		_, _ = io.WriteString(w, reply(`"q":`+docNoul))
	})
	got, err := c.Decide(context.Background(), noulReq)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /typesafe/v1/systemone" || gotAuth != "Bearer sk-test" {
		t.Errorf("request = %s auth %q", gotPath, gotAuth)
	}
	if gotBody["model"] != "jev-1.13.0" || gotBody["state"] != "the state" || gotBody["questions"] == nil {
		t.Errorf("body = %v", gotBody)
	}
	if !got.CostReported || got.CostUSD != 0.0000171 {
		t.Errorf("cost = %v reported %v, want the header", got.CostUSD, got.CostReported)
	}
}

func TestDecideCostFallback(t *testing.T) {
	for name, header := range map[string]string{"absent": "", "unparseable": "n/a", "negative": "-1"} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if header != "" {
					w.Header().Set(CostHeader, header)
				}
				_, _ = io.WriteString(w, reply(`"q":`+docNoul))
			})
			got, err := c.Decide(context.Background(), noulReq)
			if err != nil {
				t.Fatal(err)
			}
			if want := 391 * DefaultUSDPerMTokIn / 1e6; got.CostReported || got.CostUSD != want {
				t.Errorf("cost = %v reported %v, want estimate %v", got.CostUSD, got.CostReported, want)
			}
		})
	}
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, reply(`"q":`+docNoul))
	}, WithUSDPerMTokIn(1))
	got, err := c.Decide(context.Background(), noulReq)
	if err != nil || got.CostUSD != 391/1e6 {
		t.Errorf("custom price cost = %v, %v", got, err)
	}
}

func TestDecideTypedErrors(t *testing.T) {
	tests := []struct {
		status    int
		header    string
		check     func(error) bool
		class     string
		retryable bool
	}{
		{422, "", func(e error) bool { var x *UnprocessableError; return errors.As(e, &x) }, "422", false},
		{429, "7", func(e error) bool {
			var x *RateLimitError
			return errors.As(e, &x) && x.RetryAfter == 7*time.Second
		}, "429", true},
		{503, "", func(e error) bool { var x *ServerError; return errors.As(e, &x) && x.Code == 503 }, "503", true},
		{401, "", func(e error) bool { var x *StatusError; return errors.As(e, &x) && x.Code == 401 }, "401", false},
	}
	for _, tt := range tests {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if tt.header != "" {
				w.Header().Set("Retry-After", tt.header)
			}
			w.WriteHeader(tt.status)
			// An error body that echoes the request must not reach Error().
			_, _ = io.WriteString(w, `{"detail":"bad state: the state"}`)
		})
		_, err := c.Decide(context.Background(), noulReq)
		if !tt.check(err) {
			t.Errorf("%d: err = %#v", tt.status, err)
			continue
		}
		if Retryable(err) != tt.retryable {
			t.Errorf("%d: Retryable = %v", tt.status, !tt.retryable)
		}
		if c, ok := err.(interface{ ErrorClass() string }); !ok || c.ErrorClass() != tt.class {
			t.Errorf("%d: class = %v", tt.status, err)
		}
		if strings.Contains(err.Error(), "the state") || strings.Contains(err.Error(), "sk-test") {
			t.Errorf("%d: error leaks content: %v", tt.status, err)
		}
	}
}

// hang drains the request (so the server notices the client going away) and
// waits for it to give up; the bound keeps a broken test from wedging Close.
func hang(_ http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

func TestDecideTimeout(t *testing.T) {
	c := newTestClient(t, hang, WithTimeout(50*time.Millisecond))
	_, err := c.Decide(context.Background(), noulReq)
	var te *TimeoutError
	if !errors.As(err, &te) || !Retryable(err) || te.ErrorClass() != "timeout" {
		t.Fatalf("err = %v, want *TimeoutError", err)
	}

	// The caller's deadline is a timeout too; a cancellation is not.
	slow := newTestClient(t, hang)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := slow.Decide(ctx, noulReq); !errors.As(err, &te) {
		t.Errorf("deadline err = %v", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := slow.Decide(ctx2, noulReq); !errors.Is(err, context.Canceled) || errors.As(err, &te) {
		t.Errorf("canceled err = %v", err)
	}
}

func TestDecideRefusesInvalidRequestWithoutCalling(t *testing.T) {
	called := false
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { called = true })
	if _, err := c.Decide(context.Background(), Request{State: "s", Questions: noulReq.Questions}); err == nil || called {
		t.Errorf("model-less request: err %v, called %v", err, called)
	}
}

func TestDecideDoesNotFollowRedirects(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/steal", http.StatusFound)
	})
	var se *StatusError
	if _, err := c.Decide(context.Background(), noulReq); !errors.As(err, &se) || se.Code != http.StatusFound {
		t.Errorf("redirect err = %v", err)
	}
}

func TestModels(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/typesafe/v1/models" || r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, "wrong", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"jev-1.13.0"},{"id":"jev-latest"},{"id":""}]}`)
	})
	ids, err := c.Models(context.Background())
	if err != nil || strings.Join(ids, ",") != "jev-1.13.0,jev-latest" {
		t.Errorf("Models = %v, %v", ids, err)
	}
	bad := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	var se *ServerError
	if _, err := bad.Models(context.Background()); !errors.As(err, &se) {
		t.Errorf("Models 502 = %v", err)
	}
}

func TestNewRejectsBadBaseURL(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "http://", "::"} {
		if _, err := New(u, ""); err == nil {
			t.Errorf("base %q accepted", u)
		}
	}
}
