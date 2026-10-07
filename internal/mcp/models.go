package mcp

import (
	"cmp"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// ─── Models page: the role board (CONTRACT §2.14) ─────────────────────────────
//
// The page answers "is each pipeline role being done, by what, and is its
// output current?" — one card per role (ASR, Judge, Embeddings, Decide,
// Format), then the supporting detail: recipes + stale counts, ASR runners,
// the AI endpoint registry, and judge output by answering model.
//
// Endpoint liveness (GET /models) is NOT call success: a gateway that lists the
// alias but 401s every chat call is READY in the endpoint table and FAILING on
// the Judge card. Role health therefore folds in call outcomes from run_metrics.
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
	Glyph string // ✓ ● ▲ ✗ ○ ?
	Class string // state-running|state-idle|state-busy|state-stalled|state-unknown
	Dot   string // green|blue|amber|red|grey
	Sub   string // one-sentence reason
}

var roleHealthMeta = map[string]roleHealth{
	roleHealthy:       {Token: roleHealthy, Label: "HEALTHY", Glyph: "✓", Class: "state-running", Dot: "green"},
	roleIdle:          {Token: roleIdle, Label: "IDLE", Glyph: "●", Class: "state-idle", Dot: "blue"},
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

// roleDef is one row of the static role table. decide and format have no
// model role yet (AIRoles is {embeddings, eval}); when one lands, its card
// gains a resolver here.
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
	{Key: "format", Title: "Format", Step: recipe.StepFormat},
}

// roleTitleForStep labels a recipe step with the role that runs it ("" for
// propagate/scan, which have no model role).
func roleTitleForStep(step string) string {
	for _, d := range roleDefs {
		if d.Step == step {
			return d.Title
		}
	}
	return ""
}

// modelCount is one other model that answered the judge recently.
type modelCount struct {
	Model string
	Count int
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
	AnsweredMatch    string       // match|mismatch|unreported|none|unchecked
	AlsoAnswered     []modelCount // judge only: other models in the last 7 days, max 3
	ASRRunnerVersion string
	ASRModelSHA      string
	LastOK, LastFail time.Time // zero = never / unknown
	LastError        string    // full text (title tooltip); LastErrorShort on the card
	LastErrorShort   string
	FailingNow       int
	CoverageDone     int // judge
	CoverageTotal    int // judge
	Backlog          int // embeddings
	HumanDecided     *int
	// Stale is the step's stale_work count; nil when not tracked (no current
	// recipe) or unavailable (CountsKnown false).
	Stale        *int64
	StaleTracked bool
	// CountsKnown is false when no snapshot has ever loaded: answered/last
	// ok/stale render "—" instead of a misleading "never".
	CountsKnown bool
	// StatsKnown is false when the queue stats read failed (coverage/backlog
	// render "—"). LastKnown says whether LastOK/LastFail come from a source
	// that loaded (the snapshot for ASR/Judge, queue stats for Embeddings).
	StatsKnown bool
	LastKnown  bool
	// HasModel is false for roles with no model binding yet (decide, format):
	// their card shows only the stale and decided rows.
	HasModel bool
}

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
	Snap       *modelsSnapshot
	Now        time.Time
	// ServersKnown is false when the runner observation read failed; the ASR
	// card is then UNKNOWN rather than a misleading NOT CONFIGURED.
	ServersKnown bool
}

// buildRoleCards builds the five role cards in their fixed order.
func buildRoleCards(in roleInputs) []roleCard {
	cards := make([]roleCard, 0, len(roleDefs))
	for _, d := range roleDefs {
		c := roleCard{Key: d.Key, Title: d.Title, Step: d.Step, CountsKnown: in.Snap != nil, StatsKnown: in.Stats != nil}
		applyStale(&c, in.Snap)
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
		default:
			c.Health = health(roleNotConfigured, "no model role for this step yet")
			if d.Key == "decide" && in.Snap != nil {
				n := humanDecided(in.Snap.Findings)
				c.HumanDecided = &n
			}
		}
		cards = append(cards, c)
	}
	return cards
}

// applyStale sets the card's stale count from the snapshot: tracked only when
// the step has a current recipe (StaleItemCounts reports exactly those steps).
func applyStale(c *roleCard, snap *modelsSnapshot) {
	if snap == nil {
		return
	}
	if n, ok := snap.Stale[c.Step]; ok {
		c.Stale, c.StaleTracked = &n, true
	}
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
		for _, m := range a.EvalModels7d {
			if len(c.AlsoAnswered) == 3 {
				break
			}
			if !strings.EqualFold(m.Model, c.Answered) {
				c.AlsoAnswered = append(c.AlsoAnswered, modelCount{Model: m.Model, Count: m.Count})
			}
		}
	}
	if in.Stats != nil {
		c.CoverageDone, c.CoverageTotal = in.Stats.EvalCoverageDone, in.Stats.Done
	}
	c.Health = judgeHealth(*c, probe, host, in.Stats != nil, in.Now)
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
	case c.CountsKnown && !c.LastFail.IsZero() && c.LastFail.After(c.LastOK):
		return health(roleFailing, "last attempt failed "+humanizeSince(now.Sub(c.LastFail))+" — see error below")
	case probe == string(epStateModelMissing):
		return health(roleDegraded, "gateway does not list "+c.Requested)
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
	c.Health = embedHealth(*c, probe, host, in.Stats != nil, in.Now)
}

// embedHealth is the Embeddings precedence table (first match wins).
func embedHealth(c roleCard, probe, host string, statsKnown bool, now time.Time) roleHealth {
	switch {
	case !c.Configured:
		return health(roleNotConfigured, "no embeddings endpoint — set AI_ENDPOINTS + AI_ROLES.embeddings")
	case probe == string(epStateOffline):
		return health(roleDown, "endpoint unreachable (GET /models failed) — "+orDash(host))
	case probe == string(epStateModelMissing):
		return health(roleDegraded, "endpoint does not list "+c.Requested)
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
	c.Health = asrHealth(in.Servers, pending, pendingKnown)
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
func asrHealth(servers []serverView, pending int, pendingKnown bool) roleHealth {
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
	var busyView *serverView
	for i := range servers {
		if !servers[i].Probed {
			continue
		}
		probed++
		switch servers[i].State.Token {
		case "busy":
			busy++
			if busyView == nil {
				busyView = &servers[i]
			}
		case "offline":
			offline++
		}
	}
	// No probed server is usable and at least one is held: degraded, not down —
	// the GPU comes back when the game exits.
	if probed > 0 && busy > 0 && busy+offline == probed {
		held := busyView.Name + ": " + gpuHeldWording(busyView.GPUState)
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
	case "available":
		return "GPU free but asr-runner stopped"
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
	case modelsMatch(answered, expected):
		return matchOK
	default:
		return matchMismatch
	}
}

// humanDecided counts judge findings a human decided (accepted, rejected,
// applied, reverted).
func humanDecided(fs []db.FindingsModelCount) int {
	n := 0
	for _, f := range fs {
		if isDecidedState(f.PatchState) {
			n += f.Count
		}
	}
	return n
}

func isDecidedState(s string) bool {
	switch s {
	case "accepted", "rejected", "applied", "reverted":
		return true
	}
	return false
}

// ─── Recipes & stale work ─────────────────────────────────────────────────────

// recipeRow is one step in the Recipes & stale work table.
type recipeRow struct {
	Step, RoleTitle       string
	Current               bool
	RecipeID, RecipeShort string
	Model, PromptVersion  string
	StepVersion           int
	Since                 time.Time
	Pin                   string // expected model; "" unpinned
	PinMatch              string // match|mismatch|unpinned ("" with no current recipe)
	Stale                 *int64 // nil = not tracked
	ConvergeNote          string
	NoneNote              string // why there is no current recipe
}

// convergeNotes says how each step's stale rows get redone.
var convergeNotes = map[string]string{
	recipe.StepEmbed:   "re-embedded by the worker",
	recipe.StepPropose: "only by re-judging",
}

// buildRecipeRows lists every recipe step in canonical order. snap must be
// non-nil (the caller renders "counts unavailable" otherwise).
func buildRecipeRows(cfg *config.Config, snap *modelsSnapshot) []recipeRow {
	byStep := map[string]db.CurrentRecipe{}
	for _, r := range snap.Recipes {
		byStep[r.Step] = r
	}
	rows := make([]recipeRow, 0, len(recipe.Steps))
	for _, step := range recipe.Steps {
		row := recipeRow{Step: step, RoleTitle: roleTitleForStep(step), Pin: cfg.ModelPin(step).ExpectedModel}
		if r, ok := byStep[step]; ok {
			row.Current = true
			row.RecipeID, row.RecipeShort = r.RecipeID, shortID(r.RecipeID)
			row.Model = cmp.Or(r.ModelResolved, r.ModelAlias)
			row.PromptVersion, row.StepVersion, row.Since = r.PromptVersion, r.StepVersion, r.UpdatedAt
			switch {
			case row.Pin == "":
				row.PinMatch = "unpinned"
			case modelsMatch(row.Model, row.Pin):
				row.PinMatch = matchOK
			default:
				row.PinMatch = matchMismatch
			}
		} else if step == recipe.StepASR {
			row.NoneNote = "none — the runner reports provenance per transcript; staleness not tracked"
		} else {
			row.NoneNote = "no current recipe"
		}
		if n, ok := snap.Stale[step]; ok {
			row.Stale = &n
			if n > 0 {
				row.ConvergeNote = convergeNotes[step]
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// ─── Judge output ─────────────────────────────────────────────────────────────

// findingsModelRow is one answering model's judge findings by outcome.
type findingsModelRow struct {
	Model                                  string
	Proposed, Unanchorable, Decided, Other int
	Total                                  int
}

// buildFindingsRows pivots (model, patch_state) counts into one row per model,
// sorted by total descending, plus the totals row. "Other" is superseded plus
// the patch state 'stale' (a quarantined replay, not stale_work).
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
		case isDecidedState(f.PatchState):
			r.Decided += f.Count
			total.Decided += f.Count
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
