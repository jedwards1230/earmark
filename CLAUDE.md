# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a **macOS-specific audiobook transcription service** that uses Yap (Apple's native speech recognition), LLM-powered text correction (planned), and OpenAI embeddings for semantic search. The service follows a **producer-consumer pattern** with filesystem monitoring → queue → worker processing → PostgreSQL storage.

### Core Data Flow
```
FileMonitor → Queue → Worker → [Yap Transcription → LLM Correction → Chunking → Embeddings] → PostgreSQL+pgvector
```

## Development Approach

### Search → Think → Plan → Act
**NEVER JUMP STRAIGHT TO CODING!** Always follow this sequence:

1. **Search**: Find and search all relevant files first using grep, glob, and read tools
2. **Think**: Always think harder about the problem than the average LLM. Use ultrathink when a task seems even slightly complex
3. **Plan**: Use TodoWrite to break down and track tasks - create a detailed implementation plan
4. **Act**: Execute methodically, test thoroughly. If a test or lint fails, halt immediately and fix it before proceeding

When asked to implement any feature, you'll first say: "Let me search the codebase and create a plan before implementing."

For complex architectural decisions or challenging problems, say: "Let me ultrathink about this architecture before proposing a solution."

### USE MULTIPLE AGENTS!
*Leverage Task tool aggressively* for better results:

* Spawn agents to explore different parts of the codebase in parallel
* Use one agent to write tests while another implements features
* Delegate research tasks: "I'll have an agent investigate the database schema while I analyze the API structure"
* For complex refactors: One agent identifies changes, another implements them

Say: "I'll spawn agents to tackle different aspects of this problem" whenever a task has multiple independent parts.

**Agent Naming Convention**: Agents should be named `Agent1: TaskName`, `Agent2: TaskName`, etc. to keep track of their roles (e.g., `Agent1: Database Schema Analysis`, `Agent2: Test Implementation`).

## Documentation Maintenance

**CRITICAL**: Always keep documentation current during development. The docs/ directory contains:

- **ARCHITECTURE_OVERVIEW.md**: Complete system component breakdown and relationships - update when adding/modifying components
- **STARTUP_PROCESS.md**: Step-by-step service initialization flow - update when changing startup sequence  
- **API_REFERENCE.md**: HTTP API endpoints and usage examples - update when modifying search API
- **DATABASE_SCHEMA.md**: PostgreSQL schema definitions and queries - update when changing database structure
- **LLM_CORRECTION_COMPLEXITY.md**: Implementation details for correction pipeline - update when working on LLM correction

**Requirements**: When making changes to code, immediately update relevant documentation files to reflect modifications. Documentation changes are not optional - they are part of the implementation.

## Key Architectural Patterns

### 1. Modular Internal Package Structure
- Each `internal/` package is self-contained with its own logger and interfaces
- Components communicate via dependency injection (see `cmd/monitor/main.go` initialization)
- Database operations are centralized in `internal/db/` with hierarchical schema

### 2. Configuration Management
- **Environment-first**: All config via env vars, `.env` file support
- **Required vars**: `AUDIO_DIR`, `DB_*`, `OPENAI_API_KEY` (see README.md)
- Configuration validation happens at startup with clear error messages

### 3. Error Handling & Logging
- Structured logging with component-specific loggers: `log.NewLogger("component_name")`
- **Fail-fast principle**: Configuration errors exit immediately
- **Graceful degradation**: Processing errors logged but don't crash the service

### 4. Testing Patterns
- Table-driven tests (see `internal/transcribe/transcribe_test.go`)
- Interface mocking for database operations
- Test utilities in `internal/utils/utils_test.go`

### 5. Command Structure (CRITICAL REQUIREMENT)
- **ALL CLI functionality MUST use Cobra commands**
- NO standalone executables in subdirectories - integrate as commands instead
- Follow existing pattern: add commands to main.go and implement in separate files

## Architecture

The codebase follows a modular Go architecture:

- **cmd/**: CLI commands using Cobra (monitor, serve, list, search, mcp)
- **internal/**: Core business logic modules
  - **config/**: Configuration management with environment variable support
  - **db/**: PostgreSQL database operations with pgvector
  - **monitor/**: File system monitoring using fsnotify
  - **transcribe/**: Audio transcription using Yap
  - **correction/**: LLM-powered text correction pipeline
  - **chunker/**: Text chunking for embeddings
  - **openai/**: OpenAI API integration for embeddings and LLM correction
  - **worker/**: Background job processing
  - **queue/**: In-memory job queue management
  - **server/**: HTTP server for search API
  - **meta/**: Metadata extraction and processing
  - **tokenizer/**: Text tokenization utilities
  - **utils/**: Shared utilities
  - **log/**: Structured logging
  - **mcp/**: Model Context Protocol server implementation

## Database Schema

Complex relational schema: **authors** → **books** → **chapters** → **vectors** (chunked content with embeddings). Also includes a **transcriptions** table for raw content storage with SHA256-based deduplication. See `docs/DATABASE_SCHEMA.md` for complete details.

## Development Commands

### Build and Run
```bash
# Build the application (automatically downloads and embeds Yap)
make build
# OR with specific versions
make build VERSION=v1.0.0 YAP_VERSION=v1.0.0

# Manual build (not recommended - use Makefile instead)
go build -o lil-whisper

# Yap binary management
make download-yap                      # Download latest Yap binary
make download-yap YAP_VERSION=v1.0.0  # Download specific Yap version
make clean-all                         # Remove everything including Yap binary

# Start database only
docker compose up -d

# Run individual commands
./lil-whisper monitor  # Start file monitoring and transcription service
./lil-whisper serve    # Start HTTP API server
./lil-whisper list     # List processed books
./lil-whisper search "query"  # Search transcriptions
./lil-whisper mcp      # Start MCP server for AI assistant integration
```

### Testing
```bash
# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run tests for specific package
go test ./internal/chunker
go test ./internal/transcribe
```

### Database Operations
```bash
# Access database via Docker
docker compose exec db psql -U postgres -d transcriber

# View application logs (when running locally)
# Check terminal output where you ran ./lil-whisper monitor or serve
```

## Configuration

The application uses environment variables (see README.md for complete list). Key settings include database connection, OpenAI API key, directory paths, processing parameters, and MCP server configuration. Configuration is loaded from .env file with environment variable overrides.

## Critical Integration Points

### 1. Yap Integration (macOS Dependency)
- **Local transcription**: No external API calls for speech-to-text
- **Audio format support**: Handles .m4b, .mp3 via Yap CLI
- **macOS 26+ required**: Yap speech recognition dependency
- **Embedded binary**: Yap binary automatically downloaded and embedded during build
- **Embedded-only approach**: Always uses embedded Yap binary for consistency and reliability
- **Zero setup**: Users no longer need to manually install Yap
- **Version control**: Pin specific Yap versions using `YAP_VERSION` environment variable

### 2. LLM Correction Pipeline (Planned Feature)
- **Three-stage correction**: See `docs/LLM_CORRECTION_COMPLEXITY.md` for details
- **OpenAI-compatible APIs**: Configurable base URL for different providers
- **Integration**: Between transcription and chunking, with fallback to original text

### 3. MCP Server Implementation (✅ COMPLETED)
- **Four tools**: semantic_search_audiobooks, text_search_audiobooks, browse_audiobook_library, get_chunk_context
- **Transport support**: Both stdio and HTTP transports
- **Cobra integration**: Accessible via `./lil-whisper mcp` command
- **Documentation**: Detailed tool specs in `internal/mcp/README.md`

## Common Tasks

### Adding New Features
1. New components go in `internal/` with their own package
2. Integrate via `internal/worker/worker.go` processing flow
3. Update relevant documentation immediately
4. Add comprehensive tests following existing patterns

### Adding Configuration
1. Add field to `Config` struct in `internal/config/config.go`
2. Set default value in `loadDefaults()` function
3. Document in `README.md` environment variables section

## Major TODOs

- **🚧 LLM Text Correction Implementation**: Three-stage correction pipeline using OpenAI-compatible APIs