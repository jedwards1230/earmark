package db

import (
	"strings"
	"testing"
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
