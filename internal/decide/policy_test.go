package decide

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// policyBase is a deployment with Jev behind the TypeSafe pass-through and a
// LiteLLM-root systemone endpoint serving Clef; decide is bound to Jev. envPin
// is the MODELS_FILE steps.decide pin.
func policyBase(envPin config.ModelPin) *config.Config {
	return &config.Config{
		AIEndpoints: []config.AIEndpoint{
			{ID: "litellm-jev", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000/typesafe", Model: "jev-1.13.0"},
			{ID: "litellm-s1", Type: config.AIEndpointTypeSystemOne, BaseURL: "http://litellm:4000", Model: "clef:27b"},
		},
		AIRoles: &config.AIRoles{Decide: "litellm-jev"},
		Models:  &config.ModelRegistry{Steps: map[string]config.ModelPin{recipe.StepDecide: envPin}},
	}
}

func clefRow(params map[string]any) config.StepSetting {
	return config.StepSetting{Step: recipe.StepDecide, EndpointID: "litellm-s1", Model: "clef:27b", Params: params}
}

func TestResolvePolicyPrecedence(t *testing.T) {
	_, clef, _ := PresetFor("clef:27b")
	with := func(p ShouldApplyParams, mut func(*ShouldApplyParams)) ShouldApplyParams {
		p.ApplyEvidence = append([]string(nil), p.ApplyEvidence...)
		mut(&p)
		return p
	}
	tests := []struct {
		name       string
		envPin     config.ModelPin
		settings   []config.StepSetting
		override   map[string]any
		want       ShouldApplyParams
		wantSource string
		wantPreset string
		wantLayers []PolicyLayer
		wantLabel  string
	}{
		{
			name: "env jev: preset", want: DefaultShouldApplyParams(),
			wantSource: PolicySourcePreset, wantPreset: PresetJev, wantLabel: "preset jev",
		},
		{
			name:       "env MODELS_FILE params over the jev preset",
			envPin:     config.ModelPin{Params: map[string]any{config.ParamUSDPerMTokIn: 0.042, config.ParamApplyP: 0.9}},
			want:       with(DefaultShouldApplyParams(), func(p *ShouldApplyParams) { p.ApplyP = 0.9 }),
			wantSource: PolicySourceEnv, wantPreset: PresetJev, wantLayers: []PolicyLayer{{PolicySourceEnv, []string{config.ParamApplyP}}},
			wantLabel: "env (apply_p) over preset jev",
		},
		{
			name:     "db row on clef: the clef preset",
			settings: []config.StepSetting{clefRow(nil)},
			want:     clef, wantSource: PolicySourcePreset, wantPreset: PresetClef, wantLabel: "preset clef",
		},
		{
			name:     "db row on clef replaces the env pin: its params do not leak",
			envPin:   config.ModelPin{Params: map[string]any{config.ParamApplyP: 0.9}},
			settings: []config.StepSetting{clefRow(nil)},
			want:     clef, wantSource: PolicySourcePreset, wantPreset: PresetClef, wantLabel: "preset clef",
		},
		{
			name:       "db row params over the clef preset, per key",
			settings:   []config.StepSetting{clefRow(map[string]any{config.ParamRejectFloor: 0.05, config.ParamApplyP: 0.9})},
			want:       with(clef, func(p *ShouldApplyParams) { p.RejectFloor, p.ApplyP = 0.05, 0.9 }),
			wantSource: PolicySourceDB, wantPreset: PresetClef,
			wantLayers: []PolicyLayer{{PolicySourceDB, []string{config.ParamApplyP, config.ParamRejectFloor}}},
			wantLabel:  "db (apply_p, reject_floor) over preset clef",
		},
		{
			name:     "per-run override over the db row",
			settings: []config.StepSetting{clefRow(map[string]any{config.ParamApplyP: 0.9})},
			override: map[string]any{config.ParamApplyP: 0.95, config.ParamApplyEvidence: "exact_repeat, asin_verbatim"},
			want: with(clef, func(p *ShouldApplyParams) {
				p.ApplyP, p.ApplyEvidence = 0.95, []string{EvidenceExactRepeat, EvidenceASINVerbatim}
			}),
			wantSource: PolicySourceOverride, wantPreset: PresetClef,
			wantLayers: []PolicyLayer{
				{PolicySourceOverride, []string{config.ParamApplyEvidence, config.ParamApplyP}},
				{PolicySourceDB, []string{config.ParamApplyP}},
			},
			wantLabel: "override (apply_evidence, apply_p) + db (apply_p) over preset clef",
		},
		{
			name:   "per-run --model clef: the clef preset, not the env jev params",
			envPin: config.ModelPin{Params: map[string]any{config.ParamApplyP: 0.9}},
			settings: []config.StepSetting{{Step: recipe.StepDecide, EndpointID: "litellm-s1", Model: "clef:27b",
				Source: config.SourceOverride}},
			want: clef, wantSource: PolicySourcePreset, wantPreset: PresetClef, wantLabel: "preset clef",
		},
		{
			name:     "unknown model: the default",
			settings: []config.StepSetting{{Step: recipe.StepDecide, EndpointID: "litellm-s1", Model: "other-2.0.0"}},
			want:     DefaultShouldApplyParams(), wantSource: PolicySourceDefault, wantLabel: "default",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := policyBase(tc.envPin).WithStepSettings(tc.settings...)
			if err != nil {
				t.Fatal(err)
			}
			pol, err := ResolvePolicy(cfg, tc.override)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(pol.Params, tc.want) {
				t.Errorf("params %+v, want %+v", pol.Params, tc.want)
			}
			if pol.Source != tc.wantSource || pol.Preset != tc.wantPreset || !reflect.DeepEqual(pol.Layers, tc.wantLayers) {
				t.Errorf("source %q preset %q layers %v, want %q %q %v", pol.Source, pol.Preset, pol.Layers,
					tc.wantSource, tc.wantPreset, tc.wantLayers)
			}
			if got := pol.SourceLabel(); got != tc.wantLabel {
				t.Errorf("label %q, want %q", got, tc.wantLabel)
			}
		})
	}
}

// A policy that does not validate once merged fails closed, naming where it
// came from.
func TestResolvePolicyFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		settings []config.StepSetting
		override map[string]any
		want     string
	}{
		{"row apply_p under the preset reject_p", []config.StepSetting{clefRow(map[string]any{config.ParamApplyP: 0.1})}, nil, "reject_p"},
		{"row floor over reject_p", []config.StepSetting{clefRow(map[string]any{config.ParamRejectFloor: 0.5})}, nil, "reject_floor"},
		{"row unknown evidence kind", []config.StepSetting{clefRow(map[string]any{config.ParamApplyEvidence: "asin"})}, nil, "not an evidence kind"},
		{"override floor without the lowercase rule", nil, map[string]any{config.ParamRejectFloor: 0.1}, "also set reject_floor=0"},
		{"clef: turning the lowercase rule off names the floor", []config.StepSetting{clefRow(map[string]any{
			config.ParamRejectRequiresLowercase: false})}, nil, "also set reject_floor=0"},
		{"override wrong type", nil, map[string]any{config.ParamRejectRequiresLowercase: "yes"}, "true or false"},
		{"override apply_p as text", nil, map[string]any{config.ParamApplyP: "high"}, "must be a number"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := policyBase(config.ModelPin{}).WithStepSettings(tc.settings...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ResolvePolicy(cfg, tc.override); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// Turning Clef's lowercase rule off works once the floor is cleared too.
func TestResolvePolicyClefRuleOff(t *testing.T) {
	cfg, err := policyBase(config.ModelPin{}).WithStepSettings(clefRow(map[string]any{
		config.ParamRejectRequiresLowercase: false, config.ParamRejectFloor: 0}))
	if err != nil {
		t.Fatal(err)
	}
	pol, err := ResolvePolicy(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pol.Params.RejectRequiresLowercase || pol.Params.RejectFloor != 0 {
		t.Errorf("policy %+v", pol.Params)
	}
	if d, _ := pol.Params.Decide(0.15, EvidenceNone, IssueMisheardProperNoun, "Arecibo"); d != DecisionReject {
		t.Errorf("a capitalised low-p replacement %s with the rule off", d)
	}
}

// The recipe current_recipes names follows the policy: a policy change on the
// same model is a new current recipe, and the clef preset is not Jev's.
func TestCurrentRecipeFollowsPolicy(t *testing.T) {
	id := func(settings ...config.StepSetting) string {
		t.Helper()
		cfg, err := policyBase(config.ModelPin{}).WithStepSettings(settings...)
		if err != nil {
			t.Fatal(err)
		}
		r, ok, err := CurrentRecipe(cfg)
		if !ok || err != nil {
			t.Fatalf("CurrentRecipe: ok=%v err=%v", ok, err)
		}
		return recipeID(t, r)
	}
	jev := id()
	clef := id(clefRow(nil))
	clefTuned := id(clefRow(map[string]any{config.ParamRejectFloor: 0.05}))
	if jev == clef || clef == clefTuned {
		t.Errorf("recipes jev %s clef %s clef tuned %s: a policy change must be a new recipe", jev, clef, clefTuned)
	}
	if again := id(clefRow(map[string]any{config.ParamRejectFloor: 0.10})); again != clef {
		t.Errorf("setting the preset's own value changed the recipe: %s != %s", again, clef)
	}
	cfg, _ := policyBase(config.ModelPin{}).WithStepSettings(clefRow(map[string]any{config.ParamApplyP: 0.1}))
	if _, _, err := CurrentRecipe(cfg); err == nil {
		t.Error("an invalid policy produced a current recipe")
	}
}

func TestParsePolicyFlags(t *testing.T) {
	got, err := ParsePolicyFlags([]string{"apply_p=0.9", "reject_requires_lowercase=true", "apply_evidence=asin_verbatim"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{config.ParamApplyP: 0.9, config.ParamRejectRequiresLowercase: true, config.ParamApplyEvidence: "asin_verbatim"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
	for _, bad := range []string{"apply_p", "=0.9", "usd_per_mtok_in=1", "applyp=0.9"} {
		if _, err := ParsePolicyFlags([]string{bad}); err == nil {
			t.Errorf("--policy %q accepted", bad)
		}
	}
	if got, err := ParsePolicyFlags(nil); got != nil || err != nil {
		t.Errorf("no flags: %v %v", got, err)
	}
}

func TestPolicyDescribe(t *testing.T) {
	_, clef, _ := PresetFor("clef:27b")
	for p, want := range map[*ShouldApplyParams]string{
		ptr(DefaultShouldApplyParams()): "apply p ≥ 0.85 with asin_verbatim or exact_repeat · reject p ≤ 0.20",
		&clef:                           "apply p ≥ 0.85 with asin_verbatim · reject p ≤ 0.20 if the replacement is lowercase or p < 0.10",
		{ApplyP: 0.855, RejectP: 0.2, ApplyEvidence: []string{EvidenceExactRepeat}, RejectRequiresLowercase: true, RejectFloor: 0.005}: "apply p ≥ 0.855 with exact_repeat · reject p ≤ 0.20 if the replacement is lowercase or p < 0.005",
		{ApplyP: 1, RejectP: 0, ApplyEvidence: []string{EvidenceASINVerbatim}}:                                                         "apply p ≥ 1.00 with asin_verbatim · reject p ≤ 0.00",
	} {
		if got := p.Describe(); got != want {
			t.Errorf("Describe() = %q, want %q", got, want)
		}
	}
}

func ptr[T any](v T) *T { return &v }
