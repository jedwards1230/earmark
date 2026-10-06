package reanchor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
)

type fakeReanchorer struct {
	calls int
	scope db.ReanchorScope
	apply bool
	rep   db.ReanchorReport
	err   error
}

func (f *fakeReanchorer) Reanchor(_ context.Context, scope db.ReanchorScope, apply bool) (db.ReanchorReport, error) {
	f.calls++
	f.scope, f.apply = scope, apply
	return f.rep, f.err
}

func report() db.ReanchorReport {
	return db.ReanchorReport{ByModel: map[string]*db.ReanchorTally{
		"gemma3:12b": {Total: 10, Unique: 3, Moved: 2, Ambiguous: 1, None: 3, Pending: 1},
		"qwen3.8":    {Total: 2, Already: 2},
	}}
}

func TestRun_DryRunIsTheDefault(t *testing.T) {
	f := &fakeReanchorer{rep: report()}
	var out strings.Builder
	if err := run(context.Background(), &out, f, options{book: "Dune", limit: 50, batch: 7}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.apply {
		t.Error("without --yes the pass must run read-only")
	}
	if f.scope != (db.ReanchorScope{Book: "Dune", Limit: 50, BatchSize: 7}) {
		t.Errorf("scope = %+v", f.scope)
	}
	got := out.String()
	for _, want := range []string{
		"(dry-run)", "re-anchor 5", "park 4 as unanchorable",
		"gemma3:12b", "qwen3.8", "all", "50.0%", "100.0%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preview missing %q:\n%s", want, got)
		}
	}
}

func TestRun_YesApplies(t *testing.T) {
	rep := report()
	rep.Reanchored, rep.MarkedUnanchorable, rep.Conflicts = 5, 4, 1
	f := &fakeReanchorer{rep: rep}
	var out strings.Builder
	if err := run(context.Background(), &out, f, options{yes: true, batch: 25}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !f.apply {
		t.Error("--yes must apply")
	}
	if got := out.String(); strings.Contains(got, "dry-run") ||
		!strings.Contains(got, "Re-anchored 5, marked unanchorable 4, skipped 1") {
		t.Errorf("apply summary wrong:\n%s", got)
	}
}

func TestRun_RejectsBadFlags(t *testing.T) {
	for name, o := range map[string]options{
		"negative limit": {limit: -1, batch: 1},
		"zero batch":     {batch: 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeReanchorer{}
			if err := run(context.Background(), &strings.Builder{}, f, o); err == nil {
				t.Error("want an error")
			}
			if f.calls != 0 {
				t.Error("bad flags must not reach the database")
			}
		})
	}
}

// TestRun_ErrorStillReportsProgress: a run cut short (Ctrl-C, a DB error) has
// already committed its earlier batches; the operator must see what landed.
func TestRun_ErrorStillReportsProgress(t *testing.T) {
	rep := report()
	rep.Reanchored = 3
	f := &fakeReanchorer{rep: rep, err: context.Canceled}
	var out strings.Builder
	err := run(context.Background(), &out, f, options{yes: true, batch: 25})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !strings.Contains(out.String(), "Re-anchored 3") {
		t.Errorf("partial progress not reported:\n%s", out.String())
	}
}

func TestRun_NothingInScope(t *testing.T) {
	f := &fakeReanchorer{rep: db.ReanchorReport{ByModel: map[string]*db.ReanchorTally{}}}
	var out strings.Builder
	if err := run(context.Background(), &out, f, options{batch: 25}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No proposed or unanchorable findings") {
		t.Errorf("got %q", out.String())
	}
}
