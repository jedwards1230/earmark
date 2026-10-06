package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"
)

var correctedChunkCols = []string{"id", "chunk_index", "start_sec", "end_sec", "text", "corrected"}

// TestGetCorrectedTranscriptPageServesProjection: a track with corrections
// returns the requested page of the projection (transcript_chunks.text — the
// surface search returns), with each chunk flagged when it differs from its
// pristine text.
func TestGetCorrectedTranscriptPageServesProjection(t *testing.T) {
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	const job = "11111111-1111-1111-1111-111111111111"
	mock.ExpectBeginTx(correctedPageTxOptions)
	mock.ExpectQuery(correctedChunkCountsSQL).WithArgs(job).
		WillReturnRows(pgxmock.NewRows([]string{"total", "corrected"}).AddRow(3, 1))
	mock.ExpectQuery(correctedChunkPageSQL).WithArgs(job, 1, 2).
		WillReturnRows(pgxmock.NewRows(correctedChunkCols).
			AddRow("c1", 1, 30.0, 60.0, "Ghanima said", true).
			AddRow("c2", 2, 60.0, 90.0, "nothing else", false))
	mock.ExpectRollback()

	p, err := getCorrectedTranscriptPage(context.Background(), mock, job, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if p.TotalChunks != 3 || p.CorrectedChunks != 1 || len(p.Chunks) != 2 {
		t.Fatalf("page = %+v", p)
	}
	if p.Chunks[0].Text != "Ghanima said" || !p.Chunks[0].Corrected || p.Chunks[1].Corrected {
		t.Errorf("chunks = %+v", p.Chunks)
	}
}

// TestGetCorrectedTranscriptPageNoCorrections: with nothing corrected the page
// query never runs — the caller serves the ASR segments (with word times).
// The same holds for a counts-only call (limit 0).
func TestGetCorrectedTranscriptPageNoCorrections(t *testing.T) {
	for _, tc := range []struct {
		name             string
		corrected, limit int
	}{
		{"no corrections", 0, 10},
		{"counts only", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()
			mock.ExpectBeginTx(correctedPageTxOptions)
			mock.ExpectQuery(correctedChunkCountsSQL).WithArgs("j").
				WillReturnRows(pgxmock.NewRows([]string{"total", "corrected"}).AddRow(5, tc.corrected))
			mock.ExpectRollback()

			p, err := getCorrectedTranscriptPage(context.Background(), mock, "j", 0, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
			if p.CorrectedChunks != tc.corrected || p.Chunks != nil {
				t.Errorf("page = %+v", p)
			}
		})
	}
}

// TestCorrectedTranscriptPageOneSnapshot: both queries share one read-only
// REPEATABLE READ snapshot, so the counts and the page cannot straddle a
// rebuild.
func TestCorrectedTranscriptPageOneSnapshot(t *testing.T) {
	if correctedPageTxOptions.IsoLevel != pgx.RepeatableRead || correctedPageTxOptions.AccessMode != pgx.ReadOnly {
		t.Fatalf("tx options = %+v, want read-only REPEATABLE READ", correctedPageTxOptions)
	}
}

// TestCorrectedTextSQLReadsTheProjection pins what "corrected" means: the
// served text is c.text (the projection), a chunk counts as corrected only when
// it differs from a recorded pristine text, and both queries are read-only.
func TestCorrectedTextSQLReadsTheProjection(t *testing.T) {
	for name, sql := range map[string]string{
		"correctedChunkCountsSQL": correctedChunkCountsSQL,
		"correctedChunkPageSQL":   correctedChunkPageSQL,
	} {
		up := strings.ToUpper(sql)
		if !strings.HasPrefix(strings.TrimSpace(up), "SELECT") {
			t.Errorf("%s must be a SELECT", name)
		}
		for _, w := range []string{"INSERT", "UPDATE", "DELETE"} {
			if strings.Contains(up, w) {
				t.Errorf("%s contains %s", name, w)
			}
		}
		if !strings.Contains(sql, "c.source_text IS NOT NULL AND c.text <> c.source_text") {
			t.Errorf("%s must compare the projection to the pristine text", name)
		}
	}
	if !strings.Contains(correctedChunkPageSQL, "c.end_sec, c.text,") {
		t.Error("correctedChunkPageSQL must serve c.text, the corrected projection")
	}
}
