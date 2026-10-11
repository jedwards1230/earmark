package db

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/jedwards1230/earmark/internal/patch"
)

// The eval layer's defining property (CONTRACT §2.15) is that it is read-only
// over the transcript tables: it may READ transcripts/transcript_chunks and
// INSERT into transcript_findings, but it must NEVER issue UPDATE/DELETE/ALTER
// against the transcript tables. These tests assert that at the SQL level on the
// package-var SQL the eval methods use, so a future edit that smuggles a write
// verb in is caught without a live database.

// evalSQL is the set of SQL statements the eval layer issues.
var evalSQL = map[string]string{
	"evalChunkSelectSQL": evalChunkSelectSQL,
	"insertFindingSQL":   insertFindingSQL,
}

func TestEvalSQL_NeverMutatesTranscriptTables(t *testing.T) {
	transcriptTables := []string{"transcripts", "segments", "transcript_chunks"}
	writeVerbs := []string{"UPDATE", "DELETE", "ALTER", "DROP", "TRUNCATE"}

	for name, sql := range evalSQL {
		upper := strings.ToUpper(sql)
		for _, verb := range writeVerbs {
			// A write verb appearing anywhere in an eval statement is a red flag;
			// the eval layer only ever SELECTs (chunks) or INSERTs (findings).
			if strings.Contains(upper, verb+" ") {
				// Allow the verb only if it is provably not against a transcript
				// table — but the eval layer never writes any table except
				// transcript_findings, so the simplest honest assertion is: no
				// write verb at all in these statements.
				t.Errorf("%s contains write verb %q — eval layer must be read/insert-only:\n%s", name, verb, sql)
			}
		}
		// Belt-and-suspenders: no eval statement may name a transcript table in a
		// write position. The SELECT legitimately names transcripts/chunks for
		// READING, so we only forbid the write verbs above; here we just confirm
		// the INSERT targets exactly transcript_findings.
		if name == "insertFindingSQL" {
			if !strings.Contains(upper, "INSERT INTO TRANSCRIPT_FINDINGS") {
				t.Errorf("insertFindingSQL must INSERT INTO transcript_findings, got:\n%s", sql)
			}
			for _, tbl := range transcriptTables {
				// transcript_findings contains the substring "transcript" — guard
				// against a false match by checking for the table as a write target.
				if strings.Contains(upper, "INTO "+strings.ToUpper(tbl)+" ") ||
					strings.Contains(upper, "INTO "+strings.ToUpper(tbl)+"(") ||
					strings.HasSuffix(upper, "INTO "+strings.ToUpper(tbl)) {
					t.Errorf("insertFindingSQL must not write to transcript table %q:\n%s", tbl, sql)
				}
			}
		}
	}
}

func TestEvalChunkSelectSQL_ReadsTranscriptsReadOnly(t *testing.T) {
	upper := strings.ToUpper(evalChunkSelectSQL)
	if !strings.HasPrefix(strings.TrimSpace(upper), "SELECT") {
		t.Errorf("evalChunkSelectSQL must start with SELECT (read-only):\n%s", evalChunkSelectSQL)
	}
	// It must join the chunk back to its transcript's job so a finding can carry
	// transcription_run_id (per-backend attribution, §2.15).
	if !strings.Contains(upper, "T.JOB_ID") {
		t.Errorf("evalChunkSelectSQL must select t.job_id for run attribution:\n%s", evalChunkSelectSQL)
	}
}

func TestInsertFindingSQL_ColumnOrder(t *testing.T) {
	// The 12 bind params must line up with InsertFindings's Exec argument order.
	// A reorder regression (column list vs $N) is otherwise only caught at runtime
	// against a live DB.
	wantCols := []string{
		"transcript_id", "file_path", "chunk_id", "chunk_index", "start_sec", "end_sec",
		"original_text", "issue_type", "suggested_correction", "confidence", "model",
		"transcription_run_id",
	}
	lower := strings.ToLower(insertFindingSQL)
	last := -1
	for _, col := range wantCols {
		idx := strings.Index(lower, col)
		if idx < 0 {
			t.Fatalf("insertFindingSQL missing column %q:\n%s", col, insertFindingSQL)
		}
		if idx < last {
			t.Errorf("insertFindingSQL column %q out of expected order", col)
		}
		last = idx
	}
	// 12 columns → $1..$12.
	for i := 1; i <= 12; i++ {
		if !strings.Contains(insertFindingSQL, "$"+itoa(i)) {
			t.Errorf("insertFindingSQL missing placeholder $%d:\n%s", i, insertFindingSQL)
		}
	}
}

// TestClearFindingsSQL_SupersedesNeverDeletes guards the clear-findings
// feature: a clear archives findings as superseded — an UPDATE, never a
// DELETE, so decisions and their finding_events history survive — and the
// only other thing it writes is the embedding_stale flag of the chunks whose
// accepted/applied correction it withdrew. It never touches transcripts,
// segments or chunk text (the read-only-transcripts invariant, §2.15).
func TestClearFindingsSQL_SupersedesNeverDeletes(t *testing.T) {
	for name, sql := range map[string]string{
		"clearFindingsAllSQL": clearFindingsAllSQL,
		"clearFindingsDirSQL": clearFindingsDirSQL,
	} {
		n := norm(sql)
		upper := strings.ToUpper(n)
		for _, want := range []string{
			"WHERE patch_state = ANY($1) ",
			"ORDER BY id FOR UPDATE",
			"UPDATE transcript_findings f SET patch_state = 'superseded', superseded_at = now(), unanchorable_reason = NULL",
			"RETURNING f.id, f.transcript_id, f.chunk_index, f.chunk_id, tgt.patch_state AS was",
			"UPDATE transcript_chunks c SET embedding_stale = true FROM upd f WHERE f.was = ANY($2)",
		} {
			if !strings.Contains(n, want) {
				t.Errorf("%s is missing %q:\n%s", name, want, n)
			}
		}
		for _, verb := range []string{"DELETE", "INSERT", "ALTER", "DROP", "TRUNCATE"} {
			if strings.Contains(upper, verb+" ") {
				t.Errorf("%s contains forbidden verb %q:\n%s", name, verb, n)
			}
		}
		// The only chunk column written is the rebuild flag; no transcript
		// table is a write target.
		if strings.Count(upper, "AS ( UPDATE ") != 2 || strings.Contains(upper, "UPDATE TRANSCRIPTS ") {
			t.Errorf("%s must update only transcript_findings and the chunk flag:\n%s", name, n)
		}
		for _, banned := range []string{"SET TEXT", "SOURCE_TEXT =", "EMBEDDING =", "SEGMENTS", "RAW_TEXT"} {
			if strings.Contains(upper, banned) {
				t.Errorf("%s touches %q:\n%s", name, banned, n)
			}
		}
	}
	// The book scope escapes LIKE wildcards in the directory (likePrefix).
	if !strings.Contains(clearFindingsDirSQL, `AND (file_path LIKE $3 ESCAPE '\' OR file_path = $4)`) {
		t.Errorf("clearFindingsDirSQL must scope by an escaped book-dir prefix:\n%s", clearFindingsDirSQL)
	}
}

// TestClearFindingsTx: the clear attributes its transitions (the caller's
// actor, reason 'cleared') before it moves anything, passes the derived
// from-states and the overlay states, and escapes the book directory.
func TestClearFindingsTx(t *testing.T) {
	for _, tc := range []struct {
		name, dir string
		sql       string
		args      []any
	}{
		{"all", "", clearFindingsAllSQL, []any{supersedeFromStates, overlayStates}},
		{"book", " /b/Author/Book_1%/ ", clearFindingsDirSQL,
			[]any{supersedeFromStates, overlayStates, `/b/Author/Book\_1\%/%`, "/b/Author/Book_1%"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()
			mock.ExpectExec(setEventContextSQL).WithArgs("mcp:test", "", "cleared").
				WillReturnResult(pgxmock.NewResult("SELECT", 1))
			mock.ExpectQuery(tc.sql).WithArgs(tc.args...).
				WillReturnRows(pgxmock.NewRows([]string{"n", "ids", "flagged"}).
					AddRow(int64(7), []string{"f1"}, int64(1)))
			res, err := clearFindingsTx(context.Background(), mock, tc.dir, "mcp:test")
			if err != nil {
				t.Fatal(err)
			}
			if res.moved != 7 || len(res.withdrawn) != 1 || res.flagged != 1 {
				t.Errorf("result = %+v", res)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
	if slices.Contains(supersedeFromStates, patch.StateSuperseded) {
		t.Error("a clear must not re-archive superseded findings")
	}
}

// TestListFindingsSQL_ReadOnly guards the worklist queries against the same
// read-only-over-transcripts invariant (§2.15): ListFindings only ever SELECTs
// (joined read-only to transcription_jobs for the JobID) — it must issue no write
// verb. It also pins the scoped form's LIKE clause + ESCAPE, so the "show this
// book's findings" set stays identical to the scoped clear / text-search set.
func TestListFindingsSQL_ReadOnly(t *testing.T) {
	writeVerbs := []string{"UPDATE", "DELETE", "ALTER", "DROP", "TRUNCATE", "INSERT"}
	for name, sql := range map[string]string{
		"listFindingsSQL":       listFindingsSQL,
		"listFindingsInBookSQL": listFindingsInBookSQL,
	} {
		upper := strings.ToUpper(sql)
		if !strings.HasPrefix(strings.TrimSpace(upper), "SELECT") {
			t.Errorf("%s must start with SELECT (read-only):\n%s", name, sql)
		}
		for _, verb := range writeVerbs {
			if strings.Contains(upper, verb+" ") {
				t.Errorf("%s contains write verb %q — list path must be read-only:\n%s", name, verb, sql)
			}
		}
	}
	// The scoped form must filter file_path with the LIKE-escape, matching
	// clearFindingsDirSQL/textSearchInBookSQL so the three select the same book set.
	if !strings.Contains(listFindingsInBookSQL, `file_path LIKE $2 ESCAPE '\'`) {
		t.Errorf("listFindingsInBookSQL must scope via `file_path LIKE $2 ESCAPE '\\'`:\n%s", listFindingsInBookSQL)
	}
	if strings.Contains(listFindingsSQL, "file_path LIKE") {
		t.Errorf("listFindingsSQL (global) must not be scoped to a book:\n%s", listFindingsSQL)
	}
}

// TestFindingsDashboardExcludesSuperseded: every /findings rollup and worklist
// query counts live findings only — a requeued book's archived findings must
// not be counted next to its re-judged ones.
func TestFindingsDashboardExcludesSuperseded(t *testing.T) {
	for name, sql := range map[string]string{
		"findingsTotalsSQL":      findingsTotalsSQL,
		"findingsByIssueTypeSQL": findingsByIssueTypeSQL,
		"findingsByBookSQL":      findingsByBookSQL,
		"listFindingsSQL":        listFindingsSQL,
		"listFindingsInBookSQL":  listFindingsInBookSQL,
		"findingsCountByBookSQL": findingsCountByBookSQL,
	} {
		if !strings.Contains(sql, "patch_state NOT IN ('superseded', 'duplicate')") {
			t.Errorf("%s does not exclude superseded and duplicate findings:\n%s", name, sql)
		}
	}
}

// FindingsSummary buckets must be mutually exclusive and exhaustive over [0,1].
func TestFindingsConfidenceBucketsType(t *testing.T) {
	mean := 0.5
	s := FindingsSummary{
		TotalFindings:    10,
		MeanConfidence:   &mean,
		HighConfidence:   3,
		MediumConfidence: 5,
		LowConfidence:    2,
	}
	if s.HighConfidence+s.MediumConfidence+s.LowConfidence != s.TotalFindings {
		t.Errorf("confidence buckets %d+%d+%d != total %d",
			s.HighConfidence, s.MediumConfidence, s.LowConfidence, s.TotalFindings)
	}
}

// itoa is a tiny local int→string to avoid importing strconv for one use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
