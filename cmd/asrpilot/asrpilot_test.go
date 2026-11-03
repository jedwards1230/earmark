package asrpilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/patch"
)

// fakeStore is an in-memory pilot database.
type fakeStore struct {
	dirs     map[string]string // asin → book dir
	pilots   []db.PilotTranscript
	pSegs    map[int64][]db.PilotSegment
	prod     map[string][]db.Segment
	held     map[string][]db.HeldFinding // transcript → findings, ascending id
	upserts  [][]db.FindingASRVote
	pages    []int // HeldFindings limits asked
	counts   []db.PilotVoteCount
	pairs    []db.PilotVotePair
	segLoads [][]string
}

func (f *fakeStore) PilotTranscripts(_ context.Context, flt db.PilotFilter) ([]db.PilotTranscript, error) {
	if flt.Limit < 1 || flt.Limit > db.MaxPilotPage {
		return nil, fmt.Errorf("limit %d", flt.Limit)
	}
	var out []db.PilotTranscript
	for _, p := range f.pilots {
		if p.ID <= flt.AfterID || (flt.ModelID != "" && p.ModelID != flt.ModelID) ||
			(flt.ModelRevision != "" && p.ModelRevision != flt.ModelRevision) {
			continue
		}
		out = append(out, p)
		if len(out) == flt.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) PilotSegments(_ context.Context, id int64) ([]db.PilotSegment, error) {
	return f.pSegs[id], nil
}

func (f *fakeStore) GetTranscriptSegments(_ context.Context, ids []string) (map[string][]db.Segment, error) {
	if len(ids) > db.MaxDecideTranscriptBatch {
		return nil, fmt.Errorf("too many ids")
	}
	f.segLoads = append(f.segLoads, ids)
	out := map[string][]db.Segment{}
	for _, id := range ids {
		if s, ok := f.prod[id]; ok {
			out[id] = s
		}
	}
	return out, nil
}

func (f *fakeStore) BookDirsForASINs(_ context.Context, asins []string) ([]string, error) {
	var out []string
	for _, a := range asins {
		if d, ok := f.dirs[a]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeStore) HeldFindings(_ context.Context, tid, after string, limit int) ([]db.HeldFinding, error) {
	if limit < 1 || limit > db.MaxPilotPage {
		return nil, fmt.Errorf("limit %d", limit)
	}
	f.pages = append(f.pages, limit)
	var out []db.HeldFinding
	for _, h := range f.held[tid] {
		if h.ID > after {
			out = append(out, h)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeStore) UpsertFindingASRVotes(_ context.Context, v []db.FindingASRVote) (int64, error) {
	f.upserts = append(f.upserts, v)
	return int64(len(v)), nil
}

func (f *fakeStore) PilotVoteCounts(context.Context, []string) ([]db.PilotVoteCount, error) {
	return f.counts, nil
}

func (f *fakeStore) PilotVotePairs(context.Context, []string) ([]db.PilotVotePair, error) {
	return f.pairs, nil
}

func fp(x float64) *float64 { return &x }

func seg(id int, start float64, text string) db.Segment {
	s := db.Segment{ID: id, Start: start, Text: text}
	t := start
	for _, w := range strings.Fields(text) {
		s.Words = append(s.Words, db.Word{Word: w, Start: t, End: t + 0.4})
		t += 0.5
	}
	s.End = t
	return s
}

func pseg(start float64, text string) db.PilotSegment {
	s := db.PilotSegment{Start: start, Text: text}
	t := start
	for _, w := range strings.Fields(text) {
		s.Words = append(s.Words, db.PilotWord{Word: w, Start: fp(t), End: fp(t + 0.4)})
		t += 0.5
	}
	s.End = t
	return s
}

const tid = "11111111-1111-1111-1111-111111111111"

// newFake: one track with two segments, 150 held findings on its second
// segment's "dorian grey" (enough for two pages), one granite pilot.
func newFake() *fakeStore {
	segs := []db.Segment{seg(0, 0, "it was a dark night"), seg(1, 3, "the portrait of dorian grey hung there")}
	chunk := segs[0].Text + " " + segs[1].Text
	f := &fakeStore{
		dirs: map[string]string{"B075VF1R27": "/books/Wilde/Dorian Gray"},
		pilots: []db.PilotTranscript{
			{ID: 1, FilePath: "/books/Wilde/Dorian Gray/01.m4b", ProductionTranscriptID: tid, Backend: "granite",
				ModelID: "granite", ModelRevision: "r1", Scope: db.PilotScopeFull, HasWordTimes: true,
				AudioSec: fp(7), WallSec: fp(0.5)},
		},
		pSegs: map[int64][]db.PilotSegment{1: {pseg(0, "It was a dark night."), pseg(3, "The portrait of Dorian Gray hung there.")}},
		prod:  map[string][]db.Segment{tid: segs},
		held:  map[string][]db.HeldFinding{},
	}
	for i := range 150 {
		f.held[tid] = append(f.held[tid], db.HeldFinding{
			ID: fmt.Sprintf("%08d-0000-0000-0000-000000000000", i+1), OriginalText: "dorian grey", SuggestedCorrection: "dorian gray",
			AnchorOffset: 7, AnchorOccurrence: -1, // wrong on purpose: Locate recovers it
			ChunkTextSHA256: patch.ChunkHash(chunk), ChunkFound: true, ChunkText: chunk,
		})
	}
	return f
}

func TestVoteDryRunWritesNothing(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	if err := runVote(context.Background(), &out, f, voteOptions{model: "granite", asins: "B075VF1R27"}); err != nil {
		t.Fatal(err)
	}
	if len(f.upserts) != 0 {
		t.Fatalf("dry run wrote %d batches", len(f.upserts))
	}
	s := out.String()
	for _, want := range []string{"DRY RUN", "held findings 150", "heard_replacement  150", "recovered (offset wrong) 150"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if len(f.pages) != 2 || f.pages[0] != db.MaxPilotPage {
		t.Errorf("pages asked %v, want two bounded pages", f.pages)
	}
	if len(f.segLoads) != 1 || len(f.segLoads[0]) != 1 {
		t.Errorf("production segments must load one track at a time: %v", f.segLoads)
	}
}

func TestVoteYesUpsertsPages(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	if err := runVote(context.Background(), &out, f, voteOptions{model: "granite", asins: "B075VF1R27", yes: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.upserts) != 2 || len(f.upserts[0]) != 100 || len(f.upserts[1]) != 50 {
		t.Fatalf("upsert batches %d", len(f.upserts))
	}
	v := f.upserts[0][0]
	if v.Vote != "heard_replacement" || v.HeardText != "Dorian Gray" || v.PilotTranscriptID != 1 || v.ModelID != "granite" ||
		v.SpanStartSec == nil || *v.SpanStartSec != 4.5 {
		t.Errorf("vote row %+v", v)
	}
	var d map[string]any
	if err := json.Unmarshal(v.Detail, &d); err != nil || d["rule"] != "asr_vote@v1" || d["anchor"] != "recovered" {
		t.Errorf("detail %s (%v)", v.Detail, err)
	}
	if !strings.Contains(out.String(), "wrote 150 vote(s)") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestVoteLimit(t *testing.T) {
	f := newFake()
	if err := runVote(context.Background(), &bytes.Buffer{}, f, voteOptions{model: "granite", bookDir: "/books/Wilde/Dorian Gray/", limit: 30, yes: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.upserts) != 1 || len(f.upserts[0]) != 30 || f.pages[0] != 30 {
		t.Errorf("limit 30: batches %d pages %v", len(f.upserts), f.pages)
	}
}

func TestVoteFlagErrors(t *testing.T) {
	tests := []struct {
		name string
		o    voteOptions
		want string
	}{
		{"model required", voteOptions{asins: "B075VF1R27"}, "--model is required"},
		{"book required", voteOptions{model: "granite"}, "one of --asin or --book-dir"},
		{"exclusive", voteOptions{model: "granite", asins: "B075VF1R27", bookDir: "/x"}, "mutually exclusive"},
		{"unknown asin", voteOptions{model: "granite", asins: "NOPE"}, "no book_metadata row"},
		{"negative limit", voteOptions{model: "granite", asins: "B075VF1R27", limit: -1}, "--limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runVote(context.Background(), &bytes.Buffer{}, newFake(), tt.o)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestOnePerTrack(t *testing.T) {
	ps := []db.PilotTranscript{
		{ID: 1, FilePath: "a", ModelRevision: "r1", Scope: db.PilotScopeFindings},
		{ID: 2, FilePath: "a", ModelRevision: "r1", Scope: db.PilotScopeFull},
		{ID: 3, FilePath: "a", ModelRevision: "r1", Scope: db.PilotScopeFindings},
		{ID: 4, FilePath: "b", ModelRevision: "r1", Scope: db.PilotScopeFindings},
	}
	got, err := onePerTrack(ps, "")
	if err != nil || len(got) != 2 || got[0].ID != 2 || got[1].ID != 4 {
		t.Errorf("onePerTrack = %+v, %v (full beats findings; one per track)", got, err)
	}
	ps = append(ps, db.PilotTranscript{ID: 5, FilePath: "c", ModelRevision: "r2"})
	if _, err := onePerTrack(ps, ""); err == nil || !strings.Contains(err.Error(), "--revision") {
		t.Errorf("two revisions without --revision: %v", err)
	}
}

func TestReport(t *testing.T) {
	f := newFake()
	// A Canary pilot of the same track in scope findings: only the second
	// segment's window, segment times only.
	f.pilots = append(f.pilots, db.PilotTranscript{ID: 2, FilePath: "/books/Wilde/Dorian Gray/01.m4b",
		ProductionTranscriptID: tid, Backend: "canary", ModelID: "canary", ModelRevision: "r9",
		Scope: db.PilotScopeFindings, AudioSec: fp(3.5), WallSec: fp(1)})
	f.pSegs[2] = []db.PilotSegment{{Start: 3, End: 6.5, Text: "The portrait of Dorian Gray hung there.",
		Words: []db.PilotWord{{Word: "The"}, {Word: "portrait"}, {Word: "of"}, {Word: "Dorian"}, {Word: "Gray"}, {Word: "hung"}, {Word: "there."}}}}
	f.counts = []db.PilotVoteCount{
		{ModelID: "granite", ModelRevision: "r1", Vote: "heard_replacement", N: 3},
		{ModelID: "granite", ModelRevision: "r1", Vote: "neither", N: 1},
	}
	f.pairs = []db.PilotVotePair{{ModelA: "canary", RevisionA: "r9", VoteA: "heard_replacement",
		ModelB: "granite", RevisionB: "r1", VoteB: "heard_replacement", N: 2}}

	var out bytes.Buffer
	if err := runReport(context.Background(), &out, f, reportOptions{format: "json"}); err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Models) != 2 {
		t.Fatalf("models %+v", r.Models)
	}
	c, g := r.Models[0], r.Models[1]
	if c.Model != "canary@r9" || c.WERScope != "covered_windows_only" || c.RefWords != 7 || c.Errors != 1 || c.CoveredHours*3600 != 3.5 {
		t.Errorf("canary row %+v", c)
	}
	if g.Model != "granite@r1" || g.WERScope != "full" || g.RefWords != 12 || g.Errors != 1 || g.RTFx != 14 || g.CasingRate == 0 {
		t.Errorf("granite row %+v", g)
	}
	if len(r.Cross) != 1 || r.Cross[0].WERScope != "covered_windows_only" || r.Cross[0].Errors != 0 || r.Cross[0].RefWords != 7 {
		t.Errorf("cross %+v", r.Cross)
	}
	if len(r.Votes) != 1 || r.Votes[0].Voted != 4 || r.Votes[0].Resolved != 3 {
		t.Errorf("votes %+v", r.Votes)
	}
	if len(r.Agreement) != 1 || r.Agreement[0].AgreementRate != 1 {
		t.Errorf("agreement %+v", r.Agreement)
	}
	if r.Production.CasingRate != 0 || r.Production.Words != 12 {
		t.Errorf("production text %+v", r.Production)
	}

	out.Reset()
	if err := runReport(context.Background(), &out, f, reportOptions{format: "text"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"covered windows only", "granite@r1", "Two-model vote agreement", "not ground truth"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text report lacks %q:\n%s", want, out.String())
		}
	}
	if err := runReport(context.Background(), &out, f, reportOptions{format: "xml"}); err == nil {
		t.Error("--format xml must fail")
	}
}
