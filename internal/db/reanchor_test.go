package db

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

// TestReanchorStates_OnlyTheReviewQueue: the pass may only rewrite findings no
// human has decided. Every state it reads must be proposed or unanchorable.
func TestReanchorStates_OnlyTheReviewQueue(t *testing.T) {
	want := []string{patch.StateProposed, patch.StateUnanchorable}
	if !slices.Equal(reanchorStates, want) {
		t.Errorf("reanchorStates = %v, want %v", reanchorStates, want)
	}
	// And every move between them is legal in the state machine.
	if !patch.CanTransition(patch.StateProposed, patch.StateUnanchorable) ||
		!patch.CanTransition(patch.StateUnanchorable, patch.StateProposed) {
		t.Error("proposed <-> unanchorable must both be legal")
	}
}

// TestReanchorSQL_LocksAndGuards pins the concurrency contract at the source
// level: apply-mode reads lock (findings FOR UPDATE SKIP LOCKED, chunks
// FOR SHARE), dry-run reads lock nothing, both read PRISTINE chunk text, and
// both writes are compare-and-swaps on the state the row was read in — the
// anchor write additionally re-checking the chunk hash in SQL.
func TestReanchorSQL_LocksAndGuards(t *testing.T) {
	if !strings.Contains(reanchorFindingsLockSQL, "FOR UPDATE SKIP LOCKED") {
		t.Errorf("apply-mode findings read must lock FOR UPDATE SKIP LOCKED:\n%s", reanchorFindingsLockSQL)
	}
	if !strings.Contains(reanchorChunksLockSQL, "FOR SHARE") {
		t.Errorf("apply-mode chunk read must lock FOR SHARE:\n%s", reanchorChunksLockSQL)
	}
	for name, sql := range map[string]string{
		"reanchorFindingsSQL": reanchorFindingsSQL, "reanchorChunksSQL": reanchorChunksSQL,
		"reanchorTranscriptsSQL": reanchorTranscriptsSQL,
	} {
		if strings.Contains(sql, "FOR ") {
			t.Errorf("%s is the dry-run read and must not lock:\n%s", name, sql)
		}
	}
	for name, sql := range map[string]string{"reanchorChunksSQL": reanchorChunksSQL, "reanchorWriteSQL": reanchorWriteSQL} {
		if !strings.Contains(sql, "COALESCE(source_text, text)") && !strings.Contains(sql, "COALESCE(c.source_text, c.text)") {
			t.Errorf("%s must use the pristine text COALESCE(source_text, text):\n%s", name, sql)
		}
	}
	if !strings.Contains(reanchorWriteSQL, "f.patch_state = $2") ||
		!strings.Contains(reanchorWriteSQL, "'hex') = $4") {
		t.Errorf("reanchorWriteSQL must CAS on the read state and re-check the chunk hash:\n%s", reanchorWriteSQL)
	}
	if !strings.Contains(markUnanchorableSQL, "WHERE id = $1 AND patch_state = $2") {
		t.Errorf("markUnanchorableSQL must CAS on the read state:\n%s", markUnanchorableSQL)
	}
	// The pass never stamps a human decision, never flags a chunk, never edits
	// chunk text: it is not a review.
	for name, sql := range map[string]string{"reanchorWriteSQL": reanchorWriteSQL, "markUnanchorableSQL": markUnanchorableSQL} {
		for _, banned := range []string{"decided_at", "decided_by", "embedding_stale", "UPDATE transcript_chunks"} {
			if strings.Contains(sql, banned) {
				t.Errorf("%s must not touch %s:\n%s", name, banned, sql)
			}
		}
	}
}

// findingRow builds one mocked reanchorFindings row.
func findingRow(rows *pgxmock.Rows, id, model, state, span string, chunkIdx int, start, end float64, reason string) *pgxmock.Rows {
	dead := "dead"
	return rows.AddRow(id, "t1", model, state, span, &dead, &chunkIdx, start, end,
		(*string)(nil), (*int)(nil), (*int)(nil), reason)
}

var findingCols = []string{"id", "transcript_id", "model", "patch_state", "original_text",
	"chunk_id", "chunk_index", "start_sec", "end_sec", "chunk_text_sha256",
	"anchor_offset", "anchor_occurrence", "reason"}

// expectReanchorBatch queues one batch's reads. lock picks the apply-mode SQL.
func expectReanchorBatch(mock pgxmock.PgxPoolIface, lock bool) {
	findingsSQL, chunksSQL := reanchorFindingsSQL, reanchorChunksSQL
	if lock {
		findingsSQL, chunksSQL = reanchorFindingsLockSQL, reanchorChunksLockSQL
	}
	mock.ExpectBegin()
	mock.ExpectQuery(reanchorTranscriptsSQL).
		WithArgs(reanchorStates, (*string)(nil), "%%", DefaultReanchorBatch).
		WillReturnRows(pgxmock.NewRows([]string{"transcript_id"}).AddRow("t1"))

	rows := pgxmock.NewRows(findingCols)
	findingRow(rows, "f-unique", "gemma3:12b", patch.StateProposed, "ganema", 0, 0, 30, "")
	findingRow(rows, "f-moved", "gemma3:12b", patch.StateProposed, "Chani", 0, 0, 90, "")
	findingRow(rows, "f-ambig", "gemma3:12b", patch.StateProposed, "the fox", 1, 30, 60, "")
	findingRow(rows, "f-none", "qwen3.8", patch.StateProposed, "zzz", 0, 0, 30, "")
	findingRow(rows, "f-parked", "qwen3.8", patch.StateUnanchorable, "zzz", 0, 0, 30, patch.UnanchorableNotFound)
	mock.ExpectQuery(findingsSQL).
		WithArgs([]string{"t1"}, reanchorStates, "%%", nil).
		WillReturnRows(rows)

	mock.ExpectQuery(chunksSQL).
		WithArgs([]string{"t1"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "transcript_id", "chunk_index", "start_sec", "end_sec", "text"}).
			AddRow("c0", "t1", 0, 0.0, 30.0, "Leto and ganema walked.").
			AddRow("c1", "t1", 1, 30.0, 60.0, "the fox and the fox.").
			AddRow("c2", "t1", 2, 60.0, 90.0, "Then Chani arrived."))
}

var wantMockTallies = map[string]ReanchorTally{
	"gemma3:12b": {Total: 3, Unique: 1, Moved: 1, Ambiguous: 1},
	"qwen3.8":    {Total: 2, None: 2},
}

// TestReanchor_DryRunReadsOnlyAndRollsBack: without --yes the pass classifies,
// takes no locks, issues no write, and never commits.
func TestReanchor_DryRunReadsOnlyAndRollsBack(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	expectReanchorBatch(mock, false)
	mock.ExpectRollback()

	rep, err := reanchor(context.Background(), mock, ReanchorScope{}, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	requireTallies(t, "dry-run", rep, wantMockTallies)
	if rep.Reanchored+rep.MarkedUnanchorable+rep.Conflicts != 0 {
		t.Errorf("dry-run counted writes: %+v", rep)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("dry-run must only read, then roll back: %v", err)
	}
}

// TestReanchor_YesLocksWritesAndCommits: with --yes the same batch is read under
// locks, each outcome is written with a guarded UPDATE (an unanchorable row
// already parked for the same reason is left alone), and the batch commits.
// A guarded UPDATE that matches nothing is a conflict, not an error.
func TestReanchor_YesLocksWritesAndCommits(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	expectReanchorBatch(mock, true)
	mock.ExpectExec(reanchorWriteSQL).
		WithArgs("f-unique", patch.StateProposed, "c0", patch.ChunkHash("Leto and ganema walked."),
			9, 0, patch.StateProposed).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(reanchorWriteSQL).
		WithArgs("f-moved", patch.StateProposed, "c2", patch.ChunkHash("Then Chani arrived."),
									5, 0, patch.StateProposed).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0)) // rebuilt or decided mid-run
	mock.ExpectExec(markUnanchorableSQL).
		WithArgs("f-ambig", patch.StateProposed, patch.StateUnanchorable, patch.UnanchorableAmbiguous).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(markUnanchorableSQL).
		WithArgs("f-none", patch.StateProposed, patch.StateUnanchorable, patch.UnanchorableNotFound).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// f-parked: already unanchorable/anchor_not_found → no write at all.
	mock.ExpectCommit()
	mock.ExpectRollback() // the deferred rollback after commit is a no-op

	rep, err := reanchor(context.Background(), mock, ReanchorScope{}, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	requireTallies(t, "apply", rep, wantMockTallies)
	if rep.Reanchored != 1 || rep.Conflicts != 1 || rep.MarkedUnanchorable != 2 {
		t.Errorf("writes: reanchored=%d conflicts=%d unanchorable=%d, want 1/1/2",
			rep.Reanchored, rep.Conflicts, rep.MarkedUnanchorable)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestReanchor_LimitStopsTheWalk: --limit bounds the findings read, and the walk
// stops once it is spent instead of opening another batch.
func TestReanchor_LimitStopsTheWalk(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(reanchorTranscriptsSQL).
		WithArgs(reanchorStates, (*string)(nil), "%%", 1).
		WillReturnRows(pgxmock.NewRows([]string{"transcript_id"}).AddRow("t1"))
	rows := pgxmock.NewRows(findingCols)
	findingRow(rows, "f1", "gemma3:12b", patch.StateProposed, "zzz", 0, 0, 30, "")
	mock.ExpectQuery(reanchorFindingsSQL).
		WithArgs([]string{"t1"}, reanchorStates, "%%", 1).
		WillReturnRows(rows)
	mock.ExpectQuery(reanchorChunksSQL).WithArgs([]string{"t1"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "transcript_id", "chunk_index", "start_sec", "end_sec", "text"}).
			AddRow("c0", "t1", 0, 0.0, 30.0, "text"))
	mock.ExpectRollback()

	rep, err := reanchor(context.Background(), mock, ReanchorScope{Limit: 1, BatchSize: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := rep.Sum(); s.Total != 1 {
		t.Errorf("examined %d, want 1", s.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a spent limit must not open another batch: %v", err)
	}
}
