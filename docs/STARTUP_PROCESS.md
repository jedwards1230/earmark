# Startup Process Overview

## Command: `./lil-whisper start`

This document outlines the step-by-step process that occurs when the transcription service starts up.

## Initialization Sequence

### 1. Configuration Loading
**File**: `cmd/start.go:30`
```go
cfg, err := config.LoadConfig()
```

**Process**:
- Loads configuration from environment variables and `.env` file
- Validates required settings (database, OpenAI API key, directories)
- Sets default values for optional parameters
- Prints environment variables for verification

**Key Settings Loaded**:
- Database connection parameters
- OpenAI API configuration
- Audio and cache directory paths
- Processing parameters (chunk size, similarity thresholds)

### 2. Database Connection & Initialization
**File**: `cmd/start.go:37-48`
```go
database, err := db.New(cfg)
```

**Process**:
- Establishes PostgreSQL connection using configuration
- Initializes database schema if needed:
  - Creates tables: `authors`, `books`, `chapters`, `vectors`, `transcriptions`
  - Sets up pgvector extension for similarity search
  - Creates indexes for performance optimization
- Optionally resets database state if `cfg.ResetState` is true

### 3. Core Component Initialization
**File**: `cmd/start.go:50-52`

#### Work Queue Creation
```go
workQueue := queue.NewQueue()
```
- Creates in-memory FIFO queue for processing coordination
- Thread-safe with mutex protection

#### File Monitor Setup
```go
fileMonitor := monitor.NewFileMonitor(cfg, workQueue, database)
```
- Initializes filesystem monitoring component
- Links to work queue and database
- Prepares for directory scanning and fsnotify watching

#### Worker Initialization  
```go
worker := worker.NewWorker(workQueue, database)
```
- Creates background processing worker
- Links to work queue for job consumption
- Initializes transcription and storage capabilities

### 4. File Monitor Startup (Sequential)
**File**: `cmd/start.go:54-61`
```go
go func() {
    fileMonitor.Start(monitorReady)
}()
<-monitorReady  // Wait for completion
```

**Monitor Startup Process** (`internal/monitor/monitor.go:352`):

#### 4.1 Chunk Size Verification
- Checks existing database chunks against current configuration
- Identifies books with mismatched chunk sizes
- Deletes old chunks and re-queues chapters for reprocessing
- Logs reprocessing statistics

#### 4.2 Library Statistics Collection
- Walks audio directory to count books and files
- Queries database for processing statistics
- Logs comprehensive library overview:
  - Total books and audio files discovered
  - Books and chapters already processed
  - Books queued for reprocessing

#### 4.3 Orphaned File Detection
- Scans for audio files without corresponding metadata.json
- Warns about files that cannot be properly processed
- Helps identify incomplete book imports

#### 4.4 Initial Book Scanning
- Walks directory tree looking for `metadata.json` files
- For each metadata file:
  - Parses book metadata (author, title, ISBN, ASIN, chapters)
  - Finds associated audio files in same directory
  - Matches audio files to chapter information
  - Checks processing status in database
  - Enqueues new/unprocessed files to work queue

#### 4.5 Filesystem Watcher Setup
- Creates fsnotify watcher for real-time monitoring
- Recursively adds all subdirectories to watch list
- Begins monitoring for new file creation events

**Ready Signal**: Monitor signals completion and service proceeds

### 5. Worker Startup (Concurrent)
**File**: `cmd/start.go:64-69`
```go
go func() {
    defer wg.Done()
    worker.Start(cfg)
}()
```

**Worker Process** (`internal/worker/worker.go:41`):
- Begins continuous loop checking work queue
- For each queued item:
  1. **Transcription Check**: Verifies if current transcription exists and is up-to-date
  2. **Audio Transcription**: Uses Yap for local speech recognition if needed
  3. **Text Processing**: Chunks transcription text for embedding generation
  4. **Embedding Generation**: Calls OpenAI API to create vector embeddings
  5. **Database Storage**: Stores raw transcription and vector chunks
  6. **Progress Logging**: Reports completion status and timing

### 6. HTTP Server Startup (Concurrent)
**File**: `cmd/start.go:71-73`
```go
srv := server.NewServer(database, cfg)
httpServer := srv.Start()
```

**Server Process** (`internal/server/server.go:33`):
- Creates HTTP server on port 8080
- Sets up `/search` endpoint for semantic search queries
- Begins accepting search requests
- Logs server readiness with access URL

### 7. Signal Handling Setup
**File**: `cmd/start.go:75-78`
```go
sigChan := make(chan os.Signal, 1)
signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
<-sigChan  // Wait for shutdown signal
```

- Configures graceful shutdown on SIGINT/SIGTERM
- Service runs indefinitely until signal received

## Operational State

### Running Services
Once startup completes, the following services run concurrently:

1. **File Monitor**: 
   - Watches for new audio files
   - Automatically enqueues new content for processing

2. **Worker**: 
   - Continuously processes queued audio files
   - Performs transcription, chunking, and embedding generation
   - Updates database with results

3. **HTTP Server**: 
   - Accepts search queries on http://localhost:8080/search
   - Performs vector similarity search against processed content
   - Returns ranked results with metadata

Note: Currently all services start together with `./lil-whisper start`. Planned refactor will split into separate monitor/transcribe and serve commands.

### Processing Pipeline
When new audio files are detected:
```
[New Audio File] → [Monitor Detects] → [Queue Enqueue] → [Worker Dequeue] 
    → [Transcribe] → [Chunk Text] → [Generate Embeddings] → [Store in DB]
```

## Shutdown Process
**File**: `cmd/start.go:80-102`

When shutdown signal received:
1. **Service Shutdown**: Stop monitor and worker
2. **HTTP Graceful Shutdown**: 30-second timeout for in-flight requests  
3. **Wait for Completion**: All goroutines finish processing
4. **Database Cleanup**: Close database connections
5. **Exit**: Clean service termination

## Error Handling

### Startup Failures
- **Configuration errors**: Service exits immediately with error message
- **Database connection failures**: Service exits with connection details
- **Directory access issues**: Monitor logs warnings but continues
- **Missing dependencies**: Transcriber validates Yap availability

### Runtime Resilience
- **Transcription failures**: Logged but don't stop service
- **Database errors**: Retried with exponential backoff
- **Queue overflow**: Additional items wait until space available
- **API failures**: OpenAI errors logged, processing continues for other files

## Performance Characteristics

### Startup Time
- **Small libraries** (< 100 files): 1-5 seconds
- **Large libraries** (> 1000 files): 30-60 seconds for initial scan
- **Cold start**: Additional time for database schema creation

### Resource Usage
- **Memory**: Scales with queue size and concurrent processing
- **CPU**: Intensive during transcription and embedding generation  
- **Disk**: Temporary files created during transcription
- **Network**: OpenAI API calls for embedding generation

### Throughput
- **Transcription**: Depends on audio length and hardware
- **Processing**: Limited by OpenAI API rate limits
- **Search**: Sub-second response times for vector queries