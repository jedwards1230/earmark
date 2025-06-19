# API Documentation

This document describes the HTTP API endpoints available in the Audiobook Transcription Service.

## Base URL

The service runs on port 8080 by default:
- **Local Development**: `http://localhost:8080`
- **Docker Compose**: `http://localhost:${HOST_PORT:-8080}`

## Authentication

Currently, no authentication is required for API endpoints.

---

## Endpoints

### Search Transcriptions

**Endpoint**: `GET /search`

**Description**: Performs semantic search across transcribed audiobook content using vector embeddings. This endpoint searches through chunked transcriptions and returns relevant content with metadata including book, author, and chapter information.

**Query Parameters**:
- `q` (string, required): The search query text
- `p` (float, optional): Similarity threshold (0.0-1.0). Default: `0.3`
- `k` (integer, optional): Maximum number of results to return. Default: `10`

**Response Format**:
```json
{
  "query": "search query text",
  "count": 5,
  "results": [
    {
      "id": 123,
      "content": "Chunk of transcribed text content...",
      "author": "Author Name",
      "title": "Book Title",
      "chapter": "Chapter Title",
      "chunkIndex": 42,
      "similarity": 0.85,
      "chapterIndex": 5,
      "chapterTitle": "Chapter Title",
      "totalChunks": 150,
      "totalChapters": 25
    }
  ]
}
```

**Response Fields**:
- `id`: Unique identifier for the content chunk
- `content`: The actual transcribed text content
- `author`: Book author name
- `title`: Book title
- `chapter`: Chapter title
- `chunkIndex`: Index of this chunk within the chapter (0-based)
- `similarity`: Cosine similarity score (0.0-1.0, higher is more relevant)
- `chapterIndex`: Chapter number within the book (1-based)
- `chapterTitle`: Full chapter title
- `totalChunks`: Total number of chunks in this chapter
- `totalChapters`: Total number of chapters in this book

**Status Codes**:
- `200 OK`: Search completed successfully
- `400 Bad Request`: Missing or invalid query parameters
- `500 Internal Server Error`: Database or processing error

**Debug Command**:
```bash
# Basic search
curl "http://localhost:8080/search?q=dragon"

# Search with custom threshold and limit
curl "http://localhost:8080/search?q=magic%20sword&p=0.5&k=5"

# Search for specific concepts
curl "http://localhost:8080/search?q=character%20development&p=0.3&k=20"

# URL-encoded complex query
curl "http://localhost:8080/search?q=battle%20scene%20with%20magic&p=0.4&k=15"
```
