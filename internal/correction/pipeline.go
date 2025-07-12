package correction

import (
	"context"
	"fmt"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/openai"
	"github.com/jedwards1230/lil-whisper/internal/tokenizer"
)

type StageResult struct {
	StageName     string `json:"stage_name"`
	Input         string `json:"input"`
	Output        string `json:"output"`
	TokensUsed    int    `json:"tokens_used"`
	Duration      int64  `json:"duration_ms"`
	Success       bool   `json:"success"`
	ErrorMessage  string `json:"error_message,omitempty"`
}

type PipelineResult struct {
	CorrectedText     string        `json:"corrected_text"`
	StagesCompleted   int           `json:"stages_completed"`
	TotalTokens       int           `json:"total_tokens"`
	ProcessingTimeMs  int64         `json:"processing_time_ms"`
	StageResults      []StageResult `json:"stage_results"`
	ChunksProcessed   int           `json:"chunks_processed"`
	WasChunked        bool          `json:"was_chunked"`
}

type Pipeline struct {
	client    *openai.Client
	log       log.Logger
	templates *Templates
	chunker   *TextChunker
}

func NewPipeline(client *openai.Client, logger log.Logger) *Pipeline {
	return &Pipeline{
		client:    client,
		log:       logger,
		templates: NewTemplates(),
		chunker:   NewTextChunker(),
	}
}

func (p *Pipeline) ProcessStages(ctx context.Context, correctionCtx *CorrectionContext, cfg *config.Config) (*PipelineResult, error) {
	startTime := time.Now()
	
	result := &PipelineResult{
		StageResults: make([]StageResult, 0, 3),
	}

	// Check if text needs chunking
	tokenCount, err := tokenizer.CountTokens(correctionCtx.OriginalText)
	if err != nil {
		p.log.Warn("Failed to count tokens, proceeding without chunking", "error", err)
		tokenCount = MaxTokensPerChunk + 1 // Force non-chunked processing
	}

	// Add buffer for prompt template overhead
	const PromptOverhead = 1000
	needsChunking := tokenCount > (MaxTokensPerChunk - PromptOverhead)

	if needsChunking {
		p.log.Debug("Text exceeds token limits, using chunked processing", 
			"tokens", tokenCount, 
			"max_tokens", MaxTokensPerChunk)
		return p.processWithChunking(ctx, correctionCtx, cfg, startTime, result)
	} else {
		p.log.Debug("Text within token limits, using standard processing", 
			"tokens", tokenCount)
		return p.processStandard(ctx, correctionCtx, cfg, startTime, result)
	}
}

func (p *Pipeline) processStandard(ctx context.Context, correctionCtx *CorrectionContext, cfg *config.Config, startTime time.Time, result *PipelineResult) (*PipelineResult, error) {
	currentText := correctionCtx.OriginalText
	result.WasChunked = false
	result.ChunksProcessed = 1
	
	// Stage 1: Spelling & Grammar Correction
	stage1Result, err := p.processStage(ctx, "spelling_grammar", currentText, correctionCtx, cfg)
	result.StageResults = append(result.StageResults, *stage1Result)
	result.TotalTokens += stage1Result.TokensUsed
	
	if err != nil {
		return result, fmt.Errorf("stage 1 (spelling_grammar) failed: %w", err)
	}
	
	result.StagesCompleted = 1
	currentText = stage1Result.Output

	// Stage 2: Formatting & Structure Correction
	stage2Result, err := p.processStage(ctx, "formatting", currentText, correctionCtx, cfg)
	result.StageResults = append(result.StageResults, *stage2Result)
	result.TotalTokens += stage2Result.TokensUsed
	
	if err != nil {
		// Use stage 1 result as fallback
		result.CorrectedText = stage1Result.Output
		return result, fmt.Errorf("stage 2 (formatting) failed, using stage 1 result: %w", err)
	}
	
	result.StagesCompleted = 2
	currentText = stage2Result.Output

	// Stage 3: Verification & Final Polish
	stage3Result, err := p.processStage(ctx, "verification", currentText, correctionCtx, cfg)
	result.StageResults = append(result.StageResults, *stage3Result)
	result.TotalTokens += stage3Result.TokensUsed
	
	if err != nil {
		// Use stage 2 result as fallback
		result.CorrectedText = stage2Result.Output
		return result, fmt.Errorf("stage 3 (verification) failed, using stage 2 result: %w", err)
	}
	
	result.StagesCompleted = 3
	result.CorrectedText = stage3Result.Output
	result.ProcessingTimeMs = time.Since(startTime).Milliseconds()

	return result, nil
}

func (p *Pipeline) processWithChunking(ctx context.Context, correctionCtx *CorrectionContext, cfg *config.Config, startTime time.Time, result *PipelineResult) (*PipelineResult, error) {
	// Chunk the text
	chunks, err := p.chunker.ChunkText(correctionCtx.OriginalText)
	if err != nil {
		return result, fmt.Errorf("failed to chunk text: %w", err)
	}

	result.WasChunked = true
	result.ChunksProcessed = len(chunks)
	
	p.log.Debug("Processing text in chunks", "chunk_count", len(chunks))

	// Process each chunk through all stages
	var correctedChunks []string
	
	for _, chunk := range chunks {
		// Create a temporary context for this chunk
		chunkContext := &CorrectionContext{
			BookTitle:    correctionCtx.BookTitle,
			Author:       correctionCtx.Author,
			ChapterTitle: correctionCtx.ChapterTitle,
			ChapterIndex: correctionCtx.ChapterIndex,
			OriginalText: chunk.Text,
			FilePath:     correctionCtx.FilePath,
			ISBN:         correctionCtx.ISBN,
			ASIN:         correctionCtx.ASIN,
		}

		currentText := chunk.Text

		// Stage 1: Spelling & Grammar Correction
		stage1Result, err := p.processStage(ctx, "spelling_grammar", currentText, chunkContext, cfg)
		result.StageResults = append(result.StageResults, *stage1Result)
		result.TotalTokens += stage1Result.TokensUsed
		
		if err != nil {
			p.log.Warn("Stage 1 failed for chunk, using original", 
				"chunk_index", chunk.Index, 
				"error", err)
			correctedChunks = append(correctedChunks, currentText)
			continue
		}
		
		currentText = stage1Result.Output

		// Stage 2: Formatting & Structure Correction
		stage2Result, err := p.processStage(ctx, "formatting", currentText, chunkContext, cfg)
		result.StageResults = append(result.StageResults, *stage2Result)
		result.TotalTokens += stage2Result.TokensUsed
		
		if err != nil {
			p.log.Warn("Stage 2 failed for chunk, using stage 1 result", 
				"chunk_index", chunk.Index, 
				"error", err)
			correctedChunks = append(correctedChunks, stage1Result.Output)
			continue
		}
		
		currentText = stage2Result.Output

		// Stage 3: Verification & Final Polish
		stage3Result, err := p.processStage(ctx, "verification", currentText, chunkContext, cfg)
		result.StageResults = append(result.StageResults, *stage3Result)
		result.TotalTokens += stage3Result.TokensUsed
		
		if err != nil {
			p.log.Warn("Stage 3 failed for chunk, using stage 2 result", 
				"chunk_index", chunk.Index, 
				"error", err)
			correctedChunks = append(correctedChunks, stage2Result.Output)
			continue
		}
		
		correctedChunks = append(correctedChunks, stage3Result.Output)
	}

	// Reassemble the corrected chunks
	result.CorrectedText = p.chunker.ReassembleChunks(correctedChunks)
	result.StagesCompleted = 3 // Assume success if we got here
	result.ProcessingTimeMs = time.Since(startTime).Milliseconds()

	p.log.Debug("Completed chunked processing", 
		"chunks_processed", len(chunks),
		"total_tokens", result.TotalTokens,
		"duration_ms", result.ProcessingTimeMs)

	return result, nil
}

func (p *Pipeline) processStage(ctx context.Context, stageName, input string, correctionCtx *CorrectionContext, cfg *config.Config) (*StageResult, error) {
	startTime := time.Now()
	
	stageResult := &StageResult{
		StageName: stageName,
		Input:     input,
	}

	// Get the appropriate prompt template for this stage
	prompt, err := p.templates.GetPrompt(stageName, correctionCtx, input)
	if err != nil {
		stageResult.Success = false
		stageResult.ErrorMessage = err.Error()
		stageResult.Duration = time.Since(startTime).Milliseconds()
		return stageResult, fmt.Errorf("failed to get prompt for stage %s: %w", stageName, err)
	}

	p.log.Debug("Processing correction stage", 
		"stage", stageName, 
		"input_length", len(input),
		"prompt_length", len(prompt))

	// Make LLM API call with retry logic
	response, tokenCount, err := p.callLLMWithRetry(ctx, prompt, cfg)
	if err != nil {
		stageResult.Success = false
		stageResult.ErrorMessage = err.Error()
		stageResult.Duration = time.Since(startTime).Milliseconds()
		return stageResult, fmt.Errorf("LLM call failed for stage %s: %w", stageName, err)
	}

	stageResult.Output = response
	stageResult.TokensUsed = tokenCount
	stageResult.Duration = time.Since(startTime).Milliseconds()
	stageResult.Success = true

	p.log.Debug("Stage completed", 
		"stage", stageName,
		"output_length", len(response),
		"tokens_used", tokenCount,
		"duration_ms", stageResult.Duration)

	return stageResult, nil
}

func (p *Pipeline) callLLMWithRetry(ctx context.Context, prompt string, cfg *config.Config) (string, int, error) {
	maxRetries := cfg.LLMCorrectionMaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	var lastErr error
	
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff
			backoff := time.Duration(attempt*attempt) * time.Second
			p.log.Debug("Retrying LLM call", "attempt", attempt+1, "backoff", backoff)
			
			select {
			case <-ctx.Done():
				return "", 0, ctx.Err()
			case <-time.After(backoff):
			}
		}

		response, tokenCount, err := p.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model: cfg.LLMCorrectionModel,
			Messages: []openai.Message{
				{
					Role:    "user",
					Content: prompt,
				},
			},
			Temperature: cfg.LLMCorrectionTemperature,
			MaxTokens:   cfg.LLMCorrectionMaxTokens,
		})

		if err == nil {
			return response, tokenCount, nil
		}

		lastErr = err
		p.log.Warn("LLM call failed", "attempt", attempt+1, "error", err)
	}

	return "", 0, fmt.Errorf("all %d retry attempts failed, last error: %w", maxRetries, lastErr)
}