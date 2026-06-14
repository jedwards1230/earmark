# Design: Read-only LLM eval layer (transcript error detection + confidence)

Status: implemented (foundations) — GitHub issue #49. Depends on #48 (AI
endpoint registry); the chat-endpoint binding is stubbed via standalone env vars
until #48 lands.

## 1. Goal and the immutability asymmetry

A `chat`-type LLM (an OpenAI-compatible `/v1/chat/completions` endpoint, e.g.
vLLM on a GPU host) **reads** transcript text and **records suspected errors as
advisory metadata** — it never edits transcripts.

The whole point is an asymmetry of risk:

- A **wrong flag** costs nothing. It is a row in a side table you triage by
  confidence and ignore if it's noise.
- A **wrong correction** corrupts the corpus. A generative model producing a
  plausible-but-wrong same-length rewrite of the source of truth is a net
  *downgrade* in trust, and guards can't fully prevent it.

So the eval layer is **strictly read-then-insert**:

```
transcripts / segments / transcript_chunks   ── READ ONLY ──▶  judge (LLM)
                                                                   │
                                                                   ▼
                                              INSERT-ONLY ──▶  transcript_findings
```

The eval package issues **no** `UPDATE`/`DELETE`/`ALTER` against the transcript
tables — ever. `transcript_findings` has no FK that could cascade a mutation
back into them. A unit test asserts the package's SQL never contains a write
verb against those tables.

## 2. Sampling / trigger strategy (bounded cost)

Running the judge over every segment of every book is wasteful — the judge is a
big LLM and the corpus is large. The eval layer is **sampled or on-demand**,
never an always-on pass:

- **On-demand, per-book**: `earmark eval "<book substring>"` evaluates the
  matched book(s). The operator picks what to look at (e.g. a book that read
  badly, or one transcribed by a new backend).
- **Sampled**: `earmark eval --sample N` picks N chunks at random across the
  library (or within a book scope) and evaluates only those. This bounds cost to
  N LLM calls regardless of library size and still yields a representative
  error-rate estimate.
- **Unit of evaluation = the chunk**, not the segment. A chunk is tens of
  consecutive ASR segments (the same unit search uses), so one LLM call covers a
  meaningful span of text with timestamps, and the finding's
  `segment_or_chunk_ref` points at a real, addressable row.
- **Dry-run by default.** Like `requeue`, `earmark eval` previews what it would
  evaluate/record and writes nothing unless `--write`/`--yes` is passed. This
  keeps an accidental invocation from spending GPU time or writing rows.

Cost is therefore always operator-bounded: by book scope, by `--sample N`, and
by the dry-run gate.

## 3. `transcript_findings` schema

Additive table, created in the same `internal/db` schema-init transaction as the
other tables. **No cascade that mutates the transcript tables.**

```sql
CREATE TABLE transcript_findings (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    transcript_id        UUID        NOT NULL,        -- which transcript (no FK cascade-mutate)
    file_path            TEXT        NOT NULL,        -- book/track path (denormalized, query convenience)
    chunk_id             UUID,                        -- the evaluated chunk, when chunk-scoped (nullable)
    chunk_index          INTEGER,                     -- ordinal within the transcript (nullable)
    start_sec            FLOAT8      NOT NULL,         -- finding span start
    end_sec              FLOAT8      NOT NULL,         -- finding span end
    original_text        TEXT        NOT NULL,        -- the suspected-wrong span, verbatim
    issue_type           TEXT        NOT NULL,         -- misheard_proper_noun | run_on | number_artifact | ...
    suggested_correction TEXT,                        -- ADVISORY ONLY — never applied
    confidence           FLOAT8      NOT NULL,         -- judge self-score 0..1
    model                TEXT        NOT NULL,         -- judge model id (attributable)
    transcription_run_id UUID,                        -- the job/run that produced the transcript (per-backend attribution)
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX transcript_findings_file_path_idx     ON transcript_findings (file_path);
CREATE INDEX transcript_findings_transcript_id_idx ON transcript_findings (transcript_id);
CREATE INDEX transcript_findings_run_id_idx        ON transcript_findings (transcription_run_id);
CREATE INDEX transcript_findings_issue_type_idx    ON transcript_findings (issue_type);
```

Notes:

- `transcript_id` is **not** a foreign key with `ON DELETE CASCADE` back into
  the transcript table in a way that would let findings mutate transcripts.
  Findings are downstream, advisory observations; a requeue that drops a
  transcript may orphan findings, which is acceptable (they describe a now-stale
  run; cleanup is a separate, additive concern, mirroring `run_metrics`).
- `transcription_run_id` is the `transcription_jobs.id` of the run that produced
  the transcript. This is what makes findings **attributable per backend/run** —
  the same judge over Parakeet vs Whisper output yields two run ids you can
  group by.
- `confidence` is the judge's self-scored confidence; it is the triage and
  scoring signal (§5), not ground truth.

## 4. Judge prompt + confidence calibration

The judge is given a chunk's text (with its timestamp span and book context) and
asked to return **strict JSON** — an array of findings. Prompt shape:

- System: "You are a transcription QA reviewer. You are given a span of an
  audiobook transcript produced by an automatic speech recognizer. Identify
  *suspected* transcription errors. You are advisory only: never rewrite the
  transcript, only flag spans. Be conservative — prefer missing a subtle error
  to flagging correct text."
- The closed `issue_type` vocabulary is given to the model so the field stays
  enumerable:
  - `misheard_proper_noun` — a name/place likely mis-recognized
  - `run_on` — sentence/segment boundary lost
  - `number_artifact` — digits/units garbled ("nineteen eighty four" vs "1984")
  - `homophone` — wrong word, right sound ("their"/"there")
  - `dropped_word` — likely omission
  - `other` — anything else suspected
- Output contract (one object per suspected error):
  ```jsonc
  {
    "findings": [
      {
        "original_text": "...verbatim span from the input...",
        "issue_type": "misheard_proper_noun",
        "suggested_correction": "...advisory...",
        "confidence": 0.0
      }
    ]
  }
  ```
- An **empty `findings` array is a valid, expected answer** (clean span).

### Confidence calibration

- `confidence` is in `[0,1]`. The prompt instructs the model to self-score:
  `>0.8` only for an obvious error (clear proper-noun garble), `0.4–0.7` for
  "looks off, could be correct", `<0.4` for a weak hunch.
- The judge has false positives and misses; because findings are advisory-only
  that is harmless — you **triage by confidence**. The dashboard surfaces a
  confidence distribution so a flood of low-confidence noise is visible.
- Parsing is defensive: malformed JSON, an out-of-range confidence (clamped to
  `[0,1]`), or an unknown `issue_type` (coerced to `other`) never crashes the
  run — the bad finding is dropped or normalized and the run continues.
- **Judge precision is itself a tracked metric over time** — spot-checking a
  sample of high-confidence findings tells you whether to trust the judge for a
  given model/book.

## 5. Backend eval-harness tie-in (the second, free payoff)

Because findings carry `model` (the judge) and `transcription_run_id` (the ASR
run under evaluation), running the **same judge** over transcripts produced by
**different ASR backends** yields a comparative quality metric for free:

- Transcribe a book (or sample) with Parakeet, then with Whisper/Granite (the
  deferred multi-backend A/B, issue #50-ish).
- Run `earmark eval` over both runs with the same judge model.
- Compare: findings-per-1000-words, mean confidence of findings, issue-type mix,
  per backend/run.

`confidence` becomes the **scoring signal** for that comparison: a backend whose
output yields fewer high-confidence findings transcribed the same audio more
faithfully (per the judge). This is exactly the measurement the multi-backend
A/B needs, and the eval layer produces it as a side effect of its primary
quality-observability job.

## 6. Dashboard surface

Additive, self-contained `/findings` page (new nav entry) so it merges cleanly
with #48's parallel reshaping of the Servers/Models page — no shared template is
restructured. The page shows:

- Total findings, mean confidence, and a confidence-bucket breakdown
  (high `≥0.8` / medium `0.4–0.8` / low `<0.4`).
- Findings grouped per book: count, dominant issue types, mean confidence.
- Issue-type tally across the library.

It is read-only/observability only, consistent with the rest of the dashboard,
and backed by the same `--demo` fixture path so it renders with no database.

## 7. Endpoint resolution (the #48 stub)

#48 owns the AI endpoint registry (`AI_ROLES`). Until it lands, the eval layer
resolves its chat endpoint from **standalone env vars**, read directly in
`internal/eval` (config structs are left untouched to avoid a merge conflict
with #48):

| Env var | Required | Meaning |
|---------|----------|---------|
| `EVAL_CHAT_BASE_URL` | yes (to run) | OpenAI-compatible base URL, e.g. `http://vllm:8000/v1` |
| `EVAL_CHAT_MODEL` | yes (to run) | judge model id, e.g. `qwen2.5-32b-instruct` |
| `EVAL_CHAT_API_KEY` | no | bearer token if the endpoint requires one |

The chat call is abstracted behind a small `eval.ChatClient` interface so the
swap to the registry is a one-line resolution change at the call site (marked
with a `TODO(#48)` comment), not a rewrite of the judge.

## 8. Read-only / advisory contract (CONTRACT §2.15)

Documented in `docs/CONTRACT.md §2.15`: the env vars, the `transcript_findings`
table, and the binding rule that the eval layer is read-only over the transcript
tables and its output is advisory metadata only — never applied.
