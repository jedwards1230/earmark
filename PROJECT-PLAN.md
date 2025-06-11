# Project Plan (v1.0.0)

This document breaks down the roadmap into phases and actionable tasks with checkboxes you can mark off as you progress.

---

## Phase 1: Core Refactoring - Database and Whisper Sidecar

- [x] **Database Integration**
  - [x] Design `transcriptions` table schema for raw transcription storage
  - [x] Implement settings hash computation for deduplication
  - [x] Add database methods for transcription management:
    - [x] Check if file needs transcription (checksum + settings hash)
    - [x] Store raw transcription text and metadata
    - [x] Update transcription status tracking
  - [x] Update `IsProcessed()` logic to use new transcriptions table
  - [x] Preserve existing chunking/embedding workflow for semantic search

- [ ] **Whisper Sidecar Service**
  - [ ] Create new Dockerfile for dedicated Whisper container
  - [ ] Design REST API specification:
    - [ ] `POST /transcribe` - accept audio file and return transcription
    - [ ] `GET /health` - health check endpoint
    - [ ] `GET /models` - list available models
  - [ ] Implement FastAPI server with whisper-ctranslate2 integration
  - [ ] Add proper error handling and logging to sidecar
  - [ ] Write integration tests for sidecar API
  - [ ] Update Go `TranscribeAudio` to call HTTP API instead of exec

- [ ] **Docker Compose Enhancement**
  - [ ] Add Whisper sidecar service to existing compose.yaml
  - [ ] Configure service networking and volume mounts
  - [ ] Update environment variables and configuration
  - [ ] Ensure PostgreSQL service remains unchanged

---

## Phase 2: MCP Server Implementation

- [ ] **MCP Data Models & Server Design**
  - [ ] Design MCP data models for dual access:
    - [ ] Raw transcription access (full text retrieval)
    - [ ] Semantic search access (chunked content with embeddings)
  - [ ] Define MCP server capabilities and resources
  - [ ] Plan MCP endpoint structure for LLM integration

- [ ] **MCP Server Implementation** 
  - [ ] Implement MCP server handlers in Go
  - [ ] Add transcription text retrieval endpoints
  - [ ] Integrate existing semantic search functionality
  - [ ] Add book/chapter/author metadata endpoints
  - [ ] Implement content filtering and pagination

- [ ] **Configuration & Integration**
  - [ ] Add MCP configuration options to config.json
  - [ ] Update Docker Compose to expose MCP server port
  - [ ] Add MCP server documentation and usage examples

---

## Phase 3: Enhancements, Polish, and Testing

- [ ] **Persistent Job Queue**
  - [ ] Evaluate and choose between Redis vs. DB-backed queue
  - [ ] Implement persistent queue logic

- [ ] **Configuration Management**
  - [ ] Integrate a config library (e.g., Viper) for env vars and file-based configs

- [ ] **Error Handling & Retries**
  - [ ] Add robust error handling around DB operations and HTTP calls
  - [ ] Implement retry logic for transient failures

- [x] **Advanced Deduplication**
  - [x] Implement file checksum computation (SHA256)
  - [x] Create transcription settings hash function
  - [x] Update deduplication logic to re-transcribe only when:
    - [x] File content changes (different checksum)
    - [x] Transcription settings change (different settings hash)
  - [x] Add logic to preserve existing chunked/embedded content when possible

- [ ] **Testing**
  - [ ] Unit tests for:
    - [ ] Transcriptions table database operations
    - [ ] Settings hash computation and comparison
    - [ ] Whisper sidecar HTTP client
    - [ ] MCP server handlers and data models
  - [ ] Integration tests for:
    - [ ] End-to-end transcription workflow with sidecar
    - [ ] Deduplication logic with file and settings changes
    - [ ] MCP server data access (raw + semantic search)
    - [ ] Docker Compose service orchestration

- [ ] **Logging Enhancements**
  - [ ] Standardize log formats across services
  - [ ] Add correlation IDs to trace jobs end-to-end

---

## Phase 4: Documentation & Release Prep

- [ ] Update `readme.md` with:
  - [ ] Updated architecture overview (sidecar + dual storage)
  - [ ] Docker Compose setup instructions 
  - [ ] Environment variable configuration guide
  - [ ] MCP server usage examples and API documentation
  - [ ] Whisper sidecar API documentation
  - [ ] Database schema documentation updates
- [ ] Perform code cleanup and refactoring for clarity
- [ ] Choose and add an open-source LICENSE file
- [ ] Implement semantic versioning (Git tags) and create `v1.0.0` tag
- [ ] Draft release notes for the `1.0.0` release
- [ ] Create `CONTRIBUTING.md` with guidelines for future contributors

---

*Ready to start checking off tasks!*
