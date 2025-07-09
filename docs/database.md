# Database Schema and Relationships

## Core Relationships

1. Authors have many Books
2. Books have many Chapters
3. Chapters have many Chunks (vectors)

## Table Relationships

The database schema consists of the following tables and their relationships:

1. **authors**: Stores author information
   - `id`: Primary key
   - `name`: Author's name (UNIQUE)

2. **books**: Stores book information
   - `id`: Primary key
   - `author_id`: Foreign key referencing `authors(id)`
   - `title`: Title of the book
   - `isbn`: ISBN of the book
   - `asin`: ASIN of the book
   - UNIQUE constraint on (author_id, title)

3. **chapters**: Stores chapter information
   - `id`: Primary key
   - `book_id`: Foreign key referencing `books(id)`
   - `title`: Title of the chapter
   - `index`: Chapter index number
   - UNIQUE constraint on (book_id, index)

4. **vectors**: Stores vector embeddings for content chunks
   - `id`: Primary key
   - `chapter_id`: Foreign key referencing `chapters(id)`
   - `chunk_index`: Index of the chunk
   - `content`: Text content of the chunk
   - `embedding`: Vector embedding of the content (1536 dimensions)
   - `chunk_size`: Size of the chunk used for processing
   - UNIQUE constraint on (chapter_id, chunk_index)

5. **transcriptions**: Stores raw transcription data with deduplication
   - `id`: Primary key
   - `file_path`: Path to the original audio file (UNIQUE)
   - `file_checksum`: SHA256 checksum of the file content
   - `file_size`: Size of the original file in bytes
   - `settings_hash`: Hash of processing settings for deduplication
   - `transcription_text`: Complete raw transcription text
   - `word_count`: Number of words in the transcription
   - `processing_duration_ms`: Time taken to process in milliseconds
   - `created_at`: Timestamp of creation
   - `updated_at`: Timestamp of last update

## Indexes

The following indexes optimize query performance:

1. **authors indexes**:
   - `idx_authors_name`: Index on the `name` column

2. **books indexes**:
   - `idx_books_title`: Index on the `title` column

3. **chapters indexes**:
   - `idx_chapters_book_id`: Index on the `book_id` column

4. **vectors indexes**:
   - `idx_vectors_chapter_id`: Index on the `chapter_id` column
   - `vectors_embedding_idx`: HNSW index on the `embedding` column for vector similarity search

5. **transcriptions indexes**:
   - `idx_transcriptions_checksum_settings`: Index on `(file_checksum, settings_hash)` for deduplication
   - `idx_transcriptions_file_path`: Index on the `file_path` column

## Example Queries

### Insert New Author with Book and Chapter

```sql
-- Insert or get author
INSERT INTO authors (name) 
VALUES ($1)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING id;

-- Insert or get book
INSERT INTO books (author_id, title, isbn, asin) 
VALUES ($1, $2, $3, $4)
ON CONFLICT (author_id, title) DO UPDATE 
SET isbn = EXCLUDED.isbn, asin = EXCLUDED.asin
RETURNING id;

-- Insert chapter
INSERT INTO chapters (book_id, title, index)
VALUES ($1, $2, $3)
ON CONFLICT (book_id, index) DO UPDATE SET title = EXCLUDED.title
RETURNING id;
```

### Search Similar Vectors with Metadata

```sql
WITH vector_matches AS (
    SELECT 
        v.id,
        v.content,
        v.chunk_index,
        1 - (v.embedding <=> $1) as similarity
    FROM vectors v
    WHERE 1 - (v.embedding <=> $1) >= $threshold
    ORDER BY v.embedding <=> $1
    LIMIT $limit
)
SELECT 
    vm.id,
    vm.content,
    a.name as author,
    b.title as book_title,
    c.title as chapter,
    vm.chunk_index,
    vm.similarity
FROM vector_matches vm
JOIN chapters c ON c.id = vm.chapter_id
JOIN books b ON b.id = c.book_id
JOIN authors a ON a.id = b.author_id
ORDER BY vm.similarity DESC;
```

### Search Similar Vectors with Extended Metadata

```sql
WITH vector_matches AS (
    SELECT 
        v.id,
        v.content,
        v.chunk_index,
        1 - (v.embedding <=> $1) as similarity
    FROM vectors v
    WHERE 1 - (v.embedding <=> $1) >= $threshold
    ORDER BY v.embedding <=> $1
    LIMIT $limit
)
SELECT 
    vm.id,
    vm.content,
    a.name as author,
    b.title as book_title,
    c.title as chapter_title,
    c.index as chapter_index,
    vm.chunk_index,
    vm.similarity,
    (SELECT COUNT(*) FROM vectors WHERE chapter_id = c.id) as total_chunks,
    (SELECT COUNT(*) FROM chapters WHERE book_id = b.id) as total_chapters
FROM vector_matches vm
JOIN chapters c ON c.id = vm.chapter_id
JOIN books b ON b.id = c.book_id
JOIN authors a ON a.id = b.author_id
ORDER BY vm.similarity DESC;
```