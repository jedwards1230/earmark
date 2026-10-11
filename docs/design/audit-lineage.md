# Design: audit lineage for transcripts and findings

Migration `00013_audit_lineage.sql`. Reference: CONTRACT §1.4 "Operator
requeue", §2.15 "Lineage" and "Clearing", §2.17 "Lifecycle" and "Version
history"; schema detail in `docs/DATABASE_SCHEMA.md`.

## Goal

Every transcript carries a full record of every pass a model or an operator
made over it — each ASR pass, each judge pass per recipe, each finding and
every move it made — so that model quality can be compared across recipes and
over time, and nothing is lost when work is redone. Concretely, three things
used to destroy history:

- a **requeue** deleted the transcript row and its `run_metrics` row, so the
  replaced ASR pass was gone;
- the dashboard's **clear findings** `DELETE`d findings, and `finding_events`
  cascaded with them;
- a **re-anchor** rewrote a finding's anchor in place, unlogged.

And two things were never recorded: which judge pass (recipe) proposed a
finding, and whether two recipes proposed the same fix.

## What the schema records

### Judge passes — `judge_passes`

One row per (transcript × judge recipe) pass: status
(`running`/`complete`/`partial`/`failed`/`legacy`), chunk counters
(total/judged/failed/cached), findings kept, drops by reason (jsonb), tokens,
cost, latency p50/p95, the hash of the book context the judge saw, the
`step_runs` row and start/finish/error. It is the per-recipe "judged" record —
"has recipe R judged transcript T" is a lookup on
`(transcript_id, recipe_id) WHERE status = 'complete'` — while
`run_metrics.eval_*` stays the compatibility "latest judge" summary.
`transcript_id` is a plain column, so a pass outlives the requeue that deletes
its transcript. Each finding points at its pass (`judge_pass_id`).

No writer exists yet: the only rows are `legacy` ones the migration
backfilled — one per (transcript, recipe) of judge findings, and one for the
latest judge run of each latched transcript when that run found nothing. A
legacy row's recipe is the findings' `recipe_id`; for the latest run it is
resolved from `run_metrics.eval_model` only when exactly one propose recipe
matches by `model_alias`/`model_resolved`. A resolved recipe gets its
zero-finding row unless the transcript already has a pass for it (so an older
recipe's findings and a newer recipe's empty run both show); an unresolved one
gets a NULL-recipe row only when the transcript has no judge finding at all. Legacy counters, tokens, cost,
latency and drop counts were never recorded and stay NULL; an older judge run
that found nothing, and was then superseded by a later run, left no trace and
has no row.

### Replaced transcripts — `transcript_archive`

A requeue copies each transcript it is about to delete — every transcripts
column (metadata, `raw_text`, `segments`, the asr recipe and runner
provenance), its `created_at`, and the job's whole `run_metrics` row as jsonb
— into `transcript_archive`, in the same transaction, before the delete. The
ASR pass a requeue replaces is therefore kept in full, alongside the findings
that described it.

### Finding lineage — `origin_transcript_id`, `correction_key`, `fn_call_id`

- `origin_transcript_id` is the transcript a finding was proposed against.
  Filled on insert from `transcript_id`, then frozen by a trigger. A requeue's
  `ON DELETE SET NULL` clears only `transcript_id`, so an archived finding still
  names its transcript, and joins to the archived copy. Findings a requeue
  archived before this migration cannot be attributed: their transcript id was
  never kept.
- `correction_key` is a generated md5 over the chunk text hash, the normalized
  original span and the normalized correction, joined by U+001F so the parts
  cannot run into each other. Normalizing collapses every whitespace run
  (tabs, newlines, NBSP and the other Unicode spaces included) to one space,
  trims, and lower-cases under the ICU root collation — the live cluster is
  `LC_CTYPE=C`, where plain `lower()` would leave `ÉMILE` unequal to `émile`.
  Accents are not folded: `é` and `e` are different corrections. Code reads
  the column rather than recomputing the key. It deliberately excludes the
  recipe: two recipes proposing the same fix to the same chunk text share a
  key. That is what turns cross-model agreement — and the link between a new
  proposal and an older, already-decided finding of the same fix — into a
  join. NULL when the finding has no chunk hash (legacy rows), because the text
  alone does not identify a fix.
- `fn_call_id` points at the judge call's `fn_calls` row. Not yet written by
  any path.

### Agreement is not rejection — the `duplicate` state

When findings agree (same fix), only one should be decided and applied; the
others are neither wrong nor rejected. `patch_state = 'duplicate'` with
`duplicate_of` naming the representative records exactly that. A duplicate is
never in the overlay and never replayed. It moves only back to `proposed` —
undoing the representative's grouping, with `duplicate_of` cleared in the same
UPDATE — or to `superseded`, which keeps `duplicate_of` so an archived
duplicate still says what it agreed with (the CHECK admits `duplicate_of` on
those two states only). Both moves are machine moves; no review action applies.
No path groups findings into `duplicate` yet.

### Nothing is deleted — clear-as-supersede

The dashboard's clear moves findings to `superseded` (stamped, attributed to
the caller's actor — `mcp:dashboard` — with reason `cleared`) instead of
deleting them, so decisions and their event history survive. Withdrawing an
accepted or applied correction flags its chunk for rebuild in the same
statement, and a rebuild that read the overlay before the clear cannot clear
that flag afterwards: `ClearEmbeddingStale` takes the ids of the corrections
the rebuild replayed, locks them, and keeps any chunk flagged whose replayed
correction has since left the overlay — exact where a timestamp comparison
cannot be (a clear stamps no `decided_at`, and its statement time can precede
the rebuild's read while its commit follows it). The served and embedded text
therefore match the log. The book scope escapes `_` and `%` in the
directory name. A cleared finding keeps its `transcript_id`
(the transcript still exists), so "live" means `patch_state <> 'superseded'`
for every reader, not a non-NULL transcript: in particular the eval dedupe
ignores superseded rows, and re-running eval after a clear proposes again.

### Every move is in the log — reanchor events and reasons

- A second trigger on `transcript_findings` appends a `transition` with
  `from_state = to_state` and reason `reanchor_moved` (`reanchor_placed` for a
  legacy finding's first anchoring) whenever a finding's
  `chunk_text_sha256` or `anchor_offset` changes, recording the **old** hash
  and offset (`finding_events.anchor_offset`). An UPDATE that also changes the
  state records both events, the reanchor one first.
- Transitions take their `reason` from the transaction-local setting
  `earmark.reason` (`db.SetEventContext`), like `earmark.actor`: `requeue` for a
  requeue's supersede, `cleared` for a clear. A requeue sets both once, before
  it writes anything, from the caller's actor (`db.WithActor`; `system` when
  none), which is also the archive's `archived_by`.
- Duplicates stay out of every live count — the findings summaries and
  `stale_work`'s propose arm skip `duplicate` as well as `superseded` — so one
  fix is never counted twice.

## Migration cost and locks

Up first takes, under a 5 s lock timeout, SHARE ROW EXCLUSIVE on `recipes`
and then ACCESS EXCLUSIVE on `transcript_findings` — the order a running judge
takes them (`db.InsertFindings` registers its recipe before inserting
findings in the same transaction), so the migration cannot deadlock with an
eval in flight; every later lock is taken after `transcript_findings`, as the
decide and review writers do. Holding `transcript_findings` for the whole
transaction means the backfill reads a frozen table and no finding lands
between the backfill and the new triggers. The table is rewritten (the
generated column); at the live scale (~32k findings, ~4.3k transcripts) Up
measures about one second on a local Postgres 16. Behind a long reader the
migration gives up and the next process start retries it. Down drops the new
tables and columns, restores the previous `stale_work`, and returns
duplicates to `proposed` — but it refuses while `transcript_archive` has rows
(the only copy of the replaced transcripts) unless the session sets
`earmark.allow_data_loss = 'on'`.
