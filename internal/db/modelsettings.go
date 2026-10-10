package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// Runtime step model settings (CONTRACT §2.20): model_settings holds at most
// one row per AI step overriding the env default (AI_ENDPOINTS + AI_ROLES +
// MODELS_FILE) for that step; model_setting_events is its append-only history,
// written in the same transaction as every change. Readers apply the rows onto
// their env config with config.WithStepSettings.

// Model setting event actions.
const (
	ModelSettingSet   = "set"
	ModelSettingUnset = "unset"
)

// ModelSettingsRefresh is how often a long-running process without a natural
// per-batch seam (the search embedder of mcp/serve) re-reads model_settings.
const ModelSettingsRefresh = 30 * time.Second

// MaxModelSettingHistory caps one ModelSettingHistory read.
const MaxModelSettingHistory = 1000

// maxModelSettingText bounds every text column a setting carries, and
// maxModelSettingNote an event's note (both also CHECKed by the table, in
// characters).
const (
	maxModelSettingText = 256
	maxModelSettingNote = 512
)

// ModelSetting is one model_settings row.
type ModelSetting struct {
	Step          string         `json:"step"`
	EndpointID    string         `json:"endpoint_id"`
	Model         string         `json:"model"`
	ExpectedModel string         `json:"expected_model,omitempty"`
	Revision      string         `json:"revision,omitempty"`
	Params        map[string]any `json:"params,omitempty"`
	UpdatedAt     time.Time      `json:"updated_at"`
	UpdatedBy     string         `json:"updated_by"`
}

// StepSetting is the row as a config setting (Source db).
func (m ModelSetting) StepSetting() config.StepSetting {
	return config.StepSetting{
		Step: m.Step, EndpointID: m.EndpointID, Model: m.Model,
		ExpectedModel: m.ExpectedModel, Revision: m.Revision, Params: m.Params,
		Source: config.SourceDB,
	}
}

// ModelSettingValue is a row's value as recorded in an event (old/new).
type ModelSettingValue struct {
	EndpointID    string         `json:"endpoint_id"`
	Model         string         `json:"model"`
	ExpectedModel string         `json:"expected_model,omitempty"`
	Revision      string         `json:"revision,omitempty"`
	Params        map[string]any `json:"params,omitempty"`
}

func (m ModelSetting) value() ModelSettingValue {
	return ModelSettingValue{
		EndpointID: m.EndpointID, Model: m.Model, ExpectedModel: m.ExpectedModel,
		Revision: m.Revision, Params: m.Params,
	}
}

// ModelSettingEvent is one model_setting_events row.
type ModelSettingEvent struct {
	ID     int64              `json:"id"`
	Step   string             `json:"step"`
	Action string             `json:"action"`
	Old    *ModelSettingValue `json:"old,omitempty"`
	New    *ModelSettingValue `json:"new,omitempty"`
	Actor  string             `json:"actor"`
	Note   string             `json:"note,omitempty"`
	At     time.Time          `json:"at"`
}

// SettingsReader reads the model_settings rows (*DB implements it).
type SettingsReader interface {
	ListModelSettings(ctx context.Context) ([]ModelSetting, error)
}

// StepSettings converts rows to config settings, keeping only the named
// steps (all rows when none are named).
func StepSettings(rows []ModelSetting, steps ...string) []config.StepSetting {
	out := make([]config.StepSetting, 0, len(rows))
	for _, r := range rows {
		if len(steps) > 0 && !containsStr(steps, r.Step) {
			continue
		}
		out = append(out, r.StepSetting())
	}
	return out
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// EffectiveConfig loads model_settings and applies the rows for steps (every
// row when none are named) onto base, strictly: a row that is invalid against
// base's registry fails with a *config.StepSettingError naming the step and
// the fix — the step never silently runs on its env default instead. base is
// not modified.
func EffectiveConfig(ctx context.Context, r SettingsReader, base *config.Config, steps ...string) (*config.Config, error) {
	rows, err := r.ListModelSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read model_settings: %w", err)
	}
	return base.WithStepSettings(StepSettings(rows, steps...)...)
}

// ResolveStep is the effective config for one step of an ad-hoc run
// (`earmark eval|decide|scan`): base, then the step's model_settings row,
// then the run's --endpoint / --model override (both "" = none). Every
// error names the step; nothing falls back to another model.
func ResolveStep(ctx context.Context, r SettingsReader, base *config.Config, step, endpoint, model string) (*config.Config, error) {
	cfg, err := EffectiveConfig(ctx, r, base, step)
	if err != nil {
		return nil, err
	}
	ov, ok, err := cfg.OverrideSetting(step, endpoint, model)
	if err != nil || !ok {
		return cfg, err
	}
	return cfg.WithStepSettings(ov)
}

// ApplyEmbedSettings points this process's embedder at the effective embed
// setting: base with the embed row of r applied (CONTRACT §2.20). A read
// error, an invalid row or a failed dimension probe keeps the current
// embedder and returns the error (also kept for EmbedSettingError, so the
// Models page shows why search is not on the set model). Search processes
// call it once before serving, then WatchEmbedSettings keeps it current.
func (db *DB) ApplyEmbedSettings(ctx context.Context, r SettingsReader, base *config.Config) (changed bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg, err := EffectiveConfig(cctx, r, base, recipe.StepEmbed)
	if err != nil {
		db.setEmbedSettingErr(err.Error())
		return false, err
	}
	changed, err = db.ApplyEmbedConfig(cfg)
	if changed {
		ep, _ := cfg.EmbeddingsEndpoint()
		db.log.Info("search embedder follows the embed model setting",
			"endpoint", ep.ID, "model", ep.Model, "source", cfg.StepSource(recipe.StepEmbed))
	}
	return changed, err
}

// WatchEmbedSettings re-applies the embed setting every interval until ctx
// ends (ApplyEmbedSettings). Failures keep the embedder last applied and are
// logged once per distinct error — the query side of search has no run to
// fail.
func (db *DB) WatchEmbedSettings(ctx context.Context, r SettingsReader, base *config.Config, interval time.Duration) {
	var lastErr string
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, err := db.ApplyEmbedSettings(ctx, r, base)
		switch {
		case err == nil:
			lastErr = ""
		case err.Error() != lastErr:
			lastErr = err.Error()
			db.log.Error("embed model setting not applied; search keeps its current embedder", "error", err)
		}
	}
}

const listModelSettingsSQL = `
	SELECT step, endpoint_id, model, COALESCE(expected_model, ''), COALESCE(revision, ''),
	       params, updated_at, updated_by
	  FROM model_settings
	 ORDER BY step`

// ListModelSettings returns every model_settings row, ordered by step.
func (db *DB) ListModelSettings(ctx context.Context) ([]ModelSetting, error) {
	rows, err := db.pool.Query(ctx, listModelSettingsSQL)
	if err != nil {
		return nil, fmt.Errorf("list model settings: %w", err)
	}
	defer rows.Close()
	var out []ModelSetting
	for rows.Next() {
		var m ModelSetting
		var params []byte
		if err := rows.Scan(&m.Step, &m.EndpointID, &m.Model, &m.ExpectedModel, &m.Revision,
			&params, &m.UpdatedAt, &m.UpdatedBy); err != nil {
			return nil, fmt.Errorf("scan model setting: %w", err)
		}
		if m.Params, err = decodeParams(params); err != nil {
			return nil, fmt.Errorf("model setting %s params: %w", m.Step, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func decodeParams(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if len(p) == 0 {
		return nil, nil
	}
	return p, nil
}

// lockModelSettingSQL serializes changes to one step's setting, so the old
// value an event records is the value the change replaced even for the very
// first set (no row to lock FOR UPDATE yet).
const lockModelSettingSQL = `SELECT pg_advisory_xact_lock(hashtext('earmark.model_settings'), hashtext($1))`

const selectModelSettingForUpdateSQL = `
	SELECT step, endpoint_id, model, COALESCE(expected_model, ''), COALESCE(revision, ''),
	       params, updated_at, updated_by
	  FROM model_settings
	 WHERE step = $1
	   FOR UPDATE`

const upsertModelSettingSQL = `
	INSERT INTO model_settings (step, endpoint_id, model, expected_model, revision, params, updated_at, updated_by)
	VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6::jsonb, clock_timestamp(), $7)
	ON CONFLICT (step) DO UPDATE
	SET endpoint_id = EXCLUDED.endpoint_id, model = EXCLUDED.model,
	    expected_model = EXCLUDED.expected_model, revision = EXCLUDED.revision,
	    params = EXCLUDED.params, updated_at = clock_timestamp(), updated_by = EXCLUDED.updated_by
	RETURNING updated_at`

const deleteModelSettingSQL = `DELETE FROM model_settings WHERE step = $1`

const insertModelSettingEventSQL = `
	INSERT INTO model_setting_events (step, action, old_value, new_value, actor, note)
	VALUES ($1, $2, $3::jsonb, $4::jsonb, $5, NULLIF($6, ''))`

// ErrInvalidModelSetting marks a setting refused before any write.
var ErrInvalidModelSetting = errors.New("invalid model setting")

// checkSettingText validates the row-shape rules the table enforces too, so a
// bad value is a clear error instead of a constraint violation. Whether the
// endpoint and model fit the registry is config.ValidateStepSetting's job,
// done by the caller against its own AI_ENDPOINTS.
func checkSettingText(step, actor string, s *ModelSetting) error {
	if !containsStr(config.SettingSteps, step) {
		return fmt.Errorf("%w: unknown step %q (settable: %s)", ErrInvalidModelSetting, step,
			strings.Join(config.SettingSteps, ", "))
	}
	if strings.TrimSpace(actor) == "" {
		return fmt.Errorf("%w: actor is required", ErrInvalidModelSetting)
	}
	if s == nil {
		return nil
	}
	if s.EndpointID == "" || s.Model == "" {
		return fmt.Errorf("%w: endpoint and model are required", ErrInvalidModelSetting)
	}
	for _, v := range []string{s.EndpointID, s.Model, s.ExpectedModel, s.Revision, actor} {
		if utf8.RuneCountInString(v) > maxModelSettingText {
			return fmt.Errorf("%w: value longer than %d bytes", ErrInvalidModelSetting, maxModelSettingText)
		}
	}
	return nil
}

// SetModelSetting upserts the step's row and appends a "set" event recording
// the previous value, in one transaction. The caller has validated s against
// its registry (config.ValidateStepSetting). Returns the previous row (nil
// when the env default was in effect) and the stored row.
func (db *DB) SetModelSetting(ctx context.Context, s ModelSetting, actor, note string) (prev *ModelSetting, stored ModelSetting, err error) {
	unchanged := false
	s.Step, s.EndpointID, s.Model = strings.TrimSpace(s.Step), strings.TrimSpace(s.EndpointID), strings.TrimSpace(s.Model)
	s.ExpectedModel, s.Revision = strings.TrimSpace(s.ExpectedModel), strings.TrimSpace(s.Revision)
	if err := checkSettingText(s.Step, actor, &s); err != nil {
		return nil, ModelSetting{}, err
	}
	params := s.Params
	if params == nil {
		params = map[string]any{}
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return nil, ModelSetting{}, fmt.Errorf("encode params: %w", err)
	}
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if prev, err = lockAndReadSetting(ctx, tx, s.Step); err != nil {
			return err
		}
		if prev != nil && sameSettingValue(prev.value(), s.value()) {
			// Nothing changes: no write, no event (CLI says "unchanged").
			s = *prev
			unchanged = true
			return nil
		}
		if err := tx.QueryRow(ctx, upsertModelSettingSQL, s.Step, s.EndpointID, s.Model, s.ExpectedModel,
			s.Revision, string(rawParams), actor).Scan(&s.UpdatedAt); err != nil {
			return fmt.Errorf("upsert model setting %s: %w", s.Step, err)
		}
		s.UpdatedBy = actor
		return appendSettingEvent(ctx, tx, s.Step, ModelSettingSet, prev, &s, actor, note)
	})
	if err != nil {
		return nil, ModelSetting{}, err
	}
	if unchanged {
		return prev, s, ErrModelSettingUnchanged
	}
	return prev, s, nil
}

// ErrModelSettingUnchanged is SetModelSetting's answer to a set equal to the
// stored row: nothing was written (no event). prev and stored are the row.
var ErrModelSettingUnchanged = errors.New("model setting unchanged")

// sameSettingValue compares two setting values by their JSON form (params
// decoded from the database are float64 like the CLI's).
func sameSettingValue(a, b ModelSettingValue) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// UnsetModelSetting deletes the step's row and appends an "unset" event, in
// one transaction, returning the deleted row. With no row it writes nothing
// and returns nil: the step was already on its env default.
func (db *DB) UnsetModelSetting(ctx context.Context, step, actor, note string) (prev *ModelSetting, err error) {
	step = strings.TrimSpace(step)
	if err := checkSettingText(step, actor, nil); err != nil {
		return nil, err
	}
	err = pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if prev, err = lockAndReadSetting(ctx, tx, step); err != nil || prev == nil {
			return err
		}
		if _, err := tx.Exec(ctx, deleteModelSettingSQL, step); err != nil {
			return fmt.Errorf("delete model setting %s: %w", step, err)
		}
		return appendSettingEvent(ctx, tx, step, ModelSettingUnset, prev, nil, actor, note)
	})
	if err != nil {
		return nil, err
	}
	return prev, nil
}

func lockAndReadSetting(ctx context.Context, tx pgx.Tx, step string) (*ModelSetting, error) {
	if _, err := tx.Exec(ctx, lockModelSettingSQL, step); err != nil {
		return nil, fmt.Errorf("lock model setting %s: %w", step, err)
	}
	var m ModelSetting
	var params []byte
	err := tx.QueryRow(ctx, selectModelSettingForUpdateSQL, step).Scan(&m.Step, &m.EndpointID, &m.Model,
		&m.ExpectedModel, &m.Revision, &params, &m.UpdatedAt, &m.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read model setting %s: %w", step, err)
	}
	if m.Params, err = decodeParams(params); err != nil {
		return nil, fmt.Errorf("model setting %s params: %w", step, err)
	}
	return &m, nil
}

func appendSettingEvent(ctx context.Context, tx pgx.Tx, step, action string, prev, next *ModelSetting, actor, note string) error {
	enc := func(m *ModelSetting) (*string, error) {
		if m == nil {
			return nil, nil
		}
		b, err := json.Marshal(m.value())
		if err != nil {
			return nil, err
		}
		s := string(b)
		return &s, nil
	}
	oldV, err := enc(prev)
	if err != nil {
		return fmt.Errorf("encode old model setting: %w", err)
	}
	newV, err := enc(next)
	if err != nil {
		return fmt.Errorf("encode new model setting: %w", err)
	}
	if r := []rune(note); len(r) > maxModelSettingNote {
		note = string(r[:maxModelSettingNote])
	}
	if _, err := tx.Exec(ctx, insertModelSettingEventSQL, step, action, oldV, newV, actor, note); err != nil {
		return fmt.Errorf("record model setting event: %w", err)
	}
	return nil
}

// The history reads: one statement per shape, so the step filter can use
// model_setting_events_step_idx under a cached generic plan.
const (
	modelSettingHistoryAllSQL = `
	SELECT id, step, action, old_value, new_value, actor, COALESCE(note, ''), created_at
	  FROM model_setting_events
	 ORDER BY id DESC
	 LIMIT $1`
	modelSettingHistoryStepSQL = `
	SELECT id, step, action, old_value, new_value, actor, COALESCE(note, ''), created_at
	  FROM model_setting_events
	 WHERE step = $1
	 ORDER BY id DESC
	 LIMIT $2`
)

// ModelSettingHistory returns the newest limit events (1..MaxModelSettingHistory),
// newest first, for one step or every step ("").
func (db *DB) ModelSettingHistory(ctx context.Context, step string, limit int) ([]ModelSettingEvent, error) {
	if limit < 1 || limit > MaxModelSettingHistory {
		return nil, fmt.Errorf("history limit must be 1..%d", MaxModelSettingHistory)
	}
	var (
		rows pgx.Rows
		err  error
	)
	if step == "" {
		rows, err = db.pool.Query(ctx, modelSettingHistoryAllSQL, limit)
	} else {
		rows, err = db.pool.Query(ctx, modelSettingHistoryStepSQL, step, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("model setting history: %w", err)
	}
	defer rows.Close()
	var out []ModelSettingEvent
	for rows.Next() {
		var e ModelSettingEvent
		var oldV, newV []byte
		if err := rows.Scan(&e.ID, &e.Step, &e.Action, &oldV, &newV, &e.Actor, &e.Note, &e.At); err != nil {
			return nil, fmt.Errorf("scan model setting event: %w", err)
		}
		if e.Old, err = decodeSettingValue(oldV); err != nil {
			return nil, fmt.Errorf("model setting event %d old value: %w", e.ID, err)
		}
		if e.New, err = decodeSettingValue(newV); err != nil {
			return nil, fmt.Errorf("model setting event %d new value: %w", e.ID, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func decodeSettingValue(raw []byte) (*ModelSettingValue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v ModelSettingValue
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
