# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is an audiobook transcription service that uses Yap for local transcription and OpenAI for embeddings to provide semantic search capabilities. The service monitors directories for new audio files, transcribes them locally using Apple's speech recognition, chunks the content, generates embeddings via OpenAI API, and stores everything in PostgreSQL with pgvector for search.

## Architecture

The codebase follows a modular Go architecture:

- **cmd/**: CLI commands using Cobra (start, list, search)
- **internal/**: Core business logic modules
  - **config/**: Configuration management with environment variable support
  - **db/**: PostgreSQL database operations with pgvector
  - **monitor/**: File system monitoring using fsnotify
  - **transcribe/**: Audio transcription using Yap
  - **chunker/**: Text chunking for embeddings
  - **openai/**: OpenAI API integration for embeddings
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
./lil-whisper start    # Start the monitoring service
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
- **Vector Search**: Uses pgvector for semantic similarity search
- **Deduplication**: SHA256-based file content deduplication
- **Chunking**: Intelligent text chunking for optimal embedding generation
- **Queue Management**: In-memory buffered channel (max 100 items)
- **Embeddings**: Uses OpenAI API for generating vector embeddings

## Recent Changes

The project uses a hybrid approach combining the best of local and cloud services:
- **Yap for Transcription**: Local Apple speech recognition (macOS 26+) for fast, reliable transcription
- **OpenAI for Embeddings**: Proven OpenAI API for high-quality vector embeddings
- **Whisper Deprecated**: Removed all Whisper dependencies in favor of Yap

This approach provides optimal performance with local transcription speed and cloud embedding quality.

## Major TODOs

- **🚧 MCP Server Implementation**: Model Context Protocol server for LLM integration and tool access - this is a critical component for making the service accessible to AI assistants and workflow automation tools