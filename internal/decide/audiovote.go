package decide

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jedwards1230/earmark/internal/asrpilot"
	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/runs"
)

// The audio-vote rung (CONTRACT §2.19 "Audio-vote rung", `earmark decide
// --audio-vote`): a second, separately registered decide recipe that decides
// HELD findings from the second-ASR votes (finding_asr_votes, CONTRACT §1.11)
// of the configured pilot models. No model is asked. It re-runs rung 0 exactly
// as decide does, then:
//
//	every configured model heard the original     → reject asr_heard_original
//	every configured model heard the replacement  → apply  asr_agree, if no
//	                                                 existing rung objects
//	anything else                                  → hold (never written)
//
// Holds are not written: the finding's latest decision stays the hold that
// made it held, so the should_apply recipe's work list is unchanged.

// Audio-vote rung identity. AudioVoteRungVersion is bumped with any change
// to the evaluation below that can change an outcome.
const (
	AudioVoteName        = "asr_vote_decide"
	AudioVoteModelAlias  = "asr-vote"
	AudioVoteStepVersion = 1
	AudioVoteRungVersion = "asr_vote_decide@v1"
	// AudioVoteDetailRung is finding_events.detail.rung on the rung's events.
	AudioVoteDetailRung = "asr_vote"
)

// Audio-vote reasons (Outcome.Reason). A data contract like the others: add,
// never rename.
const (
	// ReasonASRAgree — every configured model heard the replacement and no
	// existing rung objects: applied.
	ReasonASRAgree = "asr_agree"
	// ReasonASRHeardOriginal — every configured model heard the original:
	// rejected (rung 0 still passing).
	ReasonASRHeardOriginal = "asr_heard_original"
	// ReasonASRSplit — every configured model voted on the word, some heard
	// the replacement and some the original: held.
	ReasonASRSplit = "asr_split"
	// ReasonASRVoteMissing — a configured model has no current vote (none,
	// another rule, another revision, or against another chunk revision): held.
	ReasonASRVoteMissing = "asr_vote_missing"
	// ReasonASRNoAgreement — every configured model voted, but at least one
	// vote is neither, unmappable or style_only: held.
	ReasonASRNoAgreement = "asr_no_agreement"
	// ReasonASRAgreeVetoed — every model heard the replacement, but the hold
	// the finding is held by is not one the rung may lift (no decoded p, or a
	// reason outside AudioApplyHoldReasons): held.
	ReasonASRAgreeVetoed = "asr_agree_vetoed"
	// ReasonASRAgreeConflict — every model heard the replacement, but the
	// finding overlaps a different proposed fix that is not recorded as
	// having lost to it (held overlap_pending naming it): held.
	ReasonASRAgreeConflict = "asr_agree_conflict"
)

// AudioApplyHoldReasons are the should_apply holds an audio agreement may
// lift: the pinned model answered (a decoded p in [0, 1]) and did not lean
// reject — uncertain (reject_p < p < apply_p), no_evidence and
// evidence_excluded (both p ≥ apply_p). Never jev_unavailable (no answer),
// capitalised_replacement (p ≤ reject_p), issue_type_capped or
// overlap_pending. Sorted.
var AudioApplyHoldReasons = []string{ReasonEvidenceExcluded, ReasonNoEvidence, ReasonUncertain}

// audioRuleTable is the evaluation, as hashed into the recipe's prompt_sha256
// (there is no prompt; the table is what a version names).
var audioRuleTable = []string{
	"duplicate of a representative → duplicate (not written)",
	"rung 0 fails → reject <rung-0 reason>",
	"a configured model has no current vote → hold asr_vote_missing",
	"every model heard_original → reject asr_heard_original",
	"every model heard_replacement, capped issue type → hold issue_type_capped",
	"every model heard_replacement, hold without p or reason not in apply_hold_reasons → hold asr_agree_vetoed",
	"every model heard_replacement, conflict not recorded as lost to it → hold asr_agree_conflict",
	"every model heard_replacement → apply asr_agree",
	"heard_replacement and heard_original only → hold asr_split",
	"otherwise → hold asr_no_agreement",
}

// AudioVoteRung is the configured rung: its models (sorted) and rule, and the
// decide recipe every outcome is attributed to.
type AudioVoteRung struct {
	cfg    config.AudioVoteConfig
	recipe recipe.Recipe
}

// NewAudioVoteRung validates cfg and builds the rung.
func NewAudioVoteRung(cfg config.AudioVoteConfig) (*AudioVoteRung, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("decide --audio-vote: %w", err)
	}
	models := slices.Clone(cfg.Models)
	slices.SortFunc(models, func(a, b config.AudioVoteModel) int {
		return cmp.Or(strings.Compare(a.ModelID, b.ModelID), strings.Compare(a.Revision, b.Revision))
	})
	cfg.Models = models
	pins := make([]map[string]any, len(models))
	for i, m := range models {
		pins[i] = map[string]any{"model_id": m.ModelID, "revision": m.Revision}
	}
	capped := make([]string, 0, len(cappedTypes))
	for t := range cappedTypes {
		capped = append(capped, t)
	}
	slices.Sort(capped)
	return &AudioVoteRung{cfg: cfg, recipe: recipe.Recipe{
		Step:          recipe.StepDecide,
		StepVersion:   AudioVoteStepVersion,
		CodeVersion:   recipe.CodeVersion(),
		ModelAlias:    AudioVoteModelAlias,
		ModelResolved: AudioVoteModelAlias,
		PromptVersion: AudioVoteRungVersion,
		PromptSHA256:  recipe.PromptSHA256(audioRuleTable...),
		Params: map[string]any{
			"fn":                 AudioVoteName,
			"rule":               cfg.Rule,
			"models":             pins,
			"apply_hold_reasons": slices.Clone(AudioApplyHoldReasons),
			"capped_issue_types": capped,
			"rung0_version":      Rung0Version,
			"phonetic_min_sim":   DefaultShouldApplyParams().PhoneticMinSim,
		},
	}}, nil
}

// Recipe is the rung's decide recipe. It is never current_recipes.decide:
// only the should_apply recipe is (CurrentRecipe).
func (r *AudioVoteRung) Recipe() recipe.Recipe { return r.recipe }

// Config is the rung's configuration, models sorted.
func (r *AudioVoteRung) Config() config.AudioVoteConfig { return r.cfg }

// AudioVoteInput is everything the audio-vote evaluation needs about one held
// finding.
type AudioVoteInput struct {
	FindingID, IssueType, ChunkHash string
	// Rung0 is the finding's rung-0 verdict, re-run now (planChunk).
	Rung0 Verdict
	// DuplicateOf is the representative grouping made it a duplicate of ("" =
	// none).
	DuplicateOf string
	// ConflictsLost is true when every finding in Rung0.Conflicts is held
	// overlap_pending naming this finding — recorded as having lost to it —
	// and trivially true with no conflict.
	ConflictsLost bool
	// HoldReason and HoldP are the hold the finding is held by: its latest
	// live decision (p nil = none recorded).
	HoldReason string
	HoldP      *float64
	// Votes are the finding's current counted votes (db.CurrentASRVotes).
	Votes []db.ASRVote
}

// audioDetail is finding_events.detail on the rung's events: a snapshot of
// the votes, which are upserted in place.
type audioDetail struct {
	Rung  string       `json:"rung"`
	Rule  string       `json:"rule"`
	Votes []db.ASRVote `json:"votes"`
}

// Evaluate decides one held finding (pure). The outcome's P is nil and its
// evidence none: the rung asks no model and reads no text evidence.
func (r *AudioVoteRung) Evaluate(in AudioVoteInput) Outcome {
	out := Outcome{FindingID: in.FindingID, Rung0: in.Rung0, ChunkHash: in.ChunkHash, Evidence: EvidenceNone}
	votes := r.configuredVotes(in.Votes)
	snapshot := make([]db.ASRVote, 0, len(votes))
	for _, v := range votes {
		if v != nil {
			snapshot = append(snapshot, *v)
		}
	}
	if d, err := json.Marshal(audioDetail{Rung: AudioVoteDetailRung, Rule: r.cfg.Rule, Votes: snapshot}); err == nil {
		out.Detail = d
	}
	set := func(decision, reason string) Outcome {
		out.Decision, out.Reason = decision, reason
		return out
	}
	switch {
	case in.DuplicateOf != "":
		out.Related = in.DuplicateOf
		return set(DecisionDuplicate, ReasonDuplicate)
	case !in.Rung0.Pass:
		return set(DecisionReject, in.Rung0.Reason)
	}
	var replacement, original, missing int
	for _, v := range votes {
		switch {
		case v == nil:
			missing++
		case v.Vote == asrpilot.VoteHeardReplacement:
			replacement++
		case v.Vote == asrpilot.VoteHeardOriginal:
			original++
		}
	}
	n := len(votes)
	switch {
	case missing > 0:
		return set(DecisionHold, ReasonASRVoteMissing)
	case original == n:
		return set(DecisionReject, ReasonASRHeardOriginal)
	case replacement == n:
		switch {
		case cappedTypes[in.IssueType]:
			return set(DecisionHold, ReasonTypeCapped)
		case in.HoldP == nil || !slices.Contains(AudioApplyHoldReasons, in.HoldReason):
			return set(DecisionHold, ReasonASRAgreeVetoed)
		case !in.ConflictsLost:
			return set(DecisionHold, ReasonASRAgreeConflict)
		}
		return set(DecisionApply, ReasonASRAgree)
	case replacement+original == n:
		return set(DecisionHold, ReasonASRSplit)
	}
	return set(DecisionHold, ReasonASRNoAgreement)
}

// configuredVotes is, per configured model (sorted), its vote among vs (nil =
// none). A vote by another model or revision is ignored.
func (r *AudioVoteRung) configuredVotes(vs []db.ASRVote) []*db.ASRVote {
	out := make([]*db.ASRVote, len(r.cfg.Models))
	for i, m := range r.cfg.Models {
		for j := range vs {
			if vs[j].ModelID == m.ModelID && vs[j].ModelRevision == m.Revision {
				out[i] = &vs[j]
				break
			}
		}
	}
	return out
}

// AudioVoteStore is what the rung reads and writes (*db.DB implements it).
// A dry run makes only the reads; RegisterRecipe and the decision writes
// belong to --yes.
type AudioVoteStore interface {
	ChunkStore
	DecisionWriter
	RegisterRecipe(ctx context.Context, r recipe.Recipe) (string, error)
	AudioVoteWork(ctx context.Context, s db.AudioVoteScope) ([]db.AudioVoteFinding, error)
	CurrentASRVotes(ctx context.Context, findingIDs []string, rule string, models []config.AudioVoteModel) (map[string][]db.ASRVote, error)
	LatestDecisions(ctx context.Context, findingIDs []string) (map[string]db.LatestDecision, error)
}

// DefaultAudioSample is how many findings the report lists when --sample is
// not given.
const DefaultAudioSample = 20

// AudioVoteOptions configures an audio-vote run.
type AudioVoteOptions struct {
	Config          config.AudioVoteConfig
	Book, IssueType string
	Shard, Shards   int
	// Limit caps the findings decided (0 = all in scope); Batch the
	// transcripts per write transaction (0 = DefaultWriteBatch); MaxAccepts
	// the accepts written (0 = no cap; a dry run counts what it would cap).
	Limit, Batch, MaxAccepts int
	// Yes writes the apply and reject decisions; otherwise nothing is written.
	Yes bool
	// Sample is how many findings the report lists (0 = DefaultAudioSample),
	// chosen by Seed.
	Sample int
	Seed   string
	// Progress records the run's live progress (nil = none).
	Progress *runs.Run
}

// AudioVoteReport is an audio-vote run's result (the --json output).
type AudioVoteReport struct {
	DryRun   bool     `json:"dry_run"`
	RecipeID string   `json:"recipe_id"`
	Rule     string   `json:"rule"`
	Models   []string `json:"models"`
	Seed     string   `json:"seed"`
	// Limit is --limit; Findings the held findings decided.
	Limit    int `json:"limit"`
	Findings int `json:"findings"`
	// Outcomes counts apply / hold / reject / duplicate / reanchor; Reasons
	// "<outcome>/<reason>"; HoldReasons the reasons of the holds the findings
	// were held by; Votes the vote patterns ("heard_original+heard_replacement",
	// one vote per configured model in model order, "-" = missing).
	Outcomes    map[string]int `json:"outcomes"`
	Reasons     map[string]int `json:"reasons"`
	HoldReasons map[string]int `json:"hold_reasons"`
	Votes       map[string]int `json:"votes"`
	Cells       []AudioCell    `json:"cells"`
	// Capped counts applies a dry run would not write under --max-accepts
	// (a --yes run reports them in Write.Capped).
	Capped int `json:"capped"`
	// Sample lists up to Sample findings in seed order.
	Sample []AudioSample `json:"sample"`
	Write  *WriteStats   `json:"write,omitempty"`
}

// AudioCell is one outcome × reason × issue type count.
type AudioCell struct {
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason"`
	IssueType string `json:"issue_type"`
	Count     int    `json:"count"`
}

// AudioSample is one listed finding.
type AudioSample struct {
	FindingID   string   `json:"finding_id"`
	IssueType   string   `json:"issue_type"`
	Original    string   `json:"original"`
	Replacement string   `json:"replacement"`
	HoldReason  string   `json:"hold_reason"`
	Votes       []string `json:"votes"`
	Outcome     string   `json:"outcome"`
	Reason      string   `json:"reason"`
	rank        string
}

// AudioVote runs the rung over every held finding in scope with a counted
// vote, page by page in finding-id order. A dry run (o.Yes false) writes
// nothing — not even the recipe row; its id is computed. --yes registers the
// recipe once, then writes each page's apply and reject decisions through
// db.ApplyDecisions (holds and duplicates are never written), Batch
// transcripts per transaction, capped by MaxAccepts.
func AudioVote(ctx context.Context, store AudioVoteStore, o AudioVoteOptions) (*AudioVoteReport, error) {
	if o.Limit < 0 || o.MaxAccepts < 0 || o.Batch < 0 || o.Sample < 0 {
		return nil, errors.New("decide --audio-vote: limit, max-accepts, batch and sample must be >= 0")
	}
	rung, err := NewAudioVoteRung(o.Config)
	if err != nil {
		return nil, err
	}
	batch := cmp.Or(o.Batch, DefaultWriteBatch)
	sampleN := cmp.Or(o.Sample, DefaultAudioSample)
	var recipeID string
	if o.Yes {
		if recipeID, err = store.RegisterRecipe(ctx, rung.Recipe()); err != nil {
			return nil, fmt.Errorf("register audio-vote recipe: %w", err)
		}
	} else if recipeID, err = rung.Recipe().ID(); err != nil {
		return nil, fmt.Errorf("audio-vote recipe id: %w", err)
	}
	rec := o.Progress
	rec.SetRecipe(recipeID, AudioVoteModelAlias)
	cfg := rung.Config()
	rep := newAudioReport(!o.Yes, recipeID, cfg, o)
	ws := &WriteStats{}
	scope := db.AudioVoteScope{Rule: cfg.Rule, Models: cfg.Models, Book: o.Book, IssueType: o.IssueType,
		Shard: o.Shard, Shards: o.Shards}
	threshold := DefaultShouldApplyParams().PhoneticMinSim
	wouldAccept := 0
	for page := 1; ; page++ {
		scope.Limit = workPage
		if o.Limit > 0 {
			left := o.Limit - rep.Findings
			if left <= 0 {
				ws.Stopped = "limit"
				break
			}
			scope.Limit = min(scope.Limit, left)
		}
		rec.Phase(fmt.Sprintf("loading page %d", page))
		work, err := store.AudioVoteWork(ctx, scope)
		if err != nil {
			return nil, err
		}
		if len(work) == 0 {
			break
		}
		scope.After = work[len(work)-1].ID

		rec.Phase(fmt.Sprintf("page %d: loading chunks + rung-0 + votes", page))
		items, err := evaluateAudioPage(ctx, store, rung, work, threshold, recipeID)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			rep.add(it, sampleN, o.Seed)
			tickOutcome(rec, it.item.outcome)
		}
		if !o.Yes {
			for _, it := range items {
				if it.item.outcome.Decision != DecisionApply {
					continue
				}
				if o.MaxAccepts > 0 && wouldAccept >= o.MaxAccepts {
					rep.Capped++
					continue
				}
				wouldAccept++
			}
			continue
		}
		// Written: apply and reject (and reanchor-class rejects, which
		// eventOf drops and counts). Holds and duplicates never are.
		var write []*item
		for _, it := range items {
			if d := it.item.outcome.Decision; d == DecisionApply || d == DecisionReject {
				write = append(write, it.item)
			}
		}
		before := *ws
		if err := writeItems(ctx, store, recipeID, write, batch, o.MaxAccepts, ws, pageRecorder{rec, page}); err != nil {
			return nil, err
		}
		rec.Count("skipped", int64(ws.Skipped-before.Skipped))
		rec.Count("capped", int64(ws.Capped-before.Capped))
		if ws.Stopped != "" {
			break
		}
	}
	if o.Yes {
		rep.Write = ws
	}
	rep.finish()
	return rep, nil
}

// audioItem is one evaluated finding of a page.
type audioItem struct {
	item  *item
	hold  string
	votes []string // per configured model: its vote, "-" = none
}

// evaluateAudioPage runs rung 0 over a page (prepareWith: Check, grouping and
// Dedupe against the chunk's proposed findings and overlay, exactly as
// decide), loads the page's votes and the latest decisions of its conflicts,
// and evaluates each finding. Shadow items (competitors outside the page) are
// not decided here.
func evaluateAudioPage(ctx context.Context, store AudioVoteStore, rung *AudioVoteRung, work []db.AudioVoteFinding,
	threshold float64, recipeID string) ([]audioItem, error) {
	fs := make([]db.DecideFinding, len(work))
	byID := make(map[string]db.AudioVoteFinding, len(work))
	ids := make([]string, len(work))
	for i, w := range work {
		fs[i], byID[w.ID], ids[i] = w.DecideFinding, w, w.ID
	}
	prep, err := prepareWith(ctx, store, fs, threshold, recipeID, false)
	if err != nil {
		return nil, err
	}
	cfg := rung.Config()
	votes, err := store.CurrentASRVotes(ctx, ids, cfg.Rule, cfg.Models)
	if err != nil {
		return nil, err
	}
	var conflictIDs []string
	for _, it := range prep.items {
		if !it.shadow {
			conflictIDs = append(conflictIDs, it.input.Verdict.Conflicts...)
		}
	}
	slices.Sort(conflictIDs)
	conflictIDs = slices.Compact(conflictIDs)
	latest := map[string]db.LatestDecision{}
	for start := 0; start < len(conflictIDs); start += db.MaxDecideWorkPage {
		part, err := store.LatestDecisions(ctx, conflictIDs[start:min(start+db.MaxDecideWorkPage, len(conflictIDs))])
		if err != nil {
			return nil, err
		}
		for k, v := range part {
			latest[k] = v
		}
	}
	var out []audioItem
	for _, it := range prep.items {
		if it.shadow {
			continue
		}
		w := byID[it.finding.ID]
		v := *it.input.Verdict
		lost := true
		for _, c := range v.Conflicts {
			d, ok := latest[c]
			if !ok || d.Outcome != db.OutcomeHold || d.Reason != ReasonOverlapPending || d.Related != w.ID {
				lost = false
			}
		}
		in := AudioVoteInput{
			FindingID: w.ID, IssueType: w.IssueType, ChunkHash: w.ChunkTextSHA256, Rung0: v,
			ConflictsLost: lost, HoldReason: w.HoldReason, HoldP: w.HoldP, Votes: votes[w.ID],
		}
		if it.outcome.Decision == DecisionDuplicate {
			in.DuplicateOf = it.outcome.Related
		}
		it.outcome = rung.Evaluate(in)
		ai := audioItem{item: it, hold: w.HoldReason}
		for _, cv := range rung.configuredVotes(votes[w.ID]) {
			if cv == nil {
				ai.votes = append(ai.votes, "-")
			} else {
				ai.votes = append(ai.votes, cv.Vote)
			}
		}
		out = append(out, ai)
	}
	return out, nil
}

func newAudioReport(dry bool, recipeID string, cfg config.AudioVoteConfig, o AudioVoteOptions) *AudioVoteReport {
	rep := &AudioVoteReport{
		DryRun: dry, RecipeID: recipeID, Rule: cfg.Rule, Seed: o.Seed, Limit: o.Limit,
		Outcomes: map[string]int{}, Reasons: map[string]int{}, HoldReasons: map[string]int{}, Votes: map[string]int{},
		Cells: []AudioCell{}, Sample: []AudioSample{},
	}
	for _, m := range cfg.Models {
		rep.Models = append(rep.Models, m.String())
	}
	return rep
}

// add counts one evaluated finding and keeps it in the sample when it ranks
// among the first n by sha256(seed, id).
func (r *AudioVoteReport) add(ai audioItem, n int, seed string) {
	o, f := ai.item.outcome, ai.item.finding
	class := classOf(o)
	r.Findings++
	r.Outcomes[class]++
	r.Reasons[class+"/"+o.Reason]++
	r.HoldReasons[ai.hold]++
	r.Votes[strings.Join(ai.votes, "+")]++
	cell := AudioCell{Outcome: class, Reason: o.Reason, IssueType: f.IssueType}
	if i := slices.IndexFunc(r.Cells, func(c AudioCell) bool {
		return c.Outcome == cell.Outcome && c.Reason == cell.Reason && c.IssueType == cell.IssueType
	}); i >= 0 {
		r.Cells[i].Count++
	} else {
		cell.Count = 1
		r.Cells = append(r.Cells, cell)
	}
	sum := sha256.Sum256([]byte(seed + "\x00" + f.ID))
	s := AudioSample{FindingID: f.ID, IssueType: f.IssueType, Original: f.Original, Replacement: f.Replacement,
		HoldReason: ai.hold, Votes: ai.votes, Outcome: class, Reason: o.Reason, rank: hex.EncodeToString(sum[:])}
	i, _ := slices.BinarySearchFunc(r.Sample, s, func(a, b AudioSample) int { return strings.Compare(a.rank, b.rank) })
	if i >= n {
		return
	}
	r.Sample = slices.Insert(r.Sample, i, s)
	if len(r.Sample) > n {
		r.Sample = r.Sample[:n]
	}
}

// finish orders the cells (outcome class, reason, issue type).
func (r *AudioVoteReport) finish() {
	slices.SortFunc(r.Cells, func(a, b AudioCell) int {
		return cmp.Or(cmp.Compare(classRank(a.Outcome), classRank(b.Outcome)),
			strings.Compare(a.Reason, b.Reason), strings.Compare(a.IssueType, b.IssueType))
	})
}
