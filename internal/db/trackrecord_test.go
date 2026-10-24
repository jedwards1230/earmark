package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"
)

func TestModelTrackTotals(t *testing.T) {
	m := ModelTrack{PassesComplete: 5, PassesPartial: 2, PassesFailed: 1, PassesLegacy: 4, PassesRunning: 1,
		Apply: 3, Hold: 2, Reject: 1, Duplicate: 1, Undone: 9}
	if got := m.PassTotal(); got != 13 {
		t.Errorf("PassTotal = %d, want 13 (running included)", got)
	}
	if got := m.DecisionTotal(); got != 7 {
		t.Errorf("DecisionTotal = %d, want 7 (undone excluded)", got)
	}
	if !m.HasPasses() || !m.HasDecisions() {
		t.Error("a row with both sides must report both")
	}
	if (ModelTrack{}).HasPasses() || (ModelTrack{}).HasDecisions() {
		t.Error("an empty row has neither")
	}
	// Only undone decisions: the alias did decide, nothing is live.
	if u := (ModelTrack{Undone: 2}); !u.HasDecisions() || u.HasPasses() {
		t.Errorf("undone-only row: decisions %v passes %v", u.HasDecisions(), u.HasPasses())
	}
}

func TestModelTrackRecord(t *testing.T) {
	ctx := context.Background()
	roTx := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	t0 := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

	expectSession := func(mock pgxmock.PgxPoolIface) {
		mock.ExpectBeginTx(roTx)
		mock.ExpectExec(`set_config\('jit', 'off', true\), set_config\('statement_timeout', \$1, true\)`).
			WithArgs("4000").WillReturnResult(pgxmock.NewResult("SELECT", 1))
	}

	t.Run("merges both sides by alias, alias-less last", func(t *testing.T) {
		mock := newMockPool(t)
		expectSession(mock)
		mock.ExpectQuery(`FROM judge_passes p\s+LEFT JOIN recipes r ON r\.recipe_id = p\.recipe_id\s+GROUP BY 1, 2\s+ORDER BY 1, 2\s+LIMIT \$1`).
			WithArgs(trackRecordLimit).
			WillReturnRows(pgxmock.NewRows([]string{"alias", "status", "n", "at"}).
				AddRow("", "legacy", int64(3), &t0).
				AddRow("luna", "complete", int64(40), &t1).
				AddRow("luna", "failed", int64(2), &t0).
				AddRow("luna", "partial", int64(5), &t0).
				AddRow("luna", "running", int64(1), &t0).
				AddRow("luna", "legacy", int64(7), &t0))
		mock.ExpectQuery(`WITH ev AS MATERIALIZED`).
			WithArgs(trackRecordLimit).
			WillReturnRows(pgxmock.NewRows([]string{"alias", "outcome", "n", "at"}).
				AddRow("clef", "apply", int64(10), &t1).
				AddRow("clef", "duplicate", int64(4), &t0).
				AddRow("clef", "hold", int64(20), &t0).
				AddRow("clef", "reject", int64(6), &t0).
				AddRow("clef", "undone", int64(2), (*time.Time)(nil)))
		mock.ExpectCommit()

		got, err := modelTrackRecord(ctx, mock)
		if err != nil {
			t.Fatal(err)
		}
		want := []ModelTrack{
			{ModelAlias: "clef", Apply: 10, Hold: 20, Reject: 6, Duplicate: 4, Undone: 2, Last: t1},
			{ModelAlias: "luna", PassesComplete: 40, PassesPartial: 5, PassesFailed: 2, PassesLegacy: 7, PassesRunning: 1, Last: t1},
			{ModelAlias: "", PassesLegacy: 3, Last: t0},
		}
		if len(got) != len(want) {
			t.Fatalf("rows = %+v, want %d", got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
			}
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})

	t.Run("an alias on both sides is one row", func(t *testing.T) {
		mock := newMockPool(t)
		expectSession(mock)
		mock.ExpectQuery(`FROM judge_passes`).WithArgs(trackRecordLimit).
			WillReturnRows(pgxmock.NewRows([]string{"alias", "status", "n", "at"}).AddRow("both", "complete", int64(1), &t0))
		mock.ExpectQuery(`WITH ev AS`).WithArgs(trackRecordLimit).
			WillReturnRows(pgxmock.NewRows([]string{"alias", "outcome", "n", "at"}).AddRow("both", "apply", int64(1), &t1))
		mock.ExpectCommit()
		got, err := modelTrackRecord(ctx, mock)
		if err != nil || len(got) != 1 || !got[0].HasPasses() || !got[0].HasDecisions() || !got[0].Last.Equal(t1) {
			t.Errorf("got %+v, %v", got, err)
		}
	})

	t.Run("empty database", func(t *testing.T) {
		mock := newMockPool(t)
		expectSession(mock)
		mock.ExpectQuery(`FROM judge_passes`).WithArgs(trackRecordLimit).
			WillReturnRows(pgxmock.NewRows([]string{"alias", "status", "n", "at"}))
		mock.ExpectQuery(`WITH ev AS`).WithArgs(trackRecordLimit).
			WillReturnRows(pgxmock.NewRows([]string{"alias", "outcome", "n", "at"}))
		mock.ExpectCommit()
		got, err := modelTrackRecord(ctx, mock)
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("got %#v, %v; want an empty non-nil slice", got, err)
		}
	})

	t.Run("errors carry their stage", func(t *testing.T) {
		boom := errors.New("boom")
		tests := []struct {
			name  string
			setup func(pgxmock.PgxPoolIface)
			want  string
		}{
			{"begin", func(m pgxmock.PgxPoolIface) { m.ExpectBeginTx(roTx).WillReturnError(boom) }, "track record: begin"},
			{"session", func(m pgxmock.PgxPoolIface) {
				m.ExpectBeginTx(roTx)
				m.ExpectExec(`set_config`).WithArgs("4000").WillReturnError(boom)
				m.ExpectRollback()
			}, "session settings"},
			{"passes", func(m pgxmock.PgxPoolIface) {
				expectSession(m)
				m.ExpectQuery(`FROM judge_passes`).WithArgs(trackRecordLimit).WillReturnError(boom)
				m.ExpectRollback()
			}, "judge passes"},
			{"decisions", func(m pgxmock.PgxPoolIface) {
				expectSession(m)
				m.ExpectQuery(`FROM judge_passes`).WithArgs(trackRecordLimit).
					WillReturnRows(pgxmock.NewRows([]string{"alias", "status", "n", "at"}))
				m.ExpectQuery(`WITH ev AS`).WithArgs(trackRecordLimit).WillReturnError(boom)
				m.ExpectRollback()
			}, "decisions"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				mock := newMockPool(t)
				tt.setup(mock)
				_, err := modelTrackRecord(ctx, mock)
				if !errors.Is(err, boom) || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("err = %v, want %q wrapping boom", err, tt.want)
				}
			})
		}
	})
}
