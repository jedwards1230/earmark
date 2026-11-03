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

| `--backend` | Model | Runtime | Window (default / hard max) | Output |
|---|---|---|---|---|
| `granite` | `ibm-granite/granite-speech-4.1-2b-plus` | transformers, bf16 | 90 s / 210 s | words **with** start/end; lowercase, no punctuation |
| `canary` | `nvidia/canary-qwen-2.5b` | NeMo SALM (`speechlm2`), bf16 | 38 s / 40 s | one segment per window; words **without** start/end; casing and punctuation kept |

### How a track is processed

- **Track selection.** Tracks are the files of the given ASINs that have a
  production transcript: `book_metadata.asin`, then `book_dir`, then the jobs
  whose `file_path` directory equals `book_dir`, then `transcripts`.
- **Window planning.** Windows are cut from the **production** (parakeet) word
  times, read one track at a time. Each cut sits at the largest inter-word gap in
  the last 25% of the target window, so no word is split. No window exceeds the
  backend's hard max. With no production words, the cuts fall at fixed intervals.
- **Audio.** Each track is decoded once to 16 kHz mono float32 (`ffmpeg`, with a
  timeout of 120 s + 5% of the track length) and sliced in memory. That is about
  230 MB of RAM per hour of audio.
- **Granite.** The model gets the model card's timestamp prompt with
  `repetition_penalty=2.0` and `num_beams=1`. `max_new_tokens` is
  `max(256, 48 × window_sec × 1.1)`. The output is parsed with
  `re.split(r"\[T:(\d+)\]")`:
  - the centisecond end times wrap every 10 s and are unrolled;
  - `_` silence tokens are dropped, but they still advance the clock;
  - a word starts where the previous token ended.
- **Repetition guard.** A window is marked degenerate if any n-gram repeats
  back-to-back (a single word 8 times, longer n-grams 4 to 5 times) or if the
  window hits its token limit. Its segments then carry an optional
  `"degenerate": true` key.
- **`--scope findings`.** Only the windows around the track's **held** findings
  are transcribed. A held finding is `patch_state='proposed'` with a decide
  `hold` event. Each finding's chunk span (`start_sec`/`end_sec`) is padded by
  ±5 s. Overlapping spans are merged, then windowed as above. Rows carry
  `scope='findings'`, and `audio_sec` is the sum of the windows.
- **Idempotency.** A row is keyed by `(file_path, model_id, model_revision,
  scope)`. Tracks already stored are skipped. `--force` deletes the old row and
  inserts the new one in the same transaction.
- **Gaming check.** With `--arbiter-url`, gpu-arbiter `GET /status` is read
  before each track and before each window. State `gaming` or `evicting`, or an
  unreadable status, stops the run with exit 4. The current track is not
  written; finished tracks stay done, so a re-run resumes.
- **Metrics per track:**
  - `audio_sec`;
  - `wall_sec`, which covers decode plus transcription;
  - `rtfx = audio_sec / wall_sec`;
  - `peak_vram_mib` (`torch.cuda.max_memory_allocated`, reset per track);
  - `peak_reserved_mib` (`max_memory_reserved`).

### CLI

```text
python pilot.py --backend {granite,canary} --asin A1,A2,...
                [--scope full|findings] [--limit-tracks N] [--track-limit-per-book N]
                [--force] [--dry-run] [--out-dir DIR] [--arbiter-url URL] [--self-check]
```

| Flag | Effect |
|---|---|
| `--track-limit-per-book N` | Keeps the first N tracks of each book (by `file_path`). The set is stable across re-runs. |
| `--limit-tracks N` | Caps how many not-yet-done tracks this run processes. |
| `--dry-run` | Prints one JSON line per track with its window plan. It reads the DB in a read-only session. It loads no model, decodes no audio and writes nothing. |
| `--out-dir` | Also writes each finished track's JSON (plan, segments, metrics) there. |
| `--self-check` | Loads the model and transcribes 5 s of silence. Prints `{backend, model, revision, torch, cuda, vram, output, ffmpeg, ffprobe}`. Needs no DB. |

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
| `PILOT_DEVICE` | Default `cuda`. |

**Exit codes:** `0` ok, `2` config error, `3` some tracks failed, `4` stopped for gaming.

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
pinned for Windows x86_64 / CPython 3.12. Install each with:

```text
uv pip install --no-deps -r <file> --extra-index-url https://download.pytorch.org/whl/cu130 --index-strategy unsafe-best-match
```

### Expected cost (UNMEASURED)

None of this has run on real hardware. These are guesses, to be replaced by the
first `--self-check` and pilot runs on the 5090:

- **Granite 2B-plus:** about 4–6 GB of bf16 weights, about 8–12 GB peak on 90 s
  windows. It decodes autoregressively at about 48 tokens per audio second, so
  expect RTFx in the tens, far below parakeet.
- **Canary-Qwen 2.5B:** about 5–6 GB of weights, about 8–12 GB working. The
  model card reports RTFx 418 on an A100 with batched inference. Here it runs one
  40 s window at a time, so expect much lower.

### Tests

`python -m pytest runner/test_pilot.py -v` stubs the backend, the decoder and
Postgres. It needs no torch, transformers, NeMo, numpy or ffmpeg.
