// Package monitor watches the books directory for new audio files and
// enqueues them into the transcription_jobs table (dedup by SHA-256 checksum).
//
// The monitor no longer calls a local transcriber. It is a pure producer:
//   - Initial scan: walk BOOKS_DIR, compute checksum for each audio file,
//     insert a pending job if none exists.
//   - Live watch (fsnotify): handle CREATE events for new audio files.
package monitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/transcribe"
)

// DBInterface is the subset of db.DB used by the monitor.
type DBInterface interface {
	transcribe.JobInserter
	// PruneAppleDoubleJobs removes junk AppleDouble (._*) jobs enqueued before
	// the audio-file filter learned to skip them. Returns the count removed.
	PruneAppleDoubleJobs(ctx context.Context) (int, error)
}

// FileMonitor watches the books directory and enqueues new audio files.
type FileMonitor struct {
	cfg    *config.Config
	db     DBInterface
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	log    log.Logger
}

// NewFileMonitor creates a FileMonitor. Call Start to begin watching.
func NewFileMonitor(cfg *config.Config, db DBInterface) *FileMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &FileMonitor{
		cfg:    cfg,
		db:     db,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		log:    log.NewLogger("monitor"),
	}
}

// Start performs the initial scan and then watches for new files.
// It closes ready once the initial scan is complete.
func (fm *FileMonitor) Start(ready chan<- struct{}) {
	defer close(fm.done)
	fm.log.Info("starting file monitor", "books_dir", fm.cfg.BooksDir)

	// Self-heal: remove any AppleDouble (._*) junk jobs enqueued before the
	// audio-file filter skipped them. Idempotent — a no-op once the queue is clean.
	if n, err := fm.db.PruneAppleDoubleJobs(fm.ctx); err != nil {
		fm.log.Error("prune AppleDouble jobs failed", "error", err)
	} else if n > 0 {
		fm.log.Info("pruned AppleDouble (._*) junk jobs", "count", n)
	}

	if err := fm.scan(); err != nil {
		fm.log.Error("initial scan failed", "error", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fm.log.Error("failed to create watcher", "error", err)
		close(ready)
		return
	}
	defer func() { _ = watcher.Close() }()

	if err := fm.addDirAndSubDirs(watcher, fm.cfg.BooksDir); err != nil {
		fm.log.Error("failed to watch directories", "error", err)
	}

	fm.log.Info("monitor ready", "path", fm.cfg.BooksDir)
	close(ready)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					if err := fm.addDirAndSubDirs(watcher, event.Name); err != nil {
						fm.log.Error("watch new dir", "error", err)
					}
				} else if err == nil {
					go fm.handleCreate(event.Name)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			fm.log.Error("watcher error", "error", err)
		case <-fm.ctx.Done():
			return
		}
	}
}

// Stop signals the monitor to shut down and waits for it to finish.
func (fm *FileMonitor) Stop() {
	fm.cancel()
	<-fm.done
}

// scan walks BooksDir and enqueues any unqueued audio files.
func (fm *FileMonitor) scan() error {
	return filepath.Walk(fm.cfg.BooksDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !isAudioFile(info.Name()) {
			return nil
		}
		fm.enqueueFile(path)
		return nil
	})
}

// handleCreate is called on fsnotify CREATE events.
func (fm *FileMonitor) handleCreate(filePath string) {
	if !isAudioFile(filePath) {
		return
	}
	// Small delay to let the kernel finish writing the file.
	time.Sleep(500 * time.Millisecond)
	fm.enqueueFile(filePath)
}

// enqueueFile computes the checksum and inserts a job row if absent.
func (fm *FileMonitor) enqueueFile(filePath string) {
	ctx, cancel := context.WithTimeout(fm.ctx, 30*time.Second)
	defer cancel()

	jobID, created, err := transcribe.EnqueueJob(ctx, filePath, fm.db)
	if err != nil {
		fm.log.Error("failed to enqueue job", "file", filePath, "error", err)
		return
	}
	if created {
		fm.log.Info("enqueued transcription job", "file", filePath, "job_id", jobID)
	} else {
		fm.log.Debug("job already exists", "file", filePath)
	}
}

// addDirAndSubDirs adds a directory and all its subdirectories to the watcher.
func (fm *FileMonitor) addDirAndSubDirs(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := watcher.Add(path); err != nil {
				return fmt.Errorf("watch %s: %w", path, err)
			}
		}
		return nil
	})
}

// supportedAudioExtensions is the set of file extensions the monitor tracks.
var supportedAudioExtensions = map[string]bool{
	".mp3":  true,
	".m4a":  true,
	".m4b":  true,
	".ogg":  true,
	".flac": true,
	".aac":  true,
	".wma":  true,
	".wav":  true,
}

func isAudioFile(filename string) bool {
	base := filepath.Base(filename)
	// macOS AppleDouble sidecar files (._name.ext) are created on non-HFS
	// filesystems (NFS/SMB) and keep the real file's extension, so they would
	// otherwise pass the extension check. They are metadata, not audio — and
	// because "._" sorts before letters/digits, one would also become a book's
	// MIN(file_path) sample and corrupt the derived title.
	if strings.HasPrefix(base, "._") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(filename))
	return supportedAudioExtensions[ext]
}
