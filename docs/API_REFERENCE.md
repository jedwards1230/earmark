# API Reference

## Overview

The lilbro-whisper service provides both RESTful HTTP API and Model Context Protocol (MCP) server for semantic search across transcribed audiobook content. The API uses vector embeddings to find relevant content chunks based on natural language queries.

## HTTP API

### Base Configuration

#### Server Details
- **Default Port**: 8080
- **Protocol**: HTTP
- **Content Type**: `application/json`
- **Authentication**: None required

#### Service URLs
- **Local Development**: `http://localhost:8080`
- **Docker Compose**: `http://localhost:${HOST_PORT:-8080}`

### API Endpoints

#### Search Transcriptions

**Endpoint**: `GET /search`

**Purpose**: Performs semantic search across transcribed audiobook content using vector similarity matching.

##### Request Parameters

| Parameter | Type    | Required | Default | Description                    |
| --------- | ------- | -------- | ------- | ------------------------------ |
| `q`       | string  | Yes      | -       | Search query text              |
| `p`       | float   | No       | 0.3     | Similarity threshold (0.0-1.0) |
| `k`       | integer | No       | 10      | Maximum number of results      |

##### Parameter Details

**Query (`q`)**:
- Natural language search terms
- Supports complex phrases and concepts
- Automatically generates embeddings for similarity matching
- URL encoding required for special characters

**Threshold (`p`)**:
- Range: 0.0 to 1.0
- Higher values = more restrictive matching
- Lower values = broader result set
- Recommended: 0.2-0.5 for most queries

**Limit (`k`)**:
- Maximum results returned
- Range: 1 to system maximum
- Results ordered by similarity score (descending)

##### Response Format

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

##### Response Fields

| Field     | Type    | Description                      |
| --------- | ------- | -------------------------------- |
| `query`   | string  | Original search query            |
| `count`   | integer | Number of results returned       |
| `results` | array   | Array of matching content chunks |

##### Result Object Fields

| Field           | Type    | Description                                 |
| --------------- | ------- | ------------------------------------------- |
| `id`            | integer | Unique vector chunk identifier              |
| `content`       | string  | Transcribed text content                    |
| `author`        | string  | Book author name                            |
| `title`         | string  | Book title                                  |
| `chapter`       | string  | Chapter title                               |
| `chunkIndex`    | integer | Chunk position within chapter (0-based)     |
| `similarity`    | float   | Cosine similarity score (0.0-1.0)           |
| `chapterIndex`  | integer | Chapter number within book (1-based)        |
| `chapterTitle`  | string  | Full chapter title (duplicate of `chapter`) |
| `totalChunks`   | integer | Total chunks in this chapter                |
| `totalChapters` | integer | Total chapters in this book                 |

##### HTTP Status Codes

| Code | Status                | Description                         |
| ---- | --------------------- | ----------------------------------- |
| 200  | OK                    | Search completed successfully       |
| 400  | Bad Request           | Missing or invalid query parameters |
| 500  | Internal Server Error | Database or processing error        |

##### Error Response Format

```json
{
  "error": "error message",
  "details": "additional error context"
}
```

## Usage Examples

### Basic Search
```bash
curl "http://localhost:8080/search?q=dragon"
```

### Advanced Search with Parameters
```bash
curl "http://localhost:8080/search?q=magic%20sword&p=0.5&k=5"
```

### Complex Query
```bash
curl "http://localhost:8080/search?q=character%20development&p=0.3&k=20"
```

### Programmatic Usage (JavaScript)
```javascript
async function searchContent(query, threshold = 0.3, limit = 10) {
  const params = new URLSearchParams({
    q: query,
    p: threshold.toString(),
    k: limit.toString()
  });
  
  const response = await fetch(`http://localhost:8080/search?${params}`);
  const data = await response.json();
  
  return data.results;
}

// Usage
const results = await searchContent("battle scene with magic", 0.4, 15);
```

### Python Example
```python
import requests
from urllib.parse import urlencode

def search_content(query, threshold=0.3, limit=10):
    params = {
        'q': query,
        'p': threshold,
        'k': limit
    }
    
    response = requests.get(
        'http://localhost:8080/search',
        params=params
    )
    
    return response.json()

# Usage
results = search_content("dragon battle", threshold=0.4, limit=15)
```

## Performance Considerations

### Response Times
- **Typical latency**: 100-500ms for most queries
- **Factors affecting speed**:
  - Database size
  - Query complexity
  - Similarity threshold
  - Result limit

### Rate Limiting
- Currently no rate limiting implemented
- Consider implementing for production deployments

### Caching
- Vector embeddings cached in database
- No query result caching currently implemented

## Search Quality Tips

### Effective Query Strategies
1. **Use natural language**: "battle between armies" vs "battle army"
2. **Include context**: "magical healing potion" vs "potion"
3. **Vary specificity**: Start broad, then narrow down
4. **Experiment with thresholds**: Lower for discovery, higher for precision

### Similarity Threshold Guidelines
- **0.1-0.2**: Very broad matching (may include irrelevant results)
- **0.3-0.4**: Balanced relevance (recommended starting point)
- **0.5-0.7**: High precision (may miss relevant content)
- **0.8+**: Exact matching (very restrictive)

## Future API Enhancements

### Planned Features
- **Authentication and authorization**
- **Query result pagination**
- **Advanced filtering** (by author, book, chapter)
- **Query suggestions and autocomplete**
- **Batch search operations**
- **Real-time search via WebSocket**

### Potential Endpoints
- `GET /books` - List available books
- `GET /authors` - List available authors  
- `GET /stats` - Service statistics
- `POST /reindex` - Trigger reindexing

## MCP Server API

### Overview

The Model Context Protocol (MCP) server provides tool-based access to the audiobook transcription database for AI assistants like Claude Desktop. The MCP server runs as a separate service integrated into the main CLI.

### Server Configuration

#### Starting the MCP Server

```bash
# Default stdio transport
./lil-whisper mcp

# HTTP transport
MCP_TRANSPORT=http ./lil-whisper mcp

# Custom HTTP address
MCP_TRANSPORT=http MCP_HTTP_ADDR=:9000 ./lil-whisper mcp
```

#### Environment Variables

| Variable        | Default | Description                       |
| --------------- | ------- | --------------------------------- |
| `MCP_TRANSPORT` | `stdio` | Transport type: "stdio" or "http" |
| `MCP_HTTP_ADDR` | `:8081` | HTTP server address for MCP       |

### Available Tools

#### 1. semantic_search_audiobooks

**Purpose**: Search audiobook transcriptions using semantic similarity.

**Parameters**:
- `query` (string, required): Search query text
- `threshold` (number, optional, default: 0.3): Similarity threshold (0.0-1.0)
- `limit` (number, optional, default: 10): Maximum results to return

**Example Usage**:
```json
{
  "query": "dragons and magic spells",
  "threshold": 0.4,
  "limit": 5
}
```

#### 2. text_search_audiobooks

**Purpose**: Search audiobook transcriptions using PostgreSQL full-text search.

**Parameters**:
- `query` (string, required): Search query text
- `limit` (number, optional, default: 10): Maximum results to return

**Example Usage**:
```json
{
  "query": "Eragon spoke to Saphira",
  "limit": 10
}
```

#### 3. browse_audiobook_library

**Purpose**: Browse the audiobook library structure with hierarchical display.

**Parameters**:
- `author` (string, optional): Filter by author name (case-insensitive)
- `book` (string, optional): Filter by book title (case-insensitive)

**Example Usage**:
```json
{
  "author": "Paolini",
  "book": "Inheritance"
}
```

### Integration Examples

#### Claude Desktop

Add to your Claude Desktop MCP configuration:

```json
{
  "mcpServers": {
    "lilbro-whisper": {
      "command": "/path/to/lil-whisper",
      "args": ["mcp"],
      "env": {
        "MCP_TRANSPORT": "stdio"
      }
    }
  }
}
```

#### HTTP Client

For HTTP transport, the MCP server runs on `:8081` by default (different from the main HTTP API on `:8080`).

### Response Format

All MCP tools return formatted text responses optimized for display in AI assistants:

**Search Results**:
```markdown
Found 2 result(s):

**The Inheritance Cycle** by Christopher Paolini
Chapter 5: Eldest (chunk 15/120, similarity: 85%)
> Eragon felt the dragon's mind touch his own, a warm presence...

**The Inheritance Cycle** by Christopher Paolini  
Chapter 12: Brisingr (chunk 8/95, similarity: 78%)
> The ancient language flowed through him as he spoke...
```

**Library Browse**:
```markdown
📚 **Audiobook Library**

**Christopher Paolini**
├── Eragon (25 chapters)
│   ├── Chapter 1: Discovery
│   ├── Chapter 2: Palancar Valley
│   └── Chapter 3: Dragon Tales
└── Eldest (30 chapters)
    ├── Chapter 1: A Twin Disaster
    └── Chapter 2: The Council of Elders
```