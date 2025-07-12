package transcribe

import (
	"context"
	"fmt"

	"github.com/jedwards1230/lil-whisper/internal/meta"
)

// SpeechEngine defines a simple interface for speech-to-text transcription
type SpeechEngine interface {
	// Transcribe converts audio file to text
	Transcribe(ctx context.Context, audioFilePath string, fileMeta *meta.FileMetadata) (string, error)
	
	// GetVersion returns the version/identifier of the speech engine
	GetVersion() string
	
	// GetInfo returns metadata about the speech engine
	GetInfo() (map[string]interface{}, error)
}

// EngineManager manages the speech engine configuration
type EngineManager struct {
	engine SpeechEngine
}

// NewEngineManager creates a new engine manager with the provided speech engine
func NewEngineManager(engine SpeechEngine) *EngineManager {
	return &EngineManager{
		engine: engine,
	}
}

// Transcribe delegates transcription to the configured speech engine
func (em *EngineManager) Transcribe(ctx context.Context, audioFilePath string, fileMeta *meta.FileMetadata) (string, error) {
	if em.engine == nil {
		return "", fmt.Errorf("no speech engine configured")
	}
	
	return em.engine.Transcribe(ctx, audioFilePath, fileMeta)
}

// GetVersion returns the version of the configured speech engine
func (em *EngineManager) GetVersion() string {
	if em.engine == nil {
		return "unknown"
	}
	
	return em.engine.GetVersion()
}

// GetInfo returns information about the configured speech engine
func (em *EngineManager) GetInfo() (map[string]interface{}, error) {
	if em.engine == nil {
		return nil, fmt.Errorf("no speech engine configured")
	}
	
	return em.engine.GetInfo()
}