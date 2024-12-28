package monitor

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
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
}

func NewFileMonitor(cfg *config.Config, q *queue.Queue, sm *state.StateManager) *FileMonitor {
	return &FileMonitor{
		config:       cfg,
		queue:        q,
		stateManager: sm,
	}
}

// Add new method to scan existing files
func (fm *FileMonitor) scanExistingFiles() error {
	log.Println("Scanning for existing audio files...")
	rootDir := fm.config.AudioDir
	return filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("failed to access path %s: %w", path, err)
		}

		if info.IsDir() {
			return nil
		}

		if filepath.Ext(path) == ".txt" {
			return nil
		}

		relPath, err := filepath.Rel(rootDir, path)
		if err != nil {
			log.Printf("Error getting relative path for %s: %v", path, err)
			return nil
		}

		log.Printf("Found existing file: %s", relPath)
		if !fm.stateManager.IsProcessed(path) {
			fm.queue.Enqueue(path)
		}
		return nil
	})
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
	if filepath.Ext(filePath) == ".txt" {
		log.Printf("Ignoring text file: %s", filePath)
		return
	}

	log.Printf("New file detected: %s", filePath)

	// Add a small delay to allow file creation to complete
	time.Sleep(1 * time.Second)

	if !fm.stateManager.IsProcessed(filePath) {
		fm.queue.Enqueue(filePath)
	} else {
		log.Printf("File already processed: %s", filePath)
	}
}
