package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/spelling"
)

// fakeSpellings is a SpellingStore over fixed per-book rows.
type fakeSpellings struct {
	findings map[string][]db.SpellingFinding
	pilots   map[string]map[string][]string // dir → model → texts
	prod     map[string][]string
	loads    map[string]int
	err      error
}

func (f *fakeSpellings) SpellingFindings(_ context.Context, dir string) ([]db.SpellingFinding, error) {
	if f.loads == nil {
		f.loads = map[string]int{}
	}
	f.loads[dir]++
	if f.err != nil {
		return nil, f.err
	}
	return f.findings[dir], nil
}

func (f *fakeSpellings) StreamPilotText(_ context.Context, dir string, fn func(string, string) error) error {
	for m, ts := range f.pilots[dir] {
		for _, t := range ts {
			if err := fn(m, t); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeSpellings) StreamProductionText(_ context.Context, dir string, fn func(string) error) error {
	for _, t := range f.prod[dir] {
		if err := fn(t); err != nil {
			return err
		}
	}
	return nil
}

func sf(id, orig, repl, state string) db.SpellingFinding {
	return db.SpellingFinding{ID: id, Original: orig, Replacement: repl, State: state}
}

func spItem(id, orig, repl, decision, reason string) *item {
	p := 0.9
	return &item{
		finding: db.DecideFinding{ID: id, FilePath: "/b/Book/01.m4b", Original: orig, Replacement: repl, PatchState: "proposed"},
		outcome: Outcome{FindingID: id, Decision: decision, Reason: reason, P: &p, Rung0: Verdict{Pass: true}, Evidence: EvidenceNone},
	}
}

func detailOf(t *testing.T, o Outcome) spellingDetail {
	t.Helper()
	var d spellingDetail
	if o.Detail == nil {
		t.Fatalf("%s: no detail", o.FindingID)
	}
	if err := json.Unmarshal(o.Detail, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// The book every case decides in: "ilmus" proposed as ilmas twice and ilmis
// once (contradictory), plus whatever the case adds.
func ilmBook(extra ...db.SpellingFinding) []db.SpellingFinding {
	return append([]db.SpellingFinding{
		sf("a1", "ilmus", "ilmas", "proposed"),
		sf("a2", "the ilmus", "the ilmas", "proposed"),
		sf("a3", "ilmus", "ilmis", "proposed"),
	}, extra...)
}

func TestSpellingPass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		book     []db.SpellingFinding
		pilots   map[string][]string
		it       *item
		decision string
		reason   string
		action   string // "" = no detail expected
		prior    string
		strong   string
	}{
		{
			name: "no strong, contradictory: apply held spelling_conflict",
			book: ilmBook(), it: spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident),
			decision: DecisionHold, reason: ReasonSpellingConflict, action: spellingHeldConflict, prior: ReasonConfident,
		},
		{
			name: "no strong, contradictory: uncertain hold re-labelled",
			book: ilmBook(), it: spItem("a3", "ilmus", "ilmis", DecisionHold, ReasonUncertain),
			decision: DecisionHold, reason: ReasonSpellingConflict, action: spellingHeldConflict, prior: ReasonUncertain,
		},
		{
			name: "no strong, consistent: apply kept",
			book: []db.SpellingFinding{sf("a1", "ilmus", "ilmas", "proposed"), sf("a2", "ilmis", "ilmas", "proposed")},
			it:   spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident), decision: DecisionApply, reason: ReasonConfident,
			action: spellingKept,
		},
		{
			name: "applied strong, toward: apply kept",
			book: ilmBook(sf("x", "ilmus", "ilmas", "applied")), it: spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident),
			decision: DecisionApply, reason: ReasonConfident, action: spellingKept, strong: "ilmas",
		},
		{
			name: "applied strong, toward: no_evidence hold promoted",
			book: ilmBook(sf("x", "ilmus", "ilmas", "applied")), it: spItem("a1", "ilmus", "ilmas", DecisionHold, ReasonNoEvidence),
			decision: DecisionApply, reason: ReasonSpellingEstablished, action: spellingPromoted, prior: ReasonNoEvidence, strong: "ilmas",
		},
		{
			name: "applied strong, toward: uncertain hold not promoted",
			book: ilmBook(sf("x", "ilmus", "ilmas", "applied")), it: spItem("a1", "ilmus", "ilmas", DecisionHold, ReasonUncertain),
			decision: DecisionHold, reason: ReasonUncertain, action: spellingKept, strong: "ilmas",
		},
		{
			name: "applied strong, away: apply held spelling_off_established",
			book: ilmBook(sf("x", "ilmus", "ilmas", "applied")), it: spItem("a3", "ilmus", "ilmis", DecisionApply, ReasonConfident),
			decision: DecisionHold, reason: ReasonSpellingOffEstablished, action: spellingHeldOff, prior: ReasonConfident, strong: "ilmas",
		},
		{
			name: "pilot strong, toward: contradiction resolved, apply kept",
			book: ilmBook(), pilots: map[string][]string{"granite": {"ilmas ilmas ilmas"}, "canary": {"ilmas", "ilmas ilmas"}},
			it:       spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident),
			decision: DecisionApply, reason: ReasonConfident, action: spellingKept, strong: "ilmas",
		},
		{
			name: "pilot strong never promotes",
			book: ilmBook(), pilots: map[string][]string{"granite": {"ilmas ilmas ilmas"}, "canary": {"ilmas ilmas ilmas"}},
			it:       spItem("a1", "ilmus", "ilmas", DecisionHold, ReasonNoEvidence),
			decision: DecisionHold, reason: ReasonNoEvidence, action: spellingKept, strong: "ilmas",
		},
		{
			name: "pilot strong, away: held spelling_off_established",
			book: ilmBook(), pilots: map[string][]string{"granite": {"ilmas ilmas ilmas"}, "canary": {"ilmas ilmas ilmas"}},
			it:       spItem("a3", "ilmus", "ilmis", DecisionHold, ReasonEvidenceExcluded),
			decision: DecisionHold, reason: ReasonSpellingOffEstablished, action: spellingHeldOff, prior: ReasonEvidenceExcluded, strong: "ilmas",
		},
		{
			name: "competing pilot spelling: no strong, conflict",
			book: ilmBook(), pilots: map[string][]string{"granite": {"ilmas ilmas ilmas"}, "canary": {"ilmas ilmas ilmas ilmis"}},
			it:       spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident),
			decision: DecisionHold, reason: ReasonSpellingConflict, action: spellingHeldConflict, prior: ReasonConfident,
		},
		{
			name: "terrillyn split: both held",
			book: []db.SpellingFinding{sf("t1", "said terrillyn", "said terrin", "proposed"), sf("t2", "terrillyn", "terrilyn", "proposed")},
			it:   spItem("t2", "terrillyn", "terrilyn", DecisionApply, ReasonConfident), decision: DecisionHold, reason: ReasonSpellingConflict,
			action: spellingHeldConflict, prior: ReasonConfident,
		},
		// Untouched outcomes: no re-label, no detail.
		{name: "reject untouched", book: ilmBook(), it: spItem("a1", "ilmus", "ilmas", DecisionReject, ReasonJevReject),
			decision: DecisionReject, reason: ReasonJevReject},
		{name: "duplicate untouched", book: ilmBook(), it: spItem("a2", "ilmus", "ilmas", DecisionDuplicate, ReasonDuplicate),
			decision: DecisionDuplicate, reason: ReasonDuplicate},
		{name: "jev_unavailable hold untouched", book: ilmBook(), it: spItem("a1", "ilmus", "ilmas", DecisionHold, ReasonJevUnavailable),
			decision: DecisionHold, reason: ReasonJevUnavailable},
		{name: "issue_type_capped hold untouched", book: ilmBook(), it: spItem("a1", "ilmus", "ilmas", DecisionHold, ReasonTypeCapped),
			decision: DecisionHold, reason: ReasonTypeCapped},
		{name: "capitalised hold untouched", book: ilmBook(), it: spItem("a1", "ilmus", "Ilmas", DecisionHold, ReasonCapitalised),
			decision: DecisionHold, reason: ReasonCapitalised},
		{name: "not in a group untouched", book: ilmBook(), it: spItem("z", "dish", "telescope", DecisionApply, ReasonConfident),
			decision: DecisionApply, reason: ReasonConfident},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeSpellings{findings: map[string][]db.SpellingFinding{"/b/Book": tc.book},
				pilots: map[string]map[string][]string{"/b/Book": tc.pilots}}
			p0 := *tc.it.outcome.P
			if err := newSpellingPass(store).run(context.Background(), []*item{tc.it}); err != nil {
				t.Fatal(err)
			}
			o := tc.it.outcome
			if o.Decision != tc.decision || o.Reason != tc.reason {
				t.Errorf("outcome %s %s, want %s %s", o.Decision, o.Reason, tc.decision, tc.reason)
			}
			if o.P == nil || *o.P != p0 || o.Retryable {
				t.Errorf("p %v retryable %v: the pass keeps p and never marks retryable", o.P, o.Retryable)
			}
			if tc.action == "" {
				if o.Detail != nil {
					t.Errorf("untouched outcome got detail %s", o.Detail)
				}
				return
			}
			d := detailOf(t, o)
			if d.Rung != spelling.DetailRung || d.Rule != spelling.RuleID || d.Action != tc.action || d.PriorReason != tc.prior || d.Strong != tc.strong {
				t.Errorf("detail %+v", d)
			}
			if !slices.Contains(d.Group, d.Original) || !slices.Contains(d.Group, d.Target) || len(d.Counts) != len(d.Group) {
				t.Errorf("detail group %v counts %v", d.Group, d.Counts)
			}
		})
	}
}

// The detail carries the counts the strong spelling rests on.
func TestSpellingDetailCounts(t *testing.T) {
	store := &fakeSpellings{
		findings: map[string][]db.SpellingFinding{"/b/Book": ilmBook()},
		pilots:   map[string]map[string][]string{"/b/Book": {"granite": {"ilmas ilmas ilmas"}, "canary": {"ilmas ilmas ilmas"}}},
		prod:     map[string][]string{"/b/Book": {"ilmus said ilmus"}},
	}
	it := spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident)
	if err := newSpellingPass(store).run(context.Background(), []*item{it}); err != nil {
		t.Fatal(err)
	}
	d := detailOf(t, it.outcome)
	if d.StrongSource != spelling.SourcePilots || d.Counts["ilmas"].Pilots["granite"] != 3 || d.Counts["ilmus"].Production != 2 {
		t.Errorf("detail %+v", d)
	}
	if !slices.Equal(d.Targets, []string{"ilmas", "ilmis"}) || !d.Contradictory || d.SpellingsTotal != 3 {
		t.Errorf("detail targets %v contradictory %v total %d", d.Targets, d.Contradictory, d.SpellingsTotal)
	}
}

// One load per book per run, only for books with an eligible item; a load
// error fails the run; a store without the reads is a no-op.
func TestSpellingPassLoading(t *testing.T) {
	store := &fakeSpellings{findings: map[string][]db.SpellingFinding{"/b/Book": ilmBook()}}
	sp := newSpellingPass(store)
	other := spItem("o", "ilmus", "ilmas", DecisionApply, ReasonConfident)
	other.finding.FilePath = "/b/Other/01.m4b"
	rejected := spItem("r", "ilmus", "ilmas", DecisionReject, ReasonJevReject)
	rejected.finding.FilePath = "/b/Skipped/01.m4b"
	items := []*item{spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident), spItem("a3", "ilmus", "ilmis", DecisionApply, ReasonConfident), other, rejected}
	if err := sp.run(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	if err := sp.run(context.Background(), items[:1]); err != nil {
		t.Fatal(err)
	}
	if store.loads["/b/Book"] != 1 || store.loads["/b/Other"] != 1 || store.loads["/b/Skipped"] != 0 {
		t.Errorf("loads %v", store.loads)
	}
	if other.outcome.Decision != DecisionApply || other.outcome.Detail != nil {
		t.Errorf("a book without the pair: %+v", other.outcome)
	}

	boom := errors.New("boom")
	if err := newSpellingPass(&fakeSpellings{err: boom}).run(context.Background(),
		[]*item{spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident)}); !errors.Is(err, boom) {
		t.Errorf("load error %v", err)
	}
	it := spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident)
	if err := newSpellingPass(struct{}{}).run(context.Background(), []*item{it}); err != nil || it.outcome.Decision != DecisionApply {
		t.Errorf("no-op pass: %v %+v", err, it.outcome)
	}
}

// The pass's holds must not be liftable by the audio-vote rung: asr_agree
// applies only a hold whose reason is in AudioApplyHoldReasons.
func TestSpellingReasonsNotAudioLiftable(t *testing.T) {
	for _, r := range []string{ReasonSpellingConflict, ReasonSpellingOffEstablished, ReasonSpellingEstablished} {
		if slices.Contains(AudioApplyHoldReasons, r) {
			t.Errorf("%s is in AudioApplyHoldReasons", r)
		}
	}
}

// A held winner holds its losers: the pass runs before resolveConflicts, so a
// conflict whose top-p fix the pass holds leaves the loser overlap_pending,
// never rejected for a fix that is not going in.
func TestSpellingHoldBeforeConflictRanking(t *testing.T) {
	const chunk = "and ilmus walked"
	win := spItem("w", "ilmus", "ilmis", DecisionApply, ReasonConfident)
	lose := spItem("l", "ilmus walked", "ilmus talked", DecisionApply, ReasonConfident)
	pw, pl := 0.95, 0.9
	win.outcome.P, lose.outcome.P = &pw, &pl
	win.outcome.Rung0 = Verdict{Pass: true, Span: patch.Span{Start: 4, End: 9}, Conflicts: []string{"l"}}
	lose.outcome.Rung0 = Verdict{Pass: true, Span: patch.Span{Start: 4, End: 16}, Conflicts: []string{"w"}}
	win.input.Candidate.ChunkText, lose.input.Candidate.ChunkText = chunk, chunk
	store := &fakeSpellings{findings: map[string][]db.SpellingFinding{"/b/Book": ilmBook(sf("w", "ilmus", "ilmis", "proposed"))}}
	items := []*item{win, lose}
	if err := newSpellingPass(store).run(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	var g Grouping
	resolveConflicts(items, &g, false)
	if win.outcome.Reason != ReasonSpellingConflict {
		t.Fatalf("winner %s %s", win.outcome.Decision, win.outcome.Reason)
	}
	if lose.outcome.Decision != DecisionHold || lose.outcome.Reason != ReasonOverlapPending || lose.outcome.Related != "w" {
		t.Errorf("loser %s %s %s, want hold overlap_pending naming the held winner", lose.outcome.Decision, lose.outcome.Reason, lose.outcome.Related)
	}
}

// A shared phonetic key is not the same pair: "right", "write" and "rot" all
// key RT, and an applied "right → write" must not promote "rot → write".
func TestSpellingPromotionNeedsTheAppliedPair(t *testing.T) {
	book := []db.SpellingFinding{sf("x", "right", "write", "applied"), sf("r", "rot", "write", "proposed"), sf("s", "right", "write", "proposed")}
	for _, tc := range []struct {
		name, orig, decision, reason string
	}{
		{"key-only match keeps the hold", "rot", DecisionHold, ReasonNoEvidence},
		{"the applied pair promotes", "right", DecisionApply, ReasonSpellingEstablished},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeSpellings{findings: map[string][]db.SpellingFinding{"/b/Book": book}}
			it := spItem("r", tc.orig, "write", DecisionHold, ReasonNoEvidence)
			if err := newSpellingPass(store).run(context.Background(), []*item{it}); err != nil {
				t.Fatal(err)
			}
			if it.outcome.Decision != tc.decision || it.outcome.Reason != tc.reason {
				t.Errorf("outcome %s %s, want %s %s", it.outcome.Decision, it.outcome.Reason, tc.decision, tc.reason)
			}
		})
	}
}

// A target whose only finding this run rejects does not make the group
// contradictory: a junk "ilmus → ilmis" at p 0.02 must not hold "ilmus →
// ilmas". The same junk finding outside the run still counts.
func TestSpellingIgnoresTargetsTheRunRejects(t *testing.T) {
	book := []db.SpellingFinding{sf("a1", "ilmus", "ilmas", "proposed"), sf("a3", "ilmus", "ilmis", "proposed")}
	for _, tc := range []struct {
		name    string
		junk    *item
		outcome string
	}{
		{"rejected on p", spItem("a3", "ilmus", "ilmis", DecisionReject, ReasonJevReject), DecisionApply},
		{"failed rung 0", func() *item {
			it := spItem("a3", "ilmus", "ilmis", DecisionReject, ReasonNotSoundAlike)
			it.outcome.Rung0 = Verdict{Reason: ReasonNotSoundAlike}
			return it
		}(), DecisionApply},
		{"not in the run", nil, DecisionHold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeSpellings{findings: map[string][]db.SpellingFinding{"/b/Book": book}}
			good := spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident)
			items := []*item{good}
			if tc.junk != nil {
				items = append(items, tc.junk)
			}
			if err := newSpellingPass(store).run(context.Background(), items); err != nil {
				t.Fatal(err)
			}
			if good.outcome.Decision != tc.outcome {
				t.Errorf("good fix %s %s, want %s", good.outcome.Decision, good.outcome.Reason, tc.outcome)
			}
			if d := detailOf(t, good.outcome); (tc.outcome == DecisionApply) == d.Contradictory {
				t.Errorf("detail contradictory %v targets %v", d.Contradictory, d.Targets)
			}
		})
	}
}

// A book over a read cap is skipped — counted, logged, its items untouched,
// loaded once — and the run goes on.
func TestSpellingCapSkipsTheBook(t *testing.T) {
	store := &fakeSpellings{err: fmt.Errorf("spelling findings of /b/Book: more than 50000 live findings: %w", db.ErrSpellingCapExceeded)}
	sp := newSpellingPass(store)
	a := spItem("a1", "ilmus", "ilmas", DecisionApply, ReasonConfident)
	b := spItem("a3", "ilmus", "ilmis", DecisionApply, ReasonConfident)
	if err := sp.run(context.Background(), []*item{a, b}); err != nil {
		t.Fatalf("a capped book must not fail the run: %v", err)
	}
	if sp.skipped != 1 || store.loads["/b/Book"] != 1 {
		t.Errorf("skipped %d loads %v", sp.skipped, store.loads)
	}
	for _, it := range []*item{a, b} {
		if it.outcome.Decision != DecisionApply || it.outcome.Detail != nil {
			t.Errorf("%s touched: %+v", it.finding.ID, it.outcome)
		}
	}
	var out strings.Builder
	rep := &Report{SpellingSkippedBooks: sp.skipped, Findings: 2, Requested: 2, Outcomes: map[string]int{DecisionApply: 2}}
	if err := rep.Print(&out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "skipped 1 book(s) over a read cap") {
		t.Errorf("report does not surface the skip:\n%s", out.String())
	}
}
