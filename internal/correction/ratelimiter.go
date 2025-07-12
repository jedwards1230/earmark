package correction

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/log"
)

type RateLimiter struct {
	requestsPerMinute int
	costPerRequest    float64
	dailyBudget       float64
	
	// Rate limiting
	requests      []time.Time
	requestsMutex sync.Mutex
	
	// Cost tracking
	dailyCost     float64
	lastReset     time.Time
	costMutex     sync.RWMutex
	
	log log.Logger
}

type CostEstimate struct {
	EstimatedCost       float64
	EstimatedTokens     int
	WouldExceedBudget   bool
	WouldExceedRate     bool
	SuggestedDelay      time.Duration
}

func NewRateLimiter(requestsPerMinute int, costPerRequest, dailyBudget float64) *RateLimiter {
	return &RateLimiter{
		requestsPerMinute: requestsPerMinute,
		costPerRequest:    costPerRequest,
		dailyBudget:       dailyBudget,
		requests:          make([]time.Time, 0),
		lastReset:         time.Now().Truncate(24 * time.Hour),
		log:               log.NewLogger("rate-limiter"),
	}
}

func (rl *RateLimiter) CheckRateLimit(ctx context.Context) error {
	rl.requestsMutex.Lock()
	defer rl.requestsMutex.Unlock()
	
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	
	// Remove requests older than 1 minute
	var recentRequests []time.Time
	for _, reqTime := range rl.requests {
		if reqTime.After(cutoff) {
			recentRequests = append(recentRequests, reqTime)
		}
	}
	rl.requests = recentRequests
	
	// Check if we're at the rate limit
	if len(rl.requests) >= rl.requestsPerMinute && len(rl.requests) > 0 {
		oldestRequest := rl.requests[0]
		waitTime := oldestRequest.Add(time.Minute).Sub(now)
		
		rl.log.Warn("Rate limit reached, would need to wait", 
			"requests_in_minute", len(rl.requests),
			"limit", rl.requestsPerMinute,
			"wait_time", waitTime)
		
		return fmt.Errorf("rate limit exceeded: %d requests per minute, wait %v", 
			rl.requestsPerMinute, waitTime)
	}
	
	return nil
}

func (rl *RateLimiter) CheckDailyBudget(estimatedCost float64) error {
	rl.costMutex.RLock()
	defer rl.costMutex.RUnlock()
	
	// Reset daily cost if it's a new day
	now := time.Now()
	todayStart := now.Truncate(24 * time.Hour)
	if todayStart.After(rl.lastReset) {
		rl.costMutex.RUnlock()
		rl.costMutex.Lock()
		rl.dailyCost = 0
		rl.lastReset = todayStart
		rl.costMutex.Unlock()
		rl.costMutex.RLock()
		
		rl.log.Info("Daily cost reset", "new_day", todayStart.Format("2006-01-02"))
	}
	
	projectedCost := rl.dailyCost + estimatedCost
	if projectedCost > rl.dailyBudget {
		rl.log.Warn("Daily budget would be exceeded",
			"current_cost", rl.dailyCost,
			"estimated_cost", estimatedCost,
			"projected_total", projectedCost,
			"daily_budget", rl.dailyBudget)
		
		return fmt.Errorf("daily budget exceeded: current $%.2f + estimated $%.2f = $%.2f > budget $%.2f",
			rl.dailyCost, estimatedCost, projectedCost, rl.dailyBudget)
	}
	
	return nil
}

func (rl *RateLimiter) RecordRequest(actualCost float64) {
	rl.requestsMutex.Lock()
	rl.requests = append(rl.requests, time.Now())
	rl.requestsMutex.Unlock()
	
	rl.costMutex.Lock()
	rl.dailyCost += actualCost
	rl.costMutex.Unlock()
	
	rl.log.Debug("Recorded API request", 
		"cost", actualCost, 
		"daily_total", rl.dailyCost,
		"requests_in_minute", len(rl.requests))
}

func (rl *RateLimiter) EstimateCost(tokenCount int) CostEstimate {
	// Rough cost estimation (adjust based on your LLM provider)
	// This is based on OpenAI GPT-4 pricing as of late 2024
	const (
		InputTokenCost  = 0.00003  // $0.03 per 1K tokens
		OutputTokenCost = 0.00006  // $0.06 per 1K tokens
	)
	
	// Estimate output tokens as roughly 80% of input tokens
	estimatedInputTokens := tokenCount
	estimatedOutputTokens := int(float64(tokenCount) * 0.8)
	
	inputCost := float64(estimatedInputTokens) * InputTokenCost / 1000
	outputCost := float64(estimatedOutputTokens) * OutputTokenCost / 1000
	totalCost := inputCost + outputCost
	
	// For 3-stage pipeline, multiply by 3
	totalCost *= 3
	
	estimate := CostEstimate{
		EstimatedCost:   totalCost,
		EstimatedTokens: estimatedInputTokens + estimatedOutputTokens,
	}
	
	// Check budget
	rl.costMutex.RLock()
	projectedCost := rl.dailyCost + totalCost
	estimate.WouldExceedBudget = projectedCost > rl.dailyBudget
	rl.costMutex.RUnlock()
	
	// Check rate limit
	rl.requestsMutex.Lock()
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	
	var recentRequests []time.Time
	for _, reqTime := range rl.requests {
		if reqTime.After(cutoff) {
			recentRequests = append(recentRequests, reqTime)
		}
	}
	
	// For 3-stage pipeline, we'll make 3 requests
	wouldExceed := len(recentRequests) + 3 > rl.requestsPerMinute
	estimate.WouldExceedRate = wouldExceed
	
	if wouldExceed && len(recentRequests) > 0 {
		oldestRequest := recentRequests[0]
		estimate.SuggestedDelay = oldestRequest.Add(time.Minute).Sub(now)
	}
	rl.requestsMutex.Unlock()
	
	return estimate
}

func (rl *RateLimiter) GetDailyUsage() (float64, float64) {
	rl.costMutex.RLock()
	defer rl.costMutex.RUnlock()
	
	return rl.dailyCost, rl.dailyBudget
}

func (rl *RateLimiter) GetCurrentRateUsage() (int, int) {
	rl.requestsMutex.Lock()
	defer rl.requestsMutex.Unlock()
	
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	
	var recentRequests int
	for _, reqTime := range rl.requests {
		if reqTime.After(cutoff) {
			recentRequests++
		}
	}
	
	return recentRequests, rl.requestsPerMinute
}

// WaitForRateLimit blocks until it's safe to make a request
func (rl *RateLimiter) WaitForRateLimit(ctx context.Context) error {
	for {
		if err := rl.CheckRateLimit(ctx); err == nil {
			return nil
		}
		
		// Calculate wait time
		rl.requestsMutex.Lock()
		if len(rl.requests) == 0 {
			rl.requestsMutex.Unlock()
			return nil
		}
		
		oldestRequest := rl.requests[0]
		waitTime := oldestRequest.Add(time.Minute).Sub(time.Now())
		rl.requestsMutex.Unlock()
		
		if waitTime <= 0 {
			continue // Try again
		}
		
		rl.log.Info("Waiting for rate limit", "wait_time", waitTime)
		
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitTime):
			// Continue to next iteration
		}
	}
}