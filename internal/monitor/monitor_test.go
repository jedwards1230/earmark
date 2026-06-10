package monitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
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

func newTestMonitor(dir string, db DBInterface) *FileMonitor {
	cfg := &config.Config{BooksDir: dir}
	ctx, cancel := context.WithCancel(context.Background())
	return &FileMonitor{
		cfg:    cfg,
		db:     db,
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

	// Second call — should be idempotent.
	fm.enqueueFile(filePath)
	if len(db.inserted) != 1 {
		t.Errorf("expected still 1 job after duplicate enqueue, got %d", len(db.inserted))
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
