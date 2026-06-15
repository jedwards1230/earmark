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
