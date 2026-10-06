package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/recipe"
)

// ASR provenance (CONTRACT §1.9, migration 00006). The ASR runner reports what
// it ran — model name, the .nemo sha256, its version tag and its parameters —
// on each transcripts row; the Go worker turns that into an asr recipe and
// stamps transcripts.recipe_id. Rows from a runner that reports nothing stay
// unstamped (NULL), because nothing is invented.

// asrStepVersion is bumped whenever earmark's own handling of runner output
// changes what an asr recipe means (CONTRACT §1.9).
const asrStepVersion = 1

// ASRProvenance is the runner-reported provenance of one transcript.
type ASRProvenance struct {
	ModelName     string
	ModelSHA256   string
	RunnerVersion string
	// Params is the runner's asr_params object; nil when absent.
	Params map[string]any
}

// ASRRecipe builds the asr recipe for runner-reported provenance: the model
// the runner loaded is both what was asked for and what answered, the .nemo
// sha256 pins the weights, and the runner's version tag is the code version.
func ASRRecipe(p ASRProvenance) recipe.Recipe {
	return recipe.Recipe{
		Step:          recipe.StepASR,
		StepVersion:   asrStepVersion,
		CodeVersion:   p.RunnerVersion,
		ModelAlias:    p.ModelName,
		ModelResolved: p.ModelName,
		ModelRevision: p.ModelSHA256,
		Params:        p.Params,
	}
}

// StampedTranscript is one transcript StampASRRecipes stamped.
type StampedTranscript struct {
	ID           string
	FilePath     string
	RecipeID     string
	EmbeddedASIN string
}

// selectUnstampedASRSQL selects one keyset page of transcripts a
// provenance-aware runner wrote that have no recipe yet (served by
// transcripts_asr_unstamped_idx), after the cursor (created_at, id). A runner
// version of "unknown" (a runner that cannot tell its own version) is never
// selected: a recipe with that code_version would invent provenance (CONTRACT
// §1.9). It reads only the small provenance columns — never segments or
// raw_text.
const selectUnstampedASRSQL = `
	SELECT id::text, created_at, file_path, model_name, coalesce(asr_model_sha256, ''),
	       asr_runner_version, coalesce(asr_params::text, ''), coalesce(embedded_asin, '')
	  FROM transcripts
	 WHERE recipe_id IS NULL AND asr_runner_version IS NOT NULL
	   AND asr_runner_version NOT IN ('', 'unknown')
	   AND (created_at, id) > ($1, $2::uuid)
	 ORDER BY created_at, id
	 LIMIT $3`

// stampASRSQL stamps one transcript; guarded so a row stamped meanwhile (a
// second ingest process) is never overwritten.
const stampASRSQL = `UPDATE transcripts SET recipe_id = $1 WHERE id = $2 AND recipe_id IS NULL`

// StampASRRecipes stamps every unstamped transcript a provenance-aware runner
// wrote with the asr recipe built from its runner-reported provenance. It
// walks the selection in keyset pages of pageSize (bounded memory), and stamps
// each row in its own transaction (register the recipe, set recipe_id), so a
// row that cannot be stamped — malformed asr_params, an invalid recipe — is
// skipped and reported in the returned error without blocking the rows behind
// it or being retried ahead of them. Rows whose runner version is unknown are
// not selected at all. It returns the rows it stamped.
func (db *DB) StampASRRecipes(ctx context.Context, pageSize int) ([]StampedTranscript, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	// The model registry's asr pin (MODELS_FILE, CONTRACT §2.18) is what the
	// runner is expected to run; a runner reporting something else still gets
	// an honest recipe, and the mismatch is logged.
	pin := db.cfg.ModelPin(recipe.StepASR)

	var (
		stamped []StampedTranscript
		errs    []error
		afterAt = time.Time{}
		afterID = "00000000-0000-0000-0000-000000000000"
	)
	for {
		page, err := db.unstampedASRPage(ctx, afterAt, afterID, pageSize)
		if err != nil {
			return stamped, errors.Join(append(errs, err)...)
		}
		for _, p := range page {
			afterAt, afterID = p.createdAt, p.ID
			if p.paramsErr != nil {
				errs = append(errs, p.paramsErr)
				continue
			}
			modelDiffers := pin.ExpectedModel != "" && pin.ExpectedModel != p.prov.ModelName
			revisionDiffers := pin.Revision != "" && p.prov.ModelSHA256 != "" && pin.Revision != p.prov.ModelSHA256
			if modelDiffers || revisionDiffers {
				db.log.Warn("runner reported an ASR model other than the registry pin",
					"transcript_id", p.ID, "model", p.prov.ModelName, "sha256", p.prov.ModelSHA256,
					"expected_model", pin.ExpectedModel, "expected_revision", pin.Revision)
			}
			id, ok, err := db.stampOneASR(ctx, p.ID, p.prov)
			if err != nil {
				errs = append(errs, fmt.Errorf("transcript %s: %w", p.ID, err))
				continue
			}
			if ok {
				p.RecipeID = id
				stamped = append(stamped, p.StampedTranscript)
			}
		}
		if len(page) < pageSize {
			return stamped, errors.Join(errs...)
		}
	}
}

// unstampedRow is one row of selectUnstampedASRSQL.
type unstampedRow struct {
	StampedTranscript
	createdAt time.Time
	prov      ASRProvenance
	paramsErr error
}

func (db *DB) unstampedASRPage(ctx context.Context, afterAt time.Time, afterID string, limit int) ([]unstampedRow, error) {
	rows, err := db.pool.Query(ctx, selectUnstampedASRSQL, afterAt, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("select unstamped transcripts: %w", err)
	}
	page, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (unstampedRow, error) {
		var p unstampedRow
		var params string
		if err := r.Scan(&p.ID, &p.createdAt, &p.FilePath, &p.prov.ModelName, &p.prov.ModelSHA256,
			&p.prov.RunnerVersion, &params, &p.EmbeddedASIN); err != nil {
			return p, err
		}
		if params != "" {
			if err := json.Unmarshal([]byte(params), &p.prov.Params); err != nil {
				p.paramsErr = fmt.Errorf("transcript %s asr_params: %w", p.ID, err)
			}
		}
		return p, nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan unstamped transcripts: %w", err)
	}
	return page, nil
}

// stampOneASR registers the recipe and stamps one transcript in a transaction.
// ok is false when the row was stamped meanwhile by another process.
func (db *DB) stampOneASR(ctx context.Context, transcriptID string, prov ASRProvenance) (string, bool, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("begin asr stamp tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, err := registerRecipe(ctx, tx, ASRRecipe(prov))
	if err != nil {
		return "", false, err
	}
	tag, err := tx.Exec(ctx, stampASRSQL, id, transcriptID)
	if err != nil {
		return "", false, fmt.Errorf("stamp: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("commit asr stamp: %w", err)
	}
	return id, tag.RowsAffected() == 1, nil
}

// EmbeddedASIN returns the ASIN tag the runner read from filePath's audio
// ("" when the file carries none or has no transcript yet). It is the third
// ASIN source after the directory and the filename (CONTRACT §1.6) and
// satisfies metaprovider.EmbeddedASINSource.
func (db *DB) EmbeddedASIN(ctx context.Context, filePath string) (string, error) {
	var asin string
	err := db.pool.QueryRow(ctx, `
		SELECT embedded_asin FROM transcripts
		 WHERE file_path = $1 AND embedded_asin IS NOT NULL
		 ORDER BY created_at DESC
		 LIMIT 1`, filePath).Scan(&asin)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("embedded asin for %s: %w", filePath, err)
	}
	return asin, nil
}

// CurrentRecipe is one current_recipes row with the fields earmark_recipe_info
// labels carry.
type CurrentRecipe struct {
	Step          string
	RecipeID      string
	Model         string // model_resolved, else model_alias
	Revision      string
	PromptVersion string
}

// ListCurrentRecipes returns the current recipe of every step that has one.
func (db *DB) ListCurrentRecipes(ctx context.Context) ([]CurrentRecipe, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT cr.step, cr.recipe_id, coalesce(r.model_resolved, r.model_alias, ''),
		       coalesce(r.model_revision, ''), coalesce(r.prompt_version, '')
		  FROM current_recipes cr
		  JOIN recipes r ON r.recipe_id = cr.recipe_id
		 ORDER BY cr.step`)
	if err != nil {
		return nil, fmt.Errorf("list current recipes: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CurrentRecipe, error) {
		var c CurrentRecipe
		err := r.Scan(&c.Step, &c.RecipeID, &c.Model, &c.Revision, &c.PromptVersion)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan current recipes: %w", err)
	}
	return out, nil
}

// staleCountsSQL counts stale_work rows per step that has a current recipe (a
// step without one reports nothing, CONTRACT §1.9). The step filter is pushed
// into the view's UNION ALL branches, so each count only scans its own table.
const staleCountsSQL = `
	SELECT cr.step, (SELECT count(*) FROM stale_work s WHERE s.step = cr.step)
	  FROM current_recipes cr
	 ORDER BY cr.step`

// StaleItemCounts returns the number of stale output rows per step, for every
// step that has a current recipe (0 included). Backs earmark_stale_items.
func (db *DB) StaleItemCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := db.pool.Query(ctx, staleCountsSQL)
	if err != nil {
		return nil, fmt.Errorf("count stale work: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var step string
		var n int64
		if err := rows.Scan(&step, &n); err != nil {
			return nil, fmt.Errorf("scan stale count: %w", err)
		}
		out[step] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count stale work: %w", err)
	}
	return out, nil
}
