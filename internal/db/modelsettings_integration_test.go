package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// TestIntegrationModelSettings: migration 12 against a real Postgres — set
// and unset write the row and its history event in one transaction, the
// history is append-only and newest-first, and the table refuses rows the
// code would never write. Skipped unless EARMARK_TEST_DATABASE_URL.
func TestIntegrationModelSettings(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()

	rows, err := d.ListModelSettings(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("fresh model_settings = %v, %v; want empty", rows, err)
	}

	// First set: the env default was in effect, so the event has no old value.
	prev, stored, err := d.SetModelSetting(ctx, ModelSetting{Step: "decide", EndpointID: "clef", Model: "clef-1.0.0",
		Params: map[string]any{config.ParamUSDPerMTokIn: 0.01}}, "cli:justin", "arm C")
	if err != nil {
		t.Fatalf("SetModelSetting: %v", err)
	}
	if prev != nil || stored.UpdatedBy != "cli:justin" || stored.UpdatedAt.IsZero() {
		t.Fatalf("first set: prev %+v stored %+v", prev, stored)
	}
	// Second set replaces it and records what it replaced.
	prev, _, err = d.SetModelSetting(ctx, ModelSetting{Step: "decide", EndpointID: "jev", Model: "jev-1.14.0",
		ExpectedModel: "typesafe/jev-1.14.0", Revision: "r9"}, "cli:carol", "")
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || prev.Model != "clef-1.0.0" || prev.Params[config.ParamUSDPerMTokIn] != 0.01 {
		t.Fatalf("second set's prev = %+v", prev)
	}
	if _, _, err := d.SetModelSetting(ctx, ModelSetting{Step: "propose", EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0"}, "cli:justin", ""); err != nil {
		t.Fatal(err)
	}
	// Setting the stored value again writes nothing and records no event.
	if _, stored, err := d.SetModelSetting(ctx, ModelSetting{Step: "propose", EndpointID: "judge", Model: "ollama/qwen3.8:27b-q8_0"}, "cli:other", ""); !errors.Is(err, ErrModelSettingUnchanged) || stored.UpdatedBy != "cli:justin" {
		t.Fatalf("no-op set: err %v stored %+v", err, stored)
	}

	rows, err = d.ListModelSettings(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	var eventAt time.Time
	if err := d.pool.QueryRow(ctx, `SELECT max(created_at) FROM model_setting_events WHERE step = 'decide'`).Scan(&eventAt); err != nil {
		t.Fatal(err)
	}
	if rows[0].UpdatedAt.After(eventAt) {
		t.Errorf("row updated_at %v after its event %v: one clock expected", rows[0].UpdatedAt, eventAt)
	}
	if r := rows[0]; r.Step != "decide" || r.Model != "jev-1.14.0" || r.ExpectedModel != "typesafe/jev-1.14.0" ||
		r.Revision != "r9" || r.Params != nil || r.UpdatedBy != "cli:carol" {
		t.Errorf("decide row = %+v", r)
	}

	// Unset deletes the row and records it; a second unset is a no-op.
	prev, err = d.UnsetModelSetting(ctx, "decide", "cli:justin", "back to env")
	if err != nil || prev == nil || prev.Model != "jev-1.14.0" {
		t.Fatalf("unset: prev %+v err %v", prev, err)
	}
	if prev, err := d.UnsetModelSetting(ctx, "decide", "cli:justin", ""); err != nil || prev != nil {
		t.Fatalf("second unset: prev %+v err %v", prev, err)
	}

	// History: newest first, one event per change, the old/new values intact.
	evs, err := d.ModelSettingHistory(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		got = append(got, e.Step+":"+e.Action+":"+valueModel(e.Old)+">"+valueModel(e.New)+":"+e.Actor)
	}
	want := []string{
		"decide:unset:jev-1.14.0>:cli:justin",
		"propose:set:>ollama/qwen3.8:27b-q8_0:cli:justin",
		"decide:set:clef-1.0.0>jev-1.14.0:cli:carol",
		"decide:set:>clef-1.0.0:cli:justin",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("history =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if evs[0].Note != "back to env" || evs[3].Note != "arm C" || evs[2].New.Revision != "r9" ||
		evs[3].New.Params[config.ParamUSDPerMTokIn] != 0.01 {
		t.Errorf("event details lost: %+v", evs)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].ID >= evs[i-1].ID || evs[i].At.After(evs[i-1].At) {
			t.Errorf("history not newest-first at %d", i)
		}
	}
	if evs, _ := d.ModelSettingHistory(ctx, "propose", 100); len(evs) != 1 {
		t.Errorf("step filter: %d events, want 1", len(evs))
	}
	if evs, _ := d.ModelSettingHistory(ctx, "", 2); len(evs) != 2 || evs[0].Action != "unset" {
		t.Errorf("limit: %+v", evs)
	}
	if _, err := d.ModelSettingHistory(ctx, "", 0); err == nil {
		t.Error("limit 0 accepted")
	}

	// Append-only.
	if _, err := d.pool.Exec(ctx, `UPDATE model_setting_events SET actor = 'x'`); err == nil {
		t.Error("UPDATE on model_setting_events succeeded")
	}
	if _, err := d.pool.Exec(ctx, `DELETE FROM model_setting_events`); err == nil {
		t.Error("DELETE on model_setting_events succeeded")
	}

	// The table refuses rows the code never writes.
	for _, sql := range []string{
		`INSERT INTO model_settings (step, endpoint_id, model, updated_by) VALUES ('asr', 'e', 'm', 'x')`,
		`INSERT INTO model_settings (step, endpoint_id, model, updated_by) VALUES ('scan', '', 'm', 'x')`,
		`INSERT INTO model_settings (step, endpoint_id, model, params, updated_by) VALUES ('scan', 'e', 'm', '[]', 'x')`,
		`INSERT INTO model_setting_events (step, action, new_value, actor) VALUES ('scan', 'unset', '{}', 'x')`,
		`INSERT INTO model_setting_events (step, action, new_value, actor) VALUES ('asr', 'set', '{}', 'x')`,
		`INSERT INTO model_setting_events (step, action, new_value, actor) VALUES ('scan', 'set', '[]', 'x')`,
		`INSERT INTO model_settings (step, endpoint_id, model, expected_model, updated_by) VALUES ('scan', 'e', 'm', '', 'x')`,
		`INSERT INTO model_settings (step, endpoint_id, model, revision, updated_by) VALUES ('scan', 'e', 'm', '', 'x')`,
		`INSERT INTO model_settings (step, endpoint_id, model, updated_by) VALUES ('scan', 'e', repeat('m', 257), 'x')`,
		`INSERT INTO model_settings (step, endpoint_id, model, updated_by) VALUES ('scan', 'e', 'm', '')`,
	} {
		if _, err := d.pool.Exec(ctx, sql); err == nil {
			t.Errorf("accepted: %s", sql)
		}
	}
	if _, _, err := d.SetModelSetting(ctx, ModelSetting{Step: "asr", EndpointID: "e", Model: "m"}, "cli:x", ""); !errors.Is(err, ErrInvalidModelSetting) {
		t.Errorf("unknown step: %v", err)
	}
	if _, _, err := d.SetModelSetting(ctx, ModelSetting{Step: "scan", EndpointID: "e", Model: "m"}, " ", ""); !errors.Is(err, ErrInvalidModelSetting) {
		t.Errorf("no actor: %v", err)
	}
}

// TestIntegrationModelSettingAtomic: the row and its event commit together —
// when the event cannot be written, the row change rolls back.
func TestIntegrationModelSettingAtomic(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()
	if _, _, err := d.SetModelSetting(ctx, ModelSetting{Step: "scan", EndpointID: "jev", Model: "jev-1.13.0"}, "cli:a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `
		CREATE FUNCTION refuse_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'event refused'; END $$;
		CREATE TRIGGER refuse_event BEFORE INSERT ON model_setting_events
		FOR EACH ROW EXECUTE FUNCTION refuse_event();`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.SetModelSetting(ctx, ModelSetting{Step: "scan", EndpointID: "clef", Model: "clef-1.0.0"}, "cli:b", ""); err == nil {
		t.Fatal("set succeeded with its event refused")
	}
	if _, err := d.UnsetModelSetting(ctx, "scan", "cli:b", ""); err == nil {
		t.Fatal("unset succeeded with its event refused")
	}
	rows, err := d.ListModelSettings(ctx)
	if err != nil || len(rows) != 1 || rows[0].Model != "jev-1.13.0" || rows[0].UpdatedBy != "cli:a" {
		t.Fatalf("the row changed without its event: %+v %v", rows, err)
	}
	if evs, _ := d.ModelSettingHistory(ctx, "", 10); len(evs) != 1 {
		t.Errorf("events = %d, want the first set only", len(evs))
	}
}

// TestIntegrationModelSettingRecipes: a db-set embed model registers a recipe
// that is NOT equivalent to the env one (so stale_work and the work selectors
// treat its output as another configuration), the process's embedder and
// chunk stamp follow it, and unsetting gives back the env recipe id.
func TestIntegrationModelSettingRecipes(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()
	base := d.cfg.Clone()
	base.AIEndpoints = append(base.AIEndpoints,
		config.AIEndpoint{ID: "e2", Type: config.AIEndpointTypeEmbeddings, BaseURL: "http://vllm:8000/v1", Model: "mxbai-embed-large"})

	envCfg, err := EffectiveConfig(ctx, d, base)
	if err != nil {
		t.Fatal(err)
	}
	envRecipe := EmbedRecipeFor(envCfg)
	envID := mustID(t, envRecipe)
	if envID != mustID(t, d.EmbedRecipe()) {
		t.Fatal("with no rows the effective embed recipe differs from the process's")
	}

	if _, _, err := d.SetModelSetting(ctx, ModelSetting{Step: recipe.StepEmbed, EndpointID: "e2", Model: "mxbai-embed-large"}, "cli:a", ""); err != nil {
		t.Fatal(err)
	}
	eff, err := EffectiveConfig(ctx, d, base, recipe.StepEmbed)
	if err != nil {
		t.Fatal(err)
	}
	dbRecipe := EmbedRecipeFor(eff)
	dbID := mustID(t, dbRecipe)
	if dbID == envID {
		t.Fatal("the db-set embed model reused the env recipe id")
	}
	for _, r := range []recipe.Recipe{envRecipe, dbRecipe} {
		if _, err := d.RegisterRecipe(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	var eq []string
	rows, err := d.pool.Query(ctx, `WITH `+equivalentRecipesCTE+` SELECT recipe_id FROM eq`, dbID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		eq = append(eq, id)
	}
	rows.Close()
	if len(eq) != 1 || eq[0] != dbID {
		t.Errorf("equivalent recipes of the db-set one = %v, want only itself", eq)
	}

	// The process follows the setting: chunks are stamped with its recipe.
	// (No live embeddings endpoint here: skip the dimension probe.)
	d.embedProbe = nil
	if changed, err := d.ApplyEmbedConfig(eff); !changed || err != nil {
		t.Fatalf("ApplyEmbedConfig: changed %v err %v", changed, err)
	}
	if changed, _ := d.ApplyEmbedConfig(eff); changed {
		t.Error("re-applying the same config reported a change")
	}
	if got := mustID(t, d.currentEmbedRecipe()); got != dbID {
		t.Errorf("chunk stamp = %s, want the db recipe %s", got, dbID)
	}

	// Unset: back to the env recipe.
	if _, err := d.UnsetModelSetting(ctx, recipe.StepEmbed, "cli:a", ""); err != nil {
		t.Fatal(err)
	}
	back, err := EffectiveConfig(ctx, d, base, recipe.StepEmbed)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustID(t, EmbedRecipeFor(back)); got != envID {
		t.Errorf("after unset the embed recipe is %s, want the env %s", got, envID)
	}
	if _, err := d.ApplyEmbedConfig(back); err != nil {
		t.Fatal(err)
	}
	if got := mustID(t, d.currentEmbedRecipe()); got != envID {
		t.Errorf("after unset the chunk stamp is %s, want the env %s", got, envID)
	}

	// An invalid row fails strictly and names the step and the fix.
	if _, _, err := d.SetModelSetting(ctx, ModelSetting{Step: recipe.StepEmbed, EndpointID: "gone", Model: "x"}, "cli:a", ""); err != nil {
		t.Fatal(err)
	}
	_, err = EffectiveConfig(ctx, d, base, recipe.StepEmbed)
	var se *config.StepSettingError
	if !errors.As(err, &se) || se.Step != recipe.StepEmbed || !strings.Contains(err.Error(), "earmark models unset embed") {
		t.Errorf("invalid row: %v", err)
	}
	// ...but only for the steps asked for.
	if _, err := EffectiveConfig(ctx, d, base, recipe.StepPropose); err != nil {
		t.Errorf("an invalid embed row failed the propose step: %v", err)
	}
}

func valueModel(v *ModelSettingValue) string {
	if v == nil {
		return ""
	}
	return v.Model
}
