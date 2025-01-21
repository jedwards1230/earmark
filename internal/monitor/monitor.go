package monitor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"transcriber/internal/config"
	"transcriber/internal/meta"
	"transcriber/internal/queue"
	"transcriber/internal/state"

	"github.com/fsnotify/fsnotify"
)

type FileMonitor struct {
	config       *config.Config
	queue        *queue.Queue
	stateManager *state.StateManager
	queuedFiles  map[string]bool
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
}

func NewFileMonitor(cfg *config.Config, q *queue.Queue, sm *state.StateManager) *FileMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &FileMonitor{
		config:       cfg,
		queue:        q,
		stateManager: sm,
		queuedFiles:  make(map[string]bool),
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
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

// parseFilePath extracts author and title information from a filepath
func parseFilePath(path string) (author, title string) {
	// Remove the base audiobooks directory
	parts := strings.Split(path, string(os.PathSeparator))
	if len(parts) < 3 {
		return "", ""
	}

	// Author is typically the first directory after "audiobooks"
	for i, part := range parts {
		if part == "audiobooks" && i+1 < len(parts) {
			author = parts[i+1]
			break
		}
	}

	// Title is typically the next directory, might include series info
	// Extract up to the ASIN/ISBN if present
	if titleIdx := strings.LastIndex(path, author) + len(author) + 1; titleIdx < len(path) {
		title = path[titleIdx:]
		// Split on directory separator
		titleParts := strings.Split(title, string(os.PathSeparator))
		if len(titleParts) > 0 {
			title = titleParts[0]
		}
		// Remove ASIN/ISBN if present
		if idx := strings.Index(title, "["); idx != -1 {
			title = strings.TrimSpace(title[:idx])
		}
	}

	return author, title
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
				author, title := parseFilePath(filePath)
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
	author, title := parseFilePath(filePath)
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
				log.Printf("Error finding audio files in %s: %v", dir, err)
				return nil
			}

			// Add audio files to metadata
			metadata.FileMetas = []meta.FileMetadata{}
			for i, audioFile := range audioFiles {
				metadata.FileMetas = append(metadata.FileMetas, meta.FileMetadata{
					FilePath: audioFile,
					FileName: filepath.Base(audioFile),
					Author:   metadata.Author,
					Title:    metadata.Title,
					ISBN:     metadata.ISBN,
					Chapter:  fmt.Sprintf("%d", i+1),
				})
			}

			// Enqueue audio files that haven't been processed
			for _, fileMeta := range metadata.FileMetas {
				if !fm.stateManager.IsProcessed(fileMeta.FilePath) {
					log.Printf("Enqueueing audio file for book '%s' by %s", metadata.Title, fileMeta.Author)
					fm.queue.Enqueue(queue.QueueItem{
						FilePath: fileMeta.FilePath,
						Metadata: metadata,
					})
				}
			}
		}
		return nil
	})
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

func (fm *FileMonitor) Start() {
	defer close(fm.done)
	log.Println("Starting file monitor...")

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

	log.Printf("New audio file detected: %s", filePath)

	// Add a small delay to allow file creation to complete
	time.Sleep(1 * time.Second)

	if !fm.stateManager.IsProcessed(filePath) {
		// Extract basic metadata from filepath for new files
		author, title := parseFilePath(filePath)
		metadata := &meta.BookMetadata{
			Author: author,
			Title:  title,
		}

		fm.queue.Enqueue(queue.QueueItem{
			FilePath: filePath,
			Metadata: metadata,
		})
	} else {
		log.Printf("Audio file already processed: %s", filePath)
	}
}

func (fm *FileMonitor) Stop() {
	fm.cancel()
	<-fm.done
}
