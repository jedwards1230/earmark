package models

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

type fakeScorecards struct {
	props []db.ProposeScore
	decs  []db.DecideScore
	since []time.Time
}

func (f *fakeScorecards) RecipeScorecard(_ context.Context, since time.Time) ([]db.ProposeScore, error) {
	f.since = append(f.since, since)
	return f.props, nil
}

func (f *fakeScorecards) DecideScorecard(_ context.Context, since time.Time) ([]db.DecideScore, error) {
	f.since = append(f.since, since)
	return f.decs, nil
}

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

func tp(s string) *time.Time {
	t, _ := time.Parse(time.DateOnly, s)
	return &t
}

func withPrecision(l db.LabelTally) db.LabelTally {
	if n := l.Good + l.Junk + l.Partial; n > 0 {
		l.Precision = fp(float64(l.Good) / float64(n))
	}
	return l
}

// scoreFixture is a legacy gemma recipe, a recorded luna recipe and two
// decide recipes, shaped like the seeded integration fixture.
func scoreFixture() *fakeScorecards {
	step1 := 1
	return &fakeScorecards{
		props: []db.ProposeScore{
			{RecipeID: strings.Repeat("a", 63) + "1", ModelAlias: "gemma3:12b", PromptVersion: "judge@v1", StepVersion: &step1,
				Legacy: true, Passes: 1, PassesLegacy: 1, TranscriptsJudged: 1, FindingsTotal: 3, FindingsLive: 3,
				IssueMix: map[string]int64{"misheard_proper_noun": 1, "misheard_word": 1, "repeated_text": 1},
				StateMix: map[string]int64{"applied": 1, "rejected": 1, "reverted": 1},
				Decisions: []db.DecisionMix{{DecideRecipeID: strings.Repeat("d", 63) + "1", DecideModel: "jev-1.13.0",
					Decided: 2, Apply: 1, Reject: 1, Reasons: map[string]int64{"apply:confident_with_evidence": 1, "reject:jev_reject": 1}}},
				AppliedEver: 2, Reverted: 1, RevertRate: fp(0.5), CorrectionKeys: 3, AgreedKeys: 1, AgreementRate: fp(1.0 / 3),
				Labels:           withPrecision(db.LabelTally{Good: 1, Junk: 1, Partial: 1}),
				ProperNounLabels: withPrecision(db.LabelTally{Good: 1}),
				FirstActivityAt:  tp("2026-06-02"), LastActivityAt: tp("2026-06-03")},
			{RecipeID: strings.Repeat("a", 63) + "2", ModelAlias: "openrouter/openai/gpt-6-luna", PromptVersion: "judge@v2",
				Passes: 1, PassesComplete: 1, TranscriptsJudged: 1, ChunksJudged: ip(2), ChunksCached: ip(1), ChunksFailed: ip(0),
				CachedShare: fp(0.5), FindingsTotal: 2, FindingsLive: 2, FindingsPer1kChunks: fp(1000),
				IssueMix: map[string]int64{"misheard_proper_noun": 1, "homophone": 1},
				StateMix: map[string]int64{"duplicate": 1, "rejected": 1},
				Drops:    map[string]int64{"cosmetic_only": 3, "over_cap": 1}, ParseFailures: ip(1), Decisions: []db.DecisionMix{},
				CorrectionKeys: 2, AgreedKeys: 1, AgreementRate: fp(0.5),
				Labels:  withPrecision(db.LabelTally{Good: 1, Unsure: 1}),
				CostUSD: fp(0.002), CostPerChunkUSD: fp(0.001), LatencyMsP50: fp(1200), LatencyMsP95: fp(1200), LatencySource: "calls",
				FirstActivityAt: tp("2026-10-03"), LastActivityAt: tp("2026-10-03")},
		},
		decs: []db.DecideScore{
			{RecipeID: strings.Repeat("d", 63) + "1", ModelAlias: "jev-1.13.0", PromptVersion: "should_apply@v2",
				Decisions: 4, Revoked: 1, RevokeRate: fp(0.25), FindingsDecided: 4, Live: 3, Apply: 1, Hold: 1, Reject: 1,
				OutcomeEvidence: map[string]map[string]int64{"apply": {"asin_verbatim": 1}, "hold": {"none": 1}, "reject": {"none": 1}},
				Reasons:         map[string]int64{"hold:uncertain": 1, "reject:jev_reject": 1, "apply:confident_with_evidence": 1},
				LabelOutcomes: map[string]db.LabelTally{"apply": withPrecision(db.LabelTally{Good: 1}),
					"reject": withPrecision(db.LabelTally{Junk: 1})},
				LabelCalibration: []db.PBucket{{Bucket: 1, Lo: 0, Hi: 0.2, Junk: 1, Rate: fp(0)}, {Bucket: 2, Lo: 0.2, Hi: 0.4},
					{Bucket: 3, Lo: 0.4, Hi: 0.6}, {Bucket: 4, Lo: 0.6, Hi: 0.8}, {Bucket: 5, Lo: 0.8, Hi: 1, Good: 1, Rate: fp(1)}},
				HumanCalibration: []db.PBucket{{Bucket: 3, Lo: 0.4, Hi: 0.6, Rejected: 1, Rate: fp(0)}},
				Calls:            2, CostUSD: fp(0.0002), CostPerCallUSD: fp(0.0001), LatencyMsP50: fp(1000), LatencyMsP95: fp(1180),
				FirstDecisionAt: tp("2026-09-01"), LastActivityAt: tp("2026-10-04")},
			{RecipeID: strings.Repeat("d", 63) + "2", ModelAlias: "clef:27b", PromptVersion: "should_apply@v2",
				Decisions: 2, FindingsDecided: 2, Live: 2, Apply: 2, RevokeRate: fp(0),
				LabelOutcomes:    map[string]db.LabelTally{"apply": withPrecision(db.LabelTally{Good: 1, Junk: 1})},
				LabelCalibration: []db.PBucket{}, HumanCalibration: []db.PBucket{},
				PrevRecipeID: strings.Repeat("d", 63) + "1", FlipOverlap: 2, Flips: 1, FlipRate: fp(0.5),
				Calls: 1, CostUSD: fp(0), CostPerCallUSD: fp(0), LatencyMsP50: fp(400), LatencyMsP95: fp(400),
				FirstDecisionAt: tp("2026-10-05"), LastActivityAt: tp("2026-10-05")},
		},
	}
}

func TestParseReportOptions(t *testing.T) {
	o, err := ParseReportOptions("2026-10-01", "judge", true)
	require.NoError(t, err)
	assert.Equal(t, recipe.StepPropose, o.Step)
	assert.True(t, o.JSON)
	assert.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), o.Since)
	o, err = ParseReportOptions("", "decide", false)
	require.NoError(t, err)
	assert.Equal(t, recipe.StepDecide, o.Step)
	assert.True(t, o.Since.IsZero())
	_, err = ParseReportOptions("", "embed", false)
	assert.Error(t, err)
	_, err = ParseReportOptions("yesterday", "", false)
	assert.Error(t, err)
}

func TestReportText(t *testing.T) {
	store := scoreFixture()
	var out bytes.Buffer
	require.NoError(t, Report(context.Background(), &out, store, ReportOptions{}))
	got := out.String()
	for _, want := range []string{
		"PROPOSE RECIPES (2)",
		"gemma3:12b · judge@v1 · step 1 · legacy",
		"openrouter/openai/gpt-6-luna · judge@v2 · recorded",
		// Legacy numbers are marked, never 0.
		"chunks      n/a (legacy)\n",
		"drops       n/a (legacy)",
		"parse fail  n/a (legacy)",
		"cost        n/a (legacy)\n",
		// The recorded recipe has them.
		"judged 2 · cached 1 (50.0%) · failed 0",
		"drops       cosmetic_only 3 · over_cap 1",
		"parse fail  1",
		"$0.002000 total · $0.001000 per chunk · latency p50 1200 ms · p95 1200 ms (calls)",
		"decided     jev-1.13.0 (dddddddddddd): 2 — apply 1 · hold 0 · reject 1",
		"reverts     1 of 2 applied (50.0%)",
		"agreement   1 of 3 fixes also proposed by another recipe (33.3%)",
		"labels      33.3% (n=3) (good 1 · junk 1 · partial 1 · unsure 0) · proper nouns 100.0% (n=1)",
		"DECIDE RECIPES (2)",
		"revoked 1 (25.0%) · live apply 1 · hold 1 · reject 1",
		"evidence    apply: asin_verbatim 1",
		"vs labels   apply 100.0% (n=1)",
		"p→precision [0.0,0.2) 0.0% (n=1) · [0.8,1.0] 100.0% (n=1)",
		"vs humans   agree — · p→accept rate [0.4,0.6) 0.0% (n=1)",
		"flips       — (no earlier decide recipe)",
		"flips       50.0% of 2 findings both decided vs jev-1.13.0 (dddddddddddd)",
		"$0.000200 over 2 calls (0 cached, 0 errors) · $0.000100 per call · latency p50 1000 ms · p95 1180 ms",
	} {
		assert.Contains(t, got, want)
	}
	assert.NotContains(t, got, "recipes active since")
	// The legacy recipe's per-chunk cost never prints as zero.
	gemma := got[strings.Index(got, "● aaaaaaaaaaaa  gemma"):strings.Index(got, "● aaaaaaaaaaaa  openrouter")]
	assert.NotContains(t, gemma, "$0")
}

func TestReportStepAndSince(t *testing.T) {
	store := scoreFixture()
	var out bytes.Buffer
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, Report(context.Background(), &out, store, ReportOptions{Step: recipe.StepDecide, Since: since}))
	assert.Equal(t, []time.Time{since}, store.since, "decide only: one query, with the since bound")
	assert.NotContains(t, out.String(), "PROPOSE RECIPES")
	assert.Contains(t, out.String(), "recipes active since 2026-10-01")

	out.Reset()
	require.NoError(t, Report(context.Background(), &out, &fakeScorecards{}, ReportOptions{}))
	assert.Contains(t, out.String(), "no propose recipe has findings or judge passes")
	assert.Contains(t, out.String(), "no decide recipe has decisions or calls")
}

func TestReportJSON(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, Report(context.Background(), &out, scoreFixture(), ReportOptions{JSON: true, Step: recipe.StepPropose}))
	var got ScorecardReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.Len(t, got.Propose, 2)
	assert.Nil(t, got.Decide)
	assert.True(t, got.Propose[0].Legacy)
	assert.Nil(t, got.Propose[0].CostPerChunkUSD, "legacy cost is null in JSON, not 0")
	assert.Contains(t, out.String(), `"cost_per_chunk_usd": null`)
	assert.Equal(t, int64(3), got.Propose[1].Drops["cosmetic_only"])
}

func TestReportFormatting(t *testing.T) {
	assert.Equal(t, "1,234,567", num(1234567))
	assert.Equal(t, "-1,000", num(-1000))
	assert.Equal(t, "999", num(999))
	assert.Equal(t, "(unattributed)", shortID(""))
	assert.Equal(t, "—", usdOr(nil))
	assert.Equal(t, "$12.50", usdOr(fp(12.5)))
	assert.Equal(t, "a 3 · b 2 · other 2", mix(map[string]int64{"a": 3, "b": 2, "c": 1, "d": 1}, 2))
	assert.Equal(t, "—", mix(nil, 0))
}
