package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/jedwards1230/earmark/internal/db"
	elog "github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/spelling"
)

var spellingLog = elog.NewLogger("decide")

// One spelling per book (CONTRACT §2.19 "One spelling per book").
//
// A pass over a run's items after evaluateAll and before resolveConflicts, in
// both DryRun and Apply. For each finding whose differing words form a
// spelling group of its book (internal/spelling), it may re-label an apply or
// an audio-liftable hold (AudioApplyHoldReasons):
//
//	group has a strong spelling, the finding targets it → kept; a
//	    no_evidence hold is promoted to apply spelling_established, but only
//	    when an applied finding of the same pair (original core → the strong
//	    spelling) established it — never a phonetic-key match alone, and
//	    never the pilots (they agree on phonetic misspellings of invented
//	    names)
//	group has a strong spelling, the finding targets another → hold
//	    spelling_off_established
//	no strong spelling, the group's findings contradict → hold
//	    spelling_conflict (a target whose only findings this run rejects
//	    or fails at rung 0 is not counted)
//
// Rejects, duplicates, rung-0 outcomes and every other hold are untouched.
// Re-labelling a liftable hold is also what keeps the audio-vote rung off a
// contradictory finding: asr_agree lifts only a hold whose reason is in
// AudioApplyHoldReasons, and the spelling_* reasons are not.

// Spelling-pass reasons (Outcome.Reason). A data contract like the others:
// add, never rename.
const (
	// ReasonSpellingConflict — the finding's spelling group proposes two or
	// more spellings and none is established: held.
	ReasonSpellingConflict = "spelling_conflict"
	// ReasonSpellingOffEstablished — the finding targets a spelling other
	// than the one its book established: held.
	ReasonSpellingOffEstablished = "spelling_off_established"
	// ReasonSpellingEstablished — held no_evidence, but the finding targets
	// the spelling the book's applied findings established: applied.
	ReasonSpellingEstablished = "spelling_established"
)

// Spelling-pass actions (detail.action).
const (
	spellingKept         = "kept"
	spellingPromoted     = "promoted"
	spellingHeldOff      = "held_off_established"
	spellingHeldConflict = "held_conflict"
)

// maxDetailSpellings caps the spellings (and targets) one event's detail
// lists; detail.spellings_total says how many the group has.
const maxDetailSpellings = 12

// SpellingStore is the per-book reads of the spelling pass (*db.DB implements
// it). A store without it — a test fake — runs without the pass.
type SpellingStore interface {
	SpellingFindings(ctx context.Context, dir string) ([]db.SpellingFinding, error)
	StreamPilotText(ctx context.Context, dir string, fn func(modelID, text string) error) error
	StreamProductionText(ctx context.Context, dir string, fn func(text string) error) error
}

var _ SpellingStore = (*db.DB)(nil)

// spellingPass decides the spelling of a run's items, loading each book's
// model once per run (a snapshot taken at first use: decisions written by
// earlier pages of the same run are not seen). dead accumulates, across every
// page of the run, the findings the run rejects (on p or at rung 0), so a
// junk target rejected on an earlier page never makes a later page's group
// contradictory. A junk target rejected only on a LATER page cannot un-hold
// an earlier page's finding (its hold is already written and final under the
// recipe): a paged --yes run can hold more than a single-page view, never
// apply more (CONTRACT §2.19 "Contradiction"). A book over a read cap
// (db.ErrSpellingCapExceeded) is skipped — logged, counted in skipped, its
// items untouched — rather than failing the run.
type spellingPass struct {
	store   SpellingStore
	books   map[string]*spelling.Book // nil value = skipped book
	dead    map[string]bool           // finding ids this run rejected, all pages so far
	skipped int
}

// newSpellingPass is the pass over store; a store that is not a
// SpellingStore yields a pass that changes nothing.
func newSpellingPass(store any) *spellingPass {
	ss, _ := store.(SpellingStore)
	return &spellingPass{store: ss, books: map[string]*spelling.Book{}, dead: map[string]bool{}}
}

// spellingEligible: an outcome the pass may re-label — an apply, or a hold an
// audio agreement could lift — on a finding that passed rung 0.
func spellingEligible(o Outcome) bool {
	if !o.Rung0.Pass {
		return false
	}
	switch o.Decision {
	case DecisionApply:
		return true
	case DecisionHold:
		return slices.Contains(AudioApplyHoldReasons, o.Reason)
	}
	return false
}

// run applies the pass to items. A book's model is loaded only when one of
// its items is eligible and has a spelling core; a load error fails the run
// (no decision is made on a partial view of a book).
func (sp *spellingPass) run(ctx context.Context, items []*item) error {
	if sp == nil || sp.store == nil {
		return nil
	}
	// Findings this run rejects (on p or at rung 0), on this page or an
	// earlier one, do not make a group contradictory: their targets are
	// excluded from the run's view.
	for _, it := range items {
		if it.outcome.Decision == DecisionReject || (it.outcome.Decision != DecisionDuplicate && !it.outcome.Rung0.Pass) {
			sp.dead[it.finding.ID] = true
		}
	}
	for _, it := range items {
		if !spellingEligible(it.outcome) {
			continue
		}
		if _, _, ok := spelling.Core(it.finding.Original, it.finding.Replacement); !ok {
			continue
		}
		b, err := sp.book(ctx, filepath.Dir(it.finding.FilePath))
		if err != nil {
			return err
		}
		applySpelling(b, it, sp.dead)
	}
	return nil
}

// book is the cached model of dir, loading it on first use.
func (sp *spellingPass) book(ctx context.Context, dir string) (*spelling.Book, error) {
	if b, ok := sp.books[dir]; ok {
		return b, nil
	}
	b, err := sp.load(ctx, dir)
	if errors.Is(err, db.ErrSpellingCapExceeded) {
		spellingLog.Warn("spelling pass skipped for a book over a read cap", "book", dir, "error", err)
		sp.skipped++
		sp.books[dir] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sp.books[dir] = b
	return b, nil
}

// load builds the model of dir from the store.
func (sp *spellingPass) load(ctx context.Context, dir string) (*spelling.Book, error) {
	rows, err := sp.store.SpellingFindings(ctx, dir)
	if err != nil {
		return nil, err
	}
	fs := make([]spelling.Finding, len(rows))
	for i, r := range rows {
		fs[i] = spelling.Finding{ID: r.ID, Original: r.Original, Replacement: r.Replacement, State: r.State}
	}
	in := spelling.Input{Findings: fs, Pilots: map[string]spelling.Counts{}, Production: spelling.Counts{}}
	if want := spelling.CoreSpellings(fs); len(want) > 0 {
		set := make(map[string]bool, len(want))
		for _, w := range want {
			set[w] = true
		}
		if err := sp.store.StreamPilotText(ctx, dir, func(model, text string) error {
			c := in.Pilots[model]
			if c == nil {
				c = spelling.Counts{}
				in.Pilots[model] = c
			}
			c.Add(text, set)
			return nil
		}); err != nil {
			return nil, err
		}
		if err := sp.store.StreamProductionText(ctx, dir, func(text string) error {
			in.Production.Add(text, set)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return spelling.Build(in), nil
}

// spellingDetail is finding_events.detail on an outcome the pass looked at.
type spellingDetail struct {
	Rung           string                             `json:"rung"`
	Rule           string                             `json:"rule"`
	Group          []string                           `json:"group"`
	SpellingsTotal int                                `json:"spellings_total"`
	Targets        []string                           `json:"targets"`
	Original       string                             `json:"original"`
	Target         string                             `json:"target"`
	Contradictory  bool                               `json:"contradictory"`
	Strong         string                             `json:"strong"`
	StrongSource   string                             `json:"strong_source"`
	Counts         map[string]spelling.SpellingCounts `json:"counts"`
	// Action is what this pass did (kept, promoted, held_*), recorded at
	// the pass's step only: resolveConflicts runs after it and may still
	// turn the outcome into an overlap_dup reject or an overlap_pending
	// hold, which the event's outcome and reason show and Action does not.
	Action      string `json:"action"`
	PriorReason string `json:"prior_reason,omitempty"`
}

// applySpelling re-labels one eligible item by its book's spelling group and
// records the group in its detail. An item with no group is untouched. dead
// is the run's rejected findings, left out of the contradiction.
func applySpelling(b *spelling.Book, it *item, dead map[string]bool) {
	g, oc, rc, ok := b.Lookup(it.finding.Original, it.finding.Replacement)
	if !ok {
		return
	}
	targets, contradictory := g.Effective(dead)
	o := &it.outcome
	prior := o.Reason
	action := spellingKept
	switch {
	case g.Strong != "" && rc == g.Strong:
		if o.Decision == DecisionHold && o.Reason == ReasonNoEvidence && g.PromotedFrom(oc) {
			o.Decision, o.Reason, o.Retryable = DecisionApply, ReasonSpellingEstablished, false
			action = spellingPromoted
		}
	case g.Strong != "":
		o.Decision, o.Reason, o.Retryable = DecisionHold, ReasonSpellingOffEstablished, false
		action = spellingHeldOff
	case contradictory:
		o.Decision, o.Reason, o.Retryable = DecisionHold, ReasonSpellingConflict, false
		action = spellingHeldConflict
	}
	d := spellingDetail{
		Rung: spelling.DetailRung, Rule: spelling.RuleID,
		Group: detailSpellings(g, targets, oc, rc), SpellingsTotal: len(g.Spellings),
		Targets: capped(targets), Original: oc, Target: rc, Contradictory: contradictory,
		Strong: g.Strong, StrongSource: g.StrongSource, Action: action,
		Counts: map[string]spelling.SpellingCounts{},
	}
	if action != spellingKept {
		d.PriorReason = prior
	}
	for _, s := range d.Group {
		d.Counts[s] = g.Counts[s]
	}
	raw, err := json.Marshal(d)
	if err != nil {
		// A marshal of plain strings and ints cannot fail; if it ever did,
		// the decision stands and the event simply carries no detail.
		return
	}
	o.Detail = raw
}

// detailSpellings is the group's spellings for an event's detail, at most
// maxDetailSpellings: the finding's own two, the strong one and the targets
// first, then the rest in order.
func detailSpellings(g *spelling.Group, targets []string, oc, rc string) []string {
	var out []string
	add := func(s string) {
		if s != "" && len(out) < maxDetailSpellings && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	add(oc)
	add(rc)
	add(g.Strong)
	for _, s := range targets {
		add(s)
	}
	for _, s := range g.Spellings {
		add(s)
	}
	slices.Sort(out)
	return out
}

func capped(ss []string) []string {
	if len(ss) > maxDetailSpellings {
		return slices.Clone(ss[:maxDetailSpellings])
	}
	return slices.Clone(ss)
}

// EstablishedSpellings is the strong spellings of the book in dir
// (spelling.Book.EstablishedSpellings), for a context consumer — e.g. a
// future prompt that names a book's settled spellings. Nothing in decide
// feeds it to a model: the should_apply state never states evidence.
func EstablishedSpellings(ctx context.Context, store SpellingStore, dir string) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("established spellings of %s: no store", dir)
	}
	b, err := newSpellingPass(store).load(ctx, dir)
	if err != nil {
		return nil, err
	}
	return b.EstablishedSpellings(), nil
}
