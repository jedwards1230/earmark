package db

import (
	"context"
	"fmt"
	"log"
	"os"
	"transcriber/internal/config"
	"transcriber/internal/meta"
	"transcriber/internal/openai"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

type VectorEntry struct {
	ID      int
	Content string
}

type HierarchicalEntry struct {
	Author   string
	Title    string
	Chapters []string
}

type SearchResult struct {
	Author  string
	Title   string
	Chapter string
	Content string
}

type Statistics struct {
	ProcessedBooks    int
	ProcessedChapters int
}

type DB struct {
	conn *pgx.Conn
	e    *openai.Embeddings
}

type Author struct {
	ID   int
	Name string
}

type Book struct {
	ID       int
	AuthorID int
	Title    string
}

type ChapterData struct {
	ID     int
	BookID int
	Title  string
	Index  int
}

type ChunkEntry struct {
	ID         int
	ChapterID  int
	ChunkIndex int
	Content    string
}

type SearchResultWithMetadata struct {
	ID            int     `json:"id"`
	Content       string  `json:"content"`
	Author        string  `json:"author"`
	Title         string  `json:"title"`
	Chapter       string  `json:"chapter"`
	ChunkIndex    int     `json:"chunkIndex"`
	Similarity    float64 `json:"similarity"`
	ChapterIndex  int     `json:"chapterIndex"`
	ChapterTitle  string  `json:"chapterTitle"`
	TotalChunks   int     `json:"totalChunks"`
	TotalChapters int     `json:"totalChapters"`
}

func New(host, user, password, dbName string, cfg *config.Config) (*DB, error) {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:5432/%s", user, password, host, dbName)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		return nil, err
	}

	// Register pgvector types
	if err := pgxvector.RegisterTypes(context.Background(), conn); err != nil {
		conn.Close(context.Background())
		return nil, fmt.Errorf("failed to register vector types: %v", err)
	}

	db := &DB{
		conn: conn,
		e:    openai.NewEmbeddings(cfg),
	}

	// Reset the schema if RESET_STATE=true
	if os.Getenv("RESET_STATE") == "true" {
		log.Println("RESET_STATE=true, resetting DB")
		if err := db.Reset(context.Background()); err != nil {
			conn.Close(context.Background())
			return nil, fmt.Errorf("failed to reset DB: %v", err)
		}
	}

	if err := db.initialize(context.Background()); err != nil {
		conn.Close(context.Background())
		return nil, err
	}

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
            UNIQUE(chapter_id, chunk_index)
        );

        -- Standard indexes
        CREATE INDEX IF NOT EXISTS idx_authors_name ON authors(name);
        CREATE INDEX IF NOT EXISTS idx_books_title ON books(title);
        CREATE INDEX IF NOT EXISTS idx_chapters_book_id ON chapters(book_id);
        CREATE INDEX IF NOT EXISTS idx_vectors_chapter_id ON vectors(chapter_id);

        -- HNSW index for vector similarity
        CREATE INDEX IF NOT EXISTS vectors_embedding_idx 
            ON vectors USING hnsw (embedding vector_cosine_ops)
            WITH (m = 16, ef_construction = 64);
    `); err != nil {
		log.Printf("Warning: schema creation failed: %v", err)
		return fmt.Errorf("failed creating schema: %v", err)
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

	// Insert vector chunks
	allEmbeddings, err := db.e.GetEmbeddings(content)
	if err != nil {
		return fmt.Errorf("failed to generate embeddings: %v", err)
	}

	for i, emb := range allEmbeddings {
		_, err = tx.Exec(ctx, `
            INSERT INTO vectors (chapter_id, chunk_index, content, embedding)
            VALUES ($1, $2, $3, $4)
            ON CONFLICT (chapter_id, chunk_index) DO UPDATE 
            SET content = EXCLUDED.content, embedding = EXCLUDED.embedding
        `, chapterID, i, content, pgvector.NewVector(emb))
		if err != nil {
			return fmt.Errorf("failed to insert vector chunk: %v", err)
		}
	}

	return tx.Commit(ctx)
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
	var exists bool
	// Check if any vectors exist for this book/chapter combination
	err := db.conn.QueryRow(ctx, `
        SELECT EXISTS(
            SELECT 1 FROM vectors v
            JOIN chapters c ON v.chapter_id = c.id
            JOIN books b ON c.book_id = b.id
            WHERE b.title = $1
        )
    `, filePath).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (db *DB) Search(ctx context.Context, query string, limit int, threshold float64) ([]SearchResultWithMetadata, error) {
	log.Printf("Performing search with query: %q (limit: %d, threshold: %.2f)", query, limit, threshold)
	allEmbeddings, err := db.e.GetEmbeddings(query)
	if err != nil {
		log.Printf("Failed to get embedding: %v", err)
		return nil, err
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

func (db *DB) SearchContent(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	rows, err := db.conn.Query(ctx, `
        SELECT 
            a.name as author,
            b.title,
            c.title as chapter,
            v.content
        FROM vectors v
        JOIN chapters c ON v.chapter_id = c.id
        JOIN books b ON c.book_id = b.id
        JOIN authors a ON b.author_id = a.id
        WHERE v.content ILIKE $1
        ORDER BY a.name, b.title, c.title
        LIMIT $2
    `, "%"+query+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var result SearchResult
		if err := rows.Scan(&result.Author, &result.Title, &result.Chapter, &result.Content); err != nil {
			return nil, err
		}
		results = append(results, result)
	}

	return results, rows.Err()
}

func (db *DB) GetProcessingStats(ctx context.Context) (*Statistics, error) {
	stats := &Statistics{}
	err := db.conn.QueryRow(ctx, `
        SELECT 
            COUNT(DISTINCT b.id) as processed_books,
            COUNT(DISTINCT c.id) as processed_chapters
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
            (SELECT COUNT(*) FROM chapters WHERE book_id = b.id) as total_chapters
        FROM vector_matches vm
        JOIN chapters c ON c.id = vm.chapter_id
        JOIN books b ON c.book_id = b.id
        JOIN authors a ON b.author_id = a.id
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
		); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (db *DB) Reset(ctx context.Context) error {
	log.Println("Performing complete database reset...")

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

	log.Println("Database reset completed successfully")
	return nil
}

func (db *DB) InsertChunkWithMetadata(ctx context.Context, content string, authorName, bookTitle, chapterTitle string, chapterIndex, chunkIndex int) (int, error) {
	tx, err := db.conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// Insert or get author ID
	var authorID int
	if err := tx.QueryRow(ctx, `
		INSERT INTO authors (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id
	`, authorName).Scan(&authorID); err != nil {
		return 0, err
	}

	// Insert or get book ID
	var bookID int
	if err := tx.QueryRow(ctx, `
		INSERT INTO books (author_id, title) VALUES ($1, $2)
		ON CONFLICT (author_id, title) DO UPDATE SET title = EXCLUDED.title
		RETURNING id
	`, authorID, bookTitle).Scan(&bookID); err != nil {
		return 0, err
	}

	// Insert or get chapter ID
	var chapterID int
	if err := tx.QueryRow(ctx, `
		INSERT INTO chapters (book_id, title, index) VALUES ($1, $2, $3)
		ON CONFLICT (book_id, title, index) DO NOTHING
		RETURNING id
	`, bookID, chapterTitle, chapterIndex).Scan(&chapterID); err != nil && err != pgx.ErrNoRows {
		return 0, err
	}
	if chapterID == 0 {
		// If conflict, fetch existing ID
		if err := tx.QueryRow(ctx, `
			SELECT id FROM chapters WHERE book_id = $1 AND title = $2 AND index = $3
		`, bookID, chapterTitle, chapterIndex).Scan(&chapterID); err != nil {
			return 0, err
		}
	}

	allEmbeddings, err := db.e.GetEmbeddings(content)
	if err != nil {
		return 0, err
	}

	var lastVecID int
	for i, emb := range allEmbeddings {
		err = tx.QueryRow(ctx, `
			INSERT INTO vectors (embedding, content, chapter_id, chunk_index) 
			VALUES ($1, $2, $3, $4) RETURNING id
		`, pgvector.NewVector(emb), content, chapterID, i).Scan(&lastVecID)
		if err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return lastVecID, nil
}

// Example method to search chunks with optional metadata filters
func (db *DB) SearchChunksByMetadata(ctx context.Context, query string, authorFilter, bookTitleFilter string, limit int, threshold float64) ([]SearchResult, error) {
	// Step 1: Optionally apply filters to retrieve candidate vector IDs
	// For brevity, showing example logic:
	filterQuery := `
		SELECT v.id
		FROM vectors v
		JOIN metadata m ON v.metadata_id = m.id
		WHERE 1=1
	`
	args := []interface{}{}
	argIndex := 1

	if authorFilter != "" {
		filterQuery += fmt.Sprintf(" AND m.author = $%d", argIndex)
		args = append(args, authorFilter)
		argIndex++
	}
	if bookTitleFilter != "" {
		filterQuery += fmt.Sprintf(" AND m.title = $%d", argIndex)
		args = append(args, bookTitleFilter)
		argIndex++
	}

	// Now fetch all candidate IDs
	rows, err := db.conn.Query(ctx, filterQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidateIDs []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		candidateIDs = append(candidateIDs, id)
	}

	// Step 2: Perform the vector search across these candidates
	allEmbeddings, err := db.e.GetEmbeddings(query)
	if err != nil {
		return nil, fmt.Errorf("failed to get embedding: %v", err)
	}

	var fullResults []SearchResult
	for _, emb := range allEmbeddings {
		vectorArg := pgvector.NewVector(emb)

		// Example: refine the findSimilar query to limit to candidate IDs
		searchQuery := `
			SELECT v.id, v.content,
				   1 - (v.embedding <=> $1) AS similarity
			FROM vectors v
			WHERE v.id = ANY($2)
		`
		if threshold > 0 {
			searchQuery += " AND (1 - (v.embedding <=> $1)) >= $3"
			searchQuery += " ORDER BY v.embedding <=> $1 LIMIT $4"
		} else {
			searchQuery += " ORDER BY v.embedding <=> $1 LIMIT $3"
		}

		// Build search args
		searchArgs := []interface{}{vectorArg, candidateIDs, limit}
		if threshold > 0 {
			searchArgs = []interface{}{vectorArg, candidateIDs, threshold, limit}
		}

		sRows, err := db.conn.Query(ctx, searchQuery, searchArgs...)
		if err != nil {
			return nil, err
		}
		defer sRows.Close()

		// Collect partial results
		var partial []SearchResult
		for sRows.Next() {
			var entryID int
			var content string
			var sim float64
			if err := sRows.Scan(&entryID, &content, &sim); err != nil {
				return nil, err
			}

			// Step 3: Fetch metadata (join with authors/books/chapters as needed)
			var r SearchResult
			if err := db.conn.QueryRow(ctx, `
				SELECT au.name, bo.title, ch.title
				FROM vectors v
				JOIN chapters ch ON v.chapter_id = ch.id
				JOIN books bo ON ch.book_id = bo.id
				JOIN authors au ON bo.author_id = au.id
				WHERE v.id = $1
			`, entryID).Scan(&r.Author, &r.Title, &r.Chapter); err != nil {
				return nil, err
			}
			r.Content = content
			partial = append(partial, r)
		}
		fullResults = append(fullResults, partial...)
	}
	return fullResults, nil
}
