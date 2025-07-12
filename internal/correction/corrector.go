package correction

import (
	"context"
	"fmt"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/meta"
	"github.com/jedwards1230/lil-whisper/internal/openai"
	"github.com/jedwards1230/lil-whisper/internal/tokenizer"
)

type CorrectionResult struct {
	CorrectedText string
	Metadata      map[string]interface{}
	Error         error
}

type Corrector interface {
	CorrectText(ctx context.Context, rawText string, fileMeta *meta.FileMetadata) (*CorrectionResult, error)
	IsEnabled() bool
}

type LLMCorrector struct {
	client      *openai.Client
	pipeline    *Pipeline
	rateLimiter *RateLimiter
	config      *config.Config
	log         log.Logger
	enabled     bool
}

func New(cfg *config.Config) *LLMCorrector {
	logger := log.NewLogger("correction")
	
	// Check if LLM correction is enabled
	enabled := cfg.LLMCorrectionEnabled
	if !enabled {
		logger.Info("LLM correction is disabled")
		return &LLMCorrector{
			config:  cfg,
			log:     logger,
			enabled: false,
		}
	}

	// Initialize OpenAI client for LLM correction
	client := openai.NewClient(cfg)
	
	// Initialize the three-stage pipeline
	pipeline := NewPipeline(client, logger)

	// Initialize rate limiter
	rateLimiter := NewRateLimiter(
		cfg.LLMCorrectionRateLimit,
		0.01, // Base cost per request - will be calculated dynamically
		cfg.LLMCorrectionDailyBudget,
	)

	logger.Info("LLM correction enabled", 
		"model", cfg.LLMCorrectionModel,
		"base_url", cfg.LLMCorrectionBaseURL,
		"max_retries", cfg.LLMCorrectionMaxRetries,
		"rate_limit", cfg.LLMCorrectionRateLimit,
		"daily_budget", cfg.LLMCorrectionDailyBudget)

	return &LLMCorrector{
		client:      client,
		pipeline:    pipeline,
		rateLimiter: rateLimiter,
		config:      cfg,
		log:         logger,
		enabled:     true,
	}
}

func (c *LLMCorrector) IsEnabled() bool {
	return c.enabled
}

func (c *LLMCorrector) CorrectText(ctx context.Context, rawText string, fileMeta *meta.FileMetadata) (*CorrectionResult, error) {
	if !c.enabled {
		c.log.Debug("LLM correction disabled, returning original text")
		return &CorrectionResult{
			CorrectedText: rawText,
			Metadata: map[string]interface{}{
				"correction_enabled": false,
				"stages_completed":   0,
			},
		}, nil
	}

	if rawText == "" {
		return nil, fmt.Errorf("cannot correct empty text")
	}

	c.log.Debug("Starting LLM text correction", "file", fileMeta.FilePath, "text_length", len(rawText))

	// Estimate cost and check limits
	tokenCount, _ := tokenizer.CountTokens(rawText)
	estimate := c.rateLimiter.EstimateCost(tokenCount)
	
	// Check if we would exceed budget
	if estimate.WouldExceedBudget {
		dailyCost, budget := c.rateLimiter.GetDailyUsage()
		c.log.Warn("Skipping correction due to budget limit",
			"file", fileMeta.FilePath,
			"estimated_cost", estimate.EstimatedCost,
			"daily_cost", dailyCost,
			"budget", budget)
		
		return &CorrectionResult{
			CorrectedText: rawText,
			Metadata: map[string]interface{}{
				"correction_enabled": true,
				"skipped_reason":     "budget_exceeded",
				"estimated_cost":     estimate.EstimatedCost,
				"daily_cost":         dailyCost,
				"budget":             budget,
			},
		}, nil
	}
	
	// Check rate limit and wait if necessary
	if estimate.WouldExceedRate {
		c.log.Info("Rate limit would be exceeded, waiting",
			"file", fileMeta.FilePath,
			"suggested_delay", estimate.SuggestedDelay)
		
		if err := c.rateLimiter.WaitForRateLimit(ctx); err != nil {
			return &CorrectionResult{
				CorrectedText: rawText,
				Metadata: map[string]interface{}{
					"correction_enabled": true,
					"skipped_reason":     "rate_limit_timeout",
					"error":              err.Error(),
				},
			}, nil
		}
	}

	// Create correction context from metadata
	correctionContext := NewCorrectionContext(fileMeta, rawText)

	// Run the three-stage correction pipeline
	result, err := c.pipeline.ProcessStages(ctx, correctionContext, c.config)
	if err != nil {
		c.log.Error("LLM correction failed", "error", err, "file", fileMeta.FilePath)
		return &CorrectionResult{
			CorrectedText: rawText, // Fallback to original text
			Metadata: map[string]interface{}{
				"correction_enabled": true,
				"error":              err.Error(),
				"fallback_used":      true,
			},
			Error: err,
		}, nil // Don't return error, use fallback
	}

	// Record the actual cost (rough estimate based on tokens used)
	actualCost := float64(result.TotalTokens) * 0.00006 // Rough cost per token
	c.rateLimiter.RecordRequest(actualCost)

	c.log.Debug("LLM text correction completed", 
		"file", fileMeta.FilePath,
		"stages_completed", result.StagesCompleted,
		"original_length", len(rawText),
		"corrected_length", len(result.CorrectedText),
		"estimated_cost", estimate.EstimatedCost,
		"actual_cost", actualCost)

	return &CorrectionResult{
		CorrectedText: result.CorrectedText,
		Metadata: map[string]interface{}{
			"correction_enabled": true,
			"model":              c.config.LLMCorrectionModel,
			"stages_completed":   result.StagesCompleted,
			"total_tokens":       result.TotalTokens,
			"processing_time_ms": result.ProcessingTimeMs,
			"estimated_cost":     estimate.EstimatedCost,
			"actual_cost":        actualCost,
			"was_chunked":        result.WasChunked,
			"chunks_processed":   result.ChunksProcessed,
			"stages":             result.StageResults,
		},
	}, nil
}