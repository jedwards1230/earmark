package correction

import (
	"context"
	"testing"
	"time"
)

func TestRateLimiterBasicFunctionality(t *testing.T) {
	rl := NewRateLimiter(2, 0.01, 1.0) // 2 requests per minute, $0.01 per request, $1 daily budget

	ctx := context.Background()

	// First request should pass
	if err := rl.CheckRateLimit(ctx); err != nil {
		t.Errorf("First request should pass: %v", err)
	}

	// Record the request
	rl.RecordRequest(0.01)

	// Second request should pass
	if err := rl.CheckRateLimit(ctx); err != nil {
		t.Errorf("Second request should pass: %v", err)
	}

	// Record the second request
	rl.RecordRequest(0.01)

	// Third request should fail (rate limit)
	if err := rl.CheckRateLimit(ctx); err == nil {
		t.Error("Third request should fail due to rate limit")
	}
}

func TestRateLimiterDailyBudget(t *testing.T) {
	rl := NewRateLimiter(10, 0.01, 0.05) // High rate limit, $0.05 daily budget

	// Test budget checking
	if err := rl.CheckDailyBudget(0.03); err != nil {
		t.Errorf("Request under budget should pass: %v", err)
	}

	// Record some cost
	rl.RecordRequest(0.03)

	// This should now exceed budget
	if err := rl.CheckDailyBudget(0.03); err == nil {
		t.Error("Request over budget should fail")
	}
}

func TestRateLimiterCostEstimation(t *testing.T) {
	rl := NewRateLimiter(10, 0.01, 10.0)

	tests := []struct {
		name       string
		tokenCount int
		expectCost float64 // Approximate cost
	}{
		{
			name:       "small_text",
			tokenCount: 100,
			expectCost: 0.00003, // More realistic estimate for 3-stage pipeline
		},
		{
			name:       "medium_text",
			tokenCount: 1000,
			expectCost: 0.0003, // More realistic estimate
		},
		{
			name:       "large_text",
			tokenCount: 5000,
			expectCost: 0.0015, // More realistic estimate
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimate := rl.EstimateCost(tt.tokenCount)

			// Check that estimate is reasonable (within 50% of expected)
			if estimate.EstimatedCost < tt.expectCost*0.5 || estimate.EstimatedCost > tt.expectCost*1.5 {
				t.Errorf("Cost estimate %f not in reasonable range around %f", 
					estimate.EstimatedCost, tt.expectCost)
			}

			if estimate.EstimatedTokens == 0 {
				t.Error("Estimated tokens should not be zero")
			}
		})
	}
}

func TestRateLimiterBudgetExceeded(t *testing.T) {
	rl := NewRateLimiter(10, 0.01, 0.001) // Very small daily budget: $0.001

	// Test large request that would exceed budget
	estimate := rl.EstimateCost(10000) // Large token count should exceed tiny budget
	
	if !estimate.WouldExceedBudget {
		t.Errorf("Large request should exceed budget, estimated cost: %f, budget: %f", estimate.EstimatedCost, 0.001)
	}
}

func TestRateLimiterRateExceeded(t *testing.T) {
	rl := NewRateLimiter(1, 0.01, 10.0) // Only 1 request per minute

	// First request passes
	rl.RecordRequest(0.01)

	// Estimate for next requests should indicate rate limit exceeded
	// (3-stage pipeline would need 3 requests)
	estimate := rl.EstimateCost(100)
	
	if !estimate.WouldExceedRate {
		t.Error("Should indicate rate limit would be exceeded")
	}

	if estimate.SuggestedDelay <= 0 {
		t.Error("Should suggest a delay when rate limit exceeded")
	}
}

func TestRateLimiterDailyReset(t *testing.T) {
	rl := NewRateLimiter(10, 0.01, 1.0)

	// Record some cost
	rl.RecordRequest(0.50)

	dailyCost, budget := rl.GetDailyUsage()
	if dailyCost != 0.50 {
		t.Errorf("Expected daily cost 0.50, got %f", dailyCost)
	}

	if budget != 1.0 {
		t.Errorf("Expected budget 1.0, got %f", budget)
	}

	// Manually trigger reset by setting lastReset to yesterday
	rl.lastReset = time.Now().Add(-25 * time.Hour)

	// Check budget again - this should trigger reset
	err := rl.CheckDailyBudget(0.10)
	if err != nil {
		t.Errorf("After reset, budget check should pass: %v", err)
	}

	// Verify cost was reset
	dailyCost, _ = rl.GetDailyUsage()
	if dailyCost != 0 {
		t.Errorf("Daily cost should be reset to 0, got %f", dailyCost)
	}
}

func TestRateLimiterWaitForRateLimit(t *testing.T) {
	rl := NewRateLimiter(1, 0.01, 10.0) // 1 request per minute

	// Fill up the rate limit
	rl.RecordRequest(0.01)

	// Create context with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// WaitForRateLimit should timeout
	err := rl.WaitForRateLimit(ctx)
	if err != context.DeadlineExceeded {
		t.Errorf("Expected context deadline exceeded, got: %v", err)
	}
}

func TestRateLimiterCurrentUsage(t *testing.T) {
	rl := NewRateLimiter(5, 0.01, 10.0)

	// Initially should be 0
	current, max := rl.GetCurrentRateUsage()
	if current != 0 || max != 5 {
		t.Errorf("Expected 0/5 usage, got %d/%d", current, max)
	}

	// Record some requests
	rl.RecordRequest(0.01)
	rl.RecordRequest(0.01)

	current, max = rl.GetCurrentRateUsage()
	if current != 2 || max != 5 {
		t.Errorf("Expected 2/5 usage, got %d/%d", current, max)
	}
}

func TestRateLimiterOldRequestsCleanup(t *testing.T) {
	rl := NewRateLimiter(10, 0.01, 10.0)

	ctx := context.Background()

	// Add an old request (simulate by directly manipulating the slice)
	rl.requests = append(rl.requests, time.Now().Add(-2*time.Minute))

	// Add a recent request
	rl.RecordRequest(0.01)

	// Check rate limit - this should clean up old requests
	err := rl.CheckRateLimit(ctx)
	if err != nil {
		t.Errorf("Rate check should pass after cleanup: %v", err)
	}

	// Verify only recent requests remain
	current, _ := rl.GetCurrentRateUsage()
	if current != 1 {
		t.Errorf("Expected 1 current request after cleanup, got %d", current)
	}
}

func TestRateLimiterConcurrentAccess(t *testing.T) {
	rl := NewRateLimiter(100, 0.01, 10.0) // High limits for concurrency test

	// Run multiple goroutines concurrently
	done := make(chan bool, 10)
	
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- true }()
			
			for j := 0; j < 10; j++ {
				rl.RecordRequest(0.01)
				rl.EstimateCost(100)
				rl.GetDailyUsage()
				rl.GetCurrentRateUsage()
			}
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify final state is reasonable
	dailyCost, _ := rl.GetDailyUsage()
	expectedCost := 1.0 // 10 goroutines * 10 requests * $0.01
	if dailyCost < expectedCost-0.001 || dailyCost > expectedCost+0.001 { // Allow small floating point differences
		t.Errorf("Expected daily cost ~%.3f after concurrent access, got %f", expectedCost, dailyCost)
	}

	current, _ := rl.GetCurrentRateUsage()
	if current != 100 { // 10 goroutines * 10 requests
		t.Errorf("Expected 100 current requests, got %d", current)
	}
}

func TestRateLimiterEdgeCases(t *testing.T) {
	// Test with zero rate limit
	rl := NewRateLimiter(0, 0, 0)
	
	ctx := context.Background()
	
	// Should always fail with zero rate limit (0 >= 0 is true, so it fails)
	err := rl.CheckRateLimit(ctx)
	if err == nil {
		t.Error("Zero rate limit should always fail")
	}

	// Should always fail with zero budget  
	err = rl.CheckDailyBudget(0.01)
	if err == nil {
		t.Error("Zero budget should fail any positive cost")
	}

	// Test zero budget with zero cost (should pass)
	err = rl.CheckDailyBudget(0.0)
	if err != nil {
		t.Errorf("Zero budget should allow zero cost, got error: %v", err)
	}

	// Cost estimation should still work
	estimate := rl.EstimateCost(100)
	if estimate.EstimatedCost <= 0 {
		t.Error("Cost estimation should still work with zero limits")
	}
}