-- 00013_audit_lineage.sql — audit lineage for transcripts and findings
-- (CONTRACT §1.4 "Operator requeue", §1.9, §2.17; docs/design/audit-lineage.md).
--
-- Nothing a model or an operator did to a transcript is thrown away any more.
-- This migration adds the records; most writers that fill them come later and
-- are noted as such.
--
--   * judge_passes — one row per (transcript × judge recipe) pass: status,
--     chunk counters, findings kept, drop counts by reason, tokens, cost,
--     latency percentiles and the book-context hash the judge saw. It is the
--     per-recipe "judged" latch; run_metrics.eval_* stays the compatibility
--     "latest judge" summary. transcript_id is a plain column, not a foreign
--     key (like finding_events): a pass outlives the requeue that deletes its
--     transcript. step_run_id is plain too: nothing references step_runs
--     (00010 — recording a run must never be able to fail, or block, the
--     step). No live path writes a non-legacy row yet.
--   * transcript_archive — written by requeue (internal/db requeueTx) in the
--     same transaction, before the transcript is deleted: every transcripts
--     column (metadata, raw_text, segments) plus the job's whole run_metrics
--     row as jsonb. The ASR pass a requeue replaces is kept, not lost.
--   * transcript_findings gains
--       judge_pass_id        the pass that proposed it (ON DELETE SET NULL);
--       fn_call_id           the fn_calls row of the judge call (no writer yet);
--       origin_transcript_id the transcript it was proposed against. Set on
--                            INSERT from transcript_id by a trigger and never
--                            changed afterwards (an UPDATE that changes a set
--                            value is refused), so it survives requeue's
--                            ON DELETE SET NULL of transcript_id and joins to
--                            transcript_archive.transcript_id;
--       correction_key       md5 over (chunk_text_sha256, norm(original_text),
--                            norm(suggested_correction)), generated. It does
--                            not include the recipe: two recipes proposing the
--                            same fix to the same chunk text share a key. norm
--                            collapses every whitespace run (ASCII whitespace
--                            plus NBSP and the other Unicode space separators)
--                            to one space, trims, and lower-cases with the ICU
--                            root collation ("und-x-icu"): the live cluster is
--                            initdb'd LC_CTYPE=C, where plain lower() folds
--                            ASCII only ('ÉMILE' would not equal 'émile'). No
--                            accent folding: 'é' and 'e' are different
--                            corrections. Parts are joined with U+001F so
--                            ('ab','c') and ('a','bc') differ; a NULL
--                            correction counts as ''. NULL when the finding has
--                            no chunk hash (legacy rows), because without the
--                            chunk revision the text alone does not identify a
--                            fix. Code that needs the key reads this column; it
--                            never recomputes it;
--       duplicate_of         the representative a 'duplicate' finding agrees
--                            with (ON DELETE SET NULL).
--   * patch_state gains 'duplicate': a finding that proposes the same
--     correction as another (duplicate_of). Never in the overlay, never
--     replayed. CHECK (duplicate_of IS NULL OR patch_state IN ('duplicate',
--     'superseded')): superseded is admitted so a requeue or clear archives a
--     duplicate without erasing which finding it agreed with; returning a
--     duplicate to proposed must clear duplicate_of in the same UPDATE.
--   * finding_events gains anchor_offset, and a second trigger,
--     transcript_findings_record_reanchor, appends a 'transition' with
--     from_state = to_state = OLD.patch_state and reason 'reanchor_moved'
--     ('reanchor_placed' when the finding had no anchor_offset yet — a legacy
--     finding's first anchoring) whenever chunk_text_sha256 or anchor_offset
--     changes, recording the OLD
--     hash and offset (the new ones are on the row). When the same UPDATE also
--     changes patch_state (a re-anchor returning an unanchorable finding to
--     proposed) the transition trigger fires too: two events, the reanchor one
--     first (AFTER triggers fire in name order), so "latest transition at t"
--     still ends on the new state.
--   * finding_events_record_transition fills reason from the transaction-local
--     setting earmark.reason (db.SetEventContext), like earmark.actor. Requeue
--     sets 'requeue', ClearFindings 'cleared'.
--   * stale_work's propose arm skips 'duplicate' as well as 'superseded': a
--     duplicate is re-proposed work only through its representative, so
--     counting both would count one fix twice. Down restores 00009's view.
--
-- Backfill.
--   judge_passes: one 'legacy' row per (transcript, recipe_id) among
--   origin='judge' findings whose transcript still exists (recipe_id may be
--   NULL: unstamped findings form their own group), findings_kept = its count,
--   started/finished = the first/last finding's created_at. Plus one 'legacy'
--   row for the latest judge run of every transcript latched as judged
--   (run_metrics.eval_finished_at) when that run left no finding, with
--   chunks_total = eval_chunks and the run's eval_started_at/eval_finished_at.
--   Its recipe is resolved from run_metrics.eval_model against the propose
--   recipes' model_alias / model_resolved only when exactly one recipe matches
--   (otherwise NULL — the model name alone cannot say which prompt or params
--   judged it). A resolved recipe gets the row unless the transcript already
--   has a pass for it — so a transcript whose older recipe left findings and
--   whose newest recipe found nothing gets both. An unresolved latch gets a
--   NULL-recipe row only when the transcript has no judge finding at all:
--   otherwise the latch cannot be told apart from the run that left them.
--   Counters, tokens, cost, latency and drop counts were never recorded and
--   stay NULL on legacy rows. Judge runs older than the latest per transcript
--   that found nothing left no trace and get no row.
--   transcript_findings: judge_pass_id = the legacy pass of its (transcript,
--   recipe); origin_transcript_id = transcript_id. Findings a requeue already
--   archived (transcript_id NULL) cannot be attributed: their transcript id
--   was never kept, so origin_transcript_id and judge_pass_id stay NULL.
--
-- Locks, and their order. Up takes, before anything else and under a 5s
-- lock_timeout, SHARE ROW EXCLUSIVE on recipes and then ACCESS EXCLUSIVE on
-- transcript_findings — the order a running judge takes them:
-- db.InsertFindings registers its recipe (an INSERT into recipes) before it
-- inserts the findings in the same transaction. Taking them the other way
-- round could deadlock with an eval that holds recipes and waits for
-- transcript_findings. SHARE ROW EXCLUSIVE is what the new recipe_id foreign
-- keys need anyway; it blocks recipe registration but not the key-share row
-- locks other writers' foreign-key checks take. Every later lock (finding_events,
-- fn_calls for the fn_call_id foreign key) is taken after transcript_findings,
-- matching the decide and review writers, which lock findings first. Holding
-- transcript_findings exclusively for the whole transaction also means the
-- backfill reads a frozen table and no finding lands between it and the new
-- triggers. At live scale (~32k findings, ~4.3k transcripts) the table
-- rewrite (the generated column) plus backfill is about a second. Behind a
-- long reader the migration gives up after 5s and the next process start
-- retries it. run_metrics is read but not locked; a latch that commits
-- mid-migration simply gets no legacy row.

-- +goose Up
SET LOCAL lock_timeout = '5s';
LOCK TABLE recipes IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE transcript_findings IN ACCESS EXCLUSIVE MODE;

CREATE TABLE judge_passes (
    id                  BIGSERIAL     PRIMARY KEY,
    transcript_id       UUID          NOT NULL,
    job_id              UUID          NOT NULL,
    file_path           TEXT          NOT NULL,
    recipe_id           TEXT          REFERENCES recipes (recipe_id),
    step_run_id         BIGINT,
    status              TEXT          NOT NULL DEFAULT 'running'
                        CONSTRAINT judge_passes_status_valid
                        CHECK (status IN ('running', 'complete', 'partial', 'failed', 'legacy')),
    chunks_total        INTEGER       CONSTRAINT judge_passes_chunks_total_nonneg  CHECK (chunks_total >= 0),
    chunks_judged       INTEGER       CONSTRAINT judge_passes_chunks_judged_nonneg CHECK (chunks_judged >= 0),
    chunks_failed       INTEGER       CONSTRAINT judge_passes_chunks_failed_nonneg CHECK (chunks_failed >= 0),
    chunks_cached       INTEGER       CONSTRAINT judge_passes_chunks_cached_nonneg CHECK (chunks_cached >= 0),
    findings_kept       INTEGER       CONSTRAINT judge_passes_findings_kept_nonneg CHECK (findings_kept >= 0),
    dropped             JSONB         CONSTRAINT judge_passes_dropped_object
                                      CHECK (dropped IS NULL OR jsonb_typeof(dropped) = 'object'),
    input_tokens        BIGINT,
    output_tokens       BIGINT,
    cost_usd            NUMERIC(14,8) CONSTRAINT judge_passes_cost_nonneg CHECK (cost_usd >= 0),
    latency_ms_p50      INTEGER,
    latency_ms_p95      INTEGER,
    book_context_sha256 TEXT,
    started_at          TIMESTAMPTZ   NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ,
    error               TEXT,
    CONSTRAINT judge_passes_finished_iff_closed CHECK ((status = 'running') = (finished_at IS NULL))
);

-- A transcript's passes, and "has recipe R judged transcript T".
CREATE INDEX judge_passes_transcript_recipe_idx ON judge_passes (transcript_id, recipe_id);
CREATE INDEX judge_passes_complete_idx ON judge_passes (transcript_id, recipe_id)
    WHERE status = 'complete';
-- What did recipe R judge, and when (coverage, cost over time).
CREATE INDEX judge_passes_recipe_idx ON judge_passes (recipe_id, started_at);

CREATE TABLE transcript_archive (
    id                 BIGSERIAL   PRIMARY KEY,
    transcript_id      UUID        NOT NULL,
    job_id             UUID        NOT NULL,
    file_path          TEXT        NOT NULL,
    checksum           TEXT        NOT NULL,
    language           TEXT        NOT NULL,
    duration_seconds   FLOAT8      NOT NULL,
    speaker_count      INTEGER,
    segments           JSONB       NOT NULL,
    raw_text           TEXT        NOT NULL,
    model_name         TEXT        NOT NULL,
    recipe_id          TEXT        REFERENCES recipes (recipe_id),
    embedded_asin      TEXT,
    asr_model_sha256   TEXT,
    asr_runner_version TEXT,
    asr_params         JSONB,
    run_metrics        JSONB,
    created_at         TIMESTAMPTZ NOT NULL,
    archived_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_reason    TEXT        NOT NULL,
    archived_by        TEXT
);

CREATE INDEX transcript_archive_transcript_idx ON transcript_archive (transcript_id);
CREATE INDEX transcript_archive_file_path_idx ON transcript_archive (file_path, archived_at);

ALTER TABLE transcript_findings
    ADD COLUMN judge_pass_id        BIGINT REFERENCES judge_passes (id) ON DELETE SET NULL,
    ADD COLUMN fn_call_id           BIGINT REFERENCES fn_calls (id),
    ADD COLUMN origin_transcript_id UUID,
    ADD COLUMN correction_key       TEXT GENERATED ALWAYS AS (
        CASE WHEN chunk_text_sha256 IS NULL THEN NULL
             ELSE md5(chunk_text_sha256 || E'\x1f'
                      || lower(btrim(regexp_replace(original_text,
                               E'[[:space:]\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff]+', ' ', 'g'))
                               COLLATE "und-x-icu")
                      || E'\x1f'
                      || lower(btrim(regexp_replace(COALESCE(suggested_correction, ''),
                               E'[[:space:]\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff]+', ' ', 'g'))
                               COLLATE "und-x-icu"))
        END) STORED,
    ADD COLUMN duplicate_of         UUID REFERENCES transcript_findings (id) ON DELETE SET NULL;

-- +goose StatementBegin
CREATE FUNCTION pg_temp.set_patch_states(add_state TEXT, drop_state TEXT) RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
    states TEXT[];
BEGIN
    SELECT array_agg(m.v[1] ORDER BY m.n)
      INTO states
      FROM pg_constraint c,
           regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''', 'g')
             WITH ORDINALITY AS m(v, n)
     WHERE c.conrelid = 'transcript_findings'::regclass
       AND c.conname = 'transcript_findings_patch_state_valid';
    IF states IS NULL THEN
        RAISE EXCEPTION 'transcript_findings_patch_state_valid not found or has no values';
    END IF;
    states := array_remove(states, drop_state);
    IF add_state IS NOT NULL AND NOT add_state = ANY (states) THEN
        states := states || add_state;
    END IF;
    ALTER TABLE transcript_findings DROP CONSTRAINT transcript_findings_patch_state_valid;
    EXECUTE format(
        'ALTER TABLE transcript_findings ADD CONSTRAINT transcript_findings_patch_state_valid '
        'CHECK (patch_state IN (%s))',
        (SELECT string_agg(quote_literal(s), ', ' ORDER BY i)
           FROM unnest(states) WITH ORDINALITY AS u(s, i)));
END $$;
-- +goose StatementEnd

SELECT pg_temp.set_patch_states('duplicate', NULL);
DROP FUNCTION pg_temp.set_patch_states(TEXT, TEXT);

ALTER TABLE transcript_findings
    ADD CONSTRAINT transcript_findings_duplicate_of_state
    CHECK (duplicate_of IS NULL OR patch_state IN ('duplicate', 'superseded'));

-- Backfill: legacy passes from the findings they left.
INSERT INTO judge_passes (transcript_id, job_id, file_path, recipe_id, status,
                          findings_kept, started_at, finished_at)
SELECT f.transcript_id, t.job_id, t.file_path, f.recipe_id, 'legacy',
       count(*), min(f.created_at), max(f.created_at)
  FROM transcript_findings f
  JOIN transcripts t ON t.id = f.transcript_id
 WHERE f.origin = 'judge'
 GROUP BY f.transcript_id, t.job_id, t.file_path, f.recipe_id
 ORDER BY min(f.created_at), f.transcript_id;

-- Backfill: the latest judge run of each latched transcript, when it left no
-- finding: under its resolved recipe if that has no pass yet, or with a NULL
-- recipe when unresolved and the transcript has no judge finding at all.
INSERT INTO judge_passes (transcript_id, job_id, file_path, recipe_id, status,
                          chunks_total, findings_kept, started_at, finished_at)
SELECT t.id, t.job_id, t.file_path, r.recipe_id, 'legacy',
       m.eval_chunks, 0, COALESCE(m.eval_started_at, m.eval_finished_at), m.eval_finished_at
  FROM transcripts t
  JOIN run_metrics m ON m.job_id = t.job_id
  LEFT JOIN LATERAL (
        SELECT CASE WHEN count(DISTINCT p.recipe_id) = 1 THEN min(p.recipe_id) END AS recipe_id
          FROM recipes p
         WHERE p.step = 'propose'
           AND m.eval_model IN (p.model_alias, p.model_resolved)) r ON true
 WHERE m.eval_finished_at IS NOT NULL
   AND CASE WHEN r.recipe_id IS NOT NULL
            THEN NOT EXISTS (SELECT 1 FROM judge_passes jp
                              WHERE jp.transcript_id = t.id AND jp.recipe_id = r.recipe_id)
            ELSE NOT EXISTS (SELECT 1 FROM transcript_findings f
                              WHERE f.transcript_id = t.id AND f.origin = 'judge')
       END
 ORDER BY m.eval_finished_at, t.id;

UPDATE transcript_findings f
   SET origin_transcript_id = f.transcript_id,
       judge_pass_id = CASE WHEN f.origin = 'judge' THEN
           (SELECT p.id FROM judge_passes p
             WHERE p.transcript_id = f.transcript_id
               AND p.recipe_id IS NOT DISTINCT FROM f.recipe_id
               AND p.status = 'legacy'
               AND p.findings_kept > 0)
       END
 WHERE f.transcript_id IS NOT NULL;

CREATE INDEX transcript_findings_judge_pass_idx ON transcript_findings (judge_pass_id);
CREATE INDEX transcript_findings_origin_transcript_idx ON transcript_findings (origin_transcript_id);
CREATE INDEX transcript_findings_correction_key_idx ON transcript_findings (correction_key);
CREATE INDEX transcript_findings_duplicate_of_idx ON transcript_findings (duplicate_of)
    WHERE duplicate_of IS NOT NULL;

-- origin_transcript_id: filled on INSERT, frozen once set.
-- +goose StatementBegin
CREATE FUNCTION transcript_findings_keep_origin() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.origin_transcript_id := COALESCE(NEW.origin_transcript_id, NEW.transcript_id);
    ELSIF OLD.origin_transcript_id IS NOT NULL
          AND NEW.origin_transcript_id IS DISTINCT FROM OLD.origin_transcript_id THEN
        RAISE EXCEPTION 'transcript_findings.origin_transcript_id is immutable once set (finding %)', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER transcript_findings_keep_origin
    BEFORE INSERT OR UPDATE OF origin_transcript_id ON transcript_findings
    FOR EACH ROW EXECUTE FUNCTION transcript_findings_keep_origin();

ALTER TABLE finding_events ADD COLUMN anchor_offset INTEGER;

-- The 00008 body plus reason from earmark.reason.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION finding_events_record_transition() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    stamped BOOLEAN := TG_OP = 'INSERT'
        OR NEW.decided_at IS DISTINCT FROM OLD.decided_at
        OR NEW.decided_by IS DISTINCT FROM OLD.decided_by;
    setting_actor TEXT := NULLIF(current_setting('earmark.actor', true), '');
    why TEXT := NULLIF(current_setting('earmark.reason', true), '');
    who TEXT;
    recipe TEXT := NULLIF(current_setting('earmark.recipe_id', true), '');
BEGIN
    IF stamped THEN
        who := COALESCE(NEW.decided_by, setting_actor, 'system');
    ELSE
        who := COALESCE(setting_actor, 'system');
    END IF;
    IF recipe IS NULL AND who ~ '^(revert:)?jev:[0-9a-f]{64}$' THEN
        recipe := substring(who FROM '[0-9a-f]{64}$');
    END IF;
    INSERT INTO finding_events (finding_id, transcript_id, kind, from_state, to_state, reason,
                                actor, recipe_id, chunk_text_sha256, issue_type)
    VALUES (NEW.id,
            CASE WHEN TG_OP = 'INSERT' THEN NEW.transcript_id
                 ELSE COALESCE(NEW.transcript_id, OLD.transcript_id) END,
            'transition',
            CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE OLD.patch_state END,
            NEW.patch_state, why, who, recipe, NEW.chunk_text_sha256, NEW.issue_type);
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- A re-anchor moved the finding: keep where it was.
-- +goose StatementBegin
CREATE FUNCTION finding_events_record_reanchor() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    stamped BOOLEAN := NEW.decided_at IS DISTINCT FROM OLD.decided_at
        OR NEW.decided_by IS DISTINCT FROM OLD.decided_by;
    setting_actor TEXT := NULLIF(current_setting('earmark.actor', true), '');
    who TEXT;
    recipe TEXT := NULLIF(current_setting('earmark.recipe_id', true), '');
BEGIN
    IF stamped THEN
        who := COALESCE(NEW.decided_by, setting_actor, 'system');
    ELSE
        who := COALESCE(setting_actor, 'system');
    END IF;
    IF recipe IS NULL AND who ~ '^(revert:)?jev:[0-9a-f]{64}$' THEN
        recipe := substring(who FROM '[0-9a-f]{64}$');
    END IF;
    INSERT INTO finding_events (finding_id, transcript_id, kind, from_state, to_state, reason,
                                actor, recipe_id, chunk_text_sha256, anchor_offset, issue_type)
    VALUES (NEW.id, COALESCE(NEW.transcript_id, OLD.transcript_id), 'transition',
            OLD.patch_state, OLD.patch_state,
            CASE WHEN OLD.anchor_offset IS NULL THEN 'reanchor_placed' ELSE 'reanchor_moved' END,
            who, recipe,
            OLD.chunk_text_sha256, OLD.anchor_offset, NEW.issue_type);
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER transcript_findings_record_reanchor
    AFTER UPDATE OF chunk_text_sha256, anchor_offset ON transcript_findings
    FOR EACH ROW
    WHEN (OLD.chunk_text_sha256 IS DISTINCT FROM NEW.chunk_text_sha256
       OR OLD.anchor_offset IS DISTINCT FROM NEW.anchor_offset)
    EXECUTE FUNCTION finding_events_record_reanchor();

-- stale_work: the propose arm skips duplicates too (00009's view otherwise).
CREATE OR REPLACE VIEW stale_work AS
WITH cur AS (
    SELECT cr.step, r.recipe_id, r.step_version, r.model_alias, r.model_resolved,
           r.model_revision, r.prompt_version, r.prompt_sha256, r.params
      FROM current_recipes cr
      JOIN recipes r ON r.recipe_id = cr.recipe_id
)
SELECT 'asr'::text AS step, 'transcripts'::text AS source_table, t.id AS row_id,
       t.recipe_id, cur.recipe_id AS current_recipe_id
  FROM transcripts t
  JOIN cur ON cur.step = 'asr'
  LEFT JOIN recipes r ON r.recipe_id = t.recipe_id
 WHERE r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params)
UNION ALL
SELECT 'propose', 'transcript_findings', f.id, f.recipe_id, cur.recipe_id
  FROM transcript_findings f
  JOIN cur ON cur.step = 'propose'
  LEFT JOIN recipes r ON r.recipe_id = f.recipe_id
 WHERE f.origin = 'judge'
   AND f.patch_state NOT IN ('superseded', 'duplicate')
   AND (r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params))
UNION ALL
SELECT 'embed', 'transcript_chunks', c.id, c.recipe_id, cur.recipe_id
  FROM transcript_chunks c
  JOIN cur ON cur.step = 'embed'
  LEFT JOIN recipes r ON r.recipe_id = c.recipe_id
 WHERE r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params)
UNION ALL
-- decide: a finding whose latest live (unrevoked) decision was made by a
-- recipe that is not equivalent to the current decide recipe.
SELECT 'decide', 'transcript_findings', d.finding_id, d.recipe_id, cur.recipe_id
  FROM (SELECT DISTINCT ON (e.finding_id) e.finding_id, e.recipe_id
          FROM finding_events e
         WHERE e.kind = 'decision'
           AND NOT EXISTS (SELECT 1 FROM finding_events v
                            WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)
         ORDER BY e.finding_id, e.id DESC) d
  JOIN transcript_findings f ON f.id = d.finding_id
  JOIN cur ON cur.step = 'decide'
  JOIN recipes r ON r.recipe_id = d.recipe_id
 WHERE f.patch_state <> 'superseded'
   AND (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params)
UNION ALL
SELECT 'scan', 'transcript_chunks', c.id, ls.recipe_id, cur.recipe_id
  FROM transcript_chunks c
  JOIN cur ON cur.step = 'scan'
  LEFT JOIN LATERAL (
        SELECT s.recipe_id
          FROM chunk_scan s
         WHERE s.transcript_id = c.transcript_id
           AND s.chunk_index   = c.chunk_index
         ORDER BY s.created_at DESC, s.id DESC
         LIMIT 1) ls ON true
 WHERE NOT EXISTS (
        SELECT 1
          FROM chunk_scan s
          JOIN recipes r ON r.recipe_id = s.recipe_id
         WHERE s.transcript_id = c.transcript_id
           AND s.chunk_index   = c.chunk_index
           AND s.chunk_text_sha256 =
               encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex')
           AND (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
                r.prompt_version, r.prompt_sha256, r.params)
               IS NOT DISTINCT FROM
               (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
                cur.prompt_version, cur.prompt_sha256, cur.params));

-- +goose Down
-- Lossy, like every Down here: judge_passes and transcript_archive are
-- dropped with their rows, and so are the reanchor events' old offsets.
-- 'duplicate' did not exist before, so duplicates return to 'proposed' —
-- exactly what undoing their grouping does — which the transition trigger
-- records like any other move.
SET LOCAL lock_timeout = '5s';
LOCK TABLE recipes IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE transcript_findings IN ACCESS EXCLUSIVE MODE;

-- 00009's stale_work.
CREATE OR REPLACE VIEW stale_work AS
WITH cur AS (
    SELECT cr.step, r.recipe_id, r.step_version, r.model_alias, r.model_resolved,
           r.model_revision, r.prompt_version, r.prompt_sha256, r.params
      FROM current_recipes cr
      JOIN recipes r ON r.recipe_id = cr.recipe_id
)
SELECT 'asr'::text AS step, 'transcripts'::text AS source_table, t.id AS row_id,
       t.recipe_id, cur.recipe_id AS current_recipe_id
  FROM transcripts t
  JOIN cur ON cur.step = 'asr'
  LEFT JOIN recipes r ON r.recipe_id = t.recipe_id
 WHERE r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params)
UNION ALL
SELECT 'propose', 'transcript_findings', f.id, f.recipe_id, cur.recipe_id
  FROM transcript_findings f
  JOIN cur ON cur.step = 'propose'
  LEFT JOIN recipes r ON r.recipe_id = f.recipe_id
 WHERE f.origin = 'judge'
   AND f.patch_state <> 'superseded'
   AND (r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params))
UNION ALL
SELECT 'embed', 'transcript_chunks', c.id, c.recipe_id, cur.recipe_id
  FROM transcript_chunks c
  JOIN cur ON cur.step = 'embed'
  LEFT JOIN recipes r ON r.recipe_id = c.recipe_id
 WHERE r.recipe_id IS NULL
    OR (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params)
UNION ALL
-- decide: a finding whose latest live (unrevoked) decision was made by a
-- recipe that is not equivalent to the current decide recipe.
SELECT 'decide', 'transcript_findings', d.finding_id, d.recipe_id, cur.recipe_id
  FROM (SELECT DISTINCT ON (e.finding_id) e.finding_id, e.recipe_id
          FROM finding_events e
         WHERE e.kind = 'decision'
           AND NOT EXISTS (SELECT 1 FROM finding_events v
                            WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)
         ORDER BY e.finding_id, e.id DESC) d
  JOIN transcript_findings f ON f.id = d.finding_id
  JOIN cur ON cur.step = 'decide'
  JOIN recipes r ON r.recipe_id = d.recipe_id
 WHERE f.patch_state <> 'superseded'
   AND (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
        r.prompt_version, r.prompt_sha256, r.params)
       IS DISTINCT FROM
       (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
        cur.prompt_version, cur.prompt_sha256, cur.params)
UNION ALL
SELECT 'scan', 'transcript_chunks', c.id, ls.recipe_id, cur.recipe_id
  FROM transcript_chunks c
  JOIN cur ON cur.step = 'scan'
  LEFT JOIN LATERAL (
        SELECT s.recipe_id
          FROM chunk_scan s
         WHERE s.transcript_id = c.transcript_id
           AND s.chunk_index   = c.chunk_index
         ORDER BY s.created_at DESC, s.id DESC
         LIMIT 1) ls ON true
 WHERE NOT EXISTS (
        SELECT 1
          FROM chunk_scan s
          JOIN recipes r ON r.recipe_id = s.recipe_id
         WHERE s.transcript_id = c.transcript_id
           AND s.chunk_index   = c.chunk_index
           AND s.chunk_text_sha256 =
               encode(sha256(convert_to(COALESCE(c.source_text, c.text), 'UTF8')), 'hex')
           AND (r.step_version, r.model_alias, r.model_resolved, r.model_revision,
                r.prompt_version, r.prompt_sha256, r.params)
               IS NOT DISTINCT FROM
               (cur.step_version, cur.model_alias, cur.model_resolved, cur.model_revision,
                cur.prompt_version, cur.prompt_sha256, cur.params));

DROP TRIGGER transcript_findings_record_reanchor ON transcript_findings;
DROP FUNCTION finding_events_record_reanchor();
DROP TRIGGER transcript_findings_keep_origin ON transcript_findings;
DROP FUNCTION transcript_findings_keep_origin();

ALTER TABLE transcript_findings DROP CONSTRAINT transcript_findings_duplicate_of_state;

SELECT set_config('earmark.reason', 'migration 13 down: duplicate state removed', true);
UPDATE transcript_findings
   SET patch_state = 'proposed', duplicate_of = NULL
 WHERE patch_state = 'duplicate';
SELECT set_config('earmark.reason', '', true);

-- +goose StatementBegin
CREATE FUNCTION pg_temp.set_patch_states(add_state TEXT, drop_state TEXT) RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
    states TEXT[];
BEGIN
    SELECT array_agg(m.v[1] ORDER BY m.n)
      INTO states
      FROM pg_constraint c,
           regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''', 'g')
             WITH ORDINALITY AS m(v, n)
     WHERE c.conrelid = 'transcript_findings'::regclass
       AND c.conname = 'transcript_findings_patch_state_valid';
    IF states IS NULL THEN
        RAISE EXCEPTION 'transcript_findings_patch_state_valid not found or has no values';
    END IF;
    states := array_remove(states, drop_state);
    IF add_state IS NOT NULL AND NOT add_state = ANY (states) THEN
        states := states || add_state;
    END IF;
    ALTER TABLE transcript_findings DROP CONSTRAINT transcript_findings_patch_state_valid;
    EXECUTE format(
        'ALTER TABLE transcript_findings ADD CONSTRAINT transcript_findings_patch_state_valid '
        'CHECK (patch_state IN (%s))',
        (SELECT string_agg(quote_literal(s), ', ' ORDER BY i)
           FROM unnest(states) WITH ORDINALITY AS u(s, i)));
END $$;
-- +goose StatementEnd

SELECT pg_temp.set_patch_states(NULL, 'duplicate');
DROP FUNCTION pg_temp.set_patch_states(TEXT, TEXT);

ALTER TABLE transcript_findings
    DROP COLUMN duplicate_of,
    DROP COLUMN correction_key,
    DROP COLUMN origin_transcript_id,
    DROP COLUMN fn_call_id,
    DROP COLUMN judge_pass_id;

-- The 00008 body.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION finding_events_record_transition() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    stamped BOOLEAN := TG_OP = 'INSERT'
        OR NEW.decided_at IS DISTINCT FROM OLD.decided_at
        OR NEW.decided_by IS DISTINCT FROM OLD.decided_by;
    setting_actor TEXT := NULLIF(current_setting('earmark.actor', true), '');
    who TEXT;
    recipe TEXT := NULLIF(current_setting('earmark.recipe_id', true), '');
BEGIN
    IF stamped THEN
        who := COALESCE(NEW.decided_by, setting_actor, 'system');
    ELSE
        who := COALESCE(setting_actor, 'system');
    END IF;
    IF recipe IS NULL AND who ~ '^(revert:)?jev:[0-9a-f]{64}$' THEN
        recipe := substring(who FROM '[0-9a-f]{64}$');
    END IF;
    INSERT INTO finding_events (finding_id, transcript_id, kind, from_state, to_state,
                                actor, recipe_id, chunk_text_sha256, issue_type)
    VALUES (NEW.id,
            CASE WHEN TG_OP = 'INSERT' THEN NEW.transcript_id
                 ELSE COALESCE(NEW.transcript_id, OLD.transcript_id) END,
            'transition',
            CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE OLD.patch_state END,
            NEW.patch_state, who, recipe, NEW.chunk_text_sha256, NEW.issue_type);
    RETURN NULL;
END $$;
-- +goose StatementEnd

ALTER TABLE finding_events DROP COLUMN anchor_offset;

DROP TABLE transcript_archive;
DROP TABLE judge_passes;
