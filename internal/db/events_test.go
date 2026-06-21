package db

import (
	"strings"
	"testing"
)

// TestAppendEventSQL_IsInsertOnly asserts the append-only contract (CONTRACT
// §1.7): AppendEvent's SQL is an INSERT into pipeline_events and never an
// UPDATE/DELETE, and it carries every event column.
func TestAppendEventSQL_IsInsertOnly(t *testing.T) {
	sql := strings.Join(strings.Fields(appendEventSQL), " ")

	if !strings.HasPrefix(sql, "INSERT INTO pipeline_events") {
		t.Errorf("appendEventSQL must INSERT INTO pipeline_events; got %q", sql)
	}
	for _, forbidden := range []string{"UPDATE ", "DELETE ", "DROP ", "ALTER "} {
		if strings.Contains(strings.ToUpper(sql), forbidden) {
			t.Errorf("appendEventSQL must be append-only; found %q in %q", forbidden, sql)
		}
	}

	wantCols := []string{
		"job_id", "file_path", "stage", "event", "runner_host", "model",
		"model_version", "duration_ms", "item_count", "token_count", "attempt",
		"reason", "detail",
	}
	for _, c := range wantCols {
		if !strings.Contains(sql, c) {
			t.Errorf("appendEventSQL is missing the %s column", c)
		}
	}
	// Empty-string identity fields must coalesce to NULL.
	if !strings.Contains(sql, "NULLIF($1,'')::uuid") {
		t.Error("appendEventSQL must NULLIF the job_id so an empty string becomes NULL")
	}
	if !strings.Contains(sql, "$13::jsonb") {
		t.Error("appendEventSQL must cast detail to jsonb")
	}
}

// TestPruneEventsSQL_OnlyHighFrequencyStages asserts the retention prune deletes
// ONLY heartbeat/runner_availability rows past the window — per-job stage events
// are kept indefinitely (CONTRACT §1.7).
func TestPruneEventsSQL_OnlyHighFrequencyStages(t *testing.T) {
	sql := strings.Join(strings.Fields(pruneEventsSQL), " ")

	if !strings.HasPrefix(sql, "DELETE FROM pipeline_events") {
		t.Errorf("pruneEventsSQL must DELETE FROM pipeline_events; got %q", sql)
	}
	if !strings.Contains(sql, "stage IN ('heartbeat','runner_availability')") {
		t.Error("pruneEventsSQL must scope the delete to heartbeat/runner_availability stages")
	}
	if !strings.Contains(sql, "interval '180 days'") {
		t.Error("pruneEventsSQL must use the 180-day retention window")
	}
	// It must NOT blanket-delete per-job stages.
	for _, perJob := range []string{"'embed'", "'eval'", "'enqueue'", "'transcribe'", "'done'"} {
		if strings.Contains(sql, perJob) {
			t.Errorf("pruneEventsSQL must not delete per-job stage %s", perJob)
		}
	}
}

func TestEventPtrHelpers(t *testing.T) {
	if got := *Int64Ptr(7); got != 7 {
		t.Errorf("Int64Ptr(7) = %d", got)
	}
	if got := *IntPtr(3); got != 3 {
		t.Errorf("IntPtr(3) = %d", got)
	}
}
