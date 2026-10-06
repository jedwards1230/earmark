-- reanchor_survival.sql — server-side mirror of patch.Reanchor's classification,
-- returning ONLY counts (model, outcome, n). Used to measure re-anchor survival
-- on the live library without pulling chunk text out of Postgres, and pinned
-- against the Go matcher by TestIntegrationReanchorSurvivalSQLMatchesGo.
--
-- __FILTER__ is replaced by a predicate on f (e.g. a transcript_id bucket) so
-- every live run stays bounded:
--   SET statement_timeout='60s'; <this file, __FILTER__ := substr(f.transcript_id::text,1,1) = '0'>
--
-- Mirror rules (see patch.Reanchor / patch.WordOccurrences): tier 1 only when
-- the named chunk covers the finding's audio window or is unchanged (hash
-- match); tiers 2-3 only when it changed; case-sensitive; overlapping matches
-- counted via a zero-width lookahead; a word-boundary check only at an edge
-- where the span has a word char; word chars = [[:alnum:]_], which in the C
-- locale is ASCII — identical to Go's on ASCII chunk text.
WITH f AS (
  SELECT f.id, f.model, f.transcript_id, f.chunk_id, f.chunk_index, f.start_sec, f.end_sec,
         f.original_text AS span, f.chunk_text_sha256 AS sha,
         f.anchor_offset AS off, f.anchor_occurrence AS occ,
         f.original_text ~ '^[[:space:]]*$' AS blank,
         '(?=' || CASE WHEN f.original_text ~ '^[[:alnum:]_]' THEN '(?<![[:alnum:]_])' ELSE '' END
               || regexp_replace(f.original_text, '([^[:alnum:][:space:]])', '\\\1', 'g')
               || CASE WHEN f.original_text ~ '[[:alnum:]_]$' THEN '(?![[:alnum:]_])' ELSE '' END
               || ')' AS wpat,
         '(?=' || regexp_replace(f.original_text, '([^[:alnum:][:space:]])', '\\\1', 'g') || ')' AS spat
    FROM transcript_findings f
   WHERE f.patch_state IN ('proposed', 'unanchorable')
     AND __FILTER__
), n AS (
  SELECT f.*, c.id AS n_id, COALESCE(c.source_text, c.text) AS n_text,
         (f.end_sec <= f.start_sec OR (c.start_sec < f.end_sec AND c.end_sec > f.start_sec)) AS n_in_window,
         EXISTS (SELECT 1 FROM transcript_chunks x WHERE x.transcript_id = f.transcript_id) AS has_chunks
    FROM f LEFT JOIN transcript_chunks c
      ON c.transcript_id = f.transcript_id AND c.chunk_index = f.chunk_index
), a AS (
  SELECT n.*,
         (has_chunks AND sha IS NOT NULL AND n_id IS NOT NULL AND n_id = chunk_id
          AND encode(sha256(convert_to(n_text, 'UTF8')), 'hex') = sha
          AND NOT blank
          AND ((off IS NOT NULL AND off >= 0 AND substr(n_text, off + 1, char_length(span)) = span)
               OR (occ IS NOT NULL AND occ >= 0 AND occ < regexp_count(n_text, spat))
               OR regexp_count(n_text, spat) = 1)) AS already,
         (sha IS NOT NULL AND n_text IS NOT NULL
          AND encode(sha256(convert_to(n_text, 'UTF8')), 'hex') = sha) AS unchanged,
         CASE WHEN blank OR n_text IS NULL THEN 0
              WHEN NOT (n_in_window
                        OR (sha IS NOT NULL AND encode(sha256(convert_to(n_text, 'UTF8')), 'hex') = sha)) THEN 0
              ELSE regexp_count(n_text, wpat) END AS m1
    FROM n
), b AS (
  SELECT a.*,
         CASE WHEN has_chunks AND NOT already AND NOT blank AND m1 = 0 AND NOT unchanged THEN
           (SELECT coalesce(sum(regexp_count(COALESCE(x.source_text, x.text), a.wpat)), 0)
              FROM transcript_chunks x
             WHERE x.transcript_id = a.transcript_id
               AND x.chunk_index IS DISTINCT FROM a.chunk_index
               AND x.start_sec < a.end_sec AND x.end_sec > a.start_sec)
         END AS m2
    FROM a
), c AS (
  SELECT b.*,
         CASE WHEN m2 = 0 THEN
           (SELECT coalesce(sum(regexp_count(COALESCE(x.source_text, x.text), b.wpat)), 0)
              FROM transcript_chunks x
             WHERE x.transcript_id = b.transcript_id)
         END AS m3
    FROM b
), k AS (
  SELECT model,
         CASE
           WHEN NOT has_chunks THEN 'pending'
           WHEN already THEN 'already'
           WHEN blank THEN 'none'
           WHEN m1 = 1 THEN 'unique'
           WHEN m1 > 1 THEN 'ambiguous'
           WHEN m2 = 1 THEN 'moved'
           WHEN m2 > 1 THEN 'ambiguous'
           WHEN m3 = 1 THEN 'moved'
           WHEN m3 > 1 THEN 'ambiguous'
           ELSE 'none'
         END AS outcome
    FROM c
)
SELECT model, outcome, count(*) FROM k GROUP BY 1, 2 ORDER BY 1, 2;
