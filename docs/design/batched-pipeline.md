# Batched two-phase pipeline + repositioned eval

**Status:** design (Stage 1 in progress)
**Goal:** decouple ASR from text-processing so each large model loads once per *batch*,
reposition the eval judge to run after transcription / before embedding, and free the
GPU during the analyze phase so a 27–32B judge fits.

## Why

The ASR model (parakeet-tdt-1.1b, ~17 GB cached during inference) and a large eval judge
(27–32B, ~17–20 GB) cannot co-reside on the 32 GB RTX 5090. Today they would interleave —
the ASR runner holds its model continuously while the embed worker + on-demand eval run
alongside — which caps the judge at ~12–14 B. Sequencing the work into phases lets one big
model own the card at a time, loading/unloading once per batch instead of fighting for VRAM.

Eval is also currently an on-demand/sampled pass that reads already-embedded chunks
(CONTRACT §2.15). The desired position is *after transcript, before embedding*, so a
finding is produced from the same chunk text that gets embedded, as part of processing.

## Target flow

```
monitor → transcription_jobs
   │
   ▼  Phase A — TRANSCRIBE BATCH  (ASR resident; judge unloaded)
ASR runner claims N pending jobs (bounded by run_limit) → transcripts
   │
   │  coordinator: gpu-arbiter POST /units/asr-runner/stop   (frees ~17 GB)
   ▼  Phase B — ANALYZE BATCH     (ASR unloaded; judge + nomic resident)
for each new transcript:  chunk → EVAL → embed → insert
   │
   │  coordinator: gpu-arbiter POST /units/asr-runner/start
   ▼  repeat next batch
```

## Decisions (settled 2026-06)

1. **Eval = pipeline stage, bounded by batch.** Eval runs in Phase B for every book in the
   current batch; cost is bounded by batch size (operator-paced). The existing on-demand
   eval (`earmark eval`, `/actions/eval*`) stays for re-evaluating specific books. A gate
   (`EVAL_IN_PIPELINE`, default **off**) keeps today's behavior unchanged until the
   coordinator turns it on per batch.
2. **Scheduler = earmark coordinator.** A new earmark subcommand owns the phase loop and
   calls gpu-arbiter over HTTP for load/unload. earmark already owns the pipeline and the
   `run_limit` gate; orchestration stays in one place. gpu-arbiter needs **no changes** — it
   already exposes `POST /units/{unit}/stop|start` and `/ollama/start|stop` (+ `GET /status`).

## Key implementation constraint

`transcript_chunks.embedding` is `VECTOR(768) NOT NULL` with an HNSW index — chunks cannot
be persisted without a vector. So eval-before-embed is done **in memory**, not via
NULL-embedding rows:

```
chunk(segments) → []Chunk     # chunker, CPU-only; assign client-side UUIDs here
  → []EvalChunk (in memory)   # built from chunks + transcript metadata
  → judge.JudgeChunk → findings (chunk_id = the pre-generated UUID)  [if EVAL_IN_PIPELINE]
  → embed([]Chunk) → vectors
  → InsertChunks (explicit id + embedding)   # atomic, NOT NULL satisfied
```

Pre-generating chunk UUIDs in Go (instead of relying on the column's `gen_random_uuid()`
default) lets findings reference the chunk ids before the rows are inserted. No schema change;
`embedding` stays `NOT NULL`. The "find work" signal (`GetCompletedTranscripts` = transcripts
with no chunks) is unchanged — a transcript is still "done" once its chunks land.

## Stages

### Stage 1 — reposition eval (this PR)
- `internal/worker/processTranscript`: generate chunk UUIDs client-side; build in-memory
  `EvalChunk`s; run the judge **before** embedding when `EVAL_IN_PIPELINE=true`; persist
  findings; then embed + insert as today.
- New `EVAL_IN_PIPELINE` env (default off → behavior identical to today, just with
  client-side ids).
- Eval endpoint resolution reuses `ResolveChatClient` (AI registry `eval` role / `EVAL_CHAT_*`).
- Tests: flag-off path = unchanged insert; flag-on path = findings written before embed,
  chunk_ids consistent between findings and inserted chunks.

### Stage 2 — two-phase batch coordinator
- New `earmark batch --size N [--no-eval]` (or a monitor mode): set `run_limit=N`, wait for
  Phase A to drain, `POST /units/asr-runner/stop`, run Phase B (`EVAL_IN_PIPELINE` on for the
  batch) over the just-transcribed transcripts, `POST /units/asr-runner/start`, loop.
- gpu-arbiter base URL + unit name via config (e.g. `GPU_ARBITER_URL`, `ASR_UNIT`).
- Failure handling: always attempt asr-runner restart (defer); a Phase-B crash must not leave
  the ASR unit stopped.

### Stage 3 — bigger judge
- Once Phase B owns the card, point `aiRoles.eval` at a 27–32B **instruct** model
  (gemma3:27b ~17 GB or qwen2.5:32b ~20 GB — NOT reasoning models, which bury JSON in a
  `reasoning` field). A/B on the same chunks, pick empirically.

## Open questions (revisit at each stage)
- Phase-B concurrency: judge calls are serial per transcript today; batch throughput may want
  bounded concurrency against the judge endpoint.
- Coordinator restart/resume semantics if interrupted mid-batch (Phase A done, Phase B partial).
- Whether the embed worker's continuous loop is paused during coordinator-driven batches to
  avoid double-processing (likely: coordinator owns Phase B, worker loop gated off in batch mode).
</content>
