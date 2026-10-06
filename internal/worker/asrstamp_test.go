package worker

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/log"
)

// stampingDB is a fakeDB that also implements ASRStamper.
type stampingDB struct {
	*fakeDB
	stamped []db.StampedTranscript
	err     error
	limit   int
}

func (s *stampingDB) StampASRRecipes(_ context.Context, limit int) ([]db.StampedTranscript, error) {
	s.limit = limit
	return s.stamped, s.err
}

// TestStampASR_RefreshesBooksWithEmbeddedTag: after stamping, each book with a
// file carrying an embedded ASIN tag is re-resolved exactly once; books
// without a tag are not; a partial stamping error still refreshes what was
// stamped.
func TestStampASR_RefreshesBooksWithEmbeddedTag(t *testing.T) {
	sdb := &stampingDB{
		fakeDB: &fakeDB{},
		stamped: []db.StampedTranscript{
			{ID: "1", FilePath: "/b/Herbert/Children of Dune/01.m4b", RecipeID: "r", EmbeddedASIN: "B002V57VRC"},
			{ID: "2", FilePath: "/b/Herbert/Children of Dune/02.m4b", RecipeID: "r", EmbeddedASIN: "B002V57VRC"},
			{ID: "3", FilePath: "/b/Weir/Hail Mary/01.m4b", RecipeID: "r"},
		},
		err: errors.New("one row had malformed asr_params"),
	}
	w := &Worker{ctx: context.Background(), db: sdb, log: log.NewLogger("worker-test")}
	var refreshed []string
	w.SetBookRefresher(func(_ context.Context, filePath string) { refreshed = append(refreshed, filePath) })

	w.stampASR(25)

	if sdb.limit != 25 {
		t.Errorf("stamp limit = %d, want 25", sdb.limit)
	}
	if want := []string{"/b/Herbert/Children of Dune/01.m4b"}; !slices.Equal(refreshed, want) {
		t.Errorf("refreshed %v, want %v", refreshed, want)
	}
}

// TestStampASR_OptionalCapability: a DB without the capability is skipped.
func TestStampASR_OptionalCapability(t *testing.T) {
	w := &Worker{ctx: context.Background(), db: &fakeDB{}, log: log.NewLogger("worker-test")}
	w.SetBookRefresher(func(context.Context, string) { t.Error("refresher called without a stamper") })
	w.stampASR(10)
}
