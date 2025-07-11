# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is an audiobook transcription service that uses Yap for local transcription, LLM-powered text correction (planned), and OpenAI for embeddings to provide semantic search capabilities. The service monitors directories for new audio files, transcribes them locally using Apple's speech recognition, corrects transcription errors using LLM APIs (when implemented), chunks the corrected content, generates embeddings via OpenAI API, and stores everything in PostgreSQL with pgvector for search.

## Development Approach

Always search → think → plan → act:

1. **Search**: Find and search all relevant files first using grep, glob, and read tools
2. **Think**: Always think harder about the problem than the average LLM. Use ultrathink when a task seems even slightly complex.
3. **Plan**: Use TodoWrite to break down and track tasks  
4. **Act**: Execute methodically, test thoroughly. If a test or lint fails, halt immediately and fix it before proceeding.

## Documentation Maintenance

**CRITICAL**: Always keep documentation current during development. The docs/ directory contains:

- **ARCHITECTURE_OVERVIEW.md**: Complete system component breakdown and relationships - update when adding/modifying components
- **STARTUP_PROCESS.md**: Step-by-step service initialization flow - update when changing startup sequence  
- **API_REFERENCE.md**: HTTP API endpoints and usage examples - update when modifying search API
- **DATABASE_SCHEMA.md**: PostgreSQL schema definitions and queries - update when changing database structure

**Requirements**: When making changes to code, immediately update relevant documentation files to reflect modifications. Documentation changes are not optional - they are part of the implementation.

## Architecture

The codebase follows a modular Go architecture:

- **cmd/**: CLI commands using Cobra (start, list, search)
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

## Database Schema

Complex relational schema: **authors** → **books** → **chapters** → **vectors** (chunked content with embeddings). Also includes a **transcriptions** table for raw content storage with SHA256-based deduplication.

## Development Commands

### Build and Run
```bash
# Build the application
go build -o lil-whisper

# Start database only
docker compose up -d

# Run individual commands
./lil-whisper start    # Start the monitoring service and HTTP server
./lil-whisper list     # List processed books
./lil-whisper search "query"  # Search transcriptions
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
# Check terminal output where you ran ./lil-whisper start
```

## Configuration

The application uses environment variables (see README.md for complete list and examples). Key settings include database connection, OpenAI API key, directory paths, and processing parameters. Configuration is loaded from .env file with environment variable overrides.

## Key Components

- **File Monitoring**: Uses fsnotify to watch for new audio files
- **Transcription**: Integrates with Yap for audio-to-text conversion
- **LLM Correction**: Three-stage text correction using OpenAI-compatible APIs (not yet implemented)
- **Vector Search**: Uses pgvector for semantic similarity search
- **Deduplication**: SHA256-based file content deduplication
- **Chunking**: Intelligent text chunking for optimal embedding generation
- **Queue Management**: In-memory buffered channel (max 100 items)
- **Embeddings**: Uses OpenAI API for generating vector embeddings

## Recent Changes

The project uses a hybrid approach combining the best of local and cloud services:
- **Yap for Transcription**: Local Apple speech recognition (macOS 26+) for fast, reliable transcription
- **LLM Correction Pipeline**: Three-stage text correction using OpenAI-compatible APIs to fix transcription errors
- **OpenAI for Embeddings**: Proven OpenAI API for high-quality vector embeddings
- **Whisper Deprecated**: Removed all Whisper dependencies in favor of Yap

This approach provides optimal performance with local transcription speed, intelligent error correction (when implemented), and cloud embedding quality.

## LLM Text Correction Pipeline (Planned Feature)

Three-stage correction process to improve transcription accuracy:

1. **Spelling & Grammar**: Fix transcription errors using book metadata context
2. **Formatting**: Standardize punctuation and paragraph structure  
3. **Verification**: Validate meaning preservation and accuracy

**Integration**: Correction will occur between transcription and chunking, with fallback to original text on errors.

## Major TODOs

- **🚧 LLM Text Correction Implementation**: Three-stage correction pipeline using OpenAI-compatible APIs
- **🚧 MCP Server Implementation**: Model Context Protocol server for LLM integration and tool access - this is a critical component for making the service accessible to AI assistants and workflow automation tools
- **🚧 Command Structure Refactor**: Split `start` command into separate monitor/transcribe and serve (HTTP/MCP) commands

## MCP Server Planning

**Library**: `github.com/mark3labs/mcp-go` - exposes service to LLM applications and AI assistants

**Architecture**: HTTP and MCP servers run as parallel services accessing the same database layer

**Planned Tools**: search, list_books, get_book_info, get_chapter, transcription_status  
**Planned Resources**: book://{id}, search://{query}, stats://library  
**Implementation**: Reuse existing database layer, support stdio and HTTP transports