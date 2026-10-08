// Package systemone is a client for TypeSafe System One (the "Jev" decision
// models): POST {base}/v1/systemone answers a set of typed questions about a
// state text with calibrated probabilities (CONTRACT §2.14).
//
// The client is strict. A reply that omits an asked question, answers with
// another type, lacks the field its type requires, or carries a probability
// outside [0, 1] or a score outside its levels is an error (*DecodeError),
// never a partial answer. HTTP failures are typed (*UnprocessableError,
// *RateLimitError, *ServerError, *StatusError, *TimeoutError) so callers can
// branch with errors.As — a 429 or 5xx is retryable, a 422 is not.
//
// Nothing here logs. Error values never carry the request or response body
// (the state text is book content) or the API key.
package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

// Question types.
const (
	TypeNoul   = "noul"   // a probability that the answer is true
	TypeChoice = "choice" // one label of 1–255
	TypeScore  = "score"  // a level of an ordered list of at least 2
)

// DefaultTimeout bounds one System One request.
const DefaultTimeout = 15 * time.Second

// DefaultUSDPerMTokIn is the list price per million input tokens used to
// estimate a call's cost when the gateway reports none (output is not billed).
const DefaultUSDPerMTokIn = 0.042

// CostHeader is the LiteLLM response header carrying the call's cost in USD.
const CostHeader = "x-litellm-response-cost"

// maxResponseBody caps what a reply may make the client read.
const maxResponseBody = 1 << 20

// Question is one question asked about the state. Criteria depends on Type:
// noul takes an optional map with "true"/"false" descriptions; choice a
// map[string]string of 1–255 labels to descriptions; score a []string of at
// least 2 level descriptions (level i is described by Criteria[i]).
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Request is one System One call. Model is always sent; omitting it is a 422.
type Request struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Usage is the token usage a reply reports.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Answer is one question's answer. Which fields are set depends on Type:
// noul → Noul; choice → Choice, Probabilities, Confidence; score → Score,
// Confidence, Legend, Probabilities (keyed by level, "0".."levels-1").
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
}

// Response is a decoded, validated reply.
type Response struct {
	// Model is the model that answered (the reply's "model").
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	// CostUSD is the gateway-reported cost (CostHeader) when present and
	// parseable, else InputTokens × the per-million input price.
	CostUSD float64 `json:"-"`
	// CostReported is true when CostUSD came from the gateway.
	CostReported bool `json:"-"`
}

// Client calls one System One endpoint.
type Client struct {
	baseURL      string
	apiKey       string
	http         *http.Client
	usdPerMTokIn float64
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client (its Timeout then applies as-is).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithTimeout sets the per-request timeout (default DefaultTimeout).
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.http.Timeout = d } }

// WithUSDPerMTokIn sets the input price used for the cost estimate.
func WithUSDPerMTokIn(p float64) Option { return func(c *Client) { c.usdPerMTokIn = p } }

// New builds a client for baseURL — the route prefix /v1/systemone and
// /v1/models are appended to (e.g. "http://litellm:4000/typesafe"). apiKey is
// sent as a bearer token; "" sends none.
func New(baseURL, apiKey string, opts ...Option) (*Client, error) {
	u, err := neturl.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("systemone: base URL %q must be an http(s) URL with a host", baseURL)
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http: &http.Client{
			Timeout: DefaultTimeout,
			// Never follow a redirect: it could carry the bearer token elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		usdPerMTokIn: DefaultUSDPerMTokIn,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Decide sends req and returns the validated reply.
func (c *Client) Decide(ctx context.Context, req Request) (*Response, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("systemone: encode request: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/systemone", body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	out, err := decodeResponse(raw, req)
	if err != nil {
		return nil, err
	}
	out.CostUSD, out.CostReported = c.cost(resp.Header, out.Usage)
	return out, nil
}

// Models lists the model ids the endpoint serves (GET {base}/v1/models).
func (c *Client) Models(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&list); err != nil {
		return nil, &DecodeError{Reason: "models list is not valid JSON"}
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, fmt.Errorf("systemone: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	return resp, nil
}

// cost prefers the gateway's reported cost, else estimates from input tokens.
func (c *Client) cost(h http.Header, u Usage) (float64, bool) {
	if v := strings.TrimSpace(h.Get(CostHeader)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return f, true
		}
	}
	return float64(u.InputTokens) * c.usdPerMTokIn / 1e6, false
}

// ─── Errors ──────────────────────────────────────────────────────────────────

// UnprocessableError is a 422: the request is invalid (e.g. no model). Not
// retryable.
type UnprocessableError struct{}

func (e *UnprocessableError) Error() string      { return "systemone: request rejected (422)" }
func (e *UnprocessableError) ErrorClass() string { return "422" }
func (e *UnprocessableError) HTTPStatus() int    { return http.StatusUnprocessableEntity }

// RateLimitError is a 429. RetryAfter is the server's Retry-After, 0 when
// absent or unparseable.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string      { return "systemone: rate limited (429)" }
func (e *RateLimitError) ErrorClass() string { return "429" }
func (e *RateLimitError) HTTPStatus() int    { return http.StatusTooManyRequests }

// ServerError is a 5xx. Retryable.
type ServerError struct{ Code int }

func (e *ServerError) Error() string      { return fmt.Sprintf("systemone: server error (%d)", e.Code) }
func (e *ServerError) ErrorClass() string { return strconv.Itoa(e.Code) }
func (e *ServerError) HTTPStatus() int    { return e.Code }

// StatusError is any other non-200 reply (401, 403, 404, …).
type StatusError struct{ Code int }

func (e *StatusError) Error() string      { return fmt.Sprintf("systemone: unexpected status %d", e.Code) }
func (e *StatusError) ErrorClass() string { return strconv.Itoa(e.Code) }
func (e *StatusError) HTTPStatus() int    { return e.Code }

// TimeoutError is a request that ran out of time (the client timeout or the
// caller's deadline). Retryable.
type TimeoutError struct{ Err error }

func (e *TimeoutError) Error() string      { return "systemone: request timed out" }
func (e *TimeoutError) Unwrap() error      { return e.Err }
func (e *TimeoutError) ErrorClass() string { return "timeout" }

// DecodeError is a 200 reply that is not a valid answer to the request. Reason
// names the field at fault, never its content.
type DecodeError struct{ Reason string }

func (e *DecodeError) Error() string      { return "systemone: invalid reply: " + e.Reason }
func (e *DecodeError) ErrorClass() string { return "invalid_reply" }

// Retryable reports whether err is worth retrying later (429, 5xx, timeout).
func Retryable(err error) bool {
	var rl *RateLimitError
	var se *ServerError
	var te *TimeoutError
	return errors.As(err, &rl) || errors.As(err, &se) || errors.As(err, &te)
}

func statusError(resp *http.Response) error {
	switch c := resp.StatusCode; {
	case c == http.StatusUnprocessableEntity:
		return &UnprocessableError{}
	case c == http.StatusTooManyRequests:
		var after time.Duration
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			after = time.Duration(s) * time.Second
		}
		return &RateLimitError{RetryAfter: after}
	case c >= 500 && c <= 599:
		return &ServerError{Code: c}
	default:
		return &StatusError{Code: c}
	}
}

// transportError maps a transport failure: a timeout (client or caller
// deadline) is a *TimeoutError; a caller cancellation stays context.Canceled.
// The *url.Error is unwrapped to its cause so the URL is not repeated.
func transportError(ctx context.Context, err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout()) {
		return &TimeoutError{Err: context.DeadlineExceeded}
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("systemone: %w", context.Canceled)
	}
	var ue *neturl.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("systemone: request failed: %w", err)
}

// ─── Validation ──────────────────────────────────────────────────────────────

// maxChoiceLabels is the documented upper bound on a choice question's labels.
const maxChoiceLabels = 255

func validateRequest(req Request) error {
	if strings.TrimSpace(req.Model) == "" {
		return errors.New("systemone: request model is required")
	}
	if len(req.Questions) == 0 {
		return errors.New("systemone: request has no questions")
	}
	for name, q := range req.Questions {
		if strings.TrimSpace(q.Instructions) == "" {
			return fmt.Errorf("systemone: question %q has no instructions", name)
		}
		switch q.Type {
		case TypeNoul:
			if q.Criteria != nil {
				m, ok := stringMap(q.Criteria)
				if !ok {
					return fmt.Errorf("systemone: noul question %q criteria must map true/false to descriptions", name)
				}
				for k := range m {
					if k != "true" && k != "false" {
						return fmt.Errorf("systemone: noul question %q criteria key %q (want true/false)", name, k)
					}
				}
			}
		case TypeChoice:
			m, ok := stringMap(q.Criteria)
			if !ok || len(m) < 1 || len(m) > maxChoiceLabels {
				return fmt.Errorf("systemone: choice question %q needs 1–%d labels", name, maxChoiceLabels)
			}
		case TypeScore:
			if l, ok := stringList(q.Criteria); !ok || len(l) < 2 {
				return fmt.Errorf("systemone: score question %q needs at least 2 levels", name)
			}
		default:
			return fmt.Errorf("systemone: question %q has unknown type %q", name, q.Type)
		}
	}
	return nil
}

func stringMap(v any) (map[string]string, bool) {
	m, ok := v.(map[string]string)
	return m, ok
}

func stringList(v any) ([]string, bool) {
	l, ok := v.([]string)
	return l, ok
}

// decodeResponse parses raw and checks it answers req. Answers to questions
// that were not asked are dropped.
func decodeResponse(raw []byte, req Request) (*Response, error) {
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &DecodeError{Reason: "body is not valid JSON"}
	}
	if strings.TrimSpace(out.Model) == "" {
		return nil, &DecodeError{Reason: "model is missing"}
	}
	if out.Usage.InputTokens < 0 || out.Usage.OutputTokens < 0 {
		return nil, &DecodeError{Reason: "usage is negative"}
	}
	answers := make(map[string]Answer, len(req.Questions))
	for name, q := range req.Questions {
		a, ok := out.Answers[name]
		if !ok {
			return nil, &DecodeError{Reason: fmt.Sprintf("question %q has no answer", name)}
		}
		if err := checkAnswer(name, q, a); err != nil {
			return nil, err
		}
		answers[name] = a
	}
	out.Answers = answers
	return &out, nil
}

func checkAnswer(name string, q Question, a Answer) error {
	bad := func(format string, args ...any) error {
		return &DecodeError{Reason: fmt.Sprintf("question %q: ", name) + fmt.Sprintf(format, args...)}
	}
	if a.Type != q.Type {
		return bad("answer type %q, asked %q", a.Type, q.Type)
	}
	if a.Confidence != nil && !unit(*a.Confidence) {
		return bad("confidence outside [0,1]")
	}
	for k, p := range a.Probabilities {
		if !unit(p) {
			return bad("probability for %q outside [0,1]", k)
		}
	}
	switch q.Type {
	case TypeNoul:
		if a.Noul == nil {
			return bad("noul is missing")
		}
		if !unit(*a.Noul) {
			return bad("noul outside [0,1]")
		}
	case TypeChoice:
		if a.Choice == "" {
			return bad("choice is missing")
		}
		if labels, ok := stringMap(q.Criteria); ok {
			if _, known := labels[a.Choice]; !known {
				return bad("choice %q is not an asked label", a.Choice)
			}
		}
	case TypeScore:
		if a.Score == nil {
			return bad("score is missing")
		}
		s := *a.Score
		if math.IsNaN(s) || math.IsInf(s, 0) || s < 0 {
			return bad("score is not a non-negative number")
		}
		if levels, ok := stringList(q.Criteria); ok && s > float64(len(levels)-1) {
			return bad("score above the top level %d", len(levels)-1)
		}
	}
	return nil
}

func unit(f float64) bool { return !math.IsNaN(f) && f >= 0 && f <= 1 }
