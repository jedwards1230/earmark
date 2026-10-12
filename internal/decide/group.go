package decide

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// Agreeing and conflicting findings (CONTRACT §2.19 "Agreeing and
// conflicting findings").
//
// Several recipes — a re-judge under a new model, an older recipe's held
// proposal — often propose the same fix to the same chunk. That is agreement,
// not a reason to reject: before rung 0's Dedupe, the findings of a chunk
// that resolve to the same span with the same correction (the stored
// correction_key, which already covers the chunk revision, the normalised
// original and the normalised correction) form a group. One member, the
// representative, is decided; every other proposed member is recorded as a
// duplicate of it (patch_state duplicate, duplicate_of) and is never applied.
// Different fixes to overlapping text are a conflict: rung 0 only marks them
// (Verdict.Conflicts), the model is asked about each, and RankConflicts keeps
// the highest p.

// Representative criteria, best first (repTiers): a group's representative
// is the member that wins on the first criterion the members differ on, then
// the newest. DupGroup.Tier names the criterion that decided it.
const (
	// TierApplied — accepted or applied already: the fix is in the overlay,
	// and re-deciding it is the re-check's job, not grouping's.
	TierApplied = "accepted/applied"
	// TierExisting — already a representative (some finding is a duplicate
	// of it) or already decided under this run's recipe. Keeps a re-run from
	// flip-flopping: the group it formed stays put.
	TierExisting = "existing representative"
	// TierRung0 — passes rung 0 where another member does not (the same fix
	// labelled with an issue type whose shape rung 0 cannot vet).
	TierRung0 = "passes rung 0"
	// TierCurrentRecipe — proposed by the current propose recipe.
	TierCurrentRecipe = "current propose recipe"
	// TierNewest — the most recently proposed (then the lowest id).
	TierNewest = "newest"
)

// DupGroup is one group of findings on a chunk that propose the same fix.
type DupGroup struct {
	// Rep is the representative's finding id, RepState its patch_state, and
	// Tier why it was chosen (Tier* constants).
	Rep      string `json:"rep"`
	RepState string `json:"rep_state"`
	Tier     string `json:"tier"`
	// Members are the findings recorded as duplicates of Rep, sorted.
	Members     []string `json:"members"`
	Original    string   `json:"original"`
	Replacement string   `json:"replacement"`
}

// groupMember is one finding of a chunk as grouping sees it.
type groupMember struct {
	f       db.DecideFinding
	span    patch.Span
	overlay bool // accepted or applied
	pass    bool // passes rung 0's Check (overlay members: true)
}

// spanlessReasons are the rung-0 failures that leave a finding without a
// trustworthy span on the chunk: it cannot be compared with anything.
var spanlessReasons = map[string]bool{
	ReasonChunkChanged: true, ReasonAnchorMissing: true, ReasonNotWordBounded: true,
}

// groupable reports whether a candidate's verdict places it on the chunk:
// passed, or failed only on what it claims (shape, issue type, cosmetic).
// A cosmetic_only edit is no fix at all (a case-only change or a no-op), so
// it agrees with nothing.
func groupable(v Verdict) bool {
	return v.Span.End > v.Span.Start && v.Reason != ReasonCosmeticOnly && (v.Pass || !spanlessReasons[v.Reason])
}

// exactCorrection is the replacement as grouping compares it on top of
// correction_key: whitespace runs collapsed and trimmed like the key, but
// case kept. correction_key folds case, and "Frodo" and "frodo" are
// different corrections — a capitalised fix the decide policy may hold must
// not swallow a lowercase one as its duplicate. They conflict instead and
// are ranked by p.
func exactCorrection(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' }), " ")
}

// repTiers are the representative criteria, best first, as booleans
// (true is better), in the order of tierNames.
func repTiers(m groupMember) [4]bool {
	return [4]bool{
		m.overlay,
		m.f.HasDuplicates || m.f.DecidedHere,
		m.pass,
		m.f.CurrentPropose,
	}
}

var tierNames = [4]string{TierApplied, TierExisting, TierRung0, TierCurrentRecipe}

// betterRep orders two members for representative: the criteria of
// repTiers in order, then the newest, then the lowest id.
func betterRep(a, b groupMember) bool {
	ta, tb := repTiers(a), repTiers(b)
	for i := range ta {
		if ta[i] != tb[i] {
			return ta[i]
		}
	}
	if !a.f.CreatedAt.Equal(b.f.CreatedAt) {
		return a.f.CreatedAt.After(b.f.CreatedAt)
	}
	return a.f.ID < b.f.ID
}

// tierOf names the criterion that made rep the representative over the
// runner-up (the best of the rest): the first one they differ on, else
// TierNewest (by time, then id).
func tierOf(rep, runnerUp groupMember) string {
	tr, tu := repTiers(rep), repTiers(runnerUp)
	for i := range tr {
		if tr[i] != tu[i] {
			return tierNames[i]
		}
	}
	return TierNewest
}

// groupChunk groups one chunk's agreeing findings. cands are the chunk's
// candidates (the run's findings and the chunk's other proposed findings)
// with their Check verdicts; overlay are its accepted/applied findings.
// Members are the overlay findings whose correction resolves on the chunk
// text, and the proposed candidates that groupable places; a candidate that
// is neither (a calibration run's human-decided finding) takes no part. Two
// members agree when they have the same correction_key, the same exact
// correction (exactCorrection: case kept) and the same span.
//
// In a group of two or more, the best member by repTier is the
// representative and every other PROPOSED member is a duplicate of it (dupOf:
// member → representative). Overlay members are never moved. Groups are
// returned in representative-id order, members sorted. Pure.
func groupChunk(text string, cands []db.DecideFinding, verdicts map[string]Verdict,
	overlay []db.DecideFinding) (dupOf map[string]string, groups []DupGroup) {
	type key struct {
		ck, repl   string
		start, end int
	}
	byKey := map[key][]groupMember{}
	var order []key
	seen := map[string]bool{}
	add := func(k key, m groupMember) {
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], m)
		seen[m.f.ID] = true
	}
	hash := patch.ChunkHash(text)
	for _, f := range overlay {
		if f.CorrectionKey == "" || strings.TrimSpace(f.Replacement) == "" || f.ChunkTextSHA256 != hash {
			continue
		}
		span, err := patch.Locate(text, patch.Anchor{OriginalText: f.Original, Offset: f.AnchorOffset, Occurrence: f.AnchorOccurrence})
		if err != nil {
			continue
		}
		add(key{f.CorrectionKey, exactCorrection(f.Replacement), span.Start, span.End},
			groupMember{f: f, span: span, overlay: true, pass: true})
	}
	for _, f := range cands {
		v, ok := verdicts[f.ID]
		if seen[f.ID] || !ok || f.PatchState != patch.StateProposed || f.CorrectionKey == "" || !groupable(v) {
			continue
		}
		add(key{f.CorrectionKey, exactCorrection(f.Replacement), v.Span.Start, v.Span.End},
			groupMember{f: f, span: v.Span, pass: v.Pass})
	}

	dupOf = map[string]string{}
	for _, k := range order {
		ms := byKey[k]
		if len(ms) < 2 {
			continue
		}
		best := 0
		for i := 1; i < len(ms); i++ {
			if betterRep(ms[i], ms[best]) {
				best = i
			}
		}
		rep := ms[best]
		runnerUp := -1
		for i := range ms {
			if i != best && (runnerUp < 0 || betterRep(ms[i], ms[runnerUp])) {
				runnerUp = i
			}
		}
		tier := tierOf(rep, ms[runnerUp])
		g := DupGroup{Rep: rep.f.ID, RepState: rep.f.PatchState, Tier: tier,
			Original: rep.f.Original, Replacement: rep.f.Replacement}
		for i, m := range ms {
			if i == best || m.overlay {
				continue
			}
			dupOf[m.f.ID] = rep.f.ID
			g.Members = append(g.Members, m.f.ID)
		}
		if len(g.Members) == 0 {
			continue // only overlay rows agree: nothing to record
		}
		slices.Sort(g.Members)
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b DupGroup) int { return strings.Compare(a.Rep, b.Rep) })
	return dupOf, groups
}

// duplicateOutcome is the outcome of a finding grouped as a duplicate of rep:
// no model call, no evidence, never applied.
func duplicateOutcome(f db.DecideFinding, v Verdict, rep string) Outcome {
	return Outcome{FindingID: f.ID, Decision: DecisionDuplicate, Reason: ReasonDuplicate, Related: rep,
		Rung0: v, ChunkHash: f.ChunkTextSHA256, Evidence: EvidenceNone}
}

// Grouping reports what a run did with agreeing and conflicting findings.
type Grouping struct {
	// Groups counts groups of findings proposing the same fix that hold a
	// finding of the run; Duplicates the findings recorded (or, in a dry run,
	// that would be recorded) as a duplicate of their group's representative.
	Groups     int `json:"groups"`
	Duplicates int `json:"duplicates"`
	// Conflicts counts sets of overlapping, different fixes the model ranked;
	// Ranked their members (each asked); Losers the members rejected
	// overlap_dup (their winner applies); Unranked the sets left unranked because a member got no
	// usable p (nothing in them is applied; they are decided again).
	Conflicts int `json:"conflicts"`
	Ranked    int `json:"ranked"`
	Losers    int `json:"losers"`
	// Pending counts losers held overlap_pending because their winner is
	// held, not applied.
	Pending  int `json:"pending"`
	Unranked int `json:"unranked"`
	// GroupLines and ConflictLines describe each group and each ranked set,
	// one line apiece (dry run only).
	GroupLines    []string `json:"group_lines,omitempty"`
	ConflictLines []string `json:"conflict_lines,omitempty"`
}

// addGroups counts groups (and, with lines, describes them).
func (g *Grouping) addGroups(gs []DupGroup, lines bool) {
	for _, d := range gs {
		g.Groups++
		g.Duplicates += len(d.Members)
		if lines {
			short := make([]string, len(d.Members))
			for i, m := range d.Members {
				short[i] = shortID(m)
			}
			g.GroupLines = append(g.GroupLines, fmt.Sprintf("%s [%s · %s] %q → %q · duplicates: %s",
				shortID(d.Rep), d.RepState, d.Tier, d.Original, d.Replacement, strings.Join(short, ", ")))
		}
	}
}

// resolveConflicts ranks every conflict among items by p (RankConflicts),
// after evaluation. Each set is the connected component of Verdict.Conflicts.
// Only members the model did not reject on their own p are ranked: a member
// rejected by the decision rule keeps its reject and cannot win, so the
// survivors are ranked among themselves (two overlapping fixes are never
// both kept because a third, rejected one sat between them). A loser's
// outcome follows its winner's (CONTRACT §2.19): reject overlap_dup naming
// the winner when the winner applies — written only if the winner's accept is
// written in the same transaction, else held overlap_pending
// (db.DecisionEvent.RequiresAccepted) — and hold overlap_pending naming it
// while the winner is held. Its p, evidence and call stay on the record. A set with a member that got no
// usable p is unranked: any apply in it becomes a retryable hold. Items not
// in a conflict are untouched; so are rung-0-only runs (no p), which the
// caller does not pass here.
func resolveConflicts(items []*item, g *Grouping, lines bool) {
	byID := map[string]*item{}
	parent := map[string]string{}
	find := func(x string) string {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for _, it := range items {
		if len(it.outcome.Rung0.Conflicts) == 0 || !it.outcome.Rung0.Pass {
			continue
		}
		byID[it.finding.ID] = it
		parent[it.finding.ID] = it.finding.ID
	}
	for id, it := range byID {
		for _, o := range it.outcome.Rung0.Conflicts {
			if _, ok := byID[o]; ok {
				parent[find(id)] = find(o)
			}
		}
	}
	comps := map[string][]*item{}
	for id, it := range byID {
		r := find(id)
		comps[r] = append(comps[r], it)
	}
	roots := make([]string, 0, len(comps))
	for r := range comps {
		roots = append(roots, r)
	}
	slices.Sort(roots)
	for _, r := range roots {
		set := comps[r]
		if len(set) < 2 {
			continue
		}
		slices.SortFunc(set, func(a, b *item) int { return strings.Compare(a.finding.ID, b.finding.ID) })
		ok := true
		var rs []Ranked
		for _, it := range set {
			if it.outcome.P == nil {
				ok = false
			}
			if it.outcome.Decision == DecisionReject {
				continue // rejected on its own p: neither wins nor loses
			}
			rs = append(rs, Ranked{FindingID: it.finding.ID, ChunkText: it.input.Candidate.ChunkText,
				Span: it.outcome.Rung0.Span, P: it.outcome.P, Confidence: it.finding.Confidence})
		}
		var losers map[string]string
		if ok {
			losers, ok = RankConflicts(rs)
		}
		g.Conflicts++
		g.Ranked += len(set)
		if !ok {
			g.Unranked++
			for _, it := range set {
				if it.outcome.Decision == DecisionApply {
					it.outcome.Decision, it.outcome.Reason, it.outcome.Retryable = DecisionHold, ReasonJevUnavailable, true
				}
			}
			if lines {
				g.ConflictLines = append(g.ConflictLines, conflictLine(set, nil, false))
			}
			continue
		}
		winners := map[string]*item{}
		for _, it := range set {
			winners[it.finding.ID] = it
		}
		for _, it := range set {
			w, lost := losers[it.finding.ID]
			if !lost {
				continue
			}
			switch winners[w].outcome.Decision {
			case DecisionApply:
				// The winner's fix goes in: the loser is rejected for it.
				it.outcome.Decision, it.outcome.Reason, it.outcome.Retryable = DecisionReject, ReasonOverlapDup, false
				it.outcome.Related = w
				g.Losers++
			case DecisionHold:
				// The winner is held: nothing is applied over the loser yet,
				// and rejecting it now would lose it if the winner never
				// applies. Held, naming the winner; decided again once the
				// winner leaves proposed (decideWorkSQL).
				it.outcome.Decision, it.outcome.Reason, it.outcome.Retryable = DecisionHold, ReasonOverlapPending, false
				it.outcome.Related = w
				g.Pending++
			}
		}
		if lines {
			g.ConflictLines = append(g.ConflictLines, conflictLine(set, losers, true))
		}
	}
}

// conflictLine describes one ranked set, highest p first.
func conflictLine(set []*item, losers map[string]string, ranked bool) string {
	order := slices.Clone(set)
	slices.SortStableFunc(order, func(a, b *item) int {
		pa, pb := -1.0, -1.0
		if a.outcome.P != nil {
			pa = *a.outcome.P
		}
		if b.outcome.P != nil {
			pb = *b.outcome.P
		}
		switch {
		case pa > pb:
			return -1
		case pa < pb:
			return 1
		}
		return 0
	})
	parts := make([]string, len(order))
	for i, it := range order {
		p := "p n/a"
		if it.outcome.P != nil {
			p = fmt.Sprintf("p %.2f", *it.outcome.P)
		}
		verdict := "kept"
		switch {
		case !ranked:
			verdict = "unranked"
		case losers[it.finding.ID] != "" && (it.outcome.Reason == ReasonOverlapDup || it.outcome.Reason == ReasonOverlapPending):
			verdict = it.outcome.Reason + " → " + shortID(losers[it.finding.ID])
		case it.outcome.Decision == DecisionReject:
			verdict = "rejected on its own p"
		}
		parts[i] = fmt.Sprintf("%s %q → %q %s %s", shortID(it.finding.ID), it.finding.Original, it.finding.Replacement, p, verdict)
	}
	return strings.Join(parts, " · ")
}

// shortID is the first 8 characters of a finding id.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
