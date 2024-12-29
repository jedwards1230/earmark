package state

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type ProcessingMetadata struct {
	Processed      bool    `json:"processed"`
	OriginalSize   int64   `json:"original_size"`
	ProcessedSize  int64   `json:"processed_size"`
	ProcessingTime float64 `json:"processing_time"`
	ModelUsed      string  `json:"model_used"`
	ComputeType    string  `json:"compute_type"`
	ProcessedAt    string  `json:"processed_at"`
}

type StateManager struct {
	state    map[string]ProcessingMetadata
	filePath string
	mu       sync.RWMutex
}

func NewStateManager(filePath string) (*StateManager, error) {
	sm := &StateManager{
		state:    make(map[string]ProcessingMetadata),
		filePath: filePath,
	}

	// Check for RESET_STATE environment variable
	if os.Getenv("RESET_STATE") == "true" || os.Getenv("RESET_STATE") == "1" {
		log.Println("RESET_STATE environment variable detected, creating fresh state")
		return sm, sm.saveState()
	}

	if err := sm.loadState(); err != nil {
		return nil, err
	}

	return sm, nil
}

func (sm *StateManager) loadState() error {
	// Create parent directory and empty state file if they don't exist
	if err := os.MkdirAll(filepath.Dir(sm.filePath), 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	file, err := os.Open(sm.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("No existing state file found, creating new one")
			// Don't call saveState while holding the lock
			return sm.saveState()
		}
		return err
	}
	defer file.Close()

	sm.mu.Lock()
	defer sm.mu.Unlock()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&sm.state); err != nil {
		return fmt.Errorf("failed to decode state file: %w", err)
	}

	bookCount := len(sm.state)
	log.Printf("Loaded existing state file with %d processed books", bookCount)
	return nil
}

func (sm *StateManager) saveState() error {
	if err := os.MkdirAll(filepath.Dir(sm.filePath), 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	sm.mu.RLock() // Use RLock since we're only reading
	data, err := json.MarshalIndent(sm.state, "", "  ")
	sm.mu.RUnlock()

	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Write atomically using a temporary file
	tmpFile := sm.filePath + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp state file: %w", err)
	}

	if err := os.Rename(tmpFile, sm.filePath); err != nil {
		os.Remove(tmpFile) // Clean up the temp file if rename fails
		return fmt.Errorf("failed to rename state file: %w", err)
	}

	return nil
}

func (sm *StateManager) IsProcessed(filePath string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	metadata, exists := sm.state[filePath]
	return exists && metadata.Processed
}

func (sm *StateManager) MarkProcessed(filePath string, originalSize, processedSize int64, processingTime float64, modelUsed string, compute_type string) error {
	log.Printf("Marking %s as processed", filePath)

	metadata := ProcessingMetadata{
		Processed:      true,
		OriginalSize:   originalSize,
		ProcessedSize:  processedSize,
		ProcessingTime: processingTime,
		ModelUsed:      modelUsed,
		ComputeType:    compute_type,
		ProcessedAt:    time.Now().UTC().Format(time.RFC3339),
	}

	sm.mu.Lock()
	sm.state[filePath] = metadata
	sm.mu.Unlock()

	// Save state after updating the map
	if err := sm.saveState(); err != nil {
		// If save fails, remove the item from the map
		sm.mu.Lock()
		delete(sm.state, filePath)
		sm.mu.Unlock()
		log.Printf("Error saving state: %v", err)
		return err
	}

	log.Printf("Successfully marked and saved state for: %s", filePath)
	return nil
}
