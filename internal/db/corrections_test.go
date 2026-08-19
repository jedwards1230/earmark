package db

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/jedwards1230/earmark/internal/patch"
)

// correctionSQL is the set of statements the correction-overlay path issues.
// Kept SEPARATE from findings_test.go's evalSQL map on purpose: the eval layer
// is read/insert-only (§2.15) and these statements legitimately UPDATE, so
// mixing them would weaken that guard.
var correctionSQL = map[string]string{
	"correctionOverlaySQL":        correctionOverlaySQL,
	"markFindingsAppliedSQL":      markFindingsAppliedSQL,
	"markFindingsStaleSQL":        markFindingsStaleSQL,
	"setPatchStateSQL":            setPatchStateSQL,
	"setPatchStateDecidedSQL":     setPatchStateDecidedSQL,
	"markChunkStaleForFindingSQL": markChunkStaleForFindingSQL,
}

// TestCorrectionSQL_NeverTouchesTranscriptProvenance is the hard invariant of
// the whole design: transcripts.segments and transcripts.raw_text are the
// immutable ASR record. Corrections live in transcript_findings and are
// replayed onto the disposable transcript_chunks projection — no statement in
// this path may write the transcripts table at all.
func TestCorrectionSQL_NeverTouchesTranscriptProvenance(t *testing.T) {
	for name, sql := range correctionSQL {
		upper := strings.ToUpper(sql)
		for _, banned := range []string{
			"UPDATE TRANSCRIPTS", "DELETE FROM TRANSCRIPTS", "INSERT INTO TRANSCRIPTS",
			"ALTER TABLE TRANSCRIPTS", "SEGMENTS", "RAW_TEXT",
		} {
			if strings.Contains(upper, banned) {
				t.Errorf("%s must not touch transcript provenance (%q):\n%s", name, banned, sql)
			}
		}
		for _, verb := range []string{"DELETE ", "DROP ", "TRUNCATE ", "ALTER "} {
			if strings.Contains(upper, verb) {
				t.Errorf("%s contains destructive verb %q — a stale finding is retired, "+
					"never deleted:\n%s", name, verb, sql)
			}
		}
	}
}

// TestCorrectionOverlaySQL_ReadOnlyAndDeterministic pins the overlay read: it
// selects only, filters on the patch state, and orders deterministically so a
// rebuild sees the same rows in the same order every time.
func TestCorrectionOverlaySQL_ReadOnlyAndDeterministic(t *testing.T) {
	if !strings.HasPrefix(strings.TrimSpace(strings.ToUpper(correctionOverlaySQL)), "SELECT") {
		t.Errorf("correctionOverlaySQL must start with SELECT:\n%s", correctionOverlaySQL)
	}
	for _, verb := range []string{"UPDATE ", "INSERT ", "DELETE "} {
		if strings.Contains(strings.ToUpper(correctionOverlaySQL), verb) {
			t.Errorf("correctionOverlaySQL must be read-only, found %q:\n%s", verb, correctionOverlaySQL)
		}
	}
	if !strings.Contains(correctionOverlaySQL, "ORDER BY chunk_index, anchor_offset NULLS LAST, id") {
		t.Errorf("correctionOverlaySQL must order deterministically:\n%s", correctionOverlaySQL)
	}
	// The state filter is a bound parameter, not an inline literal, so it can
	// only ever be the constants below.
	if !strings.Contains(correctionOverlaySQL, "patch_state = ANY($2)") {
		t.Errorf("correctionOverlaySQL must bind the state list:\n%s", correctionOverlaySQL)
	}
}

// TestOverlayStateSetsDeriveFromTheStateMachine guards the thing that would
// rot silently: the SQL guards must be derived from patch.CanTransition, not a
// second hand-written copy of the state machine that drifts out of agreement.
func TestOverlayStateSetsDeriveFromTheStateMachine(t *testing.T) {
	// Only human-approved corrections are in the projection. A `proposed`
	// finding must NEVER appear in the overlay — that is the human gate.
	for _, s := range overlayStates {
		if s != patch.StateAccepted && s != patch.StateApplied {
			t.Errorf("overlayStates contains %q — only accepted/applied corrections may be replayed", s)
		}
	}
	if len(overlayStates) != 2 {
		t.Errorf("overlayStates must be exactly {accepted, applied}, got %v", overlayStates)
	}

	// Promotion to `applied` may only come from a state the machine allows,
	// plus `applied` itself (idempotent re-mark on every rebuild).
	for _, s := range appliedFromStates {
		if s == patch.StateApplied {
			continue
		}
		if !patch.CanTransition(s, patch.StateApplied) {
			t.Errorf("appliedFromStates allows %q -> applied, which CanTransition forbids", s)
		}
	}
	for _, s := range []string{patch.StateProposed, patch.StateRejected, patch.StateStale, patch.StateReverted} {
		for _, allowed := range appliedFromStates {
			if s == allowed {
				t.Errorf("%q must not be promotable straight to applied — that skips the human gate", s)
			}
		}
	}

	// Retiring to `stale` may only come from a state the machine allows;
	// `stale` is terminal so it must not be in the list.
	for _, s := range staleFromStates {
		if !patch.CanTransition(s, patch.StateStale) {
			t.Errorf("staleFromStates allows %q -> stale, which CanTransition forbids", s)
		}
		if s == patch.StateStale {
			t.Error("staleFromStates includes stale — the state is terminal")
		}
	}
	if len(staleFromStates) == 0 {
		t.Error("staleFromStates is empty — nothing could ever be retired")
	}
}

// TestSetPatchStateSQL_IsGuardedCompareAndSwap: the UPDATE must be conditional
// on the expected current state, or two reviewers deciding at once would let
// the second silently clobber the first.
func TestSetPatchStateSQL_IsGuardedCompareAndSwap(t *testing.T) {
	for name, sql := range map[string]string{
		"setPatchStateSQL":        setPatchStateSQL,
		"setPatchStateDecidedSQL": setPatchStateDecidedSQL,
	} {
		if !strings.Contains(sql, "WHERE id = $1 AND patch_state = $2") {
			t.Errorf("%s must guard on the expected current state:\n%s", name, sql)
		}
	}
	// Only the human-decision variant records who decided.
	if strings.Contains(setPatchStateSQL, "decided_by") {
		t.Errorf("setPatchStateSQL must not stamp decided_by — machine transitions "+
			"must not overwrite the human record:\n%s", setPatchStateSQL)
	}
	if !strings.Contains(setPatchStateDecidedSQL, "decided_at") ||
		!strings.Contains(setPatchStateDecidedSQL, "decided_by") {
		t.Errorf("setPatchStateDecidedSQL must record decided_at/decided_by:\n%s", setPatchStateDecidedSQL)
	}
}

// TestEvalChunkSelectSQL_FeedsTheJudgePristineText is the guard on the design
// fork: the judge records anchors + chunk_text_sha256 against the text it is
// shown, and replay always starts from the pristine regenerated text. If this
// SELECT ever returns c.text (the corrected projection), every finding the
// judge produces is stale the moment it is written.
func TestEvalChunkSelectSQL_FeedsTheJudgePristineText(t *testing.T) {
	if !strings.Contains(evalChunkSelectSQL, "COALESCE(c.source_text, c.text)") {
		t.Errorf("evalChunkSelectSQL must select the pristine chunk text "+
			"COALESCE(c.source_text, c.text) — the judge must never see corrected text:\n%s",
			evalChunkSelectSQL)
	}
	// A bare `c.text` in the select list would mean the corrected surface leaked
	// back into the judge path.
	for _, line := range strings.Split(evalChunkSelectSQL, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "FROM") {
			break
		}
		if strings.Contains(trimmed, "c.text") && !strings.Contains(trimmed, "COALESCE") {
			t.Errorf("evalChunkSelectSQL selects bare c.text:\n%s", evalChunkSelectSQL)
		}
	}
}

// TestInsertChunksWritesSourceTextAndClearsStale pins the projection write:
// source_text is persisted alongside the corrected text, and rebuilding a chunk
// clears embedding_stale in the same statement (so a rebuild can't forget it).
func TestInsertChunksWritesSourceTextAndClearsStale(t *testing.T) {
	raw, err := os.ReadFile("db.go")
	if err != nil {
		t.Fatalf("read db.go: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "func (db *DB) InsertChunks(")
	if start < 0 {
		t.Fatal("InsertChunks not found — this test needs updating")
	}
	body := src[start : start+3000]

	for _, want := range []string{
		"source_text",
		"embedding_stale",
		"source_text     = EXCLUDED.source_text",
		"embedding_stale = false",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("InsertChunks must contain %q so a rebuilt chunk carries its "+
				"pristine text and is no longer stale", want)
		}
	}
}

func TestBuildOverlay(t *testing.T) {
	idx0, idx1 := 0, 1
	off, occ := 4, 0
	hash := patch.ChunkHash("the auto sebo dish")
	correction := "arecibo"

	tests := []struct {
		name            string
		rows            []CorrectionRow
		wantChunks      map[int]int // chunk index -> patch count
		wantUnplaceable []string
		check           func(t *testing.T, o patch.Overlay)
	}{
		{
			name: "empty input",
			rows: nil,
		},
		{
			name: "groups by chunk index",
			rows: []CorrectionRow{
				{ID: "a", ChunkIndex: &idx0, OriginalText: "auto sebo", SuggestedCorrection: &correction,
					AnchorOffset: &off, AnchorOccurrence: &occ, ChunkTextSHA256: &hash},
				{ID: "b", ChunkIndex: &idx0, OriginalText: "dish", SuggestedCorrection: &correction},
				{ID: "c", ChunkIndex: &idx1, OriginalText: "fox", SuggestedCorrection: &correction},
			},
			wantChunks: map[int]int{0: 2, 1: 1},
			check: func(t *testing.T, o patch.Overlay) {
				got := o[0][0]
				if got.Anchor.Offset != 4 || got.Anchor.Occurrence != 0 {
					t.Errorf("anchor not carried through: %+v", got.Anchor)
				}
				if got.ChunkHash != hash {
					t.Errorf("chunk hash not carried through: %q", got.ChunkHash)
				}
			},
		},
		{
			// NULL anchor columns must become patch's "unknown" sentinel (-1),
			// NOT 0 — a zero offset is a real position and would make Locate
			// trust an anchor the judge never reported.
			name: "null anchors become unknown, not zero",
			rows: []CorrectionRow{
				{ID: "legacy", ChunkIndex: &idx0, OriginalText: "fox", SuggestedCorrection: &correction},
			},
			wantChunks: map[int]int{0: 1},
			check: func(t *testing.T, o patch.Overlay) {
				a := o[0][0].Anchor
				if a.Offset != -1 || a.Occurrence != -1 {
					t.Errorf("want unknown (-1) anchors for a legacy row, got %+v", a)
				}
				if o[0][0].ChunkHash != "" {
					t.Errorf("want empty hash for a legacy row, got %q", o[0][0].ChunkHash)
				}
			},
		},
		{
			// A NULL correction is carried through (as "") so Replay quarantines
			// it with an explicit empty_correction reason rather than it silently
			// vanishing from the overlay.
			name: "null correction is carried, not dropped",
			rows: []CorrectionRow{
				{ID: "blank", ChunkIndex: &idx0, OriginalText: "fox"},
			},
			wantChunks: map[int]int{0: 1},
			check: func(t *testing.T, o patch.Overlay) {
				if o[0][0].Correction != "" {
					t.Errorf("want empty correction, got %q", o[0][0].Correction)
				}
			},
		},
		{
			name: "null chunk index is reported as unplaceable",
			rows: []CorrectionRow{
				{ID: "orphan", OriginalText: "fox", SuggestedCorrection: &correction},
				{ID: "ok", ChunkIndex: &idx0, OriginalText: "fox", SuggestedCorrection: &correction},
			},
			wantChunks:      map[int]int{0: 1},
			wantUnplaceable: []string{"orphan"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, unplaceable := BuildOverlay(tc.rows)

			if len(o) != len(tc.wantChunks) {
				t.Fatalf("overlay has %d chunk(s), want %d: %+v", len(o), len(tc.wantChunks), o)
			}
			for idx, n := range tc.wantChunks {
				if got := len(o[idx]); got != n {
					t.Errorf("chunk %d: want %d patches, got %d", idx, n, got)
				}
			}
			if strings.Join(unplaceable, ",") != strings.Join(tc.wantUnplaceable, ",") {
				t.Errorf("unplaceable: want %v, got %v", tc.wantUnplaceable, unplaceable)
			}
			if tc.check != nil {
				tc.check(t, o)
			}
		})
	}
}

// TestGetCorrectionOverlay_ScansRows drives the real query + scan path against a
// mock pool (no live Postgres): the bound state filter and every scanned column.
func TestGetCorrectionOverlay_ScansRows(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	idx := 3
	off, occ := 12, 1
	hash := "deadbeef"
	correction := "arecibo"

	rows := pgxmock.NewRows([]string{
		"id", "chunk_index", "original_text", "suggested_correction",
		"anchor_offset", "anchor_occurrence", "chunk_text_sha256",
	}).
		AddRow("f1", &idx, "auto sebo", &correction, &off, &occ, &hash).
		AddRow("f2", (*int)(nil), "fox", (*string)(nil), (*int)(nil), (*int)(nil), (*string)(nil))

	mock.ExpectQuery("FROM transcript_findings").
		WithArgs("t-1", overlayStates).
		WillReturnRows(rows)

	got, err := database.getCorrectionOverlay(context.Background(), mock, "t-1")
	if err != nil {
		t.Fatalf("getCorrectionOverlay: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].ID != "f1" || got[0].ChunkIndex == nil || *got[0].ChunkIndex != 3 {
		t.Errorf("row 0 mis-scanned: %+v", got[0])
	}
	if got[0].AnchorOffset == nil || *got[0].AnchorOffset != 12 {
		t.Errorf("anchor offset mis-scanned: %+v", got[0])
	}
	if got[1].ChunkIndex != nil || got[1].SuggestedCorrection != nil {
		t.Errorf("row 1 should carry NULLs as nil: %+v", got[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSetPatchState_RejectsIllegalTransitions is the human gate at the DB
// boundary: an illegal move must be refused BEFORE any SQL runs, so a UI bug
// cannot write a state nobody approved. Notably proposed → applied.
func TestSetPatchState_RejectsIllegalTransitions(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	illegal := []struct{ from, to string }{
		{patch.StateProposed, patch.StateApplied},
		{patch.StateRejected, patch.StateApplied},
		{patch.StateStale, patch.StateProposed},
		{patch.StateApplied, patch.StateAccepted},
		{"nonsense", patch.StateAccepted},
	}
	for _, tc := range illegal {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			err := database.setPatchState(context.Background(), mock, "f1", tc.from, tc.to, "justin")
			if !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("want ErrIllegalTransition, got %v", err)
			}
		})
	}
	// No transaction may have been opened for any of them.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("illegal transitions must not touch the database: %v", err)
	}
}

// TestSetPatchState_AcceptFlagsChunkStale: accepting a correction changes the
// overlay, so the chunk's projection is out of date and must be re-embedded.
// Recorded in ONE transaction with the decision.
func TestSetPatchState_AcceptFlagsChunkStale(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE transcript_findings").
		WithArgs("f1", patch.StateProposed, patch.StateAccepted, "justin").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("UPDATE transcript_chunks").
		WithArgs("f1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := database.setPatchState(context.Background(), mock,
		"f1", patch.StateProposed, patch.StateAccepted, "justin"); err != nil {
		t.Fatalf("setPatchState: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSetPatchState_ConflictWhenRowMoved: the guarded UPDATE matching zero rows
// means somebody else decided first. The caller must learn that rather than
// believing it won.
func TestSetPatchState_ConflictWhenRowMoved(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE transcript_findings").
		WithArgs("f1", patch.StateProposed, patch.StateRejected, "justin").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	err = database.setPatchState(context.Background(), mock,
		"f1", patch.StateProposed, patch.StateRejected, "justin")
	if !errors.Is(err, ErrPatchStateConflict) {
		t.Fatalf("want ErrPatchStateConflict, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSetPatchState_RejectDoesNotFlagChunk: a rejection removes nothing from
// the projection (a proposed finding was never in it), so it must not schedule
// a pointless re-embed.
func TestSetPatchState_RejectDoesNotFlagChunk(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE transcript_findings").
		WithArgs("f1", patch.StateProposed, patch.StateRejected, "justin").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := database.setPatchState(context.Background(), mock,
		"f1", patch.StateProposed, patch.StateRejected, "justin"); err != nil {
		t.Fatalf("setPatchState: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestMarkFindingsStale_NoopOnEmpty / writes the reason with the derived guard.
func TestMarkFindingsStale(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	// Empty id list must not issue SQL at all.
	if err := database.markFindingsStale(context.Background(), mock, nil, patch.StaleReasonChunkChanged); err != nil {
		t.Fatalf("empty markFindingsStale: %v", err)
	}

	ids := []string{"f1", "f2"}
	mock.ExpectExec("UPDATE transcript_findings").
		WithArgs(ids, patch.StaleReasonAnchorNotFound, staleFromStates).
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))

	if err := database.markFindingsStale(context.Background(), mock, ids, patch.StaleReasonAnchorNotFound); err != nil {
		t.Fatalf("markFindingsStale: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestMarkFindingsApplied_WritesSpanLevelAudit pins the narrowed semantics of
// applied_before_text / applied_after_text: they carry the SPAN, supplied by the
// caller, one guarded UPDATE per finding inside one transaction.
func TestMarkFindingsApplied_WritesSpanLevelAudit(t *testing.T) {
	database := newTestDB()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	if err := database.markFindingsApplied(context.Background(), mock, nil); err != nil {
		t.Fatalf("empty markFindingsApplied: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE transcript_findings").
		WithArgs("f1", "auto sebo", "arecibo", appliedFromStates).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("UPDATE transcript_findings").
		WithArgs("f2", "pin name", "pen name", appliedFromStates).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	err = database.markFindingsApplied(context.Background(), mock, []AppliedFinding{
		{ID: "f1", Before: "auto sebo", After: "arecibo"},
		{ID: "f2", Before: "pin name", After: "pen name"},
	})
	if err != nil {
		t.Fatalf("markFindingsApplied: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
