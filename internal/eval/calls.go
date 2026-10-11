package eval

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/genai"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// Judge calls: the per-call deadline, retries, the fn_calls log/cache and the
// invalid_reply failure (CONTRACT §2.15 "Judge calls").

// JudgeChunkFn is the fn_calls.fn of a judge call: one chunk, one reply.
const JudgeChunkFn = "judge_chunk"

// ErrorClassInvalidReply is the fn_calls.error_class (and error.type) of a
// reply that has content but is not valid findings JSON.
const ErrorClassInvalidReply = "invalid_reply"

// ErrorClassCacheSlotTaken is the fn_calls.error_class of a successful, paid
// reply whose cache slot another row already held when it was logged (two
// identical calls in flight at once). The row keeps the reply and its cost;
// it is never served.
const ErrorClassCacheSlotTaken = "cache_slot_taken"

// logCallTimeout bounds one fn_calls write of the judge.
const logCallTimeout = 30 * time.Second

// ErrInvalidReply means the reply had content that is not valid findings
// JSON. The chunk FAILED: it is never read as "no findings" (that would be
// indistinguishable from a clean chunk) and never retried (the same request
// is likely to fail the same way, and it was paid for).
var ErrInvalidReply = errors.New("judge reply is not valid findings JSON")

// Call-policy defaults (CONTRACT §2.15 "Judge calls").
const (
	// DefaultCallTimeout bounds one judge request (`earmark eval
	// --call-timeout`); every path, the in-pipeline worker included, uses it
	// unless told otherwise.
	DefaultCallTimeout = 180 * time.Second
	// DefaultRetries is `earmark eval --retries`' default. A judge built by
	// NewJudge retries nothing (the worker's judge): a failed chunk is
	// recorded and re-judged by a later run instead.
	DefaultRetries = 4
	// defaultBackoffBase / defaultBackoffMax shape the exponential backoff
	// between attempts: base·2^attempt, capped, with equal jitter.
	defaultBackoffBase = 2 * time.Second
	defaultBackoffMax  = time.Minute
	// maxRetryAfter caps a Retry-After the endpoint asks for.
	maxRetryAfter = 5 * time.Minute
)

// CallPolicy bounds and retries the judge's model calls.
type CallPolicy struct {
	// Timeout is the deadline of one attempt; <= 0 → DefaultCallTimeout.
	Timeout time.Duration
	// Retries is the number of extra attempts after the first for a
	// retryable failure (429, 5xx, a timeout, a transport error); <= 0 → none.
	Retries int
	// BackoffBase / BackoffMax shape the wait between attempts; <= 0 → the
	// defaults (2 s, 1 min). A Retry-After on a 429/5xx replaces it.
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

func (p CallPolicy) timeout() time.Duration {
	if p.Timeout <= 0 {
		return DefaultCallTimeout
	}
	return p.Timeout
}

// backoff is the wait before retry number attempt+1 (attempt counts from 0):
// base·2^attempt capped at max, with equal jitter (half fixed, half random).
func (p CallPolicy) backoff(attempt int) time.Duration {
	base, ceiling := p.BackoffBase, p.BackoffMax
	if base <= 0 {
		base = defaultBackoffBase
	}
	if ceiling <= 0 {
		ceiling = defaultBackoffMax
	}
	d := base
	for i := 0; i < attempt && d < ceiling; i++ {
		d *= 2
	}
	d = min(d, ceiling)
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1)) // #nosec G404 -- jitter, not a secret
}

// SetCallPolicy sets the judge's per-call deadline and retries. Call it
// before judging.
func (j *Judge) SetCallPolicy(p CallPolicy) { j.policy = p }

// CallPolicy is the judge's call policy.
func (j *Judge) CallPolicy() CallPolicy { return j.policy }

// retryDelay decides whether a failed attempt is retried and how long to wait
// first. Retried: a 429 or 5xx (waiting the Retry-After the endpoint asked
// for, else the backoff), and the transient transport failures transient()
// lists — the attempt's deadline or another network timeout, a refused or
// reset connection, a connection closed mid-reply (EOF / unexpected EOF), a
// temporary DNS failure, an HTTP/2 stream reset, GOAWAY or lost connection —
// each after the backoff. Never retried: a reply-level failure
// (invalid_reply, an empty/thinking-only/truncated/refused reply), any other
// status, a TLS certificate failure, a bad URL or any other non-transient
// error, nor anything once the caller's context is done or the retries are
// spent.
func (j *Judge) retryDelay(ctx context.Context, err error, attempt int) (time.Duration, bool) {
	if ctx.Err() != nil || attempt >= j.policy.Retries {
		return 0, false
	}
	var se *StatusError
	if errors.As(err, &se) {
		if se.Code != http.StatusTooManyRequests && se.Code < 500 {
			return 0, false
		}
		if se.RetryAfter > 0 {
			return min(se.RetryAfter, maxRetryAfter), true
		}
		return j.policy.backoff(attempt), true
	}
	if replyLevel(err) || !transient(err) {
		return 0, false
	}
	return j.policy.backoff(attempt), true
}

// transient reports whether a call failure is worth another attempt: the
// attempt's deadline, a network timeout, a connection refused or reset, a
// connection that closed mid-reply (EOF / unexpected EOF, also while reading
// the body), a DNS failure other than "no such host" (a temporary resolver
// failure clears; NXDOMAIN does not), or an HTTP/2 stream reset, GOAWAY or
// lost connection. Never a TLS certificate failure, nor a url.Error that
// wraps no network failure (a bad URL, an unsupported scheme): those fail
// the same way every time.
//
// A bare io.EOF stays retryable: net/http returns it (inside a url.Error)
// when the server closes a reused keep-alive connection before answering —
// the classic stale-connection race, which a fresh connection fixes. The
// cost of being wrong is one extra paid call, which is logged like any other.
func transient(err error) bool {
	if certificateError(err) {
		return false
	}
	var ne net.Error
	var dns *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return true
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return true
	case errors.As(err, &dns):
		return dns.IsTemporary || !dns.IsNotFound
	}
	return http2Transient(err)
}

// http2Markers identify the HTTP/2 transport failures net/http's bundled
// http2 client reports only as unexported types or plain errors: a stream
// reset by the server (RST_STREAM: "stream error: …"), a GOAWAY, and a
// connection lost under an in-flight request. The server or a proxy between
// dropped the request; a new stream or connection may succeed.
var http2Markers = []string{"stream error:", "server sent GOAWAY", "client connection lost", "http2: client connection force closed"}

func http2Transient(err error) bool {
	msg := err.Error()
	for _, m := range http2Markers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// certificateError reports a TLS certificate failure.
func certificateError(err error) bool {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var host x509.HostnameError
	var verify *tls.CertificateVerificationError
	return errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &host) || errors.As(err, &verify)
}

// replyLevel reports whether err is about the reply itself, not getting one.
func replyLevel(err error) bool {
	for _, e := range []error{ErrInvalidReply, ErrThinkingOnlyResponse, ErrEmptyResponse,
		ErrTruncatedResponse, ErrRefusalResponse} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// CallLog is where the judge logs every model call and looks up earlier
// answers (fn_calls, CONTRACT §1.9; *db.DB implements it). The judge writes
// it directly — not through internal/fn, whose pinned-model rule is for
// System One functions and would refuse a hosted judge model.
type CallLog interface {
	RegisterRecipe(ctx context.Context, r recipe.Recipe) (string, error)
	LookupFnCache(ctx context.Context, key db.FnCacheKey, expectedModel string) (*db.FnCall, bool, error)
	InsertFnCall(ctx context.Context, c db.FnCall) (int64, bool, error)
}

// SetCallLog makes the judge log every call in fn_calls (fn judge_chunk) and
// serve a chunk it already answered from there instead of paying again. nil
// turns both off.
func (j *Judge) SetCallLog(l CallLog) { j.calls = l }

// RequestParamsReporter is an optional ChatClient extension reporting every
// request field besides the prompt that shapes the reply (openAIChatClient
// implements it). A client that does not is described by its temperature and
// thinking knobs.
type RequestParamsReporter interface {
	RequestParams() map[string]any
}

// judgeCallInput is a judge_chunk call's fn_calls input: everything the reply
// depends on that the cache key's other columns (fn, prompt_sha256 — the
// static prompt parts — and model_alias) do not already cover. The recipe's
// params are not part of the key, so the request params are here.
type judgeCallInput struct {
	// ExpectedModel / ModelRevision / StepVersion: the key's model_alias is
	// the requested id only, so the model the recipe expects to answer, its
	// pinned revision and the judge's step version are part of the input —
	// a revision bump or a new expected model on the same alias is a miss,
	// never a reply of the old model served as the new one's.
	ExpectedModel string               `json:"expected_model"`
	ModelRevision string               `json:"model_revision,omitempty"`
	StepVersion   int                  `json:"step_version"`
	PromptVersion string               `json:"prompt_version"`
	FilePath      string               `json:"file_path"`
	StartSec      float64              `json:"start_sec"`
	EndSec        float64              `json:"end_sec"`
	ChunkText     string               `json:"chunk_text"`
	BookContext   judgeCallBookContext `json:"book_context"`
	Request       map[string]any       `json:"request"`
}

type judgeCallBookContext struct {
	Source string `json:"source"`
	SHA256 string `json:"sha256,omitempty"`
	Block  string `json:"block,omitempty"`
}

// judgeCallOutput is a judge_chunk call's fn_calls output: the reply exactly
// as received, the model the endpoint reported, and how the judge read it at
// call time — the findings it kept and every one it dropped, with the reason.
// A cache hit re-reads Content under the serving judge's own filters.
type judgeCallOutput struct {
	Content       string             `json:"content"`
	ReportedModel string             `json:"reported_model,omitempty"`
	Kept          []judgeCallFinding `json:"kept"`
	Dropped       []judgeCallFinding `json:"dropped"`
	ParseError    string             `json:"parse_error,omitempty"`
}

type judgeCallFinding struct {
	OriginalText        string  `json:"original_text"`
	SuggestedCorrection string  `json:"suggested_correction,omitempty"`
	IssueType           string  `json:"issue_type"`
	Confidence          float64 `json:"confidence"`
	Reason              string  `json:"reason,omitempty"`
}

// callRecord is one chunk's fn_calls identity, built before the first
// attempt.
type callRecord struct {
	key   db.FnCacheKey
	input json.RawMessage
}

// requestParams is what the client sends besides the prompt.
func (j *Judge) requestParams() map[string]any {
	if rp, ok := j.chat.(RequestParamsReporter); ok {
		return rp.RequestParams()
	}
	p := map[string]any{}
	if t := j.sentTemperature(); t != nil {
		p["temperature"] = *t
	}
	if tr, ok := j.chat.(ThinkingReporter); ok {
		th := tr.Thinking()
		if th.ReasoningEffort != "" {
			p["reasoning_effort"] = th.ReasoningEffort
		}
		if th.ChatTemplateKwargs != nil {
			p["chat_template_kwargs"] = th.ChatTemplateKwargs
		}
	}
	return p
}

// newCallRecord builds c's fn_calls key and input; nil when the judge logs no
// calls.
func (j *Judge) newCallRecord(c db.EvalChunk, bc BookContext) (*callRecord, error) {
	if j.calls == nil {
		return nil, nil
	}
	in := judgeCallInput{
		ExpectedModel: j.Recipe().ModelResolved,
		ModelRevision: j.pin.Revision,
		StepVersion:   proposeStepVersion,
		PromptVersion: j.prompt.Version(),
		FilePath:      c.FilePath,
		StartSec:      c.StartSec,
		EndSec:        c.EndSec,
		ChunkText:     c.Text,
		BookContext:   judgeCallBookContext{Source: bc.Source, SHA256: bc.SHA256, Block: bc.Block},
		Request:       j.requestParams(),
	}
	canon, sum, err := canonicalJSON(in)
	if err != nil {
		return nil, err
	}
	return &callRecord{
		key: db.FnCacheKey{Fn: JudgeChunkFn, PromptSHA256: j.prompt.sha256(),
			ModelAlias: j.chat.Model(), InputSHA256: sum},
		input: canon,
	}, nil
}

// canonicalJSON is v's canonical JSON (keys sorted, numbers kept literal —
// recipe.MarshalCanonical, the encoding internal/fn hashes) and its sha256.
func canonicalJSON(v any) ([]byte, string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, "", fmt.Errorf("judge call input: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, "", fmt.Errorf("judge call input: %w", err)
	}
	canon, err := recipe.MarshalCanonical(generic)
	if err != nil {
		return nil, "", fmt.Errorf("judge call input: %w", err)
	}
	sum := sha256.Sum256(canon)
	return canon, hex.EncodeToString(sum[:]), nil
}

// recipeIDs memoizes registered recipe ids by id (registration is
// idempotent; this only saves the round trip per call).
type recipeIDs struct{ m sync.Map }

// registerRecipe registers r through the call log once per judge.
func (j *Judge) registerRecipe(ctx context.Context, r recipe.Recipe) (string, error) {
	id, err := r.ID()
	if err != nil {
		return "", err
	}
	if _, ok := j.registered.m.Load(id); ok {
		return id, nil
	}
	if _, err := j.calls.RegisterRecipe(ctx, r); err != nil {
		return "", err
	}
	j.registered.m.Store(id, true)
	return id, nil
}

// serveCached answers c from fn_calls when this judge already has a valid
// reply for the same input. ok is false on a miss (or an unreadable cached
// row, which is then paid for again). A served call is logged as its own
// cache_hit row pointing at the row it was served from; the findings point at
// that original row (fn_call_id), the call that produced the reply.
func (j *Judge) serveCached(ctx context.Context, call *callRecord, base Result) (Result, bool, error) {
	src, hit, err := j.calls.LookupFnCache(ctx, call.key, j.Recipe().ModelResolved)
	if err != nil {
		return base, false, fmt.Errorf("judge chunk %s: fn_calls lookup: %w", base.Chunk.ChunkID, err)
	}
	if !hit {
		return base, false, nil
	}
	var out judgeCallOutput
	if err := json.Unmarshal(src.Output, &out); err != nil {
		j.logger.WarnContext(ctx, "cached judge reply unreadable; judging again",
			"chunk_id", base.Chunk.ChunkID, "fn_call_id", src.ID, "error", err)
		return base, false, nil
	}
	res := base
	res.Cached, res.ResolvedModel = true, out.ReportedModel
	res, perr := j.readReply(ctx, res, out.Content, out.ReportedModel)
	if perr != nil {
		j.logger.WarnContext(ctx, "cached judge reply does not parse; judging again",
			"chunk_id", base.Chunk.ChunkID, "fn_call_id", src.ID, "error", perr)
		return base, false, nil
	}
	start := time.Now()
	row := db.FnCall{
		Fn: JudgeChunkFn, PromptVersion: j.prompt.Version(), PromptSHA256: call.key.PromptSHA256,
		ModelAlias: call.key.ModelAlias, ModelResolved: src.ModelResolved, ModelRevision: src.ModelRevision,
		RecipeID: src.RecipeID, InputSHA256: call.key.InputSHA256, Input: call.input, Output: src.Output,
		CacheHit: true, CachedFrom: &src.ID, LatencyMS: msPtr(time.Since(start)),
	}
	if _, _, err := j.calls.InsertFnCall(ctx, row); err != nil {
		j.logger.WarnContext(ctx, "logging a cached judge call failed (continuing)",
			"chunk_id", base.Chunk.ChunkID, "error", err)
	}
	countModelCall(ctx, j.chat.Model(), genai.OutcomeCached)
	res.setFnCall(src.ID)
	return res, true, nil
}

// logCall records one attempt in fn_calls and returns its id (0 when the
// judge logs no calls, the insert failed — logged, never fatal: the reply is
// still good — or a concurrent identical call already holds the cache row,
// in which case that row's id is returned). err is the attempt's outcome: nil
// for a usable reply, ErrInvalidReply for one that does not parse, else the
// call failure.
func (j *Judge) logCall(ctx context.Context, call *callRecord, res Result, comp Completion,
	elapsed time.Duration, content string, err error) int64 {
	if call == nil {
		return 0
	}
	// A stop must not lose the row of a paid call; bounded so a stuck
	// database cannot hold the judge forever.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logCallTimeout)
	defer cancel()
	row := db.FnCall{
		Fn: JudgeChunkFn, PromptVersion: j.prompt.Version(), PromptSHA256: call.key.PromptSHA256,
		ModelAlias: call.key.ModelAlias, ModelRevision: j.pin.Revision,
		InputSHA256: call.key.InputSHA256, Input: call.input, LatencyMS: msPtr(elapsed),
	}
	if comp.HasUsage {
		in, out := comp.InputTokens, comp.OutputTokens
		row.InputTokens, row.OutputTokens = &in, &out
	}
	if comp.HasCost {
		c := comp.CostUSD
		row.CostUSD = &c
	}
	expected := j.Recipe().ModelResolved
	switch {
	case err == nil || errors.Is(err, ErrInvalidReply):
		// What answered: the reported model, else the expected one (an
		// endpoint that reports nothing keeps the expected model, as the
		// findings' recipe does), so the row can serve a later lookup.
		row.ModelResolved = comp.ResolvedModel
		if row.ModelResolved == "" {
			row.ModelResolved = expected
		}
		r := j.Recipe()
		if err == nil {
			r = j.recipeFor(comp.ResolvedModel)
		}
		if id, rerr := j.registerRecipe(ctx, r); rerr == nil {
			row.RecipeID = id
		} else {
			j.logger.WarnContext(ctx, "registering the judge recipe for fn_calls failed (row logged without it)", "error", rerr)
		}
		out := callOutput(content, comp.ResolvedModel, res, err)
		b, merr := json.Marshal(out)
		if merr != nil {
			j.logger.WarnContext(ctx, "encoding a judge reply for fn_calls failed", "error", merr)
			return 0
		}
		row.Output = b
		switch {
		case err != nil:
			row.ErrorClass = ErrorClassInvalidReply
		case !genai.SameModel(row.ModelResolved, expected):
			// Store but never serve (the fn_calls rule): a fallback answer
			// must not take the cache slot from the expected model's.
			row.ErrorClass = db.ErrorClassModelFallback
		}
	default:
		row.ModelResolved = comp.ResolvedModel
		row.ErrorClass = errorClass(err)
		if id, rerr := j.registerRecipe(ctx, j.Recipe()); rerr == nil {
			row.RecipeID = id
		}
	}
	id, inserted, ierr := j.calls.InsertFnCall(ctx, row)
	if ierr != nil {
		j.logger.WarnContext(ctx, "logging a judge call in fn_calls failed (continuing)",
			"chunk_id", res.Chunk.ChunkID, "error", ierr)
		return 0
	}
	if !inserted {
		// Another successful row already holds this key's cache slot (a
		// concurrent identical call). This call was paid for and must still
		// be logged: store it outside the cache index, as cache_slot_taken.
		row.ErrorClass = ErrorClassCacheSlotTaken
		id, _, ierr = j.calls.InsertFnCall(ctx, row)
		if ierr != nil {
			j.logger.WarnContext(ctx, "logging a judge call in fn_calls failed (continuing)",
				"chunk_id", res.Chunk.ChunkID, "error", ierr)
			return 0
		}
	}
	return id
}

// callOutput is the fn_calls output of a reply.
func callOutput(content, reported string, res Result, err error) judgeCallOutput {
	out := judgeCallOutput{Content: content, ReportedModel: reported,
		Kept: []judgeCallFinding{}, Dropped: []judgeCallFinding{}}
	if err != nil {
		out.ParseError = err.Error()
		return out
	}
	for _, f := range res.Findings {
		cf := judgeCallFinding{OriginalText: f.OriginalText, IssueType: f.IssueType, Confidence: f.Confidence}
		if f.SuggestedCorrection != nil {
			cf.SuggestedCorrection = *f.SuggestedCorrection
		}
		out.Kept = append(out.Kept, cf)
	}
	for _, d := range res.Dropped {
		out.Dropped = append(out.Dropped, judgeCallFinding{OriginalText: d.OriginalText,
			SuggestedCorrection: d.SuggestedCorrection, IssueType: d.IssueType,
			Confidence: d.Confidence, Reason: d.Reason})
	}
	return out
}

func msPtr(d time.Duration) *int {
	ms := int(d.Milliseconds())
	return &ms
}

// setFnCall points the result and its findings at fn_calls row id.
func (r *Result) setFnCall(id int64) {
	if id == 0 {
		return
	}
	r.FnCallID = &id
	for i := range r.Findings {
		r.Findings[i].FnCallID = &id
	}
}
