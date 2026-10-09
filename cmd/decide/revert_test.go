package decide

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/db"
)

type fakeReverter struct {
	got   db.RevertScope
	apply bool
	rep   db.RevertReport
}

func (f *fakeReverter) RevertDecisions(_ context.Context, s db.RevertScope, apply bool) (db.RevertReport, error) {
	f.got, f.apply = s, apply
	f.rep.DryRun = !apply
	return f.rep, nil
}

func TestRevertScope(t *testing.T) {
	id := strings.Repeat("ab", 32)
	s, err := revertOptions{recipe: strings.ToUpper(id), finding: " 0B5C0000-0000-0000-0000-000000000001 ", since: "2026-10-09T14:00:00Z"}.scope()
	if err != nil || s.RecipeID != id || s.FindingID != "0b5c0000-0000-0000-0000-000000000001" ||
		!s.Since.Equal(time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("scope %+v, %v", s, err)
	}
	for _, bad := range []revertOptions{{}, {since: "yesterday"}} {
		if _, err := bad.scope(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestRevertOutput(t *testing.T) {
	f := &fakeReverter{rep: db.RevertReport{Transitions: map[string]int{"applied->reverted": 3, "rejected->proposed": 1}, Moved: 4, Revoked: 9}}
	var out bytes.Buffer
	if err := revert(context.Background(), &out, f, revertOptions{recipe: strings.Repeat("cd", 32)}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"applied → reverted", "rejected → proposed", "decisions to revoke: 9", "(dry run)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out.String())
		}
	}
	if f.apply {
		t.Error("dry run applied")
	}
	out.Reset()
	f.rep.Reflagged = 2
	if err := revert(context.Background(), &out, f, revertOptions{recipe: strings.Repeat("cd", 32), yes: true}); err != nil {
		t.Fatal(err)
	}
	if !f.apply || !strings.Contains(out.String(), "chunks re-flagged 2") {
		t.Errorf("apply output:\n%s", out.String())
	}
}
