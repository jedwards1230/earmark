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
	entries, err := os.ReadDir(fm.config.AudioDir)
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filePath := filepath.Join(fm.config.AudioDir, entry.Name())
		if filepath.Ext(filePath) == ".txt" {
			continue
		}

		log.Printf("Found existing file: %s", filePath)
		if !fm.stateManager.IsProcessed(filePath) {
			fm.queue.Enqueue(filePath)
		}
	}
	return nil
}

func (fm *FileMonitor) Start() {
	// Scan existing files before starting the watcher
	log.Println("Starting file monitor...")

	if err := fm.scanExistingFiles(); err != nil {
		log.Printf("Error scanning existing files: %v", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("Failed to create file watcher: %v", err)
	}
	defer watcher.Close()

	err = watcher.Add(fm.config.AudioDir)
	if err != nil {
		log.Fatalf("Failed to add directory to watcher: %v", err)
	}

	log.Printf("Monitoring directory: %s", fm.config.AudioDir)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				go fm.handleFileCreate(event.Name)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Println("File watcher error:", err)
		}
	}
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
