package db

// Postgres proof for migration 00018 (CONTRACT §1.4 "gpu-arbiter yield"): the
// runner_control.arbiter_yield columns default to "not held", the status read
// model surfaces what gpu-arbiter writes, and Down/Up round-trips. Skipped
// unless EARMARK_TEST_DATABASE_URL is set (see migrate_integration_test.go).

import (
	"context"
	"testing"
)

func TestIntegrationArbiterYieldColumns(t *testing.T) {
	ctx := context.Background()
	dbURL, d := migratedTestDB(t)
	conn := connect(t, dbURL)

	st, err := d.GetServiceStatus(ctx)
	if err != nil {
		t.Fatalf("GetServiceStatus: %v", err)
	}
	if st.ArbiterYield || st.ArbiterYieldAt != nil {
		t.Fatalf("fresh schema: want not held / no timestamp, got %v / %v", st.ArbiterYield, st.ArbiterYieldAt)
	}

	// What gpu-arbiter's gpu_gate writes on yield.
	if _, err := conn.Exec(ctx, `UPDATE runner_control SET arbiter_yield = true, arbiter_yield_at = now() WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	st, err = d.GetServiceStatus(ctx)
	if err != nil {
		t.Fatalf("GetServiceStatus: %v", err)
	}
	if !st.ArbiterYield || st.ArbiterYieldAt == nil {
		t.Fatalf("after a yield: want held with a timestamp, got %v / %v", st.ArbiterYield, st.ArbiterYieldAt)
	}
	if phase, err := d.GetPipelinePhase(ctx); err != nil || phase != PhaseIdle {
		t.Fatalf("a yield must not touch phase: %q, %v", phase, err)
	}

	provider := providerFor(t, dbURL)
	if _, err := provider.DownTo(ctx, 17); err != nil {
		t.Fatalf("Down 00018: %v", err)
	}
	const cols = `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'runner_control' AND column_name IN ('arbiter_yield', 'arbiter_yield_at')`
	if n := countRows(t, conn, cols); n != 0 {
		t.Fatalf("Down left %d arbiter columns", n)
	}
	if _, err := provider.UpTo(ctx, 18); err != nil {
		t.Fatalf("Up 00018 again: %v", err)
	}
	if n := countRows(t, conn, cols); n != 2 {
		t.Fatalf("Up: %d arbiter columns, want 2", n)
	}
	if n := countRows(t, conn, `SELECT count(*) FROM runner_control WHERE id = 1 AND NOT arbiter_yield`); n != 1 {
		t.Fatal("re-added arbiter_yield must default to false on the existing row")
	}
}
