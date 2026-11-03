#!/usr/bin/env python3
"""
pilot.py — one-shot second-ASR pilot for earmark (CONTRACT C2 of the ASR pilot).

Re-transcribes the tracks of a few books with a second ASR model and stores the
result ALONGSIDE production in ``pilot_transcripts`` (migration 00017). It never
touches transcription_jobs, runner_control, transcripts, transcript_chunks or
embeddings: the only write is one INSERT (plus a DELETE of the same key under
--force) per finished track, in one transaction. A track that does not finish
writes nothing.

Backends (ONLY these two). Both are window-level text: one segment per window,
words without start/end, has_word_times=false, casing and punctuation verbatim.
The asr_vote step aligns that text against the production (parakeet) word times.
  granite  ibm-granite/granite-speech-4.1-2b (base) via transformers (>=5.8),
           bf16, plain ASR prompt with punctuation and capitalization.
  canary   nvidia/canary-qwen-2.5b via NeMo SALM (speechlm2), <=40 s windows.

Windows of a track, and across tracks, are transcribed in batches
(--batch-size); a CUDA OOM halves the batch and retries it.

This file is self-contained: it does NOT import runner.py (importing runner.py
has side effects — required DATABASE_URL, signal handlers). The few helpers it
needs (books path re-rooting) are copied and kept small.

CLI:
    python pilot.py --backend {granite,canary} --asin A1,A2 [--scope full|findings]
                    [--limit-tracks N] [--track-limit-per-book N] [--force]
                    [--batch-size N] [--repetition-penalty P]
                    [--dry-run] [--out-dir DIR] [--arbiter-url URL] [--self-check]
                    [--bench [--bench-batches 1,8,16,32] [--bench-max-sec S]]

Environment:
    DATABASE_URL          required (except --self-check). earmark Postgres DSN.
    PILOT_MODEL_ID        default per backend (see DEFAULT_MODEL_IDS).
    PILOT_MODEL_REVISION  required: the HF commit sha to load. Unpinned runs are refused.
    HF_HOME               HF cache (the pilot's own, not production's).
    BOOKS_MOUNT           as runner.py: this host's books mount (UNC on Windows).
    BOOKS_DB_ROOT         as runner.py: default /books.
    BOOKS_PATH_ENCODING   as runner.py: none | sfm.
    FFMPEG_BIN            default "ffmpeg" (runner.py resolves ffmpeg from PATH).
    FFPROBE_BIN           default "ffprobe" (only --self-check uses it).
    PILOT_WINDOW_SEC      target window; granite default 240 (hard max 540),
                          canary default 38 (hard max 40).
    PILOT_DEVICE          default cuda.
    PILOT_STOP_FILE       optional path; if it exists (checked before each track,
                          decode chunk and batch) the run stops cleanly with exit 5.
    PILOT_OUT_DIR         per-track JSON dir when --out-dir is not given
                          (default <tempdir>/earmark-asr-pilot-out).

Exit codes: 0 ok, 2 config error, 3 some tracks failed, 4 stopped for gaming,
5 stopped by PILOT_STOP_FILE.
"""

from __future__ import annotations

import argparse
import bisect
import contextlib
import hashlib
import importlib.util
import json
import logging
import math
import os
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from collections.abc import Callable, Iterator, Sequence
from dataclasses import dataclass, field
from pathlib import Path, PurePath, PurePosixPath, PureWindowsPath
from typing import Any

log = logging.getLogger("earmark-asr-pilot")

EXIT_OK = 0
EXIT_CONFIG = 2
EXIT_TRACKS_FAILED = 3
EXIT_GAMING = 4
EXIT_STOPPED = 5

SAMPLE_RATE = 16000

BACKENDS = ("granite", "canary")
SCOPES = ("full", "findings")

DEFAULT_MODEL_IDS: dict[str, str] = {
    "granite": "ibm-granite/granite-speech-4.1-2b",
    "canary": "nvidia/canary-qwen-2.5b",
}
# Target window length (s) and the hard maximum no window may exceed.
# granite: the 4.1 family is evaluated up to 9 min per segment for plain ASR;
# canary: trained on <=40 s audio (1024-token sequence cap).
DEFAULT_WINDOW_SEC: dict[str, float] = {"granite": 240.0, "canary": 38.0}
HARD_MAX_WINDOW_SEC: dict[str, float] = {"granite": 540.0, "canary": 40.0}
MIN_WINDOW_SEC = 5.0
DEFAULT_BATCH_SIZE: dict[str, int] = {"granite": 16, "canary": 32}
DEFAULT_BENCH_BATCHES = "1,8,16,32"
DEFAULT_BENCH_MAX_SEC = 1800.0

# Cut search: the largest inter-word gap inside the last 25% of the target window.
CUT_SEARCH_FRACTION = 0.25
# findings scope: pad each held finding's chunk span by this much each side.
FINDING_PAD_SEC = 5.0

# Granite generation (model card, granite-speech-4.1-2b "Preferred prompt by task").
GRANITE_PROMPT = (
    "<|audio|>transcribe the speech with proper punctuation and capitalization."
)
GRANITE_TOKENS_PER_SECOND = 12
GRANITE_MIN_NEW_TOKENS = 64
# Canary generation (model card, canary-qwen-2.5b).
CANARY_PROMPT_TEMPLATE = "Transcribe the following: {tag}"
CANARY_TOKENS_PER_SECOND = 8
CANARY_MIN_NEW_TOKENS = 128
# max_new_tokens = max(min, tokens/s x longest window in the batch x margin).
TOKEN_MARGIN = 1.25

# Repetition-loop guard: an n-gram repeated back-to-back this many times marks
# the window degenerate (single words need a longer run: "no, no, no" is speech).
REPEAT_MIN_RUNS: dict[int, int] = {1: 8, 2: 5, 3: 4, 4: 4, 5: 4, 6: 4}
GRANITE_SILENCE_TOKEN = "_"  # never emitted in plain mode; kept out of the guard

ARBITER_TIMEOUT_SEC = 5.0
ARBITER_RETRY_DELAY_SEC = 2.0  # one retry of an unreadable /status
ARBITER_BUSY_STATES = frozenset({"gaming", "evicting"})

MAX_TRACK_ROWS = 5000  # bound on the track-list query
MAX_FINDING_SPANS = 5000  # bound on the per-track findings query

# Audio is decoded per group of contiguous windows of at most
# max(DECODE_CHUNK_MIN_SEC, batch_size x window_sec) seconds, so one chunk holds
# a full batch and memory stays bounded whatever the track length (granite
# 16 x 240 s = 64 min ~ 250 MB of float32 at 16 kHz).
DECODE_CHUNK_MIN_SEC = 1800.0


class ConfigError(Exception):
    """Bad CLI/env configuration (exit 2)."""


class GamingStop(Exception):
    """The arbiter reports a gaming tenant: stop now, write nothing partial (exit 4)."""


class StopRequested(Exception):
    """PILOT_STOP_FILE exists: stop now, write nothing partial (exit 5)."""


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------


@dataclass
class Config:
    backend: str
    asins: list[str]
    scope: str = "full"
    limit_tracks: int | None = None
    track_limit_per_book: int | None = None
    force: bool = False
    dry_run: bool = False
    out_dir: Path | None = None
    arbiter_url: str | None = None
    self_check: bool = False
    bench: bool = False
    bench_batches: list[int] = field(default_factory=lambda: [1, 8, 16, 32])
    bench_max_sec: float = DEFAULT_BENCH_MAX_SEC
    batch_size: int = 1
    repetition_penalty: float = 1.0
    stop_file: Path | None = None
    database_url: str = ""
    model_id: str = ""
    model_revision: str = ""
    window_sec: float = 0.0
    hard_max_sec: float = 0.0
    device: str = "cuda"
    ffmpeg_bin: str = "ffmpeg"
    ffprobe_bin: str = "ffprobe"
    books_mount: PurePath = field(default_factory=lambda: Path("/mnt/media/books"))
    books_db_root: PurePosixPath = field(
        default_factory=lambda: PurePosixPath("/books")
    )
    books_path_encoding: str = "none"

    @property
    def decode_chunk_sec(self) -> float:
        return max(DECODE_CHUNK_MIN_SEC, self.batch_size * self.window_sec)


def _positive_int(raw: str) -> int:
    value = int(raw)
    if value < 1:
        raise argparse.ArgumentTypeError(f"must be >= 1, got {value}")
    return value


def _batch_list(raw: str) -> list[int]:
    try:
        values = [int(x) for x in raw.split(",") if x.strip()]
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"not a list of integers: {raw!r}") from exc
    if not values or any(v < 1 for v in values):
        raise argparse.ArgumentTypeError(f"batch sizes must be >= 1: {raw!r}")
    return values


def build_arg_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="pilot.py",
        description="One-shot second-ASR pilot: writes pilot_transcripts only.",
    )
    p.add_argument("--backend", choices=BACKENDS, required=True)
    p.add_argument(
        "--asin", default="", help="comma-separated ASINs (book_metadata.asin)"
    )
    p.add_argument(
        "--scope",
        choices=SCOPES,
        default="full",
        help="full (default): whole tracks; findings: only windows around held findings",
    )
    p.add_argument("--limit-tracks", type=_positive_int, default=None)
    p.add_argument("--track-limit-per-book", type=_positive_int, default=None)
    p.add_argument("--force", action="store_true", help="re-run tracks already stored")
    p.add_argument(
        "--batch-size",
        type=_positive_int,
        default=None,
        help="windows per generate call (granite 16, canary 32)",
    )
    p.add_argument(
        "--repetition-penalty",
        type=float,
        default=1.0,
        help="granite generate() repetition_penalty (default 1.0 = off)",
    )
    p.add_argument(
        "--dry-run", action="store_true", help="print the plan; no model, no writes"
    )
    p.add_argument(
        "--out-dir", type=Path, default=None, help="per-track JSON directory"
    )
    p.add_argument(
        "--arbiter-url",
        default=None,
        help="gpu-arbiter base URL (checked between batches)",
    )
    p.add_argument(
        "--self-check",
        action="store_true",
        help="load the model, transcribe 5 s of silence",
    )
    p.add_argument(
        "--bench",
        action="store_true",
        help="sweep --bench-batches on one track; print JSON, no DB writes",
    )
    p.add_argument("--bench-batches", type=_batch_list, default=DEFAULT_BENCH_BATCHES)
    p.add_argument(
        "--bench-max-sec",
        type=float,
        default=DEFAULT_BENCH_MAX_SEC,
        help="bench at most this much audio of the track (default 1800)",
    )
    return p


def _parse_books_path_encoding(raw: str | None) -> str:
    value = (raw or "none").strip().lower() or "none"
    if value not in ("none", "sfm"):
        raise ConfigError(
            f"BOOKS_PATH_ENCODING={raw!r} is invalid; expected none or sfm"
        )
    return value


def load_config(
    argv: Sequence[str] | None, env: dict[str, str] | None = None
) -> Config:
    """Parse CLI + env into a Config. Raises ConfigError on anything invalid."""
    env = dict(os.environ if env is None else env)
    try:
        args = build_arg_parser().parse_args(argv)
    except SystemExit as exc:  # argparse exits 2 on bad flags; keep our own code path
        if exc.code == 0:  # --help
            raise
        raise ConfigError("invalid command line") from exc

    backend = args.backend
    asins = [a.strip() for a in args.asin.split(",") if a.strip()]
    if not args.self_check and not asins:
        raise ConfigError("--asin is required (comma-separated ASINs)")

    revision = env.get("PILOT_MODEL_REVISION", "").strip()
    if not revision:
        raise ConfigError(
            "PILOT_MODEL_REVISION is required (HF commit sha); refusing to run unpinned"
        )

    database_url = env.get("DATABASE_URL", "").strip()
    if not args.self_check and not database_url:
        raise ConfigError("DATABASE_URL is required")

    hard_max = HARD_MAX_WINDOW_SEC[backend]
    raw_window = env.get("PILOT_WINDOW_SEC", "").strip()
    try:
        window = float(raw_window) if raw_window else DEFAULT_WINDOW_SEC[backend]
    except ValueError as exc:
        raise ConfigError(f"PILOT_WINDOW_SEC={raw_window!r} is not a number") from exc
    if not math.isfinite(window) or window < MIN_WINDOW_SEC or window > hard_max:
        raise ConfigError(
            f"PILOT_WINDOW_SEC={window} out of range for {backend}: "
            f"[{MIN_WINDOW_SEC}, {hard_max}]"
        )
    if not math.isfinite(args.repetition_penalty) or args.repetition_penalty <= 0:
        raise ConfigError(
            f"--repetition-penalty must be > 0: {args.repetition_penalty}"
        )
    if args.self_check and (args.dry_run or args.bench):
        raise ConfigError("--self-check cannot be combined with --dry-run or --bench")
    if args.bench and args.dry_run:
        raise ConfigError("--bench and --dry-run are exclusive")
    bench_batches = (
        args.bench_batches
        if isinstance(args.bench_batches, list)
        else _batch_list(args.bench_batches)
    )

    mount_raw = env.get("BOOKS_MOUNT", "/mnt/media/books")
    return Config(
        backend=backend,
        asins=asins,
        scope=args.scope,
        limit_tracks=args.limit_tracks,
        track_limit_per_book=args.track_limit_per_book,
        force=args.force,
        dry_run=args.dry_run,
        out_dir=args.out_dir
        or Path(
            env.get("PILOT_OUT_DIR", "").strip()
            or Path(tempfile.gettempdir()) / "earmark-asr-pilot-out"
        ),
        arbiter_url=(args.arbiter_url or "").rstrip("/") or None,
        self_check=args.self_check,
        bench=args.bench,
        bench_batches=bench_batches,
        bench_max_sec=args.bench_max_sec,
        batch_size=args.batch_size or DEFAULT_BATCH_SIZE[backend],
        repetition_penalty=args.repetition_penalty,
        stop_file=Path(env["PILOT_STOP_FILE"].strip())
        if env.get("PILOT_STOP_FILE", "").strip()
        else None,
        database_url=database_url,
        model_id=env.get("PILOT_MODEL_ID", "").strip() or DEFAULT_MODEL_IDS[backend],
        model_revision=revision,
        window_sec=window,
        hard_max_sec=hard_max,
        device=env.get("PILOT_DEVICE", "").strip() or "cuda",
        ffmpeg_bin=env.get("FFMPEG_BIN", "").strip() or "ffmpeg",
        ffprobe_bin=env.get("FFPROBE_BIN", "").strip() or "ffprobe",
        books_mount=Path(os.path.abspath(mount_raw)),
        books_db_root=PurePosixPath(env.get("BOOKS_DB_ROOT", "/books")),
        books_path_encoding=_parse_books_path_encoding(env.get("BOOKS_PATH_ENCODING")),
    )


# ---------------------------------------------------------------------------
# Books path re-rooting (copied from runner.py: _map_db_path & co.)
# ---------------------------------------------------------------------------

_SFM_ENCODE_TABLE: dict[int, str] = {
    **{c: chr(0xF000 + c) for c in range(0x01, 0x20)},
    ord('"'): "\uf020",
    ord("*"): "\uf021",
    ord(":"): "\uf022",
    ord("<"): "\uf023",
    ord(">"): "\uf024",
    ord("?"): "\uf025",
    ord("\\"): "\uf026",
    ord("|"): "\uf027",
}


def map_db_path(
    file_path: str, mount: PurePath, db_root: PurePosixPath, encoding: str = "none"
) -> PurePath:
    """Lexically re-root a POSIX DB file_path from db_root onto mount (runner.py _map_db_path)."""
    if not file_path:
        raise ValueError("file_path must not be empty")
    p = PurePosixPath(file_path)
    if p.is_absolute():
        try:
            rel = p.relative_to(db_root)
        except ValueError as exc:
            raise ValueError(
                f"file_path {file_path!r} is not under BOOKS_DB_ROOT={db_root}"
            ) from exc
    else:
        rel = p
    flavour = type(mount)
    out: list[str] = []
    for part in rel.parts:
        if part == "..":
            raise ValueError(
                f"file_path {file_path!r} escapes BOOKS_MOUNT (path traversal)"
            )
        name = part.translate(_SFM_ENCODE_TABLE) if encoding == "sfm" else part
        parsed = flavour(name)
        if parsed.anchor or parsed.parts != (name,):
            raise ValueError(
                f"file_path {file_path!r} has component {name!r} that is not a plain name"
            )
        out.append(name)
    return mount.joinpath(*out)


def _extended_length_path(path: PurePath) -> PurePath:
    r"""Windows \\?\ form so >MAX_PATH books open (runner.py _extended_length_path)."""
    if not isinstance(path, PureWindowsPath):
        return path
    text = str(path)
    if text.startswith(("\\\\?\\", "\\\\.\\")):
        return path
    if not path.is_absolute() or ".." in path.parts:
        raise ValueError(f"cannot make an extended-length path from {text!r}")
    if text.startswith("\\\\"):
        return type(path)("\\\\?\\UNC\\" + text[2:])
    return type(path)("\\\\?\\" + text)


def resolve_audio_path(cfg: Config, file_path: str) -> Path:
    mapped = Path(
        map_db_path(
            file_path, cfg.books_mount, cfg.books_db_root, cfg.books_path_encoding
        )
    )
    audio = Path(_extended_length_path(Path(_extended_length_path(mapped)).resolve()))
    root = Path(_extended_length_path(Path(cfg.books_mount).resolve()))
    if not audio.is_relative_to(root):
        raise ValueError(f"file_path {file_path!r} escapes BOOKS_MOUNT: {audio}")
    return audio


# ---------------------------------------------------------------------------
# Window planning (pure)
# ---------------------------------------------------------------------------

Span = tuple[float, float]


class Gaps:
    """Inter-word gaps of the production words, indexed by midpoint for bisect."""

    def __init__(self, words: Sequence[Span]) -> None:
        gaps = sorted(
            ((words[i][1] + words[i + 1][0]) / 2.0, words[i + 1][0] - words[i][1])
            for i in range(len(words) - 1)
            if words[i + 1][0] > words[i][1]
        )
        self.mids = [m for m, _ in gaps]
        self.lens = [g for _, g in gaps]

    def best(self, lo: float, hi: float) -> float | None:
        """Midpoint of the largest gap whose midpoint lies in [lo, hi], or None."""
        i = bisect.bisect_left(self.mids, lo)
        j = bisect.bisect_right(self.mids, hi)
        if i >= j:
            return None
        k = max(range(i, j), key=self.lens.__getitem__)
        return self.mids[k]


def _gaps(words: Sequence[Span] | Gaps) -> Gaps:
    return words if isinstance(words, Gaps) else Gaps(words)


def plan_windows(
    start: float,
    end: float,
    words: Sequence[Span] | Gaps,
    target: float,
    hard_max: float,
) -> list[Span]:
    """Split [start, end) into windows of at most `target` (<= hard_max) seconds.

    Each cut is placed at the midpoint of the largest inter-word gap (from the
    PRODUCTION word timestamps) whose midpoint lies in the last
    CUT_SEARCH_FRACTION of the target window, so no word is split. With no usable
    gap there (or no production words at all) the cut falls back to a fixed cut
    at start + target. A tail shorter than MIN_WINDOW_SEC is folded into the
    previous window when the merged window still fits in hard_max.
    """
    target = min(target, hard_max)
    if end <= start:
        return []
    gaps = _gaps(words)
    windows: list[Span] = []
    pos = start
    while end - pos > target:
        hi = pos + target
        best = gaps.best(pos + target * (1.0 - CUT_SEARCH_FRACTION), hi)
        cut = best if best is not None else hi
        windows.append((pos, cut))
        pos = cut
    windows.append((pos, end))
    if (
        len(windows) > 1
        and end - pos < MIN_WINDOW_SEC
        and end - windows[-2][0] <= hard_max
    ):
        windows[-2:] = [(windows[-2][0], end)]
    return windows


def merge_spans(
    spans: Sequence[Span],
    pad: float,
    duration: float,
    words: Sequence[Span] | Gaps = (),
) -> list[Span]:
    """Pad each span by `pad`, snap, clamp to [0, duration], merge overlapping ones.

    Each padded edge is snapped to the largest production inter-word gap inside
    its pad (between the padded edge and the original one), so a window never
    starts or ends inside a word when a gap is available; otherwise the padded
    edge is kept.
    """
    gaps = _gaps(words)
    padded: list[Span] = []
    for s, e in spans:
        if e < s:
            continue
        lo, hi = max(0.0, s - pad), min(duration, e + pad)
        snap_lo = gaps.best(lo, min(s, hi))
        snap_hi = gaps.best(max(e, lo), hi)
        padded.append(
            (
                snap_lo if snap_lo is not None else lo,
                snap_hi if snap_hi is not None else hi,
            )
        )
    padded.sort()
    merged: list[list[float]] = []
    for s, e in padded:
        if e <= s:
            continue
        if merged and s <= merged[-1][1]:
            merged[-1][1] = max(merged[-1][1], e)
        else:
            merged.append([s, e])
    return [(s, e) for s, e in merged]


def plan_track(
    duration: float,
    words: Sequence[Span],
    target: float,
    hard_max: float,
    scope: str = "full",
    finding_spans: Sequence[Span] = (),
) -> list[Span]:
    """Whole-track plan (full) or the merged held-finding spans, each windowed (findings)."""
    gaps = Gaps(words)
    if scope == "full":
        return plan_windows(0.0, duration, gaps, target, hard_max)
    plan: list[Span] = []
    for s, e in merge_spans(finding_spans, FINDING_PAD_SEC, duration, gaps):
        plan.extend(plan_windows(s, e, gaps, target, hard_max))
    return plan


def group_windows(plan: Sequence[Span], max_sec: float) -> list[list[Span]]:
    """Group contiguous windows into decode chunks of at most max_sec each.

    A window longer than max_sec gets its own group (windows are <= 210 s, so
    that does not happen with the defaults).
    """
    groups: list[list[Span]] = []
    for w in plan:
        if (
            groups
            and abs(groups[-1][-1][1] - w[0]) < 1e-9
            and w[1] - groups[-1][0][0] <= max_sec
        ):
            groups[-1].append(w)
        else:
            groups.append([w])
    return groups


def production_word_spans(segments: Any) -> list[Span]:
    """(start, end) of every timed production word, sorted by start."""
    out: list[Span] = []
    for seg in segments or []:
        for w in (seg or {}).get("words") or []:
            s, e = w.get("start"), w.get("end")
            if isinstance(s, (int, float)) and isinstance(e, (int, float)):
                out.append((float(s), float(e)))
    out.sort()
    return out


def has_repetition_loop(tokens: Sequence[str]) -> bool:
    """True when some n-gram repeats back-to-back REPEAT_MIN_RUNS[n] times."""
    toks = [
        t.lower().strip(".,;:!?\"'") for t in tokens if t and t != GRANITE_SILENCE_TOKEN
    ]
    n_tok = len(toks)
    for n, min_runs in REPEAT_MIN_RUNS.items():
        if n * min_runs > n_tok:
            continue
        for i in range(n_tok - n * min_runs + 1):
            gram = toks[i : i + n]
            runs = 1
            j = i + n
            while j + n <= n_tok and toks[j : j + n] == gram:
                runs += 1
                j += n
                if runs >= min_runs:
                    return True
    return False


# ---------------------------------------------------------------------------
# Output (pure)
# ---------------------------------------------------------------------------


def window_segment(
    text: str, start: float, end: float, seg_id: int
) -> dict[str, Any] | None:
    """One segment per window; words carry NO start/end keys (no timestamps)."""
    text = (text or "").strip()
    if not text:
        return None
    return {
        "id": seg_id,
        "start": round(start, 3),
        "end": round(end, 3),
        "text": text,
        "speaker": None,
        "words": [{"word": t, "score": None, "speaker": None} for t in text.split()],
    }


def degenerate_reason(text: str, hit_token_limit: bool) -> str | None:
    """Why a window's output is unusable, or None."""
    if not (text or "").strip():
        return "empty"
    if hit_token_limit:
        return "token_limit"
    if has_repetition_loop(text.split()):
        return "repetition"
    return None


def max_new_tokens(
    tokens_per_second: float, minimum: int, window_lens: Sequence[float]
) -> int:
    """Generation cap for a batch: sized by its LONGEST window."""
    longest = max(window_lens, default=0.0)
    return max(minimum, int(tokens_per_second * longest * TOKEN_MARGIN))


def pad_audios(audios: Sequence[Any]) -> tuple[list[Any], list[int]]:
    """Zero-pad windows to the longest one; returns (padded rows, original lengths).

    Works on numpy arrays (np.pad) or plain sequences (tests).
    """
    lens = [len(a) for a in audios]
    longest = max(lens, default=0)
    rows: list[Any] = []
    for a, n in zip(audios, lens):
        if hasattr(a, "dtype"):
            import numpy as np

            rows.append(np.pad(a, (0, longest - n)))
        else:
            rows.append(list(a) + [0.0] * (longest - n))
    return rows, lens


def generated_rows(output: Sequence[Sequence[int]], input_len: int) -> list[list[int]]:
    """Per-row new tokens of a left-padded batch generate(): everything past input_len."""
    return [list(row[input_len:]) for row in output]


def real_token_count(row: Sequence[int], pad_ids: set[int]) -> int:
    return sum(1 for t in row if t not in pad_ids)


# ---------------------------------------------------------------------------
# Audio
# ---------------------------------------------------------------------------


def ffmpeg_timeout(duration: float) -> float:
    """Decode deadline per chunk: 2 min floor plus 5% of the chunk's real time."""
    return 120.0 + 0.05 * max(0.0, duration)


def decode_audio(
    cfg: Config, path: Path, start: float, end: float
) -> Any:  # pragma: no cover - needs ffmpeg+numpy
    """Decode [start, end) of a track to 16 kHz mono float32 (numpy) via ffmpeg.

    Streams ffmpeg's stdout into a preallocated buffer sized for the chunk (no
    capture_output buffering), so memory is bounded by the chunk length; stderr
    goes to a temp file. A watchdog kills ffmpeg past ffmpeg_timeout().
    """
    import numpy as np

    length = max(0.0, end - start)
    cmd = [
        cfg.ffmpeg_bin, "-nostdin", "-hide_banner", "-loglevel", "error",
        "-ss", f"{start:.3f}", "-i", str(path), "-t", f"{length:.3f}",
        "-vn", "-ac", "1", "-ar", str(SAMPLE_RATE), "-f", "f32le", "-",
    ]  # fmt: skip
    expected = (int(length * SAMPLE_RATE) + SAMPLE_RATE) * 4  # +1 s of slack
    buf = bytearray(expected)
    view = memoryview(buf)
    filled = 0
    with tempfile.TemporaryFile() as err:
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=err)
        timer = threading.Timer(ffmpeg_timeout(length), proc.kill)
        timer.start()
        try:
            assert proc.stdout is not None
            while True:
                if filled == len(buf):  # longer than expected: grow by 1 s
                    view.release()  # a bytearray cannot resize while exported
                    buf.extend(bytes(SAMPLE_RATE * 4))
                    view = memoryview(buf)
                n = proc.stdout.readinto(view[filled:])
                if not n:
                    break
                filled += n
            rc = proc.wait()
        finally:
            timer.cancel()
            if proc.poll() is None:
                proc.kill()
        if rc != 0:
            err.seek(0)
            msg = err.read()[-500:].decode("utf-8", "replace")
            raise RuntimeError(f"ffmpeg exit {rc} ({start:.1f}-{end:.1f} s): {msg}")
    del view
    usable = filled - filled % 4
    return np.frombuffer(buf, dtype=np.float32, count=usable // 4)


def slice_audio(audio: Any, start: float, end: float) -> Any:
    """Samples [start, end) (seconds relative to the decoded chunk), as a writable copy."""
    return audio[round(start * SAMPLE_RATE) : round(end * SAMPLE_RATE)].copy()


# ---------------------------------------------------------------------------
# gpu-arbiter and stop file
# ---------------------------------------------------------------------------


def fetch_arbiter_status(url: str) -> dict[str, Any]:
    with urllib.request.urlopen(f"{url}/status", timeout=ARBITER_TIMEOUT_SEC) as resp:
        return json.loads(resp.read().decode("utf-8"))


def gaming_active(status: dict[str, Any] | None) -> bool:
    """A game holds (or is taking) the GPU. An unreadable status counts as busy."""
    if not isinstance(status, dict):
        return True
    return str(status.get("state", "")).lower() in ARBITER_BUSY_STATES


def check_gaming(
    cfg: Config,
    fetch: Callable[[str], dict[str, Any]] = fetch_arbiter_status,
    sleep: Callable[[float], None] = time.sleep,
) -> None:
    """Raise GamingStop when a game holds the GPU.

    An unreadable status is retried once after ARBITER_RETRY_DELAY_SEC; if it is
    still unreadable it counts as gaming (fail toward stopping). A readable
    `gaming`/`evicting` state stops at once.
    """
    if not cfg.arbiter_url:
        return
    status: dict[str, Any] | None = None
    for attempt in range(2):
        try:
            got = fetch(cfg.arbiter_url)
        except (OSError, ValueError, urllib.error.URLError) as exc:
            log.warning("arbiter status unreadable (%s), attempt %d", exc, attempt + 1)
            got = None
        if isinstance(got, dict):
            status = got
            break
        if attempt == 0:
            sleep(ARBITER_RETRY_DELAY_SEC)
    if gaming_active(status):
        raise GamingStop(
            f"arbiter state={None if status is None else status.get('state')}"
        )


def check_stop_file(cfg: Config) -> None:
    if cfg.stop_file is not None and cfg.stop_file.exists():
        raise StopRequested(f"stop file {cfg.stop_file} exists")


# ---------------------------------------------------------------------------
# Database (reads bounded; the only write is pilot_transcripts)
# ---------------------------------------------------------------------------

TRACKS_SQL = """
    SELECT bm.asin, j.file_path, t.id::text, t.duration_seconds
      FROM book_metadata bm
      JOIN transcription_jobs j ON regexp_replace(j.file_path, '/[^/]+$', '') = bm.book_dir
      JOIN transcripts t ON t.job_id = j.id
     WHERE bm.asin = ANY(%s)
     ORDER BY bm.asin, j.file_path
     LIMIT %s
"""

PRODUCTION_SEGMENTS_SQL = "SELECT segments FROM transcripts WHERE id = %s::uuid"

# Held = proposed, not superseded, and the finding's LATEST live decision (one
# not revoked by a kind='revoke' event; same pattern as internal/db/decidecands.go
# decideLatestDecisionJoin) is a hold.
HELD_FINDING_SPANS_SQL = """
    SELECT DISTINCT f.start_sec, f.end_sec
      FROM transcript_findings f
      JOIN LATERAL (
            SELECT e.outcome FROM finding_events e
             WHERE e.finding_id = f.id AND e.kind = 'decision'
               AND NOT EXISTS (SELECT 1 FROM finding_events v
                                WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)
             ORDER BY e.id DESC
             LIMIT 1) ld ON true
     WHERE f.file_path = %s
       AND f.patch_state = 'proposed'
       AND f.superseded_at IS NULL
       AND f.end_sec > f.start_sec
       AND ld.outcome = 'hold'
     ORDER BY 1, 2
     LIMIT %s
"""

EXISTS_SQL = """
    SELECT 1 FROM pilot_transcripts
     WHERE file_path = %s AND model_id = %s AND model_revision = %s AND scope = %s
     LIMIT 1
"""

DELETE_SQL = """
    DELETE FROM pilot_transcripts
     WHERE file_path = %s AND model_id = %s AND model_revision = %s AND scope = %s
"""

INSERT_SQL = """
    INSERT INTO pilot_transcripts (
        file_path, production_transcript_id, backend, model_id, model_revision,
        scope, window_sec, has_word_times, segments, raw_text,
        audio_sec, wall_sec, rtfx, peak_vram_mib, peak_reserved_mib
    ) VALUES (
        %s, %s::uuid, %s, %s, %s,
        %s, %s, %s, %s::jsonb, %s,
        %s, %s, %s, %s, %s
    )
"""


@dataclass
class Track:
    asin: str
    file_path: str
    transcript_id: str
    duration: float


def connect(cfg: Config) -> Any:  # pragma: no cover - thin psycopg2 wrapper
    import psycopg2

    conn = psycopg2.connect(cfg.database_url)
    if cfg.dry_run:
        conn.set_session(readonly=True)
    return conn


class DbSession:
    """Holds the connection; `ensure()` reconnects one that died during a long track."""

    def __init__(self, conn: Any, connect_fn: Callable[[], Any]) -> None:
        self.conn = conn
        self.connect_fn = connect_fn

    def ensure(self) -> Any:
        alive = not getattr(self.conn, "closed", 0)
        if alive:
            try:
                with self.conn.cursor() as cur:
                    cur.execute("SELECT 1")
                    cur.fetchone()
                self.conn.rollback()
            except Exception as exc:  # noqa: BLE001 - any failure means reconnect
                log.warning("DB connection unusable (%s); reconnecting", exc)
                alive = False
        if not alive:
            with contextlib.suppress(Exception):
                self.conn.close()
            self.conn = self.connect_fn()
        return self.conn


def select_tracks(conn: Any, cfg: Config) -> list[Track]:
    with conn.cursor() as cur:
        cur.execute(TRACKS_SQL, (cfg.asins, MAX_TRACK_ROWS))
        rows = cur.fetchall()
    conn.rollback()  # end the read transaction
    tracks = [
        Track(str(a), str(fp), str(tid), float(d or 0.0)) for a, fp, tid, d in rows
    ]
    if cfg.track_limit_per_book:
        seen: dict[str, int] = {}
        kept: list[Track] = []
        for t in tracks:
            seen[t.asin] = seen.get(t.asin, 0) + 1
            if seen[t.asin] <= cfg.track_limit_per_book:
                kept.append(t)
        tracks = kept
    missing = sorted(set(cfg.asins) - {t.asin for t in tracks})
    if missing:
        log.warning("no transcribed tracks for ASIN(s): %s", ",".join(missing))
    return tracks


def already_done(conn: Any, cfg: Config, track: Track) -> bool:
    with conn.cursor() as cur:
        cur.execute(
            EXISTS_SQL, (track.file_path, cfg.model_id, cfg.model_revision, cfg.scope)
        )
        row = cur.fetchone()
    conn.rollback()
    return row is not None


def tracks_to_do(conn: Any, cfg: Config, tracks: Sequence[Track]) -> list[Track]:
    """Drop already-stored tracks (unless --force), then apply --limit-tracks."""
    todo = [t for t in tracks if cfg.force or not already_done(conn, cfg, t)]
    log.info("%d track(s) selected, %d to do", len(tracks), len(todo))
    return todo[: cfg.limit_tracks] if cfg.limit_tracks else todo


def read_production_words(conn: Any, track: Track) -> list[Span]:
    """One track's production word times (a single bounded row)."""
    with conn.cursor() as cur:
        cur.execute(PRODUCTION_SEGMENTS_SQL, (track.transcript_id,))
        row = cur.fetchone()
    conn.rollback()
    segments = row[0] if row else None
    if isinstance(segments, str):
        segments = json.loads(segments)
    return production_word_spans(segments)


def read_finding_spans(conn: Any, track: Track) -> list[Span]:
    with conn.cursor() as cur:
        cur.execute(HELD_FINDING_SPANS_SQL, (track.file_path, MAX_FINDING_SPANS))
        rows = cur.fetchall()
    conn.rollback()
    return [(float(s), float(e)) for s, e in rows]


def track_plan(conn: Any, cfg: Config, track: Track) -> tuple[list[Span], int]:
    """(window plan, production word count) for one track; warns on zero duration."""
    if track.duration <= 0:
        log.warning("skip %s: zero duration in transcripts", track.file_path)
        return [], 0
    words = read_production_words(conn, track)
    spans = read_finding_spans(conn, track) if cfg.scope == "findings" else []
    plan = plan_track(
        track.duration, words, cfg.window_sec, cfg.hard_max_sec, cfg.scope, spans
    )
    return plan, len(words)


def write_result(conn: Any, cfg: Config, track: Track, result: dict[str, Any]) -> None:
    """One transaction per finished track: (DELETE under --force) + INSERT, then COMMIT."""
    try:
        with conn.cursor() as cur:
            if cfg.force:
                cur.execute(
                    DELETE_SQL,
                    (track.file_path, cfg.model_id, cfg.model_revision, cfg.scope),
                )
            cur.execute(
                INSERT_SQL,
                (
                    track.file_path,
                    track.transcript_id,
                    cfg.backend,
                    cfg.model_id,
                    cfg.model_revision,
                    cfg.scope,
                    cfg.window_sec,
                    result["has_word_times"],
                    json.dumps(result["segments"]),
                    result["raw_text"],
                    result["audio_sec"],
                    result["wall_sec"],
                    result["rtfx"],
                    result["peak_vram_mib"],
                    result["peak_reserved_mib"],
                ),
            )
        conn.commit()
    except Exception:
        conn.rollback()
        raise


def _out_file(cfg: Config, track: Track) -> Path | None:
    if cfg.out_dir is None:
        return None
    digest = hashlib.sha256(track.file_path.encode("utf-8")).hexdigest()[:16]
    return cfg.out_dir / f"{track.asin}-{digest}-{cfg.backend}-{cfg.scope}.json"


# ---------------------------------------------------------------------------
# Backends (heavy imports are lazy: CI and --dry-run never import torch)
# ---------------------------------------------------------------------------


@dataclass
class WindowResult:
    text: str
    hit_token_limit: bool = False


class Backend:
    name = ""
    has_word_times = False  # both backends: window-level text only

    def __init__(self, cfg: Config) -> None:
        self.cfg = cfg
        self.torch: Any = None

    def load(self) -> None:  # pragma: no cover - needs the GPU stack
        raise NotImplementedError

    def transcribe_batch(
        self, audios: Sequence[Any]
    ) -> list[WindowResult]:  # pragma: no cover
        raise NotImplementedError

    def is_oom(self, exc: BaseException) -> bool:
        oom = getattr(getattr(self.torch, "cuda", None), "OutOfMemoryError", None)
        if oom is not None and isinstance(exc, oom):
            return True
        return "out of memory" in str(exc).lower()

    def clear_cache(self) -> None:
        if self.torch is not None and self.torch.cuda.is_available():
            self.torch.cuda.empty_cache()

    def reset_peak(self) -> None:
        if self.torch is not None and self.torch.cuda.is_available():
            self.torch.cuda.reset_peak_memory_stats()

    def peak_mib(self) -> tuple[int | None, int | None]:
        if self.torch is None or not self.torch.cuda.is_available():
            return None, None
        mib = 1024 * 1024
        return (
            int(self.torch.cuda.max_memory_allocated() // mib),
            int(self.torch.cuda.max_memory_reserved() // mib),
        )

    def info(self) -> dict[str, Any]:
        torch = self.torch
        cuda = bool(torch is not None and torch.cuda.is_available())
        vram: dict[str, Any] = {}
        if cuda:
            alloc, reserved = self.peak_mib()
            vram = {
                "device": torch.cuda.get_device_name(0),
                "total_mib": int(
                    torch.cuda.get_device_properties(0).total_memory // (1024 * 1024)
                ),
                "peak_allocated_mib": alloc,
                "peak_reserved_mib": reserved,
            }
        return {
            "backend": self.name,
            "model": self.cfg.model_id,
            "revision": self.cfg.model_revision,
            "torch": getattr(torch, "__version__", None),
            "cuda": getattr(getattr(torch, "version", None), "cuda", None)
            if cuda
            else None,
            "vram": vram,
        }


class GraniteBackend(Backend):
    """Granite Speech 4.1 2B (base), plain ASR with punctuation (usage per the HF card)."""

    name = "granite"

    def load(self) -> None:  # pragma: no cover - needs the GPU stack
        import torch
        from transformers import AutoModelForSpeechSeq2Seq, AutoProcessor

        self.torch = torch
        self.processor = AutoProcessor.from_pretrained(
            self.cfg.model_id, revision=self.cfg.model_revision
        )
        self.tokenizer = self.processor.tokenizer
        self.tokenizer.padding_side = "left"  # batched generate needs left padding
        attn = (
            "flash_attention_2"
            if importlib.util.find_spec("flash_attn") is not None
            else "sdpa"
        )
        self.model = AutoModelForSpeechSeq2Seq.from_pretrained(
            self.cfg.model_id,
            revision=self.cfg.model_revision,
            device_map=self.cfg.device,
            dtype=torch.bfloat16,
            attn_implementation=attn,
        )
        self.model.eval()
        self.attn_implementation = attn
        chat = [{"role": "user", "content": GRANITE_PROMPT}]
        self.prompt = self.tokenizer.apply_chat_template(
            chat, tokenize=False, add_generation_prompt=True
        )

    def transcribe_batch(
        self, audios: Sequence[Any]
    ) -> list[WindowResult]:  # pragma: no cover
        torch = self.torch
        # Pass the windows UNPADDED as a list: the feature extractor pads them
        # itself and derives per-row audio_embed_sizes + input_features_mask
        # from each row's real length (transformers 5.22.1,
        # GraniteSpeechFeatureExtractor._get_audios_and_audio_lengths). A
        # pre-stacked tensor would make every row the longest one's length.
        lens = [len(a) for a in audios]
        inputs = self.processor(
            [self.prompt] * len(audios),
            [torch.from_numpy(a) for a in audios],
            device=self.cfg.device,
            return_tensors="pt",
            padding=True,
        ).to(self.cfg.device)
        limit = max_new_tokens(
            GRANITE_TOKENS_PER_SECOND,
            GRANITE_MIN_NEW_TOKENS,
            [n / SAMPLE_RATE for n in lens],
        )
        with torch.inference_mode():
            out = self.model.generate(
                **inputs,
                max_new_tokens=limit,
                do_sample=False,
                num_beams=1,
                repetition_penalty=self.cfg.repetition_penalty,
            )
        new = generated_rows(out.tolist(), inputs["input_ids"].shape[-1])
        pad = {
            t
            for t in (self.tokenizer.pad_token_id, self.tokenizer.eos_token_id)
            if t is not None
        }
        texts = self.tokenizer.batch_decode(
            new, add_special_tokens=False, skip_special_tokens=True
        )
        return [
            WindowResult(
                text=t.strip(), hit_token_limit=real_token_count(r, pad) >= limit
            )
            for t, r in zip(texts, new)
        ]


class CanaryBackend(Backend):
    """Canary-Qwen 2.5B via NeMo SALM (usage per the HF card / SALM.generate docstring)."""

    name = "canary"

    def load(self) -> None:  # pragma: no cover - needs the GPU stack
        import torch
        from huggingface_hub import snapshot_download
        from nemo.collections.speechlm2.models import SALM

        self.torch = torch
        # snapshot_download pins the exact HF revision; SALM then loads the local dir.
        local = snapshot_download(self.cfg.model_id, revision=self.cfg.model_revision)
        # Weights stay in the checkpoint dtype (the card loads it as-is); bf16
        # compute comes from autocast in transcribe_batch, so the float32 audio
        # tensor SALM.generate expects still matches the perception module.
        self.model = SALM.from_pretrained(local).to(self.cfg.device).eval()

    def transcribe_batch(
        self, audios: Sequence[Any]
    ) -> list[WindowResult]:  # pragma: no cover
        torch = self.torch
        rows, lens = pad_audios(audios)
        batch = torch.stack([torch.from_numpy(r) for r in rows]).to(self.cfg.device)
        audio_lens = torch.tensor(lens, dtype=torch.int64, device=self.cfg.device)
        prompt = CANARY_PROMPT_TEMPLATE.format(tag=self.model.audio_locator_tag)
        limit = max_new_tokens(
            CANARY_TOKENS_PER_SECOND,
            CANARY_MIN_NEW_TOKENS,
            [n / SAMPLE_RATE for n in lens],
        )
        autocast = (
            torch.autocast("cuda", dtype=torch.bfloat16)
            if str(self.cfg.device).startswith("cuda")
            else contextlib.nullcontext()
        )
        with torch.inference_mode(), autocast:
            ids = self.model.generate(
                prompts=[[{"role": "user", "content": prompt}]] * len(rows),
                audios=batch,
                audio_lens=audio_lens,
                max_new_tokens=limit,
            )
        pad = {
            t
            for t in (
                getattr(self.model, "text_pad_id", None),
                getattr(self.model.tokenizer, "eos_id", None),
            )
            if t is not None
        }
        out: list[WindowResult] = []
        for row in ids.cpu():
            toks = row.tolist()
            text = self.model.tokenizer.ids_to_text(
                [t for t in toks if t not in pad]
            ).strip()
            out.append(
                WindowResult(
                    text=text, hit_token_limit=real_token_count(toks, pad) >= limit
                )
            )
        return out


def make_backend(cfg: Config) -> Backend:
    if cfg.backend == "granite":
        return GraniteBackend(cfg)
    if cfg.backend == "canary":
        return CanaryBackend(cfg)
    raise ConfigError(f"unknown backend {cfg.backend!r}")


class Batcher:
    """Runs batches through a backend; a CUDA OOM halves the batch size and retries.

    The halved size persists for the rest of the run (`size`), and the smallest
    size used is reported per track.
    """

    def __init__(self, backend: Backend, size: int) -> None:
        self.backend = backend
        self.size = size

    def run(self, audios: Sequence[Any]) -> tuple[list[WindowResult], int]:
        """Transcribe all `audios`; returns (results, batch size in effect at the end)."""
        results: list[WindowResult] = []
        i = 0
        while i < len(audios):
            part = audios[i : i + self.size]
            try:
                got = self.backend.transcribe_batch(part)
            except Exception as exc:
                if self.size > 1 and self.backend.is_oom(exc):
                    self.backend.clear_cache()
                    self.size = max(1, self.size // 2)
                    log.warning(
                        "CUDA OOM; batch size halved to %d, retrying", self.size
                    )
                    continue
                raise
            if len(got) != len(part):
                raise RuntimeError(
                    f"backend returned {len(got)} results for {len(part)} windows"
                )
            results.extend(got)
            i += len(part)
        return results, self.size


# ---------------------------------------------------------------------------
# Pipeline: windows of a track — and across tracks — go through shared batches
# ---------------------------------------------------------------------------

DecodeFn = Callable[[Config, Path, float, float], Any]


@dataclass
class TrackState:
    track: Track
    plan: list[Span]
    texts: list[WindowResult | None]
    left: int
    audio_sec: float = 0.0
    decode_sec: float = 0.0
    wall_sec: float = 0.0
    peak_alloc: int | None = None
    peak_reserved: int | None = None
    batch_size_used: int | None = None
    failed: bool = False


@dataclass
class WindowItem:
    state: TrackState
    idx: int
    start: float
    end: float
    audio: Any


def _max_opt(a: int | None, b: int | None) -> int | None:
    return b if a is None else a if b is None else max(a, b)


def build_result(cfg: Config, st: TrackState) -> dict[str, Any]:
    segments: list[dict[str, Any]] = []
    degenerate: list[dict[str, Any]] = []
    for idx, ((ws, we), res) in enumerate(zip(st.plan, st.texts)):
        assert res is not None
        reason = degenerate_reason(res.text, res.hit_token_limit)
        seg = window_segment(res.text, ws, we, len(segments))
        if reason:
            degenerate.append({"window": idx, "start": ws, "end": we, "reason": reason})
            if seg:  # optional markers; §1.2.1 readers ignore them
                seg["degenerate"] = True
                seg["degenerate_reason"] = reason
        if seg:
            segments.append(seg)
    return {
        "segments": segments,
        "raw_text": " ".join(s["text"] for s in segments),
        "has_word_times": False,
        "windows": len(st.plan),
        "audio_sec": round(st.audio_sec, 3),
        "decode_sec": round(st.decode_sec, 3),
        "wall_sec": round(st.wall_sec, 3),
        "rtfx": round(st.audio_sec / st.wall_sec, 3) if st.wall_sec > 0 else None,
        "peak_vram_mib": st.peak_alloc,
        "peak_reserved_mib": st.peak_reserved,
        "batch_size": cfg.batch_size,
        "batch_size_used": st.batch_size_used,
        "degenerate_windows": degenerate,
    }


class Pipeline:
    def __init__(
        self,
        db: DbSession,
        cfg: Config,
        backend: Backend,
        decode: DecodeFn,
        check: Callable[[], None],
        write: bool = True,
    ) -> None:
        self.db = db
        self.cfg = cfg
        self.backend = backend
        self.decode = decode
        self.check = check
        self.write = write
        self.batcher = Batcher(backend, cfg.batch_size)
        self.failed: list[str] = []
        self.done: list[dict[str, Any]] = []

    def _fail(self, st: TrackState | None, track: Track) -> None:
        log.exception("track failed: %s", track.file_path)
        if st is not None:
            st.failed = True
        self.failed.append(track.file_path)
        with contextlib.suppress(Exception):
            self.db.conn.rollback()

    def windows(
        self, tracks: Sequence[Track], max_sec: float | None = None
    ) -> Iterator[WindowItem]:
        """Yield every planned window (a writable audio copy each), one decode chunk at a time."""
        for track in tracks:
            st: TrackState | None = None
            try:
                plan, _ = track_plan(self.db.ensure(), self.cfg, track)
                if max_sec is not None:
                    plan = [w for w in plan if w[1] <= max_sec] or plan[:1]
                if not plan:
                    log.info(
                        "skip %s: nothing to transcribe (scope=%s)",
                        track.file_path,
                        self.cfg.scope,
                    )
                    continue
                self.check()
                path = resolve_audio_path(self.cfg, track.file_path)
                st = TrackState(track, plan, [None] * len(plan), len(plan))
                idx = 0
                for group in group_windows(plan, self.cfg.decode_chunk_sec):
                    self.check()
                    g0, g1 = group[0][0], group[-1][1]
                    t0 = time.monotonic()
                    audio = self.decode(self.cfg, path, g0, g1)
                    st.decode_sec += time.monotonic() - t0
                    for ws, we in group:
                        if st.failed:
                            break
                        yield WindowItem(
                            st, idx, ws, we, slice_audio(audio, ws - g0, we - g0)
                        )
                        idx += 1
                    del audio
                    if st.failed:
                        break
            except (GamingStop, StopRequested):
                raise
            except Exception:  # noqa: BLE001 - a bad track must not stop the run
                self._fail(st, track)

    def flush(self, batch: list[WindowItem]) -> None:
        batch = [it for it in batch if not it.state.failed]
        if not batch:
            return
        self.check()
        self.backend.reset_peak()
        t0 = time.monotonic()
        try:
            results, used = self.batcher.run([it.audio for it in batch])
        except Exception:  # noqa: BLE001 - fail the batch's tracks, keep going
            for st in {id(it.state): it.state for it in batch}.values():
                self._fail(st, st.track)
            return
        wall = time.monotonic() - t0
        alloc, reserved = self.backend.peak_mib()
        total = sum(it.end - it.start for it in batch) or 1.0
        for it, res in zip(batch, results):
            st = it.state
            share = (it.end - it.start) / total
            st.texts[it.idx] = res
            st.left -= 1
            st.audio_sec += it.end - it.start
            st.wall_sec += wall * share  # batch time split by audio length
            st.peak_alloc = _max_opt(st.peak_alloc, alloc)
            st.peak_reserved = _max_opt(st.peak_reserved, reserved)
            st.batch_size_used = (
                used if st.batch_size_used is None else min(st.batch_size_used, used)
            )
        for st in {id(it.state): it.state for it in batch}.values():
            if st.left == 0 and not st.failed:
                self.finish(st)

    def finish(self, st: TrackState) -> None:
        result = build_result(self.cfg, st)
        self.done.append({"track": st.track, "result": result})
        if not self.write:
            return
        try:
            out = _out_file(self.cfg, st.track)
            if out is not None:
                out.parent.mkdir(parents=True, exist_ok=True)
                out.write_text(
                    json.dumps(
                        {
                            "file_path": st.track.file_path,
                            "transcript_id": st.track.transcript_id,
                            "backend": self.cfg.backend,
                            "model_id": self.cfg.model_id,
                            "model_revision": self.cfg.model_revision,
                            "scope": self.cfg.scope,
                            "plan": st.plan,
                            **result,
                        },
                        ensure_ascii=False,
                    ),
                    encoding="utf-8",
                )
            write_result(self.db.ensure(), self.cfg, st.track, result)
        except Exception:  # noqa: BLE001 - the out-file keeps the work
            self._fail(st, st.track)
            return
        log.info(
            "done %s: %d windows, %.0f s audio, decode %.0f s, rtfx %s, peak %s MiB, batch %s, degenerate %d",
            st.track.file_path, len(st.plan), result["audio_sec"], result["decode_sec"],
            result["rtfx"], result["peak_vram_mib"], result["batch_size_used"],
            len(result["degenerate_windows"]),
        )  # fmt: skip

    def run(self, tracks: Sequence[Track], max_sec: float | None = None) -> None:
        batch: list[WindowItem] = []
        for item in self.windows(tracks, max_sec):
            if item.state.failed:
                continue
            batch.append(item)
            if len(batch) >= self.batcher.size:
                self.flush(batch)
                batch = []
        self.flush(batch)


def dry_run(
    conn: Any, cfg: Config, tracks: Sequence[Track], emit: Callable[[str], None] = print
) -> None:
    """Print the window plan of the tracks a real run would process. Reads only."""
    for t in tracks:
        plan, n_words = track_plan(conn, cfg, t)
        emit(
            json.dumps(
                {
                    "asin": t.asin,
                    "file_path": t.file_path,
                    "duration_sec": t.duration,
                    "scope": cfg.scope,
                    "production_words": n_words,
                    "batch_size": cfg.batch_size,
                    "windows": [[round(s, 2), round(e, 2)] for s, e in plan],
                },
                ensure_ascii=False,
            )
        )


def bench(
    db: DbSession,
    cfg: Config,
    backend: Backend,
    track: Track,
    decode: DecodeFn,
    emit: Callable[[str], None] = print,
) -> None:
    """Sweep cfg.bench_batches over (at most bench_max_sec of) one track. No DB writes."""
    rows: list[dict[str, Any]] = []
    for size in cfg.bench_batches:
        sub = Config(**{**cfg.__dict__, "batch_size": size})
        pipe = Pipeline(
            db, sub, backend, decode, lambda: check_stop_file(cfg), write=False
        )
        pipe.run([track], max_sec=cfg.bench_max_sec)
        if not pipe.done:
            rows.append({"batch_size": size, "error": "failed (see log)"})
            continue
        r = pipe.done[0]["result"]
        rows.append(
            {
                "batch_size": size,
                "batch_size_used": r["batch_size_used"],
                "windows": r["windows"],
                "audio_sec": r["audio_sec"],
                "wall_sec": r["wall_sec"],
                "decode_sec": r["decode_sec"],
                "rtfx": r["rtfx"],
                "peak_vram_mib": r["peak_vram_mib"],
                "peak_reserved_mib": r["peak_reserved_mib"],
                "degenerate_windows": len(r["degenerate_windows"]),
            }
        )
    emit(
        json.dumps(
            {
                "bench": {
                    "backend": cfg.backend,
                    "model_id": cfg.model_id,
                    "model_revision": cfg.model_revision,
                    "file_path": track.file_path,
                    "window_sec": cfg.window_sec,
                    "max_sec": cfg.bench_max_sec,
                    "results": rows,
                }
            },
            ensure_ascii=False,
        )
    )


def self_check(
    cfg: Config, emit: Callable[[str], None] = print
) -> int:  # pragma: no cover - GPU stack
    import numpy as np

    backend = make_backend(cfg)
    backend.load()
    backend.reset_peak()
    res = backend.transcribe_batch([np.zeros(5 * SAMPLE_RATE, dtype=np.float32)])[0]
    info = backend.info()
    info["output"] = res.text[:200]
    info["attn_implementation"] = getattr(backend, "attn_implementation", None)
    for name, binary in (("ffmpeg", cfg.ffmpeg_bin), ("ffprobe", cfg.ffprobe_bin)):
        try:
            out = subprocess.run(
                [binary, "-version"], capture_output=True, timeout=30, check=False
            )
            info[name] = (
                out.stdout.decode("utf-8", "replace").splitlines()[0]
                if out.returncode == 0
                else None
            )
        except OSError:
            info[name] = None
    emit(json.dumps(info))
    return EXIT_OK


def run(
    cfg: Config,
    conn: Any,
    backend: Backend | None = None,
    decode: DecodeFn = decode_audio,
    gaming_check: Callable[[], None] | None = None,
    connect_fn: Callable[[], Any] | None = None,
) -> int:
    db = DbSession(conn, connect_fn or (lambda: connect(cfg)))
    gaming = gaming_check or (lambda: check_gaming(cfg))

    def check() -> None:
        check_stop_file(cfg)
        gaming()

    try:
        tracks = select_tracks(db.ensure(), cfg)
        if cfg.bench:
            tracks = tracks[:1]
            if not tracks:
                log.error("--bench: no track for %s", ",".join(cfg.asins))
                return EXIT_CONFIG
        else:
            tracks = tracks_to_do(db.ensure(), cfg, tracks)
            if cfg.dry_run:
                dry_run(db.ensure(), cfg, tracks)
                return EXIT_OK
            if not tracks:
                return EXIT_OK
        try:
            check()  # before the multi-GB model load, not only after it
            if backend is None:  # pragma: no cover - real model
                backend = make_backend(cfg)
                backend.load()
            if cfg.bench:
                bench(db, cfg, backend, tracks[0], decode)
                return EXIT_OK
            pipe = Pipeline(db, cfg, backend, decode, check)
            pipe.run(tracks)
        except GamingStop as exc:
            log.warning("stopping for gaming (%s); unfinished tracks not written", exc)
            return EXIT_GAMING
        except StopRequested as exc:
            log.warning("stopping (%s); unfinished tracks not written", exc)
            return EXIT_STOPPED
        return EXIT_TRACKS_FAILED if pipe.failed else EXIT_OK
    finally:
        if db.conn is not conn:  # reconnected mid-run: the caller only closes its own
            with contextlib.suppress(Exception):
                db.conn.close()


def main(argv: Sequence[str] | None = None) -> int:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
        stream=sys.stderr,
    )
    try:
        cfg = load_config(argv)
    except ConfigError as exc:
        log.error("config error: %s", exc)
        return EXIT_CONFIG
    if cfg.self_check:  # pragma: no cover - GPU stack
        return self_check(cfg)
    conn = connect(cfg)
    try:
        return run(cfg, conn)
    finally:
        with contextlib.suppress(Exception):
            conn.close()


if __name__ == "__main__":
    sys.exit(main())
