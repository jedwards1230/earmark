# Copilot Instructions for lilbro-whisper

## Architecture Overview

This is a **macOS-specific audiobook transcription service** that uses Yap (Apple's native speech recognition), LLM-powered text correction, and OpenAI embeddings for semantic search. The service follows a **producer-consumer pattern** with filesystem monitoring → queue → worker processing → PostgreSQL storage.

### Core Data Flow
```
FileMonitor → Queue → Worker → [Yap Transcription → LLM Correction → Chunking → Embeddings] → PostgreSQL+pgvector
```

## Key Architectural Patterns

### 1. Modular Internal Package Structure
- Each `internal/` package is self-contained with its own logger and interfaces
- Components communicate via dependency injection (see `cmd/start.go` initialization)
- Database operations are centralized in `internal/db/` with hierarchical schema: `authors → books → chapters → vectors`

### 2. Configuration Management
- **Environment-first**: All config via env vars, `.env` file support (see `internal/config/config.go`)
- **Required vars**: `AUDIO_DIR`, `DB_*`, `OPENAI_API_KEY` (check `readme.md` for complete list)
- Configuration validation happens at startup with clear error messages

### 3. Error Handling & Logging
- Structured logging with component-specific loggers: `log.NewLogger("component_name")`
- **Fail-fast principle**: Configuration/dependency errors exit immediately in constructors
- **Graceful degradation**: Processing errors logged but don't crash the service

### 4. Testing Patterns
- Table-driven tests (see `internal/transcribe/transcribe_test.go`)
- Interface mocking for database operations in tests
- Test utilities in `internal/utils/utils_test.go` for common patterns
- In development, use the runTests MCP tool for running tests instead of `go test` directly

### 5. Command Structure (CRITICAL REQUIREMENT)
- **ALL CLI functionality MUST use Cobra commands in `cmd/` package**
- NO standalone executables in `cmd/subdirectories/` - integrate as commands instead
- Follow existing pattern: add commands to `cmd/main.go` and implement in separate files
- Example: MCP server is `./lil-whisper mcp`, not a separate binary

## Development Workflows

### Essential Commands
```bash
# Build and run locally
go build -o lil-whisper && ./lil-whisper start

# Run MCP server
./lil-whisper mcp

# List available commands
./lil-whisper --help

# Database via Docker (required dependency)
docker compose up -d

# Run specific component tests
go test ./internal/transcribe -v
go test ./internal/db -v

# Full test suite
go test ./...
```

### Debugging Approach
1. **Component isolation**: Each internal package can be tested independently
2. **Database debugging**: `docker compose exec db psql -U postgres -d transcriber`
3. **Processing flow**: Check logs for specific component (monitor → queue → worker → db)

## Critical Integration Points

### 1. Database Schema (PostgreSQL + pgvector)
- **Hierarchical relations**: Authors → Books → Chapters → Vectors (see `docs/DATABASE_SCHEMA.md`)
- **SHA256 deduplication**: Files deduplicated in `transcriptions` table
- **Vector search**: Uses pgvector extension for semantic similarity

### 2. Yap Integration (macOS Dependency)
- **Local transcription**: No external API calls for speech-to-text
- **Cache management**: Transcriptions cached by file hash in `internal/transcribe/`
- **Audio format support**: Handles .m4b, .mp3 via Yap CLI

### 3. LLM Correction Pipeline (Planned Feature)
- **Three-stage correction**: See `docs/LLM_CORRECTION_COMPLEXITY.md` for implementation details
- **OpenAI-compatible APIs**: Configurable base URL for different providers
- **Cost/rate limiting**: Critical for production deployment

### 4. MCP Server Implementation (COMPLETED)
- **Complete implementation**: Located in `internal/mcp/` package with full test coverage
- **Three tools**: semantic_search_audiobooks, text_search_audiobooks, browse_audiobook_library
- **Transport support**: Both stdio (default) and HTTP transports via environment configuration
- **Cobra integration**: Accessible via `./lil-whisper mcp` command (not standalone binary)
- **AI assistant compatibility**: Works with Claude Desktop and other MCP clients

## Project-Specific Conventions

### 1. Metadata Handling
- **Dual sources**: JSON metadata files + file path extraction (see `internal/meta/`)
- **Libation integration**: Supports Libation's audiobook directory structure
- **Orphaned files**: Audio files without metadata still processed with path-derived info

### 2. Queue Management
- **In-memory channels**: Simple producer-consumer with `internal/queue/`
- **Graceful shutdown**: Uses context cancellation throughout (see `cmd/start.go`)
- **Worker coordination**: Monitor waits for initial scan before starting worker

### 3. Documentation Maintenance
- **Live documentation**: Keep `docs/` directory updated with code changes
- **Architecture docs**: `ARCHITECTURE_OVERVIEW.md`, `DATABASE_SCHEMA.md` must reflect actual implementation
- **API documentation**: Update `docs/API_REFERENCE.md` when modifying search endpoints or MCP tools
- **MCP documentation**: Update MCP-related documentation when modifying tools or server configuration

## External Dependencies

- **macOS 26+ required**: Yap speech recognition dependency
- **PostgreSQL + pgvector**: Database with vector extension
- **OpenAI API**: For embeddings and planned LLM correction
- **Go 1.23+**: Module uses modern Go features

## Common Tasks

### Adding New Database Entities
1. Update schema in `internal/db/db.go`
2. Add migration logic if needed
3. Update `docs/DATABASE_SCHEMA.md`
4. Add corresponding tests in `internal/db/db_test.go`

### Adding New Configuration
1. Add field to `Config` struct in `internal/config/config.go`
2. Set default value in `loadDefaults()` function
3. Document in `readme.md` environment variables section
4. Add validation if required

### Extending Transcription Pipeline
1. New components go in `internal/` with their own package
2. Integrate via `internal/worker/worker.go` processing flow
3. Update `docs/ARCHITECTURE_OVERVIEW.md` with new component
4. Add comprehensive tests following existing patterns