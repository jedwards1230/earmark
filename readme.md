# Audiobook Transcription Service

This is a personal service to automatically transcribe audiobooks using Yap, correct transcription errors with LLM-powered text correction, and provide semantic search capabilities through OpenAI embeddings.

## Overview

The service monitors a specified directory for new audio files. When a new audio file is detected, it is added to a queue for transcription. A worker process takes audio files from the queue, transcribes them using Yap (Apple's native speech recognition), corrects transcription errors using LLM-powered text correction (not yet implemented), processes the corrected transcriptions into chunks with OpenAI embeddings, and stores everything in a PostgreSQL database with pgvector for semantic search.

The service uses a PostgreSQL database to track processed files and avoid redundant transcriptions.

## Components

*   **Directory Monitoring:** Uses `fsnotify` to watch for new audio files in the input directory.
*   **Queue Management:** Uses a simple in-memory channel to queue audio files for processing.
*   **Database:** PostgreSQL with pgvector extension for storing transcriptions, embeddings, and metadata.
*   **Transcription:** Uses Yap (Apple's native speech recognition) for fast, accurate local transcription.
*   **LLM Text Correction:** Three-stage correction pipeline using OpenAI-compatible APIs to fix transcription errors (not yet implemented).
*   **Semantic Search:** Chunks corrected transcriptions and creates OpenAI embeddings for vector similarity search.
*   **Full-Text Search:** PostgreSQL full-text search capabilities across all content.

## Dependencies

*   **Go:** The service is written in Go.
*   **PostgreSQL:** Database with pgvector extension for vector operations.
*   **Docker Compose:** For running the PostgreSQL database.
*   **Yap:** Apple's native speech recognition for audio transcription.
*   **OpenAI API:** For generating embeddings and LLM text correction (configurable endpoint).
*   **macOS 26+:** Required for Yap speech recognition capabilities.

## Configuration

The service is configured using environment variables:

```bash
# Core directories
AUDIO_DIR=/audiobooks
CACHE_DIR=/cache
OUTPUT_DIR=/transcriptions

# Database connection
DB_HOST=db
DB_USER=postgres
DB_PASSWORD=password
DB_NAME=transcriber

# OpenAI integration
OPENAI_API_KEY=your-api-key-here
OPENAI_BASE_URL=https://api.openai.com/v1
CHUNK_SIZE=1024

# LLM Correction settings
LLM_CORRECTION_ENABLED=true
LLM_CORRECTION_MODEL=gpt-4o-mini
LLM_CORRECTION_BASE_URL=https://api.openai.com/v1
LLM_CORRECTION_API_KEY=your-api-key-here
LLM_CORRECTION_TEMPERATURE=0.1
LLM_CORRECTION_MAX_RETRIES=3

# Debug settings
DEBUG=false
RESET_STATE=false
```

Key configuration options:
*   `AUDIO_DIR`: Directory to monitor for new audio files
*   `DB_*`: PostgreSQL database connection settings
*   `CHUNK_SIZE`: Size of text chunks for embedding generation
*   `OPENAI_API_KEY`: API key for generating embeddings
*   `LLM_CORRECTION_*`: Settings for the three-stage text correction pipeline (not yet implemented)
*   `LLM_CORRECTION_ENABLED`: Toggle for enabling/disabling LLM correction
*   `LLM_CORRECTION_MODEL`: LLM model to use for text correction

## Usage

1.  **Setup environment variables:**
    ```bash
    cp .env.example .env
    # Edit .env with your database credentials, OpenAI API key, and LLM correction settings
    ```

2.  **Start the database:**
    ```bash
    docker compose up -d
    ```

3.  **Build and run the application:**
    ```bash
    go build -o lil-whisper
    ./lil-whisper start    # Start monitoring service and HTTP server
    ```

4.  **Search transcriptions:**
    ```bash
    # Semantic search
    ./lil-whisper search "your query here"
    
    # List processed books
    ./lil-whisper list
    ```

## Database Schema

The service uses a sophisticated PostgreSQL schema:
- **authors** → **books** → **chapters** → **vectors** (chunked content with embeddings)
- **transcriptions** table for raw transcription storage with deduplication
- Full-text search and vector similarity search capabilities
- Automatic deduplication based on file content and processing settings

## Recent Improvements (v0.9)

- ✅ **Raw Transcription Storage:** Store full transcriptions alongside chunked content
- ✅ **Settings-Based Deduplication:** Re-transcribe only when settings change
- ✅ **File Checksum Validation:** Use SHA256 checksums for reliable deduplication
- ✅ **Enhanced Database Schema:** Added transcriptions table with optimized indexes

## Current Limitations

- Requires macOS 26+ for Yap speech recognition
- Requires OpenAI API key for embeddings generation and LLM text correction (when implemented)
- Hybrid architecture: local transcription + cloud correction (planned) + cloud embeddings

## Roadmap to v1.0.0

See [PROJECT-PLAN.md](PROJECT-PLAN.md) for detailed roadmap. Key improvements planned:

- **LLM Text Correction Implementation:** Three-stage correction pipeline using OpenAI-compatible APIs
- **MCP Server Implementation:** 🚧 **MAJOR TODO** - Model Context Protocol server for LLM integration and tool access
- **Command Structure Refactor:** Split `start` command into separate monitor/transcribe and serve commands
- **Hybrid Architecture Enhancements:** Optimize the Yap + LLM Correction + OpenAI integration
- **Enhanced Error Handling:** Robust retry logic and better error reporting

## Notes

*   The service uses a buffered channel for the work queue (max 100 items)
*   Automatic audio preprocessing with ffmpeg for optimal transcription quality  
*   All transcriptions are corrected using LLM APIs (when implemented) and stored both as raw text and chunked with embeddings for search
*   Intelligent deduplication avoids re-processing unchanged files
*   PostgreSQL full-text search provides additional query options