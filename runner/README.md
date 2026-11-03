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
`arbiter_yield` (migration 00016, CONTRACT §1.4) — and, if it closed, releases
the job (attempt refunded) and parks: an eviction costs at most one window.

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
