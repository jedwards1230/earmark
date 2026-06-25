#!/usr/bin/env python3
"""
asr-runner — CUDA transcription worker for earmark.

Polls the transcription_jobs Postgres queue, claims pending jobs, transcribes
audio files with NVIDIA NeMo Parakeet-TDT (word-level timestamps native — no
separate alignment stage), and writes results back per the CONTRACT.md JSON
shape.

The Postgres job-queue logic (claim, heartbeat, mark_done/failed, busy-flag
gate, path-traversal guard) is engine-agnostic and preserved unchanged from the
original runner design. Claims are gated by runner_control (CONTRACT
§1.4): the durable `paused` flag plus a `run_limit` counter (NULL = unlimited)
that the runner decrements per claim for bounded runs (e.g. a single-job smoke
test driven by the Go control API).

GPU self-parking (CONTRACT §1.4): when the runner is paused or runner_control.phase
is 'analyze', it moves its NeMo model OFF the GPU (asr_model.cpu() +
torch.cuda.empty_cache(), parking weights in host RAM — seconds, not a from-disk
reload) so a different GPU tenant (a future eval-judge LLM) can use the card. When
active again (not paused AND phase in NULL/'idle'/'transcribe') it restores the
model (asr_model.cuda()). The park/unpark only happen at the gate between jobs,
never mid-transcription, and only on a state change. The `phase` column is read
defensively (it may not exist yet in the deployed DB — a separate PR adds it);
a missing column/row degrades to 'idle' (model stays on GPU, today's behavior).

Verified on RTX 5090 (Blackwell, torch 2.12.0+cu130, 2026-06-07): model load,
transcribe(timestamps=True), word timestamps in seconds (hyp.timestamp['word']),
segment text (hyp.timestamp['segment']), bfloat16, and the contract mapping all
work end-to-end.

Long-form chunking verified on RTX 5090 (32 GB, NeMo 2.7.3, 2026-06-10):
single-pass full-attention inference scales ~quadratically in VRAM and OOMs
above ~18 min of audio on the 32 GB card (15 min ≈ 16 GB peak, 18 min ≈ 23 GB
peak, 20 min OOMs). The published "~3 h single pass" figure does NOT hold here.
So the chunked path uses split-and-stitch: ffmpeg-cut the file into overlapping
windows, transcribe each single-pass, and stitch word/segment timestamps back to
absolute file offsets (de-duplicating the overlap). Validated on a 90-min 1984
clip: 9 × 600 s windows, 8.2 GB peak, 0 non-monotonic word boundaries, words
spanning 0.96 s → 5399.68 s. NOT yet exercised on hardware: the diarization
(Sortformer) path — it stays flagged below.

Environment variables (from systemd EnvironmentFile):
    DATABASE_URL                required  PostgreSQL DSN for earmark (earmark-pg)
    ASR_BACKEND                 default: nemo-parakeet (backend selector)
    ASR_FAMILY                  default: nemo-parakeet (CONTRACT §2.13 family id → run_metrics.asr_family)
    ASR_RUNTIME                 default: nemo-cuda (CONTRACT §2.13 runtime id → run_metrics.asr_runtime)
    ASR_CAPABILITIES            default: unset (optional JSON map of advertised caps, §2.13)
    ASR_MODEL                   default: nvidia/parakeet-tdt-0.6b-v3
    ASR_DIARIZE                 default: false (true enables NeMo Sortformer)
    ASR_COMPUTE_TYPE            default: bfloat16
    ASR_CHUNK_THRESHOLD_SECONDS default: 900 (single-pass below; chunked above)
    ASR_BIASING_ENABLED         default: false (true applies NeMo boosting_tree word-boosting)
    ASR_BIASING_ALPHA           default: 2.5 (boosting weight; 2–3 is typical, ≥6 injects terms)
    RUNNER_IDENTITY             default: earmark-runner-pid-<pid>
    RUNNER_POLL_INTERVAL_SECONDS  default: 30
    RUNNER_HEARTBEAT_SECONDS    default: 60
    RUNNER_BUSY_FLAG_PATH       default: /tmp/earmark-asr-busy
    BOOKS_MOUNT                 default: /mnt/media/books (this host's NFS mount)
    BOOKS_DB_ROOT               default: /books (root the DB file_path is rooted at,
                                i.e. the Go producer's container BOOKS_DIR; re-rooted onto BOOKS_MOUNT)

HF_TOKEN is NOT required — Parakeet-TDT and NeMo Sortformer are public models.
"""

from __future__ import annotations

import abc
import json
import logging
import math
import os
import signal
import socket
import subprocess
import tempfile
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import psycopg2
import psycopg2.extras

# ---------------------------------------------------------------------------
# Configuration (env var names fixed by CONTRACT.md §2.4)
# ---------------------------------------------------------------------------

DATABASE_URL: str = os.environ["DATABASE_URL"]

# ASR backend selector — determines which ASRProvider class is instantiated.
# Accepted values: "nemo-parakeet" (default). Future values: "whisperx", etc.
ASR_BACKEND: str = os.environ.get("ASR_BACKEND", "nemo-parakeet")

# ASR backend descriptor (CONTRACT §2.13) — reported into run_metrics so backends
# can be compared honestly (A/B). family/runtime are free-form-but-conventional
# ids (recommended set in §2.13); they default to this concrete runner's actual
# backend rather than NULL, since this runner *is* the NeMo-Parakeet-on-CUDA one.
#   ASR_FAMILY   — model-family id (e.g. nemo-parakeet, whisper).
#   ASR_RUNTIME  — runtime id (e.g. nemo-cuda, whisper.cpp-sycl).
#   ASR_CAPABILITIES — optional JSON map of the runner's *advertised* (static)
#     capabilities, keys from the §2.13 closed enum → bool. Unset → derived from
#     the provider. Unknown keys are ignored with a warning. This is advisory
#     metadata only; the per-run caps_applied/caps_requested are computed from the
#     actual code behavior, not from this var.
ASR_FAMILY: str = os.environ.get("ASR_FAMILY", "nemo-parakeet")
ASR_RUNTIME: str = os.environ.get("ASR_RUNTIME", "nemo-cuda")

# Closed capability enum (CONTRACT §2.13). The runner uses exactly these keys in
# caps_applied / caps_requested / caps_skipped_reason; any other key is dropped
# with a warning (forward-compat — a newer earmark may add keys an older runner
# does not know).
_CAPABILITY_KEYS: frozenset[str] = frozenset(
    {
        "word_timestamps",
        "context_biasing",
        "diarization",
        "confidence_scores",
        "language_detection",
    }
)


def _parse_advertised_capabilities(raw: str | None) -> dict[str, bool] | None:
    """
    Parse the optional ASR_CAPABILITIES env var (a JSON object of
    capability-key → bool) per CONTRACT §2.13.

    Returns a dict restricted to the closed enum keys, or None when unset/blank.
    Unknown keys are dropped with a warning (forward-compat). A malformed value
    (not JSON, not an object) logs a warning and returns None — it is advisory
    metadata and must never fail startup or a transcription.
    """
    if not raw or not raw.strip():
        return None
    try:
        parsed = json.loads(raw)
    except (ValueError, TypeError) as exc:
        log.warning("ASR_CAPABILITIES is not valid JSON (ignoring): %s", exc)
        return None
    if not isinstance(parsed, dict):
        log.warning(
            "ASR_CAPABILITIES must be a JSON object of capability→bool (ignoring): %r",
            parsed,
        )
        return None
    out: dict[str, bool] = {}
    for key, val in parsed.items():
        if key not in _CAPABILITY_KEYS:
            log.warning("ASR_CAPABILITIES: ignoring unknown capability key %r", key)
            continue
        out[key] = bool(val)
    return out or None


ASR_CAPABILITIES: dict[str, bool] | None = _parse_advertised_capabilities(
    os.environ.get("ASR_CAPABILITIES")
)

# ASR backend configuration (NeMo Parakeet-TDT).
ASR_MODEL_ID: str = os.environ.get(
    "ASR_MODEL", "nvidia/parakeet-tdt-0.6b-v3"
)
ASR_DIARIZE: bool = os.environ.get("ASR_DIARIZE", "false").lower() == "true"
ASR_COMPUTE_TYPE: str = os.environ.get("ASR_COMPUTE_TYPE", "bfloat16")

# Word-boosting via NeMo boosting_tree / key_phrases_file (PR 5).
#
# ASR_BIASING_ENABLED gates the entire feature; when false (the default) the
# runner behaves identically to before PR 5 — no decoding-config mutation, no
# temp file, byte-identical output.
#
# ASR_BIASING_ALPHA controls the context_score weight fed to BoostingTreeModelConfig.
# Measured on RTX 5090: 2–3 is effective, ≥6 starts injecting bias terms even
# when not spoken. Default 2.5 is a conservative midpoint validated in practice.
ASR_BIASING_ENABLED: bool = os.environ.get("ASR_BIASING_ENABLED", "false").lower() == "true"
ASR_BIASING_ALPHA: float = float(os.environ.get("ASR_BIASING_ALPHA", "2.5"))

RUNNER_IDENTITY: str = os.environ.get(
    "RUNNER_IDENTITY", f"earmark-runner-pid-{os.getpid()}"
)
POLL_INTERVAL: int = int(os.environ.get("RUNNER_POLL_INTERVAL_SECONDS", "30"))
HEARTBEAT_INTERVAL: int = int(os.environ.get("RUNNER_HEARTBEAT_SECONDS", "60"))
BUSY_FLAG_PATH: Path = Path(
    os.environ.get("RUNNER_BUSY_FLAG_PATH", "/tmp/earmark-asr-busy")
)
BOOKS_MOUNT: Path = Path(
    os.environ.get("BOOKS_MOUNT", "/mnt/media/books")
)
# Producer-side root the DB file_path is rooted at. The Go monitor records
# file_path rooted at its container books mount (BOOKS_DIR, default /books) —
# e.g. "/books/audio-custom/Author/Book.m4b". This host mounts the SAME NFS
# share at BOOKS_MOUNT (a different path), so paths are re-rooted from
# BOOKS_DB_ROOT onto BOOKS_MOUNT before opening the file. Relative file_paths
# (the CONTRACT's nominal form) are joined to BOOKS_MOUNT directly.
BOOKS_DB_ROOT: Path = Path(os.environ.get("BOOKS_DB_ROOT", "/books"))

# Single-pass vs chunked threshold.
#
# Measured on RTX 5090 (32 GB, NeMo 2.7.3, bfloat16, 2026-06-10):
# Parakeet-TDT v3's encoder uses full attention at inference, so peak VRAM grows
# ~quadratically with audio length:
#     5 min ≈ 3.8 GB | 15 min ≈ 16.4 GB | 18 min ≈ 22.7 GB (OK) | 20 min → OOM.
# The often-quoted "handles ~3 h single pass" does NOT hold on a 32 GB card.
#
# Default threshold = 900 s (15 min): single-pass below it peaks ~16 GB, which
# leaves comfortable headroom for other GPU users on this shared box (Sunshine,
# game-shell). Files above it take the split-and-stitch chunked path, which
# peaks well under 9 GB regardless of total length. Overridable via env for
# tuning, but do not raise it past ~1080 s (18 min) without re-measuring VRAM.
CHUNK_THRESHOLD_SECONDS: float = float(
    os.environ.get("ASR_CHUNK_THRESHOLD_SECONDS", "900")
)

# Chunked (split-and-stitch) window geometry, in seconds.
#   CHUNK_WINDOW_SECONDS  — core audio transcribed per window (kept text).
#   CHUNK_OVERLAP_SECONDS — extra audio appended to each window purely for
#                           acoustic context so words straddling a cut aren't
#                           truncated; words starting inside the overlap are
#                           dropped (the next window's core owns them), which
#                           de-duplicates the seam.
# 600 s windows peak ~6-8 GB (far under the single-pass threshold), giving large
# headroom. 15 s overlap comfortably exceeds any single spoken word/clause.
CHUNK_WINDOW_SECONDS: float = float(
    os.environ.get("ASR_CHUNK_WINDOW_SECONDS", "600")
)
CHUNK_OVERLAP_SECONDS: float = float(
    os.environ.get("ASR_CHUNK_OVERLAP_SECONDS", "15")
)

# Fine-grained segmentation geometry (CONTRACT §1.2.1).
#
# NeMo Parakeet-TDT returns exactly ONE segment per transcribe() call regardless
# of clip length (verified on desktop-1: a 60 s multi-sentence clip yields
# N_SEGMENTS=1), so hyp.timestamp['segment'] carries no usable granularity — one
# window == one ~600 s block. We therefore ALWAYS derive segments from the
# per-word timestamps (hyp.timestamp['word'], which are accurate to ~0.1 s) by
# splitting on inter-word silence gaps, with a hard duration/word cap so no
# segment exceeds a few tens of seconds — well under any 5-minute floor — even in
# gap-less narration. Per-second precision is always preserved in each segment's
# words[] array.
#
#   SEGMENT_GAP_SECONDS  — a gap larger than this starts a new segment. 0.6 s
#                          tracks sentence boundaries in audiobook narration
#                          (measured: ~one >0.6 s gap per ~15 words, i.e. roughly
#                          one per sentence) while staying above comma/phrase
#                          pauses (p90 gap ~0.24 s).
#   SEGMENT_MAX_SECONDS  — force a split once the current segment reaches this
#                          duration, bounding granularity in continuous speech.
#   SEGMENT_MAX_WORDS    — force a split once the current segment reaches this
#                          many words (belt-and-suspenders for fast speech).
SEGMENT_GAP_SECONDS: float = float(
    os.environ.get("ASR_SEGMENT_GAP_SECONDS", "0.6")
)
SEGMENT_MAX_SECONDS: float = float(
    os.environ.get("ASR_SEGMENT_MAX_SECONDS", "30")
)
SEGMENT_MAX_WORDS: int = int(
    os.environ.get("ASR_SEGMENT_MAX_WORDS", "80")
)

# Language reported in the transcript.  Parakeet-TDT is English-only; "en"
# is the only valid value in production.
TRANSCRIPT_LANGUAGE: str = "en"

MAX_ERROR_LEN = 2000

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S",
)
log = logging.getLogger("asr-runner")

# ---------------------------------------------------------------------------
# Graceful shutdown
# ---------------------------------------------------------------------------

_shutdown = threading.Event()


def _handle_signal(signum: int, _frame: Any) -> None:
    log.info("Received signal %d — initiating graceful shutdown", signum)
    _shutdown.set()


signal.signal(signal.SIGTERM, _handle_signal)
signal.signal(signal.SIGINT, _handle_signal)

# ---------------------------------------------------------------------------
# Database helpers (engine-agnostic — preserved from original design)
# ---------------------------------------------------------------------------


def _connect() -> psycopg2.extensions.connection:
    """Open a fresh database connection with autocommit disabled."""
    conn = psycopg2.connect(DATABASE_URL, cursor_factory=psycopg2.extras.RealDictCursor)
    conn.autocommit = False
    return conn


# Atomic claim of one pending job: oldest pending row with attempts < 3, locked
# SKIP LOCKED so concurrent claimers never block, RETURNING the fields the worker
# needs. Shared by the gated and degrade-safe claim paths.
_CLAIM_SQL = """
    UPDATE transcription_jobs
    SET    status     = 'claimed',
           claimed_by = %s,
           claimed_at = now(),
           attempts   = attempts + 1
    WHERE  id = (
        SELECT id
        FROM   transcription_jobs
        WHERE  status = 'pending'
          AND  attempts < 3
        ORDER  BY created_at ASC
        FOR UPDATE SKIP LOCKED
        LIMIT  1
    )
    RETURNING id, file_path, checksum
"""


def _control_values(row: Any) -> tuple[bool, int | None]:
    """
    Parse a runner_control row into (paused, run_limit).

    A missing row → (False, None): not paused, unlimited. Tolerates dict-like
    (RealDictRow) or tuple rows.
    """
    if row is None:
        return False, None
    if isinstance(row, dict):
        return bool(row["paused"]), row["run_limit"]
    return bool(row[0]), row[1]


def _read_control_row(conn: psycopg2.extensions.connection) -> Any:
    """
    Read the runner_control row (paused, run_limit) for the GPU-park decision,
    WITHOUT a FOR UPDATE lock — this is an advisory read, not the authoritative
    claim gate (that lives in _claim_job and re-reads under a lock). A missing
    row/table degrades to None → not-paused/unlimited. Transaction state is left
    clean (commit on success, rollback on error).
    """
    try:
        with conn.cursor() as cur:
            cur.execute("SELECT paused, run_limit FROM runner_control WHERE id = 1")
            row = cur.fetchone()
        conn.commit()
        return row
    except Exception as exc:  # noqa: BLE001 — degrade safe on missing table/row
        log.debug("control read failed (%s) — defaulting to not-paused", exc)
        try:
            conn.rollback()
        except Exception:
            pass
        return None


# Phase values the runner understands (CONTRACT §1.4). NULL / 'idle' / 'transcribe'
# all mean "ASR may use the GPU"; only 'analyze' means "step off the GPU".
_PHASE_DEFAULT = "idle"
_PHASE_ON_GPU = {None, "idle", "transcribe"}


def _read_phase(conn: psycopg2.extensions.connection) -> str:
    """
    Read runner_control.phase defensively (CONTRACT §1.4).

    The `phase` column is additive and may not exist in the deployed DB yet (a
    separate PR adds it). A missing column, a missing row, or any DB/schema error
    degrades to the default phase ('idle') so the runner keeps its model on the
    GPU and behaves exactly as it does today. Each call uses its own transaction
    (committed on success, rolled back on error) so the connection's transaction
    state is left clean for the caller's subsequent work.
    """
    try:
        with conn.cursor() as cur:
            cur.execute("SELECT phase FROM runner_control WHERE id = 1")
            row = cur.fetchone()
        conn.commit()
    except Exception as exc:  # noqa: BLE001 — degrade safe on missing column/row/table
        log.debug("phase read failed (%s) — defaulting to %r", exc, _PHASE_DEFAULT)
        try:
            conn.rollback()
        except Exception:
            pass
        return _PHASE_DEFAULT
    if row is None:
        return _PHASE_DEFAULT
    value = row["phase"] if isinstance(row, dict) else row[0]
    return _PHASE_DEFAULT if value is None else str(value)


def _should_park(paused: bool, phase: str | None) -> bool:
    """
    Decide whether the ASR model should be parked OFF the GPU (CONTRACT §1.4).

    Pure function (no side effects) so the decision is unit-testable without a
    GPU. The model stays ON the GPU when NOT paused AND phase in
    (NULL/'idle'/'transcribe'); it parks to host RAM when paused OR phase is
    'analyze' (or any other non-GPU phase value), freeing VRAM for the eval judge.
    """
    if paused:
        return True
    return phase not in _PHASE_ON_GPU


def _gate_gpu(
    conn: psycopg2.extensions.connection,
    provider: "ASRProvider",
) -> bool:
    """
    Run one per-cycle GPU gate decision (CONTRACT §1.4) and return whether the
    caller should SKIP claiming a job this cycle.

    Reads paused (advisory, unlocked) + phase (defensive), decides via
    _should_park, and applies the park/unpark side-effect on the provider — but
    only on a state change (the provider tracks parked/loaded state, so a
    steady-state poll does no redundant .cpu()/.cuda()). Returns:
        True  → park the GPU; skip the claim this cycle (model is off the card).
        False → model is active; proceed to claim a job.

    A park()/unpark() failure (e.g. torch unavailable, a CUDA error) is caught
    and logged HERE with accurate context ("GPU park/unpark failed") rather than
    propagating to the main loop's claim handler, where it would be mislabeled a
    "DB error during claim". On a park failure we still skip the claim (fail safe:
    do not start a job we were told not to run); on an unpark failure we skip the
    claim too (don't transcribe on a card we couldn't reclaim) and retry next poll.
    The decision (paused/phase) is computed via the pure helpers, so this function
    is testable with stubs and no real GPU.
    """
    paused, _ = _control_values(_read_control_row(conn))
    phase = _read_phase(conn)
    should_park = _should_park(paused, phase)
    try:
        if should_park:
            provider.park()
        else:
            provider.unpark()
    except Exception as exc:  # noqa: BLE001 — isolate GPU faults from the claim path
        log.error(
            "GPU %s failed (paused=%s phase=%s): %s — skipping claim this cycle",
            "park" if should_park else "unpark",
            paused,
            phase,
            exc,
        )
        return True  # skip the claim; do not let a GPU fault masquerade as a DB error
    if should_park:
        log.debug("GPU parked (paused=%s phase=%s) — skipping claim", paused, phase)
    return should_park


def _claim_one(conn: psycopg2.extensions.connection) -> dict[str, Any] | None:
    """
    Claim a single pending job in the current transaction without gating, then
    commit. Used on the degrade-safe path when the control row can't be read.
    """
    with conn.cursor() as cur:
        cur.execute(_CLAIM_SQL, (RUNNER_IDENTITY,))
        row = cur.fetchone()
        conn.commit()
        return dict(row) if row else None


def _claim_job(
    conn: psycopg2.extensions.connection,
) -> dict[str, Any] | None:
    """
    Atomically claim one pending job, honoring the runner_control gate.

    Gate (CONTRACT §1.4): claim iff (NOT paused) AND
    (run_limit IS NULL OR run_limit > 0). When a bounded run is active
    (run_limit not NULL), the counter is decremented by 1 in the SAME
    transaction as the claim — and only when a row is actually claimed, so an
    empty-queue poll never consumes budget. The runner_control row is locked
    FOR UPDATE so the gate read, the claim, and the decrement are atomic.

    Returns the row dict {id, file_path, checksum}, or None if the queue is
    empty or a gate is set (host-local busy flag, paused, or run_limit
    exhausted). A missing runner_control table/row degrades to
    not-paused/unlimited so the runner still works before the Go service has
    initialized the schema.
    """
    if BUSY_FLAG_PATH.exists():
        log.info("Busy flag present (%s) — skipping claim", BUSY_FLAG_PATH)
        return None

    # Gate read, claim, and decrement all happen in ONE cursor under ONE
    # transaction, so the FOR UPDATE lock on runner_control is held continuously
    # from the gate read through the decrement and is released only by the final
    # commit (or rollback). In psycopg2 the transaction belongs to the connection,
    # not the cursor — but keeping everything in a single scope makes the
    # atomicity self-evident and guarantees the lock is released here (rollback on
    # any failure) rather than relying on the caller. On any failure (e.g. the
    # table doesn't exist yet) degrade safe: roll back and claim without gating.
    try:
        with conn.cursor() as cur:
            cur.execute(
                "SELECT paused, run_limit FROM runner_control WHERE id = 1 FOR UPDATE"
            )
            paused, run_limit = _control_values(cur.fetchone())

            if paused:
                conn.commit()  # release the lock; no claim this cycle
                log.info("Pipeline paused (runner_control.paused) — skipping claim")
                return None
            if run_limit is not None and run_limit <= 0:
                conn.commit()  # release the lock; no claim this cycle
                log.info("Run limit reached (run_limit=0) — skipping claim")
                return None

            # Still holding the lock. Claim one job; on success, decrement a
            # bounded run by 1 in the same transaction so N is exact. The
            # decrement runs only when a row was claimed, so an empty-queue poll
            # never consumes budget.
            cur.execute(_CLAIM_SQL, (RUNNER_IDENTITY,))
            row = cur.fetchone()
            if row is not None and run_limit is not None:
                cur.execute(
                    "UPDATE runner_control "
                    "SET run_limit = run_limit - 1, updated_at = now() "
                    "WHERE id = 1"
                )
            conn.commit()  # release the lock after claim + decrement
            return dict(row) if row else None
    except Exception as exc:  # noqa: BLE001 — degrade safe on any DB/schema error
        conn.rollback()  # always release the FOR UPDATE lock on failure
        log.debug("Control/claim failed (%s) — degrading to ungated claim", exc)
        return _claim_one(conn)


def _heartbeat(conn: psycopg2.extensions.connection, job_id: str) -> None:
    """Update updated_at to prevent stale-claim recovery from the Go service."""
    try:
        with conn.cursor() as cur:
            cur.execute(
                """
                UPDATE transcription_jobs
                SET    updated_at = now()
                WHERE  id = %s AND status = 'claimed'
                """,
                (job_id,),
            )
        conn.commit()
    except Exception as exc:
        log.warning("Heartbeat failed for job %s: %s", job_id, exc)
        try:
            conn.rollback()
        except Exception:
            pass


def _runner_heartbeat(conn: psycopg2.extensions.connection) -> None:
    """
    Stamp a liveness heartbeat on the singleton runner_control row EVERY poll
    cycle — whether the runner is working, idle (empty queue), or parked/paused.

    This is the signal that distinguishes "alive but idle" from "down": the Go
    collector exposes it as earmark_runner_alive_seconds (seconds since this
    stamp). It is DISTINCT from _heartbeat(), which only stamps
    transcription_jobs.updated_at while a job is claimed (stale-claim recovery)
    and therefore goes quiet the moment the queue drains — the exact case that
    made an idle runner look identical to a dead one.

    Best-effort: any failure (e.g. the runner_heartbeat_at column not yet
    deployed) is logged at DEBUG and swallowed so it can never disturb the
    claim/transcribe path. Uses its own commit/rollback so the connection is left
    clean for the subsequent gate + claim.
    """
    try:
        with conn.cursor() as cur:
            cur.execute("UPDATE runner_control SET runner_heartbeat_at = now()")
        conn.commit()
    except Exception as exc:
        log.debug("Runner liveness heartbeat failed (non-fatal): %s", exc)
        try:
            conn.rollback()
        except Exception:
            pass


def _fetch_bias_terms(
    conn: psycopg2.extensions.connection,
    file_path: str,
) -> list[str] | None:
    """
    Look up book_metadata.bias_terms for the book that owns *file_path*.

    The book_dir key is derived from file_path by taking its parent directory
    component (os.path.dirname), matching the convention used by the Go service
    when it inserts rows into book_metadata.

    Returns a non-empty list of strings on success, or None when:
      - the row does not exist,
      - bias_terms is NULL,
      - the list is empty after stripping whitespace,
      - any DB / schema error occurs.

    This call is best-effort: any error is logged at WARNING level and the
    function returns None so the job proceeds with plain (unbiased) transcription.
    The connection transaction state is left clean (commit on success, rollback on
    any error) so the caller's subsequent heartbeat / mark_done calls are not
    affected.
    """
    book_dir = os.path.dirname(file_path)
    try:
        with conn.cursor() as cur:
            cur.execute(
                "SELECT bias_terms FROM book_metadata WHERE book_dir = %s",
                (book_dir,),
            )
            row = cur.fetchone()
        conn.commit()
    except Exception as exc:  # noqa: BLE001
        log.warning(
            "bias_terms lookup failed for book_dir=%r (proceeding without biasing): %s",
            book_dir,
            exc,
        )
        try:
            conn.rollback()
        except Exception:
            pass
        return None

    if row is None:
        log.debug("No book_metadata row for book_dir=%r — no biasing", book_dir)
        return None

    # bias_terms is a TEXT[] column; psycopg2 returns it as a Python list or None.
    raw: list[str] | None = row["bias_terms"] if isinstance(row, dict) else row[0]
    if not raw:
        return None

    terms = [t.strip() for t in raw if t and t.strip()]
    if not terms:
        return None

    log.info(
        "Loaded %d bias term(s) for book_dir=%r",
        len(terms),
        book_dir,
    )
    return terms


def _mark_done(
    conn: psycopg2.extensions.connection,
    job_id: str,
    file_path: str,
    checksum: str,
    result: dict[str, Any],
) -> None:
    """
    Write transcript row and mark job done in a single transaction.

    result keys: language, duration_seconds, speaker_count, segments, raw_text
    """
    with conn.cursor() as cur:
        cur.execute(
            """
            INSERT INTO transcripts
                (job_id, file_path, checksum, language, duration_seconds,
                 speaker_count, segments, raw_text, model_name)
            VALUES (%s, %s, %s, %s, %s, %s, %s::jsonb, %s, %s)
            """,
            (
                job_id,
                file_path,
                checksum,
                result["language"],
                result["duration_seconds"],
                result.get("speaker_count"),
                json.dumps(result["segments"]),
                result["raw_text"],
                ASR_MODEL_ID,
            ),
        )
        cur.execute(
            """
            UPDATE transcription_jobs
            SET    status = 'done',
                   error  = NULL
            WHERE  id = %s
            """,
            (job_id,),
        )
    conn.commit()
    log.info("Job %s marked done (file: %s)", job_id, file_path)


def _mark_failed(
    conn: psycopg2.extensions.connection, job_id: str, error: str
) -> None:
    """Mark a job as permanently failed with a truncated error message."""
    truncated = error[:MAX_ERROR_LEN]
    try:
        with conn.cursor() as cur:
            cur.execute(
                """
                UPDATE transcription_jobs
                SET    status = 'failed',
                       error  = %s
                WHERE  id = %s
                """,
                (truncated, job_id),
            )
        conn.commit()
        log.error("Job %s marked failed: %s", job_id, truncated)
    except Exception as exc:
        log.error("Failed to mark job %s as failed: %s", job_id, exc)
        try:
            conn.rollback()
        except Exception:
            pass


# ---------------------------------------------------------------------------
# Audio probe helper (duration + format metadata — single ffprobe call)
# ---------------------------------------------------------------------------


def _sanitize_probe_text(value: Any, max_len: int) -> str:
    """
    Sanitize a free-text ffprobe field (codec_name/format_name) for safe logging
    and DB storage.

    ffprobe JSON is derived from an untrusted, producer-supplied audio file, so
    these fields could carry control characters (newlines/CR → log-forging or
    DB-injection-style noise) or be unboundedly long. We replace any control
    character (incl. \\n, \\r, \\t) with a space and truncate to ``max_len``.
    This never fails the probe — bad input is sanitized, not rejected.
    """
    text = str(value or "")
    cleaned = "".join(" " if ord(ch) < 0x20 or ord(ch) == 0x7F else ch for ch in text)
    cleaned = cleaned.strip()
    if len(cleaned) > max_len:
        cleaned = cleaned[:max_len]
    if cleaned != text:
        log.warning(
            "Sanitized suspicious ffprobe field (control chars or overlength): %r -> %r",
            text[:120],
            cleaned,
        )
    return cleaned


def _audio_probe(audio_path: Path) -> dict[str, Any]:
    """
    Return audio metadata via a single ffprobe call.

    Returned dict keys:
        duration       float   — seconds
        channels       int     — number of audio channels (1=mono, 2=stereo, …)
        sample_rate    int     — samples per second (e.g. 44100)
        codec_name     str     — codec short name (e.g. "aac", "mp3", "pcm_s16le")
        format_name    str     — container format (e.g. "mov,mp4,m4a,3gp,3g2,mj2")
        size_bytes     int     — file size in bytes (from format.size)
    """
    result = subprocess.run(
        [
            "ffprobe",
            "-v", "quiet",
            "-print_format", "json",
            "-show_format",
            "-show_streams",
            "-select_streams", "a:0",  # first audio stream only
            str(audio_path),
        ],
        capture_output=True,
        text=True,
        check=True,
    )
    data = json.loads(result.stdout)

    fmt = data.get("format", {})
    streams = data.get("streams", [{}])
    stream = streams[0] if streams else {}

    duration = float(fmt.get("duration") or stream.get("duration") or 0.0)
    channels = int(stream.get("channels") or 1)
    sample_rate = int(stream.get("sample_rate") or 0)
    # codec_name/format_name are free-text fields from untrusted ffprobe JSON
    # (the audio file is producer-supplied). Sanitize before they reach logs or
    # the DB: strip control characters (newlines/CR/etc. → log/DB injection) and
    # truncate to a sane length. Don't fail the probe on suspicious input —
    # sanitize and continue (warn if anything was changed).
    codec_name = _sanitize_probe_text(stream.get("codec_name"), max_len=50)
    format_name = _sanitize_probe_text(fmt.get("format_name"), max_len=100)
    size_bytes = int(fmt.get("size") or 0)

    return {
        "duration": duration,
        "channels": channels,
        "sample_rate": sample_rate,
        "codec_name": codec_name,
        "format_name": format_name,
        "size_bytes": size_bytes,
    }


def _audio_duration(audio_path: Path) -> float:
    """Return duration in seconds. Thin wrapper over _audio_probe for back-compat."""
    return _audio_probe(audio_path)["duration"]


# ---------------------------------------------------------------------------
# NeMo helpers (module-level, called by NeMoParakeetProvider)
# ---------------------------------------------------------------------------


def _to_mono_wav(audio_path: Path) -> Path:
    """
    Downmix any audio file to a 16 kHz mono WAV in a temp file and return its
    path.  The caller is responsible for deleting it (use a try/finally).

    This is the canonical ffmpeg downmix call shared by:
      - the single-pass path (stereo crash fix: NeMo crashes on multi-channel
        input with shape [1, C, N] where C > 1; must be [1, 1, N] mono), and
      - _cut_window (each chunk window also runs through this conversion so
        both paths emit identically-normalised audio to the model).

    ``-ac 1`` forces mono downmix; ``-ar 16000`` resamples to 16 kHz (the
    sample rate Parakeet-TDT was trained on).  Using ``-y`` (overwrite) is safe
    because the target is a fresh temp file.
    """
    fd, tmp = tempfile.mkstemp(prefix="asr-mono-", suffix=".wav", dir="/tmp")
    os.close(fd)
    try:
        subprocess.run(
            [
                "ffmpeg", "-y", "-loglevel", "error",
                "-i", str(audio_path),
                "-ac", "1",
                "-ar", "16000",
                tmp,
            ],
            check=True,
        )
    except BaseException:
        # ffmpeg failed (or was interrupted) — the temp file would otherwise
        # leak because the caller's finally only runs once we return the path.
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    return Path(tmp)


def _mean_word_confidence(segments: list[dict[str, Any]]) -> float | None:
    """
    Mean of per-word ``score`` across all segments, or None when the model emits
    no per-word confidence.

    CONTRACT §1.5 / §2.13: ``mean_word_confidence`` is written only when the model
    produces per-word scores; NULL ("unknown") otherwise. NeMo Parakeet-TDT does
    NOT emit alignment confidence — every word's ``score`` is None (see
    _build_segments) — so this returns None for the current backend. The
    computation is generic so a future scoring backend reusing this code path
    populates the column automatically.
    """
    scores = [
        w["score"]
        for seg in segments
        for w in seg.get("words", [])
        if w.get("score") is not None
    ]
    if not scores:
        return None
    return sum(scores) / len(scores)


def _build_capability_descriptor(
    segments: list[dict[str, Any]],
    speaker_count: int | None,
    *,
    requested_biasing: bool,
    applied_biasing: bool,
    requested_diarization: bool,
) -> dict[str, Any]:
    """
    Derive the CONTRACT §2.13 backend descriptor (caps_applied, caps_requested,
    caps_skipped_reason, mean_word_confidence) from the ACTUAL run, not assumptions.

    Capability truth for NeMo Parakeet-TDT (established from this module's code):
      - word_timestamps   APPLIED: Parakeet-TDT emits native per-word timestamps
        (_build_segments builds words[] with start/end); always requested
        (asr_model.transcribe(..., timestamps=True) on every path).
      - context_biasing   APPLIED iff word-boosting actually ran this job
        (provider's ``active_terms`` = bias_terms present AND ASR_BIASING_ENABLED);
        REQUESTED iff the book had non-empty bias_terms. requested-but-not-applied
        → caps_skipped_reason (e.g. ASR_BIASING_ENABLED is off).
      - diarization       APPLIED iff Sortformer ran AND produced speakers
        (speaker_count not None); REQUESTED iff ASR_DIARIZE.
      - confidence_scores APPLIED is always false — the TDT decoder emits no
        per-word alignment confidence (every words[].score is None). Phase 1 never
        *requests* it, so no skipped-reason is recorded.
      - language_detection APPLIED is always false — Parakeet-TDT is fixed-English
        ("en"), not auto-detected. Not requested in Phase 1.

    caps_requested only carries keys the job actually asked for (omitted = not
    requested, per §2.13). caps_skipped_reason only carries requested-but-declined
    keys.
    """
    word_timestamps_applied = any(seg.get("words") for seg in segments)
    diarization_applied = requested_diarization and speaker_count is not None

    caps_applied: dict[str, bool] = {
        "word_timestamps": word_timestamps_applied,
        "context_biasing": applied_biasing,
        "diarization": diarization_applied,
        "confidence_scores": False,  # TDT decoder emits no per-word confidence
        "language_detection": False,  # fixed-English, not auto-detected
    }

    # caps_requested: only keys the job asked for. word_timestamps is requested on
    # every job (timestamps=True is unconditional). context_biasing/diarization are
    # per-job. confidence_scores / language_detection are not requested in Phase 1.
    caps_requested: dict[str, bool] = {"word_timestamps": True}
    if requested_biasing:
        caps_requested["context_biasing"] = True
    if requested_diarization:
        caps_requested["diarization"] = True

    # caps_skipped_reason: requested-but-not-applied keys → short reason.
    caps_skipped_reason: dict[str, str] = {}
    if requested_biasing and not applied_biasing:
        if not ASR_BIASING_ENABLED:
            caps_skipped_reason["context_biasing"] = (
                "ASR_BIASING_ENABLED is off; bias terms present but boosting disabled"
            )
        else:
            caps_skipped_reason["context_biasing"] = (
                "bias terms could not be applied this run"
            )
    if requested_diarization and not diarization_applied:
        caps_skipped_reason["diarization"] = (
            "diarization requested but no speakers were assigned"
        )

    return {
        "asr_family": ASR_FAMILY,
        "asr_runtime": ASR_RUNTIME,
        "caps_applied": caps_applied,
        "caps_requested": caps_requested,
        # Omit the column entirely (→ NULL) when no requested cap was skipped, so
        # the dashboard's honest-degradation tooltip only fires on real skips.
        "caps_skipped_reason": caps_skipped_reason or None,
        "mean_word_confidence": _mean_word_confidence(segments),
    }


def _transcribe_file(
    audio_path: Path,
    asr_model: Any,
    diarize_model: Any | None,
    *,
    requested_biasing: bool = False,
    applied_biasing: bool = False,
) -> dict[str, Any]:
    """
    Run NeMo Parakeet-TDT on one audio file.

    Returns a dict matching the transcript shape in CONTRACT.md §1.2.1:
      language, duration_seconds, speaker_count, segments, raw_text

    plus runner-side metrics fields (see run_metrics table in CONTRACT.md §1.5):
      transcribe_started_at, transcribe_finished_at, chunked, n_windows,
      char_count, word_count, segment_count, audio_channels, audio_sample_rate,
      audio_codec, audio_format, audio_bytes

    and the §2.13 backend descriptor:
      asr_family, asr_runtime, caps_applied, caps_requested,
      caps_skipped_reason, mean_word_confidence

    Parakeet-TDT emits native word-level timestamps — no separate alignment
    stage is needed (the WhisperX wav2vec alignment step is eliminated).

    score is always null: Parakeet-TDT (TDT decoder) does not produce
    per-word alignment confidence scores. CONTRACT.md §1.2.1 explicitly
    permits null for score.

    ``requested_biasing`` / ``applied_biasing`` are passed by the provider so the
    capability descriptor records what was asked for vs what actually ran:
    requested = the book had non-empty bias_terms; applied = boosting was actually
    applied to the model this job. They default false for the plain/back-compat
    call path.
    """
    log.info("Transcribing %s", audio_path)

    # Single ffprobe call: duration AND format metadata (channels, sample_rate,
    # codec, format, size) — used for both the threshold decision and run_metrics.
    probe = _audio_probe(audio_path)
    duration_seconds = probe["duration"]
    log.info(
        "Audio duration: %.1f s  channels=%d  sample_rate=%d  codec=%s",
        duration_seconds,
        probe["channels"],
        probe["sample_rate"],
        probe["codec_name"],
    )

    # Single-pass vs chunked inference (threshold + VRAM rationale documented
    # at CHUNK_THRESHOLD_SECONDS). Files at/under the threshold transcribe in one
    # pass; longer files take the split-and-stitch chunked path to stay within
    # VRAM (single-pass OOMs above ~18 min on the 32 GB 5090).
    #   - timestamps=True is supported for TDT models in NeMo 2.7 (verified).
    #   - batch_size=1 is safe; activation memory, not batch, is the limiter.

    # transcribe_started_at/finished_at define a TOTAL transcription wall-clock
    # that INCLUDES audio preprocessing (ffmpeg downmix on the single-pass path,
    # per-window cuts on the chunked path), not model-inference-only. The chunked
    # path inherently measures this — it interleaves _cut_window + inference in a
    # loop, so there is no clean inference-only boundary to isolate. We deliberately
    # start the single-pass clock here, BEFORE _to_mono_wav, so both paths report
    # the same metric definition. Keep this consistent with CONTRACT §1.5.
    transcribe_started_at = datetime.now(timezone.utc)

    if duration_seconds <= CHUNK_THRESHOLD_SECONDS:
        # Single-pass path: downmix to 16 kHz mono first.
        # NeMo Parakeet-TDT crashes with "Input shape … torch.Size([1, 2, N])"
        # when given a stereo (or any multi-channel) file directly. The chunked
        # path is immune because _cut_window always runs ffmpeg -ac 1 -ar 16000.
        # Fix: convert via _to_mono_wav so the model always receives shape [1, 1, N].
        mono_path = _to_mono_wav(audio_path)
        try:
            hypotheses = asr_model.transcribe(
                [str(mono_path)],
                batch_size=1,
                timestamps=True,
            )
        finally:
            try:
                mono_path.unlink()
            except OSError:
                pass
        chunked = False
        n_windows = 1
    else:
        log.info(
            "File exceeds %.0f s threshold — using split-and-stitch chunked "
            "inference",
            CHUNK_THRESHOLD_SECONDS,
        )
        hypotheses, n_windows = _transcribe_chunked(asr_model, audio_path, duration_seconds)
        chunked = True

    transcribe_finished_at = datetime.now(timezone.utc)

    # NeMo transcribe() returns a list of Hypothesis objects (one per file).
    # MUST VERIFY ON DESKTOP-1: the Hypothesis attribute names below.
    # In NeMo 2.7, Hypothesis.text is the transcript string.
    # Hypothesis.timestamp is a dict with keys 'word' and 'segment', each
    # a list of dicts.  The exact key names may vary — check nemo source:
    #   nemo/collections/asr/parts/utils/rnnt_utils.py  (Hypothesis dataclass)
    if not hypotheses:
        raise RuntimeError("NeMo transcribe() returned empty hypotheses list")

    hyp = hypotheses[0]

    # Build contract-compliant output from NeMo hypothesis.
    segments, speaker_count = _build_segments(hyp, diarize_model, audio_path)

    raw_text = " ".join(seg["text"].strip() for seg in segments)

    # Compute text statistics for run_metrics.
    word_count = sum(len(seg.get("words", [])) for seg in segments)
    char_count = len(raw_text)
    segment_count = len(segments)

    # CONTRACT §2.13 backend descriptor — what this run actually did vs requested.
    descriptor = _build_capability_descriptor(
        segments,
        speaker_count,
        requested_biasing=requested_biasing,
        applied_biasing=applied_biasing,
        requested_diarization=ASR_DIARIZE,
    )

    return {
        # CONTRACT §1.2.1 transcript fields
        "language": TRANSCRIPT_LANGUAGE,
        "duration_seconds": duration_seconds,
        "speaker_count": speaker_count,
        "segments": segments,
        "raw_text": raw_text,
        # run_metrics fields (CONTRACT §1.5)
        "transcribe_started_at": transcribe_started_at,
        "transcribe_finished_at": transcribe_finished_at,
        "chunked": chunked,
        "n_windows": n_windows,
        "char_count": char_count,
        "word_count": word_count,
        "segment_count": segment_count,
        "audio_channels": probe["channels"],
        "audio_sample_rate": probe["sample_rate"],
        "audio_codec": probe["codec_name"],
        "audio_format": probe["format_name"],
        "audio_bytes": probe["size_bytes"],
        # run_metrics §2.13 backend descriptor
        **descriptor,
    }


class _StitchedHypothesis:
    """
    Minimal stand-in for a NeMo Hypothesis, carrying exactly the two attributes
    the downstream code reads: ``.text`` and ``.timestamp`` (a dict with 'word'
    and 'segment' lists). This lets the split-and-stitch chunked path return the
    same object shape as ``model.transcribe(timestamps=True)`` so
    ``_build_segments`` consumes it identically.
    """

    def __init__(
        self,
        text: str,
        word_ts: list[dict[str, Any]],
        segment_ts: list[dict[str, Any]],
    ) -> None:
        self.text = text
        self.timestamp = {"word": word_ts, "segment": segment_ts}


def _offset_timestamps(
    entries: list[dict[str, Any]],
    offset: float,
    keep_start: float,
    keep_end: float | None,
) -> list[dict[str, Any]]:
    """
    Shift one window's per-window-relative timestamp entries to absolute file
    time and drop entries outside this window's "core" range.

    Pure function (no NeMo/torch) so it is unit-testable off-hardware.

    Args:
        entries:    list of {'word'|'segment': str, 'start': float, 'end': float}
                    with times relative to the start of this window's clip.
        offset:     absolute file time (s) of this window's clip start.
        keep_start: window-relative start (s) of the core range to keep. Entries
                    starting before this (i.e. inside the leading overlap that
                    the previous window already owns) are dropped.
        keep_end:   window-relative end (s) of the core range, or None to keep
                    everything to the end of the clip (used for the final
                    window, whose trailing overlap is real audio, not overlap).

    Returns the kept entries with 'start'/'end' rebased to absolute file time.
    """
    out: list[dict[str, Any]] = []
    for e in entries:
        rel_start = float(e.get("start", 0.0))
        if rel_start < keep_start:
            # Inside the leading overlap — owned by the previous window's core.
            continue
        if keep_end is not None and rel_start >= keep_end:
            # Inside the trailing overlap — owned by the next window's core.
            continue
        shifted = dict(e)
        shifted["start"] = rel_start + offset
        rel_end = float(e.get("end", rel_start))
        if rel_end < rel_start:
            # Defensive: NeMo 2.7.3 always emits a sane 'end', but guard against
            # model-output drift producing an inverted/zero-duration entry rather
            # than silently propagating a backwards word into the transcript.
            log.warning(
                "Malformed timestamp entry %r: end %.3f < start %.3f; clamping end=start",
                _entry_text(e),
                rel_end,
                rel_start,
            )
            rel_end = rel_start
        shifted["end"] = rel_end + offset
        out.append(shifted)
    return out


# Seam de-dup tolerance: an incoming entry is treated as a duplicate of an
# already-accepted one if it has the same text and starts within this many
# seconds of it. The model emits times on a 0.08 s frame grid and genuine
# distinct words are spaced well above 0.25 s, so this never collapses real
# words while it absorbs the ~0.1 s jitter a boundary word shows across windows.
_SEAM_DEDUP_TOLERANCE = 0.25


def _entry_text(e: dict[str, Any]) -> str:
    """Text of a word ('word') or segment ('segment') entry."""
    return e.get("word", e.get("segment", ""))


def _append_monotonic(
    acc: list[dict[str, Any]], incoming: list[dict[str, Any]]
) -> None:
    """
    Append ``incoming`` absolute-time entries onto ``acc`` in place, dropping
    seam duplicates so stitched timestamps progress forward without repeats.

    The core-range filter in _offset_timestamps removes the bulk of each
    window's trailing overlap, but a word straddling the exact cut can be
    emitted near the seam by BOTH the window before it and the window after it,
    at slightly different times (timestamp jitter). An incoming entry is dropped
    when it is non-advancing (start <= the last accepted start) OR it repeats the
    text of a recently accepted entry within _SEAM_DEDUP_TOLERANCE seconds.
    Pure function — unit-testable off-hardware.
    """
    last_start = acc[-1]["start"] if acc else float("-inf")
    for e in incoming:
        start = e["start"]
        if start <= last_start:
            continue
        # Text-aware seam de-dup: skip a near-coincident repeat of the tail of
        # acc (only need to look back over the jitter window, not all of acc).
        is_dup = False
        for prev in reversed(acc):
            if start - prev["start"] > _SEAM_DEDUP_TOLERANCE:
                break
            if _entry_text(prev) == _entry_text(e):
                is_dup = True
                break
        if is_dup:
            continue
        acc.append(e)
        last_start = start


def _transcribe_chunked(
    asr_model: Any, audio_path: Path, duration_seconds: float
) -> tuple[list[Any], int]:
    """
    Long-form transcription via split-and-stitch (see header docstring for why
    the NeMo buffered TDT decoders are not used: they return text only and do
    not surface word-level timestamps).

    Cuts the file into windows of CHUNK_WINDOW_SECONDS that each carry
    CHUNK_OVERLAP_SECONDS of trailing context, transcribes each window single-
    pass with native word/segment timestamps, then stitches the timestamps back
    to absolute file offsets. De-duplication is two-layered: the core-range
    filter (_offset_timestamps) drops each window's trailing-overlap entries, and
    a strictly-increasing-start guard (_append_monotonic) removes any residual
    boundary word that two windows both emit due to timestamp jitter.

    Returns ``(hypotheses, n_windows)`` where ``hypotheses`` is a single-element
    list ``[hyp]`` matching the ``model.transcribe()`` contract (``hyp`` exposes
    ``.text`` and ``.timestamp['word'|'segment']`` with absolute-time entries)
    and ``n_windows`` is the number of windows actually processed (used by the
    run_metrics writer).
    """
    window = CHUNK_WINDOW_SECONDS
    overlap = CHUNK_OVERLAP_SECONDS

    all_words: list[dict[str, Any]] = []
    all_segments: list[dict[str, Any]] = []

    offset = 0.0
    win_idx = 0
    n_windows = max(1, math.ceil(duration_seconds / window))
    while offset < duration_seconds:
        is_last = (offset + window) >= duration_seconds
        # Each window covers [offset, offset + window + overlap]; the trailing
        # overlap is acoustic context only (its words are owned by the next
        # window's core), except on the final window where it is real audio.
        clip_len = window + (0.0 if is_last else overlap)
        clip_path = _cut_window(audio_path, offset, clip_len, win_idx)
        try:
            log.info(
                "Chunk %d/%d: transcribing window [%.0f s, %.0f s)",
                win_idx + 1,
                n_windows,
                offset,
                offset + clip_len,
            )
            hyps = asr_model.transcribe(
                [str(clip_path)], batch_size=1, timestamps=True
            )
            if not hyps:
                raise RuntimeError(
                    f"empty hypotheses for chunk {win_idx} of {audio_path}"
                )
            ts = getattr(hyps[0], "timestamp", None) or {}
            keep_end = None if is_last else window
            _append_monotonic(
                all_words,
                _offset_timestamps(ts.get("word", []), offset, 0.0, keep_end),
            )
            _append_monotonic(
                all_segments,
                _offset_timestamps(ts.get("segment", []), offset, 0.0, keep_end),
            )
        finally:
            try:
                clip_path.unlink()
            except OSError:
                pass

        offset += window
        win_idx += 1

    raw_text = " ".join(
        (s.get("segment") or "").strip() for s in all_segments
    ).strip()
    if not raw_text:
        raw_text = " ".join(w.get("word", "") for w in all_words).strip()

    return [_StitchedHypothesis(raw_text, all_words, all_segments)], n_windows


def _cut_window(
    audio_path: Path, offset: float, length: float, win_idx: int
) -> Path:
    """
    ffmpeg-cut one window to a 16 kHz mono WAV in a temp file and return its
    path (caller deletes it).

    Uses fast input seeking (-ss before -i): ffmpeg seeks to the nearest point
    before re-decoding, so the window genuinely starts at ``offset`` seconds and
    its emitted timestamps are window-relative from 0 — which is exactly what
    _offset_timestamps rebases by ``offset``. This avoids decoding the whole
    file from the start for every window (critical for multi-hour books, where
    -ss after -i would re-decode O(n²) audio total). Validated seamless on
    RTX 5090: a 90-min split into 600 s windows produced 0 non-monotonic word
    boundaries across all seams.
    """
    fd, tmp = tempfile.mkstemp(
        prefix=f"asr-chunk-{win_idx}-", suffix=".wav", dir="/tmp"
    )
    os.close(fd)
    try:
        subprocess.run(
            [
                "ffmpeg", "-y", "-loglevel", "error",
                "-ss", str(offset),
                "-i", str(audio_path),
                "-t", str(length),
                "-ac", "1",
                "-ar", "16000",
                tmp,
            ],
            check=True,
        )
    except BaseException:
        # ffmpeg failed (or was interrupted) — the temp file would otherwise
        # leak because the caller's finally only runs once we return the path.
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    return Path(tmp)


def _build_segments(
    hyp: Any,
    diarize_model: Any | None,
    audio_path: Path,
) -> tuple[list[dict[str, Any]], int | None]:
    """
    Convert a NeMo Hypothesis into the CONTRACT.md §1.2.1 segment list.

    Word timestamps come from hyp.timestamp['word'] (NeMo 2.7 TDT models),
    accurate to ~0.1 s. Segments are ALWAYS rebuilt from those words via
    _words_to_segments — NeMo Parakeet-TDT returns exactly one coarse segment per
    transcribe() call (one per ~600 s window), so its segment-level timestamps
    carry no usable granularity and are not used for boundaries.

    Verified on desktop-1 (parakeet-tdt-1.1b, NeMo 2.7):
      - hyp.timestamp keys are ['timestep', 'char', 'word', 'segment'].
      - A 60 s multi-sentence clip yields exactly one 'segment' entry (hence the
        re-segment-from-words approach).
      - Each word entry is {'word': str, 'start': float, 'end': float} in
        seconds (not frame offsets).

    score is always null: TDT alignment does not produce confidence values.
    CONTRACT.md §1.2.1 explicitly allows null for score.
    """
    ts = getattr(hyp, "timestamp", None) or {}
    word_ts: list[dict[str, Any]] = ts.get("word", [])
    seg_ts: list[dict[str, Any]] = ts.get("segment", [])

    # ---------------------------------------------------------------------------
    # Speaker assignment (optional, gated by ASR_DIARIZE)
    # ---------------------------------------------------------------------------
    # Default: all words/segments have speaker=null (correct for single-narrator
    # audiobooks).
    word_speakers: list[str | None] = [None] * len(word_ts)
    speaker_count: int | None = None

    if diarize_model is not None and word_ts:
        word_speakers, speaker_count = _assign_speakers(
            diarize_model, audio_path, word_ts
        )

    # ---------------------------------------------------------------------------
    # Build word list (CONTRACT format)
    # ---------------------------------------------------------------------------
    words_out: list[dict[str, Any]] = []
    for i, w in enumerate(word_ts):
        words_out.append(
            {
                # MUST VERIFY ON DESKTOP-1: NeMo may use 'word' or 'char' key.
                "word": w.get("word", ""),
                "start": float(w.get("start", 0.0)),
                "end": float(w.get("end", 0.0)),
                # score is null — TDT does not emit alignment confidence.
                "score": None,
                "speaker": word_speakers[i],
            }
        )

    # ---------------------------------------------------------------------------
    # Build segment list (CONTRACT format)
    # ---------------------------------------------------------------------------
    # Always derive segments from the per-word timestamps. NeMo Parakeet-TDT
    # emits exactly ONE coarse "segment" per transcribe() call — one per ~600 s
    # window (verified on desktop-1: even a 60 s clip yields a single segment) —
    # so hyp.timestamp['segment'] carries no usable granularity. We re-segment
    # from words instead (CONTRACT §1.2.1). seg_ts is logged for diagnostics.
    log.debug(
        "re-segmenting from %d words (NeMo returned %d coarse segment(s))",
        len(word_ts),
        len(seg_ts),
    )
    segments: list[dict[str, Any]] = _words_to_segments(words_out, word_speakers)

    return segments, speaker_count


def _words_to_segments(
    words: list[dict[str, Any]],
    word_speakers: list[str | None],
) -> list[dict[str, Any]]:
    """
    Group word-level timestamps into sentence-sized segments.

    A new segment starts when any of these hold for the next word:
      - the silence gap from the previous word exceeds SEGMENT_GAP_SECONDS
        (≈ a sentence boundary in audiobook narration), or
      - the speaker changes (when diarization is active), or
      - the current segment has reached SEGMENT_MAX_SECONDS or
        SEGMENT_MAX_WORDS — a hard cap that bounds granularity in continuous,
        gap-less speech so no segment exceeds a few tens of seconds.

    Per-second precision is preserved regardless: every segment keeps its
    words[] array (CONTRACT §1.2.1).
    """
    if not words:
        return []

    segments: list[dict[str, Any]] = []
    current_words: list[dict[str, Any]] = [words[0]]

    for i in range(1, len(words)):
        prev = words[i - 1]
        curr = words[i]
        gap = curr["start"] - prev["end"]
        speaker_change = (
            word_speakers[i] is not None
            and word_speakers[i - 1] is not None
            and word_speakers[i] != word_speakers[i - 1]
        )
        # Duration of the segment-so-far (start of first word → end of prev).
        seg_duration = prev["end"] - current_words[0]["start"]
        over_cap = (
            seg_duration >= SEGMENT_MAX_SECONDS
            or len(current_words) >= SEGMENT_MAX_WORDS
        )

        if gap > SEGMENT_GAP_SECONDS or speaker_change or over_cap:
            segments.append(_flush_segment(len(segments), current_words))
            current_words = [curr]
        else:
            current_words.append(curr)

    if current_words:
        segments.append(_flush_segment(len(segments), current_words))

    return segments


def _flush_segment(
    idx: int, words: list[dict[str, Any]]
) -> dict[str, Any]:
    """Build one CONTRACT-format segment from a list of word dicts."""
    text = " ".join(w["word"] for w in words).strip()
    seg_speakers = [w["speaker"] for w in words if w["speaker"]]
    seg_speaker = max(set(seg_speakers), key=seg_speakers.count) if seg_speakers else None
    return {
        "id": idx,
        "start": words[0]["start"],
        "end": words[-1]["end"],
        "text": text,
        "speaker": seg_speaker,
        "words": words,
    }


def resegment_existing(
    segments: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    """
    Re-derive fine-grained segments from the per-word timestamps already stored
    in an existing transcript's ``segments`` JSONB — WITHOUT re-running ASR.

    Transcripts produced before fine-grained segmentation store one ~600 s
    "segment" per window, but each already carries a full words[] array with
    accurate per-word timestamps. This flattens every segment's words[] (in
    order) and re-runs _words_to_segments, yielding the same sentence-sized
    segments a fresh transcription would now produce.

    Idempotent: applying it to already-fine segments reproduces them, so the
    backfill (runner/resegment.py) is safe to re-run. Returns ``[]`` only when
    the input carries no words (callers must guard against overwriting a
    non-empty transcript with an empty result).
    """
    words: list[dict[str, Any]] = []
    for seg in segments:
        for w in seg.get("words") or []:
            words.append(
                {
                    "word": w.get("word", ""),
                    "start": float(w.get("start", 0.0)),
                    "end": float(w.get("end", 0.0)),
                    "score": w.get("score"),
                    "speaker": w.get("speaker"),
                }
            )
    word_speakers: list[str | None] = [w["speaker"] for w in words]
    return _words_to_segments(words, word_speakers)


def _assign_speakers(
    diarize_model: Any,
    audio_path: Path,
    word_ts: list[dict[str, Any]],
) -> tuple[list[str | None], int]:
    """
    Run NeMo Sortformer diarization and greedily assign a speaker label to each
    word timestamp.

    Returns (word_speakers list, speaker_count).

    MUST VERIFY ON DESKTOP-1 — this entire function is a best-effort port of
    the NeMo 2.7.x Sortformer diarization API.  In particular:
      - diarize_model.diarize() may not exist; the correct entrypoint may be
        diarize_model.transcribe() with specific kwargs, or a ClusteringDiarizer
        wrapper.  Check the NeMo 2.7.x docs for SortformerEncLabelModel.
      - The output format of diarize() — whether it returns RTTM-style turns or
        structured dicts — is unconfirmed.  Adjust _parse_diarize_output()
        accordingly.
    """
    log.info("Running NeMo Sortformer diarization on %s", audio_path)

    # MUST VERIFY ON DESKTOP-1: correct NeMo 2.7 Sortformer API.
    diar_output = diarize_model.diarize([str(audio_path)])

    # Parse diarization output into a list of (start, end, speaker) turns.
    turns: list[tuple[float, float, str]] = _parse_diarize_output(diar_output)

    # Greedy word→speaker assignment: each word gets the speaker whose turn
    # overlaps the word midpoint.
    word_speakers: list[str | None] = []
    for w in word_ts:
        mid = (w.get("start", 0.0) + w.get("end", 0.0)) / 2.0
        assigned: str | None = None
        for t_start, t_end, speaker in turns:
            if t_start <= mid <= t_end:
                assigned = speaker
                break
        word_speakers.append(assigned)

    all_speakers = {s for s in word_speakers if s is not None}
    speaker_count = len(all_speakers) if all_speakers else 0
    return word_speakers, speaker_count


def _parse_diarize_output(diar_output: Any) -> list[tuple[float, float, str]]:
    """
    Parse NeMo Sortformer diarization output into (start, end, speaker) tuples.

    MUST VERIFY ON DESKTOP-1: the actual output format of
    SortformerEncLabelModel.diarize() in NeMo 2.7.x.  This function makes a
    reasonable guess based on NeMo 2.6 RTTM-style output but may need changes.

    Expected input: list of dicts with keys 'start', 'end', 'speaker' per turn,
    OR a list of RTTM-format line strings.  Adjust as needed.
    """
    turns: list[tuple[float, float, str]] = []

    if not diar_output:
        return turns

    # Handle list-of-dict format (preferred NeMo structured output).
    if isinstance(diar_output, list):
        for item in diar_output:
            if isinstance(item, dict):
                start = float(item.get("start", item.get("start_sec", 0.0)))
                end = float(item.get("end", item.get("end_sec", 0.0)))
                speaker = str(item.get("speaker", item.get("label", "SPEAKER_00")))
                turns.append((start, end, speaker))
            elif isinstance(item, str):
                # RTTM-format line: SPEAKER <file> <ch> <start> <dur> ...
                parts = item.strip().split()
                if len(parts) >= 6 and parts[0] == "SPEAKER":
                    start = float(parts[3])
                    duration = float(parts[4])
                    speaker = parts[7] if len(parts) > 7 else "SPEAKER_00"
                    turns.append((start, start + duration, speaker))

    return turns


def _resolve_audio_path(file_path: str) -> Path:
    """Map a DB file_path to this host's local file under BOOKS_MOUNT.

    file_path is recorded by the Go producer rooted at its container books mount
    (BOOKS_DB_ROOT, default /books), e.g. "/books/audio-custom/Author/Book.m4b".
    This host mounts the same NFS share at BOOKS_MOUNT, so an absolute file_path
    is re-rooted from BOOKS_DB_ROOT onto BOOKS_MOUNT; a relative file_path (the
    CONTRACT's nominal form) is joined to BOOKS_MOUNT directly.

    Naively doing `BOOKS_MOUNT / file_path` is wrong: pathlib discards the base
    when the right operand is absolute, so a "/books/…" path would resolve to
    "/books/…" and miss the NFS mount entirely.

    The result is validated to stay under BOOKS_MOUNT, which also rejects any
    ".." traversal in the stored path.
    """
    if not file_path:
        # Path("") is Path(".") → would resolve to BOOKS_MOUNT itself (a dir, not
        # an audio file); reject explicitly rather than fail confusingly in ffprobe.
        raise ValueError("file_path must not be empty")

    p = Path(file_path)
    if p.is_absolute():
        try:
            rel = p.relative_to(BOOKS_DB_ROOT)
        except ValueError as exc:
            raise ValueError(
                f"file_path {file_path!r} is absolute but not under "
                f"BOOKS_DB_ROOT={BOOKS_DB_ROOT}"
            ) from exc
    else:
        rel = p

    audio_path = (BOOKS_MOUNT / rel).resolve()
    if not audio_path.is_relative_to(BOOKS_MOUNT.resolve()):
        raise ValueError(
            f"file_path {file_path!r} escapes BOOKS_MOUNT (path traversal)"
        )
    return audio_path


# ---------------------------------------------------------------------------
# Run metrics writer (best-effort — must never fail the core transcript write)
# ---------------------------------------------------------------------------

_RUNNER_HOST: str = os.environ.get("HOSTNAME", "") or socket.gethostname()

_METRICS_UPSERT_SQL = """
    INSERT INTO run_metrics (
        job_id, audio_bytes, audio_channels, audio_sample_rate, audio_codec,
        audio_format, transcribe_started_at, transcribe_finished_at, asr_model,
        compute_type, runner_host, chunked, n_windows, char_count, word_count,
        segment_count,
        asr_family, asr_runtime, caps_applied, caps_requested,
        caps_skipped_reason, mean_word_confidence
    ) VALUES (
        %s, %s, %s, %s, %s,
        %s, %s, %s, %s,
        %s, %s, %s, %s, %s, %s,
        %s,
        %s, %s, %s::jsonb, %s::jsonb,
        %s::jsonb, %s
    )
    ON CONFLICT (job_id) DO UPDATE SET
        audio_bytes            = EXCLUDED.audio_bytes,
        audio_channels         = EXCLUDED.audio_channels,
        audio_sample_rate      = EXCLUDED.audio_sample_rate,
        audio_codec            = EXCLUDED.audio_codec,
        audio_format           = EXCLUDED.audio_format,
        transcribe_started_at  = EXCLUDED.transcribe_started_at,
        transcribe_finished_at = EXCLUDED.transcribe_finished_at,
        asr_model              = EXCLUDED.asr_model,
        compute_type           = EXCLUDED.compute_type,
        runner_host            = EXCLUDED.runner_host,
        chunked                = EXCLUDED.chunked,
        n_windows              = EXCLUDED.n_windows,
        char_count             = EXCLUDED.char_count,
        word_count             = EXCLUDED.word_count,
        segment_count          = EXCLUDED.segment_count,
        asr_family             = EXCLUDED.asr_family,
        asr_runtime            = EXCLUDED.asr_runtime,
        caps_applied           = EXCLUDED.caps_applied,
        caps_requested         = EXCLUDED.caps_requested,
        caps_skipped_reason    = EXCLUDED.caps_skipped_reason,
        mean_word_confidence   = EXCLUDED.mean_word_confidence,
        updated_at             = now()
"""


def _json_or_none(value: Any) -> str | None:
    """Serialize a dict to a JSON string for a ``%s::jsonb`` bind, or None.

    psycopg2 binds Python None → SQL NULL, and ``NULL::jsonb`` is NULL — so an
    omitted/empty descriptor field stays NULL ("unknown") in the column rather
    than the JSON literal ``null``.
    """
    if value is None:
        return None
    return json.dumps(value)


def _write_run_metrics(
    conn: psycopg2.extensions.connection,
    job_id: str,
    result: dict[str, Any],
) -> None:
    """
    Write per-run metrics into the ``run_metrics`` table (best-effort).

    CONTRACT §1.5 — the runner owns these columns:
        audio_bytes, audio_channels, audio_sample_rate, audio_codec,
        audio_format, transcribe_started_at, transcribe_finished_at,
        asr_model, compute_type, runner_host, chunked, n_windows,
        char_count, word_count, segment_count,
        and the §2.13 backend descriptor: asr_family, asr_runtime,
        caps_applied, caps_requested, caps_skipped_reason, mean_word_confidence.

    The embed_* columns are written by the embedding worker, not here.

    This call happens AFTER _mark_done has committed, so the transcript write
    is already durable.  The entire function is wrapped in try/except so any
    failure (missing table, schema mismatch, network drop) is logged as a
    warning and swallowed — it MUST NOT roll back the transcript or raise.

    Failure isolation is by try/except + rollback, not an explicit SAVEPOINT:
    the single INSERT … ON CONFLICT is followed by conn.commit() on success; on
    any exception the except clause rolls the connection back, discarding the
    failed statement and leaving the connection clean for the next job. The
    transcript was already committed by _mark_done before this call, so a rollback
    here can never undo it — there is no partial write either way (one statement,
    then commit-or-rollback).
    """
    try:
        with conn.cursor() as cur:
            cur.execute(
                _METRICS_UPSERT_SQL,
                (
                    job_id,
                    result.get("audio_bytes"),
                    result.get("audio_channels"),
                    result.get("audio_sample_rate"),
                    result.get("audio_codec"),
                    result.get("audio_format"),
                    result.get("transcribe_started_at"),
                    result.get("transcribe_finished_at"),
                    ASR_MODEL_ID,
                    ASR_COMPUTE_TYPE,
                    _RUNNER_HOST,
                    result.get("chunked"),
                    result.get("n_windows"),
                    result.get("char_count"),
                    result.get("word_count"),
                    result.get("segment_count"),
                    # §2.13 backend descriptor. family/runtime come from the
                    # module-level env-derived constants (so they are populated
                    # even if a result dict predates the descriptor); the caps_*
                    # JSONB shapes and mean_word_confidence come from the result.
                    result.get("asr_family", ASR_FAMILY),
                    result.get("asr_runtime", ASR_RUNTIME),
                    _json_or_none(result.get("caps_applied")),
                    _json_or_none(result.get("caps_requested")),
                    _json_or_none(result.get("caps_skipped_reason")),
                    result.get("mean_word_confidence"),
                ),
            )
        conn.commit()
        log.info("run_metrics written for job %s", job_id)
    except Exception as exc:  # noqa: BLE001
        log.warning(
            "run_metrics write failed for job %s (non-fatal): %s", job_id, exc
        )
        try:
            conn.rollback()
        except Exception:
            pass


# ---------------------------------------------------------------------------
# ASRProvider protocol — the backend seam
# ---------------------------------------------------------------------------


class ASRProvider(abc.ABC):
    """
    Abstract base class for ASR backends.

    Implementors must provide:
      - load()       Load the model(s) once at startup (called before the main loop).
      - transcribe() Run inference on one audio file; return the CONTRACT §1.2.1
                     result dict (same shape as _transcribe_file).
      - capabilities() Return the set of feature strings this backend supports.
                       Used for future feature-gating (e.g. "biasing" in PR 5).

    The ``bias_terms`` parameter on ``transcribe`` carries per-book domain terms
    read from book_metadata.bias_terms at claim time.  Implementations that do
    not support biasing MUST accept the parameter and silently ignore it (do not
    raise).
    """

    @abc.abstractmethod
    def load(self) -> None:
        """Load model(s).  Called once at startup before the main loop."""

    @abc.abstractmethod
    def transcribe(
        self,
        audio_path: Path,
        bias_terms: list[str] | None = None,
    ) -> dict[str, Any]:
        """
        Transcribe *audio_path* and return a CONTRACT §1.2.1 result dict.

        The dict must contain at minimum:
            language, duration_seconds, speaker_count, segments, raw_text,
            transcribe_started_at, transcribe_finished_at, chunked, n_windows,
            char_count, word_count, segment_count,
            audio_channels, audio_sample_rate, audio_codec, audio_format,
            audio_bytes.

        ``bias_terms`` is a list of domain-specific phrases to boost (from
        book_metadata.bias_terms).  Backends that do not support biasing MUST
        accept the parameter and silently ignore it (do not raise).
        """

    @abc.abstractmethod
    def capabilities(self) -> set[str]:
        """
        Return the set of capability strings this backend supports.

        Currently defined capabilities:
            "word_timestamps"  — backend returns per-word start/end times.
            "biasing"          — backend supports bias_terms hint via NeMo
                                 boosting_tree / key_phrases_file; present only
                                 when ASR_BIASING_ENABLED=true.
        """

    def park(self) -> None:
        """
        Free GPU VRAM by moving the loaded model OFF the GPU (CONTRACT §1.4).

        Called at the gate between jobs (never mid-transcription) when the runner
        is paused or phase='analyze', so another GPU tenant (the eval judge) can
        use the card. Default is a no-op for CPU-only backends. Implementations
        must be idempotent (the caller only calls this on a state change, but a
        redundant call must not corrupt state).
        """

    def unpark(self) -> None:
        """
        Restore the model TO the GPU after a park (CONTRACT §1.4).

        The inverse of park(); default no-op. Must be idempotent and never leave
        the model half-moved.
        """


# ---------------------------------------------------------------------------
# NeMo Parakeet-TDT provider
# ---------------------------------------------------------------------------


def _apply_boosting_config(
    model: Any,
    phrases_path: str,
    alpha: float,
) -> None:
    """
    Mutate *model*'s decoding config in-place to enable boosting_tree word-boosting.

    Uses the GLOBAL boosting_tree path fed a key_phrases FILE — the only NeMo path
    that biases AND preserves TDT word-level timestamps.  Verified recipe (RTX 5090,
    NeMo 2.7.3):

      - boosting_tree on model.cfg.decoding.greedy is set with context_score=1.0
        and depth_scaling=2.0 (MUST be 2.0 for TDT; 1.0 is Canary-only).
      - boosting_tree_alpha carries the per-call weight (typically 2–3).
      - open_dict(model.cfg.decoding.greedy) is REQUIRED to unlock OmegaConf's
        struct mode before mutating; without it OmegaConf raises ConfigAttributeError.
      - model.change_decoding_strategy(model.cfg.decoding) re-initialises the TDT
        decoder from the mutated config so the change takes effect.

    TRAPS avoided:
      - per-stream path (enable_per_stream_biasing) CRASHES TDT word-timestamp
        computation (_compute_offsets_tdt: NoneType) — do NOT use it.
      - inline key_phrases_list=[...] silently NO-OPS — must be a FILE.
      - use_triton=True enables GPU-accelerated trie traversal (safe on RTX 5090).
    """
    from omegaconf import open_dict  # type: ignore[import]
    from nemo.collections.asr.parts.context_biasing.boosting_graph_batched import (  # type: ignore[import]
        BoostingTreeModelConfig,
    )

    with open_dict(model.cfg.decoding.greedy):
        model.cfg.decoding.greedy.boosting_tree = BoostingTreeModelConfig(
            key_phrases_file=phrases_path,
            context_score=1.0,
            depth_scaling=2.0,  # MUST be 2.0 for TDT (1.0 = Canary-only)
            use_triton=True,
        )
        model.cfg.decoding.greedy.boosting_tree_alpha = alpha

    model.change_decoding_strategy(model.cfg.decoding)


def _clear_boosting_config(model: Any) -> None:
    """
    Remove the boosting_tree config from *model* so subsequent un-biased calls
    are unaffected.

    Sets boosting_tree to None and boosting_tree_alpha to 0.0, then re-initialises
    the decoder via change_decoding_strategy.  Called in a finally block so it runs
    even if the transcription raises.

    Exceptions are logged and re-raised: silently swallowing them would leave the
    model in a stale biased state and silently corrupt the next un-biased job's
    transcription — a worse outcome than failing the current job loudly.
    """
    try:
        from omegaconf import open_dict  # type: ignore[import]

        with open_dict(model.cfg.decoding.greedy):
            model.cfg.decoding.greedy.boosting_tree = None
            model.cfg.decoding.greedy.boosting_tree_alpha = 0.0

        model.change_decoding_strategy(model.cfg.decoding)
    except Exception as exc:
        log.error(
            "Failed to clear boosting config — model state may be corrupted; "
            "re-raising so the job fails loudly rather than silently poisoning "
            "subsequent jobs: %s",
            exc,
        )
        raise


class NeMoParakeetProvider(ASRProvider):
    """
    ASRProvider backed by NVIDIA NeMo Parakeet-TDT (the original implementation).

    Model loading and transcription logic are identical to the pre-refactor
    _load_models / _transcribe_file functions — this class is a pure relocation
    of that logic behind the ASRProvider interface.

    When ASR_BIASING_ENABLED is true and bias_terms is non-empty, transcribe()
    applies the NeMo boosting_tree / key_phrases_file word-boosting recipe
    (verified on RTX 5090, NeMo 2.7.3) before calling the model, then restores
    a clean config afterward.  When disabled or bias_terms is empty/None, plain
    transcription is used (byte-identical to pre-PR-5 behavior).
    """

    def __init__(self) -> None:
        self._asr_model: Any = None
        self._diarize_model: Any | None = None
        self._parked: bool = False

    def load(self) -> None:
        """
        Load NeMo Parakeet-TDT once at startup.

        Parakeet-TDT 0.6B v3 is a public model — no HF token required.
        NeMo Sortformer (diarization) is also public — no token required.

        MUST VERIFY ON DESKTOP-1: NeMo downloads models to ~/.cache/huggingface
        or a NeMo-specific cache directory on first run.  Ensure the runner user
        has write access to that directory.
        """
        import torch

        if not torch.cuda.is_available():
            raise RuntimeError(
                "CUDA not available — check NVIDIA driver and nvidia-open-dkms."
            )

        log.info(
            "Loading NeMo ASR model=%s compute_type=%s",
            ASR_MODEL_ID,
            ASR_COMPUTE_TYPE,
        )

        # Import here so the module is only required at runtime (not at import
        # time, which helps with syntax-only validation without NeMo installed).
        from nemo.collections.asr.models import ASRModel  # type: ignore[import]

        asr_model = ASRModel.from_pretrained(ASR_MODEL_ID)
        asr_model = asr_model.cuda()

        # Apply compute type.
        # MUST VERIFY ON DESKTOP-1: bfloat16 is the recommended dtype for
        # Blackwell (RTX 5090 / Ampere+).  NeMo 2.7.x supports model.to(dtype)
        # but some model internals may not support bf16 — verify no warnings.
        if ASR_COMPUTE_TYPE == "bfloat16":
            asr_model = asr_model.to(torch.bfloat16)
        elif ASR_COMPUTE_TYPE == "float16":
            asr_model = asr_model.to(torch.float16)
        # float32 / default: leave as-is

        asr_model.eval()

        diarize_model: Any | None = None
        if ASR_DIARIZE:
            # NeMo Sortformer speaker diarization — public model, no token needed.
            # MUST VERIFY ON DESKTOP-1: the exact model id and the NeMo 2.7.x
            # API for loading a SortformerEncLabelModel.  The from_pretrained call
            # and the diarize() API shape are correct per NeMo 2.7 docs but require
            # runtime confirmation (we cannot test NeMo on this Mac).
            log.info("Loading NeMo Sortformer diarization model")
            from nemo.collections.asr.models import SortformerEncLabelModel  # type: ignore[import]

            diarize_model = SortformerEncLabelModel.from_pretrained(
                "nvidia/diar_sortformer_4spk-v1"
            )
            diarize_model = diarize_model.cuda().eval()

        log.info("Models loaded successfully")
        self._asr_model = asr_model
        self._diarize_model = diarize_model

    def transcribe(
        self,
        audio_path: Path,
        bias_terms: list[str] | None = None,
    ) -> dict[str, Any]:
        """
        Transcribe *audio_path* using NeMo Parakeet-TDT.

        When ASR_BIASING_ENABLED is true and *bias_terms* is non-empty, applies
        the boosting_tree word-boosting recipe to the model before inference:
          1. Writes bias_terms to a temp file (one term per line).
          2. Calls _apply_boosting_config() to mutate model.cfg.decoding.greedy
             and re-initialise the TDT decoder via change_decoding_strategy().
          3. Runs transcription (single-pass or chunked — model config is already
             set, so both paths pick up the boosting automatically).
          4. Clears the boosting config in a finally block so subsequent un-biased
             calls are unaffected.

        When disabled (ASR_BIASING_ENABLED=false) or bias_terms is empty/None,
        the plain path is taken — no model mutation, byte-identical output.
        """
        if self._asr_model is None:
            raise RuntimeError(
                "NeMoParakeetProvider.load() must be called before transcribe()"
            )

        active_terms = bias_terms if (bias_terms and ASR_BIASING_ENABLED) else None

        # Capability descriptor signals (CONTRACT §2.13):
        #   requested_biasing = the book had non-empty bias_terms (it asked for it),
        #     regardless of whether boosting is enabled on this runner.
        #   applied_biasing   = boosting was actually applied this run (active_terms).
        requested_biasing = bool(bias_terms)

        if not active_terms:
            # Plain path: boosting not applied (disabled or no terms). Still record
            # whether biasing was *requested* so a requested-but-skipped book is
            # legible in caps_skipped_reason.
            return _transcribe_file(
                audio_path,
                self._asr_model,
                self._diarize_model,
                requested_biasing=requested_biasing,
                applied_biasing=False,
            )

        # Write bias terms to a temp file (one per line) and apply boosting config.
        # The model config is mutated BEFORE _transcribe_file so both the single-pass
        # and chunked paths inside it pick up the boosting without needing to know
        # about it — they just call asr_model.transcribe() as usual.
        #
        # Explicit 0o600: mkstemp guarantees owner-only by default, but an operator
        # umask misconfiguration could widen those bits.  Bias terms are domain
        # terminology from book metadata — defense-in-depth keeps them private.
        fd, phrases_path = tempfile.mkstemp(
            prefix="asr-bias-", suffix=".txt", dir="/tmp"
        )
        os.chmod(phrases_path, 0o600)
        try:
            with os.fdopen(fd, "w") as fh:
                fh.write("\n".join(active_terms) + "\n")

            log.info(
                "Applying word-boosting: %d term(s), alpha=%.2f",
                len(active_terms),
                ASR_BIASING_ALPHA,
            )
            _apply_boosting_config(self._asr_model, phrases_path, ASR_BIASING_ALPHA)
            try:
                return _transcribe_file(
                    audio_path,
                    self._asr_model,
                    self._diarize_model,
                    requested_biasing=requested_biasing,
                    applied_biasing=True,
                )
            finally:
                _clear_boosting_config(self._asr_model)
        finally:
            try:
                os.unlink(phrases_path)
            except OSError:
                pass

    def capabilities(self) -> set[str]:
        """Return capabilities supported by this backend."""
        caps = {"word_timestamps"}
        if ASR_BIASING_ENABLED:
            caps.add("biasing")
        return caps

    def park(self) -> None:
        """
        Move the loaded model(s) to host RAM and release the GPU cache.

        Parks weights in CPU memory (~seconds) — NOT a from-disk reload — so the
        next unpark() is fast. Idempotent: a no-op if no model is loaded or the
        model is already parked. Calls torch.cuda.empty_cache() so the freed VRAM
        is actually returned to the driver (so the eval judge can allocate it).
        """
        if self._asr_model is None or self._parked:
            return
        import torch

        self._asr_model.cpu()
        if self._diarize_model is not None:
            self._diarize_model.cpu()
        torch.cuda.empty_cache()
        self._parked = True
        log.info("freed GPU, model parked to CPU")

    def unpark(self) -> None:
        """
        Move the parked model(s) back onto the GPU.

        Idempotent: a no-op if no model is loaded or the model is not parked.
        """
        if self._asr_model is None or not self._parked:
            return
        self._asr_model.cuda()
        if self._diarize_model is not None:
            self._diarize_model.cuda()
        self._parked = False
        log.info("model restored to GPU")


# ---------------------------------------------------------------------------
# Provider factory
# ---------------------------------------------------------------------------


def _make_provider(backend: str) -> ASRProvider:
    """
    Instantiate the ASRProvider for the requested backend name.

    Currently supported:
        "nemo-parakeet"  — NeMoParakeetProvider (default)

    Raises ValueError for unrecognised backend names so a misconfigured
    ASR_BACKEND env var fails loudly at startup rather than silently.
    """
    if backend == "nemo-parakeet":
        return NeMoParakeetProvider()
    raise ValueError(
        f"Unknown ASR_BACKEND {backend!r}. Supported: 'nemo-parakeet'"
    )


# ---------------------------------------------------------------------------
# Main loop
# ---------------------------------------------------------------------------


def main() -> None:
    log.info(
        "asr-runner starting: identity=%s backend=%s model=%s diarize=%s compute=%s",
        RUNNER_IDENTITY,
        ASR_BACKEND,
        ASR_MODEL_ID,
        ASR_DIARIZE,
        ASR_COMPUTE_TYPE,
    )

    # Instantiate and load the provider once at startup (model load is expensive;
    # amortized over all jobs in the queue).
    provider = _make_provider(ASR_BACKEND)
    provider.load()

    while not _shutdown.is_set():
        conn: psycopg2.extensions.connection | None = None
        try:
            conn = _connect()

            # Liveness heartbeat every cycle — working, idle, or parked/paused —
            # so "alive but idle" is distinguishable from "down" (Go exposes it as
            # earmark_runner_alive_seconds). Best-effort; never blocks the claim.
            _runner_heartbeat(conn)

            # Gate the GPU first (CONTRACT §1.4): when paused or phase='analyze'
            # the model parks off the GPU so the eval judge can use the card; when
            # active again it is restored. _gate_gpu applies the park/unpark only
            # on a state change and only here at the gate between jobs — never
            # mid-transcription — and isolates any GPU fault from the claim path.
            # When it returns True the model is parked (or could not be reclaimed):
            # skip the claim entirely, there's no point claiming a job we won't run.
            if _gate_gpu(conn, provider):
                conn.close()
                _shutdown.wait(POLL_INTERVAL)
                continue

            job = _claim_job(conn)
        except Exception as exc:
            log.error("DB error during claim: %s", exc)
            if conn is not None:
                try:
                    conn.rollback()
                    conn.close()
                except Exception:
                    pass
            _shutdown.wait(POLL_INTERVAL)
            continue

        if job is None:
            conn.close()
            log.debug("No pending jobs — sleeping %ds", POLL_INTERVAL)
            _shutdown.wait(POLL_INTERVAL)
            continue

        job_id: str = str(job["id"])
        file_path: str = job["file_path"]
        checksum: str = job["checksum"]

        log.info("Claimed job %s (file: %s)", job_id, file_path)

        # Look up bias_terms for this book before starting the heartbeat thread.
        # This read uses the same connection as the claim (already committed), so
        # it is a clean new transaction.  Best-effort: a DB error here logs and
        # returns None so the job proceeds without biasing rather than failing.
        bias_terms: list[str] | None = None
        if ASR_BIASING_ENABLED and "biasing" in provider.capabilities():
            bias_terms = _fetch_bias_terms(conn, file_path)

        # Start heartbeat thread with its OWN connection so concurrent commits
        # cannot split the _mark_done transaction (psycopg2 thread-safety).
        stop_hb = threading.Event()

        def _hb_loop() -> None:
            hb_conn: psycopg2.extensions.connection | None = None
            try:
                hb_conn = _connect()
                while not stop_hb.wait(HEARTBEAT_INTERVAL):
                    _heartbeat(hb_conn, job_id)
            except Exception as exc:
                log.warning("Heartbeat thread failed to open connection: %s", exc)
            finally:
                if hb_conn is not None:
                    try:
                        hb_conn.close()
                    except Exception:
                        pass

        hb_thread = threading.Thread(target=_hb_loop, daemon=True)
        hb_thread.start()

        try:
            # Re-root the producer's BOOKS_DB_ROOT-relative file_path onto this
            # host's BOOKS_MOUNT; this also rejects DB-sourced paths that escape
            # the books mount (path traversal). Computed inside the try so a bad
            # path marks the job failed rather than crashing the loop.
            audio_path = _resolve_audio_path(file_path)
            if not audio_path.exists():
                raise FileNotFoundError(
                    f"Audio file not found at {audio_path}. "
                    f"Check BOOKS_MOUNT={BOOKS_MOUNT} and NFS mount."
                )

            result = provider.transcribe(audio_path, bias_terms=bias_terms)
            _mark_done(conn, job_id, file_path, checksum, result)
            # Best-effort metrics write — AFTER the transcript commit.
            # _write_run_metrics catches all exceptions internally and never
            # raises, so a missing run_metrics table (sibling schema PR not yet
            # deployed) or any other failure cannot affect the transcript write.
            _write_run_metrics(conn, job_id, result)

        except Exception as exc:
            log.exception("Transcription failed for job %s", job_id)
            _mark_failed(conn, job_id, str(exc))
        finally:
            stop_hb.set()
            hb_thread.join(timeout=5)
            try:
                conn.close()
            except Exception:
                pass

    log.info("asr-runner shut down cleanly")


if __name__ == "__main__":
    main()
