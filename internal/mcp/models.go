package mcp

import (
	"cmp"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// ─── Models page: the role board (CONTRACT §2.14) ─────────────────────────────
//
// The page answers "is each pipeline role being done, by what, and is its
// output current?" — one card per configured role (ASR, Judge, Embeddings,
// Decide, Scan, Format; not-configured roles collapse to one line), then the
// supporting detail: recipes + stale counts, the LiteLLM gateway, the AI
// endpoint registry, judge output by answering model, and the ASR runners.
//
// Endpoint liveness (GET /models) is NOT call success: a gateway that lists the
// alias but 401s every chat call "lists model" in the endpoint table and is
// FAILING on the Judge card. Role health therefore folds in call outcomes:
// run_metrics for the judge, fn_calls for decide and scan.
//
// Every builder here is pure (takes now and the cached snapshot, does no I/O),
// so the page and GET /api/v1/status share one source and cannot disagree.

// Role health tokens (the API's roles[].state values).
const (
	roleHealthy       = "healthy"
	roleIdle          = "idle"
	roleDegraded      = "degraded"
	roleFailing       = "failing"
	roleDown          = "down"
	roleNotConfigured = "not_configured"
	roleUnknown       = "unknown"
)

// Answered-vs-expected comparison tokens (roles[].answeredMatch).
const (
	matchOK         = "match"      // the answering model is the expected one
	matchMismatch   = "mismatch"   // something else answered (fallback?)
	matchUnreported = "unreported" // runs happened but recorded no model
	matchNone       = "none"       // no runs yet
	matchUnchecked  = "unchecked"  // answered, but nothing configured to compare against
	// matchPredates: the newest answer predates the current recipe (the model
	// or prompt changed since and nothing has been called under it yet), so it
	// is not compared — a pre-change answer is no evidence of a fallback.
	matchPredates = "predates_recipe"
)

// judgeQuietAfter is how long the judge may go without a success while
// transcripts wait before the card degrades: 3× the hourly backfill CronJob
// (deploy/helm cronjob-eval-backfill, "17 * * * *"), so one missed run is not
// an alarm but a stuck one is.
const judgeQuietAfter = 3 * time.Hour

// embedQuietAfter is the same idea for the embed worker, which runs
// continuously: an hour with a backlog and nothing embedded is a stall.
const embedQuietAfter = time.Hour

// maxCardErrorLen / maxAPIErrorLen bound the judge's last-error text on the
// card (full text in the title tooltip) and in the API.
const (
	maxCardErrorLen = 160
	maxAPIErrorLen  = 300
)

// roleHealth is one role's derived state. The glyph pairs with the word so the
// state is never conveyed by color alone.
type roleHealth struct {
	Token string // healthy|idle|degraded|failing|down|not_configured|unknown
	Label string // HEALTHY …
	Glyph string // ✓ ▲ ✗ ○ ?
	Class string // state-running|state-busy|state-stalled|state-unknown
	Dot   string // green|amber|red|grey
	Sub   string // one-sentence reason
}

// roleHealthMeta is each token's display. idle is healthy with nothing to do:
// it gets the same green ✓ as healthy so a working-but-quiet role never reads
// as not-OK; its API token stays "idle".
var roleHealthMeta = map[string]roleHealth{
	roleHealthy:       {Token: roleHealthy, Label: "HEALTHY", Glyph: "✓", Class: "state-running", Dot: "green"},
	roleIdle:          {Token: roleIdle, Label: "HEALTHY · idle", Glyph: "✓", Class: "state-running", Dot: "green"},
	roleDegraded:      {Token: roleDegraded, Label: "DEGRADED", Glyph: "▲", Class: "state-busy", Dot: "amber"},
	roleFailing:       {Token: roleFailing, Label: "FAILING", Glyph: "✗", Class: "state-stalled", Dot: "red"},
	roleDown:          {Token: roleDown, Label: "DOWN", Glyph: "✗", Class: "state-stalled", Dot: "red"},
	roleNotConfigured: {Token: roleNotConfigured, Label: "NOT CONFIGURED", Glyph: "○", Class: "state-unknown", Dot: "grey"},
	roleUnknown:       {Token: roleUnknown, Label: "UNKNOWN", Glyph: "?", Class: "state-unknown", Dot: "grey"},
}

func health(token, sub string) roleHealth {
	h := roleHealthMeta[token]
	h.Sub = sub
	return h
}

// roleDef is one row of the static role table. format has no model role yet;
// when one lands, its card gains a resolver here.
type roleDef struct {
	Key   string // API token
	Title string
	Step  string // recipe step
}

var roleDefs = []roleDef{
	{Key: "asr", Title: "ASR", Step: recipe.StepASR},
	{Key: "judge", Title: "Judge", Step: recipe.StepPropose},
	{Key: "embeddings", Title: "Embeddings", Step: recipe.StepEmbed},
	{Key: "decide", Title: "Decide", Step: recipe.StepDecide},
	{Key: "scan", Title: "Scan", Step: recipe.StepScan},
	{Key: "format", Title: "Format", Step: recipe.StepFormat},
}

// roleTitleForStep labels a recipe step with the role that runs it ("" for
// propagate, which has no model role).
func roleTitleForStep(step string) string {
	for _, d := range roleDefs {
		if d.Step == step {
			return d.Title
		}
	}
	return ""
}

// roleCard is one card on the role board (and one roles[] API entry).
type roleCard struct {
	Key, Title, Step string
	Health           roleHealth
	Configured       bool
	// EndpointID is the AI_ENDPOINTS id serving the role, "EVAL_CHAT_*" for an
	// env-sourced judge, "" for ASR and unconfigured roles.
	EndpointID      string
	Gateway         string // "litellm" | other declared value | ""
	GatewayInferred bool
	Requested       string // the model id earmark sends / ASR_SERVERS expects
	// Expected is the MODELS_FILE pin; "" = unpinned (the requested id is then
	// what an answer is compared against).
	Expected         string
	ExpectedRevision string
	Answered         string
	AnsweredMatch    string          // match|mismatch|unreported|none|unchecked
	AlsoAnswered     []db.ModelCount // judge only: other models in the last 7 days, max 3
	ASRRunnerVersion string
	ASRModelSHA      string
	LastOK, LastFail time.Time // zero = never / unknown
	LastError        string    // full text (title tooltip); LastErrorShort on the card
	LastErrorShort   string
	FailingNow       int
	CoverageDone     int // judge
	CoverageTotal    int // judge
	Backlog          int // embeddings
	// HumanDecided / JevDecided / JevUndone / Proposed are the decide card's
	// tally of judge findings (decideTally); HumanDecided is nil when the
	// counts are unknown (the others are then 0).
	HumanDecided *int
	JevDecided   int
	JevUndone    int
	Proposed     int
	// PriorModelChanged (matchPredates only) says the pre-recipe answer is a
	// different model from the one now expected — "the model changed" rather
	// than "the recipe changed".
	PriorModelChanged bool
	// RecipeSince is the current recipe's current_recipes.updated_at (judge,
	// decide, scan); zero when the step has no current recipe.
	RecipeSince time.Time
	// Fn-role (decide/scan) call evidence from fn_calls under the current
	// recipe. FnKnown is false when the step has no current recipe yet.
	FnKnown        bool
	Fn             string
	Calls          int
	CacheHits      int
	Fallbacks      int
	Failures24h    int
	LastErrorClass string
	// Scanned is the scan card's chunk_scan rows under the current recipe;
	// ChunksTotal the library's chunk count (queue stats).
	Scanned     *int64
	ChunksTotal int
	// Stale is the step's stale_work count; nil when not tracked (no current
	// recipe) or unavailable. StaleKnown says the stale snapshot has loaded
	// (it is cached separately, 5 min); StalePending says its first load is
	// still running.
	Stale        *int64
	StaleTracked bool
	StaleKnown   bool
	StalePending bool
	// ModelAllowed is whether the requested model is on the LiteLLM virtual
	// key's allowlist (GET /key/info); nil when the endpoint is not behind a
	// readable LiteLLM key. A false here means every call 403s.
	ModelAllowed *bool
	// GatewayHost is the LiteLLM gateway host the allowlist came from.
	GatewayHost string
	// AllowState is ModelAllowed for templates: "allowed", "denied", or "".
	// AllowlistWarn adds a red warning line when the role is NOT allowed and
	// its health sub-line says something else (e.g. DOWN outranks it).
	AllowState    string
	AllowlistWarn bool
	// CountsKnown is false when no snapshot has ever loaded: answered/last
	// ok/stale render "—" instead of a misleading "never".
	CountsKnown bool
	// StatsKnown is false when the queue stats read failed (coverage/backlog
	// render "—"). LastKnown says whether LastOK/LastFail come from a source
	// that loaded (the snapshot for ASR/Judge, queue stats for Embeddings).
	StatsKnown bool
	LastKnown  bool
	// HasModel is false for roles with no model binding yet (format): its
	// card shows only the stale row.
	HasModel bool
}

// IsFnRole reports whether the role runs through internal/fn (decide, scan):
// its call evidence is fn_calls, not run_metrics.
func (c roleCard) IsFnRole() bool { return c.Key == "decide" || c.Key == "scan" }

// ExpectedRevisionShort is the pin's revision cut to 12 characters for display.
func (c roleCard) ExpectedRevisionShort() string { return shortID(c.ExpectedRevision) }

// ASRModelSHAShort is the .nemo sha cut to 12 characters for display.
func (c roleCard) ASRModelSHAShort() string { return shortID(c.ASRModelSHA) }

// CompareTarget is what an answer is compared against: the pin, else the
// requested id.
func (c roleCard) CompareTarget() string {
	if c.Expected != "" {
		return c.Expected
	}
	return c.Requested
}

// GatewayLabel renders the gateway for display ("" when the endpoint is direct).
func (c roleCard) GatewayLabel() string { return gatewayLabel(c.Gateway) }

// Where the judge's chat endpoint came from (judgeConfig.Source).
const (
	judgeSourceRegistry = "AI_ROLES.eval"
	judgeSourceEnv      = "EVAL_CHAT_*"
)

// judgeConfig is what the dashboard resolved for the judge at construction.
type judgeConfig struct {
	Configured bool
	Source     string // "AI_ROLES.eval" | "EVAL_CHAT_*" | ""
	Model      string // the requested model id
	Host       string // host[:port] (env-sourced only; the registry row has its own)
}

// roleInputs is everything buildRoleCards reads. Stats or Snap may be nil
// (their query failed); the cards then degrade to "—"/UNKNOWN, never to a
// fabricated "never ran".
type roleInputs struct {
	Cfg        *config.Config
	Judge      judgeConfig
	ASRServers []config.ASRServer
	Servers    []serverView
	Endpoints  []endpointView
	Stats      *db.QueueStats
	Snap       *modelsSnapshot // nil = never loaded
	Stale      *staleSnapshot  // nil = never loaded (cached separately)
	// StalePending is true while the stale snapshot's first load is running.
	StalePending bool
	// Gateways maps an AI_ENDPOINTS id to the LiteLLM gateway status probed
	// with that endpoint's key (absent = not behind LiteLLM / not probed).
	Gateways map[string]gatewayStatus
	Now      time.Time
	// ServersKnown is false when the runner observation read failed; the ASR
	// card is then UNKNOWN rather than a misleading NOT CONFIGURED.
	ServersKnown bool
	// Phase is the coordinator's pipeline phase ("" when unknown). In the
	// batch analyze phase the asr-runner is parked on purpose.
	Phase string
}

// buildRoleCards builds the six role cards in their fixed order.
func buildRoleCards(in roleInputs) []roleCard {
	cards := make([]roleCard, 0, len(roleDefs))
	for _, d := range roleDefs {
		c := roleCard{Key: d.Key, Title: d.Title, Step: d.Step, CountsKnown: in.Snap != nil, StatsKnown: in.Stats != nil}
		applyStale(&c, in.Stale, in.StalePending)
		switch d.Key {
		case "asr":
			c.HasModel, c.LastKnown = true, in.Snap != nil
			buildASRCard(&c, in)
		case "judge":
			c.HasModel, c.LastKnown = true, in.Snap != nil
			buildJudgeCard(&c, in)
		case "embeddings":
			c.HasModel, c.LastKnown = true, in.Stats != nil
			buildEmbedCard(&c, in)
		case "decide", "scan":
			c.HasModel, c.LastKnown = true, in.Snap != nil
			buildFnCard(&c, in)
		default:
			c.Health = health(roleNotConfigured, "no model role for this step yet")
		}
		cards = append(cards, c)
	}
	return cards
}

// applyStale sets the card's stale count from the stale snapshot: tracked only
// when the step has a current recipe (StaleItemCounts reports exactly those
// steps).
func applyStale(c *roleCard, stale *staleSnapshot, pending bool) {
	c.StalePending = pending && stale == nil
	if stale == nil {
		return
	}
	c.StaleKnown = true
	if n, ok := stale.Counts[c.Step]; ok {
		c.Stale, c.StaleTracked = &n, true
	}
}

// applyAllowlist checks the role's requested model against the LiteLLM key
// allowlist of the gateway its endpoint sits behind (when readable), and
// reconciles that verdict with the endpoint's own /v1/models probe, which is
// authoritative: LiteLLM's /v1/models lists exactly what the key may call.
//
//   - probe "ready" (the key lists the model) → allowed, whatever the
//     allowlist parse said (it may name access groups earmark can't expand).
//   - probe "model_not_loaded" (the key does not list it) → the allowlist's
//     deny stands.
//   - probe offline / not run → a deny is NOT asserted (unknown): without the
//     probe a non-matching entry may be an access group, and "every call
//     403s" is too strong a claim to make on a guess. An offline endpoint is
//     already DOWN.
//
// A team key with an empty model list inherits the team's models, which
// earmark's key cannot read → unknown. An empty requested model → unknown.
func applyAllowlist(c *roleCard, gws map[string]gatewayStatus, probe string) {
	gw, ok := gws[c.EndpointID]
	if !ok {
		return
	}
	c.GatewayHost = gw.Host
	c.ModelAllowed = reconcileAllowlist(gw, c.Requested, probe)
	if c.ModelAllowed != nil {
		c.AllowState = "denied"
		if *c.ModelAllowed {
			c.AllowState = "allowed"
		}
	}
}

// reconcileAllowlist is the pure verdict behind applyAllowlist.
func reconcileAllowlist(gw gatewayStatus, requested, probe string) *bool {
	if !gw.KeyInfoOK || strings.TrimSpace(requested) == "" {
		return nil
	}
	if gw.Key.TeamID != "" && len(gw.Key.Models) == 0 {
		return nil // inherits the team's models: not readable with earmark's key
	}
	v := modelAllowed(gw.Key.Models, requested)
	if v == nil || *v {
		return v
	}
	switch probe {
	case string(epStateReady):
		yes := true
		return &yes // the key lists it; the non-match was an access group
	case string(epStateModelMissing):
		return v // both say no
	default:
		return nil
	}
}

// finishAllowlist sets the warning flag once health is known.
func finishAllowlist(c *roleCard) {
	c.AllowlistWarn = c.AllowState == "denied" && c.Health.Sub != notAllowedSub(c.Requested)
}

// notAllowedSub is the shared wording for a model missing from the key
// allowlist — the misconfiguration that 403'd every judge call on 2026-10-05.
func notAllowedSub(model string) string {
	return model + " is not on earmark's LiteLLM key allowlist — every call 403s"
}

func buildJudgeCard(c *roleCard, in roleInputs) {
	c.Configured = in.Judge.Configured
	pin := in.Cfg.ModelPin(c.Step)
	c.Expected, c.ExpectedRevision = pin.ExpectedModel, pin.Revision

	probe := "" // "" = not probed (env-sourced judge, or none)
	host := in.Judge.Host
	if ep, ok := evalEndpoint(in.Cfg); ok {
		c.EndpointID, c.Requested = ep.ID, ep.Model
		if v := findEndpointView(in.Endpoints, ep.ID); v != nil {
			probe, host = v.StateToken, v.HostOnly
			c.Gateway, c.GatewayInferred = v.Gateway, v.GatewayInferred
		}
	} else if in.Judge.Configured {
		c.EndpointID, c.Requested = judgeSourceEnv, in.Judge.Model
		c.Gateway, c.GatewayInferred = gatewayFor("", host)
	}

	if in.Snap != nil {
		a := in.Snap.Activity
		c.LastOK, c.LastFail = derefTime(a.EvalLastOK), derefTime(a.EvalLastFail)
		c.LastError = a.EvalLastError
		c.LastErrorShort = truncateRunes(a.EvalLastError, maxCardErrorLen)
		c.FailingNow = a.EvalFailingNow
		c.Answered = a.EvalLastModel
		c.AnsweredMatch = answeredMatch(c.Answered, c.CompareTarget(), !c.LastOK.IsZero())
		if cur := in.Snap.currentRecipe(c.Step); cur != nil {
			c.RecipeSince = cur.UpdatedAt
		}
		gateOnRecipe(c, derefTime(a.EvalLastModelAt))
		for _, m := range a.EvalModels7d {
			if len(c.AlsoAnswered) == 3 {
				break
			}
			if !strings.EqualFold(m.Model, c.Answered) {
				c.AlsoAnswered = append(c.AlsoAnswered, m)
			}
		}
	}
	if in.Stats != nil {
		c.CoverageDone, c.CoverageTotal = in.Stats.EvalCoverageDone, in.Stats.Done
	}
	applyAllowlist(c, in.Gateways, probe)
	c.Health = judgeHealth(*c, probe, host, in.Stats != nil, in.Now)
	finishAllowlist(c)
}

// gateOnRecipe sets matchPredates when the newest answer (answeredAt) is older
// than the current recipe: an answer recorded before the model or prompt
// changed says nothing about whether the current request falls back, so it
// must not read as "≠ expected". An unknown answer time or recipe keeps the
// comparison (a real fallback must still flag).
func gateOnRecipe(c *roleCard, answeredAt time.Time) {
	if c.RecipeSince.IsZero() || answeredAt.IsZero() || !answeredAt.Before(c.RecipeSince) {
		return
	}
	switch c.AnsweredMatch {
	case matchOK, matchMismatch, matchUnchecked:
		c.PriorModelChanged = c.CompareTarget() != "" && !sameModel(c.Answered, c.CompareTarget())
		c.AnsweredMatch = matchPredates
	}
}

// judgeHealth is the Judge precedence table (first match wins). probe is the
// endpoint's liveness token, "" when the judge is not probed (env-sourced).
func judgeHealth(c roleCard, probe, host string, statsKnown bool, now time.Time) roleHealth {
	gap := c.CoverageTotal - c.CoverageDone
	switch {
	case !c.Configured:
		return health(roleNotConfigured, "no AI_ROLES.eval binding and no EVAL_CHAT_* — judging is off")
	case probe == string(epStateOffline):
		return health(roleDown, "gateway unreachable (GET /models failed) — "+orDash(host))
	case c.ModelAllowed != nil && !*c.ModelAllowed:
		return health(roleDegraded, notAllowedSub(c.Requested))
	case c.CountsKnown && !c.LastFail.IsZero() && c.LastFail.After(c.LastOK):
		return health(roleFailing, "last attempt failed "+humanizeSince(now.Sub(c.LastFail))+" — see error below")
	case probe == string(epStateModelMissing):
		return health(roleDegraded, modelMissingSub(c))
	case c.CountsKnown && c.AnsweredMatch == matchMismatch:
		return health(roleDegraded, fmt.Sprintf("answered by %s, expected %s — new findings are stamped stale (fallback route?)",
			c.Answered, c.CompareTarget()))
	case !c.CountsKnown || !statsKnown:
		return health(roleUnknown, "call outcomes unavailable — counts query failed")
	case gap > 0 && c.LastOK.IsZero():
		return health(roleDegraded, fmt.Sprintf("no successful judge call yet; %s transcripts unjudged", commafy(gap)))
	case gap > 0 && now.Sub(c.LastOK) > judgeQuietAfter:
		return health(roleDegraded, fmt.Sprintf("no successful judge call in %s; %s transcripts unjudged",
			sinceShort(now.Sub(c.LastOK)), commafy(gap)))
	case gap <= 0 && c.CoverageTotal == 0:
		return health(roleIdle, "nothing to judge yet")
	case gap <= 0:
		return health(roleIdle, "every transcript judged")
	default:
		return health(roleHealthy, "judged "+humanizeSince(now.Sub(c.LastOK)))
	}
}

func buildEmbedCard(c *roleCard, in roleInputs) {
	pin := in.Cfg.ModelPin(c.Step)
	c.Expected, c.ExpectedRevision = pin.ExpectedModel, pin.Revision
	probe, host := "", ""
	if in.Cfg != nil {
		if ep, ok := in.Cfg.EmbeddingsEndpoint(); ok {
			c.Configured = true
			c.EndpointID, c.Requested = ep.ID, ep.Model
			if v := findEndpointView(in.Endpoints, ep.ID); v != nil {
				probe, host = v.StateToken, v.HostOnly
				c.Gateway, c.GatewayInferred = v.Gateway, v.GatewayInferred
			}
		}
	}
	hasRuns := false
	if in.Stats != nil {
		c.Backlog = in.Stats.EmbedBacklog
		c.LastOK = derefTime(in.Stats.LastEmbedAt)
		hasRuns = !c.LastOK.IsZero()
	}
	if in.Snap != nil {
		c.Answered = in.Snap.Activity.EmbedLastModel
		c.AnsweredMatch = answeredMatch(c.Answered, c.CompareTarget(), hasRuns)
	}
	applyAllowlist(c, in.Gateways, probe)
	c.Health = embedHealth(*c, probe, host, in.Stats != nil, in.Now)
	finishAllowlist(c)
}

// modelMissingSub words a model_not_loaded probe. Behind LiteLLM, /v1/models
// lists only the virtual key's allowed models, so a missing model is an
// allowlist problem (calls 403), not an unloaded model.
func modelMissingSub(c roleCard) string {
	if c.Gateway == "litellm" {
		return c.Requested + " is not on earmark's LiteLLM key allowlist (403 on call)"
	}
	return "endpoint does not list " + c.Requested
}

// embedHealth is the Embeddings precedence table (first match wins).
func embedHealth(c roleCard, probe, host string, statsKnown bool, now time.Time) roleHealth {
	switch {
	case !c.Configured:
		return health(roleNotConfigured, "no embeddings endpoint — set AI_ENDPOINTS + AI_ROLES.embeddings")
	case probe == string(epStateOffline):
		return health(roleDown, "endpoint unreachable (GET /models failed) — "+orDash(host))
	case c.ModelAllowed != nil && !*c.ModelAllowed:
		return health(roleDegraded, notAllowedSub(c.Requested))
	case probe == string(epStateModelMissing):
		return health(roleDegraded, modelMissingSub(c))
	case c.CountsKnown && c.AnsweredMatch == matchMismatch:
		return health(roleDegraded, fmt.Sprintf("embedded by %s, expected %s — new chunks are stamped stale",
			c.Answered, c.CompareTarget()))
	case !statsKnown:
		return health(roleUnknown, "queue stats unavailable")
	case c.Backlog > 0 && c.LastOK.IsZero():
		return health(roleDegraded, fmt.Sprintf("%s waiting, nothing embedded yet", commafy(c.Backlog)))
	case c.Backlog > 0 && now.Sub(c.LastOK) > embedQuietAfter:
		return health(roleDegraded, fmt.Sprintf("%s waiting, nothing embedded in %s",
			commafy(c.Backlog), sinceShort(now.Sub(c.LastOK))))
	case c.Backlog <= 0:
		return health(roleIdle, "nothing waiting to embed")
	default:
		return health(roleHealthy, fmt.Sprintf("embedded %s; %s waiting", humanizeSince(now.Sub(c.LastOK)), commafy(c.Backlog)))
	}
}

func buildASRCard(c *roleCard, in roleInputs) {
	c.Configured = len(in.ASRServers) > 0
	c.Requested = primaryASRModel(in.ASRServers)
	pin := in.Cfg.ModelPin(c.Step)
	c.Expected, c.ExpectedRevision = pin.ExpectedModel, pin.Revision
	if in.Snap != nil {
		if l := in.Snap.Activity.ASRLatest; l != nil {
			c.Answered = l.Model
			c.ASRRunnerVersion = derefStr(l.RunnerVersion)
			c.ASRModelSHA = derefStr(l.ModelSHA256)
			c.LastOK = l.At
		}
		c.AnsweredMatch = answeredMatch(c.Answered, c.CompareTarget(), !c.LastOK.IsZero())
	}
	if !in.ServersKnown {
		c.Health = health(roleUnknown, "runner observation unavailable — see server logs")
		return
	}
	pending, pendingKnown := 0, in.Stats != nil
	if pendingKnown {
		pending = in.Stats.Pending
	}
	c.Health = asrHealth(in.Servers, pending, pendingKnown, in.Phase)
}

// primaryASRModel is the model the primary ASR_SERVERS entry expects (else the
// first entry's), "" with no configured servers.
func primaryASRModel(servers []config.ASRServer) string {
	for _, s := range servers {
		if s.Role == "primary" && s.Model != "" {
			return s.Model
		}
	}
	for _, s := range servers {
		if s.Model != "" {
			return s.Model
		}
	}
	return ""
}

// asrHealth aggregates the runner cards' states (first match wins). pending is
// the queue depth: GPUs held by games only degrade the role when work waits.
func asrHealth(servers []serverView, pending int, pendingKnown bool, phase string) roleHealth {
	first := func(token string) *serverView {
		for i := range servers {
			if servers[i].State.Token == token {
				return &servers[i]
			}
		}
		return nil
	}
	if v := first("transcribing"); v != nil {
		return health(roleHealthy, "transcribing on "+v.Name)
	}
	if v := first("stalled"); v != nil {
		return health(roleFailing, v.Name+" holds a claim with a stale heartbeat")
	}
	if v := first("ready"); v != nil {
		return health(roleHealthy, "ready on "+v.Name)
	}
	probed, busy, offline := 0, 0, 0
	var heldView, stoppedView *serverView
	for i := range servers {
		if !servers[i].Probed {
			continue
		}
		probed++
		switch servers[i].State.Token {
		case "busy":
			busy++
			if servers[i].GPUState == "available" {
				if stoppedView == nil {
					stoppedView = &servers[i]
				}
			} else if heldView == nil {
				heldView = &servers[i]
			}
		case "offline":
			offline++
		}
	}
	if probed > 0 && busy > 0 && busy+offline == probed {
		// No probed server can transcribe now. A free GPU whose asr-runner is
		// stopped needs the operator regardless of the queue; GPUs held by games
		// only degrade the role while work waits (they come back by themselves).
		if stoppedView != nil && phase == db.PhaseAnalyze {
			// `earmark batch` parks the runner so the judge can use the GPU.
			return health(roleIdle, "asr-runner parked on "+stoppedView.Name+" for the batch analyze phase")
		}
		if stoppedView != nil {
			return health(roleDegraded, "asr-runner stopped on "+stoppedView.Name+" — start it before queueing work")
		}
		held := heldView.Name + ": " + gpuHeldWording(heldView.GPUState)
		if pendingKnown && pending == 0 {
			return health(roleIdle, "nothing queued; "+held)
		}
		return health(roleDegraded, held+" — jobs wait")
	}
	if probed > 0 && offline == probed {
		return health(roleDown, "every probed ASR server is unreachable")
	}
	if v := first("idle"); v != nil {
		return health(roleIdle, v.Name+" idle — "+strings.TrimPrefix(v.State.Sub, "idle — "))
	}
	if len(servers) == 0 {
		return health(roleNotConfigured, "no ASR_SERVERS and no runner has claimed a job")
	}
	return health(roleUnknown, "no runner activity observed yet")
}

func gpuHeldWording(state string) string {
	switch state {
	case "gaming":
		return "GPU held by a game (gaming)"
	case "evicting":
		return "evicting other GPU work"
	default:
		return "GPU busy"
	}
}

// answeredMatch compares what answered against what was expected.
func answeredMatch(answered, expected string, hasRuns bool) string {
	switch {
	case answered == "" && hasRuns:
		return matchUnreported
	case answered == "":
		return matchNone
	case expected == "":
		return matchUnchecked
	case sameModel(answered, expected):
		return matchOK
	default:
		return matchMismatch
	}
}

// fnRoleEndpoint resolves the AI_ROLES binding of a decide/scan card.
func fnRoleEndpoint(cfg *config.Config, key string) (config.AIEndpoint, bool) {
	if cfg == nil {
		return config.AIEndpoint{}, false
	}
	if key == "scan" {
		return cfg.ScanEndpoint()
	}
	return cfg.DecideEndpoint()
}

// buildFnCard builds the Decide or Scan card: a System One endpoint bound in
// AI_ROLES, whose call outcomes are the fn_calls rows of its current recipe
// (db.FnRoleActivity). Decide also tallies the judge findings by decider.
func buildFnCard(c *roleCard, in roleInputs) {
	pin := in.Cfg.ModelPin(c.Step)
	c.Expected, c.ExpectedRevision = pin.ExpectedModel, pin.Revision
	probe, host := "", ""
	if ep, ok := fnRoleEndpoint(in.Cfg, c.Key); ok {
		c.Configured = true
		c.EndpointID, c.Requested = ep.ID, ep.Model
		if v := findEndpointView(in.Endpoints, ep.ID); v != nil {
			probe, host = v.StateToken, v.HostOnly
			c.Gateway, c.GatewayInferred = v.Gateway, v.GatewayInferred
		}
	}
	if in.Snap != nil && in.Snap.FnRolesErr != nil {
		c.CountsKnown, c.LastKnown = false, false // only this query failed: unknown, not "never"
	}
	if in.Snap != nil {
		if cur := in.Snap.currentRecipe(c.Step); cur != nil {
			c.RecipeSince = cur.UpdatedAt
		}
		if a := in.Snap.fnRole(c.Step); a != nil {
			c.FnKnown, c.Fn = true, a.Fn
			c.LastOK, c.LastFail = derefTime(a.LastOK), derefTime(a.LastFail)
			c.Calls, c.CacheHits, c.Fallbacks, c.Failures24h = a.Calls, a.CacheHits, a.Fallbacks, a.Failures24h
			c.LastErrorClass = a.LastErrorClass
			c.LastError, c.LastErrorShort = a.LastErrorClass, a.LastErrorClass
			c.Answered = a.LastModel
			c.Scanned = a.Outputs
		}
		c.AnsweredMatch = answeredMatch(c.Answered, c.CompareTarget(), !c.LastOK.IsZero())
		if c.Key == "decide" {
			t := decideTally(in.Snap.Findings)
			c.HumanDecided, c.JevDecided, c.JevUndone, c.Proposed = &t.Human, t.Jev, t.JevUndone, t.Proposed
		}
	}
	if in.Stats != nil {
		c.ChunksTotal = in.Stats.Chunks
	}
	applyAllowlist(c, in.Gateways, probe)
	c.Health = fnRoleHealth(*c, probe, host, in.Now)
	finishAllowlist(c)
}

// fnRoleHealth is the Decide/Scan precedence table (first match wins). Both
// run on demand (`earmark decide`, `earmark scan`), so a quiet role is idle,
// never degraded for want of recent calls.
func fnRoleHealth(c roleCard, probe, host string, now time.Time) roleHealth {
	switch {
	case !c.Configured:
		return health(roleNotConfigured, "no AI_ROLES."+c.Key+" binding — the "+c.Step+" step is off")
	case probe == string(epStateOffline):
		return health(roleDown, "endpoint unreachable (GET /models failed) — "+orDash(host))
	case c.ModelAllowed != nil && !*c.ModelAllowed:
		return health(roleDegraded, notAllowedSub(c.Requested))
	case c.CountsKnown && !c.LastFail.IsZero() && c.LastFail.After(c.LastOK):
		return health(roleFailing, fmt.Sprintf("last %s call failed %s (%s)",
			c.Fn, humanizeSince(now.Sub(c.LastFail)), orDash(c.LastErrorClass)))
	case probe == string(epStateModelMissing):
		return health(roleDegraded, modelMissingSub(c))
	case c.CountsKnown && c.AnsweredMatch == matchMismatch:
		return health(roleDegraded, fmt.Sprintf("answered by %s, expected %s — the reply is stored but never served (fallback route?)",
			c.Answered, c.CompareTarget()))
	case !c.CountsKnown:
		return health(roleUnknown, "call outcomes unavailable — counts query failed")
	case !c.FnKnown:
		return health(roleUnknown, "no current "+c.Step+" recipe yet — earmark monitor registers it at startup")
	case c.LastOK.IsZero():
		return health(roleIdle, "no calls under the current recipe yet")
	case c.Key == "decide" && c.Proposed > 0:
		return health(roleHealthy, fmt.Sprintf("last call ok %s; %s proposed findings await a decision",
			humanizeSince(now.Sub(c.LastOK)), commafy(c.Proposed)))
	default:
		return health(roleHealthy, "last call ok "+humanizeSince(now.Sub(c.LastOK)))
	}
}

// decisionTally is the decide card's split of judge findings by decider.
type decisionTally struct {
	Human     int // decided (accepted/rejected/applied/reverted) by a person, incl. unattributed
	Jev       int // decided by a decide recipe (decided_by jev:<recipe>)
	JevUndone int // last moved by `earmark decide revert` (revert:jev:<recipe>), any state
	Proposed  int // awaiting a decision (undone ones included)
}

// decideTally classifies the judge findings by decider. Only a decided state
// with a person's (or no) decided_by is a human decision: jev:<recipe> is the
// decide step, revert:jev:<recipe> its undo.
func decideTally(fs []db.FindingsModelCount) decisionTally {
	var t decisionTally
	for _, f := range fs {
		if f.Decider == db.DeciderJevRevert {
			t.JevUndone += f.Count
		}
		switch {
		case f.PatchState == "proposed":
			t.Proposed += f.Count
		case !isDecidedState(f.PatchState):
		case f.Decider == db.DeciderJev:
			t.Jev += f.Count
		case isHumanDecider(f.Decider):
			t.Human += f.Count
		}
	}
	return t
}

// isHumanDecider reports whether a decided_by class is a person's.
func isHumanDecider(d string) bool { return d == db.DeciderHuman || d == db.DeciderNone }

func isDecidedState(s string) bool {
	switch s {
	case "accepted", "rejected", "applied", "reverted":
		return true
	}
	return false
}

// ─── Recipes & stale work ─────────────────────────────────────────────────────

// recipeRow is one tracked step (a step with a current recipe) in the Recipes
// & stale work table.
type recipeRow struct {
	Step, RoleTitle       string
	RecipeID, RecipeShort string
	Model, PromptVersion  string
	StepVersion           int
	Since                 time.Time
	Stale                 *int64 // nil = unknown (StaleKnown false) or not counted
	StaleKnown            bool
	ConvergeNote          string
}

// convergeNotes says how each step's stale rows get redone.
var convergeNotes = map[string]string{
	recipe.StepEmbed:   "re-embedded by the worker",
	recipe.StepPropose: "only by re-judging",
}

// untrackedNotes qualifies an untracked step in the "Not tracked:" line.
var untrackedNotes = map[string]string{
	recipe.StepASR: "provenance per transcript",
}

// buildRecipeRows splits the recipe steps, in canonical order, into the
// tracked rows (a current recipe exists, so stale work is counted) and the
// untracked step labels for the one-line "Not tracked:" note. snap must be
// non-nil (the caller renders "counts unavailable" otherwise); stale may be nil
// (its counts then render as unknown, independently of the recipe columns).
func buildRecipeRows(snap *modelsSnapshot, stale *staleSnapshot) (rows []recipeRow, untracked []string) {
	byStep := map[string]db.CurrentRecipe{}
	for _, r := range snap.Recipes {
		byStep[r.Step] = r
	}
	for _, step := range recipe.Steps {
		r, ok := byStep[step]
		if !ok {
			label := step
			if n := untrackedNotes[step]; n != "" {
				label += " (" + n + ")"
			}
			untracked = append(untracked, label)
			continue
		}
		row := recipeRow{
			Step: step, RoleTitle: roleTitleForStep(step),
			RecipeID: r.RecipeID, RecipeShort: shortID(r.RecipeID),
			Model:         cmp.Or(r.ModelResolved, r.ModelAlias),
			PromptVersion: r.PromptVersion, StepVersion: r.StepVersion, Since: r.UpdatedAt,
		}
		if stale != nil {
			row.StaleKnown = true
			if n, ok := stale.Counts[step]; ok {
				row.Stale = &n
				if n > 0 {
					row.ConvergeNote = convergeNotes[step]
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, untracked
}

// ─── Judge output ─────────────────────────────────────────────────────────────

// findingsModelRow is one answering model's judge findings by outcome.
// Decided findings split by decider: a person (mcp:, cli:, other, or
// unattributed) or the decide step (jev:<recipe>, and revert:jev:<recipe> for
// its undos).
type findingsModelRow struct {
	Model                                            string
	Proposed, Unanchorable, DecidedHuman, DecidedJev int
	Other                                            int
	Total                                            int
}

// buildFindingsRows pivots (model, patch_state, decider) counts into one row
// per model, sorted by total descending, plus the totals row. "Other" is
// superseded plus the patch state 'stale' (a quarantined replay, not
// stale_work).
func buildFindingsRows(fs []db.FindingsModelCount) ([]findingsModelRow, findingsModelRow) {
	idx := map[string]int{}
	var rows []findingsModelRow
	total := findingsModelRow{Model: "total"}
	for _, f := range fs {
		i, ok := idx[f.Model]
		if !ok {
			rows = append(rows, findingsModelRow{Model: f.Model})
			i = len(rows) - 1
			idx[f.Model] = i
		}
		r := &rows[i]
		switch {
		case f.PatchState == "proposed":
			r.Proposed += f.Count
			total.Proposed += f.Count
		case f.PatchState == "unanchorable":
			r.Unanchorable += f.Count
			total.Unanchorable += f.Count
		case isDecidedState(f.PatchState) && isHumanDecider(f.Decider):
			r.DecidedHuman += f.Count
			total.DecidedHuman += f.Count
		case isDecidedState(f.PatchState):
			r.DecidedJev += f.Count
			total.DecidedJev += f.Count
		default:
			r.Other += f.Count
			total.Other += f.Count
		}
		r.Total += f.Count
		total.Total += f.Count
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Total != rows[j].Total {
			return rows[i].Total > rows[j].Total
		}
		return rows[i].Model < rows[j].Model
	})
	return rows, total
}

// ─── ASR provenance ───────────────────────────────────────────────────────────

// asrProvenanceRow is one runner-build group of transcripts.
type asrProvenanceRow struct {
	Model, RunnerVersion, SHA, SHAShort string
	Reported                            bool // false = the NULL (pre-provenance) group
	Count                               int
	First, Last                         time.Time
}

func buildASRRows(gs []db.ASRProvenanceGroup) []asrProvenanceRow {
	rows := make([]asrProvenanceRow, 0, len(gs))
	for _, g := range gs {
		sha := derefStr(g.SHA)
		rows = append(rows, asrProvenanceRow{
			Model: g.Model, RunnerVersion: derefStr(g.RunnerVersion), SHA: sha, SHAShort: shortID(sha),
			Reported: g.RunnerVersion != nil, Count: g.Count, First: g.First, Last: g.Last,
		})
	}
	return rows
}

// ─── Shared helpers ───────────────────────────────────────────────────────────

// modelsMatch reports whether two model ids name the same model: case-
// insensitive, tolerating Ollama's `:latest` and `:<tag>` suffixes (a
// configured bare `nomic-embed-text` matches a reported
// `nomic-embed-text:latest`, and vice versa). Empty input never matches.
func modelsMatch(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	return a == b ||
		strings.TrimSuffix(a, ":latest") == strings.TrimSuffix(b, ":latest") ||
		strings.HasPrefix(a, b+":") ||
		strings.HasPrefix(b, a+":")
}

// sameModel is the strict comparison for "did the expected model answer":
// case-insensitive, a router's provider prefix is ignored (eval.SameModel:
// LiteLLM answers `claude-haiku-5-5` for a requested
// `anthropic/claude-haiku-5-5`), and Ollama's implicit `:latest` equals the
// bare name, but a bare pin does NOT match an arbitrary tag (`qwen3.8` ≠
// `qwen3.8:14b`) — unlike modelsMatch, which is the lenient /models presence
// check.
func sameModel(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	return eval.SameModel(strings.TrimSuffix(a, ":latest"), strings.TrimSuffix(b, ":latest"))
}

// modelAllowed reports whether model is on a LiteLLM key allowlist: an empty
// list or "*" / "all-proxy-models" allows everything; "provider/*" (any
// trailing "*") is a prefix wildcard; otherwise an exact (case- and
// `:latest`-insensitive) match. nil when the list
// uses a scope earmark cannot resolve from its own key ("all-team-models").
func modelAllowed(allowed []string, model string) *bool {
	yes, no := true, false
	if len(allowed) == 0 {
		return &yes
	}
	m := strings.ToLower(strings.TrimSpace(model))
	unknown := false
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "*" || a == "all-proxy-models":
			return &yes
		case a == "all-team-models":
			unknown = true
		case strings.HasSuffix(a, "*") && strings.HasPrefix(m, strings.TrimSuffix(a, "*")):
			return &yes
		case a != "" && strings.TrimSuffix(a, ":latest") == strings.TrimSuffix(m, ":latest"):
			// Exact (modulo :latest), NOT sameModel: LiteLLM matches the
			// requested name, provider prefix included.
			return &yes
		}
	}
	if unknown {
		return nil
	}
	return &no
}

// gatewayFor resolves the gateway an endpoint sits behind: the declared
// AI_ENDPOINTS gateway when set, else "litellm" inferred from a host name
// containing "litellm" or "llm-gateway". Display only.
func gatewayFor(declared, baseURLOrHost string) (gateway string, inferred bool) {
	if declared != "" {
		return declared, false
	}
	h := strings.ToLower(hostOnly(baseURLOrHost))
	if strings.Contains(h, "litellm") || strings.Contains(h, "llm-gateway") {
		return "litellm", true
	}
	return "", false
}

// gatewayLabel is the display name of a gateway token ("" stays "").
func gatewayLabel(g string) string {
	if g == "litellm" {
		return "LiteLLM"
	}
	return g
}

func evalEndpoint(cfg *config.Config) (config.AIEndpoint, bool) {
	if cfg == nil {
		return config.AIEndpoint{}, false
	}
	return cfg.EvalEndpoint()
}

func findEndpointView(eps []endpointView, id string) *endpointView {
	for i := range eps {
		if eps[i].ID == id {
			return &eps[i]
		}
	}
	return nil
}

// shortID cuts a hash/id to 12 characters for display.
func shortID(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// truncateRunes cuts s to at most n runes, marking the cut with "…".
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// sinceShort is humanizeSince without the trailing " ago" ("5h").
func sinceShort(d time.Duration) string {
	return strings.TrimSuffix(humanizeSince(d), " ago")
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// modelsFuncs are the Models fragment's extra template helpers.
var modelsFuncs = template.FuncMap{
	// plural picks the singular or plural noun for n.
	"plural": func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return many
	},
	// runnerRow pairs a runner view with the table's column toggles.
	"runnerRow": func(v serverView, showRuntime, showCaps bool) runnerTableRow {
		return runnerTableRow{V: v, ShowRuntime: showRuntime, ShowCaps: showCaps}
	},
	// endpointRow pairs an endpoint view with the Options column toggle.
	"endpointRow": func(v endpointView, showOptions bool) endpointTableRow {
		return endpointTableRow{V: v, ShowOptions: showOptions}
	},
}
