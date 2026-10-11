# Design: audit lineage for transcripts and findings

Migrations `00013_audit_lineage.sql` (the record) and
`00014_finding_labels_scorecard.sql` (model quality over time, below). Reference: CONTRACT §1.4 "Operator
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

Writers (CONTRACT §2.15 "Lineage", "Re-judge"): every path that judges a
whole transcript — `earmark eval --rejudge --write`, `--backfill-* --write`,
the in-pipeline worker — opens a pass as `running` before its first model
call and closes it with its findings and the `run_metrics` eval slice in one
transaction, so a killed run leaves the `running` row behind as evidence and
the re-judge selects the transcript again. Findings are deduplicated only
against the same recipe (or an equivalent one): another recipe proposing the
same fix is stored, which is what records agreement. Book and `--sample` runs
record no pass.

The migration backfilled `legacy` rows — one per (transcript, recipe) of
judge findings, and one for the
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
- `fn_call_id` points at the judge call's `fn_calls` row (fn `judge_chunk`,
  CONTRACT §2.15 "Judge calls"): the paid call that produced the reply, also
  for a finding served from the `fn_calls` cache. Written by every judge path
  since the re-judge change; NULL on older findings.

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

## Model quality over time (migration 14)

The lineage above makes "how did each model do, for us, over time" a query.
Migration 14 adds the one thing the database could not derive — whether a
proposed fix was actually right — and the two views that put it together.
Reference: CONTRACT §2.15 "Labels and scorecards".

### Labels are about a fix to a chunk revision

`finding_labels` is append-only (UPDATE, DELETE and TRUNCATE refused; Down
refuses to drop it while it has rows unless `earmark.allow_data_loss = 'on'`).
A label's `correction_key` is generated by `finding_correction_key()`, which
is 13's column expression verbatim (pinned by a unit test on the SQL text and
an integration test on a C-locale database), so a label made during the
October bake-off — when GPT-6 Luna's proposals were dry runs with no finding
row — scores Luna's findings as soon as a re-judge writes them, as long as the
chunk text has not changed. A changed chunk is a different revision and a
different key: a new proposal on the new text is not labelled by it.

A label does follow the finding it was made about. `finding_label_current`
links a label to a finding by the finding's current key, by any key the
finding had before a re-anchor moved it (the reanchor transitions keep the old
hash, so the old key is recomputed), and by `finding_id` for labels resolved
through the finding — a legacy finding's pinned label survives its first
anchoring.

The import never attaches a label to text it was not made for:

- a finding id pins the revision the finding recorded, and the item's texts
  must match the finding's;
- a given `chunk_text_sha256` must be the current chunk's (whose text must
  contain the span) or one a finding — live or archived — recorded;
- with neither, the findings that proposed the same fix on that chunk decide:
  if they recorded the chunk's current revision, that is the label's; if they
  saw only another revision, the chunk was re-chunked since and the item is
  `chunk_changed`;
- an item no finding proposed is matched to the current chunk only with
  `--match-current-chunk`, and only if the span (and its context snippet, when
  the source has one) is in the chunk's text. Everything else is reported
  unresolved with a reason. Each stored row says how it was resolved
  (`resolved_by`).

Idempotency is the importer's, under a per-(set, labeler) advisory lock: an
item is skipped when the latest stored row of its (set, labeler, target) —
target being the correction key or, keyless, the finding id — has the same
label and reason. Re-importing a file inserts nothing; a change of mind,
including a revert (good → junk → good), inserts a row each time. Items of
one file that label the same fix differently are all refused
(`conflicting_labels`) rather than resolved by order. In force is `human:`
first, then the latest `labelled_at`. The bake-off reports re-labelled qw1's
"unsure" items a day later (qw2, `Date: 2026-10-11`), so importing every file
gives those items their context-backed label whatever the import order.

`partial` (right target, still wrong) counts against precision, as the
bake-off verdicts scored it; `unsure` is excluded. Both scorecards count each
distinct labelled fix once per recipe; the decide scorecard leaves
`duplicate` findings out.

### The bake-off files

| File | Format | Items | Targets |
|---|---|---|---|
| `tmpfiles/judge_items_key.json` + `jlabels.py` | `bakeoff-key` | 192 (G 84 · B 43 · P 14 · U 51) | chunk + span |
| `tmpfiles/bakework/decide_labels.json` | `bakeoff` | 65 (G 35 · B 12 · U 18) | finding |
| `bake/verdict.md` (Appendix C: the two above) | `bakeoff` | 257 | 192 chunk, 65 finding |
| `bake/verdict-judges.md` (Appendix A, 5 arms) | `bakeoff` | 420 (G 172 · B 98 · P 31 · U 119) | chunk + span |
| `bake/verdict-judges-qw2.md` | `bakeoff` | 421 (G 318 · B 50 · P 20 · U 33) | chunk + span + context |
| `bake/verdict-decide.md` | `bakeoff` | 446 (good 261 · junk 153 · unsure 32) | finding |

`verdict.md` repeats the first two files, and `verdict-judges.md` reuses its
192 J-labels verbatim; importing a repeat under the same set and labeler is a
no-op, under another set a second row the per-finding view collapses. The
`bake/*.jsonl` dumps are judge and decide run outputs, not labels.

Most bake-off judge items are fixes no stored finding proposed (the Luna,
Haiku and Qwen arms were dry runs), so they resolve only with
`--match-current-chunk`. Dry-run first and read the `chunk_changed` /
`span_not_in_chunk` / `context_not_in_chunk` counts: the qw2 items carry a
context snippet, which makes the current-chunk match much safer than the qw1
items' bare spans.

Pass each file's own time as `--labelled-at` (its modification time: the
two `tmpfiles` files 2026-10-10 19:3x, `verdict-judges.md` 22:04,
`verdict-decide.md` 22:17, `verdict-judges-qw2.md` 2026-10-11 08:57 EDT).
`decide_labels.json` and `judge_items_key.json` carry no `Date:` and would
otherwise be stamped with the import time — newer than, and so overriding,
the later context-backed labels of `verdict-decide.md` and qw2.

### What a legacy recipe cannot show

Recipes from before judge passes were recorded (gemma, haiku, qwen) have their
findings, issue types, decisions, reverts, agreement where transcripts
overlap, and labels for bake-off items. They never recorded per-call cost or
latency, chunk counts beyond the latest `run_metrics`, drop counts or parse
failures: the scorecard leaves those NULL (`legacy = true`) and `earmark
models report` prints `n/a (legacy)`, never 0. A recipe with recorded and
legacy passes (`mixed = true`) shows those numbers for its recorded passes
only, marked `*`.

### Definitions

- **Latest decision**: per (finding, decide recipe), the newest `decision`
  event without a `revoke` — what that recipe's verdict currently is. The flip
  rate uses each recipe's newest decision whether or not it was revoked (it
  measures the model, not the undo).
- **Agreement**: of a recipe's comparable fixes — those on a transcript a
  recipe of another configuration (not equivalent in the §1.9 sense) also
  judged — the share such a configuration also proposed. A fix nobody else
  looked at is neither agreement nor disagreement. Unattributed findings never
  count as a partner.
- **Revert rate**: findings with a transition to `reverted`, of those with a
  transition to `applied` or `reverted`.
- **Previous decide recipe**: the non-equivalent decide recipe whose first
  decision is the latest before this one's first.
- **Findings per 1k chunks**: (`findings_kept` + `dropped.already_recorded`)
  ÷ `chunks_judged` over recorded passes — what the judge found, including
  fixes an equivalent recipe had already stored.
- **Last activity** (`--since`): propose — the latest finding, judge pass,
  judge call or finding event on its findings; decide — the latest decision
  or call.

### Cost

The views aggregate whole tables. On a synthetic dataset of the live scale
(33k findings, 4.3k transcripts, 70k finding events incl. 3k reanchor moves,
135k `fn_calls`, 15.6k judge passes, 1.5k labels; local Postgres 16), with
JIT off: `recipe_scorecard` ~0.28 s, `decide_scorecard` ~0.29–0.33 s,
`finding_label_current` ~0.03 s; a full `earmark models report` 0.54 s wall.
With JIT on, `recipe_scorecard` takes ~2.0 s, ~1.6 s of it LLVM compilation:
the CTE estimates push the plan cost over `jit_above_cost`, so readers run
the views under `SET LOCAL jit = off`.

Not yet surfaced: the Models page "Track record" table and the per-book audit
view read these views in a later change.
