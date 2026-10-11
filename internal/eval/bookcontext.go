package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/tokenizer"
)

// Book context (judge@v3, CONTRACT §2.15 "Book context"). The v3 user message
// carries a short "Book record" block rendered from the book's catalogue
// record (book_metadata), so the judge can spell a misheard name the way the
// book does. The block is rendered only when the record's identity is
// resolved — a non-empty ASIN; a book with no record, no catalogue match or an
// identity conflict (catalogue columns cleared at ingest, CONTRACT §1.6) gets
// the exact path-only prompt.
//
// Everything here is pure and deterministic: the same record always renders
// the same bytes, so book_context_sha256 identifies what the judge was shown.

// Book context sources, recorded per transcript (RunStats.BookContexts,
// --dump).
const (
	// BookContextASINRecord: the user message carried the Book record block.
	BookContextASINRecord = "asin_record"
	// BookContextPathOnly: no usable record (none, no ASIN, identity
	// conflict) or a prompt version without book context — the judge saw
	// only the book/track path.
	BookContextPathOnly = "path_only"
)

// bookContextVersion names the block's rendering rules. It is a v3 recipe
// param (book_context), so a change to the extraction or layout below must
// bump it — the static layout strings are also part of the v3 prompt hash.
const bookContextVersion = "record@v1"

// Layout of the block. These strings are hashed into judge@v3's prompt
// sha256 (judgePromptSHA256V3); the per-book content is not.
const (
	bookRecordHeader = "Book record:\n"
	bookRecordLine   = "- %s: %s\n"
	bookRecordNames  = "Names"
)

// Caps. The whole block — header lines included — stays within
// maxBookRecordTokens cl100k tokens, counted with the embedded offline
// tokenizer (internal/tokenizer, the chunker's): invented names tokenise at
// barely 2 characters a token, so no character budget is both safe and
// useful. cl100k is a fixed encoding, so the count — and the rendered bytes —
// are deterministic. There is deliberately no fallback budget: one would
// render different bytes under the same record@v1, so a tokenizer that fails
// to load fails the book's context instead.
const (
	maxBookRecordTokens = 200
	maxBookRecordNames  = 40
)

// bookRecordFieldCaps are the per-field rune caps tried in order: the
// header (title, author, series, narrator) is clipped to the first cap at
// which it fits the token budget on its own, so an absurd catalogue title can
// never push the block over.
var bookRecordFieldCaps = []int{80, 48, 32, 20}

// BookContext is the book context one transcript's chunks were judged with.
type BookContext struct {
	// Source is BookContextASINRecord, BookContextPathOnly or
	// BookContextRecordError.
	Source string `json:"source"`
	// SHA256 is the hex sha256 of Block; "" for path_only.
	SHA256 string `json:"sha256,omitempty"`
	// Block is the rendered "Book record" block exactly as inserted in the
	// user message (ending in a newline); "" for path_only.
	Block string `json:"-"`
	// err is set for BookContextRecordError: the book's record exists but
	// cannot be rendered, so its chunks fail instead of being judged.
	err error
}

// BookContextRecordError: the book has a record that could not be used (its
// stored chapters do not decode, or it could not be rendered). Its chunks are
// not judged; each fails with the reason, so the transcript records an eval
// failure and stays re-judgeable.
const BookContextRecordError = "record_error"

// pathOnlyContext is the context of a chunk with no usable record.
var pathOnlyContext = BookContext{Source: BookContextPathOnly}

// NewBookContext renders rec's book context. A nil record or one without an
// ASIN (unmatched, identity conflict) is path_only. An error means the
// record could not be rendered deterministically (the tokenizer failed).
func NewBookContext(rec *db.BookRecord) (BookContext, error) {
	if rec == nil || strings.TrimSpace(rec.ASIN) == "" {
		return pathOnlyContext, nil
	}
	block, err := renderBookRecord(rec)
	if err != nil {
		return BookContext{}, err
	}
	if block == "" {
		return pathOnlyContext, nil
	}
	sum := sha256.Sum256([]byte(block))
	return BookContext{Source: BookContextASINRecord, SHA256: hex.EncodeToString(sum[:]), Block: block}, nil
}

// renderBookRecord renders the Book record block: the header fields (title,
// author, series, narrator; each omitted when empty, clipped at a word
// boundary to the largest bookRecordFieldCaps cap that fits the budget), then
// a Names line of up to maxBookRecordNames proper nouns added whole, in
// priority order, while the block stays within maxBookRecordTokens. Returns
// "" when the record has nothing to show.
func renderBookRecord(rec *db.BookRecord) (string, error) {
	var head string
	var shown []string
	for _, limit := range bookRecordFieldCaps {
		head, shown = renderBookRecordHeader(rec, limit)
		ok, err := withinBookRecordBudget(head)
		if err != nil {
			return "", err
		}
		if ok {
			break
		}
	}

	names := bookRecordNouns(rec, shown)
	block := head
	for i := range names {
		next := head + fmt.Sprintf(bookRecordLine, bookRecordNames, strings.Join(names[:i+1], ", "))
		ok, err := withinBookRecordBudget(next)
		if err != nil {
			return "", err
		}
		if !ok {
			break // whole names only, in priority order
		}
		block = next
	}
	if block == bookRecordHeader {
		return "", nil
	}
	return block, nil
}

// renderBookRecordHeader renders the block's header lines with every field
// clipped to limit runes, and returns the field values shown.
func renderBookRecordHeader(rec *db.BookRecord, limit int) (string, []string) {
	header := [][2]string{
		{"Title", clipRunes(cleanField(rec.Title), limit)},
		{"Author", clipRunes(cleanField(rec.Author), limit)},
		{"Series", clipRunes(seriesField(rec), limit)},
		{"Narrator", clipRunes(cleanField(rec.Narrator), limit)},
	}
	var b strings.Builder
	b.WriteString(bookRecordHeader)
	var shown []string
	for _, f := range header {
		if f[1] == "" {
			continue
		}
		fmt.Fprintf(&b, bookRecordLine, f[0], f[1])
		shown = append(shown, f[1])
	}
	return b.String(), shown
}

// withinBookRecordBudget reports whether block fits maxBookRecordTokens.
func withinBookRecordBudget(block string) (bool, error) {
	n, err := tokenizer.CountTokens(block)
	if err != nil {
		return false, fmt.Errorf("count book record tokens: %w", err)
	}
	return n <= maxBookRecordTokens, nil
}

// seriesField renders the series as "Name #N" entries, comma-joined.
func seriesField(rec *db.BookRecord) string {
	parts := make([]string, 0, len(rec.Series))
	for _, s := range rec.Series {
		name := cleanField(s.Name)
		if name == "" {
			continue
		}
		if seq := cleanField(s.Sequence); seq != "" {
			name += " #" + seq
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ", ")
}

// cleanField collapses whitespace (incl. newlines) to single spaces.
func cleanField(s string) string { return strings.Join(strings.Fields(s), " ") }

// clipRunes cuts s to at most n runes ending in "…", at the last word
// boundary when there is one, so a name is never cut mid-word. s fits as is
// when it is at most n runes.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	kept := string(r[:n-1])
	if r[n-1] != ' ' { // the cut falls inside a word: back off to its start
		if i := strings.LastIndexByte(kept, ' '); i > 0 {
			kept = kept[:i]
		}
	}
	return strings.TrimRight(kept, " ,;:-") + "…"
}

// bookRecordNouns picks the record's proper nouns in priority order: the
// stored bias_terms, then names in the chapter titles, then names in the
// description (HTML stripped, entities decoded). Deduplicated
// case-insensitively (curly and straight apostrophes are the same); a name
// already visible in a header field is skipped, and so is one contained in
// another candidate ("Paul" when "Paul Atreides" is listed — whichever comes
// first); at most maxBookRecordNames.
func bookRecordNouns(rec *db.BookRecord, header []string) []string {
	shown := make([]string, 0, len(header))
	for _, h := range header {
		shown = append(shown, " "+nameKey(h)+" ")
	}
	var cands []string
	seen := map[string]bool{}
	add := func(name string) {
		name = cleanField(strings.Trim(normalizeQuotes(name), nounTrim))
		key := nameKey(name)
		if key == "" || seen[key] || noiseNames[key] {
			return
		}
		seen[key] = true
		for _, s := range shown {
			if strings.Contains(s, " "+key+" ") {
				return // already visible in a header field
			}
		}
		cands = append(cands, name)
	}

	for _, t := range rec.BiasTerms {
		if usableBiasTerm(t) {
			add(t)
		}
	}

	// Chapter titles and the description are prose: extract capitalized
	// sequences. A capitalized word at a sentence start — or anywhere in a
	// Title Case chapter title, where capitals say nothing — is only a name
	// when it is also capitalized mid-sentence somewhere in these sources;
	// multi-word runs ("Bene Gesserit") are kept as they are.
	chapters := append(rec.Chapters[:0:0], rec.Chapters...)
	sort.SliceStable(chapters, func(a, b int) bool { return chapters[a].Index < chapters[b].Index })
	var seqs [][]capSeq
	for _, c := range chapters {
		seqs = append(seqs, capitalizedSequences(c.Title, isTitleCase(c.Title)))
	}
	if d := stripHTML(rec.Description); strings.TrimSpace(d) != "" {
		seqs = append(seqs, capitalizedSequences(d, false))
	}
	known := map[string]bool{}
	for _, s := range seqs {
		for _, q := range s {
			if q.titleCase {
				continue
			}
			for i, w := range q.words {
				if i > 0 || !q.initial {
					known[w] = true
				}
			}
		}
	}
	for _, s := range seqs {
		for _, q := range s {
			words := q.words
			if q.initial && !known[words[0]] && len(words) > 1 && nameParticles[words[1]] {
				// "Finally de Gaulle": an uncorroborated sentence opener
				// joined only by a particle is not part of the name.
				words = words[1:]
				for len(words) > 0 && nameParticles[words[0]] {
					words = words[1:]
				}
			} else if len(words) == 1 && q.initial && !known[words[0]] {
				continue
			}
			if len(words) > 0 {
				add(strings.Join(words, " "))
			}
		}
	}

	// Drop a candidate contained, as whole words, in another one.
	keys := make([]string, len(cands))
	for i, c := range cands {
		keys[i] = " " + nameKey(c) + " "
	}
	out := make([]string, 0, min(len(cands), maxBookRecordNames))
	for i, c := range cands {
		subsumed := false
		for j, k := range keys {
			if j != i && len(k) > len(keys[i]) && strings.Contains(k, keys[i]) {
				subsumed = true
				break
			}
		}
		if !subsumed && len(out) < maxBookRecordNames {
			out = append(out, c)
		}
	}
	return out
}

// nounTrim is the punctuation trimmed off a name's ends.
const nounTrim = " \t\n\"'“”‘’()[]{}<>.,;:!?*_—–-"

// quoteNormalizer folds curly apostrophes to ASCII and pads em/en dashes
// with spaces, so "Leto—Ghanima" is two names, not one unusable token.
var quoteNormalizer = strings.NewReplacer("’", "'", "‘", "'", "—", " — ", "–", " – ")

func normalizeQuotes(s string) string { return quoteNormalizer.Replace(s) }

// nameKey is a name's dedupe and "already shown" key: lowercase, curly
// apostrophes folded, and every rune that is not a letter, digit or
// apostrophe folded to a single space, so "Mary" is found in a "Project Hail
// Mary: A Novel" title.
func nameKey(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(normalizeQuotes(s)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}), " ")
}

// noiseNames are capitalized phrases common in publisher blurbs that are
// never in-story names.
var noiseNames = map[string]bool{
	"new york times": true, "usa today": true, "wall street journal": true,
	"audible studios": true, "audible originals": true, "audible": true,
}

// usableBiasTerm filters a stored bias term the same way prose is read: its
// words must form one name run (name words, an optional leading title such as
// "Lord", lowercase particles such as "da" only inside).
func usableBiasTerm(t string) bool {
	seqs := capitalizedSequences(strings.Trim(t, nounTrim), false)
	return len(seqs) == 1 && strings.Join(seqs[0].words, " ") == cleanField(strings.Trim(normalizeQuotes(t), nounTrim))
}

// capSeq is one run of consecutive capitalized name words in prose.
type capSeq struct {
	words []string
	// initial: the run starts a sentence (or the text), where capitalization
	// says nothing about whether the first word is a name.
	initial bool
	// titleCase: the run is from a Title Case chapter title, so none of its
	// capitals corroborate a name.
	titleCase bool
}

// nameToken is one whitespace-separated word of prose with its punctuation
// split off.
type nameToken struct {
	w     string // the word, trimmed of punctuation and a possessive
	lead  bool   // punctuation before the word
	trail string // punctuation (and a possessive) after the word
}

func nameTokens(line string) []nameToken {
	var out []nameToken
	for _, raw := range strings.Fields(line) {
		lead := strings.TrimLeft(raw, nounTrim)
		w := strings.TrimRight(lead, nounTrim)
		t := nameToken{w: w, lead: len(lead) != len(raw), trail: lead[len(w):]}
		if stem, ok := strings.CutSuffix(t.w, "'s"); ok && stem != "" {
			t.w, t.trail = stem, "'s"+t.trail // a possessive ends the name
		}
		out = append(out, t)
	}
	return out
}

// capitalizedSequences splits prose into runs of name words (nameWord). A
// run may open with a title or a first name that is also a common word
// ("Lord Leto", "Will Treaty"; nameLeads) and may hold lowercase particles
// ("Leonardo da Vinci"; nameParticles), each only when a name word follows
// with no punctuation between. A run breaks at any other word and at
// punctuation (an em dash included); a sentence boundary (. ! ? or a line
// break) marks the next word as sentence-initial. With titleCase every run is
// marked initial.
func capitalizedSequences(text string, titleCase bool) []capSeq {
	var out []capSeq
	var cur capSeq
	flush := func() {
		if len(cur.words) > 0 {
			cur.initial = cur.initial || titleCase
			cur.titleCase = titleCase
			out = append(out, cur)
		}
		cur = capSeq{}
	}
	for _, line := range strings.Split(normalizeQuotes(text), "\n") {
		sentenceStart := true // a line break is a sentence boundary
		flush()
		toks := nameTokens(line)
		for i, t := range toks {
			if t.lead && len(cur.words) > 0 {
				flush()
			}
			nameFollows := t.trail == "" && i+1 < len(toks) && !toks[i+1].lead && nameWord(toks[i+1].w)
			switch {
			case nameWord(t.w):
				if len(cur.words) == 0 {
					cur.initial = sentenceStart
				}
				cur.words = append(cur.words, t.w)
			case len(cur.words) == 0 && nameFollows && isCapitalized(t.w) && nameLeads[strings.ToLower(t.w)]:
				cur.initial = sentenceStart
				cur.words = append(cur.words, t.w)
			case len(cur.words) > 0 && nameFollows && nameParticles[t.w]:
				cur.words = append(cur.words, t.w)
			default:
				flush()
			}
			if t.w != "" {
				sentenceStart = false
			}
			if t.trail != "" {
				flush()
				if strings.ContainsAny(t.trail, ".!?") {
					sentenceStart = true
				}
			}
		}
	}
	flush()
	return out
}

// nameLeads are capitalized words that open a name only when another name
// word follows: titles ("Lord Leto") and first names that are also common
// words ("Will", "May"). Alone they are stopwords.
var nameLeads = map[string]bool{
	"will": true, "may": true, "lord": true, "lady": true, "sir": true, "dame": true,
	"mr": true, "mrs": true, "ms": true, "dr": true, "madam": true, "saint": true, "st": true,
}

// nameParticles are lowercase particles that stay inside a name between two
// name words ("Leonardo da Vinci", "Ludwig van Beethoven").
var nameParticles = map[string]bool{
	"da": true, "de": true, "del": true, "della": true, "di": true, "du": true, "des": true,
	"van": true, "von": true, "der": true, "den": true, "ter": true, "ten": true,
	"le": true, "la": true, "bin": true, "ibn": true, "al": true, "y": true,
}

// isCapitalized reports whether w starts with an upper-case letter.
func isCapitalized(w string) bool {
	first, _ := utf8.DecodeRuneInString(w)
	return unicode.IsUpper(first)
}

// isTitleCase reports whether a chapter title capitalizes every word that is
// not a stopword ("The Arecibo Message", "Ghanima's Plan") — capitals there
// are typography, not a name signal. A title with a lower-case content word
// ("The arrival of Leto") is sentence case.
func isTitleCase(title string) bool {
	content := 0
	for _, raw := range strings.Fields(title) {
		w := strings.Trim(raw, nounTrim)
		first, _ := utf8.DecodeRuneInString(w)
		if !unicode.IsLetter(first) || nameStopwords[strings.ToLower(w)] {
			continue
		}
		if !unicode.IsUpper(first) {
			return false
		}
		content++
	}
	return content > 0
}

// Note a multi-word run at a sentence start keeps its first word ("Meet
// Ryland Grace" stays whole): dropping it would also drop "Ryland" from
// "Ryland Grace wakes", and a wasted word costs less than a missing name.

// nameWord reports whether w can be (part of) a name: it starts with an
// upper-case letter (after an elided "d'" / "l'" particle: "d'Artagnan"),
// has a lower-case letter (all-caps words — headings, roman numerals,
// acronyms — are skipped), is at least two letters, has no digits, and is
// not a stopword or a structural word such as "Chapter".
func nameWord(w string) bool {
	if lw := strings.ToLower(w); len(w) > 2 && (strings.HasPrefix(lw, "d'") || strings.HasPrefix(lw, "l'")) {
		return nameWord(w[2:])
	}
	if utf8.RuneCountInString(w) < 2 {
		return false
	}
	if !isCapitalized(w) {
		return false
	}
	hasLower := false
	for _, r := range w {
		switch {
		case unicode.IsDigit(r):
			return false
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsLetter(r), r == '\'', r == '-':
		default:
			return false
		}
	}
	if !hasLower || isRomanNumeral(w) {
		return false
	}
	return !nameStopwords[strings.ToLower(w)]
}

// romanNumeral matches a well-formed roman numeral (any case).
var romanNumeral = regexp.MustCompile(`(?i)^M{0,4}(CM|CD|D?C{0,3})(XC|XL|L?X{0,3})(IX|IV|V?I{0,3})$`)

// isRomanNumeral reports whether w is a roman numeral written in capitals
// ("IV", "XII"). Mixed-case words that happen to spell one ("Mix", "Liv")
// are names, not numerals.
func isRomanNumeral(w string) bool {
	return w != "" && w == strings.ToUpper(w) && romanNumeral.MatchString(w)
}

// nameStopwords are capitalized words that are never names on their own:
// function words, sentence openers, honorifics, number words and the
// structural words of chapter titles. A stopword also breaks a run, so
// "The Bene Gesserit" yields "Bene Gesserit".
var nameStopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		a about after again against all also although am an and another any are as at
		be because been before being both but by can could did do does down during each
		either even every few for from further had has have having he her here hers herself
		him himself his how however if in into is it its itself just like may me might more
		most much must my myself neither never no nor not now of off on once only or other
		our ours out over own same she should since so some still such than that the their
		them then there these they this those though through thus to too under until up
		upon us very was we were what when where whether which while who whom whose why will
		with within without would yet you your yours
		also meanwhile soon later today tonight tomorrow yesterday perhaps maybe yes oh
		mr mrs ms dr sir madam lord lady
		chapter chapters part parts book books volume prologue epilogue interlude introduction
		foreword afterword preface appendix acknowledgments acknowledgements contents section
		act scene track disc end opening credits
		one two three four five six seven eight nine ten eleven twelve thirteen fourteen
		fifteen sixteen seventeen eighteen nineteen twenty thirty forty fifty sixty seventy
		eighty ninety hundred thousand first second third fourth fifth sixth seventh eighth
		ninth tenth last final
		bestselling bestseller author narrator series novel audiobook edition
		unabridged abridged award winning`) {
		m[w] = true
	}
	return m
}()

// stripHTML removes tags (a block-level tag becomes a line break, any other a
// space) and the contents of script/style elements, then decodes entities.
// A copy of internal/decide's StripHTML: internal/eval never imports decide.
func stripHTML(s string) string {
	var b strings.Builder
	skip := "" // inside <script> or <style>: the closing tag to wait for
	for i := 0; i < len(s); {
		if s[i] != '<' {
			if skip == "" {
				b.WriteByte(s[i])
			}
			i++
			continue
		}
		end := strings.IndexByte(s[i:], '>')
		if end < 0 { // a stray '<' — keep it as text
			if skip == "" {
				b.WriteString(s[i:])
			}
			break
		}
		tag := strings.ToLower(strings.TrimSpace(s[i+1 : i+end]))
		i += end + 1
		closing := strings.HasPrefix(tag, "/")
		name := strings.TrimLeft(tag, "/")
		if j := strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || r == '/' }); j >= 0 {
			name = name[:j]
		}
		if skip != "" {
			if closing && name == skip {
				skip = ""
			}
			continue
		}
		switch {
		case !closing && (name == "script" || name == "style"):
			skip = name
		case htmlBlockTags[name]:
			b.WriteByte('\n')
		default:
			b.WriteByte(' ')
		}
	}
	return html.UnescapeString(b.String())
}

// htmlBlockTags end a line when stripped: a paragraph or list item is a
// sentence boundary even without a full stop.
var htmlBlockTags = map[string]bool{
	"p": true, "br": true, "div": true, "li": true, "ul": true, "ol": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"tr": true, "blockquote": true,
}

// ErrBookContext marks a book context that could not be had: the record read
// failed, the judge has no record source, or one book's record is unusable
// (errors.Is). Never silently replaced by path_only.
var ErrBookContext = errors.New("judge book context unavailable")

// BookRecordReader is the read-only DB slice the v3 judge reads book records
// through (db.DB implements it): at most db.MaxDecideBookBatch dirs per call,
// keyed by book_dir = filepath.Dir(transcript file_path).
type BookRecordReader interface {
	GetBookRecords(ctx context.Context, bookDirs []string) (map[string]db.BookRecord, error)
}

// bookContexts resolves the book context of every transcript in chunks: one
// GetBookRecords call per db.MaxDecideBookBatch distinct book dirs. A dir with
// no row (or a row with no ASIN) is path_only. A read error fails the whole
// call (ErrBookContext; it never degrades to path_only silently); a single
// unusable record — chapters that do not decode, or a render failure — is a
// BookContextRecordError for that book only. Keyed by book dir.
func bookContexts(ctx context.Context, r BookRecordReader, chunks []db.EvalChunk) (map[string]BookContext, error) {
	var dirs []string
	seen := map[string]bool{}
	for _, c := range chunks {
		d := filepath.Dir(c.FilePath)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	out := make(map[string]BookContext, len(dirs))
	for start := 0; start < len(dirs); start += db.MaxDecideBookBatch {
		batch := dirs[start:min(start+db.MaxDecideBookBatch, len(dirs))]
		recs, err := r.GetBookRecords(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("%w: read book records: %w", ErrBookContext, err)
		}
		for _, d := range batch {
			rec, ok := recs[d]
			if !ok {
				out[d] = pathOnlyContext
				continue
			}
			if strings.TrimSpace(rec.ASIN) != "" && rec.DecodeError != "" {
				out[d] = recordError(d, errors.New(rec.DecodeError))
				continue
			}
			bc, err := NewBookContext(&rec)
			if err != nil {
				bc = recordError(d, err)
			}
			out[d] = bc
		}
	}
	return out, nil
}

// recordError is the context of a book whose record cannot be used.
func recordError(dir string, err error) BookContext {
	return BookContext{Source: BookContextRecordError, err: fmt.Errorf("%w: book record %s: %w", ErrBookContext, dir, err)}
}
