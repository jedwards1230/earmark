package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v5"
)

func TestGetTranscriptSegments(t *testing.T) {
	ctx := context.Background()

	if _, err := getTranscriptSegments(ctx, newMockPool(t), make([]string, MaxDecideTranscriptBatch+1)); err == nil {
		t.Error("over-cap batch accepted")
	}
	if got, err := getTranscriptSegments(ctx, newMockPool(t), nil); err != nil || len(got) != 0 {
		t.Errorf("empty batch = %v, %v", got, err)
	}

	mock := newMockPool(t)
	mock.ExpectQuery(`FROM transcripts`).WithArgs([]string{"t1", "t2", "t3"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "segments"}).
			AddRow("t1", []byte(`[{"id":0,"start":1.5,"end":2,"text":"hello there","words":[]}]`)).
			AddRow("t2", []byte(`[]`)).
			AddRow("t3", []byte(nil)))
	got, err := getTranscriptSegments(ctx, mock, []string{"t1", "t2", "t3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got["t1"]) != 1 || got["t1"][0].Text != "hello there" || got["t1"][0].Start != 1.5 {
		t.Errorf("got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}

	bad := newMockPool(t)
	bad.ExpectQuery(`FROM transcripts`).WithArgs([]string{"t1"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "segments"}).AddRow("t1", []byte(`{`)))
	if _, err := getTranscriptSegments(ctx, bad, []string{"t1"}); err == nil {
		t.Error("corrupt segments accepted")
	}
}

func TestGetBookRecords(t *testing.T) {
	ctx := context.Background()

	if _, err := getBookRecords(ctx, newMockPool(t), make([]string, MaxDecideBookBatch+1)); err == nil {
		t.Error("over-cap batch accepted")
	}

	mock := newMockPool(t)
	mock.ExpectQuery(`FROM book_metadata`).WithArgs([]string{"/b/PHM", "/b/Libro"}).
		WillReturnRows(pgxmock.NewRows([]string{"book_dir", "asin", "title", "author", "narrator", "series", "chapters", "description"}).
			AddRow("/b/PHM", "B08GB58KD5", "Project Hail Mary", "Andy Weir", "Ray Porter", "Standalone #1",
				[]byte(`[{"Index":0,"Title":"Chapter 1","StartSec":0,"EndSec":10}]`), "<p>Blurb.</p>").
			AddRow("/b/Libro", "", "Some Book", "", "", "", []byte(nil), ""))
	got, err := getBookRecords(ctx, mock, []string{"/b/PHM", "/b/Libro"})
	if err != nil {
		t.Fatal(err)
	}
	phm := got["/b/PHM"]
	if phm.ASIN != "B08GB58KD5" || phm.Narrator != "Ray Porter" || len(phm.Series) != 1 || phm.Series[0].Name != "Standalone" ||
		len(phm.Chapters) != 1 || phm.Chapters[0].Title != "Chapter 1" || phm.Description != "<p>Blurb.</p>" {
		t.Errorf("phm = %+v", phm)
	}
	if lib := got["/b/Libro"]; lib.ASIN != "" || lib.Chapters != nil || lib.Series != nil {
		t.Errorf("libro = %+v", lib)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Postgres proof of the decide-step reads: id/uuid and book_dir array
// binding, NULL columns, and that a missing row is simply absent.
func TestIntegrationDecideInputs(t *testing.T) {
	d := integrationDB(t, newTestDatabase(t))
	ctx := context.Background()
	const (
		t1 = "00000000-0000-0000-0000-0000000000d1"
		t2 = "00000000-0000-0000-0000-0000000000d2"
	)
	_, err := d.pool.Exec(ctx, `
		INSERT INTO transcription_jobs (id, file_path, checksum, status) VALUES
		  ('00000000-0000-0000-0000-00000000000a', '/b/PHM/01.m4b', 'c1', 'done'),
		  ('00000000-0000-0000-0000-00000000000b', '/b/Libro/01.m4b', 'c2', 'done');
		INSERT INTO transcripts (id, job_id, file_path, checksum, language, duration_seconds,
		                         segments, raw_text, model_name) VALUES
		  ('`+t1+`', '00000000-0000-0000-0000-00000000000a', '/b/PHM/01.m4b', 'c1', 'en', 10,
		   '[{"id":0,"start":0,"end":2,"text":"the dish at auto sebo","words":[]},{"id":1,"start":2,"end":4,"text":"picked it up","words":[]}]', 'x', 'parakeet'),
		  ('`+t2+`', '00000000-0000-0000-0000-00000000000b', '/b/Libro/01.m4b', 'c2', 'en', 10, '[]', 'x', 'parakeet');
		INSERT INTO book_metadata (book_dir, title, author, narrator, series, asin, chapters, description) VALUES
		  ('/b/PHM', 'Project Hail Mary', 'Andy Weir', 'Ray Porter', 'Standalone #1', 'B08GB58KD5',
		   '[{"Index":0,"Title":"The Arecibo Message","StartSec":0,"EndSec":10}]', '<p>Arecibo.</p>'),
		  ('/b/Libro', 'Some Book', NULL, NULL, NULL, NULL, NULL, NULL);`)
	if err != nil {
		t.Fatal(err)
	}

	segs, err := d.GetTranscriptSegments(ctx, []string{t1, t2, "00000000-0000-0000-0000-0000000000ff"})
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 || len(segs[t1]) != 2 || segs[t1][1].Text != "picked it up" {
		t.Errorf("segments = %+v", segs)
	}

	recs, err := d.GetBookRecords(ctx, []string{"/b/PHM", "/b/Libro", "/b/Missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	if r := recs["/b/PHM"]; r.ASIN != "B08GB58KD5" || len(r.Chapters) != 1 || r.Chapters[0].Title != "The Arecibo Message" ||
		len(r.Series) != 1 || !strings.Contains(r.Description, "Arecibo") {
		t.Errorf("phm = %+v", r)
	}
	if r := recs["/b/Libro"]; r.ASIN != "" || r.Title != "Some Book" || r.Chapters != nil {
		t.Errorf("libro = %+v", fmt.Sprint(r))
	}
}
