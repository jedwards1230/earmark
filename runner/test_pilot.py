#!/usr/bin/env python3
"""
Unit tests for runner/pilot.py (the one-shot second-ASR pilot).

No torch, transformers, NeMo, numpy, ffmpeg or database: the model backend, the
audio decoder and the Postgres connection are all fakes, so this runs anywhere:

    python -m pytest runner/test_pilot.py -v
"""

from __future__ import annotations

import ast
import importlib.util
import itertools
import json
import os
import sys
import tempfile
from pathlib import Path, PurePosixPath, PureWindowsPath
from typing import Any, ClassVar, Self

import pytest

_PILOT_PATH = Path(__file__).with_name("pilot.py")
_spec = importlib.util.spec_from_file_location("asr_pilot_under_test", _PILOT_PATH)
assert _spec and _spec.loader
pilot = importlib.util.module_from_spec(_spec)
sys.modules[_spec.name] = pilot  # dataclasses resolve annotations via sys.modules
_spec.loader.exec_module(pilot)

REV = "823ca793aa49925d4eb2e758709b7f2c98513e8e"
TID = "00000000-0000-0000-0000-000000000001"
OUT_DIR = tempfile.mkdtemp(prefix="pilot-test-out-")
DECODES: list[tuple[float, float]] = []


# ── Fakes ──────────────────────────────────────────────────────────────────────
class FakeCursor:
    def __init__(self, conn: FakeConn) -> None:
        self.conn = conn
        self.sql = ""

    def __enter__(self) -> Self:
        return self

    def __exit__(self, *exc: object) -> None:
        return None

    def execute(self, sql: str, params: Any = None) -> None:
        self.sql = sql
        if self.conn.fail_insert and sql.lstrip().startswith("INSERT"):
            raise OSError("insert failed")
        self.conn.executed.append((sql, params))

    def fetchall(self) -> list[Any]:
        if "FROM book_metadata" in self.sql:
            return list(self.conn.tracks)
        if "FROM transcript_findings" in self.sql:
            return list(self.conn.finding_spans)
        raise AssertionError(f"unexpected fetchall for {self.sql}")

    def fetchone(self) -> Any:
        if self.sql == "SELECT 1":
            if self.conn.dead:
                raise OSError("server closed the connection")
            return (1,)
        if "FROM pilot_transcripts" in self.sql:
            fp = self.conn.executed[-1][1][0]
            return (1,) if fp in self.conn.done else None
        if "FROM transcripts" in self.sql:
            return (self.conn.segments,)
        raise AssertionError(f"unexpected fetchone for {self.sql}")


class FakeConn:
    def __init__(
        self, tracks: list[tuple[str, str, str, float]], segments: Any = None
    ) -> None:
        self.tracks = tracks
        self.segments = segments if segments is not None else []
        self.finding_spans: list[tuple[float, float]] = []
        self.done: set[str] = set()
        self.executed: list[tuple[str, Any]] = []
        self.commits = 0
        self.closed = 0
        self.dead = False
        self.fail_insert = False

    def cursor(self) -> FakeCursor:
        return FakeCursor(self)

    def commit(self) -> None:
        self.commits += 1

    def rollback(self) -> None:
        return None

    def close(self) -> None:
        self.closed = 1

    def writes(self) -> list[tuple[str, Any]]:
        return [
            (s, p)
            for s, p in self.executed
            if s.lstrip().startswith(("INSERT", "DELETE", "UPDATE"))
        ]

    def inserts(self) -> list[Any]:
        return [p for s, p in self.executed if s.lstrip().startswith("INSERT")]


class FakeBackend(pilot.Backend):
    """Records batch sizes; `outputs` are per-window texts (or Exceptions); OOMs above `oom_above`."""

    def __init__(
        self,
        cfg: Any,
        name: str,
        outputs: list[Any] | None = None,
        oom_above: int | None = None,
    ) -> None:
        super().__init__(cfg)
        self.name = name
        self.outputs = outputs
        self.oom_above = oom_above
        self.batches: list[int] = []
        self.cleared = 0

    def load(self) -> None:
        raise AssertionError("tests never load a real model")

    def clear_cache(self) -> None:
        self.cleared += 1

    def transcribe_batch(self, audios: Any) -> list[Any]:
        if self.oom_above is not None and len(audios) > self.oom_above:
            raise RuntimeError("CUDA out of memory. Tried to allocate 2.00 GiB")
        self.batches.append(len(audios))
        out = []
        for _ in audios:
            text = self.outputs.pop(0) if self.outputs else "Hello, World."
            if isinstance(text, Exception):
                raise text
            out.append(pilot.WindowResult(text=text))
        return out


def fake_decode(cfg: Any, path: Path, start: float, end: float) -> list[float]:
    DECODES.append((start, end))
    return [0.0] * round((end - start) * pilot.SAMPLE_RATE)


def make_cfg(
    backend: str = "granite", *extra: str, env: dict[str, str] | None = None
) -> Any:
    base = {
        "DATABASE_URL": "postgres://x@y/z",
        "PILOT_OUT_DIR": OUT_DIR,
        "PILOT_MODEL_REVISION": REV,
        "BOOKS_MOUNT": "/books",
    }
    base.update(env or {})
    return pilot.load_config(["--backend", backend, "--asin", "B1", *extra], base)


def no_gaming() -> None:
    return None


def run(cfg: Any, conn: FakeConn, backend: Any = None, **kw: Any) -> int:
    kw.setdefault("decode", fake_decode)
    kw.setdefault("gaming_check", no_gaming)
    if backend is None:
        backend = FakeBackend(cfg, cfg.backend)
    return pilot.run(cfg, conn, backend=backend, **kw)


TRACKS = [("B1", "/books/a/t1.m4b", TID, 120.0), ("B1", "/books/a/t2.m4b", TID, 60.0)]
SEGMENTS = [
    {
        "words": [
            {"word": "x", "start": 1.0, "end": 1.5},
            {"word": "y", "start": 80.0, "end": 80.4},
        ]
    }
]


# ── Window planning ────────────────────────────────────────────────────────────
def _words_every(
    step: float, until: float, dur: float = 0.3
) -> list[tuple[float, float]]:
    out, t = [], 0.0
    while t < until:
        out.append((t, t + dur))
        t += step
    return out


def test_plan_cuts_at_largest_gap_in_last_quarter() -> None:
    words = _words_every(0.5, 200.0)  # 0.2 s gaps everywhere
    words = [
        w for w in words if not (50.0 <= w[0] < 52.0) and not (80.0 <= w[0] < 81.0)
    ]
    plan = pilot.plan_windows(0.0, 200.0, words, 90.0, 210.0)
    assert 79.8 < plan[0][1] < 81.0  # the 79.8..81.0 gap, not the bigger 50 s one
    assert plan[-1][1] == 200.0
    for (_s0, e0), (s1, _e1) in itertools.pairwise(plan):
        assert e0 == s1  # contiguous, no audio dropped
    for _, cut in plan[:-1]:
        assert not any(s < cut < e for s, e in words)


def test_plan_never_exceeds_hard_max() -> None:
    plan = pilot.plan_windows(0.0, 5000.0, [], 900.0, 540.0)
    assert all(e - s <= 540.0 + 1e-9 for s, e in plan)
    plan = pilot.plan_windows(0.0, 400.0, _words_every(0.5, 400.0), 40.0, 40.0)
    assert all(e - s <= 40.0 + 1e-9 for s, e in plan)


def test_plan_without_words_falls_back_to_fixed_cuts() -> None:
    plan = pilot.plan_windows(0.0, 600.0, [], 240.0, 540.0)
    assert plan == [(0.0, 240.0), (240.0, 480.0), (480.0, 600.0)]


def test_plan_short_track_single_window_and_empty() -> None:
    assert pilot.plan_windows(0.0, 30.0, [], 240.0, 540.0) == [(0.0, 30.0)]
    assert pilot.plan_windows(5.0, 5.0, [], 240.0, 540.0) == []


def test_short_tail_folds_into_previous_window_within_hard_max() -> None:
    assert pilot.plan_windows(0.0, 243.0, [], 240.0, 540.0) == [(0.0, 243.0)]
    assert pilot.plan_windows(0.0, 42.0, [], 40.0, 40.0) == [(0.0, 40.0), (40.0, 42.0)]
    assert pilot.plan_windows(0.0, 246.0, [], 240.0, 540.0) == [
        (0.0, 240.0),
        (240.0, 246.0),
    ]


def test_gaps_bisect_picks_largest_gap_in_range() -> None:
    gaps = pilot.Gaps([(0.0, 1.0), (1.2, 2.0), (3.0, 4.0), (4.1, 5.0)])
    assert gaps.best(0.0, 10.0) == 2.5
    assert gaps.best(0.0, 2.0) == 1.1
    assert gaps.best(6.0, 9.0) is None


def test_group_windows_breaks_on_gaps_and_size() -> None:
    plan = [(0.0, 90.0), (90.0, 180.0), (500.0, 590.0)]
    assert pilot.group_windows(plan, 1800.0) == [plan[:2], plan[2:]]
    assert pilot.group_windows(plan[:2], 100.0) == [[plan[0]], [plan[1]]]


def test_merge_spans_pads_merges_and_clamps() -> None:
    spans = [(100.0, 200.0), (203.0, 300.0), (500.0, 510.0), (2.0, 4.0)]
    assert pilot.merge_spans(spans, 5.0, 512.0) == [
        (0.0, 9.0),
        (95.0, 305.0),
        (495.0, 512.0),
    ]
    assert pilot.merge_spans([(10.0, 20.0), (30.0, 40.0)], 5.0, 100.0) == [(5.0, 45.0)]
    assert pilot.merge_spans([], 5.0, 100.0) == []


def test_findings_span_edges_snap_to_production_gaps_inside_pad() -> None:
    words = [(t, t + 0.4) for t in [x * 0.5 for x in range(500)]]  # 0.1 s gaps
    words = [w for w in words if not 96.0 <= w[0] < 97.0 and not 203.0 <= w[0] < 204.0]
    ((s, e),) = pilot.merge_spans([(100.0, 200.0)], 5.0, 1000.0, words)
    assert 95.9 < s < 97.0 and 202.9 < e < 204.0
    assert pilot.merge_spans([(100.0, 200.0)], 5.0, 1000.0, []) == [(95.0, 205.0)]


def test_plan_track_findings_windows_each_merged_span() -> None:
    plan = pilot.plan_track(
        1000.0, [], 40.0, 40.0, "findings", [(100.0, 181.0), (600.0, 610.0)]
    )
    assert plan[0][0] == 95.0 and plan[-1] == (595.0, 615.0)
    assert all(e - s <= 40.0 for s, e in plan)


# ── Output helpers ─────────────────────────────────────────────────────────────
def test_repetition_guard() -> None:
    assert pilot.has_repetition_loop(["the", "cat", "sat"] * 4 + ["end"])
    assert pilot.has_repetition_loop(["and"] * 8)
    assert not pilot.has_repetition_loop(["no", "no", "no", "I", "said", "no"])
    assert not pilot.has_repetition_loop(
        [
            "it",
            "was",
            "the",
            "best",
            "of",
            "times",
            "it",
            "was",
            "the",
            "worst",
            "of",
            "times",
        ]
    )


def test_degenerate_reasons() -> None:
    assert pilot.degenerate_reason("Call me Ishmael.", False) is None
    assert pilot.degenerate_reason("  ", False) == "empty"
    assert pilot.degenerate_reason("Some text", True) == "token_limit"
    assert pilot.degenerate_reason(" ".join(["again"] * 9), False) == "repetition"


def test_max_new_tokens_uses_longest_window_in_batch() -> None:
    assert pilot.max_new_tokens(12, 64, [30.0, 240.0, 100.0]) == int(12 * 240 * 1.25)
    assert pilot.max_new_tokens(12, 64, [2.0]) == 64
    assert pilot.max_new_tokens(8, 128, [38.0]) == 380


def test_pad_audios_and_generated_row_slicing() -> None:
    rows, lens = pilot.pad_audios([[1.0, 2.0, 3.0], [4.0], []])
    assert lens == [3, 1, 0]
    assert rows == [[1.0, 2.0, 3.0], [4.0, 0.0, 0.0], [0.0, 0.0, 0.0]]
    # A left-padded batch: every row shares input_len; new tokens follow it.
    out = [[0, 0, 7, 7, 101, 102, 2], [0, 7, 7, 7, 201, 2, 2]]
    new = pilot.generated_rows(out, 4)
    assert new == [[101, 102, 2], [201, 2, 2]]
    assert [pilot.real_token_count(r, {2}) for r in new] == [2, 1]


def test_window_segment_shape_keeps_casing_and_punctuation() -> None:
    seg = pilot.window_segment(" Dr. Who? Yes, Sir. ", 76.0, 100.0, 3)
    assert seg == {
        "id": 3,
        "start": 76.0,
        "end": 100.0,
        "text": "Dr. Who? Yes, Sir.",
        "speaker": None,
        "words": [
            {"word": w, "score": None, "speaker": None}
            for w in ["Dr.", "Who?", "Yes,", "Sir."]
        ],
    }
    assert pilot.window_segment("", 0.0, 1.0, 0) is None


# ── Batching ───────────────────────────────────────────────────────────────────
def test_batcher_halves_on_oom_and_keeps_the_smaller_size() -> None:
    cfg = make_cfg("granite")
    backend = FakeBackend(cfg, "granite", oom_above=4)
    b = pilot.Batcher(backend, 16)
    results, used = b.run([[0.0]] * 10)
    assert len(results) == 10 and used == 4 and b.size == 4
    assert backend.batches == [4, 4, 2] and backend.cleared == 2  # 10 -> OOM, 8 -> OOM


def test_batcher_raises_non_oom_and_oom_at_size_one() -> None:
    cfg = make_cfg("granite")
    with pytest.raises(ValueError):
        pilot.Batcher(FakeBackend(cfg, "granite", outputs=[ValueError("bad")]), 4).run(
            [[0.0]]
        )
    with pytest.raises(RuntimeError, match="out of memory"):
        pilot.Batcher(FakeBackend(cfg, "granite", oom_above=0), 8).run([[0.0]] * 3)


def test_windows_are_batched_across_tracks() -> None:
    cfg = make_cfg("canary")  # batch 32
    tracks = [("B1", f"/books/a/{i}.m4b", TID, 30.0) for i in range(3)]
    conn = FakeConn(tracks, [])
    backend = FakeBackend(cfg, "canary")
    assert run(cfg, conn, backend) == 0
    assert backend.batches == [3]  # one generate call for three 1-window tracks
    assert [p[0] for p in conn.inserts()] == [t[1] for t in tracks]


def test_batches_fill_to_batch_size_within_a_track() -> None:
    cfg = make_cfg("granite", "--batch-size", "2")
    conn = FakeConn([("B1", "/books/a/long.m4b", TID, 600.0)], [])
    backend = FakeBackend(cfg, "granite")
    assert run(cfg, conn, backend) == 0
    assert backend.batches == [2, 1]  # windows 240 + 240 + 120


def test_oom_batch_size_used_is_recorded_in_out_file() -> None:
    out = tempfile.mkdtemp(prefix="pilot-oom-")
    cfg = make_cfg("canary", "--out-dir", out, "--batch-size", "8")
    conn = FakeConn([("B1", "/books/a/long.m4b", TID, 300.0)], [])  # 8 windows
    backend = FakeBackend(cfg, "canary", oom_above=2)
    assert run(cfg, conn, backend) == 0
    (f,) = Path(out).glob("*.json")
    data = json.loads(f.read_text(encoding="utf-8"))
    assert data["batch_size"] == 8 and data["batch_size_used"] == 2
    assert data["windows"] == 8 and "decode_sec" in data


# ── run(): writes, idempotency, stop conditions ────────────────────────────────
def test_run_writes_one_row_per_track_in_its_own_commit() -> None:
    cfg = make_cfg("granite", "--batch-size", "1")
    conn = FakeConn(TRACKS, SEGMENTS)
    assert run(cfg, conn) == pilot.EXIT_OK
    inserts = conn.inserts()
    assert [p[0] for p in inserts] == ["/books/a/t1.m4b", "/books/a/t2.m4b"]
    assert conn.commits == 2
    row = inserts[0]
    assert (
        row[1] == TID
        and row[2] == "granite"
        and row[3] == "ibm-granite/granite-speech-4.1-2b"
    )
    assert row[4] == REV and row[5] == "full" and row[6] == 240.0
    assert row[7] is False  # has_word_times: window-level text only
    segs = json.loads(row[8])
    assert segs[0]["text"] == "Hello, World." and "start" not in segs[0]["words"][0]
    assert all("pilot_transcripts" in s for s, _ in conn.writes())


def test_run_skips_tracks_already_done_unless_forced() -> None:
    cfg = make_cfg("granite")
    conn = FakeConn(TRACKS, SEGMENTS)
    conn.done = {"/books/a/t1.m4b"}
    assert run(cfg, conn) == 0
    assert [p[0] for p in conn.inserts()] == ["/books/a/t2.m4b"]

    cfg_f = make_cfg("granite", "--force")
    conn_f = FakeConn(TRACKS, SEGMENTS)
    conn_f.done = {"/books/a/t1.m4b", "/books/a/t2.m4b"}
    assert run(cfg_f, conn_f) == 0
    assert [s.split()[0] for s, _ in conn_f.writes()] == [
        "DELETE",
        "INSERT",
        "DELETE",
        "INSERT",
    ]


def test_run_all_done_loads_no_model() -> None:
    cfg = make_cfg("granite")
    conn = FakeConn(TRACKS, SEGMENTS)
    conn.done = {t[1] for t in TRACKS}
    assert (
        pilot.run(cfg, conn, backend=None, decode=fake_decode, gaming_check=no_gaming)
        == 0
    )
    assert conn.writes() == []


def test_gaming_between_batches_exits_4_and_keeps_done_tracks() -> None:
    cfg = make_cfg("canary", "--batch-size", "1", "--arbiter-url", "http://arb:48750/")
    assert cfg.arbiter_url == "http://arb:48750"
    conn = FakeConn(TRACKS, SEGMENTS)
    # Pre-load check, then track 1 = 4 windows: track + chunk + 4 batch checks.
    states = iter(["available"] * 7 + ["gaming"] * 10)

    def check() -> None:
        pilot.check_gaming(cfg, fetch=lambda url: {"state": next(states)})

    assert run(cfg, conn, gaming_check=check) == pilot.EXIT_GAMING
    assert [p[0] for p in conn.inserts()] == ["/books/a/t1.m4b"]


def test_check_gaming_states_retry_and_unreachable() -> None:
    cfg = make_cfg("granite", "--arbiter-url", "http://arb:48750")
    pilot.check_gaming(cfg, fetch=lambda url: {"state": "available"})
    for state in ("gaming", "evicting"):
        with pytest.raises(pilot.GamingStop):
            pilot.check_gaming(cfg, fetch=lambda url, s=state: {"state": s})

    def boom(url: str) -> dict[str, Any]:
        raise OSError("connection refused")

    sleeps: list[float] = []
    with pytest.raises(pilot.GamingStop):
        pilot.check_gaming(cfg, fetch=boom, sleep=sleeps.append)
    assert sleeps == [pilot.ARBITER_RETRY_DELAY_SEC]
    flaky = iter([OSError("blip"), {"state": "available"}])

    def blip(url: str) -> dict[str, Any]:
        got = next(flaky)
        if isinstance(got, Exception):
            raise got
        return got

    pilot.check_gaming(cfg, fetch=blip, sleep=lambda _s: None)
    pilot.check_gaming(make_cfg("granite"), fetch=boom)  # no --arbiter-url


def test_stop_file_exits_5_and_keeps_done_tracks() -> None:
    stop = Path(tempfile.mkdtemp(prefix="pilot-stop-")) / "STOP"
    cfg = make_cfg("canary", "--batch-size", "1", env={"PILOT_STOP_FILE": str(stop)})
    conn = FakeConn(TRACKS, SEGMENTS)

    class StopAfterTrack1(FakeBackend):
        def transcribe_batch(self, audios: Any) -> list[Any]:
            out = super().transcribe_batch(audios)
            if len(self.batches) == 4:  # last window of track 1
                stop.touch()
            return out

    assert run(cfg, conn, StopAfterTrack1(cfg, "canary")) == pilot.EXIT_STOPPED
    assert [p[0] for p in conn.inserts()] == ["/books/a/t1.m4b"]
    assert make_cfg("canary").stop_file is None


def test_failed_batch_exits_3_and_other_tracks_still_written() -> None:
    cfg = make_cfg("canary", "--batch-size", "1")
    conn = FakeConn(TRACKS, SEGMENTS)
    backend = FakeBackend(cfg, "canary", outputs=[RuntimeError("device-side assert")])
    assert run(cfg, conn, backend) == pilot.EXIT_TRACKS_FAILED
    assert [p[0] for p in conn.inserts()] == ["/books/a/t2.m4b"]


def test_degenerate_windows_are_marked() -> None:
    out = tempfile.mkdtemp(prefix="pilot-deg-")
    cfg = make_cfg("canary", "--out-dir", out)
    conn = FakeConn([("B1", "/books/a/t.m4b", TID, 76.0)], [])
    backend = FakeBackend(cfg, "canary", outputs=[" ".join(["again"] * 9), ""])
    assert run(cfg, conn, backend) == 0
    data = json.loads(next(Path(out).glob("*.json")).read_text(encoding="utf-8"))
    assert [d["reason"] for d in data["degenerate_windows"]] == ["repetition", "empty"]
    (seg,) = data["segments"]
    assert seg["degenerate"] is True and seg["degenerate_reason"] == "repetition"


# ── Scopes, limits, dry-run, bench ─────────────────────────────────────────────
def test_findings_scope_transcribes_only_finding_windows() -> None:
    cfg = make_cfg("canary", "--scope", "findings")
    conn = FakeConn([TRACKS[0]], SEGMENTS)
    conn.finding_spans = [(10.0, 20.0), (22.0, 30.0)]
    DECODES.clear()
    assert run(cfg, conn) == 0
    assert DECODES == [(5.0, 35.0)]
    row = conn.inserts()[0]
    assert row[5] == "findings" and row[10] == 30.0  # audio_sec = 5..35


def test_findings_scope_without_held_findings_writes_nothing() -> None:
    cfg = make_cfg("granite", "--scope", "findings")
    conn = FakeConn([TRACKS[0]], SEGMENTS)
    assert run(cfg, conn) == 0 and conn.inserts() == []


def test_held_finding_sql_uses_latest_unrevoked_decision() -> None:
    sql = " ".join(pilot.HELD_FINDING_SPANS_SQL.split())
    assert "JOIN LATERAL" in sql and "ORDER BY e.id DESC LIMIT 1" in sql
    assert "v.kind = 'revoke' AND v.revokes_event_id = e.id" in sql
    assert "ld.outcome = 'hold'" in sql and "f.superseded_at IS NULL" in sql
    assert "f.end_sec > f.start_sec" in sql and "LIMIT %s" in sql


def test_track_limits() -> None:
    tracks = [("B1", f"/books/a/{i}.m4b", TID, 30.0) for i in range(3)] + [
        ("B2", "/books/b/0.m4b", TID, 30.0)
    ]
    cfg = make_cfg("canary", "--track-limit-per-book", "1")
    conn = FakeConn(tracks, SEGMENTS)
    assert [t.file_path for t in pilot.select_tracks(conn, cfg)] == [
        "/books/a/0.m4b",
        "/books/b/0.m4b",
    ]
    cfg = make_cfg("canary", "--limit-tracks", "2")
    conn = FakeConn(tracks, SEGMENTS)
    conn.done = {"/books/a/0.m4b"}
    run(cfg, conn)
    assert [p[0] for p in conn.inserts()] == ["/books/a/1.m4b", "/books/a/2.m4b"]


def test_dry_run_prints_plan_honours_filters_and_writes_nothing(
    capsys: pytest.CaptureFixture[str],
) -> None:
    tracks = [("B1", f"/books/a/{i}.m4b", TID, 300.0) for i in range(4)]
    cfg = make_cfg("granite", "--dry-run", "--limit-tracks", "2")
    conn = FakeConn(tracks, SEGMENTS)
    conn.done = {"/books/a/0.m4b"}

    def no_decode(*a: Any) -> Any:
        raise AssertionError("dry-run must not decode audio")

    assert (
        pilot.run(cfg, conn, backend=None, decode=no_decode, gaming_check=no_gaming)
        == 0
    )
    assert conn.writes() == [] and conn.commits == 0
    lines = [json.loads(x) for x in capsys.readouterr().out.splitlines()]
    assert [x["file_path"] for x in lines] == ["/books/a/1.m4b", "/books/a/2.m4b"]
    assert lines[0]["windows"][-1][1] == 300.0 and lines[0]["batch_size"] == 16


def test_zero_duration_track_is_skipped_with_warning(
    caplog: pytest.LogCaptureFixture,
) -> None:
    cfg = make_cfg("granite")
    conn = FakeConn([("B1", "/books/a/z.m4b", TID, 0.0)], SEGMENTS)
    with caplog.at_level("WARNING"):
        assert run(cfg, conn) == 0
    assert conn.inserts() == []
    assert any("zero duration" in r.getMessage() for r in caplog.records)


def test_decode_is_chunked_to_hold_a_full_batch_and_bounded() -> None:
    cfg = make_cfg("granite")  # batch 16 x 240 s -> 3840 s chunks
    assert cfg.decode_chunk_sec == 3840.0
    assert make_cfg("canary").decode_chunk_sec == pilot.DECODE_CHUNK_MIN_SEC
    conn = FakeConn([("B1", "/books/a/long.m4b", TID, 7200.0)], [])
    DECODES.clear()
    backend = FakeBackend(cfg, "granite")
    assert run(cfg, conn, backend) == 0
    assert DECODES == [(0.0, 3840.0), (3840.0, 7200.0)]
    assert backend.batches == [16, 14]


def test_bench_sweeps_batch_sizes_and_writes_nothing(
    capsys: pytest.CaptureFixture[str],
) -> None:
    cfg = make_cfg(
        "canary", "--bench", "--bench-batches", "1,4", "--limit-tracks", "1",
        "--bench-max-sec", "100",
    )  # fmt: skip
    assert cfg.bench_batches == [1, 4]
    conn = FakeConn([("B1", "/books/a/long.m4b", TID, 3600.0)], [])
    backend = FakeBackend(cfg, "canary")
    assert run(cfg, conn, backend) == 0
    assert conn.writes() == [] and conn.commits == 0
    out = json.loads(capsys.readouterr().out)
    b = out["bench"]
    assert b["backend"] == "canary" and b["model_revision"] == REV
    assert b["file_path"] == "/books/a/long.m4b" and b["max_sec"] == 100.0
    assert [r["batch_size"] for r in b["results"]] == [1, 4]
    for r in b["results"]:
        assert set(r) >= {
            "batch_size", "batch_size_used", "windows", "audio_sec", "wall_sec",
            "decode_sec", "rtfx", "peak_vram_mib", "peak_reserved_mib",
        }  # fmt: skip
        assert r["windows"] == 2 and r["audio_sec"] == 76.0  # 0-38, 38-76 (<= 100 s)
    assert backend.batches == [1, 1, 2]


def test_bench_and_dry_run_are_exclusive() -> None:
    with pytest.raises(pilot.ConfigError):
        make_cfg("granite", "--bench", "--dry-run")
    for flag in ("--dry-run", "--bench"):
        with pytest.raises(pilot.ConfigError):
            make_cfg("granite", "--self-check", flag)
    with pytest.raises(pilot.ConfigError):
        make_cfg("granite", "--bench", "--bench-batches", "0,2")


# ── Write path ─────────────────────────────────────────────────────────────────
def test_out_file_written_before_db_write_and_kept_on_failure() -> None:
    out = tempfile.mkdtemp(prefix="pilot-out-")
    cfg = make_cfg("canary", "--out-dir", out)
    conn = FakeConn([TRACKS[1]], SEGMENTS)
    conn.fail_insert = True
    assert run(cfg, conn) == pilot.EXIT_TRACKS_FAILED
    (f,) = Path(out).glob("*.json")
    data = json.loads(f.read_text(encoding="utf-8"))
    assert data["segments"] and data["model_revision"] == REV


def test_dead_connection_is_replaced_before_the_write() -> None:
    cfg = make_cfg("canary")
    conn = FakeConn([TRACKS[1]], SEGMENTS)
    fresh = FakeConn([TRACKS[1]], SEGMENTS)

    class DropAfterTranscribe(FakeBackend):
        def transcribe_batch(self, audios: Any) -> list[Any]:
            conn.dead = True  # the session dies while the GPU works
            return super().transcribe_batch(audios)

    assert (
        run(cfg, conn, DropAfterTranscribe(cfg, "canary"), connect_fn=lambda: fresh)
        == 0
    )
    assert conn.inserts() == [] and conn.closed == 1
    assert [p[0] for p in fresh.inserts()] == ["/books/a/t2.m4b"]
    assert fresh.closed == 1  # run closes a connection it opened itself


def test_dead_connection_before_planning_is_replaced() -> None:
    cfg = make_cfg("canary")
    conn = FakeConn(TRACKS, SEGMENTS)
    conn.dead = True  # dropped before the run starts reading (e.g. during model load)
    fresh = FakeConn(TRACKS, SEGMENTS)
    assert run(cfg, conn, connect_fn=lambda: fresh) == 0
    assert [p[0] for p in fresh.inserts()] == ["/books/a/t1.m4b", "/books/a/t2.m4b"]
    assert not any("FROM transcripts" in q for q, _ in conn.executed)


def test_gaming_check_runs_before_the_model_loads() -> None:
    cfg = make_cfg("granite")
    conn = FakeConn(TRACKS, SEGMENTS)

    def gaming() -> None:
        raise pilot.GamingStop("state=gaming")

    # backend=None would import torch and load the model; exit 4 first proves the order.
    rc = pilot.run(cfg, conn, backend=None, decode=fake_decode, gaming_check=gaming)
    assert rc == pilot.EXIT_GAMING and conn.writes() == []


# ── Config ─────────────────────────────────────────────────────────────────────
def test_backend_defaults() -> None:
    g, c = make_cfg("granite"), make_cfg("canary")
    assert (g.model_id, g.window_sec, g.hard_max_sec, g.batch_size) == (
        "ibm-granite/granite-speech-4.1-2b", 240.0, 540.0, 16,
    )  # fmt: skip
    assert (c.model_id, c.window_sec, c.hard_max_sec, c.batch_size) == (
        "nvidia/canary-qwen-2.5b", 38.0, 40.0, 32,
    )  # fmt: skip
    assert g.repetition_penalty == 1.0 and g.scope == "full"
    assert make_cfg("granite", "--repetition-penalty", "1.3").repetition_penalty == 1.3
    with pytest.raises(pilot.ConfigError):
        make_cfg("granite", "--repetition-penalty", "0")


def test_load_config_rejects_window_above_hard_max() -> None:
    with pytest.raises(pilot.ConfigError):
        make_cfg("canary", env={"PILOT_WINDOW_SEC": "41"})
    with pytest.raises(pilot.ConfigError):
        make_cfg("granite", env={"PILOT_WINDOW_SEC": "541"})
    assert make_cfg("granite", env={"PILOT_WINDOW_SEC": "540"}).window_sec == 540.0


def test_config_errors_exit_2(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("PILOT_MODEL_REVISION", raising=False)
    monkeypatch.setenv("DATABASE_URL", "postgres://x@y/z")
    assert pilot.main(["--backend", "granite", "--asin", "B1"]) == pilot.EXIT_CONFIG
    monkeypatch.setenv("PILOT_MODEL_REVISION", REV)
    assert pilot.main(["--backend", "granite"]) == pilot.EXIT_CONFIG
    assert pilot.main(["--backend", "nemotron", "--asin", "B1"]) == pilot.EXIT_CONFIG
    monkeypatch.delenv("DATABASE_URL")
    assert pilot.main(["--backend", "canary", "--asin", "B1"]) == pilot.EXIT_CONFIG


def test_self_check_needs_no_database() -> None:
    cfg = pilot.load_config(
        ["--backend", "canary", "--self-check"], {"PILOT_MODEL_REVISION": REV}
    )
    assert cfg.self_check and cfg.database_url == ""


def test_env_overrides_and_out_dir_default() -> None:
    cfg = make_cfg(
        "granite",
        env={
            "FFMPEG_BIN": r"C:\ff\ffmpeg.exe",
            "PILOT_DEVICE": "cuda:1",
            "BOOKS_PATH_ENCODING": "sfm",
        },
    )
    assert cfg.ffmpeg_bin == r"C:\ff\ffmpeg.exe" and cfg.ffprobe_bin == "ffprobe"
    assert cfg.device == "cuda:1" and cfg.books_path_encoding == "sfm"
    assert cfg.out_dir == Path(OUT_DIR)
    bare = pilot.load_config(
        ["--backend", "granite", "--asin", "B1"],
        {"DATABASE_URL": "x", "PILOT_MODEL_REVISION": REV},
    )
    assert bare.out_dir == Path(tempfile.gettempdir()) / "earmark-asr-pilot-out"
    with pytest.raises(pilot.ConfigError):
        make_cfg("granite", env={"BOOKS_PATH_ENCODING": "utf8"})


def test_map_db_path_reroots_and_encodes() -> None:
    out = pilot.map_db_path(
        "/books/A/Part I: x.m4b",
        PureWindowsPath(r"\\nas\books"),
        PurePosixPath("/books"),
        "sfm",
    )
    assert str(out) == "\\\\nas\\books\\A\\Part I\uf022 x.m4b"
    with pytest.raises(ValueError):
        pilot.map_db_path(
            "/books/../etc", PureWindowsPath(r"\\nas\books"), PurePosixPath("/books")
        )
    with pytest.raises(ValueError):
        pilot.map_db_path(
            "/other/x.m4b", PureWindowsPath(r"\\nas\books"), PurePosixPath("/books")
        )


def test_pilot_does_not_import_runner_and_keeps_heavy_imports_lazy() -> None:
    tree = ast.parse(_PILOT_PATH.read_text(encoding="utf-8"))
    names = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            names.update(a.name.split(".")[0] for a in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module:
            names.add(node.module.split(".")[0])
    assert "runner" not in names
    top = {
        a.name.split(".")[0]
        for n in tree.body
        if isinstance(n, ast.Import)
        for a in n.names
    }
    top |= {
        n.module.split(".")[0]
        for n in tree.body
        if isinstance(n, ast.ImportFrom) and n.module
    }
    assert not top & {"torch", "transformers", "nemo", "numpy", "psycopg2"}


def test_no_timestamp_mode_left() -> None:
    src = _PILOT_PATH.read_text(encoding="utf-8")
    assert "[T:" not in src and "2b-plus" not in src


# ── Granite batch call (stubbed torch / processor / model) ─────────────────────
class _Shape(tuple):
    pass


class _FakeTensor:
    def __init__(self, data: Any) -> None:
        self.data = data
        self.shape = (
            _Shape([len(data[0])]) if data and isinstance(data[0], list) else None
        )

    def tolist(self) -> Any:
        return self.data


class _FakeInputs(dict):
    def to(self, device: str) -> Self:
        self["moved_to"] = device
        return self


class _FakeTorch:
    def __init__(self) -> None:
        self.from_numpy_calls: list[Any] = []

    def from_numpy(self, a: Any) -> Any:
        self.from_numpy_calls.append(a)
        return ("tensor", tuple(a))

    def stack(self, *a: Any, **k: Any) -> Any:
        raise AssertionError("Granite must not stack/pad the batch itself")

    def inference_mode(self) -> Any:
        import contextlib

        return contextlib.nullcontext()


def test_granite_batch_passes_unpadded_rows_and_forwards_feature_mask() -> None:
    cfg = make_cfg("granite")
    b = pilot.GraniteBackend(cfg)
    b.torch = _FakeTorch()
    b.prompt = "<chat>PROMPT"
    seen: dict[str, Any] = {}
    mask = [[True, True, True], [True, False, False]]

    def processor(text: Any, audio: Any, **kw: Any) -> Any:
        seen["text"], seen["audio"], seen["kw"] = text, audio, kw
        # input_ids: 2 left-padded rows of length 3; plus the per-row feature mask.
        return _FakeInputs(
            input_ids=_FakeTensor([[0, 5, 6], [5, 6, 7]]), input_features_mask=mask
        )

    class Model:
        def generate(self, **kw: Any) -> Any:
            seen["gen"] = kw
            return _FakeTensor([[0, 5, 6, 11, 12, 2], [5, 6, 7, 21, 2, 2]])

    class Tok:
        pad_token_id = 2
        eos_token_id = 2

        def batch_decode(self, rows: Any, **kw: Any) -> list[str]:
            seen["decoded"] = rows
            return [" First row. ", "Second."]

    b.processor, b.model, b.tokenizer = processor, Model(), Tok()
    long_row = [0.1] * (3 * pilot.SAMPLE_RATE)
    short_row = [0.2] * pilot.SAMPLE_RATE
    res = b.transcribe_batch([long_row, short_row])

    # The processor gets a LIST of 1-D rows with their own lengths, not one padded tensor.
    assert isinstance(seen["audio"], list) and len(seen["audio"]) == 2
    assert [len(r[1]) for r in seen["audio"]] == [
        3 * pilot.SAMPLE_RATE,
        pilot.SAMPLE_RATE,
    ]
    assert seen["text"] == ["<chat>PROMPT", "<chat>PROMPT"]
    assert seen["kw"]["padding"] is True and seen["kw"]["device"] == "cuda"
    # Whatever the processor returns (incl. input_features_mask) reaches generate().
    assert seen["gen"]["input_features_mask"] is mask
    assert seen["gen"]["num_beams"] == 1 and seen["gen"]["do_sample"] is False
    assert seen["gen"]["repetition_penalty"] == 1.0
    assert seen["gen"]["max_new_tokens"] == pilot.max_new_tokens(12, 64, [3.0, 1.0])
    # New tokens are sliced per row past the shared input length.
    assert seen["decoded"] == [[11, 12, 2], [21, 2, 2]]
    assert [r.text for r in res] == ["First row.", "Second."]
    assert not any(r.hit_token_limit for r in res)


def test_bench_honours_the_gaming_check_between_batches(
    capsys: pytest.CaptureFixture[str],
) -> None:
    cfg = make_cfg(
        "canary", "--bench", "--bench-batches", "1,4", "--bench-max-sec", "100"
    )
    conn = FakeConn([("B1", "/books/a/long.m4b", TID, 3600.0)], [])
    # pre-load + track + chunk + 2 batches of the first sweep step; then a game starts.
    states = iter(["available"] * 5 + ["gaming"] * 10)

    def check() -> None:
        pilot.check_gaming(
            make_cfg("canary", "--arbiter-url", "http://arb:48750"),
            fetch=lambda url: {"state": next(states)},
        )

    backend = FakeBackend(cfg, "canary")
    assert run(cfg, conn, backend, gaming_check=check) == pilot.EXIT_GAMING
    assert backend.batches == [1, 1]  # step 1 finished; step 2 never ran a batch
    assert capsys.readouterr().out == "" and conn.writes() == []


# ── Output language + --self-check on a real English clip ──────────────────────
SPANISH = "Ellos son los que se encuentran en la mayoría de los edificios de la ciudad."
EXPECTED = (
    (Path(__file__).with_name("testdata") / "selfcheck_en.txt").read_text().strip()
)


def test_language_check_flags_non_english() -> None:
    assert (
        pilot.language_reason(SPANISH) == "language"
    )  # the desktop-2 self-check output
    assert (
        pilot.language_reason("Le chat est sur la table et il ne veut pas descendre.")
        == "language"
    )
    assert (
        pilot.language_reason(
            "Der Hund ist nicht im Haus und er will auch nicht spielen."
        )
        == "language"
    )
    assert (
        pilot.language_reason("こんにちは、世界。今日はいい天気ですね。") == "language"
    )
    english = (
        "It was the best of times, it was the worst of times, it was the age of wisdom."
    )
    assert pilot.language_reason(english) is None
    # Names-only or short text proves nothing either way.
    assert (
        pilot.language_reason(
            "Kaliko, Shaggy, Ruggedo, Polychrome, Betsy, Ann, Quox, Tititi-Hoochoo."
        )
        is None
    )
    assert pilot.language_reason("Buenos días, señor.") is None
    assert pilot.degenerate_reason(SPANISH, False) == "language"


def test_word_error_rate_ignores_case_punctuation_and_mister() -> None:
    hyp = "Mr. Quilter is the apostle of the middle classes, and we are glad to welcome his gospel."
    assert pilot.word_error_rate(EXPECTED, hyp) == 0.0
    assert pilot.word_error_rate("a b c d", "a x c") == 0.5
    assert pilot.word_error_rate(EXPECTED, SPANISH) > 0.9


def test_self_check_clip_is_bundled_and_small() -> None:
    clip = pilot.SELF_CHECK_AUDIO
    assert clip.is_file() and clip.stat().st_size < 200_000
    assert clip.with_suffix(".txt").is_file()
    assert EXPECTED.startswith("MISTER QUILTER IS THE APOSTLE")


def _self_check(
    output: str, capsys: pytest.CaptureFixture[str], **extra: Any
) -> tuple[int, dict]:
    cfg = pilot.load_config(
        ["--backend", "granite", "--self-check"], {"PILOT_MODEL_REVISION": REV}
    )
    seen: list[Any] = []

    def decode(c: Any, path: Path, start: float, end: float) -> list[float]:
        seen.append((path, start, end))
        return [0.0] * 5 * pilot.SAMPLE_RATE

    rc = pilot.self_check(
        cfg,
        backend=FakeBackend(cfg, "granite", outputs=[output]),
        decode=decode,
        **extra,
    )
    assert seen == [(pilot.SELF_CHECK_AUDIO, 0.0, pilot.SELF_CHECK_MAX_SEC)]
    return rc, json.loads(capsys.readouterr().out)


def test_self_check_passes_on_the_expected_english_text(
    capsys: pytest.CaptureFixture[str],
) -> None:
    rc, info = _self_check(
        "Mister Quilter is the apostle of the middle classes, and we are glad to welcome his gospel.",
        capsys,
    )
    assert rc == pilot.EXIT_OK and info["passed"] is True and info["wer"] == 0.0
    assert info["expected"] == EXPECTED and info["degenerate_reason"] is None


def test_self_check_fails_on_spanish_or_wrong_text(
    capsys: pytest.CaptureFixture[str],
) -> None:
    rc, info = _self_check(SPANISH, capsys)
    assert rc == pilot.EXIT_SELF_CHECK_FAILED and info["passed"] is False
    assert info["degenerate_reason"] == "language"
    rc, info = _self_check(
        "The weather is nice today and the market opens at nine.", capsys
    )
    assert rc == pilot.EXIT_SELF_CHECK_FAILED and info["wer"] > pilot.SELF_CHECK_MAX_WER
    rc, info = _self_check("", capsys)
    assert rc == pilot.EXIT_SELF_CHECK_FAILED and info["degenerate_reason"] == "empty"


def test_self_check_missing_clip_is_a_config_error(tmp_path: Path) -> None:
    cfg = pilot.load_config(
        [
            "--backend",
            "canary",
            "--self-check",
            "--self-check-audio",
            str(tmp_path / "nope.flac"),
        ],
        {"PILOT_MODEL_REVISION": REV},
    )
    assert (
        pilot.self_check(cfg, backend=FakeBackend(cfg, "canary")) == pilot.EXIT_CONFIG
    )


def test_non_english_window_is_marked_degenerate_in_a_run() -> None:
    out = tempfile.mkdtemp(prefix="pilot-lang-")
    cfg = make_cfg("canary", "--out-dir", out)
    conn = FakeConn([("B1", "/books/a/t.m4b", TID, 38.0)], [])
    assert run(cfg, conn, FakeBackend(cfg, "canary", outputs=[SPANISH])) == 0
    data = json.loads(next(Path(out).glob("*.json")).read_text(encoding="utf-8"))
    assert [d["reason"] for d in data["degenerate_windows"]] == ["language"]
    assert data["segments"][0]["degenerate_reason"] == "language"


# ── Offline HF snapshots (deploy-time prefetch) ────────────────────────────────
# Copied from jedwards1230/homelab-ansible roles/asr_pilot_windows/defaults/main.yaml
# (origin/main, 2026-11-03): asr_pilot_windows_{granite,canary,canary_llm}_allow_patterns,
# *_model_revision and asr_pilot_windows_hf_ignore_patterns. If this test fails,
# pilot.py and the role disagree about what is prefetched: change both together.
ROLE_PREFETCH = {
    "granite": {
        "revision": "823ca793aa49925d4eb2e758709b7f2c98513e8e",
        "allow": ["*.json", "*.safetensors", "chat_template.jinja", "merges.txt"],
    },
    "canary": {
        "revision": "b1469e1bba1cfe140205529c79c434ca47180960",
        "allow": ["config.json", "model.safetensors"],
    },
    "canary_llm": {
        "repo_id": "Qwen/Qwen3-1.7B",
        "revision": "70d244cc86ccca08cf5af4e1e306ecf908b1ad5e",
        "allow": [
            "config.json",
            "generation_config.json",
            "tokenizer.json",
            "tokenizer_config.json",
            "vocab.json",
            "merges.txt",
        ],
    },
}
ROLE_IGNORE = [".eval_results/*", "*.nemo"]
CANARY_SHA = ROLE_PREFETCH["canary"]["revision"]
QWEN_SHA = ROLE_PREFETCH["canary_llm"]["revision"]
GRANITE_SHA = ROLE_PREFETCH["granite"]["revision"]


def test_prefetch_lists_match_the_role() -> None:
    for key, want in ROLE_PREFETCH.items():
        assert pilot.PREFETCH_ALLOW_PATTERNS[key] == want["allow"], key
    assert pilot.DEFAULT_MODEL_REVISIONS == {
        "granite": GRANITE_SHA,
        "canary": CANARY_SHA,
    }
    assert (pilot.QWEN_MODEL_ID, pilot.QWEN_REVISION) == ("Qwen/Qwen3-1.7B", QWEN_SHA)
    assert pilot.PREFETCH_IGNORE_PATTERNS == ROLE_IGNORE


def _snapshot(cache: Path, repo: str, sha: str, files: dict[str, str]) -> Path:
    snap = cache / ("models--" + repo.replace("/", "--")) / "snapshots" / sha
    for name, body in files.items():
        f = snap / name
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_text(body, encoding="utf-8")
    return snap


CANARY_CONFIG = json.dumps(
    {
        "pretrained_llm": "Qwen/Qwen3-1.7B",
        "pretrained_asr": "nvidia/canary-1b-flash",
        "pretrained_weights": True,
        "audio_locator_tag": "<|audioplaceholder|>",
    }
)
QWEN_FILES = {n: "{}" for n in ROLE_PREFETCH["canary_llm"]["allow"]}


def _hf_env(cache: Path, **extra: str) -> dict[str, str]:
    return {"PILOT_MODEL_REVISION": CANARY_SHA, "HF_HUB_CACHE": str(cache), **extra}


def test_print_prefetch_spec_needs_no_db_and_uses_the_pins(
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv("PILOT_MODEL_REVISION", raising=False)
    monkeypatch.delenv("DATABASE_URL", raising=False)
    monkeypatch.setenv("HF_HOME", "/hf")
    monkeypatch.delenv("HF_HUB_CACHE", raising=False)
    assert pilot.main(["--backend", "canary", "--print-prefetch-spec"]) == 0
    spec = json.loads(capsys.readouterr().out)
    assert spec["hf_hub_cache"] == str(Path("/hf") / "hub")
    assert spec["ignore_patterns"] == ROLE_IGNORE
    assert [
        (s["role_key"], s["repo_id"], s["revision"], s["allow_patterns"])
        for s in spec["snapshots"]
    ] == [
        (
            "canary",
            "nvidia/canary-qwen-2.5b",
            CANARY_SHA,
            ROLE_PREFETCH["canary"]["allow"],
        ),
        (
            "canary_llm",
            "Qwen/Qwen3-1.7B",
            QWEN_SHA,
            ROLE_PREFETCH["canary_llm"]["allow"],
        ),
    ]
    assert pilot.main(["--backend", "granite", "--print-prefetch-spec"]) == 0
    (g,) = json.loads(capsys.readouterr().out)["snapshots"]
    assert (g["revision"], g["allow_patterns"]) == (
        GRANITE_SHA,
        ROLE_PREFETCH["granite"]["allow"],
    )


def test_resolve_snapshot_offline_without_refs_main(tmp_path: Path) -> None:
    snap = _snapshot(tmp_path, "Qwen/Qwen3-1.7B", QWEN_SHA, QWEN_FILES)
    spec = pilot.SnapshotSpec(
        "canary_llm", "Qwen/Qwen3-1.7B", QWEN_SHA, tuple(QWEN_FILES)
    )
    assert pilot.resolve_snapshot(tmp_path, spec) == snap
    assert not (tmp_path / "models--Qwen--Qwen3-1.7B" / "refs").exists()
    # A branch name is resolved through refs/<name> when one exists.
    ref = tmp_path / "models--Qwen--Qwen3-1.7B" / "refs" / "main"
    ref.parent.mkdir(parents=True)
    ref.write_text(QWEN_SHA)
    branch = pilot.SnapshotSpec(
        "canary_llm", "Qwen/Qwen3-1.7B", "main", tuple(QWEN_FILES)
    )
    assert pilot.resolve_snapshot(tmp_path, branch) == snap


def test_resolve_snapshot_globs_and_missing_files_name_the_role(tmp_path: Path) -> None:
    _snapshot(
        tmp_path,
        "ibm-granite/granite-speech-4.1-2b",
        GRANITE_SHA,
        {"config.json": "{}", "merges.txt": ""},
    )
    spec = pilot.SnapshotSpec(
        "granite",
        "ibm-granite/granite-speech-4.1-2b",
        GRANITE_SHA,
        tuple(ROLE_PREFETCH["granite"]["allow"]),
    )
    with pytest.raises(pilot.PrefetchMissing) as err:
        pilot.resolve_snapshot(tmp_path, spec)
    msg = str(err.value)
    assert (
        "*.safetensors" in msg
        and "chat_template.jinja" in msg
        and "*.json" not in msg.split("lacks")[1]
    )
    assert (
        "asr_pilot_windows_granite_allow_patterns" in msg
        and "--print-prefetch-spec" in msg
    )
    with pytest.raises(pilot.PrefetchMissing, match="not in the HF cache"):
        pilot.resolve_snapshot(tmp_path / "empty", spec)


class _FakeSALM:
    instances: ClassVar[list[Any]] = []

    def __init__(self, cfg: dict) -> None:
        assert isinstance(cfg, dict)
        self.cfg = cfg
        self.loaded: Any = None
        _FakeSALM.instances.append(self)

    @classmethod
    def from_pretrained(cls, *a: Any, **k: Any) -> Any:
        raise AssertionError("from_pretrained would resolve Qwen by repo id")

    @classmethod
    def _load_as_safetensor(
        cls, model: Any, path: str, map_location: str, strict: bool
    ) -> Any:
        model.loaded = (path, map_location, strict)
        return model

    def to(self, device: str) -> Self:
        self.device = device
        return self

    def eval(self) -> Self:
        return self


def _stub_modules(monkeypatch: pytest.MonkeyPatch, **mods: Any) -> None:
    import types

    for name, attrs in mods.items():
        mod = types.ModuleType(name)
        for k, v in attrs.items():
            setattr(mod, k, v)
        monkeypatch.setitem(sys.modules, name, mod)


def test_canary_load_gives_salm_local_paths_only(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("HF_HUB_OFFLINE", "0")  # restored after the test
    monkeypatch.setenv("TRANSFORMERS_OFFLINE", "0")
    canary = _snapshot(
        tmp_path,
        "nvidia/canary-qwen-2.5b",
        CANARY_SHA,
        {"config.json": CANARY_CONFIG, "model.safetensors": "w"},
    )
    qwen = _snapshot(tmp_path, "Qwen/Qwen3-1.7B", QWEN_SHA, QWEN_FILES)
    _FakeSALM.instances.clear()
    _stub_modules(
        monkeypatch,
        torch={"bfloat16": "bf16"},
        nemo={},
        **{"nemo.collections": {}, "nemo.collections.speechlm2": {}},
        **{"nemo.collections.speechlm2.models": {"SALM": _FakeSALM}},
    )
    cfg = pilot.load_config(["--backend", "canary", "--self-check"], _hf_env(tmp_path))
    backend = pilot.CanaryBackend(cfg)
    backend.load()
    (m,) = _FakeSALM.instances
    assert m.cfg["pretrained_llm"] == str(qwen) and m.cfg["tokenizer_path"] == str(qwen)
    assert m.cfg["pretrained_weights"] is False and m.cfg["trust_remote_code"] is False
    assert (
        m.cfg["pretrained_asr"] == "nvidia/canary-1b-flash"
    )  # untouched; not loaded when weights are off
    assert m.loaded == (str(canary / "model.safetensors"), "cpu", False)
    assert backend.local_dirs == {"model": str(canary), "llm": str(qwen)}
    assert backend.model.device == "cuda"
    assert (
        os.environ["HF_HUB_OFFLINE"] == "1"
        and os.environ["TRANSFORMERS_OFFLINE"] == "1"
    )
    # Nothing SALM got for the LLM or tokenizer is a repo id.
    assert all(Path(m.cfg[k]).is_dir() for k in ("pretrained_llm", "tokenizer_path"))


def test_canary_load_qwen_override_and_missing_qwen(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("HF_HUB_OFFLINE", "0")  # restored after the test
    monkeypatch.setenv("TRANSFORMERS_OFFLINE", "0")
    _snapshot(
        tmp_path,
        "nvidia/canary-qwen-2.5b",
        CANARY_SHA,
        {"config.json": CANARY_CONFIG, "model.safetensors": "w"},
    )
    _stub_modules(
        monkeypatch,
        torch={},
        nemo={},
        **{"nemo.collections": {}, "nemo.collections.speechlm2": {}},
        **{"nemo.collections.speechlm2.models": {"SALM": _FakeSALM}},
    )
    other = "1" * 40
    cfg = pilot.load_config(
        ["--backend", "canary", "--self-check"],
        _hf_env(tmp_path, PILOT_QWEN_REVISION=other),
    )
    assert cfg.qwen_revision == other
    with pytest.raises(
        pilot.PrefetchMissing, match="asr_pilot_windows_canary_llm_allow_patterns"
    ):
        pilot.CanaryBackend(cfg).load()


def test_granite_load_uses_the_local_snapshot_dir(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("HF_HUB_OFFLINE", "0")  # restored after the test
    monkeypatch.setenv("TRANSFORMERS_OFFLINE", "0")
    files = {
        "config.json": "{}",
        "model-00001-of-00003.safetensors": "w",
        "chat_template.jinja": "",
        "merges.txt": "",
    }
    hub = tmp_path / "home" / "hub"  # HF_HOME/hub when HF_HUB_CACHE is unset
    local = _snapshot(hub, "ibm-granite/granite-speech-4.1-2b", GRANITE_SHA, files)
    seen: dict[str, Any] = {}

    class Tok:
        padding_side = "right"

        def apply_chat_template(self, chat: Any, **k: Any) -> str:
            return "PROMPT"

    class Proc:
        tokenizer = Tok()

    class AutoProcessor:
        @staticmethod
        def from_pretrained(path: str, **k: Any) -> Any:
            seen["processor"] = (path, k)
            return Proc()

    class Model:
        def eval(self) -> None:
            return None

    class AutoModelForSpeechSeq2Seq:
        @staticmethod
        def from_pretrained(path: str, **k: Any) -> Any:
            seen["model"] = (path, k)
            return Model()

    _stub_modules(
        monkeypatch,
        torch={"bfloat16": "bf16"},
        transformers={
            "AutoProcessor": AutoProcessor,
            "AutoModelForSpeechSeq2Seq": AutoModelForSpeechSeq2Seq,
        },
    )
    cfg = pilot.load_config(
        ["--backend", "granite", "--self-check"],
        {"PILOT_MODEL_REVISION": GRANITE_SHA, "HF_HOME": str(tmp_path / "home")},
    )
    backend = pilot.GraniteBackend(cfg)
    backend.load()
    assert seen["processor"] == (str(local), {})
    path, kw = seen["model"]
    assert path == str(local) and "revision" not in kw and kw["dtype"] == "bf16"
    assert backend.tokenizer.padding_side == "left" and backend.prompt == "PROMPT"


# ── --bench window gathering ───────────────────────────────────────────────────
BENCH_TRACKS = [
    ("B1", "/books/a/1.m4b", TID, 595.6),  # 3 granite windows (240 + 240 + 115.6)
    ("B1", "/books/a/2.m4b", TID, 1200.0),  # 5
    ("B2", "/books/b/1.m4b", TID, 2400.0),  # 10
]


def test_bench_default_cap_follows_batches_and_window() -> None:
    assert make_cfg("granite", "--bench").bench_max_sec == 32 * 2 * 240.0  # 15360 s
    assert (
        make_cfg("canary", "--bench", "--bench-batches", "1,4").bench_max_sec
        == 4 * 2 * 38.0
    )
    assert (
        make_cfg("granite", "--bench", "--bench-max-sec", "500").bench_max_sec == 500.0
    )


def test_bench_gathers_windows_across_tracks_until_enough() -> None:
    cfg = make_cfg("granite", "--bench", "--bench-batches", "1,4")  # wants 8 windows
    plans = pilot.gather_bench_plans(
        FakeConn(BENCH_TRACKS, []),
        cfg,
        pilot.select_tracks(FakeConn(BENCH_TRACKS, []), cfg),
    )
    assert [(t.file_path, len(p)) for t, p in plans] == [
        ("/books/a/1.m4b", 3),
        ("/books/a/2.m4b", 5),
    ]


def test_bench_gathering_respects_the_audio_cap() -> None:
    cfg = make_cfg(
        "granite", "--bench", "--bench-batches", "1,32", "--bench-max-sec", "1000"
    )
    conn = FakeConn(BENCH_TRACKS, [])
    plans = pilot.gather_bench_plans(conn, cfg, pilot.select_tracks(conn, cfg))
    # 595.6 s of track 1, then 240 s of track 2; the next 240 s would cross 1000.
    assert [(t.file_path, p) for t, p in plans] == [
        ("/books/a/1.m4b", [(0.0, 240.0), (240.0, 480.0), (480.0, 595.6)]),
        ("/books/a/2.m4b", [(0.0, 240.0)]),
    ]
    tiny = make_cfg("granite", "--bench", "--bench-max-sec", "10")
    ((_t, p),) = pilot.gather_bench_plans(conn, tiny, pilot.select_tracks(conn, tiny))
    assert len(p) == 1  # a cap below one window still benches one window


def test_bench_rows_report_batches_and_starvation(
    capsys: pytest.CaptureFixture[str], caplog: pytest.LogCaptureFixture
) -> None:
    cfg = make_cfg(
        "granite", "--bench", "--bench-batches", "1,4,16", "--bench-max-sec", "1000",
        "--limit-tracks", "1",
    )  # fmt: skip
    conn = FakeConn(BENCH_TRACKS, [])
    backend = FakeBackend(cfg, "granite")
    with caplog.at_level("WARNING"):
        assert run(cfg, conn, backend) == 0
    b = json.loads(capsys.readouterr().out)["bench"]
    # --limit-tracks is ignored in bench mode: two tracks were gathered.
    assert b["tracks"] == ["/books/a/1.m4b", "/books/a/2.m4b"]
    assert b["file_path"] == "/books/a/1.m4b" and b["windows"] == 4
    assert b["windows_wanted"] == 32 and b["max_sec"] == 1000.0
    rows = {r["batch_size"]: r for r in b["results"]}
    assert set(rows) == {1, 4, 16}
    for r in rows.values():
        assert set(r) == {
            "batch_size", "batch_size_used", "tracks", "windows", "batches",
            "windows_per_batch", "starved", "audio_sec", "wall_sec", "decode_sec",
            "rtfx", "peak_vram_mib", "peak_reserved_mib", "degenerate_windows",
        }  # fmt: skip
        assert r["tracks"] == 2 and r["windows"] == 4 and r["audio_sec"] == 835.6
    assert (rows[1]["batches"], rows[1]["windows_per_batch"]) == (
        4,
        {"mean": 1.0, "min": 1},
    )
    # Batches span tracks: 3 windows of track 1 + 1 of track 2 fill one batch of 4.
    assert (rows[4]["batches"], rows[4]["windows_per_batch"], rows[4]["starved"]) == (
        1, {"mean": 4.0, "min": 4}, False,
    )  # fmt: skip
    assert rows[16]["starved"] is True and rows[1]["starved"] is False
    assert any("batch_size=16 is starved" in r.getMessage() for r in caplog.records)
    assert conn.writes() == []


def test_print_prefetch_spec_refuses_a_default_revision_for_another_model() -> None:
    with pytest.raises(pilot.ConfigError, match="PILOT_MODEL_REVISION"):
        pilot.load_config(
            ["--backend", "granite", "--print-prefetch-spec"],
            {"PILOT_MODEL_ID": "ibm-granite/granite-speech-4.1-2b-nar"},
        )
    cfg = pilot.load_config(
        ["--backend", "granite", "--print-prefetch-spec"],
        {
            "PILOT_MODEL_ID": "ibm-granite/granite-speech-4.1-2b-nar",
            "PILOT_MODEL_REVISION": "a" * 40,
        },
    )
    assert pilot.prefetch_spec(cfg)["snapshots"][0]["revision"] == "a" * 40


def test_missing_prefetch_during_load_is_a_config_error(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    cfg = make_cfg("canary", env={"HF_HUB_CACHE": str(tmp_path)})
    conn = FakeConn(TRACKS, SEGMENTS)

    class Unfetched(FakeBackend):
        def load(self) -> None:
            pilot.resolve_snapshot(tmp_path, pilot.snapshot_specs(self.cfg)[0])

    backend = Unfetched(cfg, "canary")
    # run() loads the model itself only when no backend is passed; patch the factory.
    monkeypatch.setattr(pilot, "make_backend", lambda c: backend)
    rc = pilot.run(cfg, conn, backend=None, decode=fake_decode, gaming_check=no_gaming)
    assert rc == pilot.EXIT_CONFIG and conn.writes() == []


def test_bench_skips_an_unplannable_track_and_fails_when_none_plan(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    cfg = make_cfg(
        "canary", "--bench", "--bench-batches", "1", "--bench-max-sec", "100"
    )
    conn = FakeConn(BENCH_TRACKS, [])
    real = pilot.track_plan

    def flaky(c: Any, cf: Any, track: Any) -> Any:
        if track.file_path == "/books/a/1.m4b":
            raise OSError("server closed the connection")
        return real(c, cf, track)

    monkeypatch.setattr(pilot, "track_plan", flaky)
    assert run(cfg, conn) == 0
    assert json.loads(capsys.readouterr().out)["bench"]["tracks"] == ["/books/a/2.m4b"]

    def broken(*a: Any) -> Any:
        raise OSError("nope")

    monkeypatch.setattr(pilot, "track_plan", broken)
    assert run(cfg, FakeConn(BENCH_TRACKS, [])) == pilot.EXIT_TRACKS_FAILED
    assert capsys.readouterr().out == ""
