package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/asrpilot"
	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
	"github.com/jedwards1230/earmark/internal/recipe"
)

// *db.DB is the rung's store.
var _ AudioVoteStore = (*db.DB)(nil)

// Fixture pilot models: opaque strings, not the deployment's pins.
const (
	avA, avRevA = "org-a/asr-a", "1111111111111111111111111111111111111111"
	avB, avRevB = "org-b/asr-b", "2222222222222222222222222222222222222222"
)

func avConfig() config.AudioVoteConfig {
	return config.AudioVoteConfig{Rule: asrpilot.VoteRule, Models: []config.AudioVoteModel{
		{ModelID: avB, Revision: avRevB}, {ModelID: avA, Revision: avRevA},
	}}
}

func avRung(t *testing.T) *AudioVoteRung {
	t.Helper()
	r, err := NewAudioVoteRung(avConfig())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func vote(id int64, model, rev, v string) db.ASRVote {
	return db.ASRVote{VoteID: id, ModelID: model, ModelRevision: rev, PilotTranscriptID: id * 10, Vote: v,
		UpdatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
}

func pf(f float64) *float64 { return &f }

// TestAudioVoteEvaluate is the rung's decision table.
func TestAudioVoteEvaluate(t *testing.T) {
	const (
		R  = asrpilot.VoteHeardReplacement
		O  = asrpilot.VoteHeardOriginal
		N  = asrpilot.VoteNeither
		U  = asrpilot.VoteUnmappable
		S  = asrpilot.VoteStyleOnly
		id = "00000000-0000-0000-0000-0000000000f1"
	)
	pass := Verdict{Pass: true, Span: patch.Span{Start: 1, End: 3}}
	both := func(a, b string) []db.ASRVote { return []db.ASRVote{vote(1, avA, avRevA, a), vote(2, avB, avRevB, b)} }
	base := AudioVoteInput{FindingID: id, IssueType: IssueMisheardWord, ChunkHash: "h", Rung0: pass,
		ConflictsLost: true, HoldReason: ReasonUncertain, HoldP: pf(0.5)}
	tests := []struct {
		name             string
		mut              func(*AudioVoteInput)
		decision, reason string
	}{
		{"both replacement → apply", func(in *AudioVoteInput) { in.Votes = both(R, R) }, DecisionApply, ReasonASRAgree},
		{"both original → reject", func(in *AudioVoteInput) { in.Votes = both(O, O) }, DecisionReject, ReasonASRHeardOriginal},
		{"split → hold", func(in *AudioVoteInput) { in.Votes = both(R, O) }, DecisionHold, ReasonASRSplit},
		{"one missing → hold", func(in *AudioVoteInput) { in.Votes = both(R, R)[:1] }, DecisionHold, ReasonASRVoteMissing},
		{"none → hold", func(*AudioVoteInput) {}, DecisionHold, ReasonASRVoteMissing},
		{"missing beats original", func(in *AudioVoteInput) { in.Votes = both(O, O)[1:] }, DecisionHold, ReasonASRVoteMissing},
		{"other revision is no vote", func(in *AudioVoteInput) {
			in.Votes = []db.ASRVote{vote(1, avA, avRevA, R), vote(2, avB, "3333", R)}
		}, DecisionHold, ReasonASRVoteMissing},
		{"other model is no vote", func(in *AudioVoteInput) {
			in.Votes = []db.ASRVote{vote(1, avA, avRevA, R), vote(2, "org-c/asr-c", avRevB, R)}
		}, DecisionHold, ReasonASRVoteMissing},
		{"neither → hold", func(in *AudioVoteInput) { in.Votes = both(R, N) }, DecisionHold, ReasonASRNoAgreement},
		{"unmappable → hold", func(in *AudioVoteInput) { in.Votes = both(U, R) }, DecisionHold, ReasonASRNoAgreement},
		{"style_only → hold", func(in *AudioVoteInput) { in.Votes = both(S, S) }, DecisionHold, ReasonASRNoAgreement},
		{"original + neither → hold", func(in *AudioVoteInput) { in.Votes = both(O, N) }, DecisionHold, ReasonASRNoAgreement},
		{"rung-0 reject wins over both replacement", func(in *AudioVoteInput) {
			in.Votes, in.Rung0 = both(R, R), Verdict{Reason: ReasonOverlapsOverlay, Span: pass.Span}
		}, DecisionReject, ReasonOverlapsOverlay},
		{"rung-0 reject wins over missing votes", func(in *AudioVoteInput) {
			in.Rung0 = Verdict{Reason: ReasonNotSoundAlike}
		}, DecisionReject, ReasonNotSoundAlike},
		{"chunk changed is a reanchor reject", func(in *AudioVoteInput) {
			in.Votes, in.Rung0 = both(R, R), Verdict{Reason: ReasonChunkChanged}
		}, DecisionReject, ReasonChunkChanged},
		{"duplicate wins over everything", func(in *AudioVoteInput) {
			in.Votes, in.DuplicateOf = both(R, R), "00000000-0000-0000-0000-0000000000f9"
		}, DecisionDuplicate, ReasonDuplicate},
		{"capped dropped_word → hold", func(in *AudioVoteInput) {
			in.Votes, in.IssueType = both(R, R), IssueDroppedWord
		}, DecisionHold, ReasonTypeCapped},
		{"capped number_artifact → hold", func(in *AudioVoteInput) {
			in.Votes, in.IssueType = both(R, R), IssueNumberArtifact
		}, DecisionHold, ReasonTypeCapped},
		{"capped type may still be rejected", func(in *AudioVoteInput) {
			in.Votes, in.IssueType = both(O, O), IssueDroppedWord
		}, DecisionReject, ReasonASRHeardOriginal},
		{"no_evidence hold lifts", func(in *AudioVoteInput) {
			in.Votes, in.HoldReason, in.HoldP = both(R, R), ReasonNoEvidence, pf(0.9)
		}, DecisionApply, ReasonASRAgree},
		{"evidence_excluded hold lifts", func(in *AudioVoteInput) {
			in.Votes, in.HoldReason, in.HoldP = both(R, R), ReasonEvidenceExcluded, pf(0.9)
		}, DecisionApply, ReasonASRAgree},
		{"jev_unavailable stays held", func(in *AudioVoteInput) {
			in.Votes, in.HoldReason, in.HoldP = both(R, R), ReasonJevUnavailable, nil
		}, DecisionHold, ReasonASRAgreeVetoed},
		{"capitalised_replacement stays held", func(in *AudioVoteInput) {
			in.Votes, in.HoldReason, in.HoldP = both(R, R), ReasonCapitalised, pf(0.1)
		}, DecisionHold, ReasonASRAgreeVetoed},
		{"overlap_pending stays held", func(in *AudioVoteInput) {
			in.Votes, in.HoldReason, in.HoldP = both(R, R), ReasonOverlapPending, pf(0.6)
		}, DecisionHold, ReasonASRAgreeVetoed},
		{"issue_type_capped hold stays held", func(in *AudioVoteInput) {
			in.Votes, in.HoldReason, in.HoldP = both(R, R), ReasonTypeCapped, pf(0.9)
		}, DecisionHold, ReasonASRAgreeVetoed},
		{"allowed reason without p stays held", func(in *AudioVoteInput) {
			in.Votes, in.HoldP = both(R, R), nil
		}, DecisionHold, ReasonASRAgreeVetoed},
		{"overlap_pending may be rejected", func(in *AudioVoteInput) {
			in.Votes, in.HoldReason = both(O, O), ReasonOverlapPending
		}, DecisionReject, ReasonASRHeardOriginal},
		{"jev_unavailable may be rejected", func(in *AudioVoteInput) {
			in.Votes, in.HoldReason, in.HoldP = both(O, O), ReasonJevUnavailable, nil
		}, DecisionReject, ReasonASRHeardOriginal},
		{"unresolved conflict stays held", func(in *AudioVoteInput) {
			in.Votes, in.ConflictsLost = both(R, R), false
		}, DecisionHold, ReasonASRAgreeConflict},
		{"conflict winner whose losers are pending applies", func(in *AudioVoteInput) {
			in.Votes, in.Rung0.Conflicts = both(R, R), []string{"00000000-0000-0000-0000-0000000000f8"}
		}, DecisionApply, ReasonASRAgree},
	}
	r := avRung(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			tt.mut(&in)
			out := r.Evaluate(in)
			if out.Decision != tt.decision || out.Reason != tt.reason {
				t.Fatalf("got %s %s, want %s %s", out.Decision, out.Reason, tt.decision, tt.reason)
			}
			if out.P != nil || out.FnCallID != 0 || out.Asked || out.Evidence != EvidenceNone {
				t.Errorf("the rung asks no model: %+v", out)
			}
			if out.FindingID != id || out.ChunkHash != "h" {
				t.Errorf("identity %q %q", out.FindingID, out.ChunkHash)
			}
		})
	}
}

// The event detail snapshots every counted vote, in model order.
func TestAudioVoteDetail(t *testing.T) {
	r := avRung(t)
	out := r.Evaluate(AudioVoteInput{FindingID: "f", Rung0: Verdict{Pass: true}, ConflictsLost: true,
		HoldReason: ReasonUncertain, HoldP: pf(0.5), Votes: []db.ASRVote{
			vote(7, avB, avRevB, asrpilot.VoteHeardReplacement),
			vote(3, avA, avRevA, asrpilot.VoteHeardReplacement),
			vote(9, avA, "old", asrpilot.VoteHeardOriginal),
		}})
	var d struct {
		Rung  string
		Rule  string
		Votes []db.ASRVote
	}
	if err := json.Unmarshal(out.Detail, &d); err != nil {
		t.Fatal(err)
	}
	if d.Rung != AudioVoteDetailRung || d.Rule != asrpilot.VoteRule || len(d.Votes) != 2 ||
		d.Votes[0].VoteID != 3 || d.Votes[1].VoteID != 7 || d.Votes[0].ModelRevision != avRevA {
		t.Errorf("detail %s", out.Detail)
	}
	if !strings.Contains(string(out.Detail), `"pilot_transcript_id":30`) || !strings.Contains(string(out.Detail), `"updated_at":"2026-11-01T00:00:00Z"`) {
		t.Errorf("detail lacks the pilot transcript or the vote's time: %s", out.Detail)
	}
	e, ok := eventOf(strings.Repeat("a", 64), out)
	if !ok || string(e.Detail) != string(out.Detail) || e.P != nil || e.FnCallID != nil || e.Evidence != EvidenceNone {
		t.Errorf("event %+v", e)
	}
}

// The recipe names the rule, the models (in canonical order), the liftable
// holds and rung 0's version: changing any is another recipe; listing the
// models in another order is not.
func TestAudioVoteRecipe(t *testing.T) {
	r := avRung(t)
	rc := r.Recipe()
	if rc.Step != recipe.StepDecide || rc.ModelAlias != AudioVoteModelAlias || rc.StepVersion != AudioVoteStepVersion ||
		rc.PromptVersion != AudioVoteRungVersion || rc.Params["fn"] != AudioVoteName || rc.Params["rung0_version"] != Rung0Version {
		t.Fatalf("recipe %+v", rc)
	}
	if !slices.Equal(rc.Params["apply_hold_reasons"].([]string), AudioApplyHoldReasons) {
		t.Errorf("apply_hold_reasons %v", rc.Params["apply_hold_reasons"])
	}
	id := recipeID(t, rc)
	swapped := avConfig()
	slices.Reverse(swapped.Models)
	same, err := NewAudioVoteRung(swapped)
	if err != nil {
		t.Fatal(err)
	}
	if recipeID(t, same.Recipe()) != id {
		t.Error("model order changed the recipe")
	}
	for name, mut := range map[string]func(*config.AudioVoteConfig){
		"rule":     func(c *config.AudioVoteConfig) { c.Rule = "asr_vote@v3" },
		"revision": func(c *config.AudioVoteConfig) { c.Models[0].Revision = "4444" },
		"model":    func(c *config.AudioVoteConfig) { c.Models[1].ModelID = "org-c/asr-c" },
		"third": func(c *config.AudioVoteConfig) {
			c.Models = append(c.Models, config.AudioVoteModel{ModelID: "c", Revision: "r"})
		},
	} {
		c := avConfig()
		mut(&c)
		other, err := NewAudioVoteRung(c)
		if err != nil {
			t.Fatal(err)
		}
		if recipeID(t, other.Recipe()) == id {
			t.Errorf("changing the %s kept the recipe", name)
		}
	}
	// Never the should_apply recipe, whatever its policy.
	f, err := ShouldApplyFn(DefaultShouldApplyParams())
	if err != nil {
		t.Fatal(err)
	}
	if recipeID(t, f.Recipe("")) == id {
		t.Error("audio recipe equals the should_apply recipe")
	}
	if _, err := NewAudioVoteRung(config.AudioVoteConfig{Rule: "r", Models: avConfig().Models[:1]}); err == nil {
		t.Error("one model accepted")
	}
}

// fakeAudioStore serves a fixed held work list, votes and latest decisions on
// top of the decide fakes, and records what is written.
type fakeAudioStore struct {
	*fakeWriteStore
	held   []db.AudioVoteFinding
	votes  map[string][]db.ASRVote
	latest map[string]db.LatestDecision
	pages  []db.AudioVoteScope
	// currentDecide is current_recipes.decide ("" = none); groupRecipes the
	// run recipe each DecideChunks call was made relative to.
	currentDecide string
	groupRecipes  []string
}

func (s *fakeAudioStore) DecideChunks(ctx context.Context, rid string, keys []db.ChunkKey) (map[db.ChunkKey]*db.DecideChunk, error) {
	s.groupRecipes = append(s.groupRecipes, rid)
	return s.fakeRunStore.DecideChunks(ctx, rid, keys)
}

func (s *fakeAudioStore) AudioVoteWork(_ context.Context, sc db.AudioVoteScope) ([]db.AudioVoteFinding, error) {
	s.pages = append(s.pages, sc)
	var out []db.AudioVoteFinding
	for _, f := range s.held {
		if f.ID > sc.After && len(out) < sc.Limit {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *fakeAudioStore) CurrentASRVotes(_ context.Context, ids []string, rule string, _ []config.AudioVoteModel) (map[string][]db.ASRVote, error) {
	out := map[string][]db.ASRVote{}
	for _, id := range ids {
		if v, ok := s.votes[id]; ok && rule == asrpilot.VoteRule {
			out[id] = v
		}
	}
	return out, nil
}

func (s *fakeAudioStore) ListCurrentRecipes(context.Context) ([]db.CurrentRecipe, error) {
	if s.currentDecide == "" {
		return nil, nil
	}
	return []db.CurrentRecipe{{Step: recipe.StepDecide, RecipeID: s.currentDecide}}, nil
}

func (s *fakeAudioStore) LatestDecisions(_ context.Context, ids []string) (map[string]db.LatestDecision, error) {
	out := map[string]db.LatestDecision{}
	for _, id := range ids {
		if d, ok := s.latest[id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

const (
	avT      = "00000000-0000-0000-0000-0000000000b1"
	avApply  = "00000000-0000-0000-0000-0000000000c1" // auto sebo → Arecibo, both heard the replacement
	avReject = "00000000-0000-0000-0000-0000000000c2" // picked → packed, both heard the original
	avSplit  = "00000000-0000-0000-0000-0000000000c3" // sat → sad, the models split
	avR0     = "00000000-0000-0000-0000-0000000000c4" // dish → telescope: rung 0 rejects
	avApply2 = "00000000-0000-0000-0000-0000000000c5" // the the → the, both heard the replacement (2nd accept)
)

// newAudioStore is one chunk with five held findings.
func newAudioStore() *fakeAudioStore {
	path := "/b/PHM/01.m4b"
	held := func(id, issue, orig, repl, reason string, p *float64) db.AudioVoteFinding {
		return db.AudioVoteFinding{DecideFinding: finding(id, avT, path, issue, runText, orig, repl, 0, 0.9),
			HoldReason: reason, HoldP: p, HoldEventID: int64(len(id))*100 + int64(id[len(id)-1])}
	}
	fs := []db.AudioVoteFinding{
		held(avApply, IssueMisheardProperNoun, "auto sebo", "Arecibo", ReasonNoEvidence, pf(0.9)),
		held(avReject, IssueMisheardWord, "picked", "packed", ReasonUncertain, pf(0.5)),
		held(avSplit, IssueMisheardWord, "sat", "sad", ReasonUncertain, pf(0.5)),
		held(avR0, IssueMisheardWord, "dish", "telescope", ReasonJevUnavailable, nil),
		held(avApply2, IssueRepeatedText, "the the cat", "the cat", ReasonUncertain, pf(0.6)),
	}
	comp := make([]db.DecideFinding, len(fs))
	for i, f := range fs {
		comp[i] = f.DecideFinding
	}
	k := db.ChunkKey{TranscriptID: avT, ChunkIndex: 0}
	rs := &fakeRunStore{chunks: map[db.ChunkKey]*db.DecideChunk{k: {Key: k, Text: runText, EndSec: 6, Competitors: comp}}}
	R, O := asrpilot.VoteHeardReplacement, asrpilot.VoteHeardOriginal
	both := func(n int64, a, b string) []db.ASRVote {
		return []db.ASRVote{vote(n, avA, avRevA, a), vote(n+1, avB, avRevB, b)}
	}
	return &fakeAudioStore{
		fakeWriteStore: &fakeWriteStore{fakeRunStore: rs, skip: map[string]bool{}},
		held:           fs,
		votes: map[string][]db.ASRVote{
			avApply: both(1, R, R), avReject: both(3, O, O), avSplit: both(5, R, O), avR0: both(7, R, R), avApply2: both(9, R, R),
		},
		latest: map[string]db.LatestDecision{},
	}
}

func TestAudioVoteDryRunWritesNothing(t *testing.T) {
	s := newAudioStore()
	rep, err := AudioVote(context.Background(), s, AudioVoteOptions{Config: avConfig(), Seed: "q4", MaxAccepts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.recipes) != 0 || len(s.fresh) != 0 || len(s.recheck) != 0 {
		t.Fatalf("dry run wrote: recipes %d fresh %d recheck %d", len(s.recipes), len(s.fresh), len(s.recheck))
	}
	want := map[string]int{DecisionApply: 2, DecisionReject: 2, DecisionHold: 1}
	for k, n := range want {
		if rep.Outcomes[k] != n {
			t.Errorf("outcomes %v, want %v", rep.Outcomes, want)
		}
	}
	if rep.Reasons["reject/"+ReasonNotSoundAlike] != 1 || rep.Reasons["hold/"+ReasonASRSplit] != 1 || rep.Capped != 1 {
		t.Errorf("reasons %v capped %d", rep.Reasons, rep.Capped)
	}
	if rep.RecipeID != recipeID(t, avRung(t).Recipe()) || len(rep.Sample) != 5 || rep.Write != nil {
		t.Errorf("recipe %s sample %d write %v", rep.RecipeID, len(rep.Sample), rep.Write)
	}
	var b bytes.Buffer
	if err := rep.Print(&b, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{rep.RecipeID, "rule " + asrpilot.VoteRule, avA + "@" + avRevA, "(dry run) nothing written", "over --max-accepts: 1"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("text report lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	if err := rep.Print(&b, true); err != nil || !json.Valid(b.Bytes()) {
		t.Errorf("json report: %v", err)
	}
	// The same seed lists the same findings in the same order.
	again, err := AudioVote(context.Background(), newAudioStore(), AudioVoteOptions{Config: avConfig(), Seed: "q4", Sample: new(2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Sample) != 2 || again.Sample[0].FindingID != rep.Sample[0].FindingID || again.Sample[1].FindingID != rep.Sample[1].FindingID {
		t.Errorf("sample not deterministic: %v vs %v", again.Sample, rep.Sample[:2])
	}
}

func TestAudioVoteYesWritesAppliesAndRejectsOnly(t *testing.T) {
	s := newAudioStore()
	rep, err := AudioVote(context.Background(), s, AudioVoteOptions{Config: avConfig(), Yes: true, MaxAccepts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.recipes) != 1 {
		t.Fatalf("registered %d recipes", len(s.recipes))
	}
	evs := eventsByID(s.fresh)
	rid := rep.RecipeID
	if e := evs[avApply]; e.Outcome != db.OutcomeApply || e.Reason != ReasonASRAgree || e.RecipeID != rid || e.Detail == nil || e.P != nil {
		t.Errorf("apply event %+v", e)
	}
	if e := evs[avReject]; e.Outcome != db.OutcomeReject || e.Reason != ReasonASRHeardOriginal {
		t.Errorf("reject event %+v", e)
	}
	if e := evs[avR0]; e.Outcome != db.OutcomeReject || e.Reason != ReasonNotSoundAlike {
		t.Errorf("rung-0 reject event %+v", e)
	}
	if _, ok := evs[avSplit]; ok {
		t.Error("a hold was written")
	}
	if _, ok := evs[avApply2]; ok {
		t.Error("an apply past --max-accepts was written")
	}
	if w := rep.Write; w == nil || w.Accepted != 1 || w.Rejected != 2 || w.Held != 0 || w.Capped != 1 || w.Stopped != "max-accepts" {
		t.Errorf("write %+v", rep.Write)
	}
}

// A finding in a conflict applies only when every competitor is recorded as
// having lost to it (held overlap_pending naming it); a duplicate is never
// decided by the rung.
func TestAudioVoteConflictsAndDuplicates(t *testing.T) {
	path := "/b/PHM/01.m4b"
	const loser = "00000000-0000-0000-0000-0000000000d9"
	s := newAudioStore()
	s.held = s.held[:1] // auto sebo → Arecibo
	// A different fix to the same words, still proposed.
	comp := finding(loser, avT, path, IssueMisheardProperNoun, runText, "auto sebo", "Arecebo", 0, 0.8)
	k := db.ChunkKey{TranscriptID: avT, ChunkIndex: 0}
	s.chunks[k].Competitors = []db.DecideFinding{s.held[0].DecideFinding, comp}

	rep, err := AudioVote(context.Background(), s, AudioVoteOptions{Config: avConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reasons["hold/"+ReasonASRAgreeConflict] != 1 {
		t.Errorf("unresolved conflict: reasons %v", rep.Reasons)
	}
	s.latest[loser] = db.LatestDecision{Outcome: db.OutcomeHold, Reason: ReasonOverlapPending, Related: avApply}
	if rep, err = AudioVote(context.Background(), s, AudioVoteOptions{Config: avConfig()}); err != nil {
		t.Fatal(err)
	}
	if rep.Reasons["apply/"+ReasonASRAgree] != 1 {
		t.Errorf("conflict lost to it: reasons %v", rep.Reasons)
	}
	s.latest[loser] = db.LatestDecision{Outcome: db.OutcomeHold, Reason: ReasonOverlapPending, Related: "00000000-0000-0000-0000-0000000000aa"}
	if rep, err = AudioVote(context.Background(), s, AudioVoteOptions{Config: avConfig()}); err != nil {
		t.Fatal(err)
	}
	if rep.Reasons["hold/"+ReasonASRAgreeConflict] != 1 {
		t.Errorf("conflict lost to another: reasons %v", rep.Reasons)
	}

	// The same fix proposed again by an already-grouped representative: the
	// held finding is its duplicate, and the rung leaves it alone.
	rep2 := finding(loser, avT, path, IssueMisheardProperNoun, runText, "auto sebo", "Arecibo", 0, 0.8)
	rep2.CorrectionKey, rep2.HasDuplicates = "ck", true
	held := s.held[0].DecideFinding
	held.CorrectionKey = "ck"
	s.chunks[k].Competitors = []db.DecideFinding{held, rep2}
	if rep, err = AudioVote(context.Background(), s, AudioVoteOptions{Config: avConfig(), Yes: true}); err != nil {
		t.Fatal(err)
	}
	if rep.Outcomes[DecisionDuplicate] != 1 || len(eventsByID(s.fresh)) != 0 {
		t.Errorf("duplicate: outcomes %v, written %v", rep.Outcomes, eventsByID(s.fresh))
	}
}

// --limit and --shard reach the work list; pages follow the keyset cursor.
func TestAudioVoteScopeAndPaging(t *testing.T) {
	old := workPage
	workPage = 2
	t.Cleanup(func() { workPage = old })
	s := newAudioStore()
	rep, err := AudioVote(context.Background(), s, AudioVoteOptions{Config: avConfig(), Limit: 3, Shard: 1, Shards: 4,
		Book: "PHM", IssueType: IssueMisheardWord})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Findings != 3 || len(s.pages) != 2 {
		t.Fatalf("findings %d pages %d", rep.Findings, len(s.pages))
	}
	p := s.pages[1]
	if p.Limit != 1 || p.After != avReject || p.Shard != 1 || p.Shards != 4 || p.Book != "PHM" || p.IssueType != IssueMisheardWord ||
		p.Rule != asrpilot.VoteRule || len(p.Models) != 2 || p.Models[0].ModelID != avA {
		t.Errorf("second page scope %+v", p)
	}
}

// Grouping runs relative to the current decide recipe, not the rung's own:
// a held representative (decided here by should_apply) keeps its slot
// against a newer, undecided finding proposing the same fix, exactly as
// decide would rank them.
func TestAudioVoteGroupsAsDecideWould(t *testing.T) {
	const newer = "00000000-0000-0000-0000-0000000000e9"
	s := newAudioStore()
	s.held = s.held[:1] // auto sebo → Arecibo, held by should_apply
	s.currentDecide = strings.Repeat("c", 64)
	k := db.ChunkKey{TranscriptID: avT, ChunkIndex: 0}
	held := s.held[0].DecideFinding
	held.CorrectionKey, held.DecidedHere = "ck", true // the chunk's view, relative to current decide
	held.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	other := finding(newer, avT, "/b/PHM/01.m4b", IssueMisheardProperNoun, runText, "auto sebo", "Arecibo", 0, 0.9)
	other.CorrectionKey, other.CreatedAt = "ck", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	s.chunks[k].Competitors = []db.DecideFinding{held, other}

	rep, err := AudioVote(context.Background(), s, AudioVoteOptions{Config: avConfig(), Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.groupRecipes) == 0 || s.groupRecipes[0] != s.currentDecide {
		t.Fatalf("grouped relative to %v, want the current decide recipe %s", s.groupRecipes, s.currentDecide)
	}
	if rep.Reasons["apply/"+ReasonASRAgree] != 1 || rep.Outcomes[DecisionDuplicate] != 0 {
		t.Errorf("the held representative lost its slot to a newer undecided finding: %v", rep.Reasons)
	}
	if e := eventsByID(s.fresh)[avApply]; e.RequiresLatestDecision != s.held[0].HoldEventID {
		t.Errorf("event requires latest decision %d, want the hold %d", e.RequiresLatestDecision, s.held[0].HoldEventID)
	}
}

// --sample 0 lists nothing; unset lists DefaultAudioSample.
func TestAudioVoteSampleZero(t *testing.T) {
	rep, err := AudioVote(context.Background(), newAudioStore(), AudioVoteOptions{Config: avConfig(), Sample: new(0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sample) != 0 || rep.Findings != 5 {
		t.Errorf("sample %d findings %d", len(rep.Sample), rep.Findings)
	}
}
