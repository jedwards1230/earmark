package runs

// Postgres proof of the recorder (CONTRACT §1.10): lifecycle, throttling and
// read-time stale detection against the real step_runs table. Skipped unless
// EARMARK_TEST_DATABASE_URL points at a server the test may create and drop
// databases on (see internal/db/migrate_integration_test.go).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
)

// integrationDB creates a throwaway migrated database; it returns the earmark
// handle and a raw connection for direct SQL.
func integrationDB(t *testing.T) (*db.DB, *pgx.Conn) {
	t.Helper()
	admin := os.Getenv("EARMARK_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("EARMARK_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "earmark_it_" + hex.EncodeToString(b[:])
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = ac.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	d, err := db.New(&config.Config{
		DatabaseURL: u.String(),
		ChunkSize:   512,
		AIEndpoints: []config.AIEndpoint{{ID: "e", Type: config.AIEndpointTypeEmbeddings, Model: "nomic-embed-text"}},
		AIRoles:     &config.AIRoles{Embeddings: "e"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return d, conn
}

// countingStore counts the heartbeat writes that reach the database.
type countingStore struct {
	*db.DB
	updates atomic.Int64
}

func (c *countingStore) UpdateStepRun(ctx context.Context, id int64, p db.StepRunProgress) error {
	c.updates.Add(1)
	return c.DB.UpdateStepRun(ctx, id, p)
}

func onlyRun(t *testing.T, d *db.DB, active bool) db.StepRun {
	t.Helper()
	l, err := d.StepRuns(context.Background(), 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	list := l.Recent
	if active {
		list = l.Active
	}
	if len(list) != 1 {
		t.Fatalf("active=%v: %d runs (%+v), want 1", active, len(list), l)
	}
	return list[0]
}

// TestIntegrationRecorderLifecycleAndThrottle: 8 workers ticking as fast as
// they can for 600 ms reach the table in at most one write per Interval; the
// live row shows the progress while it runs, and Finish closes it with the
// full count.
func TestIntegrationRecorderLifecycleAndThrottle(t *testing.T) {
	d, _ := integrationDB(t)
	cs := &countingStore{DB: d}
	o := Options{Interval: 100 * time.Millisecond, Heartbeat: time.Hour, WriteTimeout: 5 * time.Second, Host: "it-pod"}
	r := StartWith(context.Background(), cs, Spec{Step: StepDecide, Mode: db.StepRunWrite, Args: "--yes", Model: "jev-1.13.0"}, o)
	if r.ID() == 0 {
		t.Fatal("no row inserted at Start")
	}
	if row := onlyRun(t, d, true); row.Message != "starting" || row.Host != "it-pod" || row.Status != db.StepRunRunning {
		t.Errorf("started row = %+v", row)
	}
	r.SetRecipe("rid-1", "")
	r.SetTotal(1_000_000)
	r.Phase("asking jev-1.13.0")

	start := time.Now()
	var wg sync.WaitGroup
	var ticks atomic.Int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Since(start) < 600*time.Millisecond {
				r.Tick("apply", 0.001)
				ticks.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	writes := cs.updates.Load()
	if limit := int64(elapsed/o.Interval) + 2; writes < 1 || writes > limit {
		t.Errorf("%d heartbeat writes for %d ticks in %v, want 1..%d (one per %v)", writes, ticks.Load(), elapsed, limit, o.Interval)
	}
	t.Logf("%d ticks in %v → %d writes", ticks.Load(), elapsed, writes)

	// Mid-run the row carries the progress of the last write.
	time.Sleep(2 * o.Interval)
	live := onlyRun(t, d, true)
	if live.Done == nil || *live.Done == 0 || live.RecipeID != "rid-1" || live.Message != "asking jev-1.13.0" ||
		*live.Total != 1_000_000 || live.Counters["apply"] != *live.Done {
		t.Errorf("live row = %+v", live)
	}

	r.Finish(nil)
	done := onlyRun(t, d, false)
	if done.Status != db.StepRunDone || done.FinishedAt == nil || *done.Done != ticks.Load() ||
		done.Counters["apply"] != ticks.Load() || done.Stale(db.StepRunStaleAfter) {
		t.Errorf("finished row = %+v, want done with %d", done, ticks.Load())
	}
}

// TestIntegrationStaleDetection: a run whose process stops heartbeating
// (simulated by ageing heartbeat_at, which is all a killed exec leaves
// behind) reads as stale, with no reaper involved: the same row reads live
// again as soon as a heartbeat lands, and the stale flag tracks the
// heartbeat, not the elapsed time.
func TestIntegrationStaleDetection(t *testing.T) {
	d, conn := integrationDB(t)
	ctx := context.Background()
	o := Options{Interval: 10 * time.Millisecond, Heartbeat: time.Hour, Host: "it-pod"}
	r := StartWith(ctx, d, Spec{Step: StepScan, Mode: db.StepRunWrite}, o)
	defer r.Finish(nil)

	// A long-running but alive run is not stale.
	if _, err := conn.Exec(ctx, `UPDATE step_runs SET started_at = now() - interval '3 hours' WHERE id = $1`, r.ID()); err != nil {
		t.Fatal(err)
	}
	if row := onlyRun(t, d, true); row.Stale(db.StepRunStaleAfter) {
		t.Fatalf("a 3-hour run with a fresh heartbeat reads stale: %+v", row)
	}

	// The process "dies": no heartbeat for 3 minutes.
	if _, err := conn.Exec(ctx, `UPDATE step_runs SET heartbeat_at = now() - interval '3 minutes' WHERE id = $1`, r.ID()); err != nil {
		t.Fatal(err)
	}
	row := onlyRun(t, d, true)
	if !row.Stale(db.StepRunStaleAfter) || row.Status != db.StepRunRunning {
		t.Fatalf("a 3-minute-old heartbeat must read stale (status stays running): %+v", row)
	}
	// Just under the threshold it is still live.
	if _, err := conn.Exec(ctx, `UPDATE step_runs SET heartbeat_at = now() - interval '110 seconds' WHERE id = $1`, r.ID()); err != nil {
		t.Fatal(err)
	}
	if row := onlyRun(t, d, true); row.Stale(db.StepRunStaleAfter) {
		t.Errorf("a 110 s heartbeat reads stale; the threshold is %v", db.StepRunStaleAfter)
	}

	// It was only slow, not dead: the next heartbeat makes it live again.
	if _, err := conn.Exec(ctx, `UPDATE step_runs SET heartbeat_at = now() - interval '3 minutes' WHERE id = $1`, r.ID()); err != nil {
		t.Fatal(err)
	}
	r.Tick("scanned", 0)
	deadline := time.Now().Add(3 * time.Second)
	for onlyRun(t, d, true).Stale(db.StepRunStaleAfter) {
		if time.Now().After(deadline) {
			t.Fatal("a fresh heartbeat did not clear stale")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestIntegrationRecordingFailureNeverFailsTheStep: with step_runs gone (a
// database on an older schema, a revoked grant), the recorder logs, counts
// and carries on; the step's own work is untouched.
func TestIntegrationRecordingFailureNeverFailsTheStep(t *testing.T) {
	d, conn := integrationDB(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `DROP TABLE step_runs`); err != nil {
		t.Fatal(err)
	}
	o := Options{Interval: 5 * time.Millisecond, Heartbeat: time.Hour, Host: "it-pod"}
	r := StartWith(ctx, d, Spec{Step: StepDecide, Mode: db.StepRunWrite}, o)
	if r == nil || r.ID() != 0 {
		t.Fatalf("start with no table: %v", r)
	}
	// "The step": real database work alongside the recorder.
	work := 0
	for i := 0; i < 50; i++ {
		var one int
		if err := conn.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("the step's own query failed: %v", err)
		}
		work += one
		r.Tick("apply", 0)
		time.Sleep(time.Millisecond)
	}
	r.Finish(nil)
	if work != 50 {
		t.Errorf("work = %d", work)
	}
	if _, f := r.Stats(); f < 2 {
		t.Errorf("recording failures = %d, want the failed inserts counted", f)
	}
}
