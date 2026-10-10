package mcp

import (
	"context"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// ─── Runtime step model settings on the Models page (CONTRACT §2.20) ─────────
//
// The page and GET /api/v1/status show each AI step's EFFECTIVE model: the env
// registry with the model_settings rows applied. Read-only — the rows are
// changed with `earmark models set|unset`. A row that is invalid against this
// deployment's AI_ENDPOINTS is shown as an error on its role card (the step's
// runs fail until it is fixed); it is never displayed as the env model.

// modelSettingsReader is the optional DB capability that lists model_settings
// (*db.DB and the demo implement it). A DBInterface without it shows the env
// config, as before.
type modelSettingsReader interface {
	ListModelSettings(ctx context.Context) ([]db.ModelSetting, error)
}

// modelSettingsTimeout bounds the model_settings read on a page render.
const modelSettingsTimeout = 2 * time.Second

// effectiveModels is one render's view of the runtime model settings.
type effectiveModels struct {
	// Cfg is the server's env config with every valid row applied (the
	// server's config itself when there are none, or the read failed).
	Cfg *config.Config
	// Rows are the model_settings rows by step.
	Rows map[string]db.ModelSetting
	// Invalid are the rows that cannot apply to this deployment, by step.
	Invalid map[string]error
	// Err is a failed read: Cfg is then the env config and the cards say the
	// settings are unknown rather than claiming "env".
	Err error
	// EmbedErr is why this process's search embedder is not on the effective
	// embed setting (a refused swap: a dimension probe failure or an invalid
	// row), from the DB's EmbedSettingError; "" when it is.
	EmbedErr string
}

// embedSettingReporter is the optional DB capability reporting a refused
// search-embedder swap (*db.DB implements it).
type embedSettingReporter interface {
	EmbedSettingError() string
}

// effectiveModels reads model_settings and applies the valid rows onto the
// server's env config. It never fails the render.
func (s *MCPServer) effectiveModels(ctx context.Context) (em effectiveModels) {
	em = effectiveModels{Cfg: s.cfg}
	defer func() {
		if er, ok := s.db.(embedSettingReporter); ok {
			em.EmbedErr = er.EmbedSettingError()
		}
	}()
	r, ok := s.db.(modelSettingsReader)
	if !ok || s.cfg == nil {
		return em
	}
	ctx, cancel := context.WithTimeout(ctx, modelSettingsTimeout)
	defer cancel()
	rows, err := r.ListModelSettings(ctx)
	if err != nil {
		s.logger.Warn("models: model_settings unavailable; showing the env models", "error", err)
		em.Err = err
		return em
	}
	em = applyModelSettings(s.cfg, rows)
	return em
}

// applyModelSettings is the pure half of effectiveModels.
func applyModelSettings(base *config.Config, rows []db.ModelSetting) effectiveModels {
	em := effectiveModels{Cfg: base}
	if len(rows) == 0 {
		return em
	}
	em.Rows = make(map[string]db.ModelSetting, len(rows))
	for _, r := range rows {
		em.Rows[r.Step] = r
	}
	em.Cfg, em.Invalid = base.WithValidStepSettings(db.StepSettings(rows)...)
	return em
}

// applySettingState sets a role card's model source, the who/when of a db
// row, and the error state of an invalid row. It runs after the card's own
// builder so an invalid row overrides whatever health the env model had: the
// step does not run until the row is fixed.
func applySettingState(c *roleCard, em effectiveModels) {
	if c.Step == "" || !c.HasModel {
		return
	}
	if _, settable := config.StepEndpointType(c.Step); !settable {
		return
	}
	if em.Err != nil {
		c.ModelSource = "unknown"
		return
	}
	row, hasRow := em.Rows[c.Step]
	if err, bad := em.Invalid[c.Step]; bad {
		c.ModelSource = config.SourceDB
		c.SettingBy, c.SettingAt = row.UpdatedBy, row.UpdatedAt
		c.SettingError = err.Error()
		c.Configured = true
		c.Requested, c.EndpointID = row.Model, row.EndpointID
		c.Expected, c.ExpectedRevision = row.ExpectedModel, row.Revision
		c.Health = health(roleFailing, "model_settings row is invalid — "+c.Step+" runs fail until it is fixed (earmark models unset "+c.Step+")")
		c.AllowlistWarn = false
		if c.AnsweredMatch == matchOK || c.AnsweredMatch == matchMismatch || c.AnsweredMatch == matchPredates {
			// The last answer was to another request; nothing runs under
			// this row, so there is nothing to compare it with.
			c.AnsweredMatch, c.PriorModelChanged = matchUnchecked, false
		}
		return
	}
	c.ModelSource = em.Cfg.StepSource(c.Step)
	if hasRow {
		c.SettingBy, c.SettingAt = row.UpdatedBy, row.UpdatedAt
	}
	if c.Step == recipe.StepEmbed && em.EmbedErr != "" {
		// This process refused to switch search to the set embed model (its
		// vectors do not fit vector(768), or it could not be probed): search
		// still embeds queries with the previous model.
		c.SearchEmbedError = em.EmbedErr
		c.Health = health(roleDegraded, "search kept its previous embedder — the embed setting was refused (see below)")
	}
	if ep, ok := em.Cfg.StepEndpoint(c.Step); ok && ep.BaseID != "" {
		// A synthesized endpoint (another model on a registered one): name
		// the registered endpoint on the card.
		c.EndpointLabel = ep.BaseID
	}
}

// effectiveStep is base with the step's model_settings row applied, strictly
// (an invalid row is an error naming the step). Without a settings reader it
// is base.
func (s *MCPServer) effectiveStep(ctx context.Context, base *config.Config, step string) (*config.Config, error) {
	r, ok := s.db.(modelSettingsReader)
	if !ok {
		return base, nil
	}
	ctx, cancel := context.WithTimeout(ctx, modelSettingsTimeout)
	defer cancel()
	return db.EffectiveConfig(ctx, r, base, step)
}

// evalConfigured reports whether the "run eval" buttons may show: an env
// judge, or a valid propose model setting naming a chat endpoint.
func (s *MCPServer) evalConfigured(ctx context.Context) bool {
	if s.eval.configured {
		return true
	}
	if s.eval.run == nil || s.cfg == nil {
		return false
	}
	if _, ok := s.db.(modelSettingsReader); !ok {
		return false
	}
	eff, err := s.effectiveStep(ctx, s.cfg, recipe.StepPropose)
	if err != nil {
		return false
	}
	_, ok := eff.EvalEndpoint()
	return ok
}
