package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

var testRecipe2 = strings.Repeat("e", 64)

func TestDecideWorkSQL(t *testing.T) {
	s := norm(decideWorkSQL)
	for _, want := range []string{
		// the scope
		"f.origin = 'judge'", "f.anchor_offset IS NOT NULL", "f.chunk_text_sha256 IS NOT NULL",
		// proposed, or a re-check of another decide recipe's accept — never a person's
		"(f.patch_state = 'proposed' OR (f.patch_state IN ('accepted', 'applied') AND f.decided_by ~ '^jev:[0-9a-f]{64}$' AND f.decided_by NOT IN (SELECT 'jev:' || eq.recipe_id FROM eq)))",
		// "this recipe" = the run's recipe and the recipes equivalent to it
		"SELECT $1::text AS recipe_id UNION SELECT o.recipe_id FROM recipes c JOIN recipes o",
		"WHERE c.recipe_id = $1",
		// latest live decision: absent, another recipe's, or this recipe's retryable hold
		"NOT EXISTS (SELECT 1 FROM finding_events v WHERE v.kind = 'revoke' AND v.revokes_event_id = e.id)",
		"ORDER BY e.id DESC LIMIT 1",
		"(d.recipe_id IS NULL OR d.recipe_id NOT IN (SELECT eq.recipe_id FROM eq) OR (d.outcome = 'hold' AND d.reason = $8))",
		// the shard, non-negative
		"($4 = 0 OR mod(mod(hashtext(f.transcript_id::text)::bigint, $4) + $4, $4) = $5)",
		// keyset
		"($6::uuid IS NULL OR f.id > $6::uuid) ORDER BY f.id LIMIT $7",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("decide work SQL lacks %q", want)
		}
	}
}

func TestDecideWorkScope(t *testing.T) {
	ok := DecideWorkScope{RecipeID: testRecipe2, RetryReason: "jev_unavailable", Limit: 10}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*DecideWorkScope){
		"recipe":   func(s *DecideWorkScope) { s.RecipeID = "x" },
		"retry":    func(s *DecideWorkScope) { s.RetryReason = "" },
		"shard":    func(s *DecideWorkScope) { s.Shard, s.Shards = 2, 2 },
		"negative": func(s *DecideWorkScope) { s.Shard, s.Shards = -1, 2 },
		"limit":    func(s *DecideWorkScope) { s.Limit = MaxDecideWorkPage + 1 },
		"zero":     func(s *DecideWorkScope) { s.Limit = 0 },
	} {
		s := ok
		mut(&s)
		if _, err := decideWork(context.Background(), newMockPool(t), s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	mock := newMockPool(t)
	cols := []string{"id", "transcript_id", "file_path", "issue_type", "original_text", "suggested_correction",
		"confidence", "chunk_index", "anchor_offset", "anchor_occurrence", "chunk_text_sha256", "patch_state", "decided_by"}
	after := "00000000-0000-0000-0000-000000000001"
	mock.ExpectQuery(`FROM transcript_findings f`).
		WithArgs(testRecipe2, "%Dune%", "", 4, 1, &after, 10, "jev_unavailable").
		WillReturnRows(pgxmock.NewRows(cols))
	s := ok
	s.Book, s.Shard, s.Shards, s.After = "Dune", 1, 4, after
	if _, err := decideWork(context.Background(), mock, s); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestDecideMove(t *testing.T) {
	for _, c := range []struct {
		state, outcome string
		recheck        bool
		want           string
	}{
		{patch.StateProposed, OutcomeApply, false, patch.StateAccepted},
		{patch.StateProposed, OutcomeReject, false, patch.StateRejected},
		{patch.StateProposed, OutcomeHold, false, ""},
		{patch.StateApplied, OutcomeApply, true, ""},
		{patch.StateApplied, OutcomeHold, true, ""},
		{patch.StateApplied, OutcomeReject, true, patch.StateReverted},
		{patch.StateAccepted, OutcomeReject, true, patch.StateRejected},
		{patch.StateAccepted, OutcomeApply, true, ""},
	} {
		got := decideMove(c.state, c.outcome, c.recheck)
		if got != c.want {
			t.Errorf("decideMove(%s, %s, %v) = %q, want %q", c.state, c.outcome, c.recheck, got, c.want)
		}
		// Every move a decision makes must be one the state machine allows
		// and the bulk path accepts — never into applied.
		if got != "" {
			if err := validateBulk(c.state, got, "jev:"+testRecipe2); err != nil {
				t.Errorf("%s -> %s: %v", c.state, got, err)
			}
		}
	}
}

func TestApplyRecheckLockSQL(t *testing.T) {
	s := norm(applyRecheckLockSQL)
	for _, want := range []string{
		"patch_state IN ('accepted', 'applied')",
		"decided_by ~ '^jev:[0-9a-f]{64}$' AND decided_by <> $2",
		"ORDER BY id FOR UPDATE SKIP LOCKED",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("re-check lock lacks %q", want)
		}
	}
}

func TestRevertTransitionsAreLegal(t *testing.T) {
	for from, to := range revertTransitions {
		if err := validateBulk(from, to, "revert:jev:"+testRecipe2); err != nil {
			t.Errorf("%s -> %s: %v", from, to, err)
		}
	}
	// Only a decide recipe's own moves are undone: a person's decided_by,
	// or an undo already done, never matches.
	s := norm(revertCandidatesSQL)
	if !strings.Contains(s, "f.decided_by ~ '^jev:[0-9a-f]{64}$'") {
		t.Errorf("revert candidates must require a jev decider:\n%s", s)
	}
}

func TestRevertScope(t *testing.T) {
	for _, s := range []RevertScope{{}, {RecipeID: "x"}, {FindingID: "not-a-uuid"}} {
		if _, err := revertDecisions(context.Background(), newMockPool(t), s, false); err == nil {
			t.Errorf("%+v accepted", s)
		}
	}
	since := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	mock := newMockPool(t)
	mock.ExpectQuery(`FROM transcript_findings f`).WithArgs(testRecipe2, (*string)(nil), &since).
		WillReturnRows(pgxmock.NewRows([]string{"id", "patch_state", "recipe"}).
			AddRow("a", patch.StateApplied, testRecipe2).
			AddRow("b", patch.StateApplied, testRecipe2).
			AddRow("c", patch.StateRejected, testRecipe2))
	mock.ExpectQuery(`SELECT count`).WithArgs(testRecipe2, (*string)(nil), &since).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(5)))
	rep, err := revertDecisions(context.Background(), mock, RevertScope{RecipeID: testRecipe2, Since: since}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || rep.Moved != 3 || rep.Transitions["applied->reverted"] != 2 || rep.Transitions["rejected->proposed"] != 1 || rep.Revoked != 5 {
		t.Errorf("dry-run report %+v", rep)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
