package monitor

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"transcriber/internal/config"
	"transcriber/internal/queue"
	"transcriber/internal/state"

	"github.com/fsnotify/fsnotify"
)

type FileMonitor struct {
	config       *config.Config
	queue        *queue.Queue
	stateManager *state.StateManager
	foundFiles   []string
}

func NewFileMonitor(cfg *config.Config, q *queue.Queue, sm *state.StateManager) *FileMonitor {
	return &FileMonitor{
		config:       cfg,
		queue:        q,
		stateManager: sm,
	}
}

// Add constant for supported audio extensions
var supportedAudioExtensions = map[string]bool{
	".mp3":  true,
	".wav":  true,
	".m4a":  true,
	".m4b":  true,
	".ogg":  true,
	".flac": true,
	".aac":  true,
	".wma":  true,
}

func isAudioFile(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return supportedAudioExtensions[ext]
}

// Add new method to scan existing files
func (fm *FileMonitor) scanExistingFiles() error {
	log.Println("Scanning for existing audio files...")
	rootDir := fm.config.AudioDir
	fm.foundFiles = make([]string, 0)

	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("failed to access path %s: %w", path, err)
		}

		if info.IsDir() {
			return nil
		}

		if !isAudioFile(path) {
			return nil
		}

		relPath, err := filepath.Rel(rootDir, path)
		if err != nil {
			log.Printf("Error getting relative path for %s: %v", path, err)
			return nil
		}

		fm.foundFiles = append(fm.foundFiles, path)
		log.Printf("Found existing audio file: %s", relPath)
		return nil
	})

	if err != nil {
		return err
	}

	// Print summary of found files
	log.Printf("\nFound %d audio files:", len(fm.foundFiles))
	for _, file := range fm.foundFiles {
		relPath, _ := filepath.Rel(rootDir, file)
		log.Printf("- %s", relPath)
	}
	log.Println("\nBeginning transcription process...")

	// Now enqueue files for processing
	for _, file := range fm.foundFiles {
		if !fm.stateManager.IsProcessed(file) {
			fm.queue.Enqueue(file)
		}
	}

	return nil
}

func (fm *FileMonitor) Start() {
	log.Println("Starting file monitor...")

	if err := fm.scanExistingFiles(); err != nil {
		log.Printf("Error scanning existing files: %v", err)
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
		fm.queue.Enqueue(filePath)
	} else {
		log.Printf("Audio file already processed: %s", filePath)
	}
}
