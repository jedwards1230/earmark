package db

import (
	"strings"
	"testing"
)

// TestRejudgeShardMatchesDecide checks the SQL text only: both selections
// contain shardClause with their own placeholders. That the Go callers bind
// those placeholders the same way (shard vs shards) — so `--shard i/N` names
// the same transcripts in `earmark decide` and `earmark eval --rejudge` — is
// proved end to end by TestIntegrationRejudgeShardEqualsDecideShard.
func TestRejudgeShardMatchesDecide(t *testing.T) {
	if want := shardClause("f.transcript_id", "$4", "$5"); !strings.Contains(decideWorkSQL, want) {
		t.Errorf("decideWorkSQL no longer uses shardClause:\nwant %s", want)
	}
	if want := shardClause("t.id", "$4", "$3"); !strings.Contains(rejudgeTargetsSQL, want) {
		t.Errorf("rejudgeTargetsSQL does not use shardClause:\nwant %s", want)
	}
	if got := shardClause("x", "$1", "$2"); got != "($1 = 0 OR mod(mod(hashtext(x::text)::bigint, $1) + $1, $1) = $2)" {
		t.Errorf("shardClause = %s", got)
	}
}
