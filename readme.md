# Audiobook Transcription Service

This is a personal service to automatically transcribe audiobooks using Yap and provide semantic search capabilities through OpenAI embeddings.

## Overview

The service monitors a specified directory for new audio files. When a new audio file is detected, it is added to a queue for transcription. A worker process takes audio files from the queue, transcribes them using Yap (Apple's native speech recognition), processes the transcriptions into chunks with OpenAI embeddings, and stores everything in a PostgreSQL database with pgvector for semantic search.

The service uses a PostgreSQL database to track processed files and avoid redundant transcriptions.

## Components

*   **Directory Monitoring:** Uses `fsnotify` to watch for new audio files in the input directory.
*   **Queue Management:** Uses a simple in-memory channel to queue audio files for processing.
*   **Database:** PostgreSQL with pgvector extension for storing transcriptions, embeddings, and metadata.
*   **Transcription:** Uses Yap (Apple's native speech recognition) for fast, accurate local transcription.
*   **Semantic Search:** Chunks transcriptions and creates OpenAI embeddings for vector similarity search.
*   **Full-Text Search:** PostgreSQL full-text search capabilities across all content.

## Dependencies

*   **Go:** The service is written in Go.
*   **PostgreSQL:** Database with pgvector extension for vector operations.
*   **Docker:** For containerization and orchestration.
*   **Yap:** Apple's native speech recognition for audio transcription.
*   **OpenAI API:** For generating embeddings (configurable endpoint).
*   **macOS 26+:** Required for Yap speech recognition capabilities.

## Configuration

The service is configured using a `config.json` file and environment variables:

```json
{
  "audio_dir": "/audiobooks",
  "cache_dir": "/cache", 
  "output_dir": "/transcriptions",
  "db_host": "db",
  "db_user": "postgres",
  "db_password": "password",
  "db_name": "transcriber",
  "chunk_size": 1024,
  "openai_api_key": "your-api-key-here",
  "debug": false
}
```

Key configuration options:
*   `audio_dir`: Directory to monitor for new audio files
*   `db_*`: PostgreSQL database connection settings
*   `chunk_size`: Size of text chunks for embedding generation
*   `openai_api_key`: API key for generating embeddings

## Usage

1.  **Setup environment variables:**
    ```bash
    cp .env.example .env
    # Edit .env with your database credentials and OpenAI API key
    ```

2.  **Start the services:**
    ```bash
    docker compose up -d
    ```

3.  **Place audiobooks in the monitored directory:**
    - The service will automatically detect and process new audio files
    - Transcriptions are stored in the database with embeddings for search
    - Progress is logged to the container output

4.  **Search transcriptions:**
    ```bash
    # Semantic search
    docker compose exec proc ./lil-whisper search "your query here"
    
    # List processed books
    docker compose exec proc ./lil-whisper list
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
- Requires OpenAI API key for embeddings generation
- Hybrid architecture: local transcription + cloud embeddings

## Roadmap to v1.0.0

See [PROJECT-PLAN.md](PROJECT-PLAN.md) for detailed roadmap. Key improvements planned:

- **Hybrid Architecture Enhancements:** Optimize the Yap + OpenAI integration
- **MCP Server:** Model Context Protocol server for LLM integration
- **Enhanced Error Handling:** Robust retry logic and better error reporting

## Notes

*   The service uses a buffered channel for the work queue (max 100 items)
*   Automatic audio preprocessing with ffmpeg for optimal transcription quality  
*   All transcriptions are stored both as raw text and chunked with embeddings for search
*   Intelligent deduplication avoids re-processing unchanged files
*   PostgreSQL full-text search provides additional query options