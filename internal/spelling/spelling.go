// Package spelling is the "one spelling per book" model behind the decide
// step's spelling pass (CONTRACT §2.19 "One spelling per book").
//
// A book is spelt one way. When the judge proposes "terrillyn → terrin" in one
// chunk and "terrillyn → terrilyn" in another, at most one of them is right,
// and applying both leaves the book spelling the same name two ways. This
// package groups a book's findings by sound and says, per group, which
// spellings are in play, whether the findings contradict each other, and
// whether one spelling is established strongly enough to decide against.
//
// It works only from what the pipeline already has — the findings and the
// transcripts (production and second-ASR pilot). There is deliberately no
// name list, no name extraction and no word-type classification: a group is
// any set of findings whose differing words sound alike.
//
// Everything here is pure: no I/O, no clock, no randomness, no state beyond
// the Book a caller builds. internal/db loads the inputs; internal/decide
// applies the outcome.
package spelling

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/jedwards1230/earmark/internal/phonetic"
)

// Identity of the rule, recorded in the decide recipe's params and in every
// decision event detail the pass writes. Bump RuleID with any change here that
// can change a group, a contradiction or a strong spelling.
const (
	// RuleID names the rule set (decide recipe param spelling_rule).
	RuleID = "one_spelling@v1"
	// DetailRung is finding_events.detail.rung on the pass's events.
	DetailRung = "spelling"
	// MinPilot is the count a spelling needs in each of at least two pilot
	// models' transcripts of the book to be established by the pilots
	// (decide recipe param spelling_min_pilot).
	MinPilot = 3
	// MaxCoreTokens is the most words either side of a finding's differing
	// core may have for the finding to join a group.
	MaxCoreTokens = 2
)

// Sources of a strong spelling (Group.StrongSource).
const (
	// SourceApplied — the single spelling applied findings of the group
	// target.
	SourceApplied = "applied"
	// SourcePilots — the only group spelling the pilot transcripts contain,
	// at least MinPilot times in each of two or more models.
	SourcePilots = "pilots"
)

// Finding states the model reads (patch_state). Anything else is ignored.
const (
	StateProposed = "proposed"
	StateAccepted = "accepted"
	StateApplied  = "applied"
)

// Keyer maps a finding's core (one or two lower-case words) to the phonetic
// key findings are grouped on: two cores with the same key join the same
// group.
type Keyer interface {
	Key(core string) string
}

// MetaphoneKeyer is the default Keyer: the unbounded Double Metaphone primary
// code of the core's letters (any script; Double Metaphone voices only Latin
// letters), spaces dropped (so "terri lynn" and "terrilyn"
// key alike, as phonetic.Compare treats a split). A shared phonetic-key helper
// in internal/phonetic can replace it once a second caller needs one.
type MetaphoneKeyer struct{}

// Key implements Keyer.
func (MetaphoneKeyer) Key(core string) string {
	var b strings.Builder
	for _, r := range core {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return phonetic.DoubleMetaphone(b.String(), 0).Primary
}

var tokenRe = regexp.MustCompile(`[\p{L}\p{N}]+(?:['’]\p{L}+)*`)

// Tokens is s as lower-case word tokens (Unicode letters and digits),
// apostrophes normalised to ' and a possessive 's dropped ("Schaffa’s" →
// "schaffa", "Ærin" → "ærin").
func Tokens(s string) []string {
	out := tokenRe.FindAllString(strings.ToLower(s), -1)
	for i, t := range out {
		t = strings.ReplaceAll(t, "’", "'")
		out[i] = strings.TrimSuffix(t, "'s")
	}
	return out
}

// Core is the differing words of a finding: the original's and the
// replacement's tokens with their common leading and trailing tokens dropped
// (decide's diffWindow). ok is true only when each side is 1–MaxCoreTokens
// tokens, the sides differ, and they sound alike
// (phonetic.DefaultSoundAlikeThreshold) — a spelling, not a rewording.
func Core(original, replacement string) (oc, rc string, ok bool) {
	a, b := Tokens(original), Tokens(replacement)
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ac, bc := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(ac) < 1 || len(bc) < 1 || len(ac) > MaxCoreTokens || len(bc) > MaxCoreTokens {
		return "", "", false
	}
	oc, rc = strings.Join(ac, " "), strings.Join(bc, " ")
	if oc == rc {
		return "", "", false
	}
	if pass, _ := phonetic.SoundAlike(oc, rc, phonetic.DefaultSoundAlikeThreshold); !pass {
		return "", "", false
	}
	return oc, rc, true
}

// Counts is how often each spelling (a token, or two tokens joined by one
// space) occurs in a text.
type Counts map[string]int

// Add counts the unigrams and bigrams of text that are in want (nil = all).
// The text itself is not kept.
func (c Counts) Add(text string, want map[string]bool) {
	ts := Tokens(text)
	var firsts map[string]bool
	if want != nil {
		firsts = map[string]bool{}
		for w := range want {
			if f, _, two := strings.Cut(w, " "); two {
				firsts[f] = true
			}
		}
	}
	for i, t := range ts {
		if want == nil || want[t] {
			c[t]++
		}
		if i+1 < len(ts) && (want == nil || firsts[t]) {
			if bg := t + " " + ts[i+1]; want == nil || want[bg] {
				c[bg]++
			}
		}
	}
}

// Finding is one of the book's live findings the model is built from.
type Finding struct {
	ID          string
	Original    string
	Replacement string
	// State is StateProposed, StateAccepted or StateApplied.
	State string
}

// CoreSpellings is every spelling the findings' cores name (sorted): the
// tokens a loader needs to count.
func CoreSpellings(fs []Finding) []string {
	set := map[string]bool{}
	for _, f := range fs {
		if !liveState(f.State) {
			continue
		}
		if oc, rc, ok := Core(f.Original, f.Replacement); ok {
			set[oc], set[rc] = true, true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func liveState(s string) bool { return s == StateProposed || s == StateAccepted || s == StateApplied }

// SpellingCounts is the evidence recorded for one spelling of a group.
type SpellingCounts struct {
	// Applied is how many applied findings of the group target it.
	Applied int `json:"applied"`
	// Pilots is its count in each pilot model's transcripts of the book
	// (model id → count; models with a zero count are omitted).
	Pilots map[string]int `json:"pilots"`
	// Production is its count in the production transcript. Recorded only:
	// the production text is what is being corrected, so it never decides.
	Production int `json:"production"`
}

// Member is one finding of a group, by its cores.
type Member struct {
	ID       string
	Original string // the original's core
	Target   string // the replacement's core
	State    string
}

// Group is one set of findings whose cores sound alike (connected through
// their phonetic keys).
type Group struct {
	// Spellings are the cores of the group's findings, both sides, sorted.
	Spellings []string
	// Targets are the replacement cores (sorted); AppliedTargets those of the
	// applied findings alone.
	Targets        []string
	AppliedTargets []string
	// Contradictory: two or more distinct targets, or a reversed pair (one
	// finding A → B, another B → A).
	Contradictory bool
	// Strong is the established spelling ("" = none) and StrongSource how it
	// was established (SourceApplied or SourcePilots).
	Strong       string
	StrongSource string
	// Counts is the evidence per spelling.
	Counts map[string]SpellingCounts
	// Findings is how many findings the group holds; Members are they.
	Findings int
	Members  []Member
}

// PromotedFrom reports whether the group's strong spelling was established by
// an applied finding of exactly this pair — original core oc → the strong
// spelling. A shared phonetic key is not enough: "right", "write", "rot" and
// "root" all key alike, and "rot → write" must not be applied because
// "right → write" was.
func (g *Group) PromotedFrom(oc string) bool {
	if g == nil || g.Strong == "" || g.StrongSource != SourceApplied {
		return false
	}
	for _, m := range g.Members {
		if m.State == StateApplied && m.Original == oc && m.Target == g.Strong {
			return true
		}
	}
	return false
}

// Effective is the group's targets and contradiction flag counting only the
// members not in excluded (finding id → true): a caller removes the findings
// it has itself rejected, so a junk target alone cannot make a group
// contradictory. With a nil excluded it is Targets and Contradictory.
func (g *Group) Effective(excluded map[string]bool) (targets []string, contradictory bool) {
	set := map[string]bool{}
	edges := map[[2]string]bool{}
	for _, m := range g.Members {
		if excluded[m.ID] {
			continue
		}
		set[m.Target] = true
		edges[[2]string{m.Original, m.Target}] = true
	}
	contradictory = len(set) > 1
	for e := range edges {
		if edges[[2]string{e[1], e[0]}] {
			contradictory = true
		}
	}
	return sortedKeys(set), contradictory
}

// Input is what a Book is built from.
type Input struct {
	// Findings are the book's proposed, accepted and applied findings.
	Findings []Finding
	// Pilots are per pilot model (model id) the spelling counts of its
	// transcripts of the book; Production the production transcript's.
	Pilots     map[string]Counts
	Production Counts
	// Keyer groups the cores (nil = MetaphoneKeyer).
	Keyer Keyer
	// MinPilot overrides the MinPilot constant (0 = MinPilot).
	MinPilot int
}

// Book is one book's spelling model.
type Book struct {
	keyer  Keyer
	parent map[string]string // union-find while building
	root   map[string]string // key → its group's root key, once built
	groups map[string]*Group // by root key
}

// Build groups the findings (union-find over the phonetic keys of each
// finding's two cores) and settles each group's contradiction flag, counts
// and strong spelling.
func Build(in Input) *Book {
	k := in.Keyer
	if k == nil {
		k = MetaphoneKeyer{}
	}
	minPilot := in.MinPilot
	if minPilot <= 0 {
		minPilot = MinPilot
	}
	b := &Book{keyer: k, parent: map[string]string{}, groups: map[string]*Group{}}
	type pair struct{ id, oc, rc, state string }
	var pairs []pair
	for _, f := range in.Findings {
		if !liveState(f.State) {
			continue
		}
		oc, rc, ok := Core(f.Original, f.Replacement)
		if !ok {
			continue
		}
		b.union(b.key(oc), b.key(rc))
		pairs = append(pairs, pair{f.ID, oc, rc, f.State})
	}
	type acc struct {
		spellings, targets, applied map[string]bool
		edges                       map[[2]string]bool
		appliedN                    map[string]int
		n                           int
		members                     []Member
	}
	accs := map[string]*acc{}
	for _, p := range pairs {
		r := b.find(b.key(p.oc))
		a := accs[r]
		if a == nil {
			a = &acc{spellings: map[string]bool{}, targets: map[string]bool{}, applied: map[string]bool{},
				edges: map[[2]string]bool{}, appliedN: map[string]int{}}
			accs[r] = a
		}
		a.n++
		a.members = append(a.members, Member{ID: p.id, Original: p.oc, Target: p.rc, State: p.state})
		a.spellings[p.oc], a.spellings[p.rc] = true, true
		a.targets[p.rc] = true
		a.edges[[2]string{p.oc, p.rc}] = true
		if p.state == StateApplied {
			a.applied[p.rc] = true
			a.appliedN[p.rc]++
		}
	}
	for r, a := range accs {
		g := &Group{
			Spellings: sortedKeys(a.spellings), Targets: sortedKeys(a.targets), AppliedTargets: sortedKeys(a.applied),
			Counts: map[string]SpellingCounts{}, Findings: a.n, Members: a.members,
		}
		g.Contradictory = len(a.targets) > 1
		for e := range a.edges {
			if a.edges[[2]string{e[1], e[0]}] {
				g.Contradictory = true
			}
		}
		for _, s := range g.Spellings {
			sc := SpellingCounts{Applied: a.appliedN[s], Pilots: map[string]int{}, Production: in.Production[s]}
			for m, c := range in.Pilots {
				if n := c[s]; n > 0 {
					sc.Pilots[m] = n
				}
			}
			g.Counts[s] = sc
		}
		g.Strong, g.StrongSource = strong(g, minPilot)
		b.groups[r] = g
	}
	// Freeze the forest: Lookup reads root only, so a built Book is safe for
	// concurrent readers.
	b.root = make(map[string]string, len(b.parent))
	for x := range b.parent {
		b.root[x] = b.find(x)
	}
	b.parent = nil
	return b
}

// strong is the group's established spelling: the single target of its
// applied findings (none when they disagree); else the only spelling at least
// two pilot models each contain minPilot times, provided no other spelling of
// the group occurs in any pilot.
func strong(g *Group, minPilot int) (string, string) {
	switch len(g.AppliedTargets) {
	case 1:
		return g.AppliedTargets[0], SourceApplied
	case 0:
	default:
		return "", ""
	}
	for _, s := range g.Spellings {
		agree := 0
		for _, n := range g.Counts[s].Pilots {
			if n >= minPilot {
				agree++
			}
		}
		if agree < 2 {
			continue
		}
		competing := false
		for _, o := range g.Spellings {
			if o != s && len(g.Counts[o].Pilots) > 0 {
				competing = true
				break
			}
		}
		if !competing {
			// Every other spelling is absent from the pilots, so no second
			// spelling can qualify: this is the only one.
			return s, SourcePilots
		}
	}
	return "", ""
}

// Lookup is the group a finding belongs to and its cores. ok is false when the
// finding has no core (Core) or its cores are not grouped together in this
// book (it was not among the findings the book was built from).
func (b *Book) Lookup(original, replacement string) (g *Group, oc, rc string, ok bool) {
	if b == nil {
		return nil, "", "", false
	}
	oc, rc, ok = Core(original, replacement)
	if !ok {
		return nil, "", "", false
	}
	r, seen := b.root[b.key(oc)]
	if r2, seen2 := b.root[b.key(rc)]; !seen || !seen2 || r != r2 {
		return nil, "", "", false
	}
	g = b.groups[r]
	return g, oc, rc, g != nil
}

// Groups is every group of the book, ordered by first spelling.
func (b *Book) Groups() []*Group {
	if b == nil {
		return nil
	}
	out := make([]*Group, 0, len(b.groups))
	for _, g := range b.groups {
		out = append(out, g)
	}
	slices.SortFunc(out, func(x, y *Group) int { return strings.Compare(x.Spellings[0], y.Spellings[0]) })
	return out
}

// EstablishedSpellings is the book's strong spellings, sorted and distinct.
// It is provided for a future context consumer (e.g. a decide or judge
// prompt naming the book's settled spellings); nothing feeds it to a model
// yet — the should_apply state never states evidence (CONTRACT §2.19).
func (b *Book) EstablishedSpellings() []string {
	set := map[string]bool{}
	for _, g := range b.Groups() {
		if g.Strong != "" {
			set[g.Strong] = true
		}
	}
	return sortedKeys(set)
}

// key is the Keyer's key, or — when the core has no phonetic code (letters
// Double Metaphone does not voice) — a key unique to the core, so silent
// cores never merge unrelated groups.
func (b *Book) key(core string) string {
	if k := b.keyer.Key(core); k != "" {
		return k
	}
	return "\x00" + core
}

func (b *Book) find(x string) string {
	if _, ok := b.parent[x]; !ok {
		b.parent[x] = x
	}
	for b.parent[x] != x {
		b.parent[x] = b.parent[b.parent[x]]
		x = b.parent[x]
	}
	return x
}

func (b *Book) union(x, y string) {
	rx, ry := b.find(x), b.find(y)
	if rx != ry {
		b.parent[rx] = ry
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
