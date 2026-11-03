#!/usr/bin/env python3
"""
Unit tests for runner/pilot.py (the one-shot second-ASR pilot).

No torch, transformers, NeMo, numpy, ffmpeg or database: the model backend, the
audio decoder and the Postgres connection are all fakes, so this runs anywhere:

    python -m pytest runner/test_pilot.py -v
"""

from __future__ import annotations

import importlib.util
import itertools
import json
import sys
from pathlib import Path
from typing import Any, Self

import pytest

_PILOT_PATH = Path(__file__).with_name("pilot.py")
_spec = importlib.util.spec_from_file_location("asr_pilot_under_test", _PILOT_PATH)
assert _spec and _spec.loader
pilot = importlib.util.module_from_spec(_spec)
sys.modules[_spec.name] = pilot  # dataclasses resolve annotations via sys.modules
_spec.loader.exec_module(pilot)

REV = "e69efe37f97b6283c61f5a8dadda2b3eb67765d6"
TID = "00000000-0000-0000-0000-000000000001"


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
        self.conn.executed.append((sql, params))

    def fetchall(self) -> list[Any]:
        if "FROM book_metadata" in self.sql:
            return list(self.conn.tracks)
        if "FROM transcript_findings" in self.sql:
            return list(self.conn.finding_spans)
        raise AssertionError(f"unexpected fetchall for {self.sql}")

    def fetchone(self) -> Any:
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
        self.rollbacks = 0

    def cursor(self) -> FakeCursor:
        return FakeCursor(self)

    def commit(self) -> None:
        self.commits += 1

    def rollback(self) -> None:
        self.rollbacks += 1

    def writes(self) -> list[tuple[str, Any]]:
        return [
            (s, p)
            for s, p in self.executed
            if s.lstrip().startswith(("INSERT", "DELETE", "UPDATE"))
        ]

    def inserts(self) -> list[Any]:
        return [p for s, p in self.executed if s.lstrip().startswith("INSERT")]


class FakeBackend(pilot.Backend):
    def __init__(self, cfg: Any, name: str, outputs: list[Any] | None = None) -> None:
        super().__init__(cfg)
        self.name = name
        self.has_word_times = name == "granite"
        self.outputs = outputs
        self.calls: list[tuple[int, float]] = []

    def load(self) -> None:
        raise AssertionError("tests never load a real model")

    def transcribe_window(self, audio: Any, window_start: float) -> Any:
        self.calls.append((len(audio), window_start))
        if self.outputs:
            out = self.outputs.pop(0)
            if isinstance(out, Exception):
                raise out
            text = out
        else:
            text = (
                "hello [T:50] world [T:120]" if self.has_word_times else "Hello, World."
            )
        if self.has_word_times:
            words = pilot.parse_granite_timestamps(
                text, window_start, len(audio) / pilot.SAMPLE_RATE
            )
            return pilot.WindowResult(text=text, words=words)
        return pilot.WindowResult(text=text, words=[])


def fake_decode(cfg: Any, path: Path, duration: float) -> list[float]:
    return [0.0] * int(duration * pilot.SAMPLE_RATE)


def make_cfg(
    backend: str = "granite", *extra: str, env: dict[str, str] | None = None
) -> Any:
    base = {
        "DATABASE_URL": "postgres://x@y/z",
        "PILOT_MODEL_REVISION": REV,
        "BOOKS_MOUNT": "/books",
    }
    base.update(env or {})
    return pilot.load_config(["--backend", backend, "--asin", "B1", *extra], base)


def no_gaming() -> None:
    return None


# ── Granite timestamp parsing ─────────────────────────────────────────────────
def test_granite_parse_simple_offsets_by_window_start() -> None:
    words = pilot.parse_granite_timestamps("hello [T:45] world [T:82]", 100.0, 90.0)
    assert [(w["word"], w["start"], w["end"]) for w in words] == [
        ("hello", 100.0, 100.45),
        ("world", 100.45, 100.82),
    ]
    assert all(w["score"] is None and w["speaker"] is None for w in words)


def test_granite_parse_unrolls_multiple_wraps() -> None:
    # 9.9 s -> wraps to 0.20 (10.2 s) -> 9.95 (19.95 s) -> 0.10 (20.1 s) -> 0.50 (20.5 s)
    text = "a [T:990] b [T:20] c [T:995] d [T:10] e [T:50]"
    words = pilot.parse_granite_timestamps(text, 0.0, 60.0)
    assert [w["end"] for w in words] == [9.9, 10.2, 19.95, 20.1, 20.5]
    assert [w["start"] for w in words] == [0.0, 9.9, 10.2, 19.95, 20.1]


def test_granite_parse_drops_silence_but_advances_clock() -> None:
    words = pilot.parse_granite_timestamps(
        "one [T:100] _ [T:300] two [T:350]", 0.0, 10.0
    )
    assert [w["word"] for w in words] == ["one", "two"]
    # "two" starts at the silence's end, not at "one"'s end.
    assert words[1]["start"] == 3.0 and words[1]["end"] == 3.5


def test_granite_parse_silence_across_wrap() -> None:
    words = pilot.parse_granite_timestamps("x [T:900] _ [T:150] y [T:200]", 0.0, 60.0)
    assert [(w["word"], w["start"], w["end"]) for w in words] == [
        ("x", 0.0, 9.0),
        ("y", 11.5, 12.0),
    ]


def test_granite_parse_clamps_to_window_and_handles_untagged_tail() -> None:
    words = pilot.parse_granite_timestamps("a [T:500] b c", 10.0, 6.0)
    assert words[0]["end"] == 15.0
    assert [w["word"] for w in words[1:]] == ["b", "c"]
    assert words[-1]["end"] == 16.0  # tail runs to the window end
    assert all(w["end"] <= 16.0 for w in words)


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
    # One big gap inside [67.5, 90] and an even bigger one before it (must be ignored).
    words = [
        w for w in words if not (50.0 <= w[0] < 52.0) and not (80.0 <= w[0] < 81.0)
    ]
    plan = pilot.plan_windows(0.0, 200.0, words, 90.0, 210.0)
    first_cut = plan[0][1]
    assert 79.8 < first_cut < 81.0  # midpoint of the 79.8..81.0 gap, not the 50 s one
    assert plan[-1][1] == 200.0
    for (_s0, e0), (s1, _e1) in itertools.pairwise(plan):
        assert e0 == s1  # contiguous, no audio dropped
    # No production word straddles a cut.
    for _, cut in plan[:-1]:
        assert not any(s < cut < e for s, e in words)


def test_plan_never_exceeds_hard_max() -> None:
    plan = pilot.plan_windows(0.0, 1000.0, [], 500.0, 210.0)
    assert all(e - s <= 210.0 + 1e-9 for s, e in plan)
    plan = pilot.plan_windows(0.0, 400.0, _words_every(0.5, 400.0), 40.0, 40.0)
    assert all(e - s <= 40.0 + 1e-9 for s, e in plan)


def test_plan_without_words_falls_back_to_fixed_cuts() -> None:
    plan = pilot.plan_windows(0.0, 250.0, [], 90.0, 210.0)
    assert plan == [(0.0, 90.0), (90.0, 180.0), (180.0, 250.0)]


def test_plan_short_track_single_window_and_empty() -> None:
    assert pilot.plan_windows(0.0, 30.0, [], 90.0, 210.0) == [(0.0, 30.0)]
    assert pilot.plan_windows(5.0, 5.0, [], 90.0, 210.0) == []


def test_load_config_rejects_window_above_hard_max() -> None:
    with pytest.raises(pilot.ConfigError):
        make_cfg("canary", env={"PILOT_WINDOW_SEC": "41"})
    with pytest.raises(pilot.ConfigError):
        make_cfg("granite", env={"PILOT_WINDOW_SEC": "211"})
    assert make_cfg("granite").window_sec == 90.0
    assert make_cfg("canary").window_sec == 38.0


# ── findings scope: span merging ───────────────────────────────────────────────
def test_merge_spans_pads_merges_and_clamps() -> None:
    spans = [(100.0, 200.0), (203.0, 300.0), (500.0, 510.0), (2.0, 4.0)]
    merged = pilot.merge_spans(spans, 5.0, 512.0)
    assert merged == [(0.0, 9.0), (95.0, 305.0), (495.0, 512.0)]


def test_merge_spans_touching_and_contained() -> None:
    assert pilot.merge_spans([(10.0, 20.0), (30.0, 40.0)], 5.0, 100.0) == [(5.0, 45.0)]
    assert pilot.merge_spans([(10.0, 90.0), (20.0, 30.0)], 0.0, 100.0) == [(10.0, 90.0)]
    assert pilot.merge_spans([], 5.0, 100.0) == []


def test_plan_track_findings_windows_each_merged_span() -> None:
    plan = pilot.plan_track(
        1000.0, [], 40.0, 40.0, "findings", [(100.0, 181.0), (600.0, 610.0)]
    )
    assert plan[0][0] == 95.0 and plan[-1] == (595.0, 615.0)
    assert all(e - s <= 40.0 for s, e in plan)
    covered = [w for w in plan if w[1] <= 186.0]
    assert covered[-1][1] == 186.0


# ── Repetition guard ───────────────────────────────────────────────────────────
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
    assert pilot.has_repetition_loop(["a", "b"] * 5)
    assert not pilot.has_repetition_loop(["a", "b"] * 4)


def test_degenerate_window_is_marked() -> None:
    cfg = make_cfg("granite")
    loop = " ".join(f"again [T:{i * 10}]" for i in range(1, 20))
    backend = FakeBackend(cfg, "granite", outputs=[loop])
    result = pilot.transcribe_track(
        cfg, backend, [0.0] * 30 * 16000, [(0.0, 30.0)], no_gaming
    )
    assert result["degenerate_windows"] == [0]
    assert all(s.get("degenerate") is True for s in result["segments"])


def test_granite_max_new_tokens_scales_with_window() -> None:
    b = pilot.GraniteBackend(make_cfg("granite"))
    assert b.max_new_tokens(90.0) == int(48 * 90 * 1.1)
    assert b.max_new_tokens(2.0) == 256


# ── Segments shape ─────────────────────────────────────────────────────────────
def test_granite_segments_shape() -> None:
    cfg = make_cfg("granite")
    backend = FakeBackend(cfg, "granite")
    res = pilot.transcribe_track(
        cfg, backend, [0.0] * 200 * 16000, [(0.0, 100.0), (100.0, 200.0)], no_gaming
    )
    segs = res["segments"]
    assert res["has_word_times"] is True
    assert [s["id"] for s in segs] == list(range(len(segs)))
    for s in segs:
        assert set(s) == {"id", "start", "end", "text", "speaker", "words"}
        assert s["speaker"] is None and s["end"] >= s["start"]
        for w in s["words"]:
            assert set(w) == {"word", "start", "end", "score", "speaker"}
    assert segs[-1]["words"][-1]["end"] == 101.2  # offset by the 2nd window start
    assert res["raw_text"] == "hello world hello world"
    json.dumps(segs)  # JSONB-serialisable


def test_canary_segments_shape_one_per_window_no_word_times() -> None:
    cfg = make_cfg("canary")
    backend = FakeBackend(cfg, "canary", outputs=["Hello, World.", "  ", "Dr. Who?"])
    plan = [(0.0, 38.0), (38.0, 76.0), (76.0, 100.0)]
    res = pilot.transcribe_track(cfg, backend, [0.0] * 100 * 16000, plan, no_gaming)
    segs = res["segments"]
    assert res["has_word_times"] is False
    assert [(s["id"], s["start"], s["end"], s["text"]) for s in segs] == [
        (0, 0.0, 38.0, "Hello, World."),
        (1, 76.0, 100.0, "Dr. Who?"),
    ]
    for s in segs:
        for w in s["words"]:
            assert set(w) == {"word", "score", "speaker"}  # NO start/end keys
    assert segs[0]["words"][0]["word"] == "Hello,"  # casing + punctuation verbatim
    assert res["audio_sec"] == 100.0


# ── run(): idempotency, gaming, failures, dry-run ──────────────────────────────
TRACKS = [("B1", "/books/a/t1.m4b", TID, 120.0), ("B1", "/books/a/t2.m4b", TID, 60.0)]
SEGMENTS = [
    {
        "words": [
            {"word": "x", "start": 1.0, "end": 1.5},
            {"word": "y", "start": 80.0, "end": 80.4},
        ]
    }
]


def test_run_writes_one_row_per_track_in_its_own_commit() -> None:
    cfg = make_cfg("granite")
    conn = FakeConn(TRACKS, SEGMENTS)
    backend = FakeBackend(cfg, "granite")
    rc = pilot.run(
        cfg, conn, backend=backend, decode=fake_decode, gaming_check=no_gaming
    )
    assert rc == pilot.EXIT_OK
    inserts = conn.inserts()
    assert [p[0] for p in inserts] == ["/books/a/t1.m4b", "/books/a/t2.m4b"]
    assert conn.commits == 2
    row = inserts[0]
    assert row[1] == TID and row[2] == "granite" and row[4] == REV and row[5] == "full"
    assert row[7] is True and json.loads(row[8])[0]["words"]
    # Only pilot_transcripts is ever written.
    assert all("pilot_transcripts" in s for s, _ in conn.writes())


def test_run_skips_tracks_already_done_unless_forced() -> None:
    cfg = make_cfg("granite")
    conn = FakeConn(TRACKS, SEGMENTS)
    conn.done = {"/books/a/t1.m4b"}
    backend = FakeBackend(cfg, "granite")
    assert (
        pilot.run(
            cfg, conn, backend=backend, decode=fake_decode, gaming_check=no_gaming
        )
        == 0
    )
    assert [p[0] for p in conn.inserts()] == ["/books/a/t2.m4b"]

    cfg_f = make_cfg("granite", "--force")
    conn_f = FakeConn(TRACKS, SEGMENTS)
    conn_f.done = {"/books/a/t1.m4b", "/books/a/t2.m4b"}
    assert (
        pilot.run(
            cfg_f,
            conn_f,
            backend=FakeBackend(cfg_f, "granite"),
            decode=fake_decode,
            gaming_check=no_gaming,
        )
        == 0
    )
    kinds = [s.split()[0] for s, _ in conn_f.writes()]
    assert kinds == ["DELETE", "INSERT", "DELETE", "INSERT"]


def test_run_all_done_loads_no_model() -> None:
    cfg = make_cfg("granite")
    conn = FakeConn(TRACKS, SEGMENTS)
    conn.done = {t[1] for t in TRACKS}
    # backend=None would call make_backend().load(); returning before that proves no load.
    assert (
        pilot.run(cfg, conn, backend=None, decode=fake_decode, gaming_check=no_gaming)
        == 0
    )
    assert conn.writes() == []


def test_gaming_between_windows_exits_4_and_writes_nothing_partial() -> None:
    cfg = make_cfg("granite", "--arbiter-url", "http://arb:48750/")
    assert cfg.arbiter_url == "http://arb:48750"
    conn = FakeConn(TRACKS, SEGMENTS)
    backend = FakeBackend(cfg, "granite")
    states = iter(["available"] * 4 + ["gaming"] * 10)

    def check() -> None:
        pilot.check_gaming(cfg, fetch=lambda url: {"state": next(states)})

    rc = pilot.run(cfg, conn, backend=backend, decode=fake_decode, gaming_check=check)
    assert rc == pilot.EXIT_GAMING
    # Track 1 (120 s -> 2 windows, 3 checks) finished and was written; track 2 stopped.
    assert [p[0] for p in conn.inserts()] == ["/books/a/t1.m4b"]


def test_check_gaming_states_and_unreachable() -> None:
    cfg = make_cfg("granite", "--arbiter-url", "http://arb:48750")
    pilot.check_gaming(cfg, fetch=lambda url: {"state": "available"})
    for state in ("gaming", "evicting"):
        with pytest.raises(pilot.GamingStop):
            pilot.check_gaming(cfg, fetch=lambda url, s=state: {"state": s})

    def boom(url: str) -> dict[str, Any]:
        raise OSError("connection refused")

    with pytest.raises(pilot.GamingStop):
        pilot.check_gaming(cfg, fetch=boom)
    pilot.check_gaming(
        make_cfg("granite"), fetch=boom
    )  # no --arbiter-url: never checked


def test_failed_track_exits_3_and_other_tracks_still_written() -> None:
    cfg = make_cfg("canary")
    conn = FakeConn(TRACKS, SEGMENTS)
    backend = FakeBackend(
        cfg, "canary", outputs=[RuntimeError("CUDA OOM")] + ["Fine."] * 10
    )
    rc = pilot.run(
        cfg, conn, backend=backend, decode=fake_decode, gaming_check=no_gaming
    )
    assert rc == pilot.EXIT_TRACKS_FAILED
    assert [p[0] for p in conn.inserts()] == ["/books/a/t2.m4b"]


def test_findings_scope_transcribes_only_finding_windows() -> None:
    cfg = make_cfg("canary", "--scope", "findings")
    conn = FakeConn([TRACKS[0]], SEGMENTS)
    conn.finding_spans = [(10.0, 20.0), (22.0, 30.0)]
    backend = FakeBackend(cfg, "canary")
    assert (
        pilot.run(
            cfg, conn, backend=backend, decode=fake_decode, gaming_check=no_gaming
        )
        == 0
    )
    assert [round(start, 2) for _, start in backend.calls] == [5.0]
    row = conn.inserts()[0]
    assert row[5] == "findings" and row[10] == 30.0  # audio_sec = 5..35


def test_findings_scope_without_held_findings_writes_nothing() -> None:
    cfg = make_cfg("granite", "--scope", "findings")
    conn = FakeConn([TRACKS[0]], SEGMENTS)
    assert (
        pilot.run(
            cfg,
            conn,
            backend=FakeBackend(cfg, "granite"),
            decode=fake_decode,
            gaming_check=no_gaming,
        )
        == 0
    )
    assert conn.inserts() == []


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
    pilot.run(
        cfg,
        conn,
        backend=FakeBackend(cfg, "canary"),
        decode=fake_decode,
        gaming_check=no_gaming,
    )
    assert [p[0] for p in conn.inserts()] == ["/books/a/1.m4b", "/books/a/2.m4b"]


def test_dry_run_prints_plan_and_writes_nothing(
    capsys: pytest.CaptureFixture[str],
) -> None:
    cfg = make_cfg("granite", "--dry-run")
    conn = FakeConn(TRACKS, SEGMENTS)

    def no_decode(*a: Any) -> Any:
        raise AssertionError("dry-run must not decode audio")

    assert (
        pilot.run(cfg, conn, backend=None, decode=no_decode, gaming_check=no_gaming)
        == 0
    )
    assert conn.writes() == [] and conn.commits == 0
    lines = [json.loads(x) for x in capsys.readouterr().out.splitlines()]
    assert [x["file_path"] for x in lines] == ["/books/a/t1.m4b", "/books/a/t2.m4b"]
    assert lines[0]["windows"][-1][1] == 120.0 and lines[0]["production_words"] == 2


# ── Config / exit codes ────────────────────────────────────────────────────────
def test_config_errors_exit_2(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("PILOT_MODEL_REVISION", raising=False)
    monkeypatch.setenv("DATABASE_URL", "postgres://x@y/z")
    assert (
        pilot.main(["--backend", "granite", "--asin", "B1"]) == pilot.EXIT_CONFIG
    )  # unpinned
    monkeypatch.setenv("PILOT_MODEL_REVISION", REV)
    assert pilot.main(["--backend", "granite"]) == pilot.EXIT_CONFIG  # no --asin
    assert pilot.main(["--backend", "nemotron", "--asin", "B1"]) == pilot.EXIT_CONFIG
    monkeypatch.delenv("DATABASE_URL")
    assert pilot.main(["--backend", "canary", "--asin", "B1"]) == pilot.EXIT_CONFIG


def test_self_check_needs_no_database() -> None:
    cfg = pilot.load_config(
        ["--backend", "canary", "--self-check"], {"PILOT_MODEL_REVISION": REV}
    )
    assert (
        cfg.self_check
        and cfg.database_url == ""
        and cfg.model_id == "nvidia/canary-qwen-2.5b"
    )


def test_env_defaults_and_overrides() -> None:
    cfg = make_cfg(
        "granite",
        env={
            "FFMPEG_BIN": r"C:\ff\ffmpeg.exe",
            "PILOT_DEVICE": "cuda:1",
            "BOOKS_PATH_ENCODING": "sfm",
        },
    )
    assert cfg.model_id == "ibm-granite/granite-speech-4.1-2b-plus"
    assert cfg.ffmpeg_bin == r"C:\ff\ffmpeg.exe" and cfg.ffprobe_bin == "ffprobe"
    assert cfg.device == "cuda:1" and cfg.books_path_encoding == "sfm"
    with pytest.raises(pilot.ConfigError):
        make_cfg("granite", env={"BOOKS_PATH_ENCODING": "utf8"})


def test_map_db_path_reroots_and_encodes() -> None:
    from pathlib import PurePosixPath, PureWindowsPath

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


def test_pilot_does_not_import_runner() -> None:
    import ast

    tree = ast.parse(_PILOT_PATH.read_text(encoding="utf-8"))
    names = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            names.update(a.name.split(".")[0] for a in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module:
            names.add(node.module.split(".")[0])
    assert "runner" not in names
    # Heavy stacks stay lazy: never imported at module top level.
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
