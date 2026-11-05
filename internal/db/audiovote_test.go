package db

import (
	"regexp"
	"strings"
	"testing"

	"github.com/jedwards1230/earmark/internal/config"
)

// The audio-vote rung's reads are reads (CONTRACT §2.19 "Audio-vote rung").
func TestAudioVoteSQLIsReadOnly(t *testing.T) {
	write := regexp.MustCompile(`(?i)\b(insert|update|delete|merge|truncate|alter|create|drop|grant|for\s+(update|share|no\s+key|key))\b|set_config|nextval`)
	for name, sql := range map[string]string{
		"work": audioVoteWorkSQL, "votes": currentASRVotesSQL, "latest decisions": latestDecisionsSQL,
	} {
		s := strings.TrimSpace(sql)
		if !strings.HasPrefix(strings.ToUpper(s), "SELECT") {
			t.Errorf("%s: does not start with SELECT", name)
		}
		if m := write.FindString(s); m != "" {
			t.Errorf("%s: contains %q", name, m)
		}
	}
}

// A vote counts only under the configured rule, by a configured (model,
// revision) pair, against the finding's chunk revision; the current vote is
// the latest written. The work list is held findings only.
func TestAudioVoteSQLShape(t *testing.T) {
	work, votes := norm(audioVoteWorkSQL), norm(currentASRVotesSQL)
	for _, s := range []string{work, votes} {
		for _, want := range []string{
			"v.detail->>'rule' =",
			"v.chunk_text_sha256 = f.chunk_text_sha256",
			"(p.model_id, p.model_revision) IN (SELECT * FROM unnest(",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("SQL lacks %q:\n%s", want, s)
			}
		}
	}
	for _, want := range []string{
		"ld ON ld.outcome = 'hold'",
		"f.patch_state = 'proposed'",
		"r.kind = 'revoke' AND r.revokes_event_id = e.id",
		"ORDER BY f.id LIMIT $9",
	} {
		if !strings.Contains(work, want) {
			t.Errorf("work SQL lacks %q", want)
		}
	}
	if !strings.Contains(votes, "DISTINCT ON (v.finding_id, p.model_id, p.model_revision)") ||
		!strings.Contains(votes, "v.updated_at DESC, v.id DESC") {
		t.Errorf("votes SQL must pick the latest vote per model and revision:\n%s", votes)
	}
}

func TestAudioVoteScopeValidate(t *testing.T) {
	ok := AudioVoteScope{Rule: "asr_vote@v2", Limit: 10, Models: []config.AudioVoteModel{
		{ModelID: "a/x", Revision: "r1"}, {ModelID: "b/y", Revision: "r2"},
	}}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*AudioVoteScope){
		"no rule":    func(s *AudioVoteScope) { s.Rule = "" },
		"one model":  func(s *AudioVoteScope) { s.Models = s.Models[:1] },
		"limit 0":    func(s *AudioVoteScope) { s.Limit = 0 },
		"limit big":  func(s *AudioVoteScope) { s.Limit = MaxDecideWorkPage + 1 },
		"bad shard":  func(s *AudioVoteScope) { s.Shard, s.Shards = 2, 2 },
		"bad cursor": func(s *AudioVoteScope) { s.After = "x" },
	} {
		s := ok
		mut(&s)
		if err := s.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
