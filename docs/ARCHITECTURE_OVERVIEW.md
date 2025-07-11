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
**Purpose**: Post-transcription text correction using OpenAI-compatible LLM APIs

**Key Features**:
- Three-stage correction pipeline for optimal accuracy
- Utilizes book metadata for context-aware corrections
- OpenAI-compatible API support with configurable endpoints
- Template-based system prompts for each correction stage
- Error handling and fallback to original text if corrections fail

**Process Flow**:
1. **Spelling & Grammar Pass**: Corrects transcription errors, proper nouns, and grammar
2. **Formatting Pass**: Ensures consistent formatting, punctuation, and paragraph structure  
3. **Verification Pass**: Validates that meaning and content remain unchanged from original
4. Returns final corrected text for chunking and embedding

**Template System**:
- Metadata-aware prompts including book title, author, series information
- Stage-specific instructions optimized for each correction type
- Context preservation to maintain original meaning and intent

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

#### 9. Queue System (`internal/queue/`)
**Purpose**: In-memory work queue for processing coordination

**Key Features**:
- Thread-safe FIFO queue
- Simple enqueue/dequeue operations
- Bounded capacity (configurable)
- Status checking utilities

#### 10. Configuration Management (`internal/config/`)
**Purpose**: Centralized configuration with environment variable support

**Key Settings**:
- Database connection parameters
- OpenAI API configuration (includes LLM correction endpoints)
- Directory paths for audio and cache
- Processing parameters (chunk size, thresholds)
- LLM correction settings (model, temperature, retry logic)
- Feature flags and operational settings

#### 11. Metadata System (`internal/meta/`)
**Purpose**: Extracts and manages book/chapter metadata

**Key Features**:
- Multiple metadata parsers (JSON, file path extraction)
- Book metadata structure (author, title, ISBN, ASIN)
- Chapter information and ordering
- File metadata association
- Metadata context for LLM correction prompts

#### 12. Logging System (`internal/log/`)
**Purpose**: Structured logging across all components

**Key Features**:
- Component-specific loggers
- Structured key-value logging
- Configurable log levels
- Performance and error tracking
- LLM correction pipeline monitoring

## Component Relationships

### Data Flow
```
[Audio Files] → [Monitor] → [Queue] → [Worker] → [Transcriber] → [Raw Text]
                    ↓           ↑         ↓                          ↓
                [Database] ←────┴─────[LLM Corrector] ← [Metadata Context]
                    ↑                     ↓
                    ↑              [Corrected Text]
                    ↑                     ↓
                    ↑              [Chunker] → [OpenAI Embeddings] → [Database]
                    ↑                                                     ↓
                    ├─────[Web Server] ←──────────────────────────────[Database]
                    ↓
                [MCP Server] (planned)
```

### Service Dependencies
1. **Monitor** depends on: Database, Queue, Meta parsers
2. **Worker** depends on: Queue, Database, Transcriber, LLM Corrector, Chunker, OpenAI
3. **LLM Corrector** depends on: OpenAI-compatible API, Metadata system
4. **Web Server** depends on: Database
5. **MCP Server** depends on: Database (same as Web Server)
6. **Database** depends on: OpenAI (for embeddings), Chunker
7. **Transcriber** depends on: External Yap tool

### Communication Patterns
- **Synchronous**: Direct function calls within components, LLM API calls
- **Asynchronous**: Queue-based worker processing
- **Event-driven**: Filesystem monitoring with fsnotify
- **Request-response**: HTTP API for search queries and LLM correction calls

## Planned Components

### MCP Server (Model Context Protocol)
**Purpose**: Expose transcription service capabilities to LLM applications and AI assistants

**Key Features**:
- **Tools**: search, list_books, get_book_info, get_chapter, transcription_status
- **Resources**: book://{id}, search://{query}, stats://library  
- **Library**: `github.com/mark3labs/mcp-go`
- **Integration**: Parallel to HTTP server, sharing database layer

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
- SHA256 checksums for file integrity
- Environment variable configuration
- Input validation in API endpoints
- PostgreSQL prepared statements
- No credential storage in code