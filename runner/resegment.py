#!/usr/bin/env python3
"""
One-off backfill: re-segment existing transcripts from their stored per-word
timestamps, WITHOUT re-running ASR.

Transcripts produced before fine-grained segmentation store one ~600 s segment
per transcription window (NeMo Parakeet-TDT emits exactly one segment per
transcribe() call). The accurate per-word timestamps are, however, already
persisted in each segment's words[] array, so we can rebuild sentence-sized
segments offline — no GPU, no re-transcription. See CONTRACT §1.2.1.

This reuses runner.resegment_existing (the same _words_to_segments path the live
runner now uses), so backfilled transcripts match freshly-produced ones.

Usage (run on the ASR host, where DATABASE_URL + the runner venv exist):

    python3 resegment.py                 # DRY RUN — report only, no writes
    python3 resegment.py --apply         # write the new segments
    python3 resegment.py --apply --limit 5   # smoke-test on 5 transcripts first

After --apply, rebuild the embedding chunks from the new segments (Go service,
in-cluster):

    earmark requeue --reembed "" --yes

The backfill is idempotent (re-segmenting already-fine segments reproduces them)
and only writes rows whose segmentation actually changed.
"""

from __future__ import annotations

import argparse
import json
import os
import sys

import psycopg2  # type: ignore[import]

import runner  # reuse resegment_existing / _words_to_segments


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--apply",
        action="store_true",
        help="write the new segments (default: dry run, report only)",
    )
    ap.add_argument(
        "--limit",
        type=int,
        default=0,
        help="process at most N transcripts (0 = all)",
    )
    ap.add_argument(
        "--commit-every",
        type=int,
        default=100,
        help="commit after this many writes (only with --apply)",
    )
    args = ap.parse_args()

    dsn = os.environ.get("DATABASE_URL")
    if not dsn:
        print("DATABASE_URL is required", file=sys.stderr)
        return 2

    conn = psycopg2.connect(dsn)
    conn.autocommit = False

    # IDs are tiny; fetch them up front so writes don't disturb a server-side
    # read cursor. The (large) segments JSONB is fetched one row at a time.
    with conn.cursor() as c:
        c.execute("SELECT id FROM transcripts ORDER BY created_at")
        ids = [r[0] for r in c.fetchall()]
    if args.limit:
        ids = ids[: args.limit]

    total = changed = written = skipped_empty = 0
    seg_before = seg_after = 0

    rcur = conn.cursor()
    wcur = conn.cursor()
    for tid in ids:
        rcur.execute("SELECT segments FROM transcripts WHERE id = %s", (tid,))
        row = rcur.fetchone()
        if row is None:
            continue
        segments = row[0]
        if isinstance(segments, str):
            segments = json.loads(segments)

        new_segments = runner.resegment_existing(segments)
        total += 1
        seg_before += len(segments)

        # Guard: never overwrite a non-empty transcript with an empty result
        # (would happen only if the stored words[] were missing).
        if not new_segments and segments:
            skipped_empty += 1
            seg_after += len(segments)
            continue

        seg_after += len(new_segments)
        if len(new_segments) == len(segments):
            # Already fine-grained (idempotent no-op) — skip the write.
            continue
        changed += 1

        if args.apply:
            wcur.execute(
                "UPDATE transcripts SET segments = %s WHERE id = %s",
                (json.dumps(new_segments), tid),
            )
            written += 1
            if written % args.commit_every == 0:
                conn.commit()

    if args.apply:
        conn.commit()
    conn.close()

    mode = "APPLY" if args.apply else "DRY-RUN"
    avg_before = seg_before / total if total else 0
    avg_after = seg_after / total if total else 0
    print(
        f"[{mode}] transcripts={total} changed={changed} written={written} "
        f"skipped_empty_guard={skipped_empty} "
        f"avg_segments {avg_before:.1f} -> {avg_after:.1f}"
    )
    if not args.apply and changed:
        print("re-run with --apply to write; then: earmark requeue --reembed \"\" --yes")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
