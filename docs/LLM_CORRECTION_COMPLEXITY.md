# LLM Text Correction: Hidden Complexity & Integration Points

## Overview

The LLM text correction feature adds significant complexity to the transcription pipeline. This document outlines the hidden complexities, integration challenges, and implementation considerations.

## Hidden Complexity Areas

### 1. Three-Stage Pipeline Coordination
**Challenge**: Managing three sequential LLM calls with different prompts and contexts
- Each stage depends on the output of the previous stage
- Error handling at any stage affects the entire pipeline
- State management across multiple API calls
- Timeout handling for potentially long-running LLM operations

**Integration Points**:
- `internal/worker/worker.go`: Insert correction between transcription and chunking
- `internal/correction/pipeline.go`: New component to orchestrate three stages
- `internal/correction/templates.go`: System prompt templates for each stage

### 2. Metadata Context Management
**Challenge**: Passing rich book metadata to LLM prompts for context-aware correction
- Extracting relevant metadata from various sources (JSON, file paths)
- Formatting metadata consistently for prompt templates
- Handling missing or incomplete metadata gracefully
- Character/token limits in LLM prompts with metadata

**Integration Points**:
- `internal/meta/metadata.go`: Enhance to provide correction-specific context
- `internal/correction/context.go`: Format metadata for prompts
- Template system needs access to: title, author, series, genre, narrator info

### 3. API Rate Limiting & Cost Management
**Challenge**: Three LLM calls per audio file significantly increases API usage
- Rate limiting coordination across multiple calls
- Cost monitoring and budgeting
- Retry logic with exponential backoff
- Fallback strategies when API limits are hit

**Integration Points**:
- `internal/openai/client.go`: Enhance with rate limiting and retry logic
- `internal/config/config.go`: Add cost controls and rate limit settings
- Worker queue may need throttling based on API availability

### 4. Error Handling & Fallback Strategies
**Challenge**: Robust error handling without losing transcription work
- API failures should not lose original transcription
- Partial corrections (1-2 stages complete) need handling
- Network timeouts and service unavailability
- Invalid LLM responses or formatting issues

**Integration Points**:
- Store original transcription before correction attempts
- Implement correction status tracking in database
- Graceful degradation: use best available correction or original text
- Logging and monitoring for correction pipeline health

### 5. Performance Impact
**Challenge**: Three additional API calls significantly slow processing
- Sequential nature of correction stages
- Network latency multiplication (3x minimum)
- Potential for processing bottlenecks
- Memory usage for storing intermediate results

**Integration Points**:
- `internal/worker/worker.go`: May need async/parallel processing
- Consider caching correction results based on content hashes
- Progress reporting for long-running correction operations
- Resource usage monitoring and optimization

## Implementation Strategy

### Phase 1: Basic Pipeline
1. Create `internal/correction/` package with basic three-stage pipeline
2. Integrate into worker between transcription and chunking
3. Simple error handling: fallback to original text on any failure
4. Basic configuration and enable/disable flag

### Phase 2: Enhanced Error Handling
1. Implement sophisticated retry logic and partial completion handling
2. Add correction status tracking to database
3. Implement fallback strategies and graceful degradation
4. Add comprehensive logging and monitoring

### Phase 3: Performance Optimization
1. Implement result caching based on content and settings hashes
2. Add rate limiting and cost controls
3. Consider parallel processing where possible
4. Add performance metrics and optimization

## Configuration Requirements

### New Environment Variables
```bash
# Core LLM correction settings
LLM_CORRECTION_ENABLED=true
LLM_CORRECTION_MODEL=gpt-4o-mini
LLM_CORRECTION_BASE_URL=https://api.openai.com/v1
LLM_CORRECTION_API_KEY=your-api-key-here

# Performance and reliability
LLM_CORRECTION_TEMPERATURE=0.1
LLM_CORRECTION_MAX_RETRIES=3
LLM_CORRECTION_TIMEOUT=120s
LLM_CORRECTION_RATE_LIMIT=10  # requests per minute

# Cost controls
LLM_CORRECTION_MAX_TOKENS=4000
LLM_CORRECTION_DAILY_BUDGET=10.00  # USD
LLM_CORRECTION_COST_ALERT_THRESHOLD=0.80
```

## Database Schema Changes

### Track Correction Status
```sql
ALTER TABLE transcriptions ADD COLUMN correction_status VARCHAR(20) DEFAULT 'pending';
ALTER TABLE transcriptions ADD COLUMN correction_error TEXT;
ALTER TABLE transcriptions ADD COLUMN correction_cost_usd DECIMAL(10,4);
ALTER TABLE transcriptions ADD COLUMN corrected_text TEXT;
ALTER TABLE transcriptions ADD COLUMN correction_metadata JSONB;
```

## Template System Design

### Three Template Types
1. **Spelling & Grammar Template**: Focus on accuracy, proper nouns, basic errors
2. **Formatting Template**: Consistent punctuation, paragraphs, professional presentation
3. **Verification Template**: Validate meaning preservation, final quality check

### Context Variables Available
- `{{.BookTitle}}` - Full book title
- `{{.Author}}` - Author name
- `{{.Series}}` - Series information if available
- `{{.Genre}}` - Genre/category if available
- `{{.ChapterTitle}}` - Current chapter title
- `{{.ChapterIndex}}` - Chapter number/position
- `{{.TranscriptionText}}` - Text to be corrected

## Testing Strategy

### Unit Tests
- Template rendering with various metadata combinations
- Error handling for each failure mode
- Retry logic and timeout behavior
- Cost tracking and budget enforcement

### Integration Tests
- Full pipeline from transcription through corrected chunking
- Database state management across correction stages
- Performance testing with realistic audio file sizes
- API failure simulation and recovery

### Load Testing
- Multiple concurrent files being corrected
- API rate limit testing and throttling
- Memory usage under load
- Processing time analysis

## Monitoring & Observability

### Key Metrics
- Correction success rate per stage
- Average correction time per stage
- API costs per correction operation
- Error rates and types
- Quality improvements (manual sampling)

### Alerts
- High correction failure rates
- API cost budget approaching limits
- Unusually long correction times
- Rate limit violations

## Risk Mitigation

### Primary Risks
1. **Cost Explosion**: Three LLM calls per file can be expensive
   - Mitigation: Daily budget limits, cost monitoring, enable/disable flag
2. **Processing Bottleneck**: Sequential API calls significantly slow processing
   - Mitigation: Async processing, progress reporting, optional feature
3. **API Dependency**: Reliance on external LLM service availability
   - Mitigation: Robust error handling, fallback to original text
4. **Quality Regressions**: LLM might introduce errors instead of fixing them
   - Mitigation: Verification stage, manual quality sampling, rollback capability

### Rollback Strategy
- Keep original transcriptions always available
- Configuration to disable correction and use original text
- Database migration to remove correction-related columns if needed
- Monitoring to detect quality regressions quickly