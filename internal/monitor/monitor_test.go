package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/metaprovider"
)

func TestIsAudioFile(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"chapter01.mp3", true},
		{"chapter01.MP3", true},
		{"book.m4a", true},
		{"audiobook.m4b", true},
		{"music.wav", true},
		{"audio.flac", true},
		{"voice.aac", true},
		{"sound.ogg", true},
		{"audio.wma", true},
		{"metadata.json", false},
		{"cover.jpg", false},
		{"readme.txt", false},
		{"document.pdf", false},
		{"", false},
		{"noextension", false},
		{".mp3", true},
		// macOS AppleDouble sidecars keep the real extension but are junk.
		{"._chapter01.mp3", false},
		{"._Thinking Fast and Slow - Track 1.mp3", false},
		{"/books/audio-libro/Andy Clark/._The Experience Machine.mp3", false},
		{".DS_Store", false},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := isAudioFile(tt.filename)
			if got != tt.expected {
				t.Errorf("isAudioFile(%q) = %v, want %v", tt.filename, got, tt.expected)
			}
		})
	}
}

// fakeDB implements DBInterface for unit tests — no real DB needed. It models
// the production dedup invariant: a job is unique by both checksum and
// file_path. Maps are lazily initialized so `&fakeDB{}` works.
type fakeDB struct {
	inserted     map[string]string // checksum -> jobID
	insertedPath map[string]string // file_path -> jobID
	insertCalls  int               // times a NEW job was actually created
	pruned       int               // times PruneAppleDoubleJobs was called
	audioBytes   map[string]int64  // jobID -> audio_bytes recorded

	// book_metadata tracking
	bookMetadata  map[string]metaprovider.BookMeta // bookDir -> BookMeta last written
	bookMetaErr   error                            // if set, UpsertBookMetadata returns this error
	bookMetaCalls int                              // number of UpsertBookMetadata calls

	// pipeline_events tracking
	events     []db.PipelineEvent // captured by AppendEvent
	pruneCalls int                // times PruneEvents was called
}

func (f *fakeDB) InsertJobIfAbsent(_ context.Context, filePath, checksum string) (string, bool, error) {
	if f.inserted == nil {
		f.inserted = map[string]string{}
	}
	if f.insertedPath == nil {
		f.insertedPath = map[string]string{}
	}
	if id, ok := f.inserted[checksum]; ok {
		return id, false, nil
	}
	if id, ok := f.insertedPath[filePath]; ok { // path dedup (the mid-copy-hash fix)
		return id, false, nil
	}
	f.insertCalls++
	id := "job-" + checksum[:8]
	f.inserted[checksum] = id
	f.insertedPath[filePath] = id
	return id, true, nil
}

func (f *fakeDB) IsPathQueued(_ context.Context, filePath string) (bool, error) {
	_, ok := f.insertedPath[filePath]
	return ok, nil
}

func (f *fakeDB) PruneAppleDoubleJobs(context.Context) (int, error) {
	f.pruned++
	return 0, nil
}

func (f *fakeDB) UpsertAudioBytes(_ context.Context, jobID string, bytes int64) error {
	if f.audioBytes == nil {
		f.audioBytes = map[string]int64{}
	}
	f.audioBytes[jobID] = bytes
	return nil
}

func (f *fakeDB) AppendEvent(_ context.Context, e db.PipelineEvent) error {
	f.events = append(f.events, e)
	return nil
}

func (f *fakeDB) PruneEvents(context.Context) (int64, error) {
	f.pruneCalls++
	return 0, nil
}

func (f *fakeDB) UpsertBookMetadata(_ context.Context, bookDir string, meta metaprovider.BookMeta) error {
	f.bookMetaCalls++
	if f.bookMetaErr != nil {
		return f.bookMetaErr
	}
	if f.bookMetadata == nil {
		f.bookMetadata = map[string]metaprovider.BookMeta{}
	}
	f.bookMetadata[bookDir] = meta
	return nil
}

// stubMetaProvider implements metaprovider.MetadataProvider for tests.
type stubMetaProvider struct {
	title  string
	author string
	source string
	err    error // if set, Lookup returns this error
}

func (s *stubMetaProvider) Lookup(_ context.Context, _, _ string) (metaprovider.BookMeta, error) {
	if s.err != nil {
		return metaprovider.BookMeta{}, s.err
	}
	return metaprovider.BookMeta{
		Title:  s.title,
		Author: s.author,
		Source: s.source,
	}, nil
}

func newTestMonitor(dir string, db DBInterface) *FileMonitor {
	return newTestMonitorWithMeta(dir, db, &stubMetaProvider{
		title:  "Test Book",
		author: "Test Author",
		source: "path",
	})
}

func newTestMonitorWithMeta(dir string, db DBInterface, meta metaprovider.MetadataProvider) *FileMonitor {
	cfg := &config.Config{BooksDir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	return &FileMonitor{
		cfg:    cfg,
		db:     db,
		meta:   meta,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		log:    log.NewLogger("monitor-test"),
		// Fast stability tuning so tests aren't slow.
		stabilityInterval: time.Millisecond,
		stabilityCount:    2,
		stabilityTimeout:  2 * time.Second,
	}
}

func TestMonitorEnqueueFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "chapter01.mp3")
	if err := os.WriteFile(filePath, []byte("audio data"), 0600); err != nil {
		t.Fatalf("create file: %v", err)
	}

	db := &fakeDB{inserted: make(map[string]string)}
	fm := newTestMonitor(dir, db)

	fm.enqueueFile(filePath)
	if len(db.inserted) != 1 {
		t.Errorf("expected 1 job inserted, got %d", len(db.inserted))
	}

	// audio_bytes must be recorded for the enqueued job (per-run observability).
	if len(db.audioBytes) != 1 {
		t.Fatalf("expected audio_bytes recorded for 1 job, got %d", len(db.audioBytes))
	}
	for _, b := range db.audioBytes {
		if b != int64(len("audio data")) {
			t.Errorf("expected audio_bytes=%d, got %d", len("audio data"), b)
		}
	}

	// A pipeline_events enqueue/finish row is emitted for the new job (CONTRACT §1.7).
	if len(db.events) != 1 {
		t.Fatalf("expected 1 enqueue event, got %d", len(db.events))
	}
	ev := db.events[0]
	if ev.Stage != db.events[0].Stage || db.events[0].Stage != "enqueue" {
		t.Errorf("event stage = %q, want enqueue", ev.Stage)
	}
	if ev.Event != "finish" {
		t.Errorf("event verb = %q, want finish", ev.Event)
	}
	if ev.FilePath != filePath {
		t.Errorf("event file_path = %q, want %q", ev.FilePath, filePath)
	}

	// Second call — should be idempotent (no new job, no new event).
	fm.enqueueFile(filePath)
	if len(db.inserted) != 1 {
		t.Errorf("expected still 1 job after duplicate enqueue, got %d", len(db.inserted))
	}
	if len(db.events) != 1 {
		t.Errorf("duplicate enqueue must not emit a second event; got %d events", len(db.events))
	}
}

func TestMonitorIgnoresNonAudioFiles(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(filePath, []byte("{}"), 0600); err != nil {
		t.Fatalf("create file: %v", err)
	}

	db := &fakeDB{inserted: make(map[string]string)}
	fm := newTestMonitor(dir, db)
	fm.handleCreate(filePath)

	if len(db.inserted) != 0 {
		t.Errorf("expected no jobs for non-audio file, got %d", len(db.inserted))
	}
}

func TestMonitorScan(t *testing.T) {
	dir := t.TempDir()
	// Create two audio files with distinct content (different checksums) and one non-audio file.
	audioFiles := map[string][]byte{
		"ch01.mp3": []byte("audio data for chapter one"),
		"ch02.m4b": []byte("audio data for chapter two"),
	}
	for name, content := range audioFiles {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0600); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "cover.jpg"), []byte("image data"), 0600); err != nil {
		t.Fatalf("create cover.jpg: %v", err)
	}
	// macOS AppleDouble sidecar with a real audio extension — must be skipped.
	if err := os.WriteFile(filepath.Join(dir, "._ch01.mp3"), []byte("apple metadata"), 0600); err != nil {
		t.Fatalf("create AppleDouble: %v", err)
	}

	db := &fakeDB{inserted: make(map[string]string)}
	fm := newTestMonitor(dir, db)
	if err := fm.scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(db.inserted) != 2 {
		t.Errorf("expected 2 jobs after scan (AppleDouble skipped), got %d", len(db.inserted))
	}

	// audio_bytes must be recorded for each enqueued file (per-run observability).
	if len(db.audioBytes) != 2 {
		t.Fatalf("expected audio_bytes recorded for 2 jobs, got %d", len(db.audioBytes))
	}
	for jobID, b := range db.audioBytes {
		if b <= 0 {
			t.Errorf("job %s: expected positive audio_bytes, got %d", jobID, b)
		}
	}
	// Verify each recorded size matches the actual file content.
	for name, content := range audioFiles {
		path := filepath.Join(dir, name)
		jobID, ok := db.insertedPath[path]
		if !ok {
			t.Errorf("no job found for path %s", path)
			continue
		}
		if got := db.audioBytes[jobID]; got != int64(len(content)) {
			t.Errorf("%s: expected audio_bytes=%d, got %d", name, len(content), got)
		}
	}
}

func TestMonitorScanSkipsKnownPaths(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"ch01.mp3": "one", "ch02.m4b": "two"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	db := &fakeDB{}
	fm := newTestMonitor(dir, db)

	if err := fm.scan(); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if db.insertCalls != 2 {
		t.Fatalf("first scan: want 2 inserts, got %d", db.insertCalls)
	}

	// Second scan: every path is already queued → no re-hash, no new inserts.
	if err := fm.scan(); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if db.insertCalls != 2 {
		t.Errorf("second scan: want still 2 inserts (known paths skipped), got %d", db.insertCalls)
	}
}

// TestMonitorDuplicatePathDifferentChecksum models the mid-copy-hash bug: the
// same file enqueued twice with different content (a partial hash then the
// finished hash) must produce ONE job, because dedup is by file_path, not just
// checksum.
func TestMonitorDuplicatePathDifferentChecksum(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ch01.mp3")
	if err := os.WriteFile(p, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	db := &fakeDB{}
	fm := newTestMonitor(dir, db)

	fm.enqueueFile(p) // hashes "partial"
	if err := os.WriteFile(p, []byte("the complete file content"), 0600); err != nil {
		t.Fatal(err)
	}
	fm.enqueueFile(p) // hashes "complete" — same path, different checksum

	if db.insertCalls != 1 {
		t.Errorf("same path enqueued twice should be one job, got %d inserts", db.insertCalls)
	}
}

func TestWaitForStableSizeStableFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "book.m4b")
	if err := os.WriteFile(p, []byte("done copying"), 0600); err != nil {
		t.Fatal(err)
	}
	fm := newTestMonitor(dir, &fakeDB{})
	if err := fm.waitForStableSize(p); err != nil {
		t.Errorf("a stable file should return nil, got %v", err)
	}
}

func TestWaitForStableSizeMissingFile(t *testing.T) {
	dir := t.TempDir()
	fm := newTestMonitor(dir, &fakeDB{})
	if err := fm.waitForStableSize(filepath.Join(dir, "nope.mp3")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestWaitForStableSizeTimeoutWhileGrowing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "growing.m4b")
	if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	fm := newTestMonitor(dir, &fakeDB{})
	fm.stabilityInterval = 5 * time.Millisecond
	fm.stabilityCount = 3
	fm.stabilityTimeout = 40 * time.Millisecond

	// Grow the file faster than the poll interval so its size never holds steady.
	done := make(chan struct{})
	go func() {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		for {
			select {
			case <-done:
				return
			default:
				_, _ = f.WriteString("growing")
				time.Sleep(time.Millisecond)
			}
		}
	}()
	defer close(done)

	if err := fm.waitForStableSize(p); err == nil {
		t.Error("expected a timeout error for a continuously growing file")
	}
}

// ─── book_metadata tests (CONTRACT §1.6) ────────────────────────────────────

// TestBookMetadataWrittenOnEnqueue verifies that enqueueFile writes a
// book_metadata row with the values from the MetadataProvider.
func TestBookMetadataWrittenOnEnqueue(t *testing.T) {
	dir := t.TempDir()
	bookDir := filepath.Join(dir, "Author", "My Book")
	if err := os.MkdirAll(bookDir, 0700); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(bookDir, "track01.mp3")
	if err := os.WriteFile(filePath, []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}

	db := &fakeDB{}
	meta := &stubMetaProvider{title: "My Book", author: "The Author", source: "path"}
	fm := newTestMonitorWithMeta(dir, db, meta)

	fm.enqueueFile(filePath)

	if db.bookMetaCalls != 1 {
		t.Fatalf("expected 1 book_metadata upsert, got %d", db.bookMetaCalls)
	}
	row, ok := db.bookMetadata[bookDir]
	if !ok {
		t.Fatalf("no book_metadata row found for book_dir=%q", bookDir)
	}
	if row.Title != "My Book" {
		t.Errorf("title: got %q, want %q", row.Title, "My Book")
	}
	if row.Author != "The Author" {
		t.Errorf("author: got %q, want %q", row.Author, "The Author")
	}
	if row.Source != "path" {
		t.Errorf("source: got %q, want %q", row.Source, "path")
	}
}

// TestBookMetadataUpsertUpdatesExisting verifies that enqueuing a second file
// from the same book directory refreshes the book_metadata row (not a dup).
func TestBookMetadataUpsertUpdatesExisting(t *testing.T) {
	dir := t.TempDir()
	bookDir := filepath.Join(dir, "Author", "My Book")
	if err := os.MkdirAll(bookDir, 0700); err != nil {
		t.Fatal(err)
	}

	db := &fakeDB{}
	meta := &stubMetaProvider{title: "My Book", author: "The Author", source: "path"}
	fm := newTestMonitorWithMeta(dir, db, meta)

	for _, track := range []string{"track01.mp3", "track02.mp3"} {
		p := filepath.Join(bookDir, track)
		if err := os.WriteFile(p, []byte(track), 0600); err != nil {
			t.Fatal(err)
		}
		fm.enqueueFile(p)
	}

	// Two tracks → two UpsertBookMetadata calls (idempotent ON CONFLICT).
	if db.bookMetaCalls != 2 {
		t.Errorf("expected 2 book_metadata upserts (one per track), got %d", db.bookMetaCalls)
	}
	// Still exactly one row per book_dir.
	if len(db.bookMetadata) != 1 {
		t.Errorf("expected 1 unique book_dir in metadata map, got %d", len(db.bookMetadata))
	}
}

// TestBookMetadataWriteFailureDoesNotBlockEnqueue verifies the best-effort
// guarantee: a DB error from UpsertBookMetadata must be logged and swallowed,
// never propagated, so enqueue still succeeds.
func TestBookMetadataWriteFailureDoesNotBlockEnqueue(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "track01.mp3")
	if err := os.WriteFile(filePath, []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}

	db := &fakeDB{bookMetaErr: errors.New("simulated DB failure")}
	meta := &stubMetaProvider{title: "Book", author: "Author", source: "path"}
	fm := newTestMonitorWithMeta(dir, db, meta)

	// Must not panic or return an error — enqueue swallows the metadata failure.
	fm.enqueueFile(filePath)

	// The job itself must still be enqueued.
	if db.insertCalls != 1 {
		t.Errorf("expected 1 job inserted despite metadata failure, got %d", db.insertCalls)
	}
	// book_metadata should have been attempted.
	if db.bookMetaCalls != 1 {
		t.Errorf("expected 1 metadata upsert attempt, got %d", db.bookMetaCalls)
	}
	// But no row recorded (the error was returned before storage).
	if len(db.bookMetadata) != 0 {
		t.Errorf("expected no book_metadata stored after failure, got %d rows", len(db.bookMetadata))
	}
}

// TestBookMetadataEmptyFromProvider verifies that an empty (but successful)
// provider result — the valid "couldn't resolve" case for a real provider —
// is still written (empty strings stored, no error) and enqueue succeeds.
func TestBookMetadataEmptyFromProvider(t *testing.T) {
	dir := t.TempDir()
	bookDir := filepath.Join(dir, "Unknown")
	if err := os.MkdirAll(bookDir, 0700); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(bookDir, "track01.mp3")
	if err := os.WriteFile(filePath, []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}

	db := &fakeDB{}
	// Successful lookup returning all-empty metadata (no err) — the case where a
	// real provider cannot resolve a directory but does not error.
	meta := &stubMetaProvider{title: "", author: "", source: ""}
	fm := newTestMonitorWithMeta(dir, db, meta)

	fm.enqueueFile(filePath)

	if db.insertCalls != 1 {
		t.Errorf("expected 1 job inserted, got %d", db.insertCalls)
	}
	if db.bookMetaCalls != 1 {
		t.Fatalf("expected 1 metadata upsert (empty values still written), got %d", db.bookMetaCalls)
	}
	row, ok := db.bookMetadata[bookDir]
	if !ok {
		t.Fatalf("no book_metadata row found for book_dir=%q", bookDir)
	}
	if row.Title != "" || row.Author != "" || row.Source != "" {
		t.Errorf("expected all-empty metadata stored, got title=%q author=%q source=%q", row.Title, row.Author, row.Source)
	}
}

// TestBookMetadataProviderFailureDoesNotBlockEnqueue verifies that a provider
// error (e.g. network timeout in a future ABS provider) is also swallowed.
func TestBookMetadataProviderFailureDoesNotBlockEnqueue(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "track01.mp3")
	if err := os.WriteFile(filePath, []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}

	db := &fakeDB{}
	meta := &stubMetaProvider{err: errors.New("simulated provider failure")}
	fm := newTestMonitorWithMeta(dir, db, meta)

	fm.enqueueFile(filePath)

	// The job must still be enqueued.
	if db.insertCalls != 1 {
		t.Errorf("expected 1 job inserted despite provider failure, got %d", db.insertCalls)
	}
	// No DB call should have been made (provider failed before upsert).
	if db.bookMetaCalls != 0 {
		t.Errorf("expected 0 metadata upserts (provider failed), got %d", db.bookMetaCalls)
	}
}
