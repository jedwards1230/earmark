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
*   **Yap:** Apple's native speech recognition for audio transcription (automatically embedded during build).
*   **ffmpeg:** Required for audio preprocessing and format conversion (must be installed separately).
*   **OpenAI API:** For generating embeddings and LLM text correction (configurable endpoint).
*   **macOS 26+:** Required for Yap speech recognition capabilities.

### Yap Binary Embedding

The application automatically downloads and embeds the Yap binary during build time, eliminating the need for manual installation:

*   **Embedded Only:** Always uses embedded Yap binary for consistency
*   **Version Control:** Pin specific Yap versions using `YAP_VERSION` environment variable
*   **Zero Setup:** Users never need to manually install Yap
*   **Portable:** Single binary contains everything needed for transcription

## Version Management

The service includes built-in version checking and update capabilities:

### Version Information
```bash
# Show current version, commit, build time
./lil-whisper version

# Check for available updates
./lil-whisper version --check

# Force fresh update check (skip cache)
./lil-whisper version --check --no-cache
```

### Automatic Updates
```bash
# Check and update if newer version available
./lil-whisper update

# Only check for updates without updating
./lil-whisper update --check

# Force update without confirmation
./lil-whisper update --force --yes

# Enable debug output to see authentication details
./lil-whisper update --debug
```

**For Private Repositories:** The updater will automatically try multiple authentication methods in this order:
1. Environment variables (GITHUB_TOKEN, GITHUB_PAT, GH_TOKEN, PAT)
2. GitHub CLI credentials (if `gh` is installed and authenticated)
3. SSH keys (for git operations only)

No additional configuration is required - the updater will use the first available authentication method.

### Automatic Version Checking
The CLI automatically checks for updates in the background when commands are run. This is non-intrusive and can be configured:

```bash
# Disable automatic version checking
export DISABLE_VERSION_CHECK=true

# Configure check interval (default: 24h)
export VERSION_CHECK_INTERVAL=12h

# Configure check timeout (default: 5s)
export VERSION_CHECK_TIMEOUT=3s
```

### Build Process

The service is a single Go binary with no native dependencies — it builds and
runs anywhere Go does. Use the Makefile for version embedding:

```bash
make build                  # Build ./lil-whisper
make release VERSION=v1.0.0 # Release build with an explicit version
make docker                 # Build the container image
make check                  # fmt + vet + lint + test
```

**Status dashboard (no database required):**

```bash
make dashboard              # serves http://localhost:8081/ with synthetic data
# equivalently: go run . mcp --demo
```

This is the accessible dev server for UI work and AI-agent visual verification
(Playwright is wired via `.claude/mcp.json`). See `CLAUDE.md` →
"Visual Verification".

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
DEBUG_DB_RESET=false  # WARNING: Destructive! Deletes ALL database tables and transcription files

# Logging settings
LOG_DEBUG=false
LOG_VERBOSE=false

# Version checking settings
DISABLE_VERSION_CHECK=false
VERSION_CHECK_INTERVAL=24h
VERSION_CHECK_TIMEOUT=5s

# GitHub authentication (required for private repositories)
# The service supports multiple authentication methods in priority order:
# 1. Environment variables (GITHUB_TOKEN, GITHUB_PAT, GH_TOKEN, PAT)
# 2. GitHub CLI authentication (gh auth login)
# 3. SSH keys (for git operations only)
GITHUB_TOKEN=your-github-token-here

# Yap binary embedding (build-time only)
YAP_VERSION=latest  # Pin specific yap version for embedded binary
```

Key configuration options:
*   `AUDIO_DIR`: Directory to monitor for new audio files
*   `DB_*`: PostgreSQL database connection settings
*   `CHUNK_SIZE`: Size of text chunks for embedding generation
*   `OPENAI_API_KEY`: API key for generating embeddings
*   `LLM_CORRECTION_*`: Settings for the three-stage text correction pipeline (not yet implemented)
*   `LLM_CORRECTION_ENABLED`: Toggle for enabling/disabling LLM correction
*   `LLM_CORRECTION_MODEL`: LLM model to use for text correction
*   `LOG_DEBUG`: Enable debug-level logging (set to `true` or `1`)
*   `LOG_VERBOSE`: Enable verbose logging including raw transcription text (set to `true` or `1`)
*   `DISABLE_VERSION_CHECK`: Disable automatic version checking (default: false)
*   `VERSION_CHECK_INTERVAL`: How often to check for updates (default: 24h)
*   `VERSION_CHECK_TIMEOUT`: Timeout for version check requests (default: 5s)
*   `GITHUB_TOKEN`: GitHub personal access token for private repository access (required for update/version checking of private repositories)
*   **Authentication Priority:** The service tries authentication methods in this order:
    1. **Environment Variables**: `GITHUB_TOKEN`, `GITHUB_PAT`, `GH_TOKEN`, `PAT`
    2. **GitHub CLI**: Uses `gh auth token` if GitHub CLI is installed and authenticated
    3. **SSH Keys**: Available for git operations (not HTTP downloads)
*   `YAP_VERSION`: Pin specific Yap version for embedded binary (build-time only, default: `latest`)

## Installation

### Option 1: Go Install (Recommended)
For users with Go installed:
```bash
# Install latest version
go install github.com/jedwards1230/lil-whisper@latest

# Install specific version  
go install github.com/jedwards1230/lil-whisper@v1.0.0

# The binary will be installed to $GOPATH/bin or $HOME/go/bin
```

### Option 2: Direct Download
For users without Go:
```bash
# Download for Apple Silicon (M1/M2)
curl -L https://github.com/jedwards1230/lil-whisper/releases/latest/download/lil-whisper-darwin-arm64 -o lil-whisper

# Download for Intel Macs
curl -L https://github.com/jedwards1230/lil-whisper/releases/latest/download/lil-whisper-darwin-amd64 -o lil-whisper

# Make executable and install
chmod +x lil-whisper
sudo mv lil-whisper /usr/local/bin/
```

### Option 3: Built-in Updater
Once installed, you can update using the CLI itself:
```bash
# Check for updates
lil-whisper version --check

# Update to latest version
lil-whisper update
```

#### Authentication for Private Repositories
For private repositories, the updater supports multiple authentication methods:

1. **Environment Variables** (highest priority):
   ```bash
   export GITHUB_TOKEN=your_personal_access_token
   # or
   export GITHUB_PAT=your_personal_access_token
   ```

2. **GitHub CLI** (automatic if authenticated):
   ```bash
   gh auth login
   # The updater will automatically use GitHub CLI credentials
   ```

3. **SSH Keys** (for git operations only):
   ```bash
   # SSH keys are used for git-based operations
   # Not supported for release downloads
   ```

The updater tries these methods in order and uses the first successful authentication method.

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
    
    # Start MCP server for AI assistant integration
    ./lil-whisper mcp
    
    # Check version and updates
    ./lil-whisper version
    ./lil-whisper version --check
    
    # Update to latest version
    ./lil-whisper update
    ```

## MCP Integration (Model Context Protocol)

The service now includes a complete MCP server implementation for integration with AI assistants like Claude Desktop:

### Available Tools

- **semantic_search_audiobooks**: Search using semantic similarity
- **text_search_audiobooks**: Full-text search across transcriptions  
- **browse_audiobook_library**: Browse library structure
- **get_chunk_context**: Get surrounding chunks for context

*See `internal/mcp/README.md` for detailed tool documentation and examples.*

### Usage

```bash
# Start MCP server with stdio transport (default)
./lil-whisper mcp

# Start with HTTP transport
MCP_TRANSPORT=http ./lil-whisper mcp

# Start with custom HTTP address
MCP_TRANSPORT=http MCP_HTTP_ADDR=:9000 ./lil-whisper mcp
```

### Claude Desktop Integration

Add to your Claude Desktop MCP settings:

```json
{
  "mcpServers": {
    "lilbro-whisper": {
      "command": "/path/to/lil-whisper",
      "args": ["mcp"]
    }
  }
}
```

## Database Schema

The service uses a sophisticated PostgreSQL schema:
- **authors** → **books** → **chapters** → **vectors** (chunked content with embeddings)
- **transcriptions** table for raw transcription storage with deduplication
- Full-text search and vector similarity search capabilities
- Automatic deduplication based on file content and processing settings

## Recent Improvements (v0.11)

- ✅ **Yap Binary Embedding:** Automatic download and embedding of Yap binary during build
- ✅ **Embedded-Only Approach:** Always uses embedded Yap binary for consistency and reliability
- ✅ **Zero Setup Required:** Users no longer need to manually install Yap
- ✅ **Version-Pinned Builds:** Control embedded Yap version with `YAP_VERSION` environment variable
- ✅ **Enhanced Build Process:** Makefile and scripts automatically manage Yap binary
- ✅ **Portable Distribution:** Single binary contains everything needed for transcription
- ✅ **Version Management System:** Built-in version checking and automatic updates
- ✅ **GitHub Release Integration:** Support for both commit-based and release-based updates
- ✅ **Automated Build Process:** Makefile and scripts with version embedding
- ✅ **CI/CD Workflows:** GitHub Actions for automated testing and releases
- ✅ **Background Update Checking:** Non-intrusive automatic version checking
- ✅ **Configurable Update Behavior:** Environment-based configuration for update checking
- ✅ **MCP Server Implementation:** Complete Model Context Protocol server with 4 tools for AI assistant integration
- ✅ **Raw Transcription Storage:** Store full transcriptions alongside chunked content
- ✅ **Settings-Based Deduplication:** Re-transcribe only when settings change
- ✅ **File Checksum Validation:** Use SHA256 checksums for reliable deduplication
- ✅ **Enhanced Database Schema:** Added transcriptions table with optimized indexes
- ✅ **Cobra Command Structure:** All CLI functionality properly integrated as commands

## Current Limitations

- Requires macOS 26+ for Yap speech recognition (binary is automatically embedded)
- Requires OpenAI API key for embeddings generation and LLM text correction (when implemented)
- Requires internet connection for first build to download Yap binary
- Hybrid architecture: local transcription + cloud correction (planned) + cloud embeddings

## Roadmap to v1.0.0

See [PROJECT-PLAN.md](PROJECT-PLAN.md) for detailed roadmap. Key improvements planned:

- **LLM Text Correction Implementation:** Three-stage correction pipeline using OpenAI-compatible APIs
- **Command Structure Refactor:** Split `start` command into separate monitor/transcribe and serve commands  
- **Hybrid Architecture Enhancements:** Optimize the Yap + LLM Correction + OpenAI integration
- **Enhanced Error Handling:** Robust retry logic and better error reporting

## Notes

*   The service uses a buffered channel for the work queue (max 100 items)
*   Automatic audio preprocessing with ffmpeg for optimal transcription quality  
*   All transcriptions are corrected using LLM APIs (when implemented) and stored both as raw text and chunked with embeddings for search
*   Intelligent deduplication avoids re-processing unchanged files
*   PostgreSQL full-text search provides additional query options