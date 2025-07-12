package correction

import (
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/meta"
	"github.com/jedwards1230/lil-whisper/internal/openai"
)

// Create a pipeline for structural testing
func createTestPipeline() *Pipeline {
	// Create a minimal config
	cfg := &config.Config{
		LLMCorrectionModel:       "gpt-4o-mini",
		LLMCorrectionAPIKey:      "test-key",
		LLMCorrectionBaseURL:     "https://api.openai.com/v1",
		LLMCorrectionTemperature: 0.1,
		LLMCorrectionMaxRetries:  3,
		LLMCorrectionMaxTokens:   4000,
	}

	// Create a real OpenAI client for structural testing
	client := openai.NewClient(cfg)
	logger := log.NewLogger("test-pipeline")

	return NewPipeline(client, logger)
}

func TestPipelineStructuralIntegrity(t *testing.T) {
	// Test that the pipeline can be created and has expected components
	pipeline := createTestPipeline()

	if pipeline == nil {
		t.Fatal("Pipeline should not be nil")
	}

	if pipeline.client == nil {
		t.Error("Pipeline should have an OpenAI client")
	}

	if pipeline.templates == nil {
		t.Error("Pipeline should have templates")
	}

	if pipeline.chunker == nil {
		t.Error("Pipeline should have a text chunker")
	}

	// Test that templates can be retrieved for each stage
	correctionCtx := NewCorrectionContext(&meta.FileMetadata{}, "test")

	for _, stage := range []string{"spelling_grammar", "formatting", "verification"} {
		_, err := pipeline.templates.GetPrompt(stage, correctionCtx, "test input")
		if err != nil {
			t.Errorf("Failed to get prompt for stage %s: %v", stage, err)
		}
	}
}

func TestPipelineResultStructure(t *testing.T) {
	// Test PipelineResult structure
	result := &PipelineResult{
		StageResults: make([]StageResult, 0, 3),
	}

	if result == nil {
		t.Error("PipelineResult should be creatable")
	}

	if len(result.StageResults) != 0 {
		t.Error("New PipelineResult should have empty stage results")
	}

	// Test StageResult structure
	stageResult := &StageResult{
		StageName:  "test_stage",
		Input:      "test input",
		Output:     "test output",
		TokensUsed: 100,
		Success:    true,
	}

	if stageResult.StageName != "test_stage" {
		t.Error("StageResult should preserve stage name")
	}

	if !stageResult.Success {
		t.Error("StageResult success flag should be settable")
	}
}

func TestPipelineTextChunkerIntegration(t *testing.T) {
	pipeline := createTestPipeline()

	// Test that the pipeline has a text chunker
	if pipeline.chunker == nil {
		t.Fatal("Pipeline should have a text chunker")
	}

	// Test chunker with short text
	shortText := "This is a short text."
	chunks, err := pipeline.chunker.ChunkText(shortText)
	if err != nil {
		t.Errorf("Chunker should handle short text: %v", err)
	}

	if len(chunks) != 1 {
		t.Errorf("Short text should produce 1 chunk, got %d", len(chunks))
	}
}

func TestPipelineTemplateIntegration(t *testing.T) {
	pipeline := createTestPipeline()

	if pipeline.templates == nil {
		t.Fatal("Pipeline should have templates")
	}

	// Test that all required stage templates exist
	correctionCtx := NewCorrectionContext(&meta.FileMetadata{
		Title:   "Test Book",
		Author:  "Test Author",
		Chapter: "Chapter 1",
	}, "test text")

	requiredStages := []string{"spelling_grammar", "formatting", "verification"}
	for _, stage := range requiredStages {
		prompt, err := pipeline.templates.GetPrompt(stage, correctionCtx, "test input")
		if err != nil {
			t.Errorf("Failed to get prompt for stage %s: %v", stage, err)
		}

		if len(prompt) == 0 {
			t.Errorf("Prompt for stage %s should not be empty", stage)
		}

		// Verify the prompt contains the stage-specific information
		if stage == "spelling_grammar" && len(prompt) < 50 {
			t.Errorf("Spelling/grammar prompt seems too short: %d chars", len(prompt))
		}
	}
}

func TestPipelineConstants(t *testing.T) {
	// Test that the pipeline uses expected constants from the chunker
	if MaxTokensPerChunk <= 0 {
		t.Error("MaxTokensPerChunk should be positive")
	}

	if ChunkOverlap <= 0 {
		t.Error("ChunkOverlap should be positive")
	}

	if ChunkOverlap >= MaxTokensPerChunk {
		t.Error("ChunkOverlap should be less than MaxTokensPerChunk")
	}
}

// Note: Full integration tests with actual LLM calls would require:
// 1. Valid API keys and network access
// 2. More sophisticated mocking infrastructure
// 3. Separate integration test suite
//
// The tests above focus on structural integrity and component integration
// without requiring external API calls.
