package spelling

import (
	"slices"
	"testing"
)

func TestCore(t *testing.T) {
	for _, tc := range []struct {
		name, orig, repl string
		oc, rc           string
		ok               bool
	}{
		{"single word", "ilmus", "ilmas", "ilmus", "ilmas", true},
		{"shared context dropped", "said terrillyn quietly", "said Terrin quietly", "terrillyn", "terrin", true},
		{"possessive stripped", "Shafa’s hand", "Schaffa's hand", "shafa", "schaffa", true},
		{"two tokens to one", "terri lynn", "terrilyn", "terri lynn", "terrilyn", true},
		{"case only is no core", "Nassun", "nassun", "", "", false},
		{"three tokens too long", "a b c", "x y z", "", "", false},
		{"pure insertion has no core", "the cat", "the black cat", "", "", false},
		{"not sound alike", "dish", "telescope", "", "", false},
		{"empty", "", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oc, rc, ok := Core(tc.orig, tc.repl)
			if ok != tc.ok || oc != tc.oc || rc != tc.rc {
				t.Errorf("Core(%q, %q) = %q, %q, %v; want %q, %q, %v", tc.orig, tc.repl, oc, rc, ok, tc.oc, tc.rc, tc.ok)
			}
		})
	}
}

func TestCountsAdd(t *testing.T) {
	c := Counts{}
	c.Add("Terri Lynn said terri lynn's name; Terrilyn nodded.", map[string]bool{"terri lynn": true, "terrilyn": true})
	if c["terri lynn"] != 2 || c["terrilyn"] != 1 || len(c) != 2 {
		t.Errorf("counts %v", c)
	}
	all := Counts{}
	all.Add("a b a", nil)
	if all["a"] != 2 || all["a b"] != 1 || all["b a"] != 1 || all["b"] != 1 {
		t.Errorf("all counts %v", all)
	}
}

func f(id, orig, repl, state string) Finding {
	return Finding{ID: id, Original: orig, Replacement: repl, State: state}
}

func onlyGroup(t *testing.T, b *Book) *Group {
	t.Helper()
	gs := b.Groups()
	if len(gs) != 1 {
		var sp [][]string
		for _, g := range gs {
			sp = append(sp, g.Spellings)
		}
		t.Fatalf("%d groups %v, want 1", len(gs), sp)
	}
	return gs[0]
}

func TestBuildGroups(t *testing.T) {
	for _, tc := range []struct {
		name          string
		in            Input
		spellings     []string
		targets       []string
		contradictory bool
		strong        string
		source        string
	}{
		{
			name: "ilmus ilmas ilmis agree on one target",
			in: Input{Findings: []Finding{
				f("1", "ilmus", "ilmas", StateProposed),
				f("2", "ilmis", "ilmas", StateProposed),
			}},
			spellings: []string{"ilmas", "ilmis", "ilmus"}, targets: []string{"ilmas"},
		},
		{
			name: "ilmus split two ways is a contradiction",
			in: Input{Findings: []Finding{
				f("1", "ilmus", "ilmas", StateProposed),
				f("2", "ilmus", "ilmis", StateProposed),
			}},
			spellings: []string{"ilmas", "ilmis", "ilmus"}, targets: []string{"ilmas", "ilmis"}, contradictory: true,
		},
		{
			name: "terrillyn to terrin and terrilyn",
			in: Input{Findings: []Finding{
				f("1", "said terrillyn", "said terrin", StateProposed),
				f("2", "terrillyn nodded", "terrilyn nodded", StateProposed),
			}},
			spellings: []string{"terrillyn", "terrilyn", "terrin"}, targets: []string{"terrilyn", "terrin"}, contradictory: true,
		},
		{
			name: "reversed pair",
			in: Input{Findings: []Finding{
				f("1", "nasun", "nassun", StateProposed),
				f("2", "nassun", "nasun", StateAccepted),
			}},
			spellings: []string{"nassun", "nasun"}, targets: []string{"nassun", "nasun"}, contradictory: true,
		},
		{
			name: "applied target is strong",
			in: Input{Findings: []Finding{
				f("1", "ilmus", "ilmas", StateApplied),
				f("2", "ilmus", "ilmis", StateProposed),
			}},
			spellings: []string{"ilmas", "ilmis", "ilmus"}, targets: []string{"ilmas", "ilmis"}, contradictory: true,
			strong: "ilmas", source: SourceApplied,
		},
		{
			name: "applied targets disagree: no strong, even with pilots",
			in: Input{Findings: []Finding{
				f("1", "ilmus", "ilmas", StateApplied),
				f("2", "ilmus", "ilmis", StateApplied),
			}, Pilots: map[string]Counts{"g": {"ilmas": 9}, "c": {"ilmas": 9}}},
			spellings: []string{"ilmas", "ilmis", "ilmus"}, targets: []string{"ilmas", "ilmis"}, contradictory: true,
		},
		{
			name: "two pilots agree",
			in: Input{Findings: []Finding{
				f("1", "ilmus", "ilmas", StateProposed),
				f("2", "ilmus", "ilmis", StateProposed),
			}, Pilots: map[string]Counts{"g": {"ilmas": 3}, "c": {"ilmas": 5}}, Production: Counts{"ilmus": 7}},
			spellings: []string{"ilmas", "ilmis", "ilmus"}, targets: []string{"ilmas", "ilmis"}, contradictory: true,
			strong: "ilmas", source: SourcePilots,
		},
		{
			name: "one pilot below the minimum",
			in: Input{Findings: []Finding{f("1", "ilmus", "ilmas", StateProposed)},
				Pilots: map[string]Counts{"g": {"ilmas": 2}, "c": {"ilmas": 5}}},
			spellings: []string{"ilmas", "ilmus"}, targets: []string{"ilmas"},
		},
		{
			name: "a single pilot is not enough",
			in: Input{Findings: []Finding{f("1", "ilmus", "ilmas", StateProposed)},
				Pilots: map[string]Counts{"g": {"ilmas": 50}}},
			spellings: []string{"ilmas", "ilmus"}, targets: []string{"ilmas"},
		},
		{
			name: "a competing spelling in any pilot blocks",
			in: Input{Findings: []Finding{f("1", "ilmus", "ilmas", StateProposed)},
				Pilots: map[string]Counts{"g": {"ilmas": 9}, "c": {"ilmas": 9, "ilmus": 1}}},
			spellings: []string{"ilmas", "ilmus"}, targets: []string{"ilmas"},
		},
		{
			name: "production counts never decide",
			in: Input{Findings: []Finding{f("1", "ilmus", "ilmas", StateProposed)},
				Production: Counts{"ilmas": 99}},
			spellings: []string{"ilmas", "ilmus"}, targets: []string{"ilmas"},
		},
		{
			name: "min pilot override",
			in: Input{Findings: []Finding{f("1", "ilmus", "ilmas", StateProposed)},
				Pilots: map[string]Counts{"g": {"ilmas": 1}, "c": {"ilmas": 1}}, MinPilot: 1},
			spellings: []string{"ilmas", "ilmus"}, targets: []string{"ilmas"}, strong: "ilmas", source: SourcePilots,
		},
		{
			name: "rejected and other states are ignored",
			in: Input{Findings: []Finding{
				f("1", "ilmus", "ilmas", StateProposed),
				f("2", "ilmus", "ilmis", "rejected"),
				f("3", "ilmus", "ilmis", "duplicate"),
			}},
			spellings: []string{"ilmas", "ilmus"}, targets: []string{"ilmas"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := onlyGroup(t, Build(tc.in))
			if !slices.Equal(g.Spellings, tc.spellings) || !slices.Equal(g.Targets, tc.targets) {
				t.Errorf("spellings %v targets %v; want %v %v", g.Spellings, g.Targets, tc.spellings, tc.targets)
			}
			if g.Contradictory != tc.contradictory {
				t.Errorf("contradictory %v, want %v", g.Contradictory, tc.contradictory)
			}
			if g.Strong != tc.strong || g.StrongSource != tc.source {
				t.Errorf("strong %q (%s), want %q (%s)", g.Strong, g.StrongSource, tc.strong, tc.source)
			}
		})
	}
}

func TestBuildCounts(t *testing.T) {
	b := Build(Input{
		Findings: []Finding{f("1", "ilmus", "ilmas", StateApplied), f("2", "ilmus", "ilmas", StateApplied)},
		Pilots:   map[string]Counts{"g": {"ilmas": 4}, "c": {"ilmus": 0}},
		Production: Counts{
			"ilmus": 6,
		},
	})
	g := onlyGroup(t, b)
	got := g.Counts["ilmas"]
	if got.Applied != 2 || got.Pilots["g"] != 4 || len(got.Pilots) != 1 || got.Production != 0 {
		t.Errorf("ilmas counts %+v", got)
	}
	if c := g.Counts["ilmus"]; c.Production != 6 || len(c.Pilots) != 0 {
		t.Errorf("ilmus counts %+v", c)
	}
	if g.Findings != 2 {
		t.Errorf("findings %d", g.Findings)
	}
}

func TestSeparateGroups(t *testing.T) {
	b := Build(Input{Findings: []Finding{
		f("1", "ilmus", "ilmas", StateApplied),
		f("2", "terrillyn", "terrin", StateProposed),
		f("3", "dish", "telescope", StateProposed), // no core: not sound alike
	}, Pilots: map[string]Counts{"g": {"terrin": 3}, "c": {"terrin": 3}}})
	if n := len(b.Groups()); n != 2 {
		t.Fatalf("%d groups, want 2", n)
	}
	if got := b.EstablishedSpellings(); !slices.Equal(got, []string{"ilmas", "terrin"}) {
		t.Errorf("established %v", got)
	}
	g, oc, rc, ok := b.Lookup("ilmus", "ilmas")
	if !ok || oc != "ilmus" || rc != "ilmas" || g.Strong != "ilmas" {
		t.Errorf("lookup %v %q %q %v", g, oc, rc, ok)
	}
	if _, _, _, ok := b.Lookup("dish", "telescope"); ok {
		t.Error("a finding with no core has no group")
	}
	if _, _, _, ok := b.Lookup("ilmus", "terrin"); ok {
		t.Error("cores in different groups must not look up")
	}
	if _, _, _, ok := b.Lookup("zorbo", "zorba"); ok {
		t.Error("a pair the book was not built from has no group")
	}
	var nb *Book
	if _, _, _, ok := nb.Lookup("ilmus", "ilmas"); ok || nb.EstablishedSpellings() != nil && len(nb.EstablishedSpellings()) != 0 {
		t.Error("nil book")
	}
}

type constKeyer struct{}

func (constKeyer) Key(string) string { return "K" }

func TestKeyerSeam(t *testing.T) {
	b := Build(Input{Keyer: constKeyer{}, Findings: []Finding{
		f("1", "ilmus", "ilmas", StateProposed),
		f("2", "terrillyn", "terrin", StateProposed),
	}})
	if g := onlyGroup(t, b); !g.Contradictory || len(g.Spellings) != 4 {
		t.Errorf("a constant keyer merges everything: %+v", g)
	}
}

func TestMetaphoneKeyerSplitsAlike(t *testing.T) {
	k := MetaphoneKeyer{}
	if k.Key("terri lynn") != k.Key("terrilyn") {
		t.Errorf("split %q vs fused %q", k.Key("terri lynn"), k.Key("terrilyn"))
	}
	if k.Key("ilmus") != k.Key("ilmas") || k.Key("ilmus") != k.Key("ilmis") {
		t.Errorf("ilm* keys %q %q %q", k.Key("ilmus"), k.Key("ilmas"), k.Key("ilmis"))
	}
}

func TestSilentCoresDoNotMerge(t *testing.T) {
	b := Build(Input{Keyer: emptyKeyer{}, Findings: []Finding{
		f("1", "ilmus", "ilmas", StateProposed),
		f("2", "terrillyn", "terrin", StateProposed),
	}})
	if n := len(b.Groups()); n != 2 {
		t.Errorf("%d groups; cores with no key must stay apart", n)
	}
}

type emptyKeyer struct{}

func (emptyKeyer) Key(string) string { return "" }

func TestCoreSpellings(t *testing.T) {
	got := CoreSpellings([]Finding{
		f("1", "ilmus", "ilmas", StateProposed),
		f("2", "terri lynn", "terrilyn", StateApplied),
		f("3", "x", "y", "rejected"),
	})
	if !slices.Equal(got, []string{"ilmas", "ilmus", "terri lynn", "terrilyn"}) {
		t.Errorf("core spellings %v", got)
	}
}

func TestPromotedFromAndEffective(t *testing.T) {
	b := Build(Input{Findings: []Finding{
		f("x", "right", "write", StateApplied),
		f("r", "rot", "rat", StateProposed),
	}})
	g := onlyGroup(t, b)
	if g.Strong != "write" || !g.PromotedFrom("right") || g.PromotedFrom("rot") {
		t.Errorf("strong %q, promoted from right %v, rot %v", g.Strong, g.PromotedFrom("right"), g.PromotedFrom("rot"))
	}
	if ts, c := g.Effective(nil); !c || !slices.Equal(ts, []string{"rat", "write"}) {
		t.Errorf("effective %v %v", ts, c)
	}
	if ts, c := g.Effective(map[string]bool{"r": true}); c || !slices.Equal(ts, []string{"write"}) {
		t.Errorf("effective without r: %v %v", ts, c)
	}
	if got := Tokens("Ærin’s café, naïve"); !slices.Equal(got, []string{"ærin", "café", "naïve"}) {
		t.Errorf("unicode tokens %v", got)
	}
}
