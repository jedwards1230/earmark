package db

// Postgres integration tests for `earmark reanchor` (CONTRACT §2.17
// "Re-anchoring"). Skipped unless EARMARK_TEST_DATABASE_URL is set (see
// migrate_integration_test.go).

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/patch"
)

// Fixture ids. Transcript a1 has five re-chunked chunks; b1 has none yet
// (mid re-embed).
const (
	raT1 = "00000000-0000-0000-0000-0000000000a1"
	raT2 = "00000000-0000-0000-0000-0000000000b1"
	raT3 = "00000000-0000-0000-0000-0000000000d1"

	raC0 = "00000000-0000-0000-0000-0000000000c0"
	raC1 = "00000000-0000-0000-0000-0000000000c1"
	raC2 = "00000000-0000-0000-0000-0000000000c2"
	raC3 = "00000000-0000-0000-0000-0000000000c3"
	raC4 = "00000000-0000-0000-0000-0000000000c4"

	// deadChunk is the chunk id every legacy finding names: it no longer exists.
	deadChunk = "00000000-0000-0000-0000-00000000dead"

	fUnique         = "00000000-0000-0000-0000-0000000000f1"
	fMovedWindow    = "00000000-0000-0000-0000-0000000000f2"
	fOutsideWinA    = "00000000-0000-0000-0000-0000000000f3"
	fAmbigNamed     = "00000000-0000-0000-0000-0000000000f4"
	fOutsideWinB    = "00000000-0000-0000-0000-0000000000f5"
	fSubword        = "00000000-0000-0000-0000-0000000000f6"
	fCase           = "00000000-0000-0000-0000-0000000000f7"
	fPunct          = "00000000-0000-0000-0000-0000000000f8"
	fGone           = "00000000-0000-0000-0000-0000000000f9"
	fHaikuAlready   = "00000000-0000-0000-0000-0000000000e1"
	fAccepted       = "00000000-0000-0000-0000-0000000000e2"
	fUnanchorable   = "00000000-0000-0000-0000-0000000000e3"
	fPending        = "00000000-0000-0000-0000-0000000000e4"
	fMisquoted      = "00000000-0000-0000-0000-0000000000e5"
	fOutOfWindow    = "00000000-0000-0000-0000-0000000000e6"
	fWindowAmbig    = "00000000-0000-0000-0000-0000000000e7"
	fWindowless     = "00000000-0000-0000-0000-0000000000e8"
	fStraddle       = "00000000-0000-0000-0000-0000000000e9"
	fDecoy          = "00000000-0000-0000-0000-0000000000ea"
	fStaleIDAlready = "00000000-0000-0000-0000-0000000000eb"
)

// Pristine chunk text (source_text). Chunk 0's corrected surface (text)
// differs, so a matcher reading the wrong column fails F1.
var raPristine = map[string]string{
	raC0: "Leto and ganema walked to the sietch.",
	raC1: "the fox and the fox ran.",
	raC2: "Stilgar waited by the rock.",
	raC3: "Later, proganema was a word.",
	raC4: "Stilgar spoke. Then Chani arrived.",
}

func seedReanchor(t *testing.T, d *DB) {
	t.Helper()
	ctx := context.Background()
	sha := patch.ChunkHash(raPristine[raC2])
	shaC1 := patch.ChunkHash(raPristine[raC1])
	off := strings.Index(raPristine[raC2], "rock")
	_, err := d.pool.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('00000000-0000-0000-0000-00000000000a', '/b/Dune/02 Children of Dune.m4b', 'c1', 'done'),
		  ('00000000-0000-0000-0000-00000000000b', '/b/Other/01.m4b', 'c2', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
		                         segments, raw_text, model_name) VALUES
		  ('`+raT1+`', '00000000-0000-0000-0000-00000000000a', '/b/Dune/02 Children of Dune.m4b',
		   'c1', 'en', 150, '[]', 'x', 'parakeet'),
		  ('`+raT2+`', '00000000-0000-0000-0000-00000000000b', '/b/Other/01.m4b',
		   'c2', 'en', 60, '[]', 'x', 'parakeet');
		INSERT INTO transcript_chunks (id, transcript_id, file_path, chunk_index, start_sec, end_sec,
		                               text, source_text, embedding) VALUES
		  ('`+raC0+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', 0,   0,  30,
		   'Leto and Ghanima walked to the sietch.', '`+raPristine[raC0]+`', array_fill(0.1, ARRAY[768])::vector),
		  ('`+raC1+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', 1,  30,  60,
		   '`+raPristine[raC1]+`', '`+raPristine[raC1]+`', array_fill(0.1, ARRAY[768])::vector),
		  ('`+raC2+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', 2,  60,  90,
		   '`+raPristine[raC2]+`', '`+raPristine[raC2]+`', array_fill(0.1, ARRAY[768])::vector),
		  ('`+raC3+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', 3,  90, 120,
		   '`+raPristine[raC3]+`', NULL, array_fill(0.1, ARRAY[768])::vector),
		  ('`+raC4+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', 4, 120, 150,
		   '`+raPristine[raC4]+`', '`+raPristine[raC4]+`', array_fill(0.1, ARRAY[768])::vector);
		INSERT INTO transcript_findings (id, transcript_id, file_path, chunk_id, chunk_index, start_sec, end_sec,
		       original_text, issue_type, suggested_correction, confidence, model, patch_state,
		       chunk_text_sha256, anchor_offset, anchor_occurrence) VALUES
		  -- gemma era: dead chunk id, no hash/offset, timings of the OLD chunking
		  ('`+fUnique+`',      '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 0,  0, 30, 'ganema',  'misheard_proper_noun', 'Ghanima', 0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  ('`+fMovedWindow+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 1, 30, 90, 'Stilgar', 'misheard_proper_noun', 'Stilgar', 0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  ('`+fOutsideWinA+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 0,  0, 30, 'Chani',   'misheard_proper_noun', 'Chani',   0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  ('`+fAmbigNamed+`',  '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 1, 30, 60, 'the fox', 'misheard_word',        'a fox',   0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  ('`+fOutsideWinB+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 0,  0, 30, 'Stilgar', 'misheard_proper_noun', 'Stilgar', 0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  ('`+fSubword+`',     '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 0,  0, 30, 'gan',     'misheard_word',        'gun',     0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  ('`+fCase+`',        '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 0,  0, 30, 'Ganema',  'misheard_proper_noun', 'Ghanima', 0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  ('`+fPunct+`',       '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 0,  0, 30, 'sietch.', 'misheard_word',        'sietch!', 0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  ('`+fGone+`',        '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 2, 60, 90, 'zzz',     'misheard_word',        'z',       0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  -- index 4 now covers 120-150 s; the judged audio (60-90 s) is chunk 2.
		  -- Both contain "Stilgar" once: the named index must NOT win.
		  ('`+fOutOfWindow+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 4, 60, 90, 'Stilgar', 'misheard_proper_noun', 'Stilgar', 0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  ('`+fPending+`',     '`+raT2+`', '/b/Other/01.m4b',                 '`+deadChunk+`', 0,  0, 30, 'word',    'misheard_word',        'ward',    0.9, 'gemma3:12b', 'proposed', NULL, NULL, NULL),
		  -- a modern finding whose anchor is current
		  ('`+fHaikuAlready+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+raC2+`', 2, 60, 90, 'rock', 'misheard_word', 'Rock', 0.9,
		   'anthropic/claude-haiku-4-5-20251001', 'proposed', '`+sha+`', `+strconv.Itoa(off)+`, 0),
		  -- a modern finding misquoting its UNCHANGED chunk: "Chani" is not in
		  -- chunk 2 (but is in chunk 4); the text did not move, so it is not moved
		  ('`+fMisquoted+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+raC2+`', 2, 60, 90, 'Chani', 'misheard_proper_noun', 'Chani', 0.9,
		   'anthropic/claude-haiku-4-5-20251001', 'proposed', '`+sha+`', 3, 0),
		  -- a current anchor whose chunk_id names no row (findings are addressed
		  -- by chunk_index first): the unchanged chunk 1 holds "the fox" twice,
		  -- and the recorded occurrence places the second — it replays, so it is
		  -- "already", never "ambiguous"
		  ('`+fStaleIDAlready+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 1, 30, 60, 'the fox', 'misheard_word', 'a fox', 0.9,
		   'anthropic/claude-haiku-4-5-20251001', 'proposed', '`+shaC1+`', 12, 1),
		  -- a human decision with a dead anchor: out of scope, never touched
		  ('`+fAccepted+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 0, 0, 30, 'ganema', 'misheard_proper_noun', 'Ghanima', 0.9,
		   'gemma3:12b', 'accepted', NULL, NULL, NULL);
		-- transcript 3: "Duncan Idaho" straddles the chunk 0/1 boundary inside
		-- the judged window; a decoy copy sits in chunk 2.
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('00000000-0000-0000-0000-00000000000d', '/b/Dune/03 God Emperor.m4b', 'c3', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
		                         segments, raw_text, model_name) VALUES
		  ('`+raT3+`', '00000000-0000-0000-0000-00000000000d', '/b/Dune/03 God Emperor.m4b',
		   'c3', 'en', 30, '[]', 'x', 'parakeet');
		INSERT INTO transcript_chunks (transcript_id, file_path, chunk_index, start_sec, end_sec,
		                               text, source_text, embedding) VALUES
		  ('`+raT3+`', '/b/Dune/03 God Emperor.m4b', 0,  0, 10, 'they met Duncan', 'they met Duncan', array_fill(0.1, ARRAY[768])::vector),
		  ('`+raT3+`', '/b/Dune/03 God Emperor.m4b', 1, 10, 20, 'Idaho at dawn.', 'Idaho at dawn.', array_fill(0.1, ARRAY[768])::vector),
		  ('`+raT3+`', '/b/Dune/03 God Emperor.m4b', 2, 20, 30, 'Later Duncan Idaho slept.', 'Later Duncan Idaho slept.', array_fill(0.1, ARRAY[768])::vector);
		INSERT INTO transcript_findings (id, transcript_id, file_path, chunk_id, chunk_index, start_sec, end_sec,
		       original_text, issue_type, suggested_correction, confidence, model) VALUES
		  -- judged occurrence straddles 0/1: no single chunk can hold it
		  ('`+fStraddle+`', '`+raT3+`', '/b/Dune/03 God Emperor.m4b', '`+deadChunk+`', 0, 5, 15, 'Duncan Idaho', 'misheard_proper_noun', 'Duncan', 0.9, 'gemma3:12b'),
		  -- window reaches chunk 2's copy too: straddler + copy = two candidates
		  ('`+fDecoy+`',    '`+raT3+`', '/b/Dune/03 God Emperor.m4b', '`+deadChunk+`', 0, 5, 25, 'Duncan Idaho', 'misheard_proper_noun', 'Duncan', 0.9, 'gemma3:12b'),
		  -- transcript 1: the named chunk 2 has one copy, chunk 4 (same window) another
		  ('`+fWindowAmbig+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 2, 60, 150, 'Stilgar', 'misheard_proper_noun', 'Stilgar', 0.9, 'gemma3:12b'),
		  -- no usable window: named chunk, then the whole transcript
		  ('`+fWindowless+`',  '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 0, 0, 0, 'Chani', 'misheard_proper_noun', 'Chani', 0.9, 'gemma3:12b');
		-- parked by an earlier run, placeable now
		INSERT INTO transcript_findings (id, transcript_id, file_path, chunk_id, chunk_index, start_sec, end_sec,
		       original_text, issue_type, suggested_correction, confidence, model, patch_state, unanchorable_reason)
		VALUES ('`+fUnanchorable+`', '`+raT1+`', '/b/Dune/02 Children of Dune.m4b', '`+deadChunk+`', 4, 120, 150,
		        'Chani', 'misheard_proper_noun', 'Chani', 0.8, 'qwen3.8', 'unanchorable', 'anchor_not_found');
	`)
	if err != nil {
		t.Fatalf("seed reanchor fixture: %v", err)
	}
}

// findingsSnapshot is every finding column the pass could write, one line per row.
func findingsSnapshot(t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.pool.Query(context.Background(), `
		SELECT concat_ws('|', id, patch_state, chunk_id, chunk_index, start_sec, end_sec,
		                 chunk_text_sha256, anchor_offset, anchor_occurrence,
		                 unanchorable_reason, reanchored_at IS NOT NULL)
		FROM transcript_findings ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

type findingAnchor struct {
	State, ChunkID, Sha, Reason string
	ChunkIndex, Offset, Occ     int
	Start, End                  float64
	Reanchored                  bool
}

func readAnchor(t *testing.T, d *DB, id string) findingAnchor {
	t.Helper()
	var a findingAnchor
	if err := d.pool.QueryRow(context.Background(), `
		SELECT patch_state, coalesce(chunk_id::text, ''), coalesce(chunk_text_sha256, ''),
		       coalesce(unanchorable_reason, ''), coalesce(chunk_index, -1),
		       coalesce(anchor_offset, -1), coalesce(anchor_occurrence, -1),
		       start_sec, end_sec, reanchored_at IS NOT NULL
		FROM transcript_findings WHERE id = $1`, id).Scan(
		&a.State, &a.ChunkID, &a.Sha, &a.Reason, &a.ChunkIndex, &a.Offset, &a.Occ,
		&a.Start, &a.End, &a.Reanchored); err != nil {
		t.Fatalf("read finding %s: %v", id, err)
	}
	return a
}

func tally(total, already, unique, moved, ambiguous, none, pending int) ReanchorTally {
	return ReanchorTally{Total: total, Already: already, Unique: unique, Moved: moved,
		Ambiguous: ambiguous, None: none, Pending: pending}
}

// TestIntegrationReanchor drives the whole pass against real Postgres: dry-run
// classifies without writing, --yes writes exactly the anchors and states the
// classification promised, a re-run is a no-op, and a re-anchored finding
// then survives accept + replay where its legacy anchor would have gone stale.
func TestIntegrationReanchor(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	seedReanchor(t, d)
	ctx := context.Background()

	wantFirst := map[string]ReanchorTally{
		// unique: ganema, "sietch." · moved: Stilgar (window), Stilgar (named
		// index now out of window), Chani (no window → whole transcript)
		// ambiguous: "the fox" (named), Stilgar (two window chunks), Duncan
		// Idaho (straddler + decoy) · none: gan, Ganema, zzz, Chani and
		// Stilgar (copies only outside the window), Duncan Idaho (lone straddler)
		"gemma3:12b":                          tally(15, 0, 2, 3, 3, 6, 1),
		"anthropic/claude-haiku-4-5-20251001": tally(3, 2, 0, 0, 0, 1, 0),
		"qwen3.8":                             tally(1, 0, 1, 0, 0, 0, 0),
	}

	// ── dry-run: classification, zero writes ──
	before := findingsSnapshot(t, d)
	rep, err := d.Reanchor(ctx, ReanchorScope{BatchSize: 1}, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	requireTallies(t, "dry-run", rep, wantFirst)
	if rep.Reanchored+rep.MarkedUnanchorable+rep.Conflicts != 0 {
		t.Errorf("dry-run reported writes: %+v", rep)
	}
	if after := findingsSnapshot(t, d); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry-run wrote to transcript_findings:\nbefore %v\nafter  %v", before, after)
	}

	// ── apply ──
	rep, err = d.Reanchor(ctx, ReanchorScope{BatchSize: 1}, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	requireTallies(t, "apply", rep, wantFirst)
	if rep.Reanchored != 6 || rep.MarkedUnanchorable != 10 || rep.Conflicts != 0 {
		t.Errorf("apply wrote reanchored=%d unanchorable=%d conflicts=%d, want 6/10/0",
			rep.Reanchored, rep.MarkedUnanchorable, rep.Conflicts)
	}

	// Every re-anchored row now carries an anchor Locate resolves to its span,
	// against the chunk's PRISTINE text, with the chunk's own identity — and
	// still its JUDGED audio window (start/end are the old chunk's, never
	// rewritten: the next re-anchor searches by them).
	for id, want := range map[string]struct {
		chunk, span string
		idx         int
		start, end  float64
	}{
		fUnique:       {raC0, "ganema", 0, 0, 30},
		fPunct:        {raC0, "sietch.", 0, 0, 30},
		fMovedWindow:  {raC2, "Stilgar", 2, 30, 90},
		fOutOfWindow:  {raC2, "Stilgar", 2, 60, 90},
		fWindowless:   {raC4, "Chani", 4, 0, 0},
		fUnanchorable: {raC4, "Chani", 4, 120, 150},
	} {
		a := readAnchor(t, d, id)
		text := raPristine[want.chunk]
		if a.State != patch.StateProposed || a.Reason != "" || !a.Reanchored {
			t.Errorf("%s: state=%s reason=%q reanchored=%v, want proposed/none/true", id, a.State, a.Reason, a.Reanchored)
		}
		if a.ChunkID != want.chunk || a.ChunkIndex != want.idx || a.Start != want.start || a.End != want.end {
			t.Errorf("%s: anchored to %s#%d [%v,%v), want %s#%d [%v,%v)", id,
				a.ChunkID, a.ChunkIndex, a.Start, a.End, want.chunk, want.idx, want.start, want.end)
		}
		if a.Sha != patch.ChunkHash(text) {
			t.Errorf("%s: chunk hash %s is not the pristine text's", id, a.Sha)
		}
		span, err := patch.Locate(text, patch.Anchor{OriginalText: want.span, Offset: a.Offset, Occurrence: a.Occ})
		if err != nil || string([]rune(text)[span.Start:span.End]) != want.span {
			t.Errorf("%s: recorded anchor (offset %d, occurrence %d) does not locate %q: %v", id, a.Offset, a.Occ, want.span, err)
		}
	}
	for id, reason := range map[string]string{
		fAmbigNamed: patch.UnanchorableAmbiguous, fWindowAmbig: patch.UnanchorableAmbiguous,
		fDecoy:       patch.UnanchorableAmbiguous,
		fOutsideWinA: patch.UnanchorableNotFound, fOutsideWinB: patch.UnanchorableNotFound,
		fStraddle: patch.UnanchorableNotFound,
		fSubword:  patch.UnanchorableNotFound, fCase: patch.UnanchorableNotFound, fGone: patch.UnanchorableNotFound,
	} {
		a := readAnchor(t, d, id)
		if a.State != patch.StateUnanchorable || a.Reason != reason || !a.Reanchored {
			t.Errorf("%s: state=%s reason=%q, want unanchorable/%s", id, a.State, a.Reason, reason)
		}
		// The original anchor is kept: it is the only record of where the judge saw it.
		if a.ChunkID != deadChunk {
			t.Errorf("%s: unanchorable row lost its original chunk_id (%s)", id, a.ChunkID)
		}
	}
	if a := readAnchor(t, d, fStaleIDAlready); a.State != patch.StateProposed || a.Reanchored ||
		a.ChunkID != deadChunk || a.Sha != patch.ChunkHash(raPristine[raC1]) || a.Occ != 1 {
		t.Errorf("stale-id current anchor was rewritten: %+v, want untouched (already)", a)
	}
	if a := readAnchor(t, d, fMisquoted); a.State != patch.StateUnanchorable ||
		a.Reason != patch.UnanchorableNotFound || a.ChunkID != raC2 {
		t.Errorf("misquoted span in an unchanged chunk: %+v, want unanchorable/anchor_not_found, still on chunk 2", a)
	}
	for _, id := range []string{fAccepted, fPending, fHaikuAlready} {
		if a := readAnchor(t, d, id); a.Reanchored {
			t.Errorf("%s: must not be written (state %s)", id, a.State)
		}
	}

	// ── idempotent: a re-run writes nothing and sees the new anchors as current ──
	snap := findingsSnapshot(t, d)
	rep, err = d.Reanchor(ctx, ReanchorScope{}, true)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if rep.Reanchored+rep.MarkedUnanchorable+rep.Conflicts != 0 {
		t.Errorf("re-run wrote: %+v", rep)
	}
	requireTallies(t, "re-run", rep, map[string]ReanchorTally{
		"gemma3:12b":                          tally(15, 5, 0, 0, 3, 6, 1),
		"anthropic/claude-haiku-4-5-20251001": tally(3, 2, 0, 0, 0, 1, 0),
		"qwen3.8":                             tally(1, 1, 0, 0, 0, 0, 0),
	})
	if after := findingsSnapshot(t, d); !reflect.DeepEqual(snap, after) {
		t.Errorf("re-run changed rows:\nbefore %v\nafter  %v", snap, after)
	}

	// ── the point of it all: a re-anchored finding survives accept + replay ──
	// Stilgar's legacy anchor named chunk 1, which does not contain it: replayed
	// as it was, it would go stale (terminal). Re-anchored, it applies.
	if err := d.SetPatchState(ctx, fMovedWindow, patch.StateProposed, patch.StateAccepted, "it"); err != nil {
		t.Fatalf("accept re-anchored finding: %v", err)
	}
	rows, _, err := d.GetCorrectionOverlay(ctx, raT1)
	if err != nil {
		t.Fatal(err)
	}
	overlay, unplaceable := BuildOverlay(rows)
	if len(unplaceable) != 0 {
		t.Fatalf("unplaceable rows: %v", unplaceable)
	}
	res := patch.Replay(raPristine[raC2], overlay[2])
	if len(res.Stale) != 0 || len(res.Applied) != 1 || res.Applied[0].ID != fMovedWindow {
		t.Errorf("re-anchored finding did not replay cleanly: applied=%v stale=%v", res.Applied, res.Stale)
	}
	for _, r := range rows {
		if r.ID == fAmbigNamed || r.ID == fGone {
			t.Errorf("unanchorable finding %s is in the overlay", r.ID)
		}
	}

	// unanchorable is never appliable — not even through the human gate.
	err = d.SetPatchState(ctx, fAmbigNamed, patch.StateUnanchorable, patch.StateAccepted, "it")
	if !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("accepting an unanchorable finding: want ErrIllegalTransition, got %v", err)
	}
}

// TestIntegrationReanchorSkipsLockedFindings: a finding locked by a concurrent
// transaction (a reviewer mid-decision) is skipped, not waited on and not
// overwritten; the next run picks it up.
func TestIntegrationReanchorSkipsLockedFindings(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	seedReanchor(t, d)
	ctx := context.Background()

	other, err := d.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx, `SELECT 1 FROM transcript_findings WHERE id = $1 FOR UPDATE`, fUnique); err != nil {
		t.Fatal(err)
	}

	rep, err := d.Reanchor(itCtx(t), ReanchorScope{}, true)
	if err != nil {
		t.Fatalf("apply with a locked row: %v", err)
	}
	if got := rep.ByModel["gemma3:12b"].Total; got != 14 {
		t.Errorf("gemma findings examined = %d, want 14 (the locked one skipped)", got)
	}
	if a := readAnchor(t, d, fUnique); a.Reanchored || a.ChunkID != deadChunk {
		t.Errorf("locked finding was written: %+v", a)
	}
	if err := other.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	rep, err = d.Reanchor(itCtx(t), ReanchorScope{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reanchored != 1 {
		t.Errorf("second run re-anchored %d, want just the previously locked finding", rep.Reanchored)
	}
	if a := readAnchor(t, d, fUnique); !a.Reanchored || a.ChunkID != raC0 {
		t.Errorf("previously locked finding not re-anchored: %+v", a)
	}
}

// TestIntegrationReanchorScope: --book and --limit narrow the run.
func TestIntegrationReanchorScope(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	seedReanchor(t, d)
	ctx := context.Background()

	rep, err := d.Reanchor(ctx, ReanchorScope{Book: "other"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := rep.Sum(); s.Total != 1 || s.Pending != 1 {
		t.Errorf("--book other: %+v, want just the pending finding", s)
	}
	rep, err = d.Reanchor(ctx, ReanchorScope{Limit: 3, BatchSize: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := rep.Sum(); s.Total != 3 {
		t.Errorf("--limit 3 examined %d", s.Total)
	}
}

// TestIntegrationReanchorSurvivalSQLMatchesGo pins the server-side survival
// query (testdata/reanchor_survival.sql, used to measure the live library
// without pulling chunk text out of Postgres) to the Go matcher: on the same
// fixture both must count every outcome identically.
func TestIntegrationReanchorSurvivalSQLMatchesGo(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	seedReanchor(t, d)
	ctx := context.Background()

	src, err := os.ReadFile("testdata/reanchor_survival.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ReplaceAll(string(src), "__FILTER__", "true")
	rows, err := d.pool.Query(ctx, sql)
	if err != nil {
		t.Fatalf("survival SQL: %v", err)
	}
	defer rows.Close()
	fromSQL := map[string]*ReanchorTally{}
	for rows.Next() {
		var model, outcome string
		var n int
		if err := rows.Scan(&model, &outcome, &n); err != nil {
			t.Fatal(err)
		}
		if fromSQL[model] == nil {
			fromSQL[model] = &ReanchorTally{}
		}
		for range n {
			fromSQL[model].add(outcome)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	goRep, err := d.Reanchor(ctx, ReanchorScope{}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ReanchorTally{}
	for m, tl := range goRep.ByModel {
		want[m] = *tl
	}
	requireTallies(t, "survival SQL", ReanchorReport{ByModel: fromSQL}, want)
}

func requireTallies(t *testing.T, label string, rep ReanchorReport, want map[string]ReanchorTally) {
	t.Helper()
	got := map[string]ReanchorTally{}
	for m, tl := range rep.ByModel {
		got[m] = *tl
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s tallies:\n got %+v\nwant %+v", label, got, want)
	}
}

// TestIntegrationReanchorWriteRefusesRebuiltChunk pins the anchor write's
// in-SQL hash check: an anchor computed against a chunk that is rebuilt before
// the write lands matches zero rows (a conflict), and the finding is left for
// the next run rather than anchored to text nobody verified.
func TestIntegrationReanchorWriteRefusesRebuiltChunk(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	seedReanchor(t, d)
	ctx := context.Background()

	// The anchor as the pass would compute it from the text it read…
	f := patch.ReanchorFinding{OriginalText: "ganema", ChunkID: deadChunk, ChunkIndex: &[]int{0}[0],
		StartSec: 0, EndSec: 30, Offset: -1, Occurrence: -1}
	res := patch.Reanchor(f, []patch.ReanchorChunk{{ID: raC0, Index: 0, StartSec: 0, EndSec: 30, Text: raPristine[raC0]}})
	if res.Outcome != patch.OutcomeUnique {
		t.Fatalf("precondition: %+v", res)
	}
	// …then the worker rebuilds the chunk before the write.
	if _, err := d.pool.Exec(ctx, `UPDATE transcript_chunks SET source_text = source_text || ' More.' WHERE id = $1`, raC0); err != nil {
		t.Fatal(err)
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rep := ReanchorReport{ByModel: map[string]*ReanchorTally{}}
	row := reanchorRow{ID: fUnique, TranscriptID: raT1, Model: "gemma3:12b", State: patch.StateProposed, Finding: f}
	if err := writeReanchor(ctx, tx, &rep, row, res); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rep.Conflicts != 1 || rep.Reanchored != 0 {
		t.Errorf("write against a rebuilt chunk: conflicts=%d reanchored=%d, want 1/0", rep.Conflicts, rep.Reanchored)
	}
	if a := readAnchor(t, d, fUnique); a.Reanchored || a.ChunkID != deadChunk || a.Sha != "" {
		t.Errorf("finding was anchored to text nobody verified: %+v", a)
	}
}
