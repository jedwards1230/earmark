// Package models implements `earmark models`: show, set and unset the runtime
// per-step model settings (CONTRACT §2.20), and list their history.
//
// A setting picks, for one AI step, which AI_ENDPOINTS entry to call and
// which model to request from it — in the database, so changing the default
// model needs no Helm/Ansible change. Precedence: a per-run
// --endpoint/--model flag on eval/decide/scan > the model_settings row > the
// env default (AI_ENDPOINTS + AI_ROLES + MODELS_FILE).
package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/openai"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// Store is the slice of the DB the command needs (*db.DB implements it).
type Store interface {
	ListModelSettings(ctx context.Context) ([]db.ModelSetting, error)
	SetModelSetting(ctx context.Context, s db.ModelSetting, actor, note string) (*db.ModelSetting, db.ModelSetting, error)
	UnsetModelSetting(ctx context.Context, step, actor, note string) (*db.ModelSetting, error)
	ModelSettingHistory(ctx context.Context, step string, limit int) ([]db.ModelSettingEvent, error)
}

type setOptions struct {
	endpoint, model         string
	expectedModel, revision string
	params                  []string
	note                    string
	noProbe                 bool
}

// probeEmbed checks an embed setting's vector dimension against
// transcript_chunks.embedding (vector(768)) before it is stored; a test
// replaces it.
var probeEmbed = func(ctx context.Context, cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return openai.NewEmbeddings(cfg).CheckDimension(ctx)
}

var (
	jsonOut      bool
	setOpts      setOptions
	unsetNote    string
	historyLimit int
)

const stepsHelp = "propose (the eval judge, AI_ROLES.eval; alias eval, judge), decide, scan, embed (AI_ROLES.embeddings; alias embeddings)"

// ModelsCmd is `earmark models`.
var ModelsCmd = &cobra.Command{
	Use:   "models",
	Short: "Show or change the model each AI step runs on (runtime settings, no deploy)",
	Long: `models shows, per AI step, the effective endpoint and model, its pins, and
where it comes from (CONTRACT §2.20):

  override  a per-run --endpoint/--model flag (eval, decide, scan only)
  db        a model_settings row ('earmark models set')
  env       AI_ENDPOINTS + AI_ROLES (+ MODELS_FILE pins)
  unset     the step has no endpoint: it is off

A model_settings row names an AI_ENDPOINTS id and a model id — never a URL or
a key — and replaces the env default for its step in every earmark process:
ad-hoc eval/decide/scan read it at start; the ingest worker (in-pipeline judge
and embedder) re-reads it every cycle, the search embedder every 30 s, and the
dashboard on every render. A row that no longer fits AI_ENDPOINTS (its
endpoint was removed) is an ERROR, never a silent fallback: the step's runs
fail until the row is unset or set again.

A different model is a different recipe (CONTRACT §1.9): findings, decisions,
scans and chunks made under it are told apart from the env model's, and
unsetting returns the step to the env recipe.

Steps: ` + stepsHelp + `.

Examples:
  earmark models                                     # effective model per step
  earmark models --json
  earmark models set propose --model ollama/qwen3.8:27b-q8_0 --note "bake-off arm B"
  earmark models set decide --endpoint litellm-s1 --model clef:27b
  earmark models set decide --endpoint litellm-jev --model jev-1.13.0 --param usd_per_mtok_in=0.042
  earmark models set propose --model openrouter/openai/gpt-6-luna --param reasoning_effort=low --param chat_template_kwargs=omit --param temperature=0
  earmark models unset decide --note "back to Jev"
  earmark models history decide --limit 20`,
	Args: cobra.NoArgs,
	Run:  func(cmd *cobra.Command, _ []string) { runShow(cmd) },
}

var showCmd = &cobra.Command{
	Use:     "show",
	Aliases: []string{"list"},
	Short:   "Show the effective model per step (the default action)",
	Args:    cobra.NoArgs,
	Run:     func(cmd *cobra.Command, _ []string) { runShow(cmd) },
}

var setCmd = &cobra.Command{
	Use:   "set <step> [--endpoint <id>] [--model <id>]",
	Short: "Set a step's model in model_settings (replaces the env default until unset)",
	Long: `set writes the step's model_settings row and appends a history event, in one
transaction. --endpoint is an AI_ENDPOINTS id of the type the step needs
(propose: chat, decide/scan: systemone, embed: embeddings); it defaults to the
step's env endpoint. --model defaults to the endpoint's own model. A
systemone model must name one build: a dotted version or an explicit Ollama
tag (jev-1.13.0, clef:27b; anything with "latest"/"preview" is refused). A
non-jev model needs a systemone endpoint whose baseURL is the LiteLLM root
(LiteLLM's unified /v1/systemone): the …/typesafe pass-through always calls
TypeSafe, so such a model is refused there.

Pins: --expected-model (the model that should ANSWER — the recipe's
model_resolved), --revision and --param key=value (known: usd_per_mtok_in;
propose also reasoning_effort, chat_template_kwargs and temperature — see below)
replace the step's MODELS_FILE pin wholesale whenever the row changes the
endpoint or model, or sets any pin; unset pin fields are then unpinned
(answers are expected from the requested model). A row equal to the env
endpoint and model with no pins keeps the env pin.

propose: --param reasoning_effort=<auto|omit|none|low|medium|high|…> and
--param chat_template_kwargs=<auto|omit|JSON object> set the judge's thinking
knobs for this model, and --param temperature=<n> its sampling temperature
(hosted routes otherwise send none). They outrank EVAL_REASONING_EFFORT /
EVAL_CHAT_TEMPLATE_KWARGS and the endpoint's temperature option, and change
the propose recipe; switching the model again drops them with the rest of
the pin.

embed: the model must produce 768-dimension vectors (transcript_chunks is
vector(768)). set embeds one probe string through the endpoint and refuses
any other size (or an unreachable endpoint; --no-probe skips this when the
endpoint is down on purpose). Every running process probes again before it
switches, and keeps its current embedder if the probe fails. Chunks already embedded are
NOT re-embedded: new chunks use the new model, so the index mixes two models
until 'earmark requeue --reembed "" --yes'.

Steps: ` + stepsHelp + `.`,
	Args: cobra.ExactArgs(1),
	Run:  runSet,
}

var unsetCmd = &cobra.Command{
	Use:   "unset <step>",
	Short: "Delete a step's model_settings row (back to the env default)",
	Args:  cobra.ExactArgs(1),
	Run:   runUnset,
}

var historyCmd = &cobra.Command{
	Use:   "history [step]",
	Short: "List model setting changes, newest first",
	Args:  cobra.MaximumNArgs(1),
	Run:   runHistory,
}

func init() {
	ModelsCmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "print JSON")
	f := setCmd.Flags()
	f.StringVar(&setOpts.endpoint, "endpoint", "", "AI_ENDPOINTS id (default: the step's env endpoint)")
	f.StringVar(&setOpts.model, "model", "", "model id to request (default: the endpoint's model)")
	f.StringVar(&setOpts.expectedModel, "expected-model", "", "pin: the model that should answer (default: the requested model)")
	f.StringVar(&setOpts.revision, "revision", "", "pin: weights revision (HF commit, Ollama digest, provider snapshot)")
	f.StringArrayVar(&setOpts.params, "param", nil, "pin param key=value (known: usd_per_mtok_in; propose: reasoning_effort, chat_template_kwargs, temperature); repeatable")
	f.StringVar(&setOpts.note, "note", "", "why, recorded in the history")
	f.BoolVar(&setOpts.noProbe, "no-probe", false, "embed only: store without embedding a probe string to check the vector dimension (running processes still probe before switching)")
	unsetCmd.Flags().StringVar(&unsetNote, "note", "", "why, recorded in the history")
	historyCmd.Flags().IntVar(&historyLimit, "limit", 50, fmt.Sprintf("events to list (1..%d)", db.MaxModelSettingHistory))
	ModelsCmd.AddCommand(showCmd, setCmd, unsetCmd, historyCmd)
}

// stepAliases maps the role names people also use to the recipe step.
var stepAliases = map[string]string{
	"eval": recipe.StepPropose, "judge": recipe.StepPropose, "embeddings": recipe.StepEmbed,
}

// normalizeStep resolves a step argument (a recipe step or a role alias).
func normalizeStep(arg string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(arg))
	if a, ok := stepAliases[s]; ok {
		s = a
	}
	if _, ok := config.StepEndpointType(s); !ok {
		return "", fmt.Errorf("unknown step %q; steps: %s", arg, stepsHelp)
	}
	return s, nil
}

// Actor is who a CLI change is attributed to: cli:<login>, the convention of
// every other CLI-attributed write (decided_by "cli:<name>").
func Actor() string {
	name := strings.TrimSpace(os.Getenv("USER"))
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	if name == "" {
		name = "unknown"
	}
	return "cli:" + name
}

// open loads the env config and the database.
func open() (*config.Config, *db.DB) {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	database, err := db.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to database: %v\n", err)
		os.Exit(1)
	}
	return cfg, database
}

func exitOn(err error, code int) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(code)
	}
}

func runShow(cmd *cobra.Command) {
	cfg, database := open()
	defer database.Close()
	rows, err := database.ListModelSettings(cmd.Context())
	if err == nil {
		err = Show(os.Stdout, cfg, rows, jsonOut)
	}
	exitOn(err, 1)
}

func runSet(cmd *cobra.Command, args []string) {
	step, err := normalizeStep(args[0])
	exitOn(err, 2)
	cfg, database := open()
	defer database.Close()
	exitOn(Set(cmd.Context(), os.Stdout, database, cfg, step, setOpts, Actor()), 1)
}

func runUnset(cmd *cobra.Command, args []string) {
	step, err := normalizeStep(args[0])
	exitOn(err, 2)
	cfg, database := open()
	defer database.Close()
	exitOn(Unset(cmd.Context(), os.Stdout, database, cfg, step, unsetNote, Actor()), 1)
}

func runHistory(cmd *cobra.Command, args []string) {
	step := ""
	if len(args) == 1 {
		s, err := normalizeStep(args[0])
		exitOn(err, 2)
		step = s
	}
	if historyLimit < 1 || historyLimit > db.MaxModelSettingHistory {
		exitOn(fmt.Errorf("--limit must be 1..%d", db.MaxModelSettingHistory), 2)
	}
	_, database := open()
	defer database.Close()
	evs, err := database.ModelSettingHistory(cmd.Context(), step, historyLimit)
	if err == nil {
		err = History(os.Stdout, evs, jsonOut)
	}
	exitOn(err, 1)
}

// StepView is one step's effective model, as `earmark models` prints it.
type StepView struct {
	Step          string         `json:"step"`
	Source        string         `json:"source"` // db | env | unset
	Endpoint      string         `json:"endpoint,omitempty"`
	Model         string         `json:"model,omitempty"`
	ExpectedModel string         `json:"expected_model,omitempty"`
	Revision      string         `json:"revision,omitempty"`
	Params        map[string]any `json:"params,omitempty"`
	EnvEndpoint   string         `json:"env_endpoint,omitempty"`
	EnvModel      string         `json:"env_model,omitempty"`
	UpdatedBy     string         `json:"updated_by,omitempty"`
	UpdatedAt     *time.Time     `json:"updated_at,omitempty"`
	// Error is set when the step's model_settings row is invalid against
	// this deployment's AI_ENDPOINTS; the fields above are then the row's.
	Error string `json:"error,omitempty"`
	// Thinking (propose only, when the step has a model) is the judge's
	// effective thinking knobs and where each came from (CONTRACT §2.15).
	Thinking *ThinkingView `json:"thinking,omitempty"`
}

// ThinkingView is the judge's resolved reasoning_effort / chat_template_kwargs
// for `earmark models`: the propose model setting's params > the EVAL_* env
// vars > the route default.
type ThinkingView struct {
	// ReasoningEffort is the value sent, "omit" when the field is not sent.
	ReasoningEffort string `json:"reasoning_effort"`
	// ChatTemplateKwargs is the object sent, or "omit".
	ChatTemplateKwargs any `json:"chat_template_kwargs"`
	// The sources are "setting" (the model setting's params), "env" or "auto".
	ReasoningEffortSource    string `json:"reasoning_effort_source"`
	ChatTemplateKwargsSource string `json:"chat_template_kwargs_source"`
	// Summary is the one-line form the table prints.
	Summary string `json:"summary"`
	// Error is set when a layer holds an invalid value: the judge refuses to
	// start until it is fixed.
	Error string `json:"error,omitempty"`
}

// thinkingView resolves the judge's thinking knobs on the effective config.
func thinkingView(eff *config.Config, model string) *ThinkingView {
	t, err := eval.JudgeThinkingFor(eval.ConfigSource(eff), model)
	if err != nil {
		return &ThinkingView{Error: err.Error()}
	}
	v := &ThinkingView{
		ReasoningEffort: t.ReasoningEffort, ChatTemplateKwargs: t.ChatTemplateKwargs,
		ReasoningEffortSource: t.EffortSource, ChatTemplateKwargsSource: t.KwargsSource,
		Summary: t.Describe(),
	}
	if v.ReasoningEffort == "" {
		v.ReasoningEffort = "omit"
	}
	if t.ChatTemplateKwargs == nil {
		v.ChatTemplateKwargs = "omit"
	}
	return v
}

// View computes every settable step's effective model from the env config
// and the model_settings rows.
func View(base *config.Config, rows []db.ModelSetting) []StepView {
	eff, invalid := base.WithValidStepSettings(db.StepSettings(rows)...)
	byStep := map[string]db.ModelSetting{}
	for _, r := range rows {
		byStep[r.Step] = r
	}
	out := make([]StepView, 0, len(config.SettingSteps))
	for _, step := range config.SettingSteps {
		v := StepView{Step: step}
		v.EnvEndpoint, v.EnvModel = endpointAndModel(base, step)
		row, hasRow := byStep[step]
		if hasRow {
			at := row.UpdatedAt
			v.UpdatedBy, v.UpdatedAt = row.UpdatedBy, &at
		}
		if err := invalid[step]; err != nil {
			v.Source, v.Error = config.SourceDB, err.Error()
			v.Endpoint, v.Model = row.EndpointID, row.Model
			v.ExpectedModel, v.Revision, v.Params = row.ExpectedModel, row.Revision, row.Params
			out = append(out, v)
			continue
		}
		v.Source = eff.StepSource(step)
		v.Endpoint, v.Model = endpointAndModel(eff, step)
		pin := eff.ModelPin(step)
		v.ExpectedModel, v.Revision, v.Params = pin.ExpectedModel, pin.Revision, pin.Params
		if step == recipe.StepPropose && v.Model != "" {
			v.Thinking = thinkingView(eff, v.Model)
		}
		out = append(out, v)
	}
	return out
}

// endpointAndModel names a step's endpoint (the registered id, not a
// synthesized one) and requested model.
func endpointAndModel(cfg *config.Config, step string) (endpoint, model string) {
	model, _ = cfg.StepModel(step)
	if ep, ok := cfg.StepEndpoint(step); ok {
		endpoint = ep.ID
		if ep.BaseID != "" {
			endpoint = ep.BaseID
		}
	} else if model != "" {
		endpoint = "EVAL_CHAT_*"
	}
	return endpoint, model
}

// Show prints the effective model per step.
func Show(out io.Writer, base *config.Config, rows []db.ModelSetting, asJSON bool) error {
	views := View(base, rows)
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(views)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "STEP\tSOURCE\tENDPOINT\tMODEL\tPIN\tENV DEFAULT\tSET BY")
	var errs []string
	for _, v := range views {
		src := v.Source
		if v.Error != "" {
			src = "db INVALID"
			errs = append(errs, fmt.Sprintf("  %s: %s", v.Step, v.Error))
		}
		env := "—"
		if v.EnvModel != "" {
			env = v.EnvModel + " @ " + v.EnvEndpoint
		}
		setBy := "—"
		if v.UpdatedAt != nil {
			setBy = v.UpdatedBy + " " + v.UpdatedAt.Local().Format("2006-01-02 15:04")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Step, src, dash(v.Endpoint), dash(v.Model),
			pinLine(v.ExpectedModel, v.Revision, v.Params), env, setBy)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, v := range views {
		switch {
		case v.Thinking == nil:
		case v.Thinking.Error != "":
			_, _ = fmt.Fprintf(out, "\n%s thinking: INVALID — %s\n", v.Step, v.Thinking.Error)
		default:
			_, _ = fmt.Fprintf(out, "\n%s thinking: %s\n", v.Step, v.Thinking.Summary)
		}
	}
	if len(errs) > 0 {
		_, _ = fmt.Fprintf(out, "\nINVALID model_settings rows — these steps fail until fixed:\n%s\n", strings.Join(errs, "\n"))
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// pinLine renders a pin compactly ("—" when unpinned).
func pinLine(expected, revision string, params map[string]any) string {
	var parts []string
	if expected != "" {
		parts = append(parts, "expects "+expected)
	}
	if revision != "" {
		parts = append(parts, "rev "+revision)
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, params[k]))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}

// parseParams parses --param key=value pairs; a numeric value is a number.
func parseParams(kvs []string) (map[string]any, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return nil, fmt.Errorf("--param %q must be key=value", kv)
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			out[k] = f
		} else {
			out[k] = v
		}
	}
	return out, nil
}

// BuildSetting turns `models set` flags into a validated setting against the
// env registry: --endpoint defaults to the step's env endpoint, --model to
// the endpoint's own model.
func BuildSetting(base *config.Config, step string, o setOptions) (config.StepSetting, error) {
	params, err := parseParams(o.params)
	if err != nil {
		return config.StepSetting{}, err
	}
	s := config.StepSetting{
		Step: step, EndpointID: strings.TrimSpace(o.endpoint), Model: strings.TrimSpace(o.model),
		ExpectedModel: strings.TrimSpace(o.expectedModel), Revision: strings.TrimSpace(o.revision),
		Params: params, Source: config.SourceDB,
	}
	if s.EndpointID == "" {
		ep, ok := base.StepEndpoint(step)
		if !ok {
			return config.StepSetting{}, fmt.Errorf("the %s step has no env endpoint (AI_ROLES); pass --endpoint <AI_ENDPOINTS id>", step)
		}
		s.EndpointID = ep.ID
	}
	if s.Model == "" {
		for _, ep := range base.AIEndpoints {
			if ep.ID == s.EndpointID && ep.BaseID == "" {
				s.Model = ep.Model
			}
		}
	}
	if err := base.ValidateStepSetting(s); err != nil {
		return config.StepSetting{}, fmt.Errorf("models set %s: %w", step, err)
	}
	return s, nil
}

// Set validates the flags against the env registry, writes the row (with its
// history event) and prints the change.
func Set(ctx context.Context, out io.Writer, store Store, base *config.Config, step string, o setOptions, actor string) error {
	s, err := BuildSetting(base, step, o)
	if err != nil {
		return err
	}
	if step == recipe.StepEmbed && !o.noProbe {
		eff, err := base.WithStepSettings(s)
		if err != nil {
			return err
		}
		if err := probeEmbed(ctx, eff); err != nil {
			return fmt.Errorf("models set embed refused (nothing stored): %w", err)
		}
	}
	prev, stored, err := store.SetModelSetting(ctx, db.ModelSetting{
		Step: s.Step, EndpointID: s.EndpointID, Model: s.Model,
		ExpectedModel: s.ExpectedModel, Revision: s.Revision, Params: s.Params,
	}, actor, o.note)
	if errors.Is(err, db.ErrModelSettingUnchanged) {
		_, _ = fmt.Fprintf(out, "%s: unchanged (already %s @ %s, set by %s); nothing written\n",
			step, stored.Model, stored.EndpointID, stored.UpdatedBy)
		return nil
	}
	if err != nil {
		return err
	}
	before := "env default"
	if prev != nil {
		before = prev.Model + " @ " + prev.EndpointID
	}
	_, _ = fmt.Fprintf(out, "%s: %s → %s @ %s (by %s)\n", step, before, stored.Model, stored.EndpointID, actor)
	if step == recipe.StepEmbed {
		_, _ = fmt.Fprintln(out, "note: Existing chunks are not re-embedded;\n"+
			"      new chunks use this model (a new embed recipe) and search follows within 30 s.\n"+
			"      Re-embed the library with: earmark requeue --reembed \"\" --yes")
	}
	return showAfter(ctx, out, store, base)
}

// Unset deletes the step's row (with its history event) and prints the
// change. Unsetting a step that has no row is a no-op, not an error.
func Unset(ctx context.Context, out io.Writer, store Store, base *config.Config, step, note, actor string) error {
	prev, err := store.UnsetModelSetting(ctx, step, actor, note)
	if err != nil {
		return err
	}
	if prev == nil {
		_, _ = fmt.Fprintf(out, "%s: no model_settings row; already on the env default\n", step)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s: %s @ %s → env default (by %s)\n", step, prev.Model, prev.EndpointID, actor)
	return showAfter(ctx, out, store, base)
}

func showAfter(ctx context.Context, out io.Writer, store Store, base *config.Config) error {
	rows, err := store.ListModelSettings(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out)
	return Show(out, base, rows, false)
}

// History prints model setting events, newest first.
func History(out io.Writer, evs []db.ModelSettingEvent, asJSON bool) error {
	if asJSON {
		if evs == nil {
			evs = []db.ModelSettingEvent{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(evs)
	}
	if len(evs) == 0 {
		_, _ = fmt.Fprintln(out, "no model setting changes recorded")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tAT\tSTEP\tACTION\tFROM\tTO\tACTOR\tNOTE")
	for _, e := range evs {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.At.Local().Format("2006-01-02 15:04:05"),
			e.Step, e.Action, valueLine(e.Old), valueLine(e.New), e.Actor, dash(e.Note))
	}
	return tw.Flush()
}

func valueLine(v *db.ModelSettingValue) string {
	if v == nil {
		return "env default"
	}
	s := v.Model + " @ " + v.EndpointID
	if p := pinLine(v.ExpectedModel, v.Revision, v.Params); p != "—" {
		s += " (" + p + ")"
	}
	return s
}
