package decide

// Postgres proof that a decide step records its run (CONTRACT §1.10) and that
// a recording failure never fails it. Skipped unless EARMARK_TEST_DATABASE_URL
// points at a server the test may create and drop databases on.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
)

func revertIntegrationDB(t *testing.T) (*db.DB, *pgx.Conn) {
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
		DatabaseURL: u.String(), ChunkSize: 512,
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

// TestIntegrationRevertRecordsStepRun: `decide revert --yes` against the real
// database leaves one closed decide_revert row; with step_runs dropped the
// same revert still succeeds with the same output.
func TestIntegrationRevertRecordsStepRun(t *testing.T) {
	d, conn := revertIntegrationDB(t)
	ctx := context.Background()
	o := revertOptions{recipe: strings.Repeat("ab", 32), yes: true}

	var out1 bytes.Buffer
	if err := revert(ctx, &out1, d, o); err != nil {
		t.Fatalf("revert: %v", err)
	}
	l, err := d.StepRuns(ctx, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Active) != 0 || len(l.Recent) != 1 {
		t.Fatalf("runs = %+v, want one closed run", l)
	}
	r := l.Recent[0]
	if r.Step != "decide_revert" || r.Mode != db.StepRunWrite || r.Status != db.StepRunDone ||
		r.RecipeID != o.recipe || r.Args != "recipe "+o.recipe[:12] || r.Counters == nil || r.Message != "done" {
		t.Errorf("recorded run = %+v", r)
	}

	if _, err := conn.Exec(ctx, `DROP TABLE step_runs`); err != nil {
		t.Fatal(err)
	}
	var out2 bytes.Buffer
	if err := revert(ctx, &out2, d, o); err != nil {
		t.Fatalf("revert with step_runs gone failed: %v", err)
	}
	if out1.String() != out2.String() {
		t.Errorf("recording changed the output:\n%s\nvs\n%s", out1.String(), out2.String())
	}
}
