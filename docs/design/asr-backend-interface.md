# Design: Swappable Multi-Backend ASR Interface

> **Status: DESIGN / PROPOSAL.** No application code is changed by this document.
> It proposes amendments to `docs/CONTRACT.md`, additive DB migrations, config
> schema evolution, and UI/observability changes, sequenced into independently
> shippable phases. Anything that touches a CONTRACT-governed value (§4 Change
> Control) is called out and must land in `CONTRACT.md` before the implementing
> code.

## 1. Background & current state (what exists today)

earmark does **not** transcribe. It is the in-cluster Go service that:

- **monitor** walks `BOOKS_DIR`, enqueues `transcription_jobs` (dedup by SHA-256),
  and writes `book_metadata` (incl. `bias_terms` derived from metadata).
- **worker** polls completed `transcripts`, chunks them, embeds via Ollama
  (`nomic-embed-text`, 768-dim), writes `transcript_chunks` (pgvector + trigram).
- **mcp** serves the MCP tools + the htmx dashboard + the `/api/v1` control API.

Transcription is performed by an **external Python ASR runner** — today exactly
one: NVIDIA NeMo `parakeet-tdt-0.6b-v3` on desktop-1 (RTX 5090), running as a
native host service. It claims jobs (`FOR UPDATE SKIP LOCKED`), heartbeats,
writes `transcripts` + its slice of `run_metrics`, and honors the `runner_control`
pause/bounded-run gate and the `book_metadata.bias_terms` for NeMo word-boosting.

The earmark↔runner relationship is governed by `docs/CONTRACT.md` (authoritative).

### What was just added (the seam this design builds on)

Two recent PRs (#42, #43) added an **observability-only** server view:

- `config.ASRServer` + `ASR_SERVERS` env JSON: a static registry of declared
  servers (`name`, `host`, `model`, `role`, `match`, `gpuArbiterUrl`). It does
  **not** route work — the runner still claims jobs itself.
- `internal/mcp/servers.go`: the `/servers` dashboard page. It **merges** the
  configured `ASR_SERVERS` list with *observed* activity (`db.ServerObservation`:
  live `claimed_by` runners + per-host `run_metrics` aggregates) and an optional
  `gpu-arbiter` readiness probe, into a status + models/modes view.
- `internal/mcp/gpuprobe.go`: polls `gpu-arbiter`'s `/status` for live GPU
  readiness (ready / busy-gaming / offline).
- `db.GetServerObservation`: the two-source observation (claims vs `run_metrics`).
- `/api/v1/status` now carries a `servers[]` array (`apiServer`).

**Crucial honesty constraint already baked in:** there is **no per-runner
registry/heartbeat table**. "Server" liveness is *inferred* from job claims and
historical `run_metrics.runner_host`, attributed to a configured server by a
**substring token match** (`ASRServer.MatchToken()` against `claimed_by` /
`runner_host`). This design must respect that seam: it is observability, and the
runner remains the autonomous claimant until a later, explicitly-deferred routing
phase.

### The data model today (relevant columns)

`run_metrics` (one row/job, three independent UPSERT writers, all columns nullable):
the runner writes `asr_model`, `compute_type`, `runner_host`, plus audio probe and
counts. There is **no** column for *runtime/family*, *capabilities requested*, or
*capabilities applied*. `transcripts.model_name` records the model string only.

## 2. The problem to solve

We want earmark to support **multiple, swappable ASR backends across multiple
model families and hosts**, to A/B test them and run a primary + fallback.

A "backend" varies along several axes that today are flattened into a single
`asr_model` string:

| Axis | Examples |
|------|----------|
| **Model family** | NeMo Parakeet, NeMo Canary, IBM Granite Speech, OpenAI Whisper / faster-whisper / WhisperX |
| **Runtime** | NeMo-CUDA, parakeet-mlx (Apple Silicon), parakeet.cpp / sherpa-onnx (CPU), whisper.cpp SYCL (Intel iGPU), OpenVINO |
| **Compute type** | bfloat16, float16, int8, etc. (already recorded) |
| **Host** | desktop-1/RTX 5090, a Mac, an Intel box, … (already recorded as `runner_host`) |

…and along **capabilities that may or may not be present**:

| Capability | Where it works / breaks |
|------------|-------------------------|
| **word-level timestamps** | present on most; absent/coarse on some CPU runtimes |
| **context biasing / word boosting** | works on NeMo Canary (AED) + NeMo generally; **broken on Parakeet-TDT** (timestamps corrupt — see auto-memory `parakeet-tdt-context-biasing`); **absent on Whisper** |
| **speaker diarization** | NeMo Sortformer (`ASR_DIARIZE`); absent on most CPU runtimes |
| **language set** | model-dependent (Parakeet v3 multilingual; Granite/Canary differ; Whisper ~broad) |

### The load-bearing requirement: capabilities are *conditional/optional*

A job submitted with a **context-biasing word list** (or diarization request)
must **still complete** on a backend that can't honor it — it must **gracefully
degrade**, and the **result must record what was actually applied** vs what was
requested. This is the difference between "this backend ignored my bias list" and
"this backend doesn't support bias lists" — both must be legible after the fact so
A/B comparisons are honest.

## 3. Core design: the capability model

### 3.1 Capability vocabulary (the shared enum)

A small, closed set of capability keys, defined once in `CONTRACT.md` and mirrored
as Go constants (`internal/asr` — a new leaf package with no DB/HTTP deps so it is
trivially testable and importable by both config and mcp):

```
word_timestamps        — per-word start/end timestamps in segments[].words
context_biasing        — word-boosting / context biasing from book_metadata.bias_terms
diarization            — speaker labels (segments[].speaker, words[].speaker)
confidence_scores      — per-word confidence (words[].score)
language_detection     — auto language id vs fixed language
```

Languages are modeled separately (a string set / ISO-639-1 list), not as a boolean
capability, since "which languages" is the useful fact.

Each capability has three observable states in the data model:

- **`supported`** — backend advertises it (static, from config/contract).
- **`requested`** — the job asked for it (from `book_metadata` / job requirements).
- **`applied`** — the runner reports it actually did it for *this run* (from
  `run_metrics`). `requested && supported && !applied` is a real, recordable
  outcome (e.g. Parakeet-TDT: bias requested, family nominally supports boosting,
  but TDT timestamps break so the runner **declines** and records `applied=false`
  with a reason).

This three-state split is the heart of the design and what makes A/B honest.

### 3.2 Backend descriptor (static, declarative)

`ASRServer` gains a **backend descriptor**: family, runtime, and a capability
declaration. This is the *expected* shape, supplied by config (`ASR_SERVERS`), and
is what the dashboard shows before any run has reported. It never blocks the
pipeline; a missing/partial descriptor just renders as "unknown".

```jsonc
// one ASR_SERVERS entry, extended (all new fields optional, back-compat)
{
  "name": "gpu-1",
  "host": "gpu-1",
  "role": "primary",
  "match": "gpu-1",
  "gpuArbiterUrl": "http://gpu-1:48750/status",

  // NEW — backend descriptor
  "family":  "nemo-parakeet",          // free-form-but-conventional family id
  "runtime": "nemo-cuda",              // free-form-but-conventional runtime id
  "model":   "nvidia/parakeet-tdt-0.6b-v3",
  "capabilities": {                     // declared/expected capabilities
    "word_timestamps": true,
    "context_biasing": false,           // TDT timestamps break under boosting
    "diarization":     false,
    "confidence_scores": false
  },
  "languages": ["en","es","fr","de","..."]   // optional; omit = unknown
}
```

`family` and `runtime` are **strings, not enums** at the config boundary — earmark
must stay generic and not gatekeep which families exist (a new runtime shouldn't
require an earmark release). `internal/asr` carries a *recommended* set of
canonical ids + a `KnownFamily(string) bool` helper purely for nice labels/icons;
unknown values render verbatim. **Capabilities keys, however, ARE the closed enum**
from §3.1 — an unknown capability key is ignored with a warning (forward-compat).

### 3.3 Why config carries the descriptor (and the runner re-asserts it)

Two sources of truth, reconciled exactly like the existing servers.go merge:

- **Config descriptor** = the operator's *declaration* ("I expect gpu-1 to run
  parakeet, no biasing"). Available before any job runs — drives the fallback /
  routing decisions in the deferred phase and the "expected" dashboard labels.
- **Runner-reported (run_metrics)** = ground truth of what *actually ran* and what
  it *actually applied*. Observed wins over configured in the UI (same precedence
  servers.go already uses for `model`).

When they disagree (config says "supports diarization", runner reports
`diarization_applied=false` on a diarization-requested job) the UI surfaces the
divergence rather than hiding it — that's a useful A/B / misconfig signal.

## 4. Config schema evolution

### 4.1 `ASR_SERVERS` extension (additive, back-compat)

Add the `family`, `runtime`, `capabilities`, `languages` fields to
`config.ASRServer` (above). All optional; an old `ASR_SERVERS` value keeps working
byte-for-byte. `capabilities` is a `map[string]bool` decoded against the §3.1 enum
(unknown keys → warn+drop). Parsing stays **best-effort** (a bad value warns and
degrades, never blocks startup) — consistent with the existing loader.

```go
type Capabilities map[string]bool   // keys constrained to the asr.* enum

type ASRServer struct {
    // ... existing fields ...
    Family       string       `json:"family,omitempty"`
    Runtime      string       `json:"runtime,omitempty"`
    Capabilities Capabilities `json:"capabilities,omitempty"`
    Languages    []string     `json:"languages,omitempty"`
}
```

### 4.2 Shareable-repo constraint (NO homelab specifics)

This is a hard rule and the current code already follows it (`gpu-1`, `example.com`
placeholders). The design preserves it:

- **Source / defaults / `.env.example` / `values.yaml` / `CONTRACT.md`**: only
  generic placeholders (`gpu-1`, `mac-1`, `cpu-1`, `nvidia/parakeet-...`,
  `http://gpu-1:48750/status`). No real IPs/hostnames.
- **Real values** (actual hosts, gpu-arbiter URLs, which box runs which family)
  live **only** in the homelab-k8s helmfile (`repos/homelab-k8s/apps/earmark/`),
  injected as the `ASR_SERVERS` JSON via Helm `config.asrServers`.
- Demo/example data (`internal/mcp/demo.go`) uses generic placeholder servers.

### 4.3 Helm `values.yaml` (spec only, not implemented here)

`config.asrServers` already exists (`[]`, serialized to `ASR_SERVERS` by
`_helpers.tpl`). The structured fields ride along for free since it's opaque JSON
→ env. **Spec the schema in the chart's values comment** (and, optionally,
`values.schema.json`) so operators get validation:

```yaml
config:
  asrServers: []
  # Each entry: { name, host?, role?, match?, gpuArbiterUrl?,
  #               family?, runtime?, model?, capabilities?{...bool}, languages?[] }
```

No template logic change is required (it's pass-through JSON). Document the new
fields in the values comment. Real per-host values stay in homelab-k8s.

## 5. Data model

### 5.1 What must be recorded to compare backends

Per run we need: **which backend** (family + runtime + model + compute_type +
host — host/model/compute already exist), and the **requested-vs-applied
capability deltas**, plus the timing/throughput already captured. Quality signals
where cheaply available (mean word confidence).

### 5.2 Migration — extend `run_metrics` (additive, nullable)

`run_metrics` is the right home: it's already the per-run, best-effort,
column-owned telemetry table, additive by contract, with `ON DELETE CASCADE`.
Add runner-owned columns (the Python runner's UPSERT slice grows):

```sql
ALTER TABLE run_metrics
  ADD COLUMN IF NOT EXISTS asr_family        TEXT,    -- "nemo-parakeet", "whisper", ...
  ADD COLUMN IF NOT EXISTS asr_runtime       TEXT,    -- "nemo-cuda", "whisper.cpp-sycl", ...
  -- capabilities the runner actually applied this run (closed enum keys → bool)
  ADD COLUMN IF NOT EXISTS caps_applied      JSONB,   -- {"word_timestamps":true,"context_biasing":false,...}
  -- capabilities the job requested (snapshot at claim time, for honest deltas)
  ADD COLUMN IF NOT EXISTS caps_requested    JSONB,   -- {"context_biasing":true,"diarization":false,...}
  -- why a requested cap was NOT applied (key → short reason), for the dashboard
  ADD COLUMN IF NOT EXISTS caps_skipped_reason JSONB, -- {"context_biasing":"parakeet-tdt timestamps break under boosting"}
  -- optional quality signal: mean per-word confidence when the model emits scores
  ADD COLUMN IF NOT EXISTS mean_word_confidence FLOAT8;
```

Rationale for **JSONB capability maps** over per-cap boolean columns: the
capability set is small but **open to growth** (§3.1 may gain keys); a JSONB map
adds keys without a migration, and Postgres JSON operators are enough for the
aggregate queries the dashboard needs (`caps_applied->>'context_biasing'`). Per-cap
columns would ossify the enum into the schema and force a migration per new
capability. (Counter-argument noted in §10 open questions.)

`asr_family` / `asr_runtime` are **flat TEXT columns** (not JSON) because they're
single-valued, want indexing/`GROUP BY` for the A/B rollups, and mirror the
existing `asr_model` / `compute_type` treatment.

### 5.3 `transcripts` — leave as-is

`transcripts.model_name` stays the authoritative model string. Family/runtime/caps
are **telemetry**, not transcript content, so they live in `run_metrics`. (If a
future need arises to query transcripts by family for re-embedding decisions, add a
denormalized `transcripts.asr_family` then — out of scope now.) Calling this out
explicitly so the implementer doesn't scatter the data.

### 5.4 Who writes what (column ownership, extends CONTRACT §1.5)

| Writer | When | New columns |
|--------|------|-------------|
| **Go monitor** | at enqueue | (writes `caps_requested` — see §5.5) |
| **Python ASR runner** | after transcribing | `asr_family`, `asr_runtime`, `caps_applied`, `caps_skipped_reason`, `mean_word_confidence` |

The runner already owns `asr_model`/`compute_type`/`runner_host`; the new
runner-owned columns join that same UPSERT slice (no clobber risk — different
columns).

### 5.5 Where does `caps_requested` come from? (the routing seam, minimal form)

Today a "request" is implicit: `book_metadata.bias_terms` being non-empty *is* a
context-biasing request; `ASR_DIARIZE` *is* a diarization request (runner-side env,
global). To record requested-vs-applied **honestly**, the request must be
**legible to earmark at enqueue time**. Two options, sequenced:

- **Phase 1 (cheap, no routing):** the **runner** writes both `caps_requested` and
  `caps_applied` — it already knows it saw `bias_terms` and chose to skip boosting.
  earmark just stores and displays. Zero new request channel; fully back-compat.
- **Phase 3 (routing):** earmark writes `caps_requested` at enqueue from a
  per-job/per-book requirements record (§7), and the matcher uses it. Deferred.

Phase 1 is enough for A/B. We adopt it first.

## 6. Contract amendments (`docs/CONTRACT.md`)

All of the following must land in `CONTRACT.md` **before** the implementing code
(§4 Change Control covers `run_metrics` column adds and `ASR_SERVERS` shape).

### 6.1 New §1.5 columns (additive, nullable — back-compat)

Document `asr_family`, `asr_runtime`, `caps_applied`, `caps_requested`,
`caps_skipped_reason`, `mean_word_confidence` with the column-ownership table
extension (§5.4). Emphasize **all nullable, best-effort** — the existing single
NeMo runner that doesn't write them keeps working (columns stay NULL → UI shows
"unknown", never an error). **This is the back-compat guarantee.**

### 6.2 New §2.13 "ASR Backend Capability Vocabulary"

Define the closed capability enum (§3.1) and the `caps_applied` / `caps_requested`
/ `caps_skipped_reason` JSON shapes (string keys from the enum → bool / reason
string). State that unknown keys are ignored. Define the recommended-but-open
`family` / `runtime` id conventions (a table of suggested ids per the §2 axes) so
multiple runners converge on the same strings without earmark gatekeeping them.

### 6.3 Runner env additions (§2.4) — generic, multi-runtime

The current §2.4 documents only the NeMo runner. Generalize to "Python ASR runner
(any backend)" and add advertised-capability env so a runner declares what it can
do (used for the deferred routing match and for `caps_applied` defaults):

| Variable | Required | Notes |
|----------|----------|-------|
| `ASR_FAMILY` | no | e.g. `nemo-parakeet`, `whisper`. Runner writes to `run_metrics.asr_family`. |
| `ASR_RUNTIME` | no | e.g. `nemo-cuda`, `whisper.cpp-sycl`. → `run_metrics.asr_runtime`. |
| `ASR_CAPABILITIES` | no | JSON map of advertised caps (the runner's static truth). Defaults per known family. |

Keep `ASR_MODEL`, `ASR_DIARIZE`, `ASR_COMPUTE_TYPE`, `ASR_CHUNK_THRESHOLD_SECONDS`
unchanged. **Breaking changes: none** for the existing runner — every new var is
optional and defaults preserve current behavior.

### 6.4 Runner result contract — report applied capabilities

Amend §1.5 / the runner's "mark done" obligations: in the same best-effort
`run_metrics` UPSERT it already does, the runner SHOULD populate `caps_applied`
(what it did) and, when it *declined* a requested capability, `caps_skipped_reason`.
**SHOULD, not MUST** — a runner that doesn't is still contract-compliant (columns
NULL). This is the explicit backward-compat carve-out.

### 6.5 Explicitly-called-out breaking changes

**None in Phases 1–2.** All amendments are additive + nullable + SHOULD. The only
place a breaking change *could* arise is Phase 3 routing if we ever gate claims on
capabilities (a runner that ignores requirements could starve) — that is deferred
and will get its own contract version bump + sign-off.

## 7. Routing hooks (design only — deferred Phase 3)

Earlier work **deliberately deferred** actual job-type / primary-fallback routing
because it needs runner-side changes (the runner claims autonomously via
`FOR UPDATE SKIP LOCKED`; earmark cannot today *assign* a job to a server). This
section designs the **earmark-side seam** only and marks it clearly later-phase.

### 7.1 The seam: job requirements + capability matching

- **Express requirements:** a nullable `transcription_jobs.requirements JSONB`
  (or a `book_metadata`-level default) carrying requested caps + optional
  `preferred_family` / language. Defaults to NULL = "no special requirements"
  (back-compat). earmark snapshots it into `run_metrics.caps_requested`.
- **Match:** a pure `internal/asr.Match(requirements, []ASRServer) → ranked []`
  function (testable, no DB) that scores configured servers by
  capability-satisfaction + role (primary first) + live readiness (the
  gpu-arbiter probe + claim freshness already available). This is the *decision*;
  it does not *enforce*.
- **Enforce (the hard part, needs runner cooperation):** the runner's claim query
  would need a `WHERE` predicate matching its advertised capabilities (e.g. a
  `transcription_jobs.required_family` filter), or earmark pre-assigns by writing a
  `target_match` token the runner filters on. **Either is a CONTRACT change to the
  claim semantics (§1.3) and a runner change** — hence deferred, with its own
  contract bump and sign-off.

### 7.2 What we build now vs later

- **Now (Phase 1–2):** the *observability* of requested-vs-applied, the
  `internal/asr` capability package, and the config descriptor. These make the
  routing decision *possible to reason about* without enforcing it.
- **Later (Phase 3):** `requirements` column, `asr.Match`, and the claim-filter
  contract change. Do **not** over-build the matcher now — ship the seam (the
  package + the descriptor) and stop.

## 8. UI / observability

The `/servers` page + `/api/v1/status` `servers[]` already merge configured vs
observed. Extend legibly:

### 8.1 Servers page (`internal/mcp/servers.go`)

- **Models & modes table** gains **Family** and **Runtime** columns (observed wins
  over configured, same precedence as `model`). Render "expected" tag when
  config-only, verbatim when unknown.
- **Capabilities row/badges** per server: a compact badge strip from the
  *observed* `caps_applied` (fallback to configured `capabilities`): e.g.
  `words` `bias✗` `diar✗` with a tooltip carrying `caps_skipped_reason`
  (`"context_biasing: parakeet-tdt timestamps break under boosting"`). The ✗-with-
  reason is the honest-degradation surface.
- **A/B affordance:** since the table already lists every server with JobsDone /
  Avg proc, add **mean word confidence** and a per-family grouping so two backends
  on the same library are visually comparable side by side. Keep it a table, not a
  chart (matches the dashboard's existing aesthetic; no JS deps beyond htmx).

### 8.2 `/api/v1/status` (`apiServer`)

Add `family`, `runtime`, `capabilities` (the resolved applied-or-declared map),
`capsSkippedReason`, `meanWordConfidence` to `apiServer`. These are the
machine-readable hooks an A/B harness or the deferred fallback automation reads.
All `omitempty`/nullable so old consumers are unaffected.

### 8.3 Demo (`internal/mcp/demo.go`)

Add a `DEMO_SCENARIO=multibackend` fixture: a Parakeet primary (bias declined),
a Whisper fallback (no bias, no diar), and a Canary server (bias applied) so the
capability badges + skipped-reason tooltips render without a DB. Generic
placeholder names only.

## 9. Testing & A/B approach

Match earmark's existing patterns: pure functions get table tests; DB-touching
code uses the (deferred, M-8) testcontainers suite; the dashboard uses the demo
fixtures + render tests (`servers_test.go`, `dashboard_test.go`).

- **`internal/asr` package:** pure, fully unit-tested — capability enum
  parse/validate (unknown keys dropped), `family`/`runtime` label helpers, and
  (Phase 3) `Match` ranking. This is where most new test value lives.
- **`buildServerViews` extension:** extend the existing table tests
  (`servers_test.go`) with cases carrying family/runtime/caps in both the
  configured entry and the observed `HostMetrics`, asserting observed-wins and the
  badge/skipped-reason resolution.
- **`run_metrics` round-trip:** in the testcontainers DB suite, UPSERT the new
  columns from a fake "runner" slice and a fake "monitor" slice, assert no clobber
  (the column-ownership invariant) and JSONB round-trips.
- **A/B method (operational, documented in the runbook, not a unit test):** run
  two backends against the **same book** by:
  1. configure two servers in `ASR_SERVERS` (e.g. parakeet + whisper);
  2. transcribe the book on backend A, snapshot `transcripts` + `run_metrics`;
  3. `earmark requeue "<book>" --yes` to reset to pending, switch the runner host
     to backend B (or run B on a second host), re-transcribe;
  4. compare via a small read-only query / MCP `get_transcript`:
     **timestamp coverage** (fraction of words with non-null start/end),
     **segment/word counts**, **mean_word_confidence**, **wall-clock** (avg proc),
     and a **WER-ish text diff** between the two `raw_text` values (Levenshtein /
     token-overlap — earmark can ship a `earmark compare <bookA-jobid> <bookB-jobid>`
     CLI subcommand as a Phase-2 nicety, or do it externally). earmark stores both
     runs' telemetry; the requeue flow already exists and is the right primitive.

   Note the requeue-overwrites-prior-run caveat: comparing A vs B needs **two
   distinct jobs** (two files, or capture A's metrics before requeueing). Document
   this; a future "keep N transcript versions per job" is explicitly out of scope.

## 10. Tradeoffs & open questions (need a decision)

1. **JSONB capability maps vs per-cap boolean columns.** Design picks JSONB (open
   enum, no migration per cap, good enough for dashboard aggregates). Tradeoff:
   slightly clunkier SQL and no column-level constraints. *Decision needed:* accept
   JSONB, or pay per-cap-column migrations for cleaner queries? **Recommendation:
   JSONB.**

2. **`family`/`runtime` as open strings vs closed enum.** Design picks open strings
   (don't force an earmark release for a new runtime) with a recommended-id table
   in CONTRACT. Tradeoff: typos/divergent ids across runners. Mitigation: the
   `internal/asr` known-id helper + a dashboard "unknown family" hint. *Decision
   needed:* open (recommended) vs closed?

3. **Where `caps_requested` originates.** Phase 1 has the **runner** write both
   requested+applied (cheapest, back-compat). Phase 3 moves "requested" to
   earmark-at-enqueue for routing. *Decision needed:* OK to start runner-authored?
   **Recommendation: yes**, it unblocks A/B with zero new request channel.

4. **A/B versioning.** earmark keeps one transcript per job (requeue overwrites).
   True side-by-side A/B needs two jobs or external capture. *Decision needed:* is
   the two-jobs/requeue workflow acceptable, or do we want first-class
   "transcript versions" (much larger change — schema + dedup + UI)? **Recommendation:
   two-jobs workflow now; versions are a separate epic if ever needed.**

5. **Does diarization request stay global (`ASR_DIARIZE`) or become per-job?**
   Per-job belongs to the Phase-3 `requirements` seam. *Decision needed:* leave
   `ASR_DIARIZE` global for now (recommended) — per-job is Phase 3.

6. **Quality signal scope.** `mean_word_confidence` is cheap and family-dependent
   (NeMo TDT often omits scores → NULL). Is that enough quality signal for v1, or
   do we want a reference-transcript WER harness (needs ground-truth texts we don't
   have for audiobooks)? **Recommendation: mean confidence + text-diff between two
   backends; no absolute WER (no ground truth).**

## 11. Phased plan (each phase independently shippable)

> Repo uses **CalVer** (operations-style) — but earmark is a *service* repo with a
> semver-style release pipeline (`semver:` labels per `release-conventions.md`).
> The bumps below are the **earmark `semver:` label** for each PR. earmark's
> contract changes are additive, so no `major` until/unless Phase 3's claim-semantics
> change lands.

### Phase 1 — Capability model + record applied-vs-requested  (`semver:minor`)
The smallest first-useful increment. **Ships A/B legibility with zero routing.**
- CONTRACT: add §1.5 columns (6.1), §2.13 vocabulary (6.2), runner result
  obligations (6.4) — all additive/SHOULD.
- New `internal/asr` package: capability enum + parse/validate + label helpers.
- DB migration: the §5.2 `run_metrics` columns (`ADD COLUMN IF NOT EXISTS`).
- Config: extend `ASRServer` with `family`/`runtime`/`capabilities`/`languages`.
- Runner (homelab-ansible, separate repo, same atomic change set per §4 Change
  Control): write `asr_family`/`asr_runtime`/`caps_applied`/`caps_skipped_reason`/
  `mean_word_confidence`. Existing NeMo runner: declares `context_biasing` declined
  on TDT (the known-broken case), records `applied=false` + reason.
- **Outcome:** you can run parakeet vs whisper and *see* what each applied.

### Phase 2 — UI/observability + A/B affordances  (`semver:minor`)
- Servers page: Family/Runtime columns, capability badges + skipped-reason tooltips,
  mean-confidence column, per-family grouping (§8.1).
- `/api/v1/status` `apiServer`: family/runtime/capabilities/meanWordConfidence (§8.2).
- Demo `multibackend` scenario (§8.3).
- Optional: `earmark compare <jobA> <jobB>` CLI for the text-diff/timestamp-coverage
  A/B readout (§9). If shipped, still `minor`.
- **Outcome:** the dashboard makes the A/B comparison legible at a glance.

### Phase 3 — Routing (DEFERRED; needs runner-side change)  (`semver:major`)
Only if/when we want earmark to *steer* work, not just observe it.
- `transcription_jobs.requirements JSONB` + earmark-authored `caps_requested`.
- `internal/asr.Match` ranking (pure, tested).
- **CONTRACT claim-semantics change (§1.3)** so the runner filters claims by
  advertised capability / target token — **the breaking change**, gets its own
  contract version + explicit sign-off + atomic 3-repo update.
- **Outcome:** primary + capability-aware fallback. Do not start until 1–2 prove
  the capability data is trustworthy in production.

## 12. Summary of files touched (by phase, for the implementer)

Phase 1: `docs/CONTRACT.md`, `internal/asr/*` (new), `internal/db/db.go`
(schema-init ALTERs + runner UPSERT slice + `HostMetrics`/`ServerObservation`
fields), `internal/config/config.go` (`ASRServer` fields), `.env.example`,
`deploy/helm/earmark/values.yaml` (comment/schema), `CLAUDE.md` (package note).
Plus homelab-ansible runner (separate repo) and homelab-k8s `ASR_SERVERS` real
values (separate repo) — atomic per Change Control.

Phase 2: `internal/mcp/servers.go`, `internal/mcp/api.go`, `internal/mcp/demo.go`,
`internal/mcp/layout.html` (badge CSS), tests; optional `cmd/compare` + `main.go`.

Phase 3: `internal/db/db.go` (`requirements` column + claim filter),
`internal/asr` (`Match`), `docs/CONTRACT.md` §1.3, runner repo. Breaking.
