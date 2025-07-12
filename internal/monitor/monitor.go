package monitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/meta"
	"github.com/jedwards1230/lil-whisper/internal/queue"
	"github.com/jedwards1230/lil-whisper/internal/utils"

	"github.com/fsnotify/fsnotify"
)

type FileMonitor struct {
	config            *config.Config
	queue             *queue.Queue
	db                *db.DB
	queuedFiles       map[string]bool
	ctx               context.Context
	cancel            context.CancelFunc
	done              chan struct{}
	log               log.Logger
	reprocessingCount int
}

// Statistics struct to track book and file counts
type Statistics struct {
	TotalBooks      int
	TotalAudioFiles int
	*db.Statistics
}

func NewFileMonitor(cfg *config.Config, q *queue.Queue, database *db.DB) *FileMonitor {
	ctx, cancel := context.WithCancel(context.Background())

	logger := log.NewLogger("monitor")

	return &FileMonitor{
		config:      cfg,
		queue:       q,
		db:          database,
		queuedFiles: make(map[string]bool),
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
		log:         logger,
	}
}

// tryParsers attempts to parse metadata using available parsers
func (fm *FileMonitor) tryParsers(data []byte, filePath string) (*meta.BookMetadata, error) {
	parsers := meta.GetMetadataParsers()

	var lastErr error

	for _, parser := range parsers {
		metadata, err := parser.Parse(data)
		if err == nil {
			// If we got metadata but no author/title, try to get from filepath
			if metadata.Author == "" || metadata.Title == "" {
				author, title, _, _ := utils.ParseFilePath(filePath)
				if metadata.Author == "" {
					metadata.Author = author
					if author != "" {
						fm.log.Info("found author from filepath", "author", author)
					}
				}
				if metadata.Title == "" {
					metadata.Title = title
					if title != "" {
						fm.log.Info("found title from filepath", "title", title)
					}
				}
				identifier := "none"
				if metadata.ASIN != "" {
					identifier = "ASIN: " + metadata.ASIN
				} else if metadata.ISBN != "" {
					identifier = "ISBN: " + metadata.ISBN
				}
				fm.log.Info("book metadata",
					"author", metadata.Author,
					"title", metadata.Title,
					"identifier", identifier)
			}
			return metadata, nil
		}
		lastErr = err
	}

	// If no parser succeeded, create metadata from filepath
	author, title, _, _ := utils.ParseFilePath(filePath)
	if author != "" || title != "" {
		fm.log.Info("using filepath metadata", "author", author, "title", title)
		return &meta.BookMetadata{
			Author: author,
			Title:  title,
		}, nil
	}

	return nil, fmt.Errorf("no parser succeeded and couldn't extract from filepath: %v", lastErr)
}

// parseBookMetadataFile now uses the parser factory
func (fm *FileMonitor) parseBookMetadataFile(path string) (*meta.BookMetadata, error) {
	// #nosec G304 - path is controlled by filesystem monitor and validated elsewhere
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("reading metadata file: %w", err)
	}

	return fm.tryParsers(data, path)
}

// scanBooks walks the directory for metadata.json files, parses them, and enqueues associated audio files.
func (fm *FileMonitor) scanBooks() error {
	return filepath.Walk(fm.config.AudioDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Only look for metadata.json files
		if !info.IsDir() && strings.HasSuffix(info.Name(), "metadata.json") {
			fm.log.Debug("found metadata file", "path", path)

			metadata, err := fm.parseBookMetadataFile(path)
			if err != nil || metadata == nil {
				fm.log.Error("error parsing metadata file", "path", path, "error", err)
				return nil // Continue walking
			}

			// Get the directory containing the metadata file
			dir := filepath.Dir(path)

			// Find all audio files in the same directory
			audioFiles, err := findAudioFilesInDir(dir)
			if err != nil {
				fm.log.Error("error finding audio files", "title", metadata.Title, "error", err)
				return nil
			}

			// Extract chapter info from file paths and add files to metadata
			metadata.FileMetas = make([]meta.FileMetadata, 0, len(audioFiles))
			newFiles := 0
			for _, audioFile := range audioFiles {
				_, _, chapterIndex, chapterName := utils.ParseFilePath(audioFile)
				if chapterIndex == 0 || chapterName == "" {
					chapterIndex, chapterName = fm.findChapterInfo(metadata.ChaptersInfo, audioFile, len(metadata.FileMetas))
				}

				// Check if the file has already been processed using the chapter info
				processed, err := fm.db.IsProcessed(fm.ctx, audioFile)
				if err != nil {
					fm.log.Error("error checking processing status", "error", err)
					continue
				}

				if processed {
					continue
				}

				fileMeta := meta.FileMetadata{
					FilePath:     audioFile,
					FileName:     filepath.Base(audioFile),
					Author:       metadata.Author,
					Title:        metadata.Title,
					ISBN:         metadata.ISBN,
					ChapterIndex: chapterIndex,
					Chapter:      chapterName,
				}
				metadata.FileMetas = append(metadata.FileMetas, fileMeta)

				fm.queue.Enqueue(queue.QueueItem{
					FilePath: audioFile,
					Metadata: metadata,
				})
				newFiles++
			}

			if newFiles > 0 {
				fm.log.Info("enqueued new files",
					"count", newFiles,
					"title", metadata.Title,
					"author", metadata.Author,
					"total_files", len(audioFiles))
			} else {
				fm.log.Info("no new files to process",
					"title", metadata.Title,
					"author", metadata.Author,
					"total_files", len(audioFiles))
			}
		}
		return nil
	})
}

// findChapterInfo attempts to match an audio file to its chapter information
func (fm *FileMonitor) findChapterInfo(chaptersInfo []meta.ChapterInfo, audioFile string, fileIndex int) (int, string) {
	if chaptersInfo == nil {
		return fileIndex + 1, fmt.Sprintf("%d", fileIndex+1) // fallback to old behavior
	}

	var matches []struct {
		index int
		name  string
		dist  int // distance between chapter index and file index
	}

	// Look for matches in chapter titles
	// Extract the base filename without extension and path for matching
	baseFileName := filepath.Base(audioFile)
	baseFileName = strings.TrimSuffix(baseFileName, filepath.Ext(baseFileName))

	for i, chapter := range chaptersInfo {
		// Skip "Opening Credits", "End Credits" etc
		// if strings.Contains(strings.ToLower(chapter.Title), "credits") {
		// 	continue
		// }

		// Check if the chapter title contains parts of the filename or vice versa
		if strings.Contains(audioFile, chapter.Title) ||
			strings.Contains(chapter.Title, baseFileName) ||
			strings.Contains(strings.ToLower(chapter.Title), strings.ToLower(baseFileName)) {
			dist := abs(i - fileIndex)
			matches = append(matches, struct {
				index int
				name  string
				dist  int
			}{i + 1, chapter.Title, dist})
		}
	}

	if len(matches) == 0 {
		return fileIndex + 1, fmt.Sprintf("%d", fileIndex+1) // fallback to old behavior
	}

	if len(matches) > 1 {
		fm.log.Info(fmt.Sprintf("Multiple chapter matches found for %s:", audioFile))
		for _, m := range matches {
			fm.log.Debug("Multiple chapter matches found",
				"chapter_index", m.index,
				"chapter_name", m.name,
				"distance", m.dist)
		}
	}

	// Find the match with smallest distance
	bestMatch := matches[0]
	for _, m := range matches[1:] {
		if m.dist < bestMatch.dist {
			bestMatch = m
		}
	}

	return bestMatch.index, bestMatch.name
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// checkOrphanedAudioFiles finds any audio files that don't have associated metadata files
func (fm *FileMonitor) checkOrphanedAudioFiles() error {
	return filepath.Walk(fm.config.AudioDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && isAudioFile(info.Name()) {
			dir := filepath.Dir(path)
			hasMetadata := false
			// Check for metadata.json in the same directory
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}

			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), "metadata.json") {
					hasMetadata = true
					break
				}
			}

			if !hasMetadata {
				fm.log.Warn("found orphaned audio file with no metadata", "path", path)
			}
		}
		return nil
	})
}

func findAudioFilesInDir(dir string) ([]string, error) {
	var audioFiles []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() && isAudioFile(entry.Name()) {
			audioFiles = append(audioFiles, filepath.Join(dir, entry.Name()))
		}
	}
	return audioFiles, nil
}

// getStatistics collects statistics about books and processed files
func (fm *FileMonitor) getStatistics(ctx context.Context) (*Statistics, error) {
	stats := &Statistics{}

	// Walk through the directory to count books and files
	err := filepath.Walk(fm.config.AudioDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			if strings.HasSuffix(info.Name(), "metadata.json") {
				stats.TotalBooks++
			} else if isAudioFile(info.Name()) {
				stats.TotalAudioFiles++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Get processing statistics from database
	dbStats, err := fm.db.GetProcessingStats(ctx)
	if err != nil {
		return nil, err
	}
	stats.Statistics = dbStats

	// Override the reprocessing count with our stored value
	stats.ReprocessingBooks = fm.reprocessingCount

	return stats, nil
}

func (fm *FileMonitor) Start(ready chan<- struct{}) {
	defer close(fm.done)
	fm.log.Info("starting file monitor")

	// Create a context with timeout for initial setup
	ctx, cancel := context.WithTimeout(fm.ctx, 30*time.Second)
	defer cancel()

	// Check for mismatched chunks before anything else starts
	if err := fm.verifyChunkSizes(ctx); err != nil {
		fm.log.Error("fatal error during chunk size verification", "error", err)
		close(ready)
		return
	}

	// Collect and log initial statistics
	stats, err := fm.getStatistics(ctx)
	if err != nil {
		fm.log.Error("error collecting statistics", "error", err)
	} else {
		fm.log.Info("library statistics",
			"total_books", stats.TotalBooks,
			"total_audio_files", stats.TotalAudioFiles,
			"processed_books", stats.ProcessedBooks,
			"processed_chapters", stats.ProcessedChapters,
			"reprocessing_books", stats.ReprocessingBooks)
	}

	// Check for orphaned audio files first
	if err := fm.checkOrphanedAudioFiles(); err != nil {
		fm.log.Error("error checking for orphaned audio files", "error", err)
	}

	// Then proceed with normal book scanning
	if err := fm.scanBooks(); err != nil {
		fm.log.Error("error scanning books", "error", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fm.log.Error("failed to create file watcher", "error", err)
	}
	defer watcher.Close()

	// Watch the root directory and recursively add all subdirectories
	if err := fm.addDirAndSubDirs(watcher, fm.config.AudioDir); err != nil {
		fm.log.Error("failed to set up directory watchers", "error", err)
	}

	fm.log.Info("monitoring root directory", "path", fm.config.AudioDir)

	// Signal that initialization is complete
	close(ready)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				// Check if the created item is a directory
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					if err := fm.addDirAndSubDirs(watcher, event.Name); err != nil {
						fm.log.Error("error adding new directory to watcher", "error", err)
					}
				} else {
					go fm.handleFileCreate(event.Name)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			fm.log.Error("file watcher error", "error", err)
		case <-fm.ctx.Done():
			return
		}
	}
}

// Helper function to add a directory and all its subdirectories to the watcher
func (fm *FileMonitor) addDirAndSubDirs(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := watcher.Add(path); err != nil {
				return fmt.Errorf("failed to watch directory %s: %w", path, err)
			}
		}
		return nil
	})
}

func (fm *FileMonitor) handleFileCreate(filePath string) {
	if !isAudioFile(filePath) {
		return
	}

	author, title, _, _ := utils.ParseFilePath(filePath)
	if author != "" && title != "" {
		fm.log.Info("new audio file detected", "title", title, "author", author)
	} else {
		fm.log.Info("new audio file detected", "path", filePath)
	}

	// Add a small delay to allow file creation to complete
	time.Sleep(1 * time.Second)

	if processed, _ := fm.db.IsProcessed(fm.ctx, filePath); processed {
		fm.log.Info("audio file already processed", "path", filePath)
		return
	}

	// Extract basic metadata from filepath for new files
	metadata := &meta.BookMetadata{
		Author: author,
		Title:  title,
	}

	fm.queue.Enqueue(queue.QueueItem{
		FilePath: filePath,
		Metadata: metadata,
	})
}

func (fm *FileMonitor) verifyChunkSizes(ctx context.Context) error {
	// Create a new context with timeout specifically for this operation
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	booksToReprocess, err := fm.db.CheckForMismatchedChunks(queryCtx, fm.config.ChunkSize)
	if err != nil {
		return fmt.Errorf("failed to check for mismatched chunks: %v", err)
	}

	if len(booksToReprocess) > 0 {
		fm.reprocessingCount = len(booksToReprocess) // Store the count
		fm.log.Info("found books with mismatched chunk sizes", "count", len(booksToReprocess))

		// Process each book sequentially
		for _, book := range booksToReprocess {
			fm.log.Info("reprocessing book", "title", book.Title, "author", book.Author)

			// Create new context for each delete operation
			deleteCtx, deleteCancel := context.WithTimeout(ctx, 5*time.Second)
			if err := fm.db.DeleteBookChunks(deleteCtx, book.ID); err != nil {
				deleteCancel()
				return fmt.Errorf("failed to delete existing chunks for '%s': %v", book.Title, err)
			}
			deleteCancel()

			// Re-queue all chapters for processing
			for _, chapter := range book.FileMetas {
				fm.queue.Enqueue(queue.QueueItem{
					FilePath: chapter.FilePath,
					Metadata: &book,
				})
			}
			fm.log.Info("successfully queued chapters for reprocessing", "count", len(book.FileMetas))
		}
	} else {
		fm.log.Info("no books found with mismatched chunk sizes")
	}

	return nil
}

func (fm *FileMonitor) Stop() {
	fm.cancel()
	<-fm.done
}

// Update supported extensions to include source formats we want to convert
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
	ext := strings.ToLower(filepath.Ext(filename))
	return supportedAudioExtensions[ext]
}
