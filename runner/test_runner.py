#!/usr/bin/env python3
"""
Unit tests for the asr-runner claim gate (runner_control: paused + run_limit).

Focused on the trickiest logic — the bounded-run decrement-on-claim — without a
real database or the heavy ML deps. psycopg2 is stubbed in sys.modules and the
module's claim SQL is exercised against a fake cursor, so this runs anywhere:

    python3 roles/asr-runner/files/test_runner.py
"""

from __future__ import annotations

import importlib.util
import os
import sys
import types
import unittest
from pathlib import Path

# ── Stub heavy/optional imports so runner.py imports with no DB or drivers ──────
os.environ.setdefault("DATABASE_URL", "postgres://test@localhost/test")
for name in ("psycopg2", "psycopg2.extras"):
    if name not in sys.modules:
        mod = types.ModuleType(name)
        if name == "psycopg2":
            mod.extras = types.ModuleType("psycopg2.extras")  # type: ignore[attr-defined]
            mod.extensions = types.ModuleType("psycopg2.extensions")  # type: ignore[attr-defined]
        sys.modules[name] = mod

_RUNNER_PATH = Path(__file__).with_name("runner.py")
_spec = importlib.util.spec_from_file_location("asr_runner_under_test", _RUNNER_PATH)
assert _spec and _spec.loader
runner = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(runner)


# ── Fake DB plumbing ───────────────────────────────────────────────────────────
class FakeCursor:
    def __init__(self, conn: "FakeConn") -> None:
        self.conn = conn

    def __enter__(self) -> "FakeCursor":
        return self

    def __exit__(self, *exc: object) -> None:
        return None

    def execute(self, sql: str, params: object = None) -> None:
        self.conn.executed.append((sql, params))
        self.conn.last_sql = sql

    def fetchone(self) -> object:
        # Route the result by which statement just ran.
        if "FROM runner_control" in self.conn.last_sql:
            return self.conn.control_row
        if "UPDATE transcription_jobs" in self.conn.last_sql:
            return self.conn.job_row
        return None


class FakeConn:
    def __init__(self, control_row: object, job_row: object) -> None:
        self.control_row = control_row
        self.job_row = job_row
        self.executed: list[tuple[str, object]] = []
        self.last_sql = ""
        self.commits = 0
        self.rollbacks = 0

    def cursor(self) -> FakeCursor:
        return FakeCursor(self)

    def commit(self) -> None:
        self.commits += 1

    def rollback(self) -> None:
        self.rollbacks += 1

    # Helpers for assertions.
    def claim_ran(self) -> bool:
        return any("UPDATE transcription_jobs" in s for s, _ in self.executed)

    def decrement_ran(self) -> bool:
        return any("run_limit = run_limit - 1" in s for s, _ in self.executed)


class GateTests(unittest.TestCase):
    def setUp(self) -> None:
        # Ensure the host-local busy flag is absent for all gate tests.
        runner.BUSY_FLAG_PATH = Path("/nonexistent/earmark-asr-busy-test")

    def test_control_values_parsing(self) -> None:
        self.assertEqual(runner._control_values(None), (False, None))
        self.assertEqual(
            runner._control_values({"paused": True, "run_limit": 3}), (True, 3)
        )
        self.assertEqual(
            runner._control_values({"paused": False, "run_limit": None}), (False, None)
        )
        self.assertEqual(runner._control_values((True, 2)), (True, 2))

    def test_paused_skips_claim(self) -> None:
        conn = FakeConn(control_row={"paused": True, "run_limit": None}, job_row={"id": "j"})
        self.assertIsNone(runner._claim_job(conn))
        self.assertFalse(conn.claim_ran())
        self.assertEqual(conn.commits, 1)  # lock released

    def test_run_limit_zero_skips_claim(self) -> None:
        conn = FakeConn(control_row={"paused": False, "run_limit": 0}, job_row={"id": "j"})
        self.assertIsNone(runner._claim_job(conn))
        self.assertFalse(conn.claim_ran())
        self.assertFalse(conn.decrement_ran())

    def test_negative_run_limit_skips_claim(self) -> None:
        # A run_limit that somehow went negative must also gate (run_limit <= 0).
        conn = FakeConn(control_row={"paused": False, "run_limit": -1}, job_row={"id": "j"})
        self.assertIsNone(runner._claim_job(conn))
        self.assertFalse(conn.claim_ran())

    def test_control_read_exception_degrades_safe(self) -> None:
        # When the control read fails (e.g. runner_control doesn't exist yet on
        # first deploy), roll back and claim without gating so the runner works.
        class FailingCursor(FakeCursor):
            def execute(self, sql: str, params: object = None) -> None:
                if "FROM runner_control" in sql:
                    raise RuntimeError('relation "runner_control" does not exist')
                super().execute(sql, params)

        class FailingConn(FakeConn):
            def cursor(self) -> FakeCursor:
                return FailingCursor(self)

        job = {"id": "j", "file_path": "/x.m4b", "checksum": "abc"}
        conn = FailingConn(control_row=None, job_row=job)
        self.assertEqual(runner._claim_job(conn), job)  # claimed despite failure
        self.assertTrue(conn.claim_ran())
        self.assertFalse(conn.decrement_ran())  # degrade path is unlimited
        self.assertEqual(conn.rollbacks, 1)  # the failed control read rolled back

    def test_bounded_run_claims_and_decrements(self) -> None:
        job = {"id": "j1", "file_path": "/books/x.m4b", "checksum": "abc"}
        conn = FakeConn(control_row={"paused": False, "run_limit": 1}, job_row=job)
        got = runner._claim_job(conn)
        self.assertEqual(got, job)
        self.assertTrue(conn.claim_ran())
        self.assertTrue(conn.decrement_ran())  # exactly-N: claim consumed budget

    def test_bounded_run_empty_queue_does_not_decrement(self) -> None:
        # Gate passes (run_limit=1) but the queue is empty (claim returns None):
        # budget must NOT be consumed.
        conn = FakeConn(control_row={"paused": False, "run_limit": 1}, job_row=None)
        self.assertIsNone(runner._claim_job(conn))
        self.assertTrue(conn.claim_ran())
        self.assertFalse(conn.decrement_ran())

    def test_unlimited_claims_without_decrement(self) -> None:
        job = {"id": "j2", "file_path": "/books/y.m4b", "checksum": "def"}
        conn = FakeConn(control_row={"paused": False, "run_limit": None}, job_row=job)
        got = runner._claim_job(conn)
        self.assertEqual(got, job)
        self.assertTrue(conn.claim_ran())
        self.assertFalse(conn.decrement_ran())  # NULL = unlimited, no counter

    def test_busy_flag_skips_claim(self) -> None:
        runner.BUSY_FLAG_PATH = Path(__file__)  # an existing file
        conn = FakeConn(control_row={"paused": False, "run_limit": None}, job_row={"id": "j"})
        self.assertIsNone(runner._claim_job(conn))
        self.assertFalse(conn.claim_ran())


class ShouldParkTests(unittest.TestCase):
    """_should_park is a pure decision: paused/phase → park the GPU model?"""

    def test_active_unlimited_stays_on_gpu(self) -> None:
        # Not paused + the GPU-allowed phases → model stays loaded (do not park).
        for phase in (None, "idle", "transcribe"):
            self.assertFalse(runner._should_park(False, phase), f"phase={phase!r}")

    def test_paused_parks_regardless_of_phase(self) -> None:
        # Paused dominates: park even on an otherwise-on-GPU phase.
        for phase in (None, "idle", "transcribe", "analyze"):
            self.assertTrue(runner._should_park(True, phase), f"phase={phase!r}")

    def test_analyze_phase_parks_when_not_paused(self) -> None:
        self.assertTrue(runner._should_park(False, "analyze"))

    def test_unknown_phase_parks_failsafe(self) -> None:
        # An unrecognised phase value is treated as "not safe to use the GPU".
        self.assertTrue(runner._should_park(False, "something-new"))


class _PhaseCursor:
    """Cursor whose fetchone routes SELECT phase / SELECT paused queries."""

    def __init__(self, conn: "_PhaseConn") -> None:
        self.conn = conn

    def __enter__(self) -> "_PhaseCursor":
        return self

    def __exit__(self, *exc: object) -> None:
        return None

    def execute(self, sql: str, params: object = None) -> None:
        if self.conn.raise_on is not None and self.conn.raise_on in sql:
            raise RuntimeError('column "phase" does not exist')
        self.conn.last_sql = sql

    def fetchone(self) -> object:
        if "phase" in self.conn.last_sql:
            return self.conn.phase_row
        if "paused" in self.conn.last_sql:
            return self.conn.control_row
        return None


class _PhaseConn:
    def __init__(
        self,
        phase_row: object = None,
        control_row: object = None,
        raise_on: str | None = None,
    ) -> None:
        self.phase_row = phase_row
        self.control_row = control_row
        self.raise_on = raise_on
        self.last_sql = ""
        self.commits = 0
        self.rollbacks = 0

    def cursor(self) -> _PhaseCursor:
        return _PhaseCursor(self)

    def commit(self) -> None:
        self.commits += 1

    def rollback(self) -> None:
        self.rollbacks += 1


class ReadPhaseTests(unittest.TestCase):
    """_read_phase reads runner_control.phase defensively (column may be absent)."""

    def test_present_value(self) -> None:
        conn = _PhaseConn(phase_row={"phase": "analyze"})
        self.assertEqual(runner._read_phase(conn), "analyze")
        self.assertEqual(conn.commits, 1)

    def test_tuple_row(self) -> None:
        conn = _PhaseConn(phase_row=("transcribe",))
        self.assertEqual(runner._read_phase(conn), "transcribe")

    def test_null_value_defaults_to_idle(self) -> None:
        # phase column exists but the row's value is NULL → default 'idle'.
        conn = _PhaseConn(phase_row={"phase": None})
        self.assertEqual(runner._read_phase(conn), "idle")

    def test_missing_row_defaults_to_idle(self) -> None:
        conn = _PhaseConn(phase_row=None)
        self.assertEqual(runner._read_phase(conn), "idle")

    def test_missing_column_degrades_to_idle(self) -> None:
        # The separate schema PR hasn't landed: SELECT phase raises. Must not crash.
        conn = _PhaseConn(raise_on="phase")
        self.assertEqual(runner._read_phase(conn), "idle")
        self.assertEqual(conn.rollbacks, 1)  # failed read rolled back, state clean


class ReadControlRowTests(unittest.TestCase):
    """_read_control_row is the advisory (unlocked) paused/run_limit read."""

    def test_present_row(self) -> None:
        conn = _PhaseConn(control_row={"paused": True, "run_limit": None})
        row = runner._read_control_row(conn)
        self.assertEqual(runner._control_values(row), (True, None))
        self.assertEqual(conn.commits, 1)

    def test_missing_table_degrades_to_none(self) -> None:
        conn = _PhaseConn(raise_on="paused")
        self.assertIsNone(runner._read_control_row(conn))
        self.assertEqual(runner._control_values(None), (False, None))
        self.assertEqual(conn.rollbacks, 1)


class ParkUnparkTests(unittest.TestCase):
    """NeMoParakeetProvider.park/unpark move the model and are idempotent."""

    def _provider_with_fake_torch(self):
        """A provider holding a fake model, with torch.cuda.empty_cache stubbed."""
        import unittest.mock as mock

        provider = runner.NeMoParakeetProvider()
        provider._asr_model = mock.MagicMock()
        # Stub the runtime `import torch` inside park() via sys.modules.
        fake_torch = types.ModuleType("torch")
        fake_torch.cuda = types.SimpleNamespace(empty_cache=mock.MagicMock())
        return provider, fake_torch

    def test_park_moves_to_cpu_and_empties_cache(self) -> None:
        import unittest.mock as mock

        provider, fake_torch = self._provider_with_fake_torch()
        with mock.patch.dict(sys.modules, {"torch": fake_torch}):
            provider.park()
        provider._asr_model.cpu.assert_called_once()
        fake_torch.cuda.empty_cache.assert_called_once()
        self.assertTrue(provider._parked)

    def test_park_is_idempotent(self) -> None:
        import unittest.mock as mock

        provider, fake_torch = self._provider_with_fake_torch()
        with mock.patch.dict(sys.modules, {"torch": fake_torch}):
            provider.park()
            provider.park()  # second call must be a no-op
        provider._asr_model.cpu.assert_called_once()
        self.assertEqual(fake_torch.cuda.empty_cache.call_count, 1)

    def test_unpark_restores_to_gpu(self) -> None:
        import unittest.mock as mock

        provider, fake_torch = self._provider_with_fake_torch()
        with mock.patch.dict(sys.modules, {"torch": fake_torch}):
            provider.park()
        provider.unpark()
        provider._asr_model.cuda.assert_called_once()
        self.assertFalse(provider._parked)

    def test_unpark_without_park_is_noop(self) -> None:
        provider, _ = self._provider_with_fake_torch()
        provider.unpark()  # never parked → no move
        provider._asr_model.cuda.assert_not_called()

    def test_park_with_no_model_is_noop(self) -> None:
        provider = runner.NeMoParakeetProvider()  # _asr_model is None
        provider.park()  # must not raise / not import torch
        self.assertFalse(provider._parked)

    def test_diarize_model_parks_too(self) -> None:
        import unittest.mock as mock

        provider, fake_torch = self._provider_with_fake_torch()
        provider._diarize_model = mock.MagicMock()
        with mock.patch.dict(sys.modules, {"torch": fake_torch}):
            provider.park()
        provider._diarize_model.cpu.assert_called_once()
        provider.unpark()
        provider._diarize_model.cuda.assert_called_once()

    def test_repeated_transitions_track_state(self) -> None:
        # Drive ONE provider through park → unpark → park → unpark and assert the
        # _parked flag and the underlying .cpu()/.cuda()/empty_cache() call counts
        # land in the right state at each step. Catches a stuck/inverted flag where
        # a second transition silently no-ops because state wasn't toggled.
        import unittest.mock as mock

        provider, fake_torch = self._provider_with_fake_torch()
        cpu, cuda = provider._asr_model.cpu, provider._asr_model.cuda
        empty_cache = fake_torch.cuda.empty_cache

        with mock.patch.dict(sys.modules, {"torch": fake_torch}):
            # 1) park
            provider.park()
            self.assertTrue(provider._parked)
            self.assertEqual(cpu.call_count, 1)
            self.assertEqual(cuda.call_count, 0)
            self.assertEqual(empty_cache.call_count, 1)

            # 2) unpark
            provider.unpark()
            self.assertFalse(provider._parked)
            self.assertEqual(cpu.call_count, 1)
            self.assertEqual(cuda.call_count, 1)
            self.assertEqual(empty_cache.call_count, 1)  # unpark does not empty cache

            # 3) park again — must actually move (not a stuck-flag no-op)
            provider.park()
            self.assertTrue(provider._parked)
            self.assertEqual(cpu.call_count, 2)
            self.assertEqual(cuda.call_count, 1)
            self.assertEqual(empty_cache.call_count, 2)

            # 4) unpark again
            provider.unpark()
            self.assertFalse(provider._parked)
            self.assertEqual(cpu.call_count, 2)
            self.assertEqual(cuda.call_count, 2)
            self.assertEqual(empty_cache.call_count, 2)


class _RecordingProvider:
    """A stub ASRProvider that records park/unpark calls (no real GPU)."""

    def __init__(self, park_raises: bool = False, unpark_raises: bool = False) -> None:
        self.parked = 0
        self.unparked = 0
        self.park_raises = park_raises
        self.unpark_raises = unpark_raises

    def park(self) -> None:
        self.parked += 1
        if self.park_raises:
            raise RuntimeError("simulated torch/CUDA failure")

    def unpark(self) -> None:
        self.unparked += 1
        if self.unpark_raises:
            raise RuntimeError("simulated torch/CUDA failure")


class GateGpuTests(unittest.TestCase):
    """_gate_gpu orchestrates the per-cycle sequence: read paused+phase →
    _should_park → park-and-skip-claim vs unpark-then-proceed. No real torch/GPU/DB."""

    def test_active_unparks_and_proceeds_to_claim(self) -> None:
        # not paused + on-GPU phase → unpark, do not skip the claim.
        conn = _PhaseConn(
            control_row={"paused": False, "run_limit": None},
            phase_row={"phase": "transcribe"},
        )
        provider = _RecordingProvider()
        skip = runner._gate_gpu(conn, provider)
        self.assertFalse(skip)  # claiming proceeds this cycle
        self.assertEqual(provider.unparked, 1)
        self.assertEqual(provider.parked, 0)

    def test_paused_parks_and_skips_claim(self) -> None:
        conn = _PhaseConn(
            control_row={"paused": True, "run_limit": None},
            phase_row={"phase": "idle"},
        )
        provider = _RecordingProvider()
        skip = runner._gate_gpu(conn, provider)
        self.assertTrue(skip)  # claim skipped this cycle
        self.assertEqual(provider.parked, 1)
        self.assertEqual(provider.unparked, 0)

    def test_analyze_phase_parks_and_skips_claim(self) -> None:
        conn = _PhaseConn(
            control_row={"paused": False, "run_limit": None},
            phase_row={"phase": "analyze"},
        )
        provider = _RecordingProvider()
        skip = runner._gate_gpu(conn, provider)
        self.assertTrue(skip)
        self.assertEqual(provider.parked, 1)

    def test_missing_phase_column_keeps_model_active(self) -> None:
        # phase read raises (column not deployed yet) → defaults to 'idle' → active.
        conn = _PhaseConn(
            control_row={"paused": False, "run_limit": None},
            raise_on="phase",
        )
        provider = _RecordingProvider()
        skip = runner._gate_gpu(conn, provider)
        self.assertFalse(skip)
        self.assertEqual(provider.unparked, 1)

    def test_park_failure_is_isolated_and_skips_claim(self) -> None:
        # A park() failure (e.g. torch unavailable) must NOT propagate to the
        # caller's DB/claim handler. _gate_gpu catches it and still skips the claim.
        conn = _PhaseConn(
            control_row={"paused": True, "run_limit": None},
            phase_row={"phase": "idle"},
        )
        provider = _RecordingProvider(park_raises=True)
        skip = runner._gate_gpu(conn, provider)  # must not raise
        self.assertTrue(skip)
        self.assertEqual(provider.parked, 1)

    def test_unpark_failure_is_isolated_and_skips_claim(self) -> None:
        # An unpark() failure means we couldn't reclaim the card → skip the claim
        # (don't transcribe on a card we don't own) rather than crash the loop.
        conn = _PhaseConn(
            control_row={"paused": False, "run_limit": None},
            phase_row={"phase": "transcribe"},
        )
        provider = _RecordingProvider(unpark_raises=True)
        skip = runner._gate_gpu(conn, provider)  # must not raise
        self.assertTrue(skip)
        self.assertEqual(provider.unparked, 1)


class ResolveAudioPathTests(unittest.TestCase):
    """_resolve_audio_path re-roots the producer's file_path onto BOOKS_MOUNT."""

    def setUp(self) -> None:
        self._mount = runner.BOOKS_MOUNT
        self._dbroot = runner.BOOKS_DB_ROOT
        runner.BOOKS_MOUNT = Path("/mnt/media/books")
        runner.BOOKS_DB_ROOT = Path("/books")

    def tearDown(self) -> None:
        runner.BOOKS_MOUNT = self._mount
        runner.BOOKS_DB_ROOT = self._dbroot

    def test_absolute_db_path_is_rerooted(self) -> None:
        # The real prod case: "/books/..." must land under BOOKS_MOUNT, NOT "/books/...".
        got = runner._resolve_audio_path(
            "/books/audio-custom/Benjamin Schumacher/The Science of Information [1629976067].m4b"
        )
        self.assertEqual(
            got,
            Path("/mnt/media/books/audio-custom/Benjamin Schumacher/"
                 "The Science of Information [1629976067].m4b"),
        )

    def test_relative_path_is_joined(self) -> None:
        # The CONTRACT's nominal relative form also works.
        got = runner._resolve_audio_path("audio-libation/W. Gibson/Neuromancer/01.mp3")
        self.assertEqual(
            got, Path("/mnt/media/books/audio-libation/W. Gibson/Neuromancer/01.mp3")
        )

    def test_traversal_is_rejected(self) -> None:
        for bad in ("/books/../../etc/passwd", "../../etc/passwd", "audio/../../../etc/shadow"):
            with self.subTest(path=bad):
                with self.assertRaisesRegex(ValueError, "escapes BOOKS_MOUNT"):
                    runner._resolve_audio_path(bad)

    def test_empty_string_is_rejected(self) -> None:
        # "" → Path(".") → would resolve to the mount dir itself; reject explicitly.
        with self.assertRaisesRegex(ValueError, "must not be empty"):
            runner._resolve_audio_path("")

    def test_relative_parent_is_rejected(self) -> None:
        # A bare relative ".." escapes BOOKS_MOUNT and must be rejected.
        with self.assertRaisesRegex(ValueError, "escapes BOOKS_MOUNT"):
            runner._resolve_audio_path("..")

    def test_absolute_outside_dbroot_is_rejected(self) -> None:
        with self.assertRaisesRegex(ValueError, "not under"):
            runner._resolve_audio_path("/etc/passwd")

    def test_books_prefix_collision_is_rejected(self) -> None:
        # relative_to is part-based, so BOOKS_DB_ROOT=/books must NOT match
        # /bookshelf/... (a string-prefix collision).
        with self.assertRaisesRegex(ValueError, "not under"):
            runner._resolve_audio_path("/bookshelf/file.m4b")

    def test_dots_in_filename_are_safe(self) -> None:
        # Literal ".." inside a filename (not a path component) is not traversal.
        got = runner._resolve_audio_path("/books/audio/file..name.m4b")
        self.assertEqual(got, Path("/mnt/media/books/audio/file..name.m4b"))


class OffsetTimestampsTests(unittest.TestCase):
    """_offset_timestamps rebases per-window timestamps to absolute file time
    and drops entries outside the window's core (de-duplicating seams)."""

    def test_rebases_to_absolute_time(self) -> None:
        entries = [
            {"word": "hello", "start": 1.0, "end": 1.5},
            {"word": "world", "start": 2.0, "end": 2.4},
        ]
        # Window starts at 600 s; core is the whole clip (keep_end=None).
        got = runner._offset_timestamps(entries, offset=600.0, keep_start=0.0, keep_end=None)
        self.assertEqual(
            got,
            [
                {"word": "hello", "start": 601.0, "end": 601.5},
                {"word": "world", "start": 602.0, "end": 602.4},
            ],
        )

    def test_drops_leading_overlap(self) -> None:
        # Entries starting before keep_start belong to the previous window.
        entries = [
            {"word": "prev", "start": 2.0, "end": 2.5},   # in leading overlap
            {"word": "core", "start": 6.0, "end": 6.3},   # kept
        ]
        got = runner._offset_timestamps(entries, offset=100.0, keep_start=5.0, keep_end=None)
        self.assertEqual(got, [{"word": "core", "start": 106.0, "end": 106.3}])

    def test_drops_trailing_overlap(self) -> None:
        # Entries starting at/after keep_end belong to the NEXT window's core,
        # so the trailing-overlap context audio doesn't duplicate words.
        entries = [
            {"word": "core", "start": 10.0, "end": 10.4},   # kept (< window)
            {"word": "ctx", "start": 605.0, "end": 605.3},  # in trailing overlap
        ]
        got = runner._offset_timestamps(entries, offset=0.0, keep_start=0.0, keep_end=600.0)
        self.assertEqual(got, [{"word": "core", "start": 10.0, "end": 10.4}])

    def test_segment_entries_preserve_text_key(self) -> None:
        # The 'segment' text key must survive the rebase untouched.
        entries = [{"segment": "A sentence.", "start": 3.0, "end": 8.0}]
        got = runner._offset_timestamps(entries, offset=1200.0, keep_start=0.0, keep_end=600.0)
        self.assertEqual(
            got, [{"segment": "A sentence.", "start": 1203.0, "end": 1208.0}]
        )

    def test_empty_input(self) -> None:
        self.assertEqual(
            runner._offset_timestamps([], offset=0.0, keep_start=0.0, keep_end=None), []
        )

    def test_boundary_word_at_keep_end_is_dropped(self) -> None:
        # A word starting exactly at keep_end is owned by the next window (>=).
        entries = [{"word": "edge", "start": 600.0, "end": 600.3}]
        got = runner._offset_timestamps(entries, offset=0.0, keep_start=0.0, keep_end=600.0)
        self.assertEqual(got, [])

    def test_does_not_mutate_input(self) -> None:
        entries = [{"word": "x", "start": 1.0, "end": 1.2}]
        runner._offset_timestamps(entries, offset=50.0, keep_start=0.0, keep_end=None)
        self.assertEqual(entries, [{"word": "x", "start": 1.0, "end": 1.2}])


class AppendMonotonicTests(unittest.TestCase):
    """_append_monotonic enforces strictly-increasing starts across seams."""

    def test_appends_increasing(self) -> None:
        acc: list = []
        runner._append_monotonic(acc, [{"word": "a", "start": 1.0, "end": 1.2}])
        runner._append_monotonic(acc, [{"word": "b", "start": 2.0, "end": 2.2}])
        self.assertEqual([w["word"] for w in acc], ["a", "b"])

    def test_drops_seam_duplicate(self) -> None:
        # Window N ends with a word at 599.9; window N+1 re-emits it at 600.0
        # (jitter) plus the genuine next word. Only forward-progress survives.
        acc = [{"word": "Police.", "start": 599.9, "end": 600.4}]
        incoming = [
            {"word": "Police.", "start": 600.0, "end": 600.5},  # duplicate-ish
            {"word": "That", "start": 601.3, "end": 601.6},     # genuine next
        ]
        runner._append_monotonic(acc, incoming)
        self.assertEqual([w["word"] for w in acc], ["Police.", "That"])

    def test_drops_equal_start(self) -> None:
        acc = [{"word": "x", "start": 5.0, "end": 5.2}]
        runner._append_monotonic(acc, [{"word": "x2", "start": 5.0, "end": 5.3}])
        self.assertEqual(len(acc), 1)

    def test_empty_acc_takes_first(self) -> None:
        acc: list = []
        runner._append_monotonic(acc, [{"word": "first", "start": 0.96, "end": 2.0}])
        self.assertEqual(acc[0]["word"], "first")

    def test_keeps_distinct_close_words(self) -> None:
        # Two genuinely different words 0.1 s apart (under tolerance) must both
        # survive — only same-text near-coincident repeats are dropped.
        acc = [{"word": "the", "start": 10.0, "end": 10.1}]
        runner._append_monotonic(acc, [{"word": "cat", "start": 10.1, "end": 10.3}])
        self.assertEqual([w["word"] for w in acc], ["the", "cat"])

    def test_segment_seam_dedup_by_text(self) -> None:
        acc = [{"segment": "Police.", "start": 599.9, "end": 600.4}]
        runner._append_monotonic(
            acc, [{"segment": "Police.", "start": 600.0, "end": 600.5}]
        )
        self.assertEqual(len(acc), 1)

    def test_result_is_strictly_increasing(self) -> None:
        acc: list = []
        for chunk in (
            [{"word": "a", "start": 0.0, "end": 0.5}, {"word": "b", "start": 1.0, "end": 1.5}],
            [{"word": "b2", "start": 1.0, "end": 1.4}, {"word": "c", "start": 2.0, "end": 2.5}],
        ):
            runner._append_monotonic(acc, chunk)
        starts = [w["start"] for w in acc]
        self.assertEqual(starts, sorted(set(starts)))
        self.assertEqual([w["word"] for w in acc], ["a", "b", "c"])


class StitchedHypothesisTests(unittest.TestCase):
    """_StitchedHypothesis mimics the NeMo Hypothesis shape _build_segments reads."""

    def test_exposes_text_and_timestamp(self) -> None:
        words = [{"word": "hi", "start": 0.0, "end": 0.3}]
        segs = [{"segment": "Hi.", "start": 0.0, "end": 0.5}]
        hyp = runner._StitchedHypothesis("Hi.", words, segs)
        self.assertEqual(hyp.text, "Hi.")
        self.assertEqual(hyp.timestamp["word"], words)
        self.assertEqual(hyp.timestamp["segment"], segs)

    def test_build_segments_consumes_stitched_hypothesis(self) -> None:
        # End-to-end: a stitched hypothesis must flow through _build_segments
        # exactly like a real NeMo hypothesis (no diarization).
        words = [
            {"word": "It", "start": 100.0, "end": 100.2},
            {"word": "was", "start": 100.2, "end": 100.4},
        ]
        segs = [{"segment": "It was", "start": 100.0, "end": 100.4}]
        hyp = runner._StitchedHypothesis("It was", words, segs)
        segments, speaker_count = runner._build_segments(hyp, None, Path("/x.wav"))
        self.assertIsNone(speaker_count)
        self.assertEqual(len(segments), 1)
        self.assertEqual(segments[0]["text"], "It was")
        self.assertEqual(segments[0]["start"], 100.0)
        self.assertEqual(segments[0]["end"], 100.4)
        self.assertEqual(len(segments[0]["words"]), 2)
        self.assertEqual(segments[0]["words"][0]["word"], "It")

    def test_offset_clamps_inverted_end(self) -> None:
        # A drifted entry with end < start must be clamped to zero-duration
        # (end == start), not propagate a backwards word.
        out = runner._offset_timestamps(
            [{"word": "x", "start": 5.0, "end": 4.0}], offset=10.0, keep_start=0.0, keep_end=None
        )
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["start"], 15.0)
        self.assertEqual(out[0]["end"], 15.0)  # clamped: end == start


class WordsToSegmentsTests(unittest.TestCase):
    """_words_to_segments groups words into pseudo-segments on a 1.5 s gap."""

    def test_empty_words(self) -> None:
        self.assertEqual(runner._words_to_segments([], []), [])

    def test_single_segment_small_gaps(self) -> None:
        words = [
            {"word": "The", "start": 0.0, "end": 0.2, "speaker": None},
            {"word": "quick", "start": 0.3, "end": 0.7, "speaker": None},
            {"word": "fox", "start": 0.8, "end": 1.1, "speaker": None},
        ]
        self.assertEqual(len(runner._words_to_segments(words, [None, None, None])), 1)

    def test_splits_on_silence_gap(self) -> None:
        # >1.5 s gap between "silence." (ends 0.8) and "Then" (starts 2.5) → split.
        words = [
            {"word": "The", "start": 0.0, "end": 0.2, "speaker": None},
            {"word": "silence.", "start": 0.2, "end": 0.8, "speaker": None},
            {"word": "Then", "start": 2.5, "end": 2.7, "speaker": None},
        ]
        self.assertEqual(len(runner._words_to_segments(words, [None, None, None])), 2)

    def test_splits_on_speaker_change(self) -> None:
        # Same timing (no silence gap) but the speaker changes → split.
        words = [
            {"word": "Hi", "start": 0.0, "end": 0.2, "speaker": "SPEAKER_00"},
            {"word": "there", "start": 0.3, "end": 0.6, "speaker": "SPEAKER_01"},
        ]
        self.assertEqual(
            len(runner._words_to_segments(words, ["SPEAKER_00", "SPEAKER_01"])), 2
        )

    def test_build_segments_falls_back_to_words(self) -> None:
        # Word timestamps present but NO segment boundaries → _build_segments
        # falls back to _words_to_segments and splits at the silence gap.
        words = [
            {"word": "One", "start": 0.0, "end": 0.2},
            {"word": "two.", "start": 0.3, "end": 0.7},
            {"word": "Three", "start": 3.0, "end": 3.3},  # >1.5 s gap
        ]
        hyp = runner._StitchedHypothesis("One two. Three", words, [])
        segments, _ = runner._build_segments(hyp, None, Path("/x.wav"))
        self.assertEqual(len(segments), 2)


class AudioProbeTests(unittest.TestCase):
    """_audio_probe returns a dict; _audio_duration remains callable via wrapper."""

    def _fake_probe_output(self, **overrides: object) -> str:
        """Build a minimal ffprobe JSON blob for a given set of values."""
        defaults = {
            "channels": 2,
            "sample_rate": "44100",
            "codec_name": "aac",
            "format_name": "mov,mp4,m4a,3gp,3g2,mj2",
            "duration": "123.456",
            "size": "1048576",
        }
        defaults.update(overrides)  # type: ignore[arg-type]
        import json as _json
        return _json.dumps({
            "streams": [{
                "channels": defaults["channels"],
                "sample_rate": defaults["sample_rate"],
                "codec_name": defaults["codec_name"],
                "duration": defaults["duration"],
            }],
            "format": {
                "format_name": defaults["format_name"],
                "duration": defaults["duration"],
                "size": defaults["size"],
            },
        })

    def _patch_run(self, output: str):
        """Context manager: patch subprocess.run to return a fake ffprobe result."""
        import unittest.mock as mock
        fake = mock.MagicMock()
        fake.stdout = output
        return mock.patch.object(runner.subprocess, "run", return_value=fake)

    def test_probe_returns_expected_keys(self) -> None:
        with self._patch_run(self._fake_probe_output()):
            result = runner._audio_probe(Path("/fake/file.m4b"))
        self.assertAlmostEqual(result["duration"], 123.456, places=2)
        self.assertEqual(result["channels"], 2)
        self.assertEqual(result["sample_rate"], 44100)
        self.assertEqual(result["codec_name"], "aac")
        self.assertEqual(result["format_name"], "mov,mp4,m4a,3gp,3g2,mj2")
        self.assertEqual(result["size_bytes"], 1048576)

    def test_audio_duration_wrapper(self) -> None:
        # _audio_duration must still return just the float duration.
        with self._patch_run(self._fake_probe_output(duration="300.0")):
            dur = runner._audio_duration(Path("/fake/file.mp3"))
        self.assertAlmostEqual(dur, 300.0, places=1)

    def test_probe_mono_file(self) -> None:
        with self._patch_run(self._fake_probe_output(channels=1)):
            result = runner._audio_probe(Path("/fake/mono.wav"))
        self.assertEqual(result["channels"], 1)

    def test_probe_stereo_file(self) -> None:
        with self._patch_run(self._fake_probe_output(channels=2)):
            result = runner._audio_probe(Path("/fake/stereo.m4b"))
        self.assertEqual(result["channels"], 2)

    def test_probe_sanitizes_control_chars(self) -> None:
        """
        codec_name/format_name come from untrusted ffprobe JSON; control chars
        (newlines/CR/tabs) must be stripped (log/DB injection guard) and the
        probe must NOT fail — it sanitizes and continues.
        """
        evil_codec = "aac\nFAKE LOG LINE\r"
        evil_format = "mp4\t\x00inj"
        with self._patch_run(self._fake_probe_output(
                codec_name=evil_codec, format_name=evil_format)):
            result = runner._audio_probe(Path("/fake/evil.m4b"))
        # No newline/CR/tab/NUL/control chars survive.
        for bad in ("\n", "\r", "\t", "\x00"):
            self.assertNotIn(bad, result["codec_name"])
            self.assertNotIn(bad, result["format_name"])
        # Real content preserved (control chars → spaces, then stripped at edges).
        self.assertTrue(result["codec_name"].startswith("aac"))
        self.assertTrue(result["format_name"].startswith("mp4"))

    def test_probe_truncates_overlong_fields(self) -> None:
        """Overlong codec_name/format_name are truncated to the documented caps."""
        with self._patch_run(self._fake_probe_output(
                codec_name="a" * 200, format_name="b" * 300)):
            result = runner._audio_probe(Path("/fake/long.m4b"))
        self.assertLessEqual(len(result["codec_name"]), 50)
        self.assertLessEqual(len(result["format_name"]), 100)


class MonoDownmixTests(unittest.TestCase):
    """_to_mono_wav calls ffmpeg -ac 1 -ar 16000 and returns a temp WAV path."""

    def test_to_mono_wav_calls_ffmpeg_with_mono_args(self) -> None:
        """_to_mono_wav must invoke ffmpeg with -ac 1 -ar 16000 for any input."""
        import unittest.mock as mock

        with mock.patch.object(runner.subprocess, "run") as mock_run, \
             mock.patch.object(runner.tempfile, "mkstemp",
                               return_value=(99, "/tmp/asr-mono-test.wav")), \
             mock.patch.object(runner.os, "close"):
            runner._to_mono_wav(Path("/fake/stereo.m4b"))

        mock_run.assert_called_once()
        cmd = mock_run.call_args[0][0]
        # Must include -ac 1 and -ar 16000 (force mono + resample).
        self.assertIn("-ac", cmd)
        self.assertEqual(cmd[cmd.index("-ac") + 1], "1")
        self.assertIn("-ar", cmd)
        self.assertEqual(cmd[cmd.index("-ar") + 1], "16000")

    def test_to_mono_wav_returns_path(self) -> None:
        import unittest.mock as mock

        with mock.patch.object(runner.subprocess, "run"), \
             mock.patch.object(runner.tempfile, "mkstemp",
                               return_value=(99, "/tmp/asr-mono-test.wav")), \
             mock.patch.object(runner.os, "close"):
            result = runner._to_mono_wav(Path("/fake/any.m4b"))

        self.assertEqual(result, Path("/tmp/asr-mono-test.wav"))

    def test_to_mono_wav_cleans_up_on_ffmpeg_failure(self) -> None:
        """
        When ffmpeg fails (check=True → CalledProcessError), _to_mono_wav must
        re-raise AND unlink the temp file it created (no leak — the caller's
        finally never runs because no path is returned).
        """
        import subprocess as _sp
        import unittest.mock as mock

        tmp_path = "/tmp/asr-mono-test.wav"
        with mock.patch.object(runner.subprocess, "run",
                               side_effect=_sp.CalledProcessError(1, "ffmpeg")), \
             mock.patch.object(runner.tempfile, "mkstemp",
                               return_value=(99, tmp_path)), \
             mock.patch.object(runner.os, "close"), \
             mock.patch.object(runner.os, "unlink") as mock_unlink:
            with self.assertRaises(_sp.CalledProcessError):
                runner._to_mono_wav(Path("/fake/any.m4b"))

        # The created temp file must be unlinked on failure.
        mock_unlink.assert_called_once_with(tmp_path)

    def test_cut_window_cleans_up_on_ffmpeg_failure(self) -> None:
        """_cut_window has the identical leak guard as _to_mono_wav."""
        import subprocess as _sp
        import unittest.mock as mock

        tmp_path = "/tmp/asr-chunk-0-test.wav"
        with mock.patch.object(runner.subprocess, "run",
                               side_effect=_sp.CalledProcessError(1, "ffmpeg")), \
             mock.patch.object(runner.tempfile, "mkstemp",
                               return_value=(99, tmp_path)), \
             mock.patch.object(runner.os, "close"), \
             mock.patch.object(runner.os, "unlink") as mock_unlink:
            with self.assertRaises(_sp.CalledProcessError):
                runner._cut_window(Path("/fake/any.m4b"), 0.0, 600.0, 0)

        mock_unlink.assert_called_once_with(tmp_path)

    def test_single_pass_invokes_to_mono_wav(self) -> None:
        """
        The single-pass branch of _transcribe_file must call _to_mono_wav so
        NeMo receives a mono WAV regardless of the source file's channel count.

        We patch _audio_probe (returns stereo 2-ch, short 60 s), _to_mono_wav
        (returns a fake temp path), and asr_model.transcribe so we can assert
        the path handed to transcribe is the mono path, not the original.
        """
        import unittest.mock as mock

        fake_probe = {
            "duration": 60.0,
            "channels": 2,
            "sample_rate": 44100,
            "codec_name": "aac",
            "format_name": "m4b",
            "size_bytes": 1000,
        }
        mono_path = Path("/tmp/asr-mono-test.wav")
        # Build a minimal fake hypothesis so _build_segments doesn't crash.
        fake_hyp = runner._StitchedHypothesis(
            "hello world",
            [{"word": "hello", "start": 0.0, "end": 0.4},
             {"word": "world", "start": 0.5, "end": 0.9}],
            [{"segment": "hello world", "start": 0.0, "end": 0.9}],
        )
        mock_model = mock.MagicMock()
        mock_model.transcribe.return_value = [fake_hyp]

        # Patch Path.unlink at the class level so the finally block in
        # _transcribe_file doesn't fail trying to delete a non-existent file.
        # Use a MagicMock (not a no-op lambda) so we can ASSERT the temp file is
        # cleaned up — a removed/broken finally must fail this test.
        with mock.patch.object(runner, "_audio_probe", return_value=fake_probe), \
             mock.patch.object(runner, "_to_mono_wav", return_value=mono_path) as mock_tmw, \
             mock.patch.object(runner.Path, "unlink") as mock_unlink:

            source_path = Path("/fake/stereo.m4b")
            result = runner._transcribe_file(source_path, mock_model, None)

        # _to_mono_wav must have been called with the source audio path.
        mock_tmw.assert_called_once_with(source_path)
        # The mono temp file MUST be cleaned up (finally block in single-pass path).
        mock_unlink.assert_called()
        # The model's transcribe must receive the mono path, not the original.
        transcribed_path = mock_model.transcribe.call_args[0][0][0]
        self.assertEqual(transcribed_path, str(mono_path))
        # Result must include the run_metrics fields.
        self.assertFalse(result["chunked"])
        self.assertEqual(result["n_windows"], 1)
        self.assertIn("transcribe_started_at", result)
        self.assertIn("transcribe_finished_at", result)
        self.assertEqual(result["audio_channels"], 2)

    def test_single_pass_mono_source_still_downmixes(self) -> None:
        """
        _to_mono_wav is called UNCONDITIONALLY on the single-pass path, even for
        an already-mono (channels=1) source — it also resamples to 16 kHz, which
        a 1-channel-but-wrong-rate file still needs. Cleanup must still happen.
        """
        import unittest.mock as mock

        fake_probe = {
            "duration": 30.0,
            "channels": 1,
            "sample_rate": 22050,
            "codec_name": "mp3",
            "format_name": "mp3",
            "size_bytes": 500,
        }
        mono_path = Path("/tmp/asr-mono-test.wav")
        fake_hyp = runner._StitchedHypothesis(
            "hi there",
            [{"word": "hi", "start": 0.0, "end": 0.2},
             {"word": "there", "start": 0.3, "end": 0.6}],
            [{"segment": "hi there", "start": 0.0, "end": 0.6}],
        )
        mock_model = mock.MagicMock()
        mock_model.transcribe.return_value = [fake_hyp]

        with mock.patch.object(runner, "_audio_probe", return_value=fake_probe), \
             mock.patch.object(runner, "_to_mono_wav", return_value=mono_path) as mock_tmw, \
             mock.patch.object(runner.Path, "unlink") as mock_unlink:

            source_path = Path("/fake/mono.mp3")
            result = runner._transcribe_file(source_path, mock_model, None)

        # Even a mono source goes through _to_mono_wav (resample to 16 kHz).
        mock_tmw.assert_called_once_with(source_path)
        mock_unlink.assert_called()
        transcribed_path = mock_model.transcribe.call_args[0][0][0]
        self.assertEqual(transcribed_path, str(mono_path))
        self.assertEqual(result["audio_channels"], 1)
        self.assertFalse(result["chunked"])


class RunMetricsTests(unittest.TestCase):
    """_write_run_metrics: best-effort UPSERT — swallows all errors gracefully."""

    def _make_result(self, **overrides: object) -> dict:
        base: dict = {
            "transcribe_started_at": None,
            "transcribe_finished_at": None,
            "chunked": False,
            "n_windows": 1,
            "char_count": 42,
            "word_count": 8,
            "segment_count": 2,
            "audio_channels": 2,
            "audio_sample_rate": 44100,
            "audio_codec": "aac",
            "audio_format": "m4b",
            "audio_bytes": 1048576,
            # §2.13 backend descriptor.
            "asr_family": "nemo-parakeet",
            "asr_runtime": "nemo-cuda",
            "caps_applied": {"word_timestamps": True, "context_biasing": False},
            "caps_requested": {"word_timestamps": True},
            "caps_skipped_reason": None,
            "mean_word_confidence": None,
        }
        base.update(overrides)
        return base

    def test_metrics_writes_on_success(self) -> None:
        conn = FakeConn(control_row=None, job_row=None)
        runner._write_run_metrics(conn, "job-abc", self._make_result())
        # Must have executed the INSERT/UPSERT and committed.
        self.assertTrue(any("run_metrics" in s for s, _ in conn.executed))
        self.assertEqual(conn.commits, 1)
        self.assertEqual(conn.rollbacks, 0)

    def test_metrics_failure_is_swallowed(self) -> None:
        """A DB error during metrics write must NOT propagate — only warn."""
        class ExplodingCursor(FakeCursor):
            def execute(self, sql: str, params: object = None) -> None:
                if "run_metrics" in sql:
                    raise RuntimeError('relation "run_metrics" does not exist')
                super().execute(sql, params)

        class ExplodingConn(FakeConn):
            def cursor(self) -> FakeCursor:
                return ExplodingCursor(self)

        conn = ExplodingConn(control_row=None, job_row=None)
        # Must not raise.
        runner._write_run_metrics(conn, "job-xyz", self._make_result())
        self.assertEqual(conn.commits, 0)
        self.assertEqual(conn.rollbacks, 1)

    # The runner owns exactly these 22 columns (job_id + 21). Kept in sync with
    # CONTRACT §1.5 / §2.13 and _METRICS_UPSERT_SQL; the count assertions below tie
    # the test to this list so a column added/dropped without updating both fails.
    _RUNNER_METRIC_COLUMNS = (
        "job_id",
        "audio_bytes", "audio_channels", "audio_sample_rate", "audio_codec",
        "audio_format", "transcribe_started_at", "transcribe_finished_at",
        "asr_model", "compute_type", "runner_host", "chunked", "n_windows",
        "char_count", "word_count", "segment_count",
        # §2.13 backend descriptor.
        "asr_family", "asr_runtime", "caps_applied", "caps_requested",
        "caps_skipped_reason", "mean_word_confidence",
    )

    def test_metrics_upsert_column_count(self) -> None:
        """Sanity-check the expected column count against the SQL itself."""
        self.assertEqual(len(self._RUNNER_METRIC_COLUMNS), 22)
        # The INSERT clause (before ON CONFLICT) must list exactly these columns.
        sql = runner._METRICS_UPSERT_SQL
        insert_clause = sql.split("ON CONFLICT", 1)[0]
        for col in self._RUNNER_METRIC_COLUMNS:
            self.assertIn(col, insert_clause)
        # Placeholder count in the INSERT clause must equal the column count.
        self.assertEqual(insert_clause.count("%s"), len(self._RUNNER_METRIC_COLUMNS))

    def test_metrics_upsert_executes_with_matching_param_count(self) -> None:
        conn = FakeConn(control_row=None, job_row=None)
        runner._write_run_metrics(conn, "job-def", self._make_result())

        # Find the actual run_metrics statement and its bound params.
        metrics_calls = [(s, p) for s, p in conn.executed if "run_metrics" in s]
        self.assertEqual(len(metrics_calls), 1)
        sql, params = metrics_calls[0]

        # Columns must appear in the INSERT clause, not merely the UPDATE clause.
        insert_clause = sql.split("ON CONFLICT", 1)[0]
        for col in self._RUNNER_METRIC_COLUMNS:
            with self.subTest(column=col):
                self.assertIn(col, insert_clause)

        # Param count must equal column count and INSERT-clause placeholder count.
        self.assertEqual(len(params), len(self._RUNNER_METRIC_COLUMNS))
        self.assertEqual(insert_clause.count("%s"), len(params))
        # job_id must be the first bound parameter.
        self.assertEqual(params[0], "job-def")


class ASRProviderTests(unittest.TestCase):
    """
    Tests for the ASRProvider seam: factory, NeMoParakeetProvider interface,
    and bias_terms behavior (disabled by default; active when ASR_BIASING_ENABLED=true).

    These tests do NOT require a GPU, NeMo, or torch — heavy imports are
    guarded by the same stub mechanism used throughout this file.
    """

    def setUp(self) -> None:
        # Ensure biasing is disabled for the baseline tests that verify plain-path
        # behavior.  Individual tests that exercise the biasing path set it to True.
        runner.ASR_BIASING_ENABLED = False

    def tearDown(self) -> None:
        runner.ASR_BIASING_ENABLED = False

    def test_make_provider_default_returns_nemo(self) -> None:
        """_make_provider('nemo-parakeet') must return NeMoParakeetProvider."""
        provider = runner._make_provider("nemo-parakeet")
        self.assertIsInstance(provider, runner.NeMoParakeetProvider)

    def test_make_provider_unknown_raises(self) -> None:
        """_make_provider with an unknown/empty/None backend must raise ValueError."""
        for bad in ("unknown-backend", "", None):
            with self.assertRaises(ValueError):
                runner._make_provider(bad)  # type: ignore[arg-type]

    def test_make_provider_env_default(self) -> None:
        """ASR_BACKEND module-level var defaults to 'nemo-parakeet'."""
        self.assertEqual(runner.ASR_BACKEND, "nemo-parakeet")

    def test_nemo_provider_capabilities_biasing_disabled(self) -> None:
        """capabilities() must be exactly {'word_timestamps'} when biasing is off."""
        runner.ASR_BIASING_ENABLED = False
        provider = runner.NeMoParakeetProvider()
        self.assertEqual(provider.capabilities(), {"word_timestamps"})
        self.assertNotIn("biasing", provider.capabilities())

    def test_nemo_provider_capabilities_biasing_enabled(self) -> None:
        """capabilities() must include 'biasing' when ASR_BIASING_ENABLED=true."""
        runner.ASR_BIASING_ENABLED = True
        provider = runner.NeMoParakeetProvider()
        caps = provider.capabilities()
        self.assertIn("word_timestamps", caps)
        self.assertIn("biasing", caps)

    def test_nemo_provider_transcribe_before_load_raises(self) -> None:
        """Calling transcribe() before load() must raise RuntimeError."""
        provider = runner.NeMoParakeetProvider()
        with self.assertRaises(RuntimeError):
            provider.transcribe(Path("/fake/file.m4b"))

    def test_nemo_provider_transcribe_accepts_bias_terms_none(self) -> None:
        """
        transcribe() must accept bias_terms=None without error.

        We use the same mock pattern as test_single_pass_invokes_to_mono_wav:
        patch _audio_probe, _to_mono_wav, and asr_model.transcribe so the call
        completes without real I/O or GPU access.
        """
        import unittest.mock as mock

        fake_probe = {
            "duration": 60.0,
            "channels": 1,
            "sample_rate": 16000,
            "codec_name": "pcm_s16le",
            "format_name": "wav",
            "size_bytes": 1920000,
        }
        mono_path = Path("/tmp/asr-mono-test.wav")
        fake_hyp = runner._StitchedHypothesis(
            "hello",
            [{"word": "hello", "start": 0.0, "end": 0.4}],
            [{"segment": "hello", "start": 0.0, "end": 0.4}],
        )
        mock_model = mock.MagicMock()
        mock_model.transcribe.return_value = [fake_hyp]

        provider = runner.NeMoParakeetProvider()
        provider._asr_model = mock_model
        provider._diarize_model = None

        with mock.patch.object(runner, "_audio_probe", return_value=fake_probe), \
             mock.patch.object(runner, "_to_mono_wav", return_value=mono_path), \
             mock.patch.object(runner.Path, "unlink"):
            # bias_terms=None — must not raise
            result = provider.transcribe(Path("/fake/file.wav"), bias_terms=None)

        # Deeper CONTRACT-field checks (not just language) so a real divergence
        # in the transcribe path under bias_terms would actually be caught.
        for field in ("language", "duration_seconds", "speaker_count",
                      "segments", "raw_text"):
            self.assertIn(field, result)
        self.assertEqual(result["language"], "en")
        self.assertEqual(result["raw_text"], "hello")
        self.assertTrue(result["segments"], "segments must be non-empty")
        # Verify segment structure, not just non-emptiness (CONTRACT §1.2.1).
        seg0 = result["segments"][0]
        for key in ("text", "start", "end"):
            self.assertIn(key, seg0)

    def test_nemo_provider_transcribe_bias_terms_ignored_when_disabled(self) -> None:
        """
        With ASR_BIASING_ENABLED=false (default), bias_terms is ignored:
        biased, empty, and plain calls must produce identical CONTRACT fields,
        and no boosting config must be applied (_apply_boosting_config not called).
        """
        import unittest.mock as mock

        runner.ASR_BIASING_ENABLED = False

        fake_probe = {
            "duration": 30.0,
            "channels": 1,
            "sample_rate": 16000,
            "codec_name": "pcm_s16le",
            "format_name": "wav",
            "size_bytes": 960000,
        }
        mono_path = Path("/tmp/asr-mono-test.wav")
        fake_hyp = runner._StitchedHypothesis(
            "world",
            [{"word": "world", "start": 0.0, "end": 0.3}],
            [{"segment": "world", "start": 0.0, "end": 0.3}],
        )
        mock_model = mock.MagicMock()
        mock_model.transcribe.return_value = [fake_hyp]

        provider = runner.NeMoParakeetProvider()
        provider._asr_model = mock_model
        provider._diarize_model = None

        with mock.patch.object(runner, "_audio_probe", return_value=fake_probe), \
             mock.patch.object(runner, "_to_mono_wav", return_value=mono_path), \
             mock.patch.object(runner.Path, "unlink"), \
             mock.patch.object(runner, "_apply_boosting_config") as mock_apply:

            result_biased = provider.transcribe(
                Path("/fake/file.wav"), bias_terms=["Parakeet", "NeMo"]
            )
            mock_model.reset_mock()
            mock_model.transcribe.return_value = [fake_hyp]

            result_empty = provider.transcribe(
                Path("/fake/file.wav"), bias_terms=[]
            )
            mock_model.reset_mock()
            mock_model.transcribe.return_value = [fake_hyp]

            result_plain = provider.transcribe(Path("/fake/file.wav"))

        # Boosting must NOT have been applied when disabled.
        mock_apply.assert_not_called()

        # All three forms must yield identical core CONTRACT fields.
        for r in (result_biased, result_empty):
            self.assertEqual(r["language"], result_plain["language"])
            self.assertEqual(r["raw_text"], result_plain["raw_text"])
            self.assertEqual(r["segments"], result_plain["segments"])
            self.assertEqual(r["speaker_count"], result_plain["speaker_count"])
            self.assertEqual(r["duration_seconds"], result_plain["duration_seconds"])

    def test_asr_provider_is_abstract(self) -> None:
        """ASRProvider cannot be instantiated directly (it is an ABC)."""
        with self.assertRaises(TypeError):
            runner.ASRProvider()  # type: ignore[abstract]


class BiasTermsDBTests(unittest.TestCase):
    """
    Tests for _fetch_bias_terms: claim-time book_metadata lookup.

    Uses the same FakeConn / FakeCursor plumbing as GateTests.
    """

    def _make_conn(self, bias_terms_value: object) -> "FakeConn":
        """Build a FakeConn whose bias_terms cursor returns bias_terms_value."""

        class BiasCursor(FakeCursor):
            def fetchone(self) -> object:
                if "book_metadata" in self.conn.last_sql:
                    return {"bias_terms": bias_terms_value}
                return super().fetchone()

        class BiasConn(FakeConn):
            def cursor(self) -> FakeCursor:
                return BiasCursor(self)

        return BiasConn(control_row=None, job_row=None)

    def test_returns_terms_for_known_book(self) -> None:
        """A populated bias_terms list is returned stripped and non-empty."""
        conn = self._make_conn(["Mithrandir", "Valinor", "Numenor"])
        result = runner._fetch_bias_terms(conn, "/books/Tolkien/LOTR/01.m4b")
        self.assertEqual(result, ["Mithrandir", "Valinor", "Numenor"])
        self.assertEqual(conn.commits, 1)

    def test_returns_none_for_missing_row(self) -> None:
        """A missing book_metadata row must return None (no crash)."""

        class MissingCursor(FakeCursor):
            def fetchone(self) -> object:
                if "book_metadata" in self.conn.last_sql:
                    return None
                return super().fetchone()

        class MissingConn(FakeConn):
            def cursor(self) -> FakeCursor:
                return MissingCursor(self)

        conn = MissingConn(control_row=None, job_row=None)
        result = runner._fetch_bias_terms(conn, "/books/Unknown/Book.m4b")
        self.assertIsNone(result)
        self.assertEqual(conn.commits, 1)

    def test_returns_none_for_null_column(self) -> None:
        """A NULL bias_terms column must return None."""
        conn = self._make_conn(None)
        result = runner._fetch_bias_terms(conn, "/books/Author/Title.m4b")
        self.assertIsNone(result)

    def test_returns_none_for_empty_list(self) -> None:
        """An empty bias_terms array must return None (nothing to boost)."""
        conn = self._make_conn([])
        result = runner._fetch_bias_terms(conn, "/books/Author/Title.m4b")
        self.assertIsNone(result)

    def test_strips_whitespace_terms(self) -> None:
        """Whitespace-only entries are filtered out; leading/trailing spaces stripped."""
        conn = self._make_conn(["  Elrond  ", "  ", "", "Rivendell"])
        result = runner._fetch_bias_terms(conn, "/books/Tolkien/Silmarillion.m4b")
        self.assertEqual(result, ["Elrond", "Rivendell"])

    def test_all_whitespace_returns_none(self) -> None:
        """If all terms are whitespace after stripping, return None."""
        conn = self._make_conn(["   ", "  "])
        result = runner._fetch_bias_terms(conn, "/books/Any/Book.m4b")
        self.assertIsNone(result)

    def test_book_dir_derived_from_file_path(self) -> None:
        """The SQL parameter must be dirname(file_path), not file_path itself."""
        conn = self._make_conn(["term"])
        runner._fetch_bias_terms(conn, "/books/Author/Book/chapter.m4b")
        # The SELECT must have been called with the parent dir, not the full path.
        book_dir_params = [
            p for s, p in conn.executed if "book_metadata" in s
        ]
        self.assertEqual(len(book_dir_params), 1)
        self.assertEqual(book_dir_params[0], ("/books/Author/Book",))

    def test_db_error_degrades_to_none(self) -> None:
        """A DB error must log a warning and return None (never raise)."""

        class ExplodingCursor(FakeCursor):
            def execute(self, sql: str, params: object = None) -> None:
                if "book_metadata" in sql:
                    raise RuntimeError("relation does not exist")
                super().execute(sql, params)

        class ExplodingConn(FakeConn):
            def cursor(self) -> FakeCursor:
                return ExplodingCursor(self)

        conn = ExplodingConn(control_row=None, job_row=None)
        result = runner._fetch_bias_terms(conn, "/books/Any/Book.m4b")
        self.assertIsNone(result)
        self.assertEqual(conn.rollbacks, 1)


class BiasConfigTests(unittest.TestCase):
    """
    Tests for NeMoParakeetProvider.transcribe() with ASR_BIASING_ENABLED=true:
    verify _apply_boosting_config is called with a key_phrases file containing the
    terms, change_decoding_strategy is called, word timestamps are preserved, and
    the config is cleared afterward (even on error).

    NeMo/torch are NOT imported — the model is a MagicMock and _apply_boosting_config
    / _clear_boosting_config are patched so the test runs anywhere.
    """

    def setUp(self) -> None:
        runner.ASR_BIASING_ENABLED = True
        runner.ASR_BIASING_ALPHA = 2.5

    def tearDown(self) -> None:
        runner.ASR_BIASING_ENABLED = False
        runner.ASR_BIASING_ALPHA = 2.5

    def _make_provider_with_mock_model(self, fake_hyp: object) -> tuple[
        "runner.NeMoParakeetProvider", object
    ]:
        import unittest.mock as mock

        mock_model = mock.MagicMock()
        mock_model.transcribe.return_value = [fake_hyp]
        provider = runner.NeMoParakeetProvider()
        provider._asr_model = mock_model
        provider._diarize_model = None
        return provider, mock_model

    def _fake_probe(self, duration: float = 60.0) -> dict:
        return {
            "duration": duration,
            "channels": 1,
            "sample_rate": 16000,
            "codec_name": "pcm_s16le",
            "format_name": "wav",
            "size_bytes": 960000,
        }

    def _fake_hyp(self) -> object:
        return runner._StitchedHypothesis(
            "Mithrandir Valinor",
            [
                {"word": "Mithrandir", "start": 0.0, "end": 0.5},
                {"word": "Valinor", "start": 0.6, "end": 1.0},
            ],
            [{"segment": "Mithrandir Valinor", "start": 0.0, "end": 1.0}],
        )

    def test_boosting_config_applied_with_correct_terms(self) -> None:
        """
        With biasing enabled and non-empty bias_terms, _apply_boosting_config must
        be called with a path to a key_phrases file that contains the terms
        (one per line), and change_decoding_strategy must have been called on the
        model (indirectly via _apply_boosting_config).
        """
        import unittest.mock as mock
        import os as _os

        provider, mock_model = self._make_provider_with_mock_model(self._fake_hyp())
        mono_path = Path("/tmp/asr-mono-test.wav")
        terms = ["Mithrandir", "Valinor", "Numenor"]

        written_content: list[str] = []

        def capture_apply(model: object, phrases_path: str, alpha: float) -> None:
            with open(phrases_path) as f:
                written_content.append(f.read())

        with mock.patch.object(runner, "_audio_probe", return_value=self._fake_probe()), \
             mock.patch.object(runner, "_to_mono_wav", return_value=mono_path), \
             mock.patch.object(runner.Path, "unlink"), \
             mock.patch.object(runner, "_apply_boosting_config",
                               side_effect=capture_apply) as mock_apply, \
             mock.patch.object(runner, "_clear_boosting_config") as mock_clear:

            result = provider.transcribe(Path("/fake/file.wav"), bias_terms=terms)

        # _apply_boosting_config must have been called exactly once.
        mock_apply.assert_called_once()
        _model_arg, phrases_path_arg, alpha_arg = mock_apply.call_args[0]
        self.assertEqual(alpha_arg, 2.5)

        # The key_phrases file must contain one term per line.
        self.assertEqual(len(written_content), 1)
        file_lines = [ln for ln in written_content[0].splitlines() if ln]
        self.assertEqual(file_lines, terms)

        # Boosting config must always be cleared afterward.
        mock_clear.assert_called_once()

        # Word timestamps must survive biasing (CONTRACT §1.2.1).
        self.assertEqual(result["raw_text"], "Mithrandir Valinor")
        segs = result["segments"]
        self.assertGreater(len(segs), 0)
        words = segs[0]["words"]
        self.assertGreater(len(words), 0)
        self.assertEqual(words[0]["word"], "Mithrandir")
        self.assertIsNotNone(words[0]["start"])
        self.assertIsNotNone(words[0]["end"])

    def test_boosting_config_cleared_after_transcription_error(self) -> None:
        """
        If _transcribe_file raises, _clear_boosting_config must still be called
        (finally block) so the next un-biased job does not inherit stale config.
        The original transcription error must propagate to the caller.
        """
        import unittest.mock as mock

        provider, mock_model = self._make_provider_with_mock_model(None)
        # Make _transcribe_file raise inside the provider.
        with mock.patch.object(runner, "_audio_probe",
                               side_effect=RuntimeError("probe failed")), \
             mock.patch.object(runner, "_apply_boosting_config") as mock_apply, \
             mock.patch.object(runner, "_clear_boosting_config") as mock_clear:

            with self.assertRaises(RuntimeError, msg="transcription error must propagate"):
                provider.transcribe(Path("/fake/file.wav"),
                                    bias_terms=["Mithrandir"])

        mock_apply.assert_called_once()
        mock_clear.assert_called_once()

    def test_clear_config_raises_on_failure(self) -> None:
        """
        _clear_boosting_config must re-raise exceptions rather than swallowing them.
        Swallowing would silently leave the model in a stale biased state and corrupt
        subsequent un-biased jobs.
        """
        import unittest.mock as mock

        provider, mock_model = self._make_provider_with_mock_model(self._fake_hyp())
        mono_path = Path("/tmp/asr-mono-test.wav")

        with mock.patch.object(runner, "_audio_probe", return_value=self._fake_probe()), \
             mock.patch.object(runner, "_to_mono_wav", return_value=mono_path), \
             mock.patch.object(runner.Path, "unlink"), \
             mock.patch.object(runner, "_apply_boosting_config"), \
             mock.patch.object(runner, "_clear_boosting_config",
                               side_effect=RuntimeError("clear failed")):

            # The re-raised error from _clear_boosting_config must propagate.
            with self.assertRaises(RuntimeError, msg="clear error must propagate"):
                provider.transcribe(Path("/fake/file.wav"),
                                    bias_terms=["Mithrandir"])

    def test_empty_bias_terms_skips_boosting(self) -> None:
        """
        An empty bias_terms list must NOT apply boosting even when biasing is
        enabled (nothing to write to the key_phrases file).
        """
        import unittest.mock as mock

        provider, mock_model = self._make_provider_with_mock_model(self._fake_hyp())
        mono_path = Path("/tmp/asr-mono-test.wav")

        with mock.patch.object(runner, "_audio_probe", return_value=self._fake_probe()), \
             mock.patch.object(runner, "_to_mono_wav", return_value=mono_path), \
             mock.patch.object(runner.Path, "unlink"), \
             mock.patch.object(runner, "_apply_boosting_config") as mock_apply:

            provider.transcribe(Path("/fake/file.wav"), bias_terms=[])

        mock_apply.assert_not_called()

    def test_none_bias_terms_skips_boosting(self) -> None:
        """bias_terms=None must skip boosting even when biasing is enabled."""
        import unittest.mock as mock

        provider, mock_model = self._make_provider_with_mock_model(self._fake_hyp())
        mono_path = Path("/tmp/asr-mono-test.wav")

        with mock.patch.object(runner, "_audio_probe", return_value=self._fake_probe()), \
             mock.patch.object(runner, "_to_mono_wav", return_value=mono_path), \
             mock.patch.object(runner.Path, "unlink"), \
             mock.patch.object(runner, "_apply_boosting_config") as mock_apply:

            provider.transcribe(Path("/fake/file.wav"), bias_terms=None)

        mock_apply.assert_not_called()

    def test_boosting_applied_on_chunked_path(self) -> None:
        """
        The chunked transcription path picks up boosting because _apply_boosting_config
        mutates the model before _transcribe_file is called — both single-pass and
        chunked paths call asr_model.transcribe() and see the same model config.

        We simulate the chunked path by setting duration > CHUNK_THRESHOLD_SECONDS.
        _transcribe_chunked is patched to return a stitched hypothesis so we do
        not need a real ffmpeg or GPU.
        """
        import unittest.mock as mock

        # duration > CHUNK_THRESHOLD_SECONDS to trigger chunked path
        long_duration = runner.CHUNK_THRESHOLD_SECONDS + 10.0
        provider, mock_model = self._make_provider_with_mock_model(self._fake_hyp())

        fake_probe = self._fake_probe(duration=long_duration)
        chunked_hyp = self._fake_hyp()

        with mock.patch.object(runner, "_audio_probe", return_value=fake_probe), \
             mock.patch.object(runner, "_transcribe_chunked",
                               return_value=([chunked_hyp], 2)) as mock_chunked, \
             mock.patch.object(runner, "_apply_boosting_config") as mock_apply, \
             mock.patch.object(runner, "_clear_boosting_config") as mock_clear:

            result = provider.transcribe(
                Path("/fake/long.wav"), bias_terms=["Mithrandir"]
            )

        # Boosting must be applied (before _transcribe_file calls _transcribe_chunked).
        mock_apply.assert_called_once()
        # Chunked path must have been taken.
        mock_chunked.assert_called_once()
        # Config cleared afterward.
        mock_clear.assert_called_once()
        # Word timestamps survive.
        self.assertEqual(result["raw_text"], "Mithrandir Valinor")

    def test_key_phrases_tempfile_has_restrictive_permissions(self) -> None:
        """
        os.chmod must be called with 0o600 on the key_phrases temp file immediately
        after mkstemp — before any data is written — regardless of process umask.
        Bias terms are domain metadata; defense-in-depth keeps them owner-only.
        """
        import unittest.mock as mock

        provider, mock_model = self._make_provider_with_mock_model(self._fake_hyp())
        mono_path = Path("/tmp/asr-mono-test.wav")
        created_path: list[str] = []
        chmod_calls: list[tuple] = []

        real_mkstemp = runner.tempfile.mkstemp

        def capturing_mkstemp(**kwargs: object) -> tuple[int, str]:
            fd, path = real_mkstemp(**kwargs)
            created_path.append(path)
            return fd, path

        real_chmod = runner.os.chmod

        def capturing_chmod(path: str, mode: int) -> None:
            chmod_calls.append((path, mode))
            real_chmod(path, mode)

        with mock.patch.object(runner, "_audio_probe", return_value=self._fake_probe()), \
             mock.patch.object(runner, "_to_mono_wav", return_value=mono_path), \
             mock.patch.object(runner.Path, "unlink"), \
             mock.patch.object(runner, "_apply_boosting_config"), \
             mock.patch.object(runner, "_clear_boosting_config"), \
             mock.patch.object(runner.tempfile, "mkstemp",
                               side_effect=capturing_mkstemp), \
             mock.patch.object(runner.os, "chmod", side_effect=capturing_chmod):

            provider.transcribe(Path("/fake/file.wav"), bias_terms=["term"])

        bias_files = [p for p in created_path if "asr-bias-" in p]
        self.assertGreater(len(bias_files), 0)
        # chmod(path, 0o600) must have been called for each bias tempfile.
        for path in bias_files:
            matching = [(p, m) for p, m in chmod_calls if p == path]
            self.assertEqual(len(matching), 1, f"expected one chmod call for {path}")
            self.assertEqual(matching[0][1], 0o600,
                             f"expected 0o600 but got {oct(matching[0][1])}")

    def test_key_phrases_tempfile_deleted_after_transcription(self) -> None:
        """
        The key_phrases temp file must be deleted after transcription — whether
        it succeeded or not — to avoid /tmp leaks.
        """
        import unittest.mock as mock

        provider, mock_model = self._make_provider_with_mock_model(self._fake_hyp())
        mono_path = Path("/tmp/asr-mono-test.wav")
        created_files: list[str] = []

        real_mkstemp = runner.tempfile.mkstemp

        def capturing_mkstemp(**kwargs: object) -> tuple[int, str]:
            fd, path = real_mkstemp(**kwargs)
            created_files.append(path)
            return fd, path

        with mock.patch.object(runner, "_audio_probe", return_value=self._fake_probe()), \
             mock.patch.object(runner, "_to_mono_wav", return_value=mono_path), \
             mock.patch.object(runner.Path, "unlink"), \
             mock.patch.object(runner, "_apply_boosting_config"), \
             mock.patch.object(runner, "_clear_boosting_config"), \
             mock.patch.object(runner.tempfile, "mkstemp",
                               side_effect=capturing_mkstemp):

            provider.transcribe(Path("/fake/file.wav"), bias_terms=["term"])

        # Every temp file created by the biasing path must be gone.
        bias_files = [p for p in created_files if "asr-bias-" in p]
        self.assertGreater(len(bias_files), 0, "expected at least one bias tempfile")
        for path in bias_files:
            self.assertFalse(
                Path(path).exists(),
                f"temp key_phrases file leaked: {path}",
            )


class CapabilityDescriptorTests(unittest.TestCase):
    """
    CONTRACT §2.13 backend descriptor: _build_capability_descriptor,
    _mean_word_confidence, ASR_CAPABILITIES parsing, and the env-derived
    family/runtime defaults — derived from the runner's ACTUAL code behavior.
    """

    def setUp(self) -> None:
        self._orig_diarize = runner.ASR_DIARIZE
        self._orig_biasing = runner.ASR_BIASING_ENABLED

    def tearDown(self) -> None:
        runner.ASR_DIARIZE = self._orig_diarize
        runner.ASR_BIASING_ENABLED = self._orig_biasing

    # -- _mean_word_confidence ------------------------------------------------

    def test_mean_word_confidence_none_when_no_scores(self) -> None:
        """Parakeet-TDT emits score=None for every word → mean is None (NULL)."""
        segments = [
            {"words": [{"word": "a", "score": None}, {"word": "b", "score": None}]},
            {"words": [{"word": "c", "score": None}]},
        ]
        self.assertIsNone(runner._mean_word_confidence(segments))

    def test_mean_word_confidence_none_when_no_words(self) -> None:
        self.assertIsNone(runner._mean_word_confidence([{"words": []}]))
        self.assertIsNone(runner._mean_word_confidence([]))

    def test_mean_word_confidence_averages_present_scores(self) -> None:
        """Generic averaging works for a (hypothetical) scoring backend."""
        segments = [
            {"words": [{"word": "a", "score": 0.8}, {"word": "b", "score": 0.6}]},
            {"words": [{"word": "c", "score": 1.0}]},
        ]
        self.assertAlmostEqual(runner._mean_word_confidence(segments), 0.8)

    def test_mean_word_confidence_skips_missing_scores(self) -> None:
        """Words without a numeric score are ignored, not treated as 0."""
        segments = [
            {"words": [{"word": "a", "score": 0.5}, {"word": "b", "score": None}]},
        ]
        self.assertAlmostEqual(runner._mean_word_confidence(segments), 0.5)

    # -- _build_capability_descriptor -----------------------------------------

    def _segments_with_words(self, speaker: object = None) -> list:
        return [
            {
                "words": [
                    {"word": "hi", "start": 0.0, "end": 0.2, "score": None,
                     "speaker": speaker},
                ],
            }
        ]

    def test_descriptor_plain_run_no_bias_no_diarize(self) -> None:
        """Baseline single-narrator run: only word_timestamps applied/requested."""
        runner.ASR_DIARIZE = False
        d = runner._build_capability_descriptor(
            self._segments_with_words(),
            speaker_count=None,
            requested_biasing=False,
            applied_biasing=False,
            requested_diarization=False,
        )
        self.assertEqual(d["asr_family"], runner.ASR_FAMILY)
        self.assertEqual(d["asr_runtime"], runner.ASR_RUNTIME)
        self.assertEqual(
            d["caps_applied"],
            {
                "word_timestamps": True,
                "context_biasing": False,
                "diarization": False,
                "confidence_scores": False,
                "language_detection": False,
            },
        )
        # Only word_timestamps is requested on a plain job.
        self.assertEqual(d["caps_requested"], {"word_timestamps": True})
        # No requested cap was skipped → NULL (column omitted).
        self.assertIsNone(d["caps_skipped_reason"])
        # Parakeet-TDT emits no scores → NULL.
        self.assertIsNone(d["mean_word_confidence"])

    def test_descriptor_biasing_applied(self) -> None:
        """Bias terms present AND boosting applied → context_biasing applied+requested."""
        d = runner._build_capability_descriptor(
            self._segments_with_words(),
            speaker_count=None,
            requested_biasing=True,
            applied_biasing=True,
            requested_diarization=False,
        )
        self.assertTrue(d["caps_applied"]["context_biasing"])
        self.assertTrue(d["caps_requested"]["context_biasing"])
        # Applied → no skip reason.
        self.assertIsNone(d["caps_skipped_reason"])

    def test_descriptor_biasing_requested_but_declined(self) -> None:
        """
        The honest-degradation case (§2.13): the book had bias_terms but boosting
        was NOT applied (ASR_BIASING_ENABLED off). context_biasing must be
        applied=false, requested=true, with a skip reason.
        """
        runner.ASR_BIASING_ENABLED = False
        d = runner._build_capability_descriptor(
            self._segments_with_words(),
            speaker_count=None,
            requested_biasing=True,
            applied_biasing=False,
            requested_diarization=False,
        )
        self.assertFalse(d["caps_applied"]["context_biasing"])
        self.assertTrue(d["caps_requested"]["context_biasing"])
        self.assertIn("context_biasing", d["caps_skipped_reason"])
        self.assertIsInstance(d["caps_skipped_reason"]["context_biasing"], str)

    def test_descriptor_diarization_applied(self) -> None:
        """ASR_DIARIZE on AND speakers assigned → diarization applied+requested."""
        d = runner._build_capability_descriptor(
            self._segments_with_words(speaker="SPEAKER_00"),
            speaker_count=2,
            requested_biasing=False,
            applied_biasing=False,
            requested_diarization=True,
        )
        self.assertTrue(d["caps_applied"]["diarization"])
        self.assertTrue(d["caps_requested"]["diarization"])
        self.assertIsNone(d["caps_skipped_reason"])

    def test_descriptor_diarization_requested_but_no_speakers(self) -> None:
        """ASR_DIARIZE on but speaker_count is None → declined with a reason."""
        d = runner._build_capability_descriptor(
            self._segments_with_words(),
            speaker_count=None,
            requested_biasing=False,
            applied_biasing=False,
            requested_diarization=True,
        )
        self.assertFalse(d["caps_applied"]["diarization"])
        self.assertTrue(d["caps_requested"]["diarization"])
        self.assertIn("diarization", d["caps_skipped_reason"])

    def test_descriptor_word_timestamps_false_when_no_words(self) -> None:
        """If no segment carries words, word_timestamps applied=false (honest)."""
        d = runner._build_capability_descriptor(
            [{"words": []}],
            speaker_count=None,
            requested_biasing=False,
            applied_biasing=False,
            requested_diarization=False,
        )
        self.assertFalse(d["caps_applied"]["word_timestamps"])

    def test_descriptor_caps_are_closed_enum_keys(self) -> None:
        """caps_applied keys must be a subset of the §2.13 closed enum."""
        d = runner._build_capability_descriptor(
            self._segments_with_words(),
            speaker_count=None,
            requested_biasing=True,
            applied_biasing=False,
            requested_diarization=True,
        )
        for key in d["caps_applied"]:
            self.assertIn(key, runner._CAPABILITY_KEYS)
        for key in d["caps_requested"]:
            self.assertIn(key, runner._CAPABILITY_KEYS)
        for key in d["caps_skipped_reason"]:
            self.assertIn(key, runner._CAPABILITY_KEYS)

    # -- ASR_CAPABILITIES parsing ---------------------------------------------

    def test_parse_capabilities_none_when_unset(self) -> None:
        self.assertIsNone(runner._parse_advertised_capabilities(None))
        self.assertIsNone(runner._parse_advertised_capabilities(""))
        self.assertIsNone(runner._parse_advertised_capabilities("   "))

    def test_parse_capabilities_valid_json(self) -> None:
        out = runner._parse_advertised_capabilities(
            '{"word_timestamps": true, "context_biasing": false}'
        )
        self.assertEqual(out, {"word_timestamps": True, "context_biasing": False})

    def test_parse_capabilities_drops_unknown_keys(self) -> None:
        """Unknown enum keys are ignored (forward-compat), known keys kept."""
        out = runner._parse_advertised_capabilities(
            '{"word_timestamps": true, "telepathy": true}'
        )
        self.assertEqual(out, {"word_timestamps": True})

    def test_parse_capabilities_malformed_returns_none(self) -> None:
        """Malformed/non-object JSON is advisory-only: warn and return None."""
        self.assertIsNone(runner._parse_advertised_capabilities("not json"))
        self.assertIsNone(runner._parse_advertised_capabilities("[1, 2, 3]"))
        self.assertIsNone(runner._parse_advertised_capabilities('"a string"'))

    # -- env-derived family/runtime defaults ----------------------------------

    def test_family_runtime_defaults(self) -> None:
        """Defaults describe this concrete runner: nemo-parakeet on nemo-cuda."""
        self.assertEqual(runner.ASR_FAMILY, "nemo-parakeet")
        self.assertEqual(runner.ASR_RUNTIME, "nemo-cuda")

    # -- end-to-end through _transcribe_file ----------------------------------

    def test_transcribe_file_emits_descriptor(self) -> None:
        """
        _transcribe_file's result dict must carry the §2.13 descriptor keys, with
        word_timestamps applied (Parakeet-TDT native), confidence_scores/
        language_detection always false, and mean_word_confidence None.
        """
        import unittest.mock as mock

        runner.ASR_DIARIZE = False
        fake_probe = {
            "duration": 30.0, "channels": 1, "sample_rate": 16000,
            "codec_name": "pcm_s16le", "format_name": "wav", "size_bytes": 100,
        }
        fake_hyp = runner._StitchedHypothesis(
            "hello world",
            [{"word": "hello", "start": 0.0, "end": 0.4},
             {"word": "world", "start": 0.5, "end": 0.9}],
            [{"segment": "hello world", "start": 0.0, "end": 0.9}],
        )
        mock_model = mock.MagicMock()
        mock_model.transcribe.return_value = [fake_hyp]

        with mock.patch.object(runner, "_audio_probe", return_value=fake_probe), \
             mock.patch.object(runner, "_to_mono_wav", return_value=Path("/tmp/x.wav")), \
             mock.patch.object(runner.Path, "unlink"):
            result = runner._transcribe_file(
                Path("/fake/file.wav"), mock_model, None,
                requested_biasing=False, applied_biasing=False,
            )

        self.assertEqual(result["asr_family"], "nemo-parakeet")
        self.assertEqual(result["asr_runtime"], "nemo-cuda")
        self.assertTrue(result["caps_applied"]["word_timestamps"])
        self.assertFalse(result["caps_applied"]["confidence_scores"])
        self.assertFalse(result["caps_applied"]["language_detection"])
        self.assertEqual(result["caps_requested"], {"word_timestamps": True})
        self.assertIsNone(result["caps_skipped_reason"])
        self.assertIsNone(result["mean_word_confidence"])


class RunMetricsDescriptorBindingTests(unittest.TestCase):
    """The §2.13 descriptor columns must be bound into the run_metrics UPSERT."""

    def _result(self) -> dict:
        return {
            "transcribe_started_at": None, "transcribe_finished_at": None,
            "chunked": False, "n_windows": 1, "char_count": 5, "word_count": 1,
            "segment_count": 1, "audio_channels": 1, "audio_sample_rate": 16000,
            "audio_codec": "pcm_s16le", "audio_format": "wav", "audio_bytes": 100,
            "asr_family": "nemo-parakeet", "asr_runtime": "nemo-cuda",
            "caps_applied": {"word_timestamps": True, "context_biasing": False},
            "caps_requested": {"word_timestamps": True, "context_biasing": True},
            "caps_skipped_reason": {"context_biasing": "biasing disabled"},
            "mean_word_confidence": None,
        }

    def test_descriptor_columns_present_in_sql(self) -> None:
        insert_clause = runner._METRICS_UPSERT_SQL.split("ON CONFLICT", 1)[0]
        for col in ("asr_family", "asr_runtime", "caps_applied",
                    "caps_requested", "caps_skipped_reason", "mean_word_confidence"):
            self.assertIn(col, insert_clause)
        # JSONB columns must be cast so a JSON string binds as jsonb.
        self.assertEqual(insert_clause.count("::jsonb"), 3)

    def test_descriptor_bound_with_json_serialized_caps(self) -> None:
        conn = FakeConn(control_row=None, job_row=None)
        runner._write_run_metrics(conn, "job-desc", self._result())

        metrics_calls = [(s, p) for s, p in conn.executed if "run_metrics" in s]
        self.assertEqual(len(metrics_calls), 1)
        _sql, params = metrics_calls[0]

        import json as _json
        # Param order matches the INSERT column order: the 6 descriptor params are
        # the last 6 bound values. Locate them by parsing the JSON ones back.
        # caps_applied / caps_requested / caps_skipped_reason are JSON strings;
        # mean_word_confidence is None; asr_family/asr_runtime are plain strings.
        self.assertIn("nemo-parakeet", params)
        self.assertIn("nemo-cuda", params)
        # The caps must be present as JSON-serialized strings (not dicts).
        json_params = [p for p in params if isinstance(p, str) and p.startswith("{")]
        decoded = [_json.loads(p) for p in json_params]
        self.assertIn({"word_timestamps": True, "context_biasing": False}, decoded)
        self.assertIn({"word_timestamps": True, "context_biasing": True}, decoded)
        self.assertIn({"context_biasing": "biasing disabled"}, decoded)

    def test_null_caps_skipped_reason_binds_none(self) -> None:
        """A None caps_skipped_reason must bind as None (SQL NULL), not 'null'."""
        result = self._result()
        result["caps_skipped_reason"] = None
        conn = FakeConn(control_row=None, job_row=None)
        runner._write_run_metrics(conn, "job-null", result)
        _sql, params = [
            (s, p) for s, p in conn.executed if "run_metrics" in s
        ][0]
        # The JSON 'null' literal must never appear among bound params.
        self.assertNotIn("null", params)
        self.assertIn(None, params)


if __name__ == "__main__":
    unittest.main(verbosity=2)
