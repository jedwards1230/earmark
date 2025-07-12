package db

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jedwards1230/lil-whisper/internal/chunker"
	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/log"
	"github.com/jedwards1230/lil-whisper/internal/meta"
	"github.com/jedwards1230/lil-whisper/internal/openai"
	"github.com/jedwards1230/lil-whisper/internal/utils"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

type HierarchicalEntry struct {
	Author   string
	Title    string
	Chapters []string
}

type Statistics struct {
	ProcessedBooks    int
	ProcessedChapters int
	ReprocessingBooks int
}

type Transcription struct {
	ID                   int       `json:"id"`
	FilePath             string    `json:"file_path"`
	FileChecksum         string    `json:"file_checksum"`
	FileSize             int64     `json:"file_size"`
	SettingsHash         string    `json:"settings_hash"`
	TranscriptionText    string    `json:"transcription_text"`
	CorrectedText        *string   `json:"corrected_text,omitempty"`
	CorrectionStatus     string    `json:"correction_status"`
	CorrectionError      *string   `json:"correction_error,omitempty"`
	CorrectionMetadata   *string   `json:"correction_metadata,omitempty"`
	WordCount            int       `json:"word_count"`
	ProcessingDurationMs int64     `json:"processing_duration_ms"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type DB struct {
	conn *pgx.Conn
	e    *openai.Embeddings
	cfg  *config.Config
	log  log.Logger
}

type SearchResultWithMetadata struct {
	ID            int     `json:"id"`
	Author        string  `json:"author"`
	Title         string  `json:"title"`
	Chapter       string  `json:"chapter"`
	ChapterIndex  int     `json:"chapterIndex"`
	ChapterTitle  string  `json:"chapterTitle"`
	ChunkIndex    int     `json:"chunkIndex"`
	ChunkID       string  `json:"chunkID"` // Unique identifier: author_book_chapter_chunk
	Content       string  `json:"content"`
	Similarity    float64 `json:"similarity"`
	WordCount     int     `json:"wordCount"`  // Words in this chunk
	ChunkStart    int     `json:"chunkStart"` // Character offset start in chapter
	ChunkEnd      int     `json:"chunkEnd"`   // Character offset end in chapter
	TotalChunks   int     `json:"totalChunks"`
	TotalChapters int     `json:"totalChapters"`
	FilePath      string  `json:"filePath"`       // Original audio file path
	FileChecksum  string  `json:"fileChecksum"`   // SHA256 for deduplication
	ISBN          string  `json:"isbn,omitempty"` // Book ISBN if available
	ASIN          string  `json:"asin,omitempty"` // Book ASIN if available
}

func New(cfg *config.Config) (*DB, error) {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:5432/%s", cfg.DBUser, cfg.DBPassword, cfg.DBHost, cfg.DBName)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		return nil, err
	}

	logger := log.NewLogger("db")

	db := &DB{
		conn: conn,
		e:    openai.NewEmbeddings(cfg),
		cfg:  cfg,
		log:  logger,
	}

	if err := db.initialize(context.Background()); err != nil {
		conn.Close(context.Background())
		return nil, err
	}

	db.log.Info(fmt.Sprintf("Database initialized at %s", cfg.DBHost))
	return db, nil
}

func (db *DB) initialize(ctx context.Context) error {
	tx, err := db.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback(ctx)

	// Enable vector extension
	if _, err := tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("failed to create vector extension: %v", err)
	}

	// Register pgvector types
	if err := pgxvector.RegisterTypes(context.Background(), db.conn); err != nil {
		db.conn.Close(context.Background())
		return fmt.Errorf("failed to register vector types: %v", err)
	}

	// Create tables and indexes in one transaction
	if _, err := tx.Exec(ctx, `
        -- Authors table
        CREATE TABLE IF NOT EXISTS authors (
            id SERIAL PRIMARY KEY,
            name TEXT NOT NULL UNIQUE
        );

        -- Books table
        CREATE TABLE IF NOT EXISTS books (
            id SERIAL PRIMARY KEY,
            author_id INTEGER REFERENCES authors(id) ON DELETE CASCADE,
            title TEXT NOT NULL,
            isbn TEXT DEFAULT '',
            asin TEXT DEFAULT '',
            UNIQUE(author_id, title)
        );

        -- Chapters table
        CREATE TABLE IF NOT EXISTS chapters (
            id SERIAL PRIMARY KEY,
            book_id INTEGER REFERENCES books(id) ON DELETE CASCADE,
            title TEXT NOT NULL,
            index INTEGER NOT NULL,
            UNIQUE(book_id, index)
        );

        -- Vectors table
        CREATE TABLE IF NOT EXISTS vectors (
            id SERIAL PRIMARY KEY,
            chapter_id INTEGER REFERENCES chapters(id) ON DELETE CASCADE,
            chunk_index INTEGER NOT NULL,
            content TEXT NOT NULL,
            embedding vector(1536),
            chunk_size INTEGER NOT NULL,
            UNIQUE(chapter_id, chunk_index)
        );

        -- Transcriptions table (raw transcription storage)
        CREATE TABLE IF NOT EXISTS transcriptions (
            id SERIAL PRIMARY KEY,
            file_path TEXT NOT NULL UNIQUE,
            file_checksum TEXT NOT NULL,
            file_size BIGINT NOT NULL,
            settings_hash TEXT NOT NULL,
            transcription_text TEXT NOT NULL,
            corrected_text TEXT,
            correction_status VARCHAR(20) DEFAULT 'pending',
            correction_error TEXT,
            correction_metadata JSONB,
            word_count INTEGER,
            processing_duration_ms BIGINT,
            created_at TIMESTAMP DEFAULT NOW(),
            updated_at TIMESTAMP DEFAULT NOW()
        );

        -- Standard indexes
        CREATE INDEX IF NOT EXISTS idx_authors_name ON authors(name);
        CREATE INDEX IF NOT EXISTS idx_books_title ON books(title);
        CREATE INDEX IF NOT EXISTS idx_chapters_book_id ON chapters(book_id);
        CREATE INDEX IF NOT EXISTS idx_vectors_chapter_id ON vectors(chapter_id);
        CREATE INDEX IF NOT EXISTS idx_transcriptions_checksum_settings 
            ON transcriptions(file_checksum, settings_hash);
        CREATE INDEX IF NOT EXISTS idx_transcriptions_file_path ON transcriptions(file_path);

        -- HNSW index for vector similarity
        CREATE INDEX IF NOT EXISTS vectors_embedding_idx 
            ON vectors USING hnsw (embedding vector_cosine_ops)
            WITH (m = 16, ef_construction = 64);
    `); err != nil {
		db.log.Warn("Warning: schema creation failed", "error", err)
		return fmt.Errorf("failed creating schema: %v", err)
	}

	// Add LLM correction columns to existing transcriptions tables (migration)
	if _, err := tx.Exec(ctx, `
        ALTER TABLE transcriptions 
        ADD COLUMN IF NOT EXISTS corrected_text TEXT,
        ADD COLUMN IF NOT EXISTS correction_status VARCHAR(20) DEFAULT 'pending',
        ADD COLUMN IF NOT EXISTS correction_error TEXT,
        ADD COLUMN IF NOT EXISTS correction_metadata JSONB;
    `); err != nil {
		db.log.Warn("Warning: LLM correction column migration failed", "error", err)
		// Don't fail initialization for migration errors
	}

	return tx.Commit(ctx)
}

func (db *DB) InsertContentWithMetadata(ctx context.Context, content string, meta *meta.FileMetadata) error {
	tx, err := db.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Insert or get author
	var authorID int
	err = tx.QueryRow(ctx, `
        INSERT INTO authors (name) 
        VALUES ($1)
        ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
        RETURNING id
    `, meta.Author).Scan(&authorID)
	if err != nil {
		return fmt.Errorf("failed to insert author: %v", err)
	}

	// Insert or get book
	var bookID int
	err = tx.QueryRow(ctx, `
        INSERT INTO books (author_id, title, isbn, asin) 
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (author_id, title) DO UPDATE 
        SET isbn = EXCLUDED.isbn, asin = EXCLUDED.asin
        RETURNING id
    `, authorID, meta.Title, meta.ISBN, meta.ASIN).Scan(&bookID)
	if err != nil {
		return fmt.Errorf("failed to insert book: %v", err)
	}

	// Insert chapter
	var chapterID int
	err = tx.QueryRow(ctx, `
        INSERT INTO chapters (book_id, title, index)
        VALUES ($1, $2, $3)
        ON CONFLICT (book_id, index) DO UPDATE SET title = EXCLUDED.title
        RETURNING id
    `, bookID, meta.Chapter, meta.ChapterIndex).Scan(&chapterID)
	if err != nil {
		return fmt.Errorf("failed to insert chapter: %v", err)
	}

	chunks, allEmbeddings, err := db.chunkAndEmbed(content, db.cfg.ChunkSize)
	if err != nil {
		return fmt.Errorf("failed to chunk and embed content: %v", err)
	}

	// Insert vector chunks
	for i, emb := range allEmbeddings {
		_, err = tx.Exec(ctx, `
            INSERT INTO vectors (chapter_id, chunk_index, content, embedding, chunk_size)
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (chapter_id, chunk_index) DO UPDATE 
            SET content = EXCLUDED.content, 
                embedding = EXCLUDED.embedding,
                chunk_size = EXCLUDED.chunk_size
        `, chapterID, i, chunks[i], pgvector.NewVector(emb), db.cfg.ChunkSize)
		if err != nil {
			return fmt.Errorf("failed to insert vector chunk: %v", err)
		}
	}

	return tx.Commit(ctx)
}

func (db *DB) chunkAndEmbed(content string, chunkSize int) (chunks []string, embeddings [][]float32, err error) {
	if content == "" {
		return nil, nil, fmt.Errorf("empty content")
	}

	chunks = chunker.Chunker(content, chunkSize, chunker.SplitTypeToken)
	if len(chunks) == 0 {
		return nil, nil, fmt.Errorf("no chunks found")
	}

	db.log.Debug("Splitting content into chunks", "count", len(chunks))

	allEmbeddings, err := db.e.GetEmbeddings(chunks)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate embeddings: %v", err)
	}

	if len(allEmbeddings) != len(chunks) {
		return nil, nil, fmt.Errorf("mismatched embeddings and chunks: %d vs %d", len(allEmbeddings), len(chunks))
	}

	return chunks, allEmbeddings, nil
}

func (db *DB) GetMetadataByVectorID(ctx context.Context, vectorID int) (*meta.FileMetadata, error) {
	var meta meta.FileMetadata
	err := db.conn.QueryRow(ctx, `
        SELECT a.name, b.title, c.title, b.isbn, b.asin, c.index
        FROM vectors v
        JOIN chapters c ON v.chapter_id = c.id
        JOIN books b ON c.book_id = b.id
        JOIN authors a ON b.author_id = a.id
        WHERE v.id = $1
    `, vectorID).Scan(&meta.Author, &meta.Title, &meta.Chapter, &meta.ISBN, &meta.ASIN, &meta.ChapterIndex)
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

func (db *DB) IsProcessed(ctx context.Context, filePath string) (bool, error) {
	// Use the new transcriptions table with checksum and settings validation
	fileChecksum, err := db.ComputeFileChecksum(filePath)
	if err != nil {
		// If we can't compute checksum, fall back to old method
		db.log.Warn("Failed to compute file checksum, using legacy check", "file", filePath, "error", err)
		return db.isProcessedLegacy(ctx, filePath)
	}

	settingsHash := db.ComputeSettingsHash(db.cfg)

	needsTranscription, err := db.NeedsTranscription(ctx, filePath, fileChecksum, settingsHash)
	if err != nil {
		return false, fmt.Errorf("error checking transcription status: %w", err)
	}

	// Return true if already processed (doesn't need transcription)
	return !needsTranscription, nil
}

// isProcessedLegacy is the original implementation for fallback
func (db *DB) isProcessedLegacy(ctx context.Context, filePath string) (bool, error) {
	// Extract chapter info from the filepath directly to use in check
	_, _, chapterIndex, chapterTitle := utils.ParseFilePath(filePath)

	var exists bool
	err := db.conn.QueryRow(ctx, `
        SELECT EXISTS(
            SELECT 1 FROM vectors v
            JOIN chapters c ON v.chapter_id = c.id
            JOIN books b ON c.book_id = b.id
            JOIN authors a ON b.author_id = a.id
            WHERE c.title = $1 AND c.index = $2
        )
    `, chapterTitle, chapterIndex).Scan(&exists)

	if err != nil {
		return false, fmt.Errorf("error checking if file is processed: %w", err)
	}

	return exists, nil
}

func (db *DB) Search(ctx context.Context, query string, limit int, threshold float64) ([]SearchResultWithMetadata, error) {
	db.log.Debug("Performing search", "query", query, "limit", limit, "threshold", threshold)
	_, allEmbeddings, err := db.chunkAndEmbed(query, db.cfg.ChunkSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk and embed content: %v", err)
	}

	// Collect combined results from each embedding
	var combined []SearchResultWithMetadata
	for _, emb := range allEmbeddings {
		partial, err := db.findSimilar(ctx, emb, limit, threshold)
		if err != nil {
			return nil, err
		}
		combined = append(combined, partial...)
	}
	return combined, nil
}

func (db *DB) GetHierarchicalData(ctx context.Context) ([]HierarchicalEntry, error) {
	rows, err := db.conn.Query(ctx, `
        SELECT 
            a.name as author,
            b.title,
            array_agg(c.title ORDER BY c.index) as chapters
        FROM authors a
        JOIN books b ON b.author_id = a.id
        JOIN chapters c ON c.book_id = b.id
        GROUP BY a.name, b.title
        ORDER BY a.name, b.title
    `)
	if err != nil {
		return nil, fmt.Errorf("failed to query hierarchical data: %v", err)
	}
	defer rows.Close()

	var entries []HierarchicalEntry
	for rows.Next() {
		var entry HierarchicalEntry
		if err := rows.Scan(&entry.Author, &entry.Title, &entry.Chapters); err != nil {
			return nil, fmt.Errorf("failed to scan entry: %v", err)
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

func (db *DB) Close() {
	db.conn.Close(context.Background())
}

func (db *DB) GetProcessingStats(ctx context.Context) (*Statistics, error) {
	stats := &Statistics{}
	err := db.conn.QueryRow(ctx, `
        SELECT 
            COUNT(DISTINCT b.id),
            COUNT(DISTINCT c.id)
        FROM books b
        LEFT JOIN chapters c ON c.book_id = b.id
    `).Scan(&stats.ProcessedBooks, &stats.ProcessedChapters)

	if err != nil {
		return nil, err
	}
	return stats, nil
}

func (db *DB) findSimilar(ctx context.Context, vec []float32, limit int, threshold float64) ([]SearchResultWithMetadata, error) {
	query := `
        WITH vector_matches AS (
            SELECT 
                v.id,
                v.content,
                v.chapter_id,
                v.chunk_index,
                1 - (v.embedding <=> $1) as similarity
            FROM vectors v
            WHERE ($3 = 0 OR 1 - (v.embedding <=> $1) >= $3)
            ORDER BY v.embedding <=> $1
            LIMIT $2
        )
        SELECT 
            vm.id,
            vm.content,
            a.name as author,
            b.title,
            c.title as chapter,
            vm.chunk_index,
            vm.similarity,
            c.index as chapter_index,
            c.title as chapter_title,
            (SELECT COUNT(*) FROM vectors WHERE chapter_id = c.id) as total_chunks,
            (SELECT COUNT(*) FROM chapters WHERE book_id = b.id) as total_chapters,
            COALESCE(t.file_path, '') as file_path,
            COALESCE(t.file_checksum, '') as file_checksum,
            CONCAT(a.name, '_', b.title, '_', c.index, '_', vm.chunk_index) as chunk_id,
            LENGTH(vm.content) - LENGTH(REPLACE(vm.content, ' ', '')) + 1 as word_count,
            COALESCE(b.isbn, '') as isbn,
            COALESCE(b.asin, '') as asin
        FROM vector_matches vm
        JOIN chapters c ON c.id = vm.chapter_id
        JOIN books b ON c.book_id = b.id
        JOIN authors a ON b.author_id = a.id
        LEFT JOIN transcriptions t ON t.file_path LIKE '%' || b.title || '%'
        ORDER BY vm.similarity DESC
    `

	rows, err := db.conn.Query(ctx, query, pgvector.NewVector(vec), limit, threshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResultWithMetadata
	for rows.Next() {
		var result SearchResultWithMetadata
		if err := rows.Scan(
			&result.ID,
			&result.Content,
			&result.Author,
			&result.Title,
			&result.Chapter,
			&result.ChunkIndex,
			&result.Similarity,
			&result.ChapterIndex,
			&result.ChapterTitle,
			&result.TotalChunks,
			&result.TotalChapters,
			&result.FilePath,
			&result.FileChecksum,
			&result.ChunkID,
			&result.WordCount,
			&result.ISBN,
			&result.ASIN,
		); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (db *DB) Reset(ctx context.Context) error {
	db.log.Warn("Performing complete database reset...")

	tx, err := db.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback(ctx)

	// Drop all tables in the public schema
	if _, err := tx.Exec(ctx, `
        DO $$ DECLARE
            r RECORD;
        BEGIN
            -- Drop all tables in the current schema
            FOR r IN (SELECT tablename FROM pg_tables WHERE schemaname = 'public') LOOP
                EXECUTE 'DROP TABLE IF EXISTS ' || quote_ident(r.tablename) || ' CASCADE';
            END LOOP;

            -- Drop all sequences
            FOR r IN (SELECT sequence_name FROM information_schema.sequences WHERE sequence_schema = 'public') LOOP
                EXECUTE 'DROP SEQUENCE IF EXISTS ' || quote_ident(r.sequence_name) || ' CASCADE';
            END LOOP;

            -- Drop all custom types
            FOR r IN (SELECT typname FROM pg_type 
                     WHERE typnamespace = 'public'::regnamespace 
                     AND typtype = 'c'
                     AND typname != 'vector') LOOP
                EXECUTE 'DROP TYPE IF EXISTS ' || quote_ident(r.typname) || ' CASCADE';
            END LOOP;
        END $$;
    `); err != nil {
		return fmt.Errorf("failed to drop schema objects: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit schema drop: %v", err)
	}

	// Reinitialize the database
	if err := db.initialize(ctx); err != nil {
		return fmt.Errorf("failed to reinitialize database: %v", err)
	}

	db.log.Info("Database reset completed successfully")
	return nil
}

// TextSearch performs a PostgreSQL full-text search on the content
func (db *DB) TextSearch(ctx context.Context, query string, limit int) ([]SearchResultWithMetadata, error) {
	// Convert the query to a tsquery format and escape special characters
	tsQuery := strings.Replace(query, "'", "''", -1)
	tsQuery = strings.Replace(tsQuery, " ", " & ", -1)

	sql := `
        SELECT 
            v.id,
            v.content,
            a.name as author,
            b.title,
            c.title as chapter,
            v.chunk_index,
            0.0 as similarity,
            c.index as chapter_index,
            c.title as chapter_title,
            (SELECT COUNT(*) FROM vectors WHERE chapter_id = c.id) as total_chunks,
            (SELECT COUNT(*) FROM chapters WHERE book_id = b.id) as total_chapters,
            COALESCE(t.file_path, '') as file_path,
            COALESCE(t.file_checksum, '') as file_checksum,
            CONCAT(a.name, '_', b.title, '_', c.index, '_', v.chunk_index) as chunk_id,
            LENGTH(v.content) - LENGTH(REPLACE(v.content, ' ', '')) + 1 as word_count,
            COALESCE(b.isbn, '') as isbn,
            COALESCE(b.asin, '') as asin
        FROM vectors v
        JOIN chapters c ON v.chapter_id = c.id
        JOIN books b ON c.book_id = b.id
        JOIN authors a ON b.author_id = a.id
        LEFT JOIN transcriptions t ON t.file_path LIKE '%' || b.title || '%'
        WHERE to_tsvector('english', v.content) @@ to_tsquery('english', $1)
        ORDER BY ts_rank(to_tsvector('english', v.content), to_tsquery('english', $1)) DESC
        LIMIT $2
    `

	rows, err := db.conn.Query(ctx, sql, tsQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("text search query failed: %w", err)
	}
	defer rows.Close()

	var results []SearchResultWithMetadata
	for rows.Next() {
		var result SearchResultWithMetadata
		err := rows.Scan(
			&result.ID,
			&result.Content,
			&result.Author,
			&result.Title,
			&result.Chapter,
			&result.ChunkIndex,
			&result.Similarity,
			&result.ChapterIndex,
			&result.ChapterTitle,
			&result.TotalChunks,
			&result.TotalChapters,
			&result.FilePath,
			&result.FileChecksum,
			&result.ChunkID,
			&result.WordCount,
			&result.ISBN,
			&result.ASIN,
		)
		if err != nil {
			return nil, fmt.Errorf("error scanning search result: %w", err)
		}
		results = append(results, result)
	}

	return results, nil
}

// CheckForMismatchedChunks finds books that have chunks with different sizes than current config
func (db *DB) CheckForMismatchedChunks(ctx context.Context, configuredChunkSize int) ([]meta.BookMetadata, error) {
	// Get all mismatched books in one query without a transaction
	rows, err := db.conn.Query(ctx, `
        WITH mismatched_chunks AS (
            SELECT DISTINCT c.book_id
            FROM vectors v
            JOIN chapters c ON v.chapter_id = c.id
            WHERE v.chunk_size != $1
        )
        SELECT DISTINCT
            b.id,
            b.title,
            b.isbn,
            b.asin,
            a.name as author
        FROM mismatched_chunks mc
        JOIN books b ON b.id = mc.book_id
        JOIN authors a ON a.id = b.author_id
    `, configuredChunkSize)
	if err != nil {
		return nil, fmt.Errorf("failed to check for mismatched chunks: %w", err)
	}
	defer rows.Close()

	var books []meta.BookMetadata
	for rows.Next() {
		var book meta.BookMetadata
		if err := rows.Scan(&book.ID, &book.Title, &book.ISBN, &book.ASIN, &book.Author); err != nil {
			return nil, fmt.Errorf("failed to scan book data: %w", err)
		}
		books = append(books, book)
	}

	// Now get chapters for each book in separate queries
	for i := range books {
		chapters, err := db.getChaptersForBook(ctx, books[i].ID)
		if err != nil {
			return nil, fmt.Errorf("failed to get chapters for book %d: %w", books[i].ID, err)
		}
		books[i].FileMetas = chapters
	}

	return books, nil
}

func (db *DB) getChaptersForBook(ctx context.Context, bookID int) ([]meta.FileMetadata, error) {
	rows, err := db.conn.Query(ctx, `
        SELECT c.id, c.title, c.index, b.title, b.isbn, b.asin, a.name
        FROM chapters c
        JOIN books b ON c.book_id = b.id
        JOIN authors a ON b.author_id = a.id
        WHERE c.book_id = $1
        ORDER BY c.index
    `, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chapters []meta.FileMetadata
	for rows.Next() {
		var chapter meta.FileMetadata
		if err := rows.Scan(
			&chapter.ID,
			&chapter.Chapter,
			&chapter.ChapterIndex,
			&chapter.Title,
			&chapter.ISBN,
			&chapter.ASIN,
			&chapter.Author,
		); err != nil {
			return nil, err
		}
		chapters = append(chapters, chapter)
	}

	return chapters, nil
}

func (db *DB) DeleteBookChunks(ctx context.Context, bookID int) error {
	_, err := db.conn.Exec(ctx, `
        DELETE FROM vectors
        WHERE chapter_id IN (
            SELECT id FROM chapters WHERE book_id = $1
        )
    `, bookID)
	return err
}

// ComputeFileChecksum calculates the SHA256 checksum of a file
func (db *DB) ComputeFileChecksum(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("failed to compute checksum: %w", err)
	}

	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// ComputeSettingsHash generates a hash from transcription settings for deduplication
func (db *DB) ComputeSettingsHash(cfg *config.Config) string {
	// Collect all transcription-relevant settings
	settings := map[string]string{
		"chunk_size": fmt.Sprintf("%d", cfg.ChunkSize),
	}

	// Create a deterministic string from settings
	var keys []string
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys) // Ensure consistent ordering

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, settings[k]))
	}

	settingsString := strings.Join(parts, "|")
	hash := sha256.Sum256([]byte(settingsString))
	return fmt.Sprintf("%x", hash[:8]) // Use first 8 bytes for shorter hash
}

// NeedsTranscription checks if a file needs to be transcribed based on checksum and settings
func (db *DB) NeedsTranscription(ctx context.Context, filePath string, fileChecksum, settingsHash string) (bool, error) {
	var exists bool
	err := db.conn.QueryRow(ctx, `
        SELECT EXISTS(
            SELECT 1 FROM transcriptions
            WHERE file_path = $1 AND file_checksum = $2 AND settings_hash = $3
        )
    `, filePath, fileChecksum, settingsHash).Scan(&exists)

	if err != nil {
		return false, fmt.Errorf("error checking transcription status: %w", err)
	}

	// Return true if transcription is needed (doesn't exist)
	return !exists, nil
}

// StoreTranscription stores raw transcription text and metadata in the database
func (db *DB) StoreTranscription(ctx context.Context, filePath, fileChecksum, settingsHash, transcriptionText string, fileSize int64, wordCount int, processingDurationMs int64) error {
	_, err := db.conn.Exec(ctx, `
        INSERT INTO transcriptions (
            file_path, file_checksum, file_size, settings_hash, 
            transcription_text, word_count, processing_duration_ms
        ) VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (file_path) DO UPDATE SET
            file_checksum = EXCLUDED.file_checksum,
            file_size = EXCLUDED.file_size,
            settings_hash = EXCLUDED.settings_hash,
            transcription_text = EXCLUDED.transcription_text,
            word_count = EXCLUDED.word_count,
            processing_duration_ms = EXCLUDED.processing_duration_ms,
            updated_at = NOW()
    `, filePath, fileChecksum, fileSize, settingsHash, transcriptionText, wordCount, processingDurationMs)

	if err != nil {
		return fmt.Errorf("failed to store transcription: %w", err)
	}

	db.log.Debug("Stored transcription", "file_path", filePath, "word_count", wordCount)
	return nil
}

// GetTranscription retrieves transcription data for a file
func (db *DB) GetTranscription(ctx context.Context, filePath string) (*Transcription, error) {
	var transcription Transcription
	err := db.conn.QueryRow(ctx, `
        SELECT id, file_path, file_checksum, file_size, settings_hash,
               transcription_text, corrected_text, correction_status, 
               correction_error, correction_metadata, word_count, 
               processing_duration_ms, created_at, updated_at
        FROM transcriptions
        WHERE file_path = $1
    `, filePath).Scan(
		&transcription.ID,
		&transcription.FilePath,
		&transcription.FileChecksum,
		&transcription.FileSize,
		&transcription.SettingsHash,
		&transcription.TranscriptionText,
		&transcription.CorrectedText,
		&transcription.CorrectionStatus,
		&transcription.CorrectionError,
		&transcription.CorrectionMetadata,
		&transcription.WordCount,
		&transcription.ProcessingDurationMs,
		&transcription.CreatedAt,
		&transcription.UpdatedAt,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to get transcription: %w", err)
	}

	return &transcription, nil
}

// UpdateCorrectedText updates an existing transcription with corrected text
func (db *DB) UpdateCorrectedText(ctx context.Context, filePath, correctedText string, metadata map[string]interface{}) error {
	var metadataJSON *string
	if metadata != nil {
		jsonBytes, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal correction metadata: %w", err)
		}
		jsonStr := string(jsonBytes)
		metadataJSON = &jsonStr
	}

	_, err := db.conn.Exec(ctx, `
        UPDATE transcriptions 
        SET corrected_text = $2,
            correction_status = 'completed',
            correction_error = NULL,
            correction_metadata = $3,
            updated_at = NOW()
        WHERE file_path = $1
    `, filePath, correctedText, metadataJSON)

	if err != nil {
		return fmt.Errorf("failed to update corrected text: %w", err)
	}

	db.log.Debug("Updated corrected text", "file_path", filePath)
	return nil
}

// UpdateCorrectionStatus updates the correction status for a transcription
func (db *DB) UpdateCorrectionStatus(ctx context.Context, filePath, status string, errorMsg *string) error {
	_, err := db.conn.Exec(ctx, `
        UPDATE transcriptions 
        SET correction_status = $2,
            correction_error = $3,
            updated_at = NOW()
        WHERE file_path = $1
    `, filePath, status, errorMsg)

	if err != nil {
		return fmt.Errorf("failed to update correction status: %w", err)
	}

	db.log.Debug("Updated correction status", "file_path", filePath, "status", status)
	return nil
}

// GetTranscriptionsForCorrection retrieves transcriptions that need LLM correction
func (db *DB) GetTranscriptionsForCorrection(ctx context.Context, limit int) ([]*Transcription, error) {
	rows, err := db.conn.Query(ctx, `
        SELECT id, file_path, file_checksum, file_size, settings_hash,
               transcription_text, corrected_text, correction_status,
               correction_error, correction_metadata, word_count,
               processing_duration_ms, created_at, updated_at
        FROM transcriptions
        WHERE correction_status = 'pending' OR corrected_text IS NULL
        ORDER BY created_at ASC
        LIMIT $1
    `, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query transcriptions for correction: %w", err)
	}
	defer rows.Close()

	var transcriptions []*Transcription
	for rows.Next() {
		var t Transcription
		err := rows.Scan(
			&t.ID,
			&t.FilePath,
			&t.FileChecksum,
			&t.FileSize,
			&t.SettingsHash,
			&t.TranscriptionText,
			&t.CorrectedText,
			&t.CorrectionStatus,
			&t.CorrectionError,
			&t.CorrectionMetadata,
			&t.WordCount,
			&t.ProcessingDurationMs,
			&t.CreatedAt,
			&t.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan transcription: %w", err)
		}
		transcriptions = append(transcriptions, &t)
	}

	return transcriptions, nil
}

// ProcessTranscriptionCorrection handles the entire correction process atomically
func (db *DB) ProcessTranscriptionCorrection(ctx context.Context, filePath string, correctionFunc func() (string, map[string]interface{}, error)) error {
	// Start transaction for atomic correction processing
	tx, err := db.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin correction transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Set status to in_progress
	_, err = tx.Exec(ctx, `
        UPDATE transcriptions 
        SET correction_status = 'in_progress',
            correction_error = NULL,
            updated_at = NOW()
        WHERE file_path = $1
    `, filePath)

	if err != nil {
		return fmt.Errorf("failed to update correction status: %w", err)
	}

	// Commit the status update first
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit status update: %w", err)
	}

	// Perform the correction (this may take a long time)
	correctedText, metadata, correctionErr := correctionFunc()

	// Start new transaction for final update
	tx, err = db.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin final transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if correctionErr != nil {
		// Update with error status
		errorMsg := correctionErr.Error()
		_, err = tx.Exec(ctx, `
            UPDATE transcriptions 
            SET correction_status = 'failed',
                correction_error = $2,
                updated_at = NOW()
            WHERE file_path = $1
        `, filePath, errorMsg)

		if err != nil {
			db.log.Error("Failed to update failed correction status", "error", err)
		} else {
			tx.Commit(ctx)
		}

		return fmt.Errorf("correction failed: %w", correctionErr)
	}

	// Update with successful correction
	var metadataJSON *string
	if metadata != nil {
		jsonBytes, err := json.Marshal(metadata)
		if err != nil {
			db.log.Warn("Failed to marshal correction metadata", "error", err)
		} else {
			jsonStr := string(jsonBytes)
			metadataJSON = &jsonStr
		}
	}

	_, err = tx.Exec(ctx, `
        UPDATE transcriptions 
        SET corrected_text = $2,
            correction_status = 'completed',
            correction_error = NULL,
            correction_metadata = $3,
            updated_at = NOW()
        WHERE file_path = $1
    `, filePath, correctedText, metadataJSON)

	if err != nil {
		return fmt.Errorf("failed to update corrected text: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit correction: %w", err)
	}

	db.log.Debug("Correction processed atomically", "file_path", filePath)
	return nil
}

// GetCorrectionStatus returns the current correction status for a file
func (db *DB) GetCorrectionStatus(ctx context.Context, filePath string) (status string, err error) {
	err = db.conn.QueryRow(ctx, `
        SELECT COALESCE(correction_status, 'pending')
        FROM transcriptions
        WHERE file_path = $1
    `, filePath).Scan(&status)

	if err != nil {
		return "", fmt.Errorf("failed to get correction status: %w", err)
	}

	return status, nil
}

// CleanupStaleCorrections resets any corrections that have been in_progress for too long
func (db *DB) CleanupStaleCorrections(ctx context.Context, timeoutMinutes int) (int, error) {
	result, err := db.conn.Exec(ctx, `
        UPDATE transcriptions 
        SET correction_status = 'pending',
            correction_error = 'Reset due to timeout',
            updated_at = NOW()
        WHERE correction_status = 'in_progress'
        AND updated_at < NOW() - INTERVAL '%d minutes'
    `, timeoutMinutes)

	if err != nil {
		return 0, fmt.Errorf("failed to cleanup stale corrections: %w", err)
	}

	rowsAffected := result.RowsAffected()
	if rowsAffected > 0 {
		db.log.Info("Cleaned up stale corrections", "count", rowsAffected)
	}

	return int(rowsAffected), nil
}

// GetChunkContext retrieves surrounding chunks for a given chunk ID
func (db *DB) GetChunkContext(ctx context.Context, chunkID string, contextWindow int) ([]SearchResultWithMetadata, error) {
	// Parse chunk ID (format: author_book_chapter_chunk)
	parts := strings.Split(chunkID, "_")
	if len(parts) < 4 {
		return nil, fmt.Errorf("invalid chunk ID format: %s", chunkID)
	}

	chapterIndex := parts[len(parts)-2]
	chunkIndex := parts[len(parts)-1]

	sql := `
        WITH target_chunk AS (
            SELECT 
                v.chapter_id,
                v.chunk_index,
                c.book_id
            FROM vectors v
            JOIN chapters c ON v.chapter_id = c.id
            JOIN books b ON c.book_id = b.id
            JOIN authors a ON b.author_id = a.id
            WHERE c.index = $1::integer 
            AND v.chunk_index = $2::integer
            AND CONCAT(a.name, '_', b.title, '_', c.index, '_', v.chunk_index) = $3
            LIMIT 1
        ),
        context_chunks AS (
            SELECT 
                v.id,
                v.content,
                v.chapter_id,
                v.chunk_index,
                0.0 as similarity
            FROM vectors v, target_chunk tc
            WHERE v.chapter_id = tc.chapter_id
            AND v.chunk_index BETWEEN (tc.chunk_index - $4) AND (tc.chunk_index + $4)
            ORDER BY v.chunk_index
        )
        SELECT 
            cc.id,
            cc.content,
            a.name as author,
            b.title,
            c.title as chapter,
            cc.chunk_index,
            cc.similarity,
            c.index as chapter_index,
            c.title as chapter_title,
            (SELECT COUNT(*) FROM vectors WHERE chapter_id = c.id) as total_chunks,
            (SELECT COUNT(*) FROM chapters WHERE book_id = b.id) as total_chapters,
            COALESCE(t.file_path, '') as file_path,
            COALESCE(t.file_checksum, '') as file_checksum,
            CONCAT(a.name, '_', b.title, '_', c.index, '_', cc.chunk_index) as chunk_id,
            LENGTH(cc.content) - LENGTH(REPLACE(cc.content, ' ', '')) + 1 as word_count,
            COALESCE(b.isbn, '') as isbn,
            COALESCE(b.asin, '') as asin
        FROM context_chunks cc
        JOIN chapters c ON cc.chapter_id = c.id
        JOIN books b ON c.book_id = b.id
        JOIN authors a ON b.author_id = a.id
        LEFT JOIN transcriptions t ON t.file_path LIKE '%' || b.title || '%'
        ORDER BY cc.chunk_index
    `

	rows, err := db.conn.Query(ctx, sql, chapterIndex, chunkIndex, chunkID, contextWindow)
	if err != nil {
		return nil, fmt.Errorf("context query failed: %w", err)
	}
	defer rows.Close()

	var results []SearchResultWithMetadata
	for rows.Next() {
		var result SearchResultWithMetadata
		err := rows.Scan(
			&result.ID,
			&result.Content,
			&result.Author,
			&result.Title,
			&result.Chapter,
			&result.ChunkIndex,
			&result.Similarity,
			&result.ChapterIndex,
			&result.ChapterTitle,
			&result.TotalChunks,
			&result.TotalChapters,
			&result.FilePath,
			&result.FileChecksum,
			&result.ChunkID,
			&result.WordCount,
			&result.ISBN,
			&result.ASIN,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan context result: %w", err)
		}
		results = append(results, result)
	}

	return results, nil
}
