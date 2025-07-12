# Architecture Overview

## System Components

The lilbro-whisper system is a sophisticated audiobook transcription and search service built with Go. It follows a modular architecture with distinct components that work together to monitor, transcribe, process, and search audio content.

### Core Components

#### 1. File Monitor (`internal/monitor/`)
**Purpose**: Watches filesystem for new audio files and manages initial processing queue

**Key Features**:
- Uses `fsnotify` for real-time filesystem monitoring
- Scans directory tree for metadata.json files and associated audio files
- Implements deduplication by checking processed files in database
- Handles orphaned audio files (files without metadata)
- Validates chunk sizes and triggers reprocessing when needed
- Extracts metadata from various sources (JSON files, file paths)

**Interactions**:
- Enqueues audio files → `Queue`
- Checks processing status → `Database`
- Parses metadata → `Meta` parsers

#### 2. Worker (`internal/worker/`)
**Purpose**: Processes queued audio files by transcribing and storing content

**Key Features**:
- Dequeues files from processing queue
- Orchestrates transcription pipeline
- Implements transcription caching with checksums
- Stores raw transcriptions and processed chunks
- Handles processing statistics and error recovery

**Interactions**:
- Dequeues work items ← `Queue` 
- Transcribes audio → `Transcriber`
- Stores content → `Database`
- Generates embeddings → `OpenAI`

#### 3. Database Layer (`internal/db/`)
**Purpose**: PostgreSQL database interface with pgvector for semantic search

**Key Features**:
- Hierarchical schema: Authors → Books → Chapters → Vectors
- SHA256-based deduplication for transcriptions
- Vector similarity search using pgvector extension
- CRUD operations for all entities
- Processing statistics and status tracking

**Schema Structure**:
```
authors (id, name)
├── books (id, author_id, title, isbn, asin)
    ├── chapters (id, book_id, title, index)
        └── vectors (id, chapter_id, content, embedding, chunk_index)
transcriptions (id, file_path, checksum, text, word_count)
```

#### 4. Transcription Engine (`internal/transcribe/`)
**Purpose**: Audio-to-text conversion using Yap (Apple Speech Recognition)

**Key Features**:
- Local transcription using macOS native speech recognition
- Optimized for speed and accuracy
- Handles multiple audio formats (mp3, m4a, m4b, etc.)
- Cache management for temporary files
- Error handling for process failures

**Process Flow**:
1. Validates audio file format
2. Invokes Yap transcription tool
3. Reads generated text output
4. Returns raw transcription text (passed to LLM Correction)

#### 5. LLM Text Correction (`internal/correction/`) 
**Purpose**: Production-ready post-transcription text correction using OpenAI-compatible LLM APIs

**Core Components**:
- **Corrector** (`corrector.go`): Main interface with rate limiting and cost control integration
- **Pipeline** (`pipeline.go`): Three-stage correction workflow with chunking support  
- **Text Chunker** (`textchunker.go`): Intelligent text splitting for long transcriptions
- **Rate Limiter** (`ratelimiter.go`): API throttling and daily budget enforcement
- **File Manager** (`filemanager.go`): Dual storage system for raw and corrected text
- **Context** (`context.go`): Metadata-aware correction prompts
- **Templates** (`templates.go`): Stage-specific LLM prompts

**Production Features**:
- **Token Limit Handling**: Automatically chunks text exceeding model limits (3000 tokens) with 200-token overlap
- **Atomic Database Transactions**: Two-phase commit ensures correction status consistency
- **Rate Limiting**: Configurable requests per minute with automatic throttling
- **Cost Control**: Daily budget tracking with automatic cutoff to prevent unexpected charges
- **Dual Text Storage**: Raw transcriptions and corrected text stored separately (database + local files)
- **Graceful Degradation**: Falls back to original text if correction fails at any stage
- **Comprehensive Error Handling**: Robust retry logic and detailed error reporting

**Three-Stage Correction Pipeline**:
1. **Spelling & Grammar Pass**: Corrects transcription errors, proper nouns, and basic grammar
2. **Formatting Pass**: Ensures consistent formatting, punctuation, and paragraph structure  
3. **Verification Pass**: Final quality check while preserving original meaning
4. **Chunked Processing**: For long texts, each chunk goes through all stages before reassembly

**Advanced Features**:
- **Context-Aware Prompts**: Uses book title, author, chapter info for accurate corrections
- **Cost Estimation**: Pre-calculation of API costs with budget checking
- **Processing Metrics**: Token usage, timing, and success rate tracking  
- **Configurable Models**: Support for any OpenAI-compatible API endpoint
- **Fallback Strategies**: Multi-level fallback from stage 3 → stage 2 → stage 1 → original text

#### 6. Text Processing Pipeline
**Chunker (`internal/chunker/`)**:
- Splits corrected transcription text into semantic chunks
- Supports multiple chunking strategies (char, word, token)
- Configurable chunk sizes for optimal embedding generation

**Tokenizer (`internal/tokenizer/`)**:
- Text tokenization for processing
- Word and token counting utilities

#### 7. OpenAI Integration (`internal/openai/`)
**Purpose**: Generates vector embeddings for semantic search and provides LLM API for text correction

**Key Features**:
- Uses OpenAI-compatible APIs for both embeddings and text correction
- Configurable models, base URLs, and parameters
- Batch processing for efficiency
- Error handling and retry logic
- Support for multiple API providers (OpenAI, local models, etc.)

#### 8. Web Server (`internal/server/`)
**Purpose**: HTTP API for searching transcribed content

**Key Features**:
- RESTful search endpoint (`/search`)
- Query parameters: `q` (query), `p` (threshold), `k` (limit)
- JSON response format with metadata
- Similarity threshold filtering

**Search Response**:
```json
{
  "query": "search terms",
  "count": 5,
  "results": [
    {
      "content": "matching text chunk",
      "author": "Author Name",
      "title": "Book Title", 
      "chapter": "Chapter Name",
      "similarity": 0.87,
      "chunkIndex": 3,
      "totalChunks": 45
    }
  ]
}
```

#### 9. MCP Server (`internal/mcp/`)
**Purpose**: Model Context Protocol server for AI assistant integration

**Key Features**:
- Four integrated tools for audiobook search and browsing
- Support for both stdio and HTTP transports
- Interface-based architecture for clean separation from database layer
- Comprehensive formatting for AI assistant display
- Environment-based configuration

**Components**:
- `types.go`: Formatting functions for MCP responses (search results, library hierarchy)
- `tools.go`: Tool handlers implementing MCP tool interface with database abstraction
- `server.go`: MCP server setup and transport configuration
- Complete test coverage using TDD approach

**Available Tools**:
1. **semantic_search_audiobooks**: Vector similarity search
2. **text_search_audiobooks**: PostgreSQL full-text search
3. **browse_audiobook_library**: Hierarchical library browsing
4. **get_chunk_context**: Retrieve surrounding chunks for context

*See `internal/mcp/README.md` for detailed tool documentation.*

**Integration**:
- Integrated as Cobra command (`./lil-whisper mcp`)
- Environment variable configuration (`MCP_TRANSPORT`, `MCP_HTTP_ADDR`)
- Compatible with Claude Desktop and other MCP clients

#### 10. Queue System (`internal/queue/`)
**Purpose**: In-memory work queue for processing coordination

**Key Features**:
- Thread-safe FIFO queue
- Simple enqueue/dequeue operations
- Bounded capacity (configurable)
- Status checking utilities

#### 11. Configuration Management (`internal/config/`)
**Purpose**: Centralized configuration with environment variable support

**Key Settings**:
- **Database**: Connection parameters for PostgreSQL with pgvector
- **OpenAI API**: Base URL, API keys for both embeddings and LLM correction
- **Directories**: Audio input, cache, output paths for raw and corrected text
- **Processing**: Chunk sizes, similarity thresholds, token limits

**LLM Correction Configuration**:
- `LLM_CORRECTION_ENABLED`: Enable/disable LLM correction (default: false)
- `LLM_CORRECTION_MODEL`: Model name (default: "gpt-4o-mini")
- `LLM_CORRECTION_BASE_URL`: API endpoint URL (default: OpenAI)
- `LLM_CORRECTION_API_KEY`: API key (default: uses OpenAI key)
- `LLM_CORRECTION_TEMPERATURE`: Response randomness (default: 0.1)
- `LLM_CORRECTION_MAX_RETRIES`: Retry attempts (default: 3)
- `LLM_CORRECTION_MAX_TOKENS`: Token limit per request (default: 4000)
- `LLM_CORRECTION_RATE_LIMIT`: Requests per minute (default: 10)
- `LLM_CORRECTION_DAILY_BUDGET`: Daily cost limit in USD (default: $10.00)
- `LLM_CORRECTION_TIMEOUT_MIN`: Request timeout (default: 30 minutes)

**Feature Flags**: Debug mode, reset state, correction enablement

#### 12. Metadata System (`internal/meta/`)
**Purpose**: Extracts and manages book/chapter metadata

**Key Features**:
- Multiple metadata parsers (JSON, file path extraction)
- Book metadata structure (author, title, ISBN, ASIN)
- Chapter information and ordering
- File metadata association
- Metadata context for LLM correction prompts

#### 13. Logging System (`internal/log/`)
**Purpose**: Structured logging across all components

**Key Features**:
- Component-specific loggers with module names (e.g., `[transcribe]`, `[worker]`)
- Structured key-value logging using Go's `slog` package
- Three configurable log levels:
  - **Info/Warn/Error**: Always enabled (default behavior)
  - **Debug**: Enabled via `LOG_DEBUG=1` environment variable
  - **Verbose**: Enabled via `LOG_VERBOSE=1` environment variable (includes raw transcription text)
- Color-coded console output with level symbols (`→` info, `•` debug, `…` verbose, `!` warn, `✗` error)
- Performance and error tracking
- LLM correction pipeline monitoring

## Component Relationships

### Data Flow
```
[Audio Files] → [Monitor] → [Queue] → [Worker] → [Transcriber] → [Raw Text]
                    ↓           ↑         ↓                          ↓
                [Database] ←────┴─────[File Manager] ←──────── [Raw Text Storage]
                    ↑                     ↓
                    ↑              [LLM Corrector] ← [Metadata Context]
                    ↑                     ↓
                    ↑              [Rate Limiter] → [Cost Control]
                    ↑                     ↓
                    ↑              [3-Stage Pipeline]:
                    ↑                ├─ Spelling/Grammar
                    ↑                ├─ Formatting  
                    ↑                └─ Verification
                    ↑                     ↓
                    ↑              [Text Chunker] (for long texts)
                    ↑                     ↓
                    ↑              [Corrected Text] → [Corrected Text Storage]
                    ↑                     ↓                    ↓
                    ↑              [Atomic DB Update] ← [File Manager]
                    ↑                     ↓
                    ↑              [Chunker] → [OpenAI Embeddings] → [Database]
                    ↑                                                     ↓
                    ├─────[Web Server] ←──────────────────────────────[Database]
                    ↓
                [MCP Server] ✅ COMPLETED
```

### Service Dependencies
1. **Monitor** depends on: Database, Queue, Meta parsers
2. **Worker** depends on: Queue, Database, Transcriber, LLM Corrector, File Manager, Chunker, OpenAI
3. **LLM Corrector** depends on: OpenAI-compatible API, Pipeline, Rate Limiter, Text Chunker, Metadata system
4. **Pipeline** depends on: OpenAI Client, Templates, Text Chunker
5. **Rate Limiter** depends on: Time-based tracking, Cost estimation
6. **File Manager** depends on: Local filesystem, Configuration
7. **Text Chunker** depends on: Tokenizer, Sentence boundary detection
8. **Web Server** depends on: Database
9. **MCP Server** depends on: Database (same as Web Server)
10. **Database** depends on: OpenAI (for embeddings), Chunker, PostgreSQL with pgvector
11. **Transcriber** depends on: External Yap tool

### Communication Patterns
- **Synchronous**: Direct function calls within components, LLM API calls
- **Asynchronous**: Queue-based worker processing
- **Event-driven**: Filesystem monitoring with fsnotify
- **Request-response**: HTTP API for search queries and LLM correction calls

## Production Safety & Monitoring

### LLM Correction Safety Features
- **Cost Control**: Daily budget limits prevent unexpected API charges
- **Rate Limiting**: Automatic throttling prevents API quota exhaustion  
- **Atomic Transactions**: Two-phase commit ensures data consistency
- **Graceful Degradation**: Multiple fallback strategies maintain service availability
- **Token Management**: Automatic chunking prevents model limit violations
- **Error Recovery**: Comprehensive retry logic with exponential backoff

### Monitoring & Observability
- **Processing Metrics**: Token usage, cost tracking, timing statistics
- **Correction Status**: Success rates, failure modes, fallback usage
- **Database Health**: Transaction success, consistency checks
- **API Performance**: Response times, rate limit status, error rates

## Scalability Considerations

### Current Limitations
- Single-threaded worker processing
- In-memory queue (not persistent)
- Local filesystem dependency
- Single database connection

### Potential Improvements
- Multi-worker processing pool
- Persistent queue system (Redis, etc.)
- Distributed storage support
- Connection pooling
- Horizontal scaling of workers

## Security Features

### Data Protection
- **SHA256 Checksums**: File integrity verification and deduplication
- **Dual Storage**: Raw and corrected text stored separately for audit trails
- **Database Transactions**: ACID compliance with atomic operations
- **Input Validation**: Comprehensive validation in all API endpoints

### API Security  
- **Rate Limiting**: Prevents API abuse and cost runaway
- **Budget Controls**: Hard limits on daily LLM API spending
- **Timeout Protection**: Request timeouts prevent hung operations
- **Error Sanitization**: Sensitive information filtered from error responses

### Configuration Security
- **Environment Variables**: No hardcoded secrets or credentials
- **PostgreSQL Prepared Statements**: SQL injection prevention
- **API Key Management**: Separate keys for different services
- **Secure Defaults**: Conservative default settings for production safety