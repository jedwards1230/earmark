package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jedwards1230/earmark/internal/metaprovider"
)

// Read-only inputs of the decide step (CONTRACT §2.19): the segments a
// finding's sentence is rebuilt from, and the catalogue record its evidence is
// looked up in. Both reads are batched and bounded — transcripts.segments is
// the largest column in the schema, so an unbounded read can OOM-kill the pod
// (the same reason the worker's transcript reads take a limit).

// MaxDecideTranscriptBatch caps the transcripts one GetTranscriptSegments call
// may load.
const MaxDecideTranscriptBatch = 32

// MaxDecideBookBatch caps the books one GetBookRecords call may load.
const MaxDecideBookBatch = 256

// BookRecord is the catalogue record of one book: the book_metadata columns
// the decide step treats as reference text. ASIN is "" for books with no
// catalogue match (libro.fm, unmatched files) and for identity conflicts,
// whose catalogue columns are cleared at ingest (CONTRACT §1.6).
type BookRecord struct {
	BookDir     string
	ASIN        string
	Title       string
	Author      string
	Narrator    string
	Series      []metaprovider.SeriesRef
	Chapters    []metaprovider.Chapter
	Description string
}

var transcriptSegmentsSQL = `
	SELECT id::text, segments
	FROM transcripts
	WHERE id = ANY($1::uuid[])`

// GetTranscriptSegments returns the immutable ASR segments of each listed
// transcript, keyed by transcript id. A transcript that does not exist, or has
// no segments, is absent from the map. At most MaxDecideTranscriptBatch ids
// per call; more is an error rather than a silent truncation.
func (db *DB) GetTranscriptSegments(ctx context.Context, ids []string) (map[string][]Segment, error) {
	return getTranscriptSegments(ctx, db.pool, ids)
}

func getTranscriptSegments(ctx context.Context, q rowQuerier, ids []string) (map[string][]Segment, error) {
	if len(ids) > MaxDecideTranscriptBatch {
		return nil, fmt.Errorf("get transcript segments: %d ids, max %d per call", len(ids), MaxDecideTranscriptBatch)
	}
	out := make(map[string][]Segment, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, transcriptSegmentsSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("get transcript segments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("get transcript segments: scan: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		var segs []Segment
		if err := json.Unmarshal(raw, &segs); err != nil {
			return nil, fmt.Errorf("get transcript segments: transcript %s: %w", id, err)
		}
		if len(segs) > 0 {
			out[id] = segs
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get transcript segments: %w", err)
	}
	return out, nil
}

var bookRecordsSQL = `
	SELECT book_dir, COALESCE(asin, ''), COALESCE(title, ''), COALESCE(author, ''),
	       COALESCE(narrator, ''), COALESCE(series, ''), chapters, COALESCE(description, '')
	FROM book_metadata
	WHERE book_dir = ANY($1::text[])`

// GetBookRecords returns the catalogue record of each listed book directory
// (filepath.Dir of a transcript's file_path), keyed by book_dir. A book with
// no book_metadata row is absent from the map. Chapter titles are cleaned on
// read exactly as GetBookChapters cleans them. At most MaxDecideBookBatch dirs
// per call.
func (db *DB) GetBookRecords(ctx context.Context, bookDirs []string) (map[string]BookRecord, error) {
	return getBookRecords(ctx, db.pool, bookDirs)
}

func getBookRecords(ctx context.Context, q rowQuerier, bookDirs []string) (map[string]BookRecord, error) {
	if len(bookDirs) > MaxDecideBookBatch {
		return nil, fmt.Errorf("get book records: %d dirs, max %d per call", len(bookDirs), MaxDecideBookBatch)
	}
	out := make(map[string]BookRecord, len(bookDirs))
	if len(bookDirs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, bookRecordsSQL, bookDirs)
	if err != nil {
		return nil, fmt.Errorf("get book records: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r BookRecord
		var series string
		var chapters []byte
		if err := rows.Scan(&r.BookDir, &r.ASIN, &r.Title, &r.Author, &r.Narrator, &series, &chapters, &r.Description); err != nil {
			return nil, fmt.Errorf("get book records: scan: %w", err)
		}
		r.Series = metaprovider.ParseSeries(series)
		if len(chapters) > 0 {
			if r.Chapters, err = decodeChapters(chapters); err != nil {
				return nil, fmt.Errorf("get book records: %s: %w", r.BookDir, err)
			}
		}
		out[r.BookDir] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get book records: %w", err)
	}
	return out, nil
}
