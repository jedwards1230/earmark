#!/usr/bin/env python3
"""
pilot.py — one-shot second-ASR pilot for earmark (CONTRACT C2 of the ASR pilot).

Re-transcribes the tracks of a few books with a second ASR model and stores the
result ALONGSIDE production in ``pilot_transcripts`` (migration 00017). It never
touches transcription_jobs, runner_control, transcripts, transcript_chunks or
embeddings: the only write is one INSERT (plus a DELETE of the same key under
--force) per finished track, in one transaction. A track that does not finish
writes nothing.

Backends (ONLY these two):
  granite  ibm-granite/granite-speech-4.1-2b-plus via transformers (>=5.8),
           bf16, timestamp prompt -> words WITH start/end (lowercase, no
           punctuation, like parakeet).
  canary   nvidia/canary-qwen-2.5b via NeMo SALM (speechlm2), <=40 s windows,
           NO timestamps -> one segment per window, words without start/end;
           casing and punctuation kept verbatim.

This file is self-contained: it does NOT import runner.py (importing runner.py
has side effects — required DATABASE_URL, signal handlers). The few helpers it
needs (books path re-rooting, segmenting) are copied and kept small.

CLI:
    python pilot.py --backend {granite,canary} --asin A1,A2 [--scope full|findings]
                    [--limit-tracks N] [--track-limit-per-book N] [--force]
                    [--dry-run] [--out-dir DIR] [--arbiter-url URL] [--self-check]

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
    PILOT_WINDOW_SEC      target window; granite default 90 (hard max 210),
                          canary default 38 (hard max 40).
    PILOT_DEVICE          default cuda.

Exit codes: 0 ok, 2 config error, 3 some tracks failed, 4 stopped for gaming.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import math
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from pathlib import Path, PurePath, PurePosixPath, PureWindowsPath
from typing import Any

log = logging.getLogger("earmark-asr-pilot")

EXIT_OK = 0
EXIT_CONFIG = 2
EXIT_TRACKS_FAILED = 3
EXIT_GAMING = 4

SAMPLE_RATE = 16000

BACKENDS = ("granite", "canary")
SCOPES = ("full", "findings")

DEFAULT_MODEL_IDS: dict[str, str] = {
    "granite": "ibm-granite/granite-speech-4.1-2b-plus",
    "canary": "nvidia/canary-qwen-2.5b",
}
# Target window length (s) and the hard maximum no window may exceed.
# granite: the card says timestamps are reliable up to 3.5 min; canary: trained
# on <=40 s audio (1024-token sequence cap).
DEFAULT_WINDOW_SEC: dict[str, float] = {"granite": 90.0, "canary": 38.0}
HARD_MAX_WINDOW_SEC: dict[str, float] = {"granite": 210.0, "canary": 40.0}
MIN_WINDOW_SEC = 5.0

# Cut search: the largest inter-word gap inside the last 25% of the target window.
CUT_SEARCH_FRACTION = 0.25
# findings scope: pad each held finding's chunk span by this much each side.
FINDING_PAD_SEC = 5.0

# Segmenting (same defaults as runner.py's ASR_SEGMENT_* geometry).
SEGMENT_GAP_SECONDS = 0.6
SEGMENT_MAX_SECONDS = 30.0
SEGMENT_MAX_WORDS = 80

# Granite generation (model card, granite-speech-4.1-2b-plus).
GRANITE_SYSTEM_PROMPT = (
    "Knowledge Cutoff Date: April 2024.\nToday's Date: December 19, 2024.\n"
    "You are Granite, developed by IBM. You are a helpful AI assistant"
)
GRANITE_TIMESTAMP_PROMPT = (
    "<|audio|> Timestamps: Transcribe the speech. After each word, add a "
    "timestamp tag showing the end time in centiseconds, e.g. hello [T:45] world [T:82]"
)
GRANITE_TOKENS_PER_SECOND = 48
GRANITE_TOKEN_MARGIN = 1.1
GRANITE_MIN_NEW_TOKENS = 256
GRANITE_REPETITION_PENALTY = 2.0
GRANITE_NUM_BEAMS = 1
GRANITE_TS_WRAP_SEC = 10.0  # [T:N] keeps only the last 3 centisecond digits
GRANITE_SILENCE_TOKEN = "_"

# Canary generation (model card, canary-qwen-2.5b).
CANARY_PROMPT_TEMPLATE = "Transcribe the following: {tag}"
CANARY_TOKENS_PER_SECOND = 8
CANARY_MIN_NEW_TOKENS = 128

# Repetition-loop guard: an n-gram repeated back-to-back this many times marks
# the window degenerate (single words need a longer run: "no, no, no" is speech).
REPEAT_MIN_RUNS: dict[int, int] = {1: 8, 2: 5, 3: 4, 4: 4, 5: 4, 6: 4}

ARBITER_TIMEOUT_SEC = 5.0
ARBITER_BUSY_STATES = frozenset({"gaming", "evicting"})

MAX_TRACK_ROWS = 5000  # bound on the track-list query
MAX_FINDING_SPANS = 5000  # bound on the per-track findings query


class ConfigError(Exception):
    """Bad CLI/env configuration (exit 2)."""


class GamingStop(Exception):
    """The arbiter reports a gaming tenant: stop now, write nothing partial (exit 4)."""


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


def _positive_int(raw: str) -> int:
    value = int(raw)
    if value < 1:
        raise argparse.ArgumentTypeError(f"must be >= 1, got {value}")
    return value


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
        help="full: whole tracks; findings: only windows around held findings",
    )
    p.add_argument("--limit-tracks", type=_positive_int, default=None)
    p.add_argument("--track-limit-per-book", type=_positive_int, default=None)
    p.add_argument("--force", action="store_true", help="re-run tracks already stored")
    p.add_argument(
        "--dry-run", action="store_true", help="print the plan; no model, no writes"
    )
    p.add_argument(
        "--out-dir", type=Path, default=None, help="also write per-track JSON here"
    )
    p.add_argument(
        "--arbiter-url",
        default=None,
        help="gpu-arbiter base URL (checked between windows)",
    )
    p.add_argument(
        "--self-check",
        action="store_true",
        help="load the model, transcribe 5 s of silence",
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

    mount_raw = env.get("BOOKS_MOUNT", "/mnt/media/books")
    return Config(
        backend=backend,
        asins=asins,
        scope=args.scope,
        limit_tracks=args.limit_tracks,
        track_limit_per_book=args.track_limit_per_book,
        force=args.force,
        dry_run=args.dry_run,
        out_dir=args.out_dir,
        arbiter_url=(args.arbiter_url or "").rstrip("/") or None,
        self_check=args.self_check,
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


def plan_windows(
    start: float,
    end: float,
    words: Sequence[Span],
    target: float,
    hard_max: float,
) -> list[Span]:
    """Split [start, end) into windows of at most `target` (<= hard_max) seconds.

    Each cut is placed at the midpoint of the largest inter-word gap (from the
    PRODUCTION word timestamps, sorted (start, end) pairs) whose midpoint lies in
    the last CUT_SEARCH_FRACTION of the target window, so no word is split. With
    no usable gap there (or no production words at all) the cut falls back to a
    fixed cut at start + target.
    """
    target = min(target, hard_max)
    if end <= start:
        return []
    windows: list[Span] = []
    pos = start
    gaps = [
        (words[i][1], words[i + 1][0])
        for i in range(len(words) - 1)
        if words[i + 1][0] > words[i][1]
    ]
    while end - pos > target:
        lo = pos + target * (1.0 - CUT_SEARCH_FRACTION)
        hi = pos + target
        best: float | None = None
        best_len = -1.0
        for g0, g1 in gaps:
            mid = (g0 + g1) / 2.0
            if lo <= mid <= hi and (g1 - g0) > best_len:
                best, best_len = mid, g1 - g0
        cut = best if best is not None else hi
        windows.append((pos, cut))
        pos = cut
    windows.append((pos, end))
    return windows


def merge_spans(spans: Sequence[Span], pad: float, duration: float) -> list[Span]:
    """Pad each span by `pad`, clamp to [0, duration], merge overlapping/touching ones."""
    padded = sorted(
        (max(0.0, s - pad), min(duration, e + pad)) for s, e in spans if e >= s
    )
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
    if scope == "full":
        return plan_windows(0.0, duration, words, target, hard_max)
    plan: list[Span] = []
    for s, e in merge_spans(finding_spans, FINDING_PAD_SEC, duration):
        plan.extend(plan_windows(s, e, words, target, hard_max))
    return plan


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


# ---------------------------------------------------------------------------
# Output parsing / segmenting (pure)
# ---------------------------------------------------------------------------

_TS_SPLIT = re.compile(r"\[T:(\d+)\]")


def parse_granite_timestamps(
    text: str, window_start: float, window_len: float
) -> list[dict[str, Any]]:
    """Parse Granite `word [T:N]` output into absolute-time words.

    N is the word END time in centiseconds modulo 1000 (it wraps every 10 s), so
    the value is unrolled by adding 10 s whenever it would go backwards. `_`
    marks silence: dropped from the output but it advances the clock. A word's
    start is the previous token's end (the window start for the first). Several
    words before one tag share that interval evenly; trailing untagged words run
    to the window end. Times are clamped to the window and offset by its start.
    """
    parts = _TS_SPLIT.split(text or "")
    words: list[dict[str, Any]] = []
    last_end = 0.0
    offset = 0.0

    def emit(chunk: str, t0: float, t1: float) -> None:
        tokens = [t for t in chunk.split() if t != GRANITE_SILENCE_TOKEN]
        if not tokens:
            return
        step = (t1 - t0) / len(tokens)
        for i, tok in enumerate(tokens):
            ws = t0 + i * step
            we = t0 + (i + 1) * step
            words.append(
                {
                    "word": tok,
                    "start": round(window_start + ws, 3),
                    "end": round(window_start + we, 3),
                    "score": None,
                    "speaker": None,
                }
            )

    for chunk, ts in zip(parts[0::2], parts[1::2]):
        end = int(ts) / 100.0
        while end + offset < last_end:
            offset += GRANITE_TS_WRAP_SEC
        end = min(end + offset, window_len)
        start = min(last_end, end)
        emit(chunk, start, end)
        last_end = max(last_end, end)
    if len(parts) % 2 == 1 and parts[-1].strip():
        emit(parts[-1], last_end, max(last_end, window_len))
    return words


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


def words_to_segments(
    words: list[dict[str, Any]], first_id: int
) -> list[dict[str, Any]]:
    """Group timed words into CONTRACT §1.2.1 segments (gap / duration / word caps)."""
    segments: list[dict[str, Any]] = []
    current: list[dict[str, Any]] = []

    def flush() -> None:
        if current:
            segments.append(
                {
                    "id": first_id + len(segments),
                    "start": min(w["start"] for w in current),
                    "end": max(w["end"] for w in current),
                    "text": " ".join(w["word"] for w in current),
                    "speaker": None,
                    "words": list(current),
                }
            )

    for w in words:
        if current:
            gap = w["start"] - current[-1]["end"]
            dur = current[-1]["end"] - current[0]["start"]
            if (
                gap > SEGMENT_GAP_SECONDS
                or dur >= SEGMENT_MAX_SECONDS
                or len(current) >= SEGMENT_MAX_WORDS
            ):
                flush()
                current = []
        current.append(w)
    flush()
    return segments


def canary_segment(
    text: str, start: float, end: float, seg_id: int
) -> dict[str, Any] | None:
    """One segment per Canary window; words carry NO start/end keys (no timestamps)."""
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


# ---------------------------------------------------------------------------
# Backends (heavy imports are lazy: CI and --dry-run never import torch)
# ---------------------------------------------------------------------------


@dataclass
class WindowResult:
    text: str
    words: list[dict[str, Any]]
    hit_token_limit: bool = False


class Backend:
    name = ""
    has_word_times = False

    def __init__(self, cfg: Config) -> None:
        self.cfg = cfg
        self.torch: Any = None

    def load(self) -> None:  # pragma: no cover - needs the GPU stack
        raise NotImplementedError

    def transcribe_window(
        self, audio: Any, window_start: float
    ) -> WindowResult:  # pragma: no cover
        raise NotImplementedError

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
    """Granite Speech 4.1 2B-plus, timestamp mode (usage per the HF model card)."""

    name = "granite"
    has_word_times = True

    def load(self) -> None:  # pragma: no cover - needs the GPU stack
        import torch
        from transformers import AutoModelForSpeechSeq2Seq, AutoProcessor

        self.torch = torch
        self.processor = AutoProcessor.from_pretrained(
            self.cfg.model_id, revision=self.cfg.model_revision
        )
        self.tokenizer = self.processor.tokenizer
        self.model = AutoModelForSpeechSeq2Seq.from_pretrained(
            self.cfg.model_id,
            revision=self.cfg.model_revision,
            device_map=self.cfg.device,
            dtype=torch.bfloat16,
        )
        self.model.eval()

    def max_new_tokens(self, window_len: float) -> int:
        return max(
            GRANITE_MIN_NEW_TOKENS,
            int(GRANITE_TOKENS_PER_SECOND * window_len * GRANITE_TOKEN_MARGIN),
        )

    def transcribe_window(
        self, audio: Any, window_start: float
    ) -> WindowResult:  # pragma: no cover
        torch = self.torch
        window_len = len(audio) / SAMPLE_RATE
        chat = [
            {"role": "system", "content": GRANITE_SYSTEM_PROMPT},
            {"role": "user", "content": GRANITE_TIMESTAMP_PROMPT},
        ]
        prompt = self.tokenizer.apply_chat_template(
            chat, tokenize=False, add_generation_prompt=True
        )
        wav = torch.from_numpy(audio).unsqueeze(0)
        inputs = self.processor(
            prompt, wav, device=self.cfg.device, return_tensors="pt"
        ).to(self.cfg.device)
        limit = self.max_new_tokens(window_len)
        with torch.inference_mode():
            out = self.model.generate(
                **inputs,
                max_new_tokens=limit,
                do_sample=False,
                num_beams=GRANITE_NUM_BEAMS,
                repetition_penalty=GRANITE_REPETITION_PENALTY,
            )
        new_tokens = out[0, inputs["input_ids"].shape[-1] :]
        text = self.tokenizer.decode(
            new_tokens, add_special_tokens=False, skip_special_tokens=True
        )
        words = parse_granite_timestamps(text, window_start, window_len)
        return WindowResult(
            text=text, words=words, hit_token_limit=len(new_tokens) >= limit
        )


class CanaryBackend(Backend):
    """Canary-Qwen 2.5B via NeMo SALM (usage per the HF model card / SALM.generate docstring)."""

    name = "canary"
    has_word_times = False

    def load(self) -> None:  # pragma: no cover - needs the GPU stack
        import torch
        from huggingface_hub import snapshot_download
        from nemo.collections.speechlm2.models import SALM

        self.torch = torch
        # snapshot_download pins the exact HF revision; SALM then loads the local dir.
        local = snapshot_download(self.cfg.model_id, revision=self.cfg.model_revision)
        # Weights stay in the checkpoint dtype (the card loads it as-is); bf16
        # compute comes from autocast in transcribe_window, so the float32 audio
        # tensor SALM.generate expects still matches the perception module.
        self.model = SALM.from_pretrained(local).to(self.cfg.device).eval()

    def transcribe_window(
        self, audio: Any, window_start: float
    ) -> WindowResult:  # pragma: no cover
        torch = self.torch
        window_len = len(audio) / SAMPLE_RATE
        prompt = CANARY_PROMPT_TEMPLATE.format(tag=self.model.audio_locator_tag)
        audios = torch.from_numpy(audio).unsqueeze(0).to(self.cfg.device)
        audio_lens = torch.tensor(
            [audios.shape[-1]], dtype=torch.int64, device=self.cfg.device
        )
        limit = max(CANARY_MIN_NEW_TOKENS, int(CANARY_TOKENS_PER_SECOND * window_len))
        with (
            torch.inference_mode(),
            torch.autocast("cuda", dtype=torch.bfloat16),
        ):
            ids = self.model.generate(
                prompts=[[{"role": "user", "content": prompt}]],
                audios=audios,
                audio_lens=audio_lens,
                max_new_tokens=limit,
            )
        text = self.model.tokenizer.ids_to_text(ids[0].cpu()).strip()
        return WindowResult(
            text=text, words=[], hit_token_limit=int(ids.shape[-1]) >= limit
        )


def make_backend(cfg: Config) -> Backend:
    if cfg.backend == "granite":
        return GraniteBackend(cfg)
    if cfg.backend == "canary":
        return CanaryBackend(cfg)
    raise ConfigError(f"unknown backend {cfg.backend!r}")


# ---------------------------------------------------------------------------
# Audio
# ---------------------------------------------------------------------------


def ffmpeg_timeout(duration: float) -> float:
    """Decode deadline: 2 min floor plus 5% of real time (a 20 h book gets 1 h)."""
    return 120.0 + 0.05 * max(0.0, duration)


def decode_audio(
    cfg: Config, path: Path, duration: float
) -> Any:  # pragma: no cover - needs ffmpeg+numpy
    """Decode the whole track ONCE to 16 kHz mono float32 (numpy) via ffmpeg."""
    import numpy as np

    cmd = [
        cfg.ffmpeg_bin, "-nostdin", "-hide_banner", "-loglevel", "error",
        "-i", str(path), "-vn", "-ac", "1", "-ar", str(SAMPLE_RATE), "-f", "f32le", "-",
    ]  # fmt: skip
    proc = subprocess.run(
        cmd, capture_output=True, check=False, timeout=ffmpeg_timeout(duration)
    )
    if proc.returncode != 0:
        raise RuntimeError(
            f"ffmpeg exit {proc.returncode}: {proc.stderr.decode('utf-8', 'replace')[-500:]}"
        )
    return np.frombuffer(proc.stdout, dtype=np.float32)


def slice_audio(audio: Any, start: float, end: float) -> Any:
    return audio[round(start * SAMPLE_RATE) : round(end * SAMPLE_RATE)]


# ---------------------------------------------------------------------------
# gpu-arbiter
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
    cfg: Config, fetch: Callable[[str], dict[str, Any]] = fetch_arbiter_status
) -> None:
    if not cfg.arbiter_url:
        return
    try:
        status: dict[str, Any] | None = fetch(cfg.arbiter_url)
    except (OSError, ValueError, urllib.error.URLError) as exc:
        log.warning("arbiter status unreadable (%s); treating as gaming", exc)
        status = None
    if gaming_active(status):
        raise GamingStop(
            f"arbiter state={None if status is None else status.get('state')}"
        )


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

HELD_FINDING_SPANS_SQL = """
    SELECT DISTINCT f.start_sec, f.end_sec
      FROM transcript_findings f
     WHERE f.file_path = %s
       AND f.patch_state = 'proposed'
       AND EXISTS (SELECT 1 FROM finding_events e
                    WHERE e.finding_id = f.id AND e.kind = 'decision' AND e.outcome = 'hold')
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


# ---------------------------------------------------------------------------
# Per-track pipeline
# ---------------------------------------------------------------------------


def transcribe_track(
    cfg: Config,
    backend: Backend,
    audio: Any,
    plan: Sequence[Span],
    gaming_check: Callable[[], None],
) -> dict[str, Any]:
    """Transcribe every planned window; raises GamingStop between windows."""
    segments: list[dict[str, Any]] = []
    degenerate: list[int] = []
    audio_sec = 0.0
    for idx, (ws, we) in enumerate(plan):
        gaming_check()
        res = backend.transcribe_window(slice_audio(audio, ws, we), ws)
        audio_sec += we - ws
        tokens = (
            [w["word"] for w in res.words] if res.words else (res.text or "").split()
        )
        bad = res.hit_token_limit or has_repetition_loop(tokens)
        if bad:
            degenerate.append(idx)
            log.warning(
                "window %d [%.1f, %.1f] degenerate (repetition/token limit)",
                idx,
                ws,
                we,
            )
        if backend.has_word_times:
            new = words_to_segments(res.words, len(segments))
        else:
            seg = canary_segment(res.text, ws, we, len(segments))
            new = [seg] if seg else []
        if bad:
            for s in new:
                s["degenerate"] = True  # optional marker; §1.2.1 readers ignore it
        segments.extend(new)
    return {
        "segments": segments,
        "raw_text": " ".join(s["text"] for s in segments),
        "has_word_times": backend.has_word_times,
        "audio_sec": round(audio_sec, 3),
        "degenerate_windows": degenerate,
    }


def _out_file(cfg: Config, track: Track) -> Path | None:
    if cfg.out_dir is None:
        return None
    digest = hashlib.sha256(track.file_path.encode("utf-8")).hexdigest()[:16]
    return cfg.out_dir / f"{track.asin}-{digest}-{cfg.backend}-{cfg.scope}.json"


def process_track(
    conn: Any,
    cfg: Config,
    backend: Backend,
    track: Track,
    decode: Callable[[Config, Path, float], Any] = decode_audio,
    gaming_check: Callable[[], None] | None = None,
) -> str:
    """Run one track end to end. Returns 'done' or 'skipped'; raises on failure/GamingStop."""
    words = read_production_words(conn, track)
    spans = read_finding_spans(conn, track) if cfg.scope == "findings" else []
    plan = plan_track(
        track.duration, words, cfg.window_sec, cfg.hard_max_sec, cfg.scope, spans
    )
    if not plan:
        log.info(
            "skip %s: nothing to transcribe (scope=%s)", track.file_path, cfg.scope
        )
        return "skipped"
    check = gaming_check or (lambda: check_gaming(cfg))
    check()
    t0 = time.monotonic()
    backend.reset_peak()
    audio = decode(cfg, resolve_audio_path(cfg, track.file_path), track.duration)
    result = transcribe_track(cfg, backend, audio, plan, check)
    wall = time.monotonic() - t0
    alloc, reserved = backend.peak_mib()
    result.update(
        wall_sec=round(wall, 3),
        rtfx=round(result["audio_sec"] / wall, 3) if wall > 0 else None,
        peak_vram_mib=alloc,
        peak_reserved_mib=reserved,
    )
    write_result(conn, cfg, track, result)
    out = _out_file(cfg, track)
    if out is not None:
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(
            json.dumps(
                {"file_path": track.file_path, "plan": plan, **result},
                ensure_ascii=False,
            ),
            encoding="utf-8",
        )
    log.info(
        "done %s: %d windows, %.0f s audio, rtfx %s, peak %s MiB, degenerate %s",
        track.file_path, len(plan), result["audio_sec"], result["rtfx"],
        alloc, result["degenerate_windows"],
    )  # fmt: skip
    return "done"


def dry_run(
    conn: Any, cfg: Config, tracks: Sequence[Track], emit: Callable[[str], None] = print
) -> None:
    """Print the track list + window plan. Reads only; no model load, no writes."""
    for t in tracks:
        words = read_production_words(conn, t)
        spans = read_finding_spans(conn, t) if cfg.scope == "findings" else []
        plan = plan_track(
            t.duration, words, cfg.window_sec, cfg.hard_max_sec, cfg.scope, spans
        )
        emit(
            json.dumps(
                {
                    "asin": t.asin,
                    "file_path": t.file_path,
                    "duration_sec": t.duration,
                    "scope": cfg.scope,
                    "already_done": already_done(conn, cfg, t),
                    "production_words": len(words),
                    "windows": [[round(s, 2), round(e, 2)] for s, e in plan],
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
    res = backend.transcribe_window(np.zeros(5 * SAMPLE_RATE, dtype=np.float32), 0.0)
    info = backend.info()
    info["output"] = res.text[:200]
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
    decode: Callable[[Config, Path, float], Any] = decode_audio,
    gaming_check: Callable[[], None] | None = None,
) -> int:
    tracks = select_tracks(conn, cfg)
    if cfg.dry_run:
        dry_run(conn, cfg, tracks)
        return EXIT_OK
    todo = [t for t in tracks if cfg.force or not already_done(conn, cfg, t)]
    log.info("%d track(s) selected, %d to do", len(tracks), len(todo))
    if cfg.limit_tracks:
        todo = todo[: cfg.limit_tracks]
    if not todo:
        return EXIT_OK
    if backend is None:  # pragma: no cover - real model
        backend = make_backend(cfg)
        backend.load()
    failed = 0
    for track in todo:
        try:
            process_track(
                conn, cfg, backend, track, decode=decode, gaming_check=gaming_check
            )
        except GamingStop as exc:
            log.warning(
                "stopping for gaming (%s); %s not written", exc, track.file_path
            )
            return EXIT_GAMING
        except Exception:
            failed += 1
            log.exception("track failed: %s", track.file_path)
            try:
                conn.rollback()
            except Exception:  # noqa: BLE001
                log.warning("rollback after failure also failed")
    return EXIT_TRACKS_FAILED if failed else EXIT_OK


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
        conn.close()


if __name__ == "__main__":
    sys.exit(main())
