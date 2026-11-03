# ASR Runner

`runner.py` is the external transcription worker that feeds earmark. It runs
**outside** the earmark Go service — natively on a CUDA GPU host — and is the
producer side of the [`docs/CONTRACT.md`](../docs/CONTRACT.md) interface.

## What it does

Polls the `transcription_jobs` Postgres queue (`FOR UPDATE SKIP LOCKED`), claims
pending jobs, transcribes the audio file with NVIDIA NeMo Parakeet-TDT (word-level
timestamps native — no separate alignment stage), and writes `transcripts` +
`run_metrics` rows in the JSON shape `docs/CONTRACT.md` specifies. The Go service
never transcribes; it only enqueues jobs and indexes the results.

It also reacts to the `runner_control` gate (CONTRACT §1.4): besides the
`paused`/`run_limit` claim gate, when `paused` or `phase='analyze'` it **parks its
GPU model to host RAM** (`asr_model.cpu()` + `torch.cuda.empty_cache()`) so a
different GPU tenant can use the card, and restores it (`asr_model.cuda()`) when
active again. The `phase` column is read defensively — a DB without it behaves
exactly as before.

Each transcript row also carries what produced it and the book's embedded
identity (CONTRACT §1.2, migration 00006): the ASIN tag ffprobe reports in the
file's format/stream tags (`ASIN`, `AUDIBLE_ASIN`, `----:<mean>:ASIN`;
validated, else NULL) as `embedded_asin`, and the provenance the Go side stamps
the asr recipe from — `asr_model_sha256` (sha256 of the `.nemo` the HF cache holds for the model —
`try_to_load_from_cache`, as NeMo does — streamed and computed once per
process at model load), `asr_runner_version` (NULL when the version is
unknown, so the row is not stamped), and `asr_params` (compute type, chunk/segment geometry,
diarization, biasing). Against a database without those columns the runner
writes the original row, so it can update before earmark migrates.
OpenTelemetry in the runner is not implemented yet.

`docs/CONTRACT.md` is **authoritative** for column names, env-var names, the
capability vocabulary (§2.13), and the result shape. This file is just orientation.

## Deployment

Not deployed from this repo. The [homelab-ansible](https://github.com/jedwards1230/homelab-ansible)
`asr-runner` role owns the host concerns — Python 3.12 venv, the cu130 torch +
NeMo install, the systemd unit, the env file (including the `DATABASE_URL` DSN),
the NFS books mount, and GPU-arbiter wiring. The role fetches this `runner.py`
from a pinned earmark tag, so the contract, runner, and tests version together.

Configuration is entirely env-driven (systemd `EnvironmentFile`); see the module
docstring in `runner.py` for the full variable list. There are no secrets or
host-specific values baked into the source — the code defaults are generic and
the role supplies the real values.

## Failures, retries and resume

Failures are classified (`_classify_failure`, CONTRACT §1.3 "Transient vs
permanent failures"). **Transient** ones — a lost DB connection, CUDA OOM, an
unreachable books share or an SMB/ffmpeg I/O error, an ffmpeg/ffprobe timeout —
are retried in place with exponential backoff and jitter; when those retries run
out the job goes back to `pending` **without** its attempt refunded (so the
3-attempt cap still bounds it; the last attempt fails it) with the error
recorded, and the main loop backs off before the next claim (and holds claims
while the share stays down). **Permanent** ones — a missing file on a reachable
share, undecodable audio, a bad path — mark the job failed, as before.

Long (chunked) jobs write a local checkpoint after every window, so a
restart, a crash, an exhausted retry or a mid-job GPU yield resumes at the next
window instead of 0 s (CONTRACT §1.3 "Resume checkpoints"). Between windows the
runner re-checks the park gate — `paused`, `phase='analyze'`, or gpu-arbiter's
`arbiter_yield` (migration 00018, CONTRACT §1.4) — and, if it closed, releases
the job (attempt refunded) and parks: an eviction costs at most one window.
Pausing therefore also stops a long job at its next window boundary. Single-pass
files (≤ `ASR_CHUNK_THRESHOLD_SECONDS`) and jobs whose checkpoint is disabled
finish first. While claims are held after a transient failure, the heartbeat,
self-update check and GPU gate keep running every poll.

| Variable | Default | What |
|---|---|---|
| `RUNNER_RETRY_BASE_SECONDS` | `10` | first backoff delay |
| `RUNNER_RETRY_FACTOR` | `3` | backoff multiplier (10 → 30 → 90 s) |
| `RUNNER_RETRY_MAX_SECONDS` | `300` | cap on one retry delay |
| `RUNNER_RETRY_JITTER` | `0.2` | ± fraction of random jitter on each delay |
| `RUNNER_RETRY_IO_ATTEMPTS` | `3` | retries for SMB/ffmpeg/ffprobe I/O errors and timeouts |
| `RUNNER_RETRY_OOM_ATTEMPTS` | `2` | retries per window after a CUDA OOM (`gc` + `torch.cuda.empty_cache()`, no sleep) |
| `RUNNER_RETRY_DB_ATTEMPTS` | `3` | reconnect-and-retry for a DB error while writing the transcript |
| `RUNNER_TRANSIENT_BACKOFF_MAX_SECONDS` | `600` | cap on the main-loop wait after consecutive transient releases |
| `RUNNER_FFMPEG_TIMEOUT_FLOOR_SECONDS` | `120` | minimum ffmpeg timeout; the ffprobe timeout |
| `RUNNER_FFMPEG_TIMEOUT_FACTOR` | `0.5` | ffmpeg timeout = max(floor, factor × seconds of audio decoded) |
| `RUNNER_CHECKPOINT_DIR` | `C:\ProgramData\earmark-asr\checkpoints` (Windows), `<RUNNER_TMP_DIR>/earmark-asr-checkpoints` (else) | resume checkpoints; `off` disables; pruned after 7 days |

## Running the tests

```bash
pip install psycopg2-binary pytest
python -m pytest runner/test_runner.py -v
```

`test_runner.py` exercises the queue logic, the capability descriptor, path
re-rooting, and the busy/pause gates with mocked Postgres. It imports only stdlib
+ psycopg2 — **no torch/NeMo**, so it runs on any machine with no GPU. earmark CI
runs this suite on every push.

## Pilot backends

`pilot.py` is a **one-shot** second-ASR pilot, separate from the production
runner. It re-transcribes the tracks of a few books with a second model and
stores the result **alongside** production in `pilot_transcripts` (migration
00017). It never touches `transcription_jobs`, `runner_control`, `transcripts`,
chunks or embeddings. It does not import `runner.py`. Its only write is one
transaction per finished track. A track that does not finish writes nothing.

Both backends produce **window-level text**: one segment per window, words
without start/end, `has_word_times=false`, casing and punctuation verbatim. The
asr_vote step aligns that text against the production (parakeet) word times, so
neither model needs timestamps.

| `--backend` | Model (revision via `PILOT_MODEL_REVISION`) | Runtime | Window (default / hard max) | `--batch-size` default |
|---|---|---|---|---|
| `granite` | `ibm-granite/granite-speech-4.1-2b` (base; pin `823ca793aa49925d4eb2e758709b7f2c98513e8e`) | transformers, bf16, `sdpa` (`flash_attention_2` if `flash_attn` is importable) | 240 s / 540 s | 16 |
| `canary` | `nvidia/canary-qwen-2.5b` | NeMo SALM (`speechlm2`), bf16 autocast on CUDA | 38 s / 40 s | 32 |

The Granite -plus timestamp mode was dropped: it budgets about 4× the output
tokens and ran at batch 1, an estimated ~14 h for the pilot set.

### How a track is processed

- **Track selection.** Tracks are the files of the given ASINs that have a
  production transcript: `book_metadata.asin`, then `book_dir`, then the jobs
  whose `file_path` directory equals `book_dir`, then `transcripts`.
- **Window planning.** Windows are cut from the **production** (parakeet) word
  times, read one track at a time. Each cut sits at the largest inter-word gap in
  the last 25% of the target window, so no word is split. No window exceeds the
  backend's hard max. With no production words, the cuts fall at fixed intervals.
  A final window shorter than 5 s is folded into the previous one when the merged
  window still fits the hard max.
- **Batching.** Windows go through `generate()` in batches of `--batch-size`,
  within a track and across tracks (short chapter files fill a batch together).
  - Granite: the audio is zero-padded to the longest window, the prompt is
    left-padded, and each row's new tokens are sliced off past the shared input
    length.
  - Canary: `SALM.generate` gets one prompt per row with padded `audios` and
    `audio_lens`.
  - `max_new_tokens` is sized by the batch's **longest** window: 12 tokens per
    audio second for Granite and 8 for Canary, × 1.25, with a floor of 64 / 128.
  - A CUDA OOM halves the batch size and retries that batch. The smaller size is
    kept for the rest of the run, and the out-file records it as `batch_size_used`.
- **Audio.** Audio is decoded in chunks of contiguous windows (`ffmpeg -ss … -t …`
  to 16 kHz mono float32). A chunk lasts max(30 min, batch size × window) so it
  holds a full batch: Granite 16 × 240 s gives 64 min, about 250 MB. In findings
  scope, only the merged spans are decoded. ffmpeg's output is streamed into a
  buffer sized for the chunk, so memory does not grow with track length. Each
  chunk has a timeout of 120 s + 5% of its length, and each window gets its own
  writable copy.
- **Granite prompt.** Taken verbatim from the card: `<|audio|>transcribe the speech
  with proper punctuation and capitalization.` It runs with `num_beams=1`,
  `do_sample=False` and `--repetition-penalty` (default 1.0: the base card gives
  none).
- **Canary prompt.** `Transcribe the following: <audio_locator_tag>`, from the card.
- **Degenerate windows.** A window is marked degenerate for one of these reasons:
  - `repetition`: an n-gram repeats back-to-back (a single word 8 times, longer
    n-grams 4 to 5 times);
  - `token_limit`: the window hit its token limit;
  - `empty`: no text came back.

  Its segment carries the optional keys `"degenerate": true` and
  `"degenerate_reason"`. The out-file lists every degenerate window.
- **`--scope findings`** (flag only; `full` is the default). It transcribes just
  the windows around a track's **held** findings, for targeted checks. A held
  finding is `patch_state='proposed'`, not superseded, with
  `end_sec > start_sec`, and its latest decision event not revoked by a `revoke`
  event is a `hold`. Spans are padded by ±5 s, each padded edge snaps to the
  largest production gap inside the pad, and overlapping spans are merged. Rows
  carry `scope='findings'`.
- **Idempotency.** A row is keyed by `(file_path, model_id, model_revision,
  scope)`. Tracks already stored are skipped. `--force` deletes the old row and
  inserts the new one in the same transaction.
- **Gaming check.** With `--arbiter-url`, gpu-arbiter `GET /status` is read
  before the model loads and before each track, decode chunk and batch. State `gaming` or `evicting` stops
  the run with exit 4. An unreadable status is retried once after 2 s and then
  counts as gaming.
- **Stop file.** If `PILOT_STOP_FILE` is set and that file exists, checked at the
  same points, the run stops cleanly with exit 5.
- **On either stop:** unfinished tracks, including those in the current batch,
  are not written. Finished tracks stay done, so a re-run resumes.
- **Write order.** The per-track JSON is written to the out dir **before** the
  DB write, so a failed write never loses the GPU work. Right before the write,
  the connection is pinged and reopened if it died during the track.
- **Metrics per track:**
  - `audio_sec`;
  - `wall_sec`: the track's share of batch time, split by audio length,
    excluding decode;
  - `rtfx = audio_sec / wall_sec`;
  - `peak_vram_mib` and `peak_reserved_mib`: the max over the batches the track
    was in;
  - out-file only: `decode_sec`, `batch_size` and `batch_size_used`. The table
    has no columns for these.

### CLI

```text
python pilot.py --backend {granite,canary} --asin A1,A2,...
                [--scope full|findings] [--limit-tracks N] [--track-limit-per-book N]
                [--batch-size N] [--repetition-penalty P] [--force] [--dry-run]
                [--out-dir DIR] [--arbiter-url URL] [--self-check]
                [--bench [--bench-batches 1,8,16,32] [--bench-max-sec 1800]]
```

| Flag | Effect |
|---|---|
| `--track-limit-per-book N` | Keeps the first N tracks of each book (by `file_path`). The set is stable across re-runs. |
| `--limit-tracks N` | Caps how many not-yet-done tracks this run processes. |
| `--batch-size N` | Windows per `generate()` call. Defaults: granite 16, canary 32. |
| `--repetition-penalty P` | Granite only. Default 1.0 (off). |
| `--dry-run` | Prints one JSON line per track a real run would process, after the already-done filter and `--limit-tracks`, with its window plan. It reads the DB in a read-only session. It loads no model, decodes no audio and writes nothing. |
| `--bench` | Use with `--asin X --limit-tracks 1`. Runs the first `--bench-max-sec` of that track once per batch size in `--bench-batches`, then prints one JSON object: `{"bench": {backend, model_id, model_revision, file_path, window_sec, max_sec, results: [{batch_size, batch_size_used, windows, audio_sec, wall_sec, decode_sec, rtfx, peak_vram_mib, peak_reserved_mib, degenerate_windows}]}}`. No DB writes. It runs the same gaming and stop-file checks as a real run (exit 4 / 5, nothing printed). |
| `--out-dir` | Where each finished track's JSON goes (plan, segments, metrics, degenerate windows). The default is `PILOT_OUT_DIR`, else `<tempdir>/earmark-asr-pilot-out`. |
| `--self-check` | Loads the model and transcribes 5 s of silence. It cannot be combined with `--dry-run` or `--bench`. Prints `{backend, model, revision, torch, cuda, vram, output, attn_implementation, ffmpeg, ffprobe}`. Needs no DB. |

### Environment

| Variable | Meaning |
|---|---|
| `DATABASE_URL` | Required, except with `--self-check`. |
| `PILOT_MODEL_REVISION` | **Required.** The HF commit sha to load. Unpinned runs are refused (exit 2). |
| `PILOT_MODEL_ID` | Defaults to the model in the table above. |
| `HF_HOME` | The pilot's own HF cache. |
| `BOOKS_MOUNT`, `BOOKS_DB_ROOT`, `BOOKS_PATH_ENCODING` | Same meaning as in `runner.py`. |
| `FFMPEG_BIN`, `FFPROBE_BIN` | Default `ffmpeg` / `ffprobe`. `runner.py` has no env names for these; it runs them from `PATH`. |
| `PILOT_WINDOW_SEC` | Target window length. Values above the backend's hard max are a config error. |
| `PILOT_DEVICE` | Default `cuda`. bf16 autocast (Canary) is only used on a CUDA device. |
| `PILOT_OUT_DIR` | Default out dir when `--out-dir` is not given. |
| `PILOT_STOP_FILE` | Optional path. If the file exists, the run stops cleanly with exit 5. |

**Exit codes:** `0` ok, `2` config error, `3` some tracks failed, `4` stopped for gaming, `5` stopped by `PILOT_STOP_FILE`.

### Install layout

The asr_pilot_windows role (homelab-ansible) installs the pilot on desktop-2:

- `C:\Program Files\earmark-asr-pilot\pilot.py`, fetched with a sha256 pin from
  an earmark ref.
- Two venvs: `...\venv-granite` (`requirements-pilot-granite.txt`) and
  `...\venv-canary` (`requirements-pilot-canary.txt`).
- Data under `C:\ProgramData\earmark-asr-pilot\{hf,out,logs}`, plus its own
  `uv-python`.

Both venvs are **separate from the frozen production venv** (torch
2.12.0+cu130, nemo_toolkit 2.7.3, transformers 4.57.6) and must never be
installed into it. Granite needs transformers ≥ 5.8, and SALM needs NeMo trunk.
The path must not contain `C:\Program Files\earmark-asr\runner.py`, because the
one-runner reaper kills anything that does. Both requirements files are fully
pinned for Windows x86_64 / CPython 3.12. `flash_attn` is not included, since it
has no official Windows wheels, so Granite runs on `sdpa`. Install each with:

```text
uv pip install --no-deps -r <file> --extra-index-url https://download.pytorch.org/whl/cu130 --index-strategy unsafe-best-match
```

### Expected cost (UNMEASURED)

None of this has run on real hardware. The pilot set is 143 h of audio.

- **Target:** RTFx ≥ 100 for each backend, which is about 1 h per backend over
  the whole set.
- **Granite 2B (base):** the Open ASR leaderboard lists RTFx 231. Expect about
  4–6 GB of bf16 weights plus activations for 16 × 240 s windows.
- **Canary-Qwen 2.5B:** the model card reports RTFx 418 on an A100 (batched).
  Expect about 5–6 GB of weights plus 32 × 38 s windows.

Measure both with `--bench` on desktop-2 before a full run, and set
`--batch-size` from the result.

### Tests

`python -m pytest runner/test_pilot.py -v` stubs the backend, the decoder and
Postgres. It needs no torch, transformers, NeMo, numpy or ffmpeg.
