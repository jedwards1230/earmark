package metaprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/jedwards1230/earmark/internal/library"
	"github.com/jedwards1230/earmark/internal/log"
)

// absMetadata is the metadata block returned by the ABS /api/items/{id} endpoint
// and the /api/libraries/{id}/items list. Both use the same flattened shape.
// Fields match the actual ABS API (confirmed against a live instance):
//   - authorName, narratorName, seriesName are flat strings (the items-list shape)
//   - asin is the Audible ASIN or numeric catalogue id
//   - description is the publisher blurb (may contain HTML; stored verbatim)
//   - genres is a string array; isbn is a flat string
type absMetadata struct {
	Title        string   `json:"title"`
	AuthorName   string   `json:"authorName"`
	NarratorName string   `json:"narratorName"`
	SeriesName   string   `json:"seriesName"`
	ASIN         string   `json:"asin"`
	Description  string   `json:"description"`
	Genres       []string `json:"genres"`
	ISBN         string   `json:"isbn"`
}

// absChapter is one element of media.chapters[] in a full item response.
// start/end are seconds (float64), matching transcript_chunks.start_sec/end_sec.
type absChapter struct {
	ID    int     `json:"id"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Title string  `json:"title"`
}

// absItemMedia is the media envelope in an item detail response.
type absItemMedia struct {
	Metadata absMetadata  `json:"metadata"`
	Chapters []absChapter `json:"chapters"`
}

// absItem is a library item from /api/items/{id} (full detail response).
type absItem struct {
	ID    string       `json:"id"`
	Media absItemMedia `json:"media"`
}

// absItemMinimal is the shape returned by /api/libraries/{id}/items (list).
// Metadata is a nested object with flattened author/narrator/series strings.
type absItemMinimal struct {
	ID    string       `json:"id"`
	Media absItemMedia `json:"media"`
}

// absLibraryItemsResponse is the envelope for GET /api/libraries/{id}/items.
type absLibraryItemsResponse struct {
	Results []absItemMinimal `json:"results"`
	Total   int              `json:"total"`
	Limit   int              `json:"limit"`
	Page    int              `json:"page"`
}

// ABSProvider implements MetadataProvider by querying an Audiobookshelf instance.
//
// # Lookup strategy
//
//  1. Extract the ASIN from filePath using library.ExtractASIN — it parses
//     bracketed catalogue ids like "[B08G9PRS1K]" from the directory name.
//  2. If no ASIN is found, return empty BookMeta (not-found signal — no error).
//  3. Otherwise, fetch all library items and scan for a matching asin field.
//     The ABS API has no server-side ASIN filter, so we iterate the list;
//     with 150–200 items this is one cheap JSON response (~100 KB).
//  4. On a hit, fetch the full item detail to get chapters (chapters are only
//     in the /api/items/{id} response, not the list).
//
// Not-found returns empty BookMeta (no error). Transport or HTTP errors are
// returned so ChainProvider can log and fall back to the next provider.
type ABSProvider struct {
	baseURL   string
	token     string
	libraryID string
	client    *http.Client
	log       log.Logger

	// embedded, when set, supplies the ASIN tag the ASR runner read from the
	// audio — the third ASIN source (CONTRACT §1.6). titles derives the book's
	// own title from its path for the cross-check that guards that source.
	// Both nil → only the directory and the filename are consulted.
	embedded EmbeddedASINSource
	titles   MetadataProvider
}

// EmbeddedASINSource reports the ASIN tag embedded in an audio file, as the
// ASR runner read it with ffprobe ("" when none or not yet transcribed).
// *db.DB implements it from transcripts.embedded_asin.
type EmbeddedASINSource interface {
	EmbeddedASIN(ctx context.Context, filePath string) (string, error)
}

// WithEmbeddedASIN enables the embedded tag as the third ASIN source. titles
// supplies the path-derived title the fetched record must match (normally the
// PathProvider); a record whose title does not match is a conflict and is not
// used.
func (p *ABSProvider) WithEmbeddedASIN(src EmbeddedASINSource, titles MetadataProvider) *ABSProvider {
	p.embedded = src
	p.titles = titles
	return p
}

// NewABSProvider constructs an ABSProvider. baseURL and token are injected from
// config (ABS_URL and ABS_TOKEN). libraryID is the configured library UUID.
// A custom http.Client may be supplied (non-nil, e.g. a test server client);
// when nil, a default client with a 10-second timeout is used.
func NewABSProvider(baseURL, token, libraryID string, client *http.Client) *ABSProvider {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &ABSProvider{
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     token,
		libraryID: libraryID,
		client:    client,
		log:       log.NewLogger("abs-provider"),
	}
}

// Lookup queries ABS for the book identified by the ASIN embedded in filePath.
// Returns empty BookMeta when no ASIN is extractable or the ASIN is not in ABS.
// Returns an error only for transport/HTTP failures.
//
// The ASIN is read from the book directory first (the `Author/Title [ASIN]/`
// layout used by audio-libation), then falls back to the file's own name. The
// fallback is what enables author-only layouts (audio-custom, audio-libro) where
// the book dir carries no ASIN but each track file does, e.g.
// `audio-custom/Benjamin Schumacher/The Science of Information [1629976067].m4b`.
//
// When both the directory and the filename carry an ASIN and they differ, the
// directory ASIN wins (it identifies the book; the filename can be a per-track
// variant) and a warning is logged to surface a likely misnaming.
//
// With WithEmbeddedASIN, the ASIN tag embedded in the audio is the third
// source, consulted only when neither the directory nor the filename carries
// an ASIN. A record found that way is used only if its title matches the
// book's path-derived title (library.TitlesMatch); otherwise the path metadata
// is returned with IdentityStatus "conflict".
func (p *ABSProvider) Lookup(ctx context.Context, filePath, sampleName string) (BookMeta, error) {
	asin, source := library.ResolveASIN(filePath, "")
	if source == library.ASINSourceDir {
		if fileASIN := library.ExtractASIN(filepath.Base(filePath)); fileASIN != "" && !strings.EqualFold(fileASIN, asin) {
			p.log.Warn("ASIN mismatch between directory and filename; using directory ASIN",
				"dir_asin", asin, "file_asin", fileASIN, "file", filePath)
		}
	}
	if asin == "" && p.embedded != nil {
		tag, err := p.embedded.EmbeddedASIN(ctx, filePath)
		if err != nil {
			p.log.Warn("embedded ASIN read failed (continuing without it)", "file", filePath, "error", err)
		}
		asin, source = library.ResolveASIN(filePath, tag)
	}
	if asin == "" {
		p.log.Debug("no ASIN in path or embedded tag, skipping ABS lookup", "file", filePath)
		return BookMeta{}, nil
	}

	itemID, minimal, err := p.findByASIN(ctx, asin)
	if err != nil {
		return BookMeta{}, fmt.Errorf("abs find by ASIN %q: %w", asin, err)
	}
	if itemID == "" {
		p.log.Debug("ASIN not found in ABS library", "asin", asin)
		return BookMeta{}, nil
	}

	// Fetch full detail for chapters.
	chapters, err := p.fetchChapters(ctx, itemID)
	if err != nil {
		return BookMeta{}, fmt.Errorf("abs fetch chapters for item %q: %w", itemID, err)
	}

	// The embedded tag is producer-supplied and the least trusted source: use
	// the record it names only when its title matches the book's own. A
	// mismatch (a wrong or reused tag) is recorded as a conflict and the book
	// keeps its path-derived identity, so a bad tag can never attach another
	// book's description and chapters.
	if source == library.ASINSourceEmbeddedTag {
		if conflict, own := p.embeddedConflict(ctx, filePath, sampleName, minimal.Title); conflict {
			p.log.Warn("embedded ASIN names a different book; recording conflict and not using it",
				"asin", asin, "record_title", minimal.Title, "book_title", own.Title, "file", filePath)
			own.IdentityStatus = IdentityConflict
			return own, nil
		}
	}

	meta := BookMeta{
		Title:       minimal.Title,
		Author:      minimal.AuthorName,
		Narrator:    minimal.NarratorName,
		Series:      minimal.SeriesName,
		ASIN:        minimal.ASIN,
		Description: strings.TrimSpace(minimal.Description),
		Genres:      cleanGenres(minimal.Genres),
		ISBN:        strings.TrimSpace(minimal.ISBN),
		Chapters:    chapters,
		Source:      "abs",

		ASINSource:     source,
		IdentityStatus: IdentityExact,
	}
	p.log.Debug("ABS lookup hit", "asin", asin, "title", meta.Title, "chapters", len(chapters))
	return meta, nil
}

// embeddedConflict cross-checks a record fetched by embedded tag against the
// book's path-derived title. It returns true (with the path metadata the book
// keeps) when the titles do not match. Without a title source there is
// nothing to check against, which is itself a conflict: the tag is never
// trusted unchecked.
func (p *ABSProvider) embeddedConflict(ctx context.Context, filePath, sampleName, recordTitle string) (bool, BookMeta) {
	if p.titles == nil {
		return true, BookMeta{}
	}
	own, err := p.titles.Lookup(ctx, filePath, sampleName)
	if err != nil || own.Title == "" {
		return true, own
	}
	return !library.TitlesMatch(recordTitle, own.Title), own
}

// findByASIN fetches the library items list and scans for the given ASIN.
// Returns ("", zero, nil) when not found. Paginates through all results if
// the library has more than the default page size.
func (p *ABSProvider) findByASIN(ctx context.Context, asin string) (string, absMetadata, error) {
	// The ABS items list uses page-based pagination. Fetch enough to cover typical
	// libraries; if total > limit we follow pages until found or exhausted.
	const pageSize = 500 // large enough to cover most collections in one request
	page := 0
	for {
		u := fmt.Sprintf("%s/api/libraries/%s/items?limit=%d&page=%d",
			p.baseURL, url.PathEscape(p.libraryID), pageSize, page)

		var resp absLibraryItemsResponse
		if err := p.getJSON(ctx, u, &resp); err != nil {
			return "", absMetadata{}, err
		}

		for _, item := range resp.Results {
			if strings.EqualFold(item.Media.Metadata.ASIN, asin) {
				return item.ID, item.Media.Metadata, nil
			}
		}

		// Check whether we've seen all pages.
		fetched := page*pageSize + len(resp.Results)
		if fetched >= resp.Total || len(resp.Results) == 0 {
			return "", absMetadata{}, nil
		}
		page++
	}
}

// fetchChapters fetches the full item detail and returns the mapped chapters.
func (p *ABSProvider) fetchChapters(ctx context.Context, itemID string) ([]Chapter, error) {
	url := fmt.Sprintf("%s/api/items/%s", p.baseURL, url.PathEscape(itemID))
	var item absItem
	if err := p.getJSON(ctx, url, &item); err != nil {
		return nil, err
	}
	return mapABSChapters(item.Media.Chapters), nil
}

// cleanGenres trims each genre and drops blanks, returning nil when nothing
// remains so an empty list is stored as NULL ("not populated"), not '{}'.
func cleanGenres(in []string) []string {
	var out []string
	for _, g := range in {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// mapABSChapters converts ABS chapter objects to the canonical Chapter type.
// The ABS chapter.id is an ordinal integer; we preserve it as the Chapter.Index.
// Titles are run through CleanChapterTitles so new lookups store clean titles
// (the provider original survives in Chapter.RawTitle).
func mapABSChapters(absChaps []absChapter) []Chapter {
	if len(absChaps) == 0 {
		return nil
	}
	out := make([]Chapter, len(absChaps))
	for i, c := range absChaps {
		out[i] = Chapter{
			Index:    c.ID,
			Title:    c.Title,
			StartSec: c.Start,
			EndSec:   c.End,
		}
	}
	return CleanChapterTitles(out)
}

// getJSON executes a GET request authenticated with the configured bearer token
// and decodes the JSON response body into dst.
func (p *ABSProvider) getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// 404 is treated as "not found, no error": dst is left at its zero value
		// and the caller (findByASIN / fetchChapters) interprets an empty struct
		// as "not present in library". This is intentional and documented.
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Limit response body to 10 MiB as a safeguard against oversized payloads.
	const maxResponseBytes = 10 * 1024 * 1024
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(dst); err != nil {
		return fmt.Errorf("decode response from %s: %w", url, err)
	}
	return nil
}
