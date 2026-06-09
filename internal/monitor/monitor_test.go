package monitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

// fakeDB implements DBInterface for unit tests — no real DB needed.
type fakeDB struct {
	inserted map[string]string // checksum -> jobID
	pruned   int               // times PruneAppleDoubleJobs was called
}

func (f *fakeDB) InsertJobIfAbsent(_ context.Context, _, checksum string) (string, bool, error) {
	if id, ok := f.inserted[checksum]; ok {
		return id, false, nil
	}
	id := "job-" + checksum[:8]
	f.inserted[checksum] = id
	return id, true, nil
}

func (f *fakeDB) PruneAppleDoubleJobs(context.Context) (int, error) {
	f.pruned++
	return 0, nil
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
