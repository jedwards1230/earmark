package monitor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"transcriber/internal/config"
	"transcriber/internal/db"
	"transcriber/internal/meta"
	"transcriber/internal/queue"

	"github.com/fsnotify/fsnotify"
)

type FileMonitor struct {
	config      *config.Config
	queue       *queue.Queue
	db          *db.DB
	queuedFiles map[string]bool
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
}

// Statistics struct to track book and file counts
type Statistics struct {
	TotalBooks      int
	TotalAudioFiles int
	*db.Statistics  // Embed DB statistics
}

func NewFileMonitor(cfg *config.Config, q *queue.Queue, database *db.DB) *FileMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &FileMonitor{
		config:      cfg,
		queue:       q,
		db:          database,
		queuedFiles: make(map[string]bool),
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
	}
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

// parseFilePath extracts author, title, and chapter information from a filepath
func parseFilePath(path string) (author, title string, chapterIndex int, chapterName string) {
	// Remove the base audiobooks directory
	parts := strings.Split(path, string(os.PathSeparator))
	if len(parts) < 3 {
		return "", "", 0, ""
	}

	// Author is typically the first directory after "audiobooks"
	for i, part := range parts {
		if part == "audiobooks" && i+1 < len(parts) {
			author = parts[i+1]
			break
		}
	}

	// Get the filename without extension
	filename := filepath.Base(path)
	ext := filepath.Ext(filename)
	filename = strings.TrimSuffix(filename, ext)

	// Check if the filename contains chapter information
	if parts := strings.Split(filename, " - "); len(parts) >= 3 {
		// Last two parts are chapter index and name
		chapterName = parts[len(parts)-1]
		chapterIndexStr := parts[len(parts)-2]
		if idx, err := strconv.Atoi(chapterIndexStr); err == nil {
			chapterIndex = idx
		}

		// Title is everything before the chapter parts, up to any ASIN/ISBN
		titlePart := strings.Join(parts[:len(parts)-2], " - ")
		if idx := strings.Index(titlePart, "["); idx != -1 {
			title = strings.TrimSpace(titlePart[:idx])
		} else {
			title = titlePart
		}
	} else {
		// Extract title up to the ASIN/ISBN if no chapter information is found
		if titleIdx := strings.LastIndex(path, author) + len(author) + 1; titleIdx < len(path) {
			title = path[titleIdx:]
			titleParts := strings.Split(title, string(os.PathSeparator))
			if len(titleParts) > 0 {
				title = titleParts[0]
			}
			if idx := strings.Index(title, "["); idx != -1 {
				title = strings.TrimSpace(title[:idx])
			}
		}
	}

	return author, title, chapterIndex, chapterName
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
				author, title, _, _ := parseFilePath(filePath)
				if metadata.Author == "" {
					metadata.Author = author
					if author != "" {
						log.Printf("Found author from filepath: %s", author)
					}
				}
				if metadata.Title == "" {
					metadata.Title = title
					if title != "" {
						log.Printf("Found title from filepath: %s", title)
					}
				}
				identifier := "none"
				if metadata.ASIN != "" {
					identifier = "ASIN: " + metadata.ASIN
				} else if metadata.ISBN != "" {
					identifier = "ISBN: " + metadata.ISBN
				}
				log.Printf("Book metadata: Author: '%s', Title: '%s', %s",
					metadata.Author, metadata.Title, identifier)
			}
			return metadata, nil
		}
		lastErr = err
	}

	// If no parser succeeded, create metadata from filepath
	author, title, _, _ := parseFilePath(filePath)
	if author != "" || title != "" {
		log.Printf("Using filepath metadata - Author: '%s', Title: '%s' (no ASIN/ISBN)",
			author, title)
		return &meta.BookMetadata{
			Author: author,
			Title:  title,
		}, nil
	}

	return nil, fmt.Errorf("no parser succeeded and couldn't extract from filepath: %v", lastErr)
}

// parseBookMetadataFile now uses the parser factory
func (fm *FileMonitor) parseBookMetadataFile(path string) (*meta.BookMetadata, error) {
	data, err := os.ReadFile(path)
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
			log.Printf("Found metadata file: %s", path)

			metadata, err := fm.parseBookMetadataFile(path)
			if err != nil {
				log.Printf("Error parsing metadata file %s: %v", path, err)
				return nil // Continue walking
			}

			// Get the directory containing the metadata file
			dir := filepath.Dir(path)

			// Find all audio files in the same directory
			audioFiles, err := findAudioFilesInDir(dir)
			if err != nil {
				log.Printf("Error finding audio files for '%s': %v", metadata.Title, err)
				return nil
			}

			// Add audio files to metadata
			metadata.FileMetas = []meta.FileMetadata{}
			bookSize := 0
			for _, audioFile := range audioFiles {
				bookSize += int(info.Size())

				// Get chapter info from file path
				_, _, chapterIndex, chapterName := parseFilePath(audioFile)

				// If we didn't get chapter info from the path, try the chapters info
				if chapterIndex == 0 || chapterName == "" {
					// Find matching chapter info from metadata
					chapterIndex, chapterName = findChapterInfo(metadata.ChaptersInfo, audioFile, len(metadata.FileMetas))
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
			}

			// Enqueue audio files that haven't been processed
			queuedFiles := 0
			for _, fileMeta := range metadata.FileMetas {
				if processed, _ := fm.db.IsProcessed(fm.ctx, fileMeta.FilePath); !processed {
					fm.queue.Enqueue(queue.QueueItem{
						FilePath: fileMeta.FilePath,
						Metadata: metadata,
					})
					queuedFiles++
				}
			}

			fmt.Printf("Enqueued %d files for '%s' by %s (%d)\n", queuedFiles, metadata.Title, metadata.Author, bookSize)
		}
		return nil
	})
}

// findChapterInfo attempts to match an audio file to its chapter information
func findChapterInfo(chaptersInfo []meta.ChapterInfo, audioFile string, fileIndex int) (int, string) {
	if chaptersInfo == nil {
		return fileIndex + 1, fmt.Sprintf("%d", fileIndex+1) // fallback to old behavior
	}

	var matches []struct {
		index int
		name  string
		dist  int // distance between chapter index and file index
	}

	// Look for matches in chapter titles
	for i, chapter := range chaptersInfo {
		// Skip "Opening Credits", "End Credits" etc
		// if strings.Contains(strings.ToLower(chapter.Title), "credits") {
		// 	continue
		// }

		if strings.Contains(audioFile, chapter.Title) {
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
		log.Printf("Multiple chapter matches found for %s:", audioFile)
		for _, m := range matches {
			log.Printf("- Chapter %d: %s (distance: %d)", m.index, m.name, m.dist)
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
				log.Printf("Warning: Found orphaned audio file with no metadata: %s", path)
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

	return stats, nil
}

func (fm *FileMonitor) Start() {
	defer close(fm.done)
	log.Println("Starting file monitor...")

	// Collect and log initial statistics
	stats, err := fm.getStatistics(fm.ctx)
	if err != nil {
		log.Printf("Error collecting statistics: %v", err)
	} else {
		log.Printf("Library Statistics:")
		log.Printf("- Total Books Found: %d", stats.TotalBooks)
		log.Printf("- Total Audio Files: %d", stats.TotalAudioFiles)
		log.Printf("- Processed Books: %d", stats.ProcessedBooks)
		log.Printf("- Processed Chapters: %d", stats.ProcessedChapters)
	}

	// Check for orphaned audio files first
	if err := fm.checkOrphanedAudioFiles(); err != nil {
		log.Printf("Error checking for orphaned audio files: %v", err)
	}

	// Then proceed with normal book scanning
	if err := fm.scanBooks(); err != nil {
		log.Printf("Error scanning books: %v", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("Failed to create file watcher: %v", err)
	}
	defer watcher.Close()

	// Watch the root directory and recursively add all subdirectories
	if err := fm.addDirAndSubDirs(watcher, fm.config.AudioDir); err != nil {
		log.Fatalf("Failed to set up directory watchers: %v", err)
	}

	log.Printf("Monitoring root directory: %s", fm.config.AudioDir)

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
						log.Printf("Error adding new directory to watcher: %v", err)
					}
				} else {
					go fm.handleFileCreate(event.Name)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Println("File watcher error:", err)
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

	author, title, _, _ := parseFilePath(filePath)
	if author != "" && title != "" {
		log.Printf("New audio file detected: '%s' by %s", title, author)
	} else {
		log.Printf("New audio file detected: %s", filePath)
	}

	// Add a small delay to allow file creation to complete
	time.Sleep(1 * time.Second)

	if processed, _ := fm.db.IsProcessed(fm.ctx, filePath); processed {
		log.Printf("Audio file already processed: %s", filePath)
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

func (fm *FileMonitor) Stop() {
	fm.cancel()
	<-fm.done
}
