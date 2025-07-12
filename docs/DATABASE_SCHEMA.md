# Database Schema Reference

## Overview

The lilbro-whisper service uses PostgreSQL with the pgvector extension to store hierarchical audiobook data and enable semantic search capabilities. The schema supports authors, books, chapters, vector embeddings, and raw transcriptions with comprehensive deduplication and metadata tracking.

## Database Requirements

### PostgreSQL Version
- **Minimum**: PostgreSQL 12+
- **Recommended**: PostgreSQL 14+
- **Required Extensions**: pgvector

### pgvector Extension
```sql
CREATE EXTENSION IF NOT EXISTS vector;
```

## Schema Architecture

### Hierarchical Structure
The database follows a hierarchical relationship model:
```
Authors (1) → Books (N) → Chapters (N) → Vectors (N)
                                      ↘
                                        Transcriptions (1:1 with audio files)
```

### Core Design Principles
1. **Hierarchical organization**: Natural book structure representation
2. **Deduplication**: SHA256-based file content deduplication
3. **Vector search**: Semantic similarity using pgvector embeddings
4. **Metadata preservation**: Complete book and chapter information
5. **Processing tracking**: Status and statistics monitoring

## Table Definitions

### 1. Authors Table
**Purpose**: Stores unique author information

```sql
CREATE TABLE authors (
    id SERIAL PRIMARY KEY,
    name VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

**Indexes**:
```sql
CREATE INDEX idx_authors_name ON authors(name);
```

**Fields**:
- `id`: Auto-incrementing primary key
- `name`: Author's full name (unique constraint)
- `created_at`: Record creation timestamp
- `updated_at`: Last modification timestamp

### 2. Books Table
**Purpose**: Stores book metadata with author relationships

```sql
CREATE TABLE books (
    id SERIAL PRIMARY KEY,
    author_id INTEGER NOT NULL REFERENCES authors(id) ON DELETE CASCADE,
    title VARCHAR(255) NOT NULL,
    isbn VARCHAR(20),
    asin VARCHAR(20),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(author_id, title)
);
```

**Indexes**:
```sql
CREATE INDEX idx_books_author_id ON books(author_id);
CREATE INDEX idx_books_title ON books(title);
```

**Fields**:
- `id`: Auto-incrementing primary key
- `author_id`: Foreign key to authors table
- `title`: Book title
- `isbn`: International Standard Book Number (optional)
- `asin`: Amazon Standard Identification Number (optional)

**Constraints**:
- `UNIQUE(author_id, title)`: Prevents duplicate books per author

### 3. Chapters Table
**Purpose**: Stores chapter information within books

```sql
CREATE TABLE chapters (
    id SERIAL PRIMARY KEY,
    book_id INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    title VARCHAR(255) NOT NULL,
    index INTEGER NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(book_id, index)
);
```

**Indexes**:
```sql
CREATE INDEX idx_chapters_book_id ON chapters(book_id);
CREATE INDEX idx_chapters_index ON chapters(book_id, index);
```

**Fields**:
- `id`: Auto-incrementing primary key
- `book_id`: Foreign key to books table
- `title`: Chapter title or name
- `index`: Chapter order within book (1-based)

**Constraints**:
- `UNIQUE(book_id, index)`: Ensures unique chapter ordering per book

### 4. Vectors Table
**Purpose**: Stores text chunks with vector embeddings for semantic search

```sql
CREATE TABLE vectors (
    id SERIAL PRIMARY KEY,
    chapter_id INTEGER NOT NULL REFERENCES chapters(id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL,
    content TEXT NOT NULL,
    embedding VECTOR(1536) NOT NULL,
    chunk_size INTEGER NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(chapter_id, chunk_index)
);
```

**Indexes**:
```sql
CREATE INDEX idx_vectors_chapter_id ON vectors(chapter_id);
CREATE INDEX vectors_embedding_idx ON vectors USING hnsw (embedding vector_cosine_ops);
```

**Fields**:
- `id`: Auto-incrementing primary key
- `chapter_id`: Foreign key to chapters table
- `chunk_index`: Position of chunk within chapter (0-based)
- `content`: Actual text content of the chunk
- `embedding`: 1536-dimensional vector embedding (OpenAI text-embedding-ada-002)
- `chunk_size`: Size configuration used when creating this chunk

**Vector Index**:
- **Type**: HNSW (Hierarchical Navigable Small World)
- **Distance Function**: Cosine similarity
- **Dimensions**: 1536 (matches OpenAI embedding model)

### 5. Transcriptions Table
**Purpose**: Stores raw transcription data with deduplication and processing metadata

```sql
CREATE TABLE transcriptions (
    id SERIAL PRIMARY KEY,
    file_path VARCHAR(500) NOT NULL UNIQUE,
    file_checksum VARCHAR(64) NOT NULL,
    file_size BIGINT NOT NULL,
    settings_hash VARCHAR(64) NOT NULL,
    transcription_text TEXT NOT NULL,
    corrected_text TEXT,
    correction_status VARCHAR(20) DEFAULT 'pending',
    correction_error TEXT,
    correction_metadata JSONB,
    word_count INTEGER NOT NULL,
    processing_duration_ms BIGINT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

**Indexes**:
```sql
CREATE INDEX idx_transcriptions_file_path ON transcriptions(file_path);
CREATE INDEX idx_transcriptions_checksum_settings ON transcriptions(file_checksum, settings_hash);
```

**Fields**:
- `id`: Auto-incrementing primary key
- `file_path`: Full path to original audio file (unique)
- `file_checksum`: SHA256 hash of file content
- `file_size`: File size in bytes
- `settings_hash`: Hash of processing configuration for cache validation
- `transcription_text`: Complete raw transcription text from Yap
- `corrected_text`: LLM-corrected transcription text using 3-stage pipeline (nullable)
- `correction_status`: Status of LLM correction process
  - `'pending'`: Correction not yet attempted (default)
  - `'in_progress'`: Currently being processed by LLM
  - `'completed'`: Successfully corrected through all 3 stages
  - `'failed'`: Correction failed, fallback to original text
- `correction_error`: Detailed error message if correction failed (nullable)
- `correction_metadata`: JSONB metadata about correction process:
  - `model`: LLM model used for correction
  - `stages_completed`: Number of pipeline stages completed (1-3)
  - `total_tokens`: Total tokens used across all stages
  - `processing_time_ms`: Time spent on LLM correction
  - `estimated_cost`/`actual_cost`: API cost tracking
  - `was_chunked`: Whether text required chunking for token limits
  - `chunks_processed`: Number of text chunks processed
  - `stages`: Detailed results from each pipeline stage
- `word_count`: Number of words in transcription
- `processing_duration_ms`: Transcription processing time in milliseconds

**Dual Storage Strategy**:
- Raw transcriptions preserved for audit and fallback purposes
- Corrected text used for chunking and embedding generation
- Atomic transaction processing prevents inconsistent correction states
- Only corrected text (or original if correction disabled/failed) is chunked and vectorized

**Deduplication Strategy**:
- Files identified by content checksum, not path
- Settings changes invalidate cached transcriptions
- Enables efficient reprocessing and cache management

## Relationships and Constraints

### Foreign Key Relationships
```sql
-- Cascade deletions maintain referential integrity
authors (1) ← books (N) ← chapters (N) ← vectors (N)

-- Orphaned records automatically cleaned up
-- Example: Deleting an author removes all associated books, chapters, and vectors
```

### Unique Constraints
1. **Authors**: `name` (prevents duplicate authors)
2. **Books**: `(author_id, title)` (prevents duplicate books per author)
3. **Chapters**: `(book_id, index)` (ensures proper chapter ordering)
4. **Vectors**: `(chapter_id, chunk_index)` (prevents duplicate chunks)
5. **Transcriptions**: `file_path` (one transcription per file path)

## Common Queries

### Insert Operations

#### Insert Author with Book and Chapter
```sql
-- Insert or get author
INSERT INTO authors (name) 
VALUES ($1)
ON CONFLICT (name) DO UPDATE SET 
    name = EXCLUDED.name,
    updated_at = CURRENT_TIMESTAMP
RETURNING id;

-- Insert or get book
INSERT INTO books (author_id, title, isbn, asin) 
VALUES ($1, $2, $3, $4)
ON CONFLICT (author_id, title) DO UPDATE SET 
    isbn = EXCLUDED.isbn, 
    asin = EXCLUDED.asin,
    updated_at = CURRENT_TIMESTAMP
RETURNING id;

-- Insert chapter
INSERT INTO chapters (book_id, title, index)
VALUES ($1, $2, $3)
ON CONFLICT (book_id, index) DO UPDATE SET 
    title = EXCLUDED.title,
    updated_at = CURRENT_TIMESTAMP
RETURNING id;
```

#### Insert Vector Embedding
```sql
INSERT INTO vectors (chapter_id, chunk_index, content, embedding, chunk_size)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (chapter_id, chunk_index) DO UPDATE SET
    content = EXCLUDED.content,
    embedding = EXCLUDED.embedding,
    chunk_size = EXCLUDED.chunk_size;
```

### Search Operations

#### Vector Similarity Search
```sql
WITH vector_matches AS (
    SELECT 
        v.id,
        v.content,
        v.chunk_index,
        v.chapter_id,
        1 - (v.embedding <=> $1::vector) as similarity
    FROM vectors v
    WHERE 1 - (v.embedding <=> $1::vector) >= $2
    ORDER BY v.embedding <=> $1::vector
    LIMIT $3
)
SELECT 
    vm.id,
    vm.content,
    a.name as author,
    b.title as title,
    c.title as chapter,
    c.index as chapter_index,
    vm.chunk_index,
    vm.similarity,
    (SELECT COUNT(*) FROM vectors WHERE chapter_id = vm.chapter_id) as total_chunks,
    (SELECT COUNT(*) FROM chapters WHERE book_id = b.id) as total_chapters
FROM vector_matches vm
JOIN chapters c ON c.id = vm.chapter_id
JOIN books b ON b.id = c.book_id
JOIN authors a ON a.id = b.author_id
ORDER BY vm.similarity DESC;
```

#### Check Processing Status
```sql
-- Check if file needs transcription
SELECT EXISTS(
    SELECT 1 FROM transcriptions 
    WHERE file_path = $1 
    AND file_checksum = $2 
    AND settings_hash = $3
);

-- Get existing transcription
SELECT transcription_text, word_count 
FROM transcriptions 
WHERE file_path = $1;

-- Check correction status
SELECT correction_status, correction_error, correction_metadata
FROM transcriptions 
WHERE file_path = $1;
```

### LLM Correction Operations

#### Atomic Correction Processing
```sql
-- Set correction status to in_progress (first transaction)
UPDATE transcriptions 
SET correction_status = 'in_progress',
    correction_error = NULL,
    updated_at = NOW()
WHERE file_path = $1;

-- Update with successful correction (second transaction)
UPDATE transcriptions 
SET corrected_text = $2,
    correction_status = 'completed',
    correction_error = NULL,
    correction_metadata = $3,
    updated_at = NOW()
WHERE file_path = $1;

-- Update with failed correction (second transaction alternative)
UPDATE transcriptions 
SET correction_status = 'failed',
    correction_error = $2,
    updated_at = NOW()
WHERE file_path = $1;
```

#### Correction Status Queries
```sql
-- Get files stuck in 'in_progress' status (potential crashes)
SELECT file_path, updated_at
FROM transcriptions 
WHERE correction_status = 'in_progress'
AND updated_at < NOW() - INTERVAL '1 hour';

-- Get correction success rate
SELECT 
    correction_status,
    COUNT(*) as count,
    ROUND(COUNT(*) * 100.0 / SUM(COUNT(*)) OVER (), 2) as percentage
FROM transcriptions
WHERE correction_status IN ('completed', 'failed')
GROUP BY correction_status;

-- Get files needing correction retry
SELECT file_path, correction_error, updated_at
FROM transcriptions
WHERE correction_status = 'failed'
ORDER BY updated_at DESC;
```

### Analytics Queries

#### Processing Statistics
```sql
-- Get comprehensive processing stats including corrections
SELECT 
    COUNT(DISTINCT a.id) as total_authors,
    COUNT(DISTINCT b.id) as total_books,
    COUNT(DISTINCT c.id) as total_chapters,
    COUNT(v.id) as total_chunks,
    COUNT(t.id) as total_transcriptions,
    SUM(t.word_count) as total_words,
    AVG(t.processing_duration_ms) as avg_processing_time_ms,
    COUNT(CASE WHEN t.correction_status = 'completed' THEN 1 END) as corrected_files,
    COUNT(CASE WHEN t.correction_status = 'failed' THEN 1 END) as correction_failures,
    COUNT(CASE WHEN t.correction_status = 'pending' THEN 1 END) as pending_corrections
FROM authors a
LEFT JOIN books b ON a.id = b.author_id
LEFT JOIN chapters c ON b.id = c.book_id
LEFT JOIN vectors v ON c.id = v.chapter_id
LEFT JOIN transcriptions t ON t.file_path LIKE '%' || b.title || '%';
```

#### LLM Correction Analytics
```sql
-- Get detailed correction performance metrics
SELECT 
    (correction_metadata->>'model') as model_used,
    (correction_metadata->>'stages_completed')::int as stages_completed,
    AVG((correction_metadata->>'total_tokens')::int) as avg_tokens,
    AVG((correction_metadata->>'processing_time_ms')::int) as avg_correction_time_ms,
    AVG((correction_metadata->>'actual_cost')::numeric) as avg_cost,
    COUNT(CASE WHEN (correction_metadata->>'was_chunked')::boolean THEN 1 END) as chunked_files,
    COUNT(*) as total_corrections
FROM transcriptions
WHERE correction_status = 'completed'
AND correction_metadata IS NOT NULL
GROUP BY (correction_metadata->>'model'), (correction_metadata->>'stages_completed')::int
ORDER BY avg_cost DESC;

-- Daily cost tracking
SELECT 
    DATE(updated_at) as correction_date,
    SUM((correction_metadata->>'actual_cost')::numeric) as daily_cost,
    COUNT(*) as corrections_count,
    AVG((correction_metadata->>'total_tokens')::int) as avg_tokens_per_correction
FROM transcriptions
WHERE correction_status = 'completed'
AND correction_metadata IS NOT NULL
AND updated_at >= NOW() - INTERVAL '30 days'
GROUP BY DATE(updated_at)
ORDER BY correction_date DESC;
```

#### Find Books with Mismatched Chunk Sizes
```sql
-- Identify books needing reprocessing due to chunk size changes
SELECT DISTINCT
    b.id as book_id,
    b.title,
    a.name as author,
    v.chunk_size as current_chunk_size,
    $1 as target_chunk_size
FROM books b
JOIN authors a ON a.id = b.author_id
JOIN chapters c ON c.book_id = b.id
JOIN vectors v ON v.chapter_id = c.id
WHERE v.chunk_size != $1;
```

## Performance Optimization

### Vector Search Performance
- **HNSW Index**: Optimizes similarity search queries
- **Index Parameters**: Can be tuned for speed vs accuracy tradeoffs
- **Batch Operations**: Use bulk inserts for large datasets

### Query Optimization Tips
1. **Use LIMIT**: Always limit vector search results
2. **Filter Early**: Apply WHERE clauses before vector operations
3. **Index Usage**: Ensure foreign key and search columns are indexed
4. **Batch Processing**: Use transactions for multiple related operations

### Index Maintenance
```sql
-- Reindex vector similarity index if needed
REINDEX INDEX vectors_embedding_idx;

-- Analyze tables for query planning
ANALYZE authors, books, chapters, vectors, transcriptions;
```

## Data Migration and Maintenance

### Schema Updates
- Use migrations for schema changes
- Test with representative data sets
- Consider downtime for large index rebuilds

### Backup Considerations
- **pg_dump**: For complete database backups
- **Vector Data**: Large embedding tables may require special handling
- **Point-in-time Recovery**: Configure WAL archiving for production

### Cleanup Operations
```sql
-- Remove orphaned vectors (chapters without books)
DELETE FROM vectors 
WHERE chapter_id NOT IN (SELECT id FROM chapters);

-- Clean up old transcriptions for non-existent files
DELETE FROM transcriptions 
WHERE file_path NOT IN (
    SELECT DISTINCT file_path FROM processed_files
);
```

## Storage Requirements

### Size Estimates
- **Vectors**: ~6KB per chunk (1536 floats + metadata)
- **Transcriptions**: Variable based on audio length
- **Indexes**: 20-30% of table size for HNSW indexes

### Scaling Considerations
- **Partition large tables** by author or date if needed
- **Consider read replicas** for high query loads
- **Monitor index bloat** and maintenance needs