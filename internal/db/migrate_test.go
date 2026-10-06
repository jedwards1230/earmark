package db

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/jedwards1230/earmark/internal/log"
)

// TestDecideBaseline is the no-database half of the baseline proof: what
// version 1 does for each kind of database it can meet.
func TestDecideBaseline(t *testing.T) {
	tests := []struct {
		name    string
		present bool
		missing []string
		want    baselineAction
	}{
		{"empty database is created", false, nil, baselineCreate},
		// A missing list without the legacy table is meaningless — still create.
		{"empty database ignores a missing list", false, []string{"table:x"}, baselineCreate},
		{"complete pre-goose database is only stamped", true, nil, baselineStamp},
		{"complete pre-goose database (empty slice) is only stamped", true, []string{}, baselineStamp},
		{"older pre-goose database is caught up", true, []string{"column:run_metrics.eval_error"}, baselineCatchUp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideBaseline(tt.present, tt.missing); got != tt.want {
				t.Errorf("decideBaseline(%v, %v) = %v, want %v", tt.present, tt.missing, got, tt.want)
			}
		})
	}
}

// TestBaselineInventory pins what "complete" means for a pre-goose database.
// The inventory is parsed from the baseline file; if the parse silently
// stopped matching, every older database would be "complete" and get stamped
// with columns missing.
func TestBaselineInventory(t *testing.T) {
	inv := baselineInventory()
	for _, want := range []string{
		"table:transcription_jobs", "table:transcripts", "table:transcript_chunks",
		"table:runner_control", "table:run_metrics", "table:book_metadata",
		"table:transcript_findings", "table:pipeline_events",
		"index:transcript_chunks_embedding_idx", "index:transcript_findings_patch_state_idx",
		"constraint:transcription_jobs_file_path_unique", "constraint:transcript_findings_origin_valid",
		"function:transcription_jobs_set_updated_at", "function:transcription_jobs_set_completed_at",
		"trigger:transcription_jobs_updated_at", "trigger:transcription_jobs_completed_at",
		// One per ALTER block, first and last column where it has several.
		"column:runner_control.run_limit", "column:runner_control.runner_heartbeat_at",
		"column:runner_control.runner_version", "column:runner_control.runner_update_at",
		"column:transcript_findings.patch_state", "column:transcript_findings.stale_reason",
		"column:transcript_findings.origin", "column:transcript_findings.resolved_model",
		"column:transcript_chunks.embedding_stale", "column:transcript_chunks.source_text",
		"column:run_metrics.asr_family", "column:run_metrics.mean_word_confidence",
		"column:run_metrics.eval_started_at", "column:run_metrics.eval_error",
		"column:run_metrics.eval_resolved_model",
		"column:book_metadata.description", "column:book_metadata.isbn",
		"column:transcription_jobs.completed_at",
	} {
		if !slices.Contains(inv, want) {
			t.Errorf("baseline inventory is missing %q", want)
		}
	}
	// Every ADD COLUMN in the file must be represented — a regex that stops at
	// the first clause of a multi-column ALTER would under-count.
	src := stripSQLComments(baselineSQL())
	if got, want := countPrefix(inv, "column:"), strings.Count(src, "ADD COLUMN IF NOT EXISTS"); got != want {
		t.Errorf("inventory has %d columns, baseline has %d ADD COLUMN clauses", got, want)
	}
	if got, want := countPrefix(inv, "table:"), strings.Count(src, "CREATE TABLE IF NOT EXISTS"); got != want {
		t.Errorf("inventory has %d tables, baseline creates %d", got, want)
	}
}

// stripSQLComments drops "--" line comments (the baseline's prose mentions
// "ADD COLUMN IF NOT EXISTS" in passing).
func stripSQLComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if at := strings.Index(l, "--"); at >= 0 {
			lines[i] = l[:at]
		}
	}
	return strings.Join(lines, "\n")
}

func countPrefix(xs []string, prefix string) int {
	n := 0
	for _, x := range xs {
		if strings.HasPrefix(x, prefix) {
			n++
		}
	}
	return n
}

// TestMissingObjects: the catch-up trigger is any single missing object.
func TestMissingObjects(t *testing.T) {
	want := []string{"table:a", "column:a.b", "index:c"}
	have := map[string]bool{"table:a": true, "index:c": true, "table:unrelated": true}
	got := missingObjects(want, have)
	if !slices.Equal(got, []string{"column:a.b"}) {
		t.Errorf("missingObjects = %v, want [column:a.b]", got)
	}
	if got := missingObjects(want, map[string]bool{"table:a": true, "column:a.b": true, "index:c": true}); got != nil {
		t.Errorf("complete schema reported missing %v", got)
	}
}

// TestBaselineIsNotAGooseSQLMigration: goose must not ALSO pick the baseline up
// as a plain SQL migration (it would run it unconditionally, re-executing DDL
// on the production database). It is excluded by name and registered as the
// Go migration for version 1.
func TestBaselineIsNotAGooseSQLMigration(t *testing.T) {
	srcs := mustProvider(t).ListSources()
	if len(srcs) == 0 || srcs[0].Version != 1 || srcs[0].Type != "go" {
		t.Fatalf("version 1 must be the registered Go baseline, got %+v", srcs)
	}
	seen := map[int64]bool{}
	for _, s := range srcs {
		if strings.HasSuffix(s.Path, baselineFileName) {
			t.Errorf("goose scanned %s as an SQL migration", baselineFileName)
		}
		if seen[s.Version] {
			t.Errorf("duplicate migration version %d", s.Version)
		}
		seen[s.Version] = true
	}
}

// mustProvider builds the migration provider over an unopened handle: goose's
// NewProvider only collects sources, it does not connect.
func mustProvider(t *testing.T) *goose.Provider {
	t.Helper()
	cfg, err := pgx.ParseConfig("postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := newMigrationProvider(sqlDB, log.NewLogger("test"))
	if err != nil {
		t.Fatalf("newMigrationProvider: %v", err)
	}
	return p
}

// TestRecipesMigrationLocksStampedTables: 00002 computes the legacy recipe
// set several times under READ COMMITTED. Writers must be frozen BEFORE the
// first of those reads, or a concurrent commit can add a key the recipes
// INSERT missed (FK failure) or be mislabelled by the constant default.
func TestRecipesMigrationLocksStampedTables(t *testing.T) {
	b, err := migrationFiles.ReadFile("migrations/00002_recipes.sql")
	if err != nil {
		t.Fatal(err)
	}
	src := stripSQLComments(string(b))
	lockAt := strings.Index(src, "LOCK TABLE transcripts, transcript_findings, transcript_chunks IN SHARE ROW EXCLUSIVE MODE;")
	if lockAt < 0 {
		t.Fatal("00002 no longer locks transcripts, transcript_findings and transcript_chunks in SHARE ROW EXCLUSIVE mode")
	}
	for _, first := range []string{"FROM transcripts", "FROM transcript_findings", "FROM transcript_chunks", "ALTER TABLE"} {
		if at := strings.Index(src, first); at >= 0 && at < lockAt {
			t.Errorf("%q appears before the LOCK TABLE — it reads or alters a stamped table unlocked", first)
		}
	}
}
