package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/metaprovider"
	"github.com/jedwards1230/earmark/internal/tokenizer"
)

// duneRecord is a resolved catalogue record with every source the block draws
// on: bias terms (all already shown in the header), Title Case and sentence
// case chapter titles, and an HTML description with entities.
func duneRecord() *db.BookRecord {
	return &db.BookRecord{
		BookDir:  "/books/Frank Herbert/Children of Dune [B002V1OF70]",
		ASIN:     "B002V1OF70",
		Title:    "Children of Dune",
		Author:   "Frank Herbert",
		Narrator: "Scott Brick, Simon Vance",
		Series:   []metaprovider.SeriesRef{{Name: "Dune", Sequence: "3"}},
		BiasTerms: []string{"Frank", "Herbert", "Frank Herbert", "Scott", "Brick", "Scott Brick",
			"Simon", "Vance", "Simon Vance", "Dune", "Children"},
		Chapters: []metaprovider.Chapter{
			{Index: 0, Title: "Opening Credits"},
			{Index: 1, Title: "Chapter 1"},
			{Index: 2, Title: "Book One: Leto"},
			{Index: 3, Title: "The Preacher"},
			{Index: 4, Title: "Ghanima's Plan"},
			{Index: 5, Title: "Part IV"},
			{Index: 6, Title: "The Sietch Tabr Ambush"},
		},
		Description: `<p>The &ldquo;Children of Dune&rdquo; are twin heirs Leto and Ghanima Atreides. ` +
			`Their aunt Alia rules Arrakis, while the Bene Gesserit plot.</p>` +
			`<p>Leto must face the Preacher &amp; Stilgar. Desert power awaits.</p>` +
			`<p>#1 <b>New York Times</b> bestselling author Frank Herbert.</p>`,
	}
}

// wantDuneBlock is duneRecord's rendered block, pinned: it is exactly what
// judge@v3 inserts into the user message and what book_context_sha256 hashes.
const wantDuneBlock = "Book record:\n" +
	"- Title: Children of Dune\n" +
	"- Author: Frank Herbert\n" +
	"- Series: Dune #3\n" +
	"- Narrator: Scott Brick, Simon Vance\n" +
	"- Names: Leto, Preacher, Sietch Tabr Ambush, Ghanima Atreides, Alia, Arrakis, Bene Gesserit, Stilgar\n"

func TestNewBookContext(t *testing.T) {
	noASIN := duneRecord()
	noASIN.ASIN = ""
	blankASIN := duneRecord()
	blankASIN.ASIN = "  "
	empty := &db.BookRecord{ASIN: "B000"}

	tests := []struct {
		name       string
		rec        *db.BookRecord
		wantSource string
		wantBlock  string
	}{
		{"resolved record", duneRecord(), BookContextASINRecord, wantDuneBlock},
		{"no record", nil, BookContextPathOnly, ""},
		{"no ASIN (unmatched or identity conflict)", noASIN, BookContextPathOnly, ""},
		{"blank ASIN", blankASIN, BookContextPathOnly, ""},
		{"ASIN but nothing to show", empty, BookContextPathOnly, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bc := mustBookContext(t, tt.rec)
			if bc.Source != tt.wantSource {
				t.Errorf("source = %q, want %q", bc.Source, tt.wantSource)
			}
			if bc.Block != tt.wantBlock {
				t.Errorf("block:\n got %q\nwant %q", bc.Block, tt.wantBlock)
			}
			if (bc.SHA256 == "") != (tt.wantBlock == "") {
				t.Errorf("sha256 = %q for block %q", bc.SHA256, bc.Block)
			}
		})
	}
}

// TestBookContextHashStable: the same record renders the same bytes and hash
// every time (map iteration, chapter order); a content change changes it.
func TestBookContextHashStable(t *testing.T) {
	first := mustBookContext(t, duneRecord())
	for range 20 {
		if got := mustBookContext(t, duneRecord()); got != first {
			t.Fatalf("unstable book context: %+v vs %+v", got, first)
		}
	}
	shuffled := duneRecord()
	ch := shuffled.Chapters
	ch[0], ch[6] = ch[6], ch[0] // stored order differs, Index order is the same
	if got := mustBookContext(t, shuffled); got.SHA256 != first.SHA256 {
		t.Error("chapter storage order changed the hash; chapters must render in Index order")
	}
	changed := duneRecord()
	changed.Narrator = "Scott Brick"
	if mustBookContext(t, changed).SHA256 == first.SHA256 {
		t.Error("a different record kept the same hash")
	}
}

func TestBookRecordNouns(t *testing.T) {
	tests := []struct {
		name   string
		rec    db.BookRecord
		header []string
		want   []string
	}{
		{
			name: "bias terms first, deduped case-insensitively, header names skipped",
			rec: db.BookRecord{
				BiasTerms:   []string{"Andy", "Weir", "Andy Weir", "Rocky", "rocky", "ROCKY", "Eridian"},
				Description: "Rocky is an Eridian. Grace meets Rocky. Then Grace flies to Tau Ceti.",
			},
			header: []string{"Andy Weir"},
			want:   []string{"Rocky", "Eridian", "Grace", "Tau Ceti"},
		},
		{
			name: "stopwords, chapter words, numbers and roman numerals dropped",
			rec: db.BookRecord{
				Chapters: []metaprovider.Chapter{
					{Index: 0, Title: "Chapter 12"}, {Index: 1, Title: "Part XIV"}, {Index: 2, Title: "Prologue"},
					{Index: 3, Title: "Epilogue: Two Years Later"}, {Index: 4, Title: "the return of Paul"},
				},
			},
			want: []string{"Paul"},
		},
		{
			name: "sentence-initial word needs a mid-sentence capital elsewhere",
			rec: db.BookRecord{
				Description: "Desert winds blow. Hunters wait. Then Muad'Dib rises. Hunters follow Muad'Dib.",
			},
			want: []string{"Muad'Dib"},
		},
		{
			name: "Title Case chapter single words need corroboration, runs do not",
			rec: db.BookRecord{
				Chapters: []metaprovider.Chapter{
					{Index: 0, Title: "The Arecibo Message"}, {Index: 1, Title: "Rescue"}, {Index: 2, Title: "Petrova Line"},
					{Index: 3, Title: "Stratt"},
				},
				Description: "<p>Earth needs Stratt to act.</p>",
			},
			want: []string{"Arecibo Message", "Petrova Line", "Stratt"},
		},
		{
			name: "HTML stripped and entities decoded; possessive and publisher noise",
			rec: db.BookRecord{
				Description: `<div>The crew of&nbsp;<i>Ryland&nbsp;Grace</i>&#39;s ship &mdash; Yáo &amp; Ilyukhina.</div>` +
					`<script>var Evil = "Ignored";</script><p>A New York Times bestseller.</p>`,
			},
			want: []string{"Ryland Grace", "Yáo", "Ilyukhina"},
		},
		{
			name: "a name contained in a longer one is skipped, in either order",
			rec: db.BookRecord{
				BiasTerms:   []string{"Paul", "Ghanima Atreides"},
				Description: "The twins Leto and Ghanima met Paul Atreides loyalists.",
			},
			want: []string{"Ghanima Atreides", "Leto", "Paul Atreides"},
		},
		{
			name: "lowercase particles inside names, d' elision",
			rec: db.BookRecord{
				Description: "Then Leonardo da Vinci met Ludwig van Beethoven and d'Artagnan. Finally de Gaulle spoke.",
			},
			want: []string{"Leonardo da Vinci", "Ludwig van Beethoven", "d'Artagnan", "Gaulle"},
		},
		{
			name: "titles and common-word first names open a name only before one",
			rec: db.BookRecord{
				Description: "Then Will Treaty met Lord Leto and May Parker. Will they win? Then may Will come.",
			},
			want: []string{"Will Treaty", "Lord Leto", "May Parker"},
		},
		{
			name: "em and en dashes separate names; curly and straight apostrophes are one",
			rec: db.BookRecord{
				BiasTerms:   []string{"Muad’Dib"},
				Description: "Then Leto—Ghanima fled with Muad'Dib – Stilgar followed.",
			},
			want: []string{"Muad'Dib", "Leto", "Ghanima", "Stilgar"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bookRecordNouns(&tt.rec, tt.header)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("nouns = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBookRecordNounsCap: at most maxBookRecordNames names, in priority order.
func TestBookRecordNounsCap(t *testing.T) {
	var rec db.BookRecord
	for i := range 60 {
		rec.BiasTerms = append(rec.BiasTerms, fmt.Sprintf("Name%c%c", 'a'+i/26, 'a'+i%26))
	}
	got := bookRecordNouns(&rec, nil)
	if len(got) != maxBookRecordNames {
		t.Fatalf("got %d names, want %d", len(got), maxBookRecordNames)
	}
	if got[0] != "Nameaa" || got[maxBookRecordNames-1] != rec.BiasTerms[maxBookRecordNames-1] {
		t.Errorf("names not in priority order: first %q last %q", got[0], got[len(got)-1])
	}
}

// TestBookRecordTokenBudget: a dense worst case (long header fields, 40 long
// invented names) stays within ~200 cl100k tokens, and truncation drops whole
// names, never part of one.
func TestBookRecordTokenBudget(t *testing.T) {
	rec := &db.BookRecord{
		ASIN:     "B000000000",
		Title:    strings.Repeat("Interminable Chronicles of Xyloquar ", 6),
		Author:   "Zbigniew Kwiatkowski-Przybylski",
		Narrator: "Ngozi Adébáyọ̀-Oyèlárán, Siobhán Ní Bhriain",
		Series:   []metaprovider.SeriesRef{{Name: "Szczebrzeszyn Saga", Sequence: "12"}},
	}
	for i := range 60 {
		rec.BiasTerms = append(rec.BiasTerms, fmt.Sprintf("Qxzthorvaldr%cyx Vbnquelloth", 'a'+i%26)+strings.Repeat("a", i/26))
	}
	bc := mustBookContext(t, rec)
	if bc.Source != BookContextASINRecord {
		t.Fatalf("source = %s", bc.Source)
	}
	n, err := tokenizer.CountTokens(bc.Block)
	if err != nil {
		t.Fatal(err)
	}
	if n > maxBookRecordTokens {
		t.Errorf("block is %d cl100k tokens, budget ~200:\n%s", n, bc.Block)
	}
	t.Logf("worst-case block: %d runes, %d cl100k tokens", utf8.RuneCountInString(bc.Block), n)
	names := bc.Block[strings.Index(bc.Block, "- Names: ")+len("- Names: "):]
	for _, name := range strings.Split(strings.TrimSuffix(names, "\n"), ", ") {
		if !strings.HasPrefix(name, "Qxzthorvaldr") || !strings.Contains(name, " Vbnquelloth") {
			t.Errorf("truncated name %q", name)
		}
	}
	if !strings.Contains(bc.Block, "- Title: Interminable") || !strings.Contains(bc.Block, "…\n") {
		t.Errorf("long title not clipped:\n%s", bc.Block)
	}

	// The header counts too: four absurd fields (the probe that reached 337
	// tokens) are clipped until the block fits, at word boundaries.
	dense := strings.Repeat("Ọ̀ṣọ̀ọ̀sì Ẹ̀ṣọ̀ọ̀ Ọ̀rúnmìlà ", 10)
	huge := &db.BookRecord{
		ASIN: "B000000001", Title: dense, Author: dense, Narrator: dense,
		Series:    []metaprovider.SeriesRef{{Name: dense, Sequence: "12"}},
		BiasTerms: []string{"Qwyzzlethorp"},
	}
	if full, _ := renderBookRecordHeader(huge, bookRecordFieldCaps[0]); mustTokens(t, full) <= maxBookRecordTokens {
		t.Fatalf("probe header fits at the first cap (%d tokens); it does not exercise the step-down", mustTokens(t, full))
	}
	hb := mustBookContext(t, huge)
	if n := mustTokens(t, hb.Block); n > maxBookRecordTokens {
		t.Errorf("header-heavy block is %d cl100k tokens, budget %d:\n%s", n, maxBookRecordTokens, hb.Block)
	}
	for _, line := range strings.Split(strings.TrimSuffix(hb.Block, "\n"), "\n")[1:] {
		v := line[strings.Index(line, ": ")+2:]
		if strings.HasSuffix(v, "…") && !strings.HasSuffix(v, " …") {
			words := strings.Fields(strings.TrimSuffix(v, "…"))
			if last := words[len(words)-1]; !strings.Contains(huge.Title+huge.Author+huge.Narrator+huge.Series[0].Name, last+" ") {
				t.Errorf("field cut mid-word: %q", v)
			}
		}
	}

	n, err = tokenizer.CountTokens(wantDuneBlock)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dune block: %d cl100k tokens", n)
}

func TestClipRunes(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"Children of Dune", 80, "Children of Dune"},
		{"Children of Dune", 12, "Children of…"},
		{"Children of Dune", 15, "Children of…"},
		{"Supercalifragilistic", 8, "Superca…"}, // one word: a hard cut is all there is
		{"Dune, Book Three", 7, "Dune…"},
	}
	for _, tt := range tests {
		if got := clipRunes(tt.in, tt.n); got != tt.want {
			t.Errorf("clipRunes(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct{ in, want string }{
		{"<p>Hello&nbsp;<b>World</b></p>", "\nHello  World \n"},
		{"Fish &amp; Chips &ldquo;q&rdquo; &#39;x&#39;", "Fish & Chips “q” 'x'"},
		{"a<script>x</script>b<style>y</style>c", "abc"},
		{"one<br/>two", "one\ntwo"},
		{"3 < 4", "3 < 4"},
	}
	for _, tt := range tests {
		if got := stripHTML(tt.in); got != tt.want {
			t.Errorf("stripHTML(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSetPromptRejectsUnknown: only v2 / v3 ("" = the default) are accepted;
// an unknown value leaves the judge unchanged, and the version helpers never
// stand an unknown value in for a real version.
func TestSetPromptRejectsUnknown(t *testing.T) {
	j := NewJudge(&fakeChat{})
	mustSetPrompt(t, j, JudgePromptV3)
	if err := j.SetPrompt("v4"); err == nil {
		t.Error("SetPrompt(v4) accepted")
	}
	if j.Prompt() != JudgePromptV3 {
		t.Errorf("a refused SetPrompt changed the prompt to %s", j.Prompt())
	}
	mustSetPrompt(t, j, "")
	if j.Prompt() != DefaultJudgePrompt {
		t.Errorf("SetPrompt(\"\") = %s, want the default", j.Prompt())
	}
	bad := JudgePrompt("v4")
	if bad.Version() != "" || bad.sha256() != "" || bad.usesBookContext() || bad.known() {
		t.Error("an unknown prompt value maps onto a real version")
	}
}

func TestParseJudgePrompt(t *testing.T) {
	tests := []struct {
		in      string
		want    JudgePrompt
		wantErr bool
	}{
		{"", JudgePromptV2, false},
		{"v2", JudgePromptV2, false},
		{"V3", JudgePromptV3, false},
		{"judge@v2", JudgePromptV2, false},
		{"v4", "", true},
	}
	for _, tt := range tests {
		got, err := ParseJudgePrompt(tt.in)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("ParseJudgePrompt(%q) = %q, %v", tt.in, got, err)
		}
	}
}

// fakeRecords is a BookRecordReader over a fixed map, recording each call.
type fakeRecords struct {
	recs  map[string]db.BookRecord
	err   error
	calls [][]string
}

func (f *fakeRecords) GetBookRecords(_ context.Context, dirs []string) (map[string]db.BookRecord, error) {
	f.calls = append(f.calls, append([]string(nil), dirs...))
	if len(dirs) > db.MaxDecideBookBatch {
		return nil, fmt.Errorf("%d dirs over the batch cap", len(dirs))
	}
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]db.BookRecord{}
	for _, d := range dirs {
		if r, ok := f.recs[d]; ok {
			out[d] = r
		}
	}
	return out, nil
}

// TestBookContextsBatches: one read per MaxDecideBookBatch distinct dirs,
// keyed by filepath.Dir(file_path); missing rows are path_only.
func TestBookContextsBatches(t *testing.T) {
	var chunks []db.EvalChunk
	for i := range db.MaxDecideBookBatch + 10 {
		dir := fmt.Sprintf("/books/b%03d", i)
		chunks = append(chunks, db.EvalChunk{FilePath: dir + "/01.m4b"}, db.EvalChunk{FilePath: dir + "/02.m4b"})
	}
	r := &fakeRecords{recs: map[string]db.BookRecord{"/books/b000": *duneRecord()}}
	got, err := bookContexts(context.Background(), r, chunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 || len(r.calls[0]) != db.MaxDecideBookBatch || len(r.calls[1]) != 10 {
		t.Errorf("calls = %d (sizes %d…)", len(r.calls), len(r.calls[0]))
	}
	if got["/books/b000"].Source != BookContextASINRecord || got["/books/b001"].Source != BookContextPathOnly {
		t.Errorf("contexts = %+v / %+v", got["/books/b000"], got["/books/b001"])
	}
}

// TestRunPassesBookContext: under judge@v3 the run reads the records once,
// sends each chunk its book's block, and reports the context per transcript;
// under judge@v2 it reads nothing and sends the v2 message.
func TestRunPassesBookContext(t *testing.T) {
	dune := duneRecord()
	chunks := []db.EvalChunk{
		{ChunkID: "c1", TranscriptID: "t-dune", FilePath: dune.BookDir + "/01.m4b", Text: "leto said"},
		{ChunkID: "c2", TranscriptID: "t-other", FilePath: "/books/Unmatched/01.m4b", Text: "hello"},
	}
	for _, tt := range []struct {
		prompt        JudgePrompt
		wantReads     int
		wantDune      string
		wantInMessage bool
	}{
		{JudgePromptV3, 1, BookContextASINRecord, true},
		{JudgePromptV2, 0, BookContextPathOnly, false},
	} {
		t.Run(string(tt.prompt), func(t *testing.T) {
			chat := &recordingChat{resp: `{"findings":[]}`}
			j := NewJudge(chat)
			mustSetPrompt(t, j, tt.prompt)
			recs := &fakeRecords{recs: map[string]db.BookRecord{dune.BookDir: *dune}}
			j.SetBookRecords(recs)
			var seen []Result
			_, stats, err := Run(context.Background(), fakeReader{chunks: chunks}, j, &fakeWriter{},
				RunOptions{Book: "x", Observe: func(r Result, _ error) { seen = append(seen, r) }})
			if err != nil {
				t.Fatal(err)
			}
			if len(recs.calls) != tt.wantReads {
				t.Errorf("record reads = %d, want %d", len(recs.calls), tt.wantReads)
			}
			if stats.PromptVersion != tt.prompt.Version() {
				t.Errorf("stats prompt = %q", stats.PromptVersion)
			}
			if got := stats.BookContexts["t-dune"].Source; got != tt.wantDune {
				t.Errorf("dune source = %q, want %q", got, tt.wantDune)
			}
			if got := stats.BookContexts["t-other"]; got.Source != BookContextPathOnly || got.SHA256 != "" {
				t.Errorf("unmatched = %+v", got)
			}
			if len(seen) != 2 || seen[0].BookContext != stats.BookContexts["t-dune"] || seen[0].PromptVersion != tt.prompt.Version() {
				t.Errorf("observed results = %+v", seen)
			}
			if strings.Contains(chat.users[0], "Book record:") != tt.wantInMessage {
				t.Errorf("dune message:\n%s", chat.users[0])
			}
			if strings.Contains(chat.users[1], "Book record:") {
				t.Errorf("unmatched book got a record block:\n%s", chat.users[1])
			}
		})
	}
}

// TestRunFailsLoudOnRecordReadError: a broken record read stops the run
// before any judge call instead of judging path_only.
func TestRunFailsLoudOnRecordReadError(t *testing.T) {
	chat := &recordingChat{resp: `{"findings":[]}`}
	j := NewJudge(chat)
	mustSetPrompt(t, j, JudgePromptV3)
	j.SetBookRecords(&fakeRecords{err: errors.New("db down")})
	_, _, err := RunOnChunks(context.Background(), j, nil, []db.EvalChunk{sampleChunk()}, false)
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v, want the record read error", err)
	}
	if len(chat.users) != 0 {
		t.Errorf("judge called %d times after a failed record read", len(chat.users))
	}

	// judge@v2 never reads records, so the same broken source is harmless.
	mustSetPrompt(t, j, JudgePromptV2)
	if _, _, err := RunOnChunks(context.Background(), j, nil, []db.EvalChunk{sampleChunk()}, false); err != nil {
		t.Errorf("v2 run: %v", err)
	}
}

// TestRunNoRecordSourceFails: judge@v3 with no record source is an error,
// not a silent path_only run under the v3 recipe.
func TestRunNoRecordSourceFails(t *testing.T) {
	chat := &recordingChat{resp: `{"findings":[]}`}
	j := NewJudge(chat)
	mustSetPrompt(t, j, JudgePromptV3)
	_, _, err := RunOnChunks(context.Background(), j, nil, []db.EvalChunk{sampleChunk()}, false)
	if !errors.Is(err, ErrBookContext) {
		t.Fatalf("err = %v, want ErrBookContext", err)
	}
	if len(chat.users) != 0 {
		t.Error("judged without a record source")
	}
	// fakeReader serves no records, so Run has no source either.
	if _, _, err := Run(context.Background(), fakeReader{chunks: []db.EvalChunk{sampleChunk()}}, j, &fakeWriter{}, RunOptions{Book: "x"}); !errors.Is(err, ErrBookContext) {
		t.Errorf("Run err = %v, want ErrBookContext", err)
	}
}

// TestRunUnusableRecordFailsOnlyItsBook: a record whose chapters did not
// decode fails that book's chunks (skipped, with the reason) while the other
// books are judged.
func TestRunUnusableRecordFailsOnlyItsBook(t *testing.T) {
	bad := *duneRecord()
	bad.BookDir, bad.Chapters, bad.DecodeError = "/books/Bad", nil, "chapters: unexpected end of JSON input"
	chunks := []db.EvalChunk{
		{ChunkID: "c1", TranscriptID: "t-bad", FilePath: "/books/Bad/01.m4b", Text: "x"},
		{ChunkID: "c2", TranscriptID: "t-ok", FilePath: "/books/Other/01.m4b", Text: "y"},
	}
	chat := &recordingChat{resp: `{"findings":[]}`}
	j := NewJudge(chat)
	mustSetPrompt(t, j, JudgePromptV3)
	j.SetBookRecords(&fakeRecords{recs: map[string]db.BookRecord{"/books/Bad": bad}})
	var errs []error
	_, stats, err := Run(context.Background(), fakeReader{chunks: chunks}, j, &fakeWriter{}, RunOptions{Book: "x",
		Observe: func(_ Result, e error) { errs = append(errs, e) }})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ChunksEvaluated != 1 || stats.ChunksSkipped != 1 || !strings.Contains(stats.FirstError, "chapters") {
		t.Errorf("stats = %+v", stats)
	}
	if !errors.Is(errs[0], ErrBookContext) || errs[1] != nil || len(chat.users) != 1 {
		t.Errorf("errs = %v, judge calls = %d", errs, len(chat.users))
	}
	if got := stats.BookContexts["t-bad"].Source; got != BookContextRecordError {
		t.Errorf("bad book source = %q", got)
	}
	// A record with a decode error but no ASIN is simply path_only.
	bad.ASIN = ""
	j.SetBookRecords(&fakeRecords{recs: map[string]db.BookRecord{"/books/Bad": bad}})
	if _, stats, err := Run(context.Background(), fakeReader{chunks: chunks}, j, &fakeWriter{}, RunOptions{Book: "x"}); err != nil || stats.ChunksSkipped != 0 {
		t.Errorf("no-ASIN record: stats %+v err %v", stats, err)
	}
}

// TestRunUsesReaderAsRecordSource: with no record source set, Run reads the
// records through its chunk reader when that serves them (db.DB does).
func TestRunUsesReaderAsRecordSource(t *testing.T) {
	dune := duneRecord()
	reader := readerWithRecords{
		fakeReader:  fakeReader{chunks: []db.EvalChunk{{ChunkID: "c", TranscriptID: "t", FilePath: dune.BookDir + "/01.m4b", Text: "x"}}},
		fakeRecords: &fakeRecords{recs: map[string]db.BookRecord{dune.BookDir: *dune}},
	}
	j := NewJudge(&fakeChat{resp: `{"findings":[]}`})
	mustSetPrompt(t, j, JudgePromptV3)
	_, stats, err := Run(context.Background(), reader, j, &fakeWriter{}, RunOptions{Book: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.BookContexts["t"].Source != BookContextASINRecord {
		t.Errorf("context = %+v", stats.BookContexts["t"])
	}
}

type readerWithRecords struct {
	fakeReader
	*fakeRecords
}

// recordingChat records every user message it is sent.
type recordingChat struct {
	resp    string
	systems []string
	users   []string
}

func (r *recordingChat) Complete(_ context.Context, system, user string) (string, error) {
	r.systems = append(r.systems, system)
	r.users = append(r.users, user)
	return r.resp, nil
}
func (r *recordingChat) Model() string { return "fake-judge" }

func mustSetPrompt(t *testing.T, j *Judge, p JudgePrompt) {
	t.Helper()
	if err := j.SetPrompt(p); err != nil {
		t.Fatal(err)
	}
}

func mustBookContext(t *testing.T, rec *db.BookRecord) BookContext {
	t.Helper()
	bc, err := NewBookContext(rec)
	if err != nil {
		t.Fatal(err)
	}
	return bc
}

func mustTokens(t *testing.T, s string) int {
	t.Helper()
	n, err := tokenizer.CountTokens(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
