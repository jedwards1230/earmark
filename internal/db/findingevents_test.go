package db

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

var (
	testRecipe = strings.Repeat("ab", 32)
	jevDecider = "jev:" + testRecipe
)

// TestSetPatchStateBulk_RejectsIllegalTransitions is the bulk twin of
// TestSetPatchState_RejectsIllegalTransitions: every refusal happens before
// a transaction is opened.
func TestSetPatchStateBulk_RejectsIllegalTransitions(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	cases := []struct{ name, from, to, by string }{
		{"proposed->applied skips the gate", patch.StateProposed, patch.StateApplied, jevDecider},
		{"accepted->applied is the rebuild's", patch.StateAccepted, patch.StateApplied, jevDecider},
		{"rejected->applied", patch.StateRejected, patch.StateApplied, jevDecider},
		{"stale is terminal", patch.StateStale, patch.StateProposed, jevDecider},
		{"applied->accepted", patch.StateApplied, patch.StateAccepted, jevDecider},
		{"unknown state", "nonsense", patch.StateAccepted, jevDecider},
		{"proposed->stale is a maintenance target", patch.StateProposed, patch.StateStale, jevDecider},
		{"reanchor move", patch.StateProposed, patch.StateUnanchorable, jevDecider},
		{"reanchor move back", patch.StateUnanchorable, patch.StateProposed, jevDecider},
		{"requeue archive", patch.StateProposed, patch.StateSuperseded, jevDecider},
		{"empty decider", patch.StateProposed, patch.StateAccepted, ""},
		{"human decider", patch.StateProposed, patch.StateAccepted, "justin"},
		{"mcp decider", patch.StateProposed, patch.StateAccepted, "mcp:jev:" + testRecipe},
		{"short recipe", patch.StateProposed, patch.StateAccepted, "jev:abc"},
		{"uppercase recipe", patch.StateProposed, patch.StateAccepted, "jev:" + strings.ToUpper(testRecipe)},
		{"trailing junk", patch.StateProposed, patch.StateAccepted, jevDecider + " "},
		{"unknown prefix", patch.StateProposed, patch.StateAccepted, "undo:jev:" + testRecipe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := setPatchStateBulk(context.Background(), mock, tc.from, tc.to, tc.by, []string{"f1"})
			if !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("want ErrIllegalTransition, got %v", err)
			}
			// The tx variant refuses too, so no caller can route around it.
			if _, err := setPatchStateBulkTx(context.Background(), mock, tc.from, tc.to, tc.by, []string{"f1"}); !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("tx variant: want ErrIllegalTransition, got %v", err)
			}
		})
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("illegal bulk transitions must not touch the database: %v", err)
	}
}

// TestSetPatchStateBulk_LegalMovesAreDecisions: every legal bulk target is a
// stamped decision, so the trigger attributes it to decided_by.
func TestSetPatchStateBulk_LegalMovesAreDecisions(t *testing.T) {
	for _, tc := range []struct{ from, to string }{
		{patch.StateProposed, patch.StateAccepted},
		{patch.StateProposed, patch.StateRejected},
		{patch.StateAccepted, patch.StateRejected},
		{patch.StateApplied, patch.StateReverted},
		{patch.StateRejected, patch.StateProposed},
		{patch.StateReverted, patch.StateProposed},
	} {
		if err := validateBulk(tc.from, tc.to, "revert:"+jevDecider); err != nil {
			t.Errorf("%s -> %s: %v", tc.from, tc.to, err)
		}
	}
}

// TestBulkSQL_IsGuardedCompareAndSwap pins the statement's shape.
func TestBulkSQL_IsGuardedCompareAndSwap(t *testing.T) {
	sql := norm(setPatchStateBulkSQL)
	for _, want := range []string{
		// Candidates locked in one global order — no bulk/bulk deadlock.
		"WHERE id = ANY($1) AND patch_state = $2 ORDER BY id FOR UPDATE",
		// The compare-and-swap itself.
		"WHERE f.id = lk.id AND f.patch_state = $2",
		// A stamped decision, on the statement's clock (the watermark guard).
		"decided_at = clock_timestamp()",
		"decided_by = $4",
		// Chunk invalidation, gated and addressed like markChunkStaleForFindingSQL.
		"SET embedding_stale = true",
		"WHERE $5",
		"c.transcript_id = f.transcript_id AND c.chunk_index = f.chunk_index",
		"(f.chunk_index IS NULL AND c.id = f.chunk_id)",
		"SELECT id::text FROM upd",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("setPatchStateBulkSQL is missing %q:\n%s", want, setPatchStateBulkSQL)
		}
	}
	if strings.Contains(sql, "'applied'") || strings.Contains(sql, "now()") {
		t.Errorf("setPatchStateBulkSQL must not name applied or use now():\n%s", setPatchStateBulkSQL)
	}
}

// TestSetPatchStateBulk_ReportsChangedAndSkipped: ids are deduplicated and
// sorted before the statement; whatever it did not change is Skipped; the
// chunk flag is on for accept.
func TestSetPatchStateBulk_ReportsChangedAndSkipped(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("WITH lk AS").
		WithArgs([]string{"a", "b", "c"}, patch.StateProposed, patch.StateAccepted, jevDecider, true).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("c").AddRow("a"))
	mock.ExpectCommit()

	res, err := setPatchStateBulk(context.Background(), mock,
		patch.StateProposed, patch.StateAccepted, jevDecider, []string{"c", "a", "b", "a"})
	if err != nil {
		t.Fatalf("setPatchStateBulk: %v", err)
	}
	if strings.Join(res.Changed, ",") != "a,c" || strings.Join(res.Skipped, ",") != "b" {
		t.Errorf("got changed=%v skipped=%v, want [a c] / [b]", res.Changed, res.Skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSetPatchStateBulk_RejectDoesNotFlag: a reject changes no overlay.
func TestSetPatchStateBulk_RejectDoesNotFlag(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("new mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery("WITH lk AS").
		WithArgs([]string{"a"}, patch.StateProposed, patch.StateRejected, jevDecider, false).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("a"))
	if _, err := setPatchStateBulkTx(context.Background(), mock,
		patch.StateProposed, patch.StateRejected, jevDecider, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestSetPatchStateBulk_EmptyIsNoop: nothing to do opens no transaction.
func TestSetPatchStateBulk_EmptyIsNoop(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	res, err := setPatchStateBulk(context.Background(), mock, patch.StateProposed, patch.StateAccepted, jevDecider, nil)
	if err != nil || len(res.Changed)+len(res.Skipped) != 0 {
		t.Fatalf("got %+v, %v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestDecisionEventValidate(t *testing.T) {
	p := func(v float64) *float64 { return &v }
	ok := DecisionEvent{FindingID: "f", RecipeID: testRecipe, Outcome: OutcomeHold, Reason: "no evidence", P: p(0.5)}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid event refused: %v", err)
	}
	bad := map[string]func(*DecisionEvent){
		"no finding":   func(e *DecisionEvent) { e.FindingID = "" },
		"bad recipe":   func(e *DecisionEvent) { e.RecipeID = "jev:" + testRecipe },
		"bad outcome":  func(e *DecisionEvent) { e.Outcome = "accept" },
		"no reason":    func(e *DecisionEvent) { e.Reason = "" },
		"p above one":  func(e *DecisionEvent) { e.P = p(1.01) },
		"p negative":   func(e *DecisionEvent) { e.P = p(-0.1) },
		"p NaN":        func(e *DecisionEvent) { e.P = p(math.NaN()) },
		"bad evidence": func(e *DecisionEvent) { e.Evidence = "vibes" },
	}
	for name, mutate := range bad {
		e := ok
		mutate(&e)
		if err := e.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestInsertDecisionEvents_RefusesMissingFinding: a decision on a finding
// that does not exist fails rather than silently dropping.
func TestInsertDecisionEvents_RefusesMissingFinding(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	mock.ExpectQuery("INSERT INTO finding_events").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(1)))
	evs := []DecisionEvent{
		{FindingID: "a", RecipeID: testRecipe, Outcome: OutcomeApply, Reason: "r"},
		{FindingID: "gone", RecipeID: testRecipe, Outcome: OutcomeHold, Reason: "r"},
	}
	if _, err := InsertDecisionEvents(context.Background(), mock, evs); err == nil {
		t.Fatal("want an error for the missing finding")
	}
}

// TestRevokeEvents_Validates: a revoke needs a recipe id and an actor.
func TestRevokeEvents_Validates(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	if _, err := RevokeEvents(context.Background(), mock, "nope", "cli:justin"); err == nil {
		t.Error("bad recipe id accepted")
	}
	if _, err := RevokeEvents(context.Background(), mock, testRecipe, ""); err == nil {
		t.Error("empty actor accepted")
	}
	mock.ExpectExec("INSERT INTO finding_events").
		WithArgs(testRecipe, "cli:justin").
		WillReturnResult(pgxmock.NewResult("INSERT", 3))
	n, err := RevokeEvents(context.Background(), mock, testRecipe, "cli:justin")
	if err != nil || n != 3 {
		t.Fatalf("RevokeEvents = %d, %v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestFindingEventSQL_AppendOnly: nothing in the events path updates or
// deletes an event; undo is a revoke row.
func TestFindingEventSQL_AppendOnly(t *testing.T) {
	for name, sql := range map[string]string{
		"insertDecisionEventsSQL": insertDecisionEventsSQL,
		"revokeEventsSQL":         revokeEventsSQL,
		"findingHistorySQL":       findingHistorySQL,
		"patchSetAtSQL":           patchSetAtSQL,
	} {
		up := strings.ToUpper(norm(sql))
		if strings.Contains(up, "UPDATE FINDING_EVENTS") || strings.Contains(up, "DELETE FROM FINDING_EVENTS") ||
			strings.Contains(up, "DO UPDATE") {
			t.Errorf("%s rewrites finding_events:\n%s", name, sql)
		}
	}
	if !strings.Contains(norm(revokeEventsSQL), "ON CONFLICT (revokes_event_id) WHERE kind = 'revoke' DO NOTHING") {
		t.Error("revokeEventsSQL must lean on finding_events_revokes_idx")
	}
	if !strings.Contains(norm(findingHistorySQL), "LIMIT $2") {
		t.Error("findingHistorySQL must be bounded")
	}
}

// TestApplyDecisions_ValidatesBeforeTx: a malformed batch opens no
// transaction.
func TestApplyDecisions_ValidatesBeforeTx(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	ok := DecisionEvent{FindingID: "a", RecipeID: testRecipe, Outcome: OutcomeApply, Reason: "r"}
	other := ok
	other.FindingID, other.RecipeID = "b", strings.Repeat("cd", 32)
	cases := map[string]struct {
		recipe string
		evs    []DecisionEvent
	}{
		"bad recipe id": {"jev:" + testRecipe, []DecisionEvent{ok}},
		"mixed recipes": {testRecipe, []DecisionEvent{ok, other}},
		"duplicate":     {testRecipe, []DecisionEvent{ok, ok}},
		"invalid event": {testRecipe, []DecisionEvent{{FindingID: "a", RecipeID: testRecipe, Outcome: "accept", Reason: "r"}}},
	}
	for name, tc := range cases {
		if _, err := applyDecisions(context.Background(), mock, tc.recipe, tc.evs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if res, err := applyDecisions(context.Background(), mock, testRecipe, nil); err != nil || len(res.Skipped) != 0 {
		t.Errorf("empty batch = %+v, %v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("validation must not touch the database: %v", err)
	}
}

// TestApplyDecisionsSQL_LockOrder: findings SKIP LOCKED in id order, then
// chunks FOR SHARE — the repo-wide findings → chunks order.
func TestApplyDecisionsSQL_LockOrder(t *testing.T) {
	if !strings.Contains(norm(applyDecisionsLockSQL), "patch_state = 'proposed' ORDER BY id FOR UPDATE SKIP LOCKED") {
		t.Errorf("findings must be locked in id order, SKIP LOCKED, proposed only:\n%s", applyDecisionsLockSQL)
	}
	if !strings.Contains(norm(applyDecisionsChunksSQL), "ORDER BY c.id FOR SHARE OF c") ||
		!strings.Contains(applyDecisionsChunksSQL, "COALESCE(c.source_text, c.text)") {
		t.Errorf("chunks must be read pristine and locked FOR SHARE in order:\n%s", applyDecisionsChunksSQL)
	}
}
