// Package monitor watches the books directory for new audio files and
// enqueues them into the transcription_jobs table (dedup by SHA-256 checksum).
//
// The monitor no longer calls a local transcriber. It is a pure producer, and
// discovers files three ways:
//   - Initial scan: walk BOOKS_DIR, compute checksum for each audio file,
//     insert a pending job if none exists.
//   - Live watch (fsnotify): handle CREATE events for new audio files. This is
//     the low-latency path, but inotify only reports writes made through *this*
//     host's kernel.
//   - Periodic scan (SCAN_INTERVAL, default 1h): re-walk BOOKS_DIR. This is the
//     correctness backstop for a library on NFS — a book written directly on the
//     file server, or by any other NFS client, never raises an inotify event
//     here, so the watch alone would miss it forever.
package monitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/log"
	"github.com/jedwards1230/earmark/internal/metaprovider"
	"github.com/jedwards1230/earmark/internal/transcribe"
)

// DBInterface is the subset of db.DB used by the monitor.
type DBInterface interface {
	transcribe.JobInserter
	// PruneAppleDoubleJobs removes junk AppleDouble (._*) jobs enqueued before
	// the audio-file filter learned to skip them. Returns the count removed.
	PruneAppleDoubleJobs(ctx context.Context) (int, error)
	// IsPathQueued reports whether a job already exists for this file_path, so
	// the scan can skip re-hashing known files.
	IsPathQueued(ctx context.Context, filePath string) (bool, error)
	// UpsertAudioBytes records the audio file size for a job (per-run
	// observability). Best-effort: a failure here must not fail enqueue.
	UpsertAudioBytes(ctx context.Context, jobID string, bytes int64) error
	// UpsertBookMetadata records book-level metadata derived from the
	// MetadataProvider at enqueue time (CONTRACT §1.6). Best-effort: a failure
	// here must not fail enqueue.
	UpsertBookMetadata(ctx context.Context, bookDir string, meta metaprovider.BookMeta) error
	// AppendEvent records one pipeline_events row (CONTRACT §1.7). Best-effort:
	// the monitor logs-and-continues; an event write never fails enqueue.
	AppendEvent(ctx context.Context, e db.PipelineEvent) error
	// PruneEvents removes high-frequency heartbeat/runner_availability events past
	// the retention window. Best-effort; run periodically from the monitor.
	PruneEvents(ctx context.Context) (int64, error)
}

// Default file-stability tuning. A new file is only hashed once its size has
// stopped changing, so a multi-GB audiobook copied over NFS isn't hashed
// mid-copy (which would enqueue a job for a partial file).
const (
	defaultStabilityInterval = 2 * time.Second
	defaultStabilityCount    = 3
	defaultStabilityTimeout  = 10 * time.Minute
)

// FileMonitor watches the books directory and enqueues new audio files.
type FileMonitor struct {
	cfg    *config.Config
	db     DBInterface
	meta   metaprovider.MetadataProvider
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	log    log.Logger

	// File-stability tuning (overridable in tests). A file's size must hold
	// steady for stabilityCount consecutive polls (stabilityInterval apart),
	// up to stabilityTimeout, before it is hashed.
	stabilityInterval time.Duration
	stabilityCount    int
	stabilityTimeout  time.Duration

	// scanInterval is how often BooksDir is re-walked after the startup scan
	// (config.ScanInterval / SCAN_INTERVAL). Overridable in tests. A
	// non-positive value disables periodic scanning entirely.
	scanInterval time.Duration
}

// NewFileMonitor creates a FileMonitor. Call Start to begin watching.
// The MetadataProvider is used at enqueue time to derive book-level metadata
// (title, author) that is written best-effort to book_metadata (CONTRACT §1.6).
func NewFileMonitor(cfg *config.Config, db DBInterface, meta metaprovider.MetadataProvider) *FileMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &FileMonitor{
		cfg:               cfg,
		db:                db,
		meta:              meta,
		ctx:               ctx,
		cancel:            cancel,
		done:              make(chan struct{}),
		log:               log.NewLogger("monitor"),
		stabilityInterval: defaultStabilityInterval,
		stabilityCount:    defaultStabilityCount,
		stabilityTimeout:  defaultStabilityTimeout,
		scanInterval:      cfg.ScanInterval,
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

	fm.runScan("initial")

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

	// Periodic retention prune of high-frequency pipeline_events
	// (heartbeat/runner_availability) so they don't grow unbounded (CONTRACT §1.7).
	// Best-effort + ctx-aware; runs once at startup and every 24h thereafter.
	go fm.runRetentionPrune()

	// Periodic re-walk of BooksDir (SCAN_INTERVAL). fsnotify above is only the
	// low-latency path for writes this kernel sees; it never fires for a file
	// written by a different NFS client, so without this a book added after
	// startup is never discovered.
	go fm.runPeriodicScan()

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

// runRetentionPrune prunes high-frequency pipeline_events past their retention
// window once at startup, then every 24h until the monitor's context is
// cancelled. Best-effort: a prune failure is logged and the loop continues.
func (fm *FileMonitor) runRetentionPrune() {
	const interval = 24 * time.Hour
	prune := func() {
		ctx, cancel := context.WithTimeout(fm.ctx, 60*time.Second)
		defer cancel()
		n, err := fm.db.PruneEvents(ctx)
		if err != nil {
			fm.log.Warn("pipeline_events retention prune failed (continuing)", "error", err)
			return
		}
		if n > 0 {
			fm.log.Info("pruned old pipeline_events", "count", n)
		}
	}

	prune()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-fm.ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

// runPeriodicScan re-walks BooksDir every scanInterval until the monitor's
// context is cancelled. Best-effort: a scan failure is logged and the loop
// continues.
//
// Why this exists: fsnotify/inotify only reports writes that pass through this
// host's kernel. The library lives on NFS and is written by other clients (the
// downloader writes straight to the file server), so those files never raise a
// watch event here — before this loop, anything added after startup stayed
// invisible until the pod restarted. fsnotify remains the low-latency path for
// local writes; this walk is the correctness backstop.
//
// Unlike runRetentionPrune, this does NOT do a pass immediately on start: Start
// has already run the initial scan, and repeating it here would walk the whole
// library twice at boot. It waits for the first tick.
//
// A non-positive interval disables periodic scanning (documented escape hatch,
// and it keeps time.NewTicker from panicking on 0).
func (fm *FileMonitor) runPeriodicScan() {
	if fm.scanInterval <= 0 {
		fm.log.Info("periodic scan disabled (SCAN_INTERVAL <= 0); relying on the startup scan and fsnotify only",
			"scan_interval", fm.scanInterval)
		return
	}

	fm.log.Info("periodic scan enabled", "scan_interval", fm.scanInterval)
	ticker := time.NewTicker(fm.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-fm.ctx.Done():
			return
		case <-ticker.C:
			fm.runScan("periodic")
		}
	}
}

// Stop signals the monitor to shut down and waits for it to finish.
func (fm *FileMonitor) Stop() {
	fm.cancel()
	<-fm.done
}

// scanResult summarizes one walk of BooksDir, so a scan that legitimately found
// nothing is distinguishable in the logs from a scan that never ran.
type scanResult struct {
	// walked is the number of audio files visited (already-queued ones included).
	walked int
	// enqueued is the number of files that produced a NEW transcription_jobs row.
	enqueued int
	// skipped is the number of paths a walk error made unreadable this pass.
	skipped int
}

// runScan performs one walk and logs its outcome. kind labels what triggered it
// ("initial" / "periodic") so the two are distinguishable in logs.
func (fm *FileMonitor) runScan(kind string) scanResult {
	start := time.Now()
	res, err := fm.scan()
	if err != nil {
		fm.log.Error(kind+" scan failed", "error", err,
			"total_audio_files", res.walked, "enqueued", res.enqueued, "skipped", res.skipped,
			"duration", time.Since(start))
		return res
	}
	fm.log.Info(kind+" scan complete",
		"total_audio_files", res.walked, "enqueued", res.enqueued, "skipped", res.skipped,
		"duration", time.Since(start))
	return res
}

// scan walks BooksDir and enqueues any unqueued audio files. Already-known
// paths are skipped without hashing, so a pod restart over a large NFS library
// is a metadata-only walk instead of a multi-TB re-hash. (The library is
// append-only — files are added, not edited in place — so a known path never
// needs re-checking.)
//
// Per-entry errors are logged, counted in scanResult.skipped, and the walk
// continues: this runs on a timer over an NFS mount, and a transient EIO on one
// subdirectory must not abort — and thereby silently disable — every subsequent
// scan. Only a hard failure on BooksDir itself is returned as an error.
//
// Caveat (mid-copy race): unlike handleCreate, the scan path does NOT call
// waitForStableSize — doing so would add stabilityCount polls per file to a walk
// over the whole library. A file still being copied by another NFS client can
// therefore be hashed mid-copy. The consequence is bounded: dedup is by
// file_path as well as checksum (see TestMonitorDuplicatePathDifferentChecksum),
// so this yields one job with a stale checksum rather than a duplicate job, and
// the ASR runner reads the file by path when it eventually claims it.
func (fm *FileMonitor) scan() (scanResult, error) {
	var res scanResult
	err := filepath.Walk(fm.cfg.BooksDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// A failure on the root itself means the library is unreadable —
			// that is a real error, not a skippable entry.
			if path == fm.cfg.BooksDir {
				return err
			}
			res.skipped++
			fm.log.Warn("scan: skipping unreadable path (continuing)", "path", path, "error", err)
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() || !isAudioFile(info.Name()) {
			return nil
		}
		res.walked++
		if fm.pathAlreadyQueued(path) {
			return nil
		}
		if fm.enqueueFile(path) {
			res.enqueued++
		}
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("walk %s: %w", fm.cfg.BooksDir, err)
	}
	return res, nil
}

// pathAlreadyQueued reports whether a job already exists for path. On a DB error
// it returns false (and logs) so the file is still attempted — the file_path
// unique constraint then prevents a duplicate.
func (fm *FileMonitor) pathAlreadyQueued(path string) bool {
	ctx, cancel := context.WithTimeout(fm.ctx, 10*time.Second)
	defer cancel()
	queued, err := fm.db.IsPathQueued(ctx, path)
	if err != nil {
		fm.log.Error("path-queued check failed; will attempt enqueue", "file", path, "error", err)
		return false
	}
	return queued
}

// handleCreate is called on fsnotify CREATE events.
func (fm *FileMonitor) handleCreate(filePath string) {
	if !isAudioFile(filePath) {
		return
	}
	// Wait for the file to finish copying before hashing — a fixed sleep is far
	// too short for a multi-GB .m4b landing over NFS, and hashing a partial file
	// would enqueue a job for incomplete content. On timeout we enqueue anyway;
	// the file_path unique constraint stops a later rescan from duplicating it.
	if err := fm.waitForStableSize(filePath); err != nil {
		fm.log.Warn("file did not stabilize; enqueuing anyway", "file", filePath, "error", err)
	}
	_ = fm.enqueueFile(filePath)
}

// waitForStableSize blocks until path's size is unchanged across stabilityCount
// consecutive polls, or until stabilityTimeout elapses (returns an error).
func (fm *FileMonitor) waitForStableSize(path string) error {
	var lastSize int64 = -1
	streak := 0
	start := time.Now()
	for {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if info.Size() == lastSize {
			if streak++; streak >= fm.stabilityCount {
				return nil
			}
		} else {
			streak = 0
			lastSize = info.Size()
		}
		if time.Since(start) > fm.stabilityTimeout {
			return fmt.Errorf("file %s did not stabilize within %s", path, fm.stabilityTimeout)
		}
		select {
		case <-fm.ctx.Done():
			return fm.ctx.Err()
		case <-time.After(fm.stabilityInterval):
		}
	}
}

// enqueueFile computes the checksum and inserts a job row if absent. It reports
// whether a NEW job row was created, so a scan can tally genuinely-new files
// instead of guessing from the number of files it visited.
func (fm *FileMonitor) enqueueFile(filePath string) (created bool) {
	ctx, cancel := context.WithTimeout(fm.ctx, 30*time.Second)
	defer cancel()

	jobID, created, err := transcribe.EnqueueJob(ctx, filePath, fm.db)
	if err != nil {
		fm.log.Error("failed to enqueue job", "file", filePath, "error", err)
		return false
	}
	if created {
		fm.log.Info("enqueued transcription job", "file", filePath, "job_id", jobID)
		// Audit event for the enqueue boundary (CONTRACT §1.7). Best-effort:
		// log-and-continue — an event write must never fail enqueue.
		if err := fm.db.AppendEvent(ctx, db.PipelineEvent{
			JobID:      jobID,
			FilePath:   filePath,
			Stage:      db.StageEnqueue,
			Event:      db.EventFinish,
			RunnerHost: db.HostGoMonitor,
		}); err != nil {
			fm.log.Warn("pipeline event (enqueue) write failed (continuing)", "file", filePath, "job_id", jobID, "error", err)
		}
	} else {
		fm.log.Debug("job already exists", "file", filePath)
	}

	// Record the audio file size for per-run observability. Best-effort: a stat
	// or metrics-write failure must never fail enqueue, so we only log it. The
	// UPSERT touches only run_metrics.audio_bytes, so re-enqueues are harmless.
	if info, statErr := os.Stat(filePath); statErr != nil {
		fm.log.Debug("audio_bytes: stat failed", "file", filePath, "error", statErr)
	} else if err := fm.db.UpsertAudioBytes(ctx, jobID, info.Size()); err != nil {
		fm.log.Warn("audio_bytes: metrics write failed (continuing)", "file", filePath, "job_id", jobID, "error", err)
	}

	// Derive and persist book-level metadata (CONTRACT §1.6). Best-effort: a
	// provider or DB error must never block enqueue — mirror the UpsertAudioBytes
	// pattern exactly. The sampleName is the filename component, which the
	// PathProvider uses to strip track-number prefixes and derive a title.
	fm.upsertBookMetadata(ctx, filePath)

	return created
}

// upsertBookMetadata derives book metadata from filePath via the configured
// MetadataProvider and writes it to book_metadata. Best-effort: all errors are
// logged and swallowed so enqueue is never blocked.
func (fm *FileMonitor) upsertBookMetadata(ctx context.Context, filePath string) {
	bookDir := filepath.Dir(filePath)
	sampleName := filepath.Base(filePath)

	meta, err := fm.meta.Lookup(ctx, filePath, sampleName)
	if err != nil {
		fm.log.Warn("book_metadata: provider lookup failed (continuing)", "file", filePath, "error", err)
		return
	}

	if err := fm.db.UpsertBookMetadata(ctx, bookDir, meta); err != nil {
		fm.log.Warn("book_metadata: DB write failed (continuing)", "file", filePath, "book_dir", bookDir, "error", err)
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
