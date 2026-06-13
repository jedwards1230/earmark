# ASR Runner Relocation — Design Doc

**Status**: Plan (Stage 1 only — raw pinned fetch)
**Date**: 2026-06-13
**Branch**: feat/asr-runner-relocation

---

## 1. Problem Statement

The ASR runner (`runner.py`, ~1784 lines) lives embedded in
`homelab-ansible/roles/asr-runner/files/`. The earmark repo holds the canonical
`docs/CONTRACT.md` that the runner implements. Having source and contract in
separate repos created a real cross-repo drift incident. The test suite
(`test_runner.py`) also lives in ansible — where it is never actually run by CI.

**Goal**: move `runner.py` + `test_runner.py` into earmark so the contract,
runner, and tests version together. The ansible role swaps its `ansible.builtin.copy`
of an embedded file for a `get_url` fetch from a pinned earmark tag.

**Stage 1 (this doc)**: pinned raw fetch with checksum.
**Stage 2 (deferred)**: earmark publishes a versioned pip wheel; the role `pip
install`s it. No further design here.

---

## 2. GPU-Arbiter Coupling Analysis

### 2.1 What gpu-arbiter does with asr-runner

gpu-arbiter (`repos/gpu-arbiter`, Rust daemon on desktop-1) manages GPU tenants
via the `managed_units` list in its TOML config. For `asr-runner.service` it:

- Calls `systemctl stop asr-runner.service` when a game launches (GPU eviction).
- Calls `systemctl start asr-runner.service` on game exit (`eager_restart: true`).
- Matches the substring `"parakeet"` against `nvidia-smi` compute-process names
  to attribute this unit's VRAM in `/status`.

**Source**: `host_vars/desktop-1.yaml`:

```yaml
gpu_arbiter_managed_units:
  - unit: "ollama.service"
    eager_restart: true
    vram_match: "ollama"
  - unit: "asr-runner.service"
    eager_restart: true
    vram_match: "parakeet"
```

gpu-arbiter does **not** read or write the `RUNNER_BUSY_FLAG_PATH` file. The busy
flag is an independent mechanism written by the game-session daemon (or operator)
to gate the runner's claim logic at the Python level.

### 2.2 What writes the busy flag

The `RUNNER_BUSY_FLAG_PATH` (`/tmp/lilbro-whisper-busy` default) is consumed
**only** by `runner.py` itself — `_claim_job()` checks `BUSY_FLAG_PATH.exists()`
before claiming. Nothing in gpu-arbiter reads or writes this path. The `asr-runner.env.j2`
template injects the value from `asr_runner_busy_flag_path` (currently defaulting
to `/tmp/lilbro-whisper-busy` in `defaults/main.yaml`).

### 2.3 Coupling verdict

| Coupling point | Nature | Safe to rename? |
|---|---|---|
| `asr-runner.service` unit name | Systemd unit name; gpu-arbiter drives by exact name | **Must not change** — rename would break gpu-arbiter stop/start; the unit name is "asr-runner" regardless of project name |
| `vram_match: "parakeet"` | Substring matched against nvidia-smi process names | Safe: this is a model-architecture substring, not a project-name string; stays as-is |
| `RUNNER_BUSY_FLAG_PATH` default `/tmp/lilbro-whisper-busy` | Python default only; real value comes from env file | **Change code default** to `/tmp/earmark-asr-busy`; ansible role continues to supply the configured value — deployed behavior unchanged; the old name on running systems is overridden by the env file |
| gpu-arbiter config TOML | References `asr-runner.service` only | No change needed |

**Conclusion**: The systemd unit name (`asr-runner.service`) is load-bearing and
must not be renamed. gpu-arbiter does not touch the busy-flag path. All other
lab-specific strings in `runner.py` are either code defaults (overridden by the
env file in production) or documentation/comments.

---

## 3. Genericize Diff — Exact Classification

Every lab-specific string, classified by action:

### 3.1 Module docstring (lines 3, 34, 42, 45–48)

| Current text | Classification | Action |
|---|---|---|
| `asr-runner — desktop-1 CUDA transcription worker for lilbro-whisper.` | (a) project rename | Change to `asr-runner — CUDA transcription worker for earmark.` |
| `DATABASE_URL … PostgreSQL DSN for lilbro-whisper-pg` | (b) comment/provenance note | Rename: `PostgreSQL DSN for earmark (earmark-pg)` |
| `RUNNER_IDENTITY default: desktop-1-runner-pid-<pid>` | (b) code default text; real value from env | Update default in both docstring and code to `earmark-runner-pid-<pid>` |
| `RUNNER_BUSY_FLAG_PATH default: /tmp/lilbro-whisper-busy` | (b) code default | Change default to `/tmp/earmark-asr-busy` (env file continues supplying lab value) |

### 3.2 RUNNER_IDENTITY default (line 102)

```python
# Before
RUNNER_IDENTITY: str = os.environ.get(
    "RUNNER_IDENTITY", f"desktop-1-runner-pid-{os.getpid()}"
)

# After
RUNNER_IDENTITY: str = os.environ.get(
    "RUNNER_IDENTITY", f"earmark-runner-pid-{os.getpid()}"
)
```

Classification: **(b)** neutralize code default; ansible env file continues
supplying `asr_runner_identity = "desktop-1-runner"` for the real deployment.

### 3.3 RUNNER_BUSY_FLAG_PATH default (line 107)

```python
# Before
BUSY_FLAG_PATH: Path = Path(
    os.environ.get("RUNNER_BUSY_FLAG_PATH", "/tmp/lilbro-whisper-busy")
)

# After
BUSY_FLAG_PATH: Path = Path(
    os.environ.get("RUNNER_BUSY_FLAG_PATH", "/tmp/earmark-asr-busy")
)
```

Classification: **(b)** neutralize code default. The ansible `defaults/main.yaml`
currently sets `asr_runner_busy_flag_path: /tmp/lilbro-whisper-busy`; the env
template injects it. The deployed runner reads the env var and ignores the code
default entirely. The `defaults/main.yaml` value itself can be renamed to
`/tmp/earmark-asr-busy` as part of the ansible PR (Step 2) — this is an
operator-visible change but `/tmp/earmark-asr-busy` is only the default when the
env file is absent, which never happens in production.

> NOTE: The `DATABASE_URL` env name (the stale `lilbro-whisper-pg` DSN credential
> bug tracked separately) is out of scope here. The env var name stays `DATABASE_URL`
> — that is CONTRACT §2.4 and changes nothing here.

### 3.4 BOOKS_MOUNT default (line 110)

```python
# Before
BOOKS_MOUNT: Path = Path(
    os.environ.get("BOOKS_MOUNT", "/mnt/tank-hdd/media/books")
)

# After — no change
BOOKS_MOUNT: Path = Path(
    os.environ.get("BOOKS_MOUNT", "/mnt/tank-hdd/media/books")
)
```

Classification: **(b) no change needed.** The path `/mnt/tank-hdd/media/books`
is the NFS mount path for audiobook files; it is a host-specific default that
the ansible env file overrides in production. It is not a project-name string.
It makes perfect sense as a default for any deployment that mounts books at this
path. Leave it as-is.

### 3.5 BOOKS_DB_ROOT default (line 118)

```python
# No change — /books is the producer-side container path from CONTRACT §2.4
BOOKS_DB_ROOT: Path = Path(os.environ.get("BOOKS_DB_ROOT", "/books"))
```

Classification: **(b) no change.** This is the CONTRACT-specified producer mount
path, not a project-name string.

### 3.6 Hardware comments throughout the file (lines 17, 22, 96, 122, 642, 944, 1040, 1432, 1447, 1507, 1549)

Examples: `Verified on desktop-1 (RTX 5090, ...)`, `Measured on desktop-1 (RTX 5090 32 GB, ...)`,
`use_triton=True enables GPU-accelerated trie traversal (safe on RTX 5090)`.

Classification: **(c)** move provenance notes. Change to generic phrasing where
they read as requirements:
- `Verified on desktop-1 (RTX 5090, ...)` → `Verified on RTX 5090 (Blackwell, 32 GB VRAM, ...)` — drop the host name, keep the hardware fact
- `safe on RTX 5090` → `safe on Blackwell/Ampere+` — hardware architecture, not host name
- `desktop-1: a 90-min split...` → remove the host prefix

### 3.7 Service template comments (asr-runner.service.j2)

`Description=ASR Runner — NeMo Parakeet-TDT (lilbro-whisper)` →
`Description=ASR Runner — NeMo Parakeet-TDT (earmark)`

`# NeMo Parakeet-TDT ASR runner for lilbro-whisper` →
`# NeMo Parakeet-TDT ASR runner for earmark`

These are ansible-side changes, part of Step 2.

### 3.8 Summary: what the DEPLOYED runner changes

**Nothing.** All real values come from the ansible env file. The code-default
changes affect only a runner started with no env file (not a production scenario).

---

## 4. earmark Repo Placement

### 4.1 Directory layout

```
repos/earmark/
└── runner/
    ├── runner.py          # genericized source (Stage 1: fetched by ansible)
    ├── test_runner.py     # test suite (CI only, not deployed)
    ├── requirements.txt   # documents torch/nemo deps (role installs; pip-installable in Stage 2)
    └── README.md          # brief: what it is, env vars, how to run tests locally
```

`runner/README.md` is minimal — purpose, key env vars, `python -m pytest runner/`
to run tests. The `docs/CONTRACT.md` stays authoritative for the full interface spec.

### 4.2 Docker image impact

The `Dockerfile` uses `COPY . .` in the builder stage but builds only Go (no
Python). The `runner/` directory will be copied into the builder and then
discarded — the final distroless image contains only the `/earmark` binary.
**No image size impact.**

The `.dockerignore` currently excludes `docs/`, `*.md`, `deploy/`, etc. — it
does NOT need a `runner/` entry because the builder stage compiles only Go
(`go build ./...`) and the final stage copies only the binary.  However, adding
`runner/` to `.dockerignore` is a minor optimization to keep the build context
lean, and is recommended:

```
runner/
```

Add this line to `.dockerignore` as part of the earmark PR.

### 4.3 Release tag containment

The release workflow checks out the tagged commit (`ref: ${{ needs.prepare.outputs.tag }}`).
At that ref, `runner/runner.py` exists in the repo tree, so the raw-content GitHub
URL resolves:

```
https://raw.githubusercontent.com/jedwards1230/earmark/<TAG>/runner/runner.py
```

This URL works for any tag created after the earmark PR merges. The file is not
excluded by `.dockerignore` from the git tree (`.dockerignore` affects docker
build context only, not the git tag).

### 4.4 Python CI job (recommended: YES)

Add a `python` job to `.github/workflows/ci.yml`:

```yaml
python:
  name: Python runner tests
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v6
    - uses: actions/setup-python@v5
      with:
        python-version: "3.12"
    - name: Install test dependencies
      run: pip install psycopg2-binary pytest
    - name: Run runner unit tests
      run: python -m pytest runner/test_runner.py -v
```

`test_runner.py` contains ~100 unit tests. Reading the test file confirms it
uses only stdlib + psycopg2 imports that are mockable — no NeMo/torch import
at test time (the GPU backend classes are instantiated only when the runner
starts). The `psycopg2-binary` + `pytest` install is fast (~5 s). This is the
correct answer: the runner stays tested in its new home, CI catches regressions
before a release.

---

## 5. Pin + Checksum Mechanics (ansible role change)

### 5.1 New role defaults

Add to `roles/asr-runner/defaults/main.yaml`:

```yaml
# -- Source fetch (Stage 1: pinned raw fetch from earmark) --
# Pin to an earmark git tag or commit SHA. Update this ref and recompute the
# checksum whenever upgrading the runner. See UPDATE PROCEDURE below.
asr_runner_source_ref: "v0.17.0"   # example — set to actual tag on first deploy
asr_runner_source_checksum: "sha256:REPLACE_WITH_ACTUAL_CHECKSUM"
```

### 5.2 Replace the copy task in tasks/main.yaml

Remove:

```yaml
- name: Deploy runner.py
  ansible.builtin.copy:
    src: runner.py
    dest: "{{ asr_runner_install_dir }}/runner.py"
    owner: root
    group: root
    mode: "0755"
  become: true
  notify: Restart asr-runner
```

Replace with:

```yaml
- name: Fetch runner.py from earmark (pinned ref)
  ansible.builtin.get_url:
    url: "https://raw.githubusercontent.com/jedwards1230/earmark/{{ asr_runner_source_ref }}/runner/runner.py"
    dest: "{{ asr_runner_install_dir }}/runner.py"
    checksum: "{{ asr_runner_source_checksum }}"
    owner: root
    group: root
    mode: "0755"
  become: true
  notify: Restart asr-runner
```

**Authentication**: earmark is a public repo. No GitHub token is needed for raw
content fetches. Confirmed: `https://raw.githubusercontent.com/<public-repo>/...`
returns 200 with no credentials.

**Idempotence**: `get_url` compares the destination file's checksum against
`checksum:` on every run and only re-downloads (and notifies the handler) when
they differ. Same semantics as the old `copy`.

**Offline / air-gap note**: if desktop-1 ever loses internet access during a
deploy, the fetch will fail. In that case fall back to manually placing the file
or temporarily using `ansible.builtin.copy` with a local copy. This is acceptable
for Stage 1; Stage 2 (pip wheel) resolves it by pre-caching in the venv.

### 5.3 Remove embedded files

Delete from `roles/asr-runner/files/`:
- `runner.py` (now fetched from earmark)
- `test_runner.py` (now lives in earmark CI; not deployed)

The `CONTRACT.md` in `roles/asr-runner/files/` should also be removed (or
converted to a symlink comment pointing to `earmark/docs/CONTRACT.md`) since it
will drift again — it was copied from earmark originally and that's what caused
the drift problem.

### 5.4 Update procedure

When a new runner version is ready:

1. Merge the runner changes to earmark `main`, create a release tag (e.g. `v0.18.0`).
2. Compute the checksum:
   ```bash
   curl -sL https://raw.githubusercontent.com/jedwards1230/earmark/v0.18.0/runner/runner.py \
     | sha256sum
   ```
3. Update `defaults/main.yaml`:
   ```yaml
   asr_runner_source_ref: "v0.18.0"
   asr_runner_source_checksum: "sha256:<hash-from-step-2>"
   ```
4. Commit to homelab-ansible `main`, deploy:
   ```bash
   ansible-playbook repos/homelab-ansible/playbooks/site-desktop.yml \
     --limit desktop-1 --tags asr-runner
   ```

The two-variable pattern (`ref` + `checksum`) ensures:
- The ref is human-readable and communicates intent.
- The checksum pins the exact content — a tag that is force-pushed or a SHA
  collision cannot silently inject new code.

---

## 6. What the Role Keeps Owning

The ansible role retains full ownership of all host concerns:

| What | Where |
|---|---|
| venv creation (uv, Python 3.12) | `tasks/main.yaml` (unchanged) |
| pip-install torch cu130 + NeMo + psycopg2 | `tasks/main.yaml` (unchanged) |
| systemd unit file | `templates/asr-runner.service.j2` (minor text change) |
| env file with vault-encrypted DSN | `templates/asr-runner.env.j2` (unchanged) |
| role defaults | `defaults/main.yaml` (add source_ref + source_checksum; update busy_flag default) |
| NFS mount for books | `tasks/main.yaml` (unchanged) |
| gpu-arbiter wiring | `host_vars/desktop-1.yaml` (unchanged) |
| GPU smoke test | `tasks/main.yaml` (unchanged) |

Only these three things change in the role:

1. `files/runner.py` and `files/test_runner.py` are **deleted**.
2. `files/CONTRACT.md` is **deleted** (contract lives in earmark; remove the stale copy).
3. The Deploy runner.py task is **replaced** with the `get_url` fetch.

---

## 7. Two-Repo Ship Sequence

### Step 1 — earmark PR (do first)

**Branch**: `feat/asr-runner-relocation`

Changes:
- Add `runner/runner.py` (genericized per Section 3)
- Add `runner/test_runner.py` (unchanged from ansible)
- Add `runner/requirements.txt` (documents deps; no pip install by this file yet)
- Add `runner/README.md` (minimal)
- Add `runner/` to `.dockerignore`
- Add `python` CI job to `.github/workflows/ci.yml`

**Semver label**: `semver:minor` — adds a new directory + CI job; the Go binary
and Helm chart are unchanged. A minor bump communicates "runner source now lives
here" without suggesting a breaking change to the Go API.

After merge, the release workflow fires and creates e.g. `v0.17.0`. Confirm the
tag exists and the raw URL resolves:

```bash
curl -sL https://raw.githubusercontent.com/jedwards1230/earmark/v0.17.0/runner/runner.py | head -5
```

Compute and record the checksum for Step 2.

### Step 2 — homelab-ansible PR (after Step 1 tag exists)

**Branch**: `feat/asr-runner-fetch-from-earmark`

Changes:
- `roles/asr-runner/defaults/main.yaml`: add `asr_runner_source_ref` + `asr_runner_source_checksum`; update `asr_runner_busy_flag_path` default to `/tmp/earmark-asr-busy`
- `roles/asr-runner/tasks/main.yaml`: replace `copy` with `get_url`
- `roles/asr-runner/files/runner.py`: **delete**
- `roles/asr-runner/files/test_runner.py`: **delete**
- `roles/asr-runner/files/CONTRACT.md`: **delete**
- `roles/asr-runner/templates/asr-runner.service.j2`: update description text
- `roles/asr-runner/README.md`: update to note source is now fetched from earmark

No semver label needed — homelab-ansible has no release process.

### Step 3 — Deploy (requires user approval)

```bash
ansible-playbook repos/homelab-ansible/playbooks/site-desktop.yml \
  --limit desktop-1 --tags asr-runner
```

Post-deploy verification:
1. `systemctl status asr-runner.service` — running.
2. `ls -la /opt/asr-runner/runner.py` — file present, owned root, mode 0755.
3. `sha256sum /opt/asr-runner/runner.py` — matches `asr_runner_source_checksum`.
4. `journalctl -u asr-runner -n 50` — runner started, no errors.
5. Check gpu-arbiter: `curl -s http://localhost:48750/status | jq .` — `asr-runner.service` appears in `units[]`.
6. Verify busy-flag gate (if a game is running): `/tmp/lilbro-whisper-busy` file
   still works (the env file continues to supply `/tmp/lilbro-whisper-busy`
   because `asr_runner_busy_flag_path` in `host_vars/desktop-1.yaml` is not
   changed).

---

## 8. Open Questions (Need User Decision)

1. **Busy-flag path in `defaults/main.yaml`**: The role default is currently
   `/tmp/lilbro-whisper-busy` and the running system uses it (injected by env
   file). Renaming the default to `/tmp/earmark-asr-busy` affects only
   deployments that lack a host_vars override — in practice, only a fresh
   deploy with no `host_vars/desktop-1.yaml` entry. Should the `defaults/main.yaml`
   value also be renamed, or left at `/tmp/lilbro-whisper-busy` to minimize
   diff? (Recommendation: rename it; it's the default and should match the project.)

2. **CONTRACT.md in `files/`**: The ansible role has a stale copy of
   `docs/CONTRACT.md`. Confirm it should be deleted in Step 2 (not just updated)
   — the contract lives in earmark's `docs/CONTRACT.md` and the runner now fetches
   from there.

3. **First earmark tag**: What is the current latest earmark version? The `source_ref`
   placeholder above uses `v0.17.0` but the actual tag created by the earmark PR
   may differ. Confirm the tag name after Step 1 before setting the checksum in
   Step 2.

4. **`host_vars/desktop-1.yaml` busy-flag**: Currently not set (using role
   default). If the default is renamed, no change is needed here. But if the
   game-session daemon is hard-coded to write `/tmp/lilbro-whisper-busy`, the
   rename of the DEFAULT does not matter — the env file always supplies the value.
   Verify which component (if any) creates this file externally.

---

## 9. Stage 2 Placeholder (Out of Scope)

Stage 2 converts the role from a raw fetch to a pip install of a versioned
earmark wheel. This requires:
- earmark adds a `pyproject.toml` in `runner/` and publishes to PyPI (or GitHub
  Packages as a simple index).
- The ansible role replaces `get_url` + path manipulation with `uv pip install
  earmark-runner==<version>` in the venv.
- The `runner.py` entry point becomes `earmark-runner` (console script).

This is a separate design doc and separate PRs. Stage 1 is complete and
self-contained without it.
