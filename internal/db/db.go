package db

import (
	"context"
	"fmt"
	"log"
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

	if err := db.initialize(context.Background()); err != nil {
		conn.Close(context.Background())
		return nil, err
	}

	return db, nil
}

func (db *DB) initialize(ctx context.Context) error {
	// Create the extension if it doesn't exist
	if _, err := db.conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("failed to create vector extension: %v", err)
	}

	// Create vectors table with pgvector
	if _, err := db.conn.Exec(ctx, `
        CREATE TABLE IF NOT EXISTS vectors (
            id SERIAL PRIMARY KEY,
            embedding vector(1536),
            content TEXT NOT NULL
        )
    `); err != nil {
		return fmt.Errorf("failed to create vectors table: %v", err)
	}

	// Create HNSW index for faster similarity search
	if _, err := db.conn.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS vectors_embedding_idx 
		ON vectors USING hnsw (embedding vector_cosine_ops)
	`); err != nil {
		log.Printf("Warning: failed to create HNSW index: %v", err)
	}

	// Add asin column to metadata table if it doesn't exist
	if _, err := db.conn.Exec(ctx, `
        DO $$ 
        BEGIN
            IF NOT EXISTS (
                SELECT 1 
                FROM information_schema.columns 
                WHERE table_name = 'metadata' 
                AND column_name = 'asin'
            ) THEN
                ALTER TABLE metadata ADD COLUMN asin TEXT DEFAULT '';
            END IF;
        END $$;
    `); err != nil {
		return fmt.Errorf("failed to add asin column: %v", err)
	}

	// Create metadata table with all necessary fields
	if _, err := db.conn.Exec(ctx, `
        CREATE TABLE IF NOT EXISTS metadata (
            id SERIAL PRIMARY KEY,
            file_path TEXT NOT NULL,
            file_name TEXT NOT NULL DEFAULT '',
            author TEXT NOT NULL DEFAULT '',
            title TEXT NOT NULL DEFAULT '',
            chapter TEXT NOT NULL DEFAULT '',
            isbn TEXT NOT NULL DEFAULT '',
            asin TEXT NOT NULL DEFAULT '',
            vector_id INTEGER REFERENCES vectors(id),
            UNIQUE(file_path)
        )
    `); err != nil {
		return fmt.Errorf("failed to create metadata table: %v", err)
	}

	// Create index on relevant metadata fields for optimized queries
	if _, err := db.conn.Exec(ctx, `
        CREATE INDEX IF NOT EXISTS idx_metadata_author ON metadata(author);
        CREATE INDEX IF NOT EXISTS idx_metadata_title ON metadata(title);
        CREATE INDEX IF NOT EXISTS idx_metadata_isbn ON metadata(isbn);
        CREATE INDEX IF NOT EXISTS idx_metadata_asin ON metadata(asin);
    `); err != nil {
		return fmt.Errorf("failed to create indexes: %v", err)
	}

	return nil
}

func (db *DB) InsertContentWithMetadata(ctx context.Context, content string, meta *meta.FileMetadata) error {
	log.Printf("Storing metadata for file: %s", meta.FilePath)

	tx, err := db.conn.Begin(ctx)
	if err != nil {
		log.Printf("Failed to begin transaction: %v", err)
		return err
	}
	defer tx.Rollback(ctx)

	log.Println("Checking for duplicate metadata entry")
	var existingID int
	err = tx.QueryRow(ctx, "SELECT id FROM metadata WHERE file_path = $1", meta.FilePath).Scan(&existingID)
	if err == nil {
		log.Printf("Duplicate entry found for file: %s (ID: %d)", meta.FilePath, existingID)
		return fmt.Errorf("metadata for file_path '%s' already exists (ID: %d)", meta.FilePath, existingID)
	}

	log.Println("Storing content vector")
	vectorID, err := db.insertContent(ctx, content)
	if err != nil {
		return err
	}

	log.Println("Storing metadata")
	_, err = tx.Exec(ctx,
		`INSERT INTO metadata (file_path, file_name, author, title, chapter, isbn, asin, vector_id) 
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		meta.FilePath, meta.FileName, meta.Author, meta.Title, meta.Chapter, meta.ISBN, meta.ASIN, vectorID)
	if err != nil {
		return err
	}

	log.Printf("Successfully stored metadata for file: %s", meta.FilePath)
	return tx.Commit(ctx)
}

func (db *DB) GetMetadataByVectorID(ctx context.Context, vectorID int) (*meta.FileMetadata, error) {
	var meta meta.FileMetadata
	err := db.conn.QueryRow(ctx, `
		SELECT id, file_path, file_name, author, title, chapter, isbn, vector_id 
		FROM metadata 
		WHERE vector_id = $1`,
		vectorID).Scan(&meta.ID, &meta.FilePath, &meta.FileName, &meta.Author,
		&meta.Title, &meta.Chapter, &meta.ISBN, &meta.VectorID)
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

func (db *DB) Search(ctx context.Context, query string, limit int, threshold float64) ([]VectorEntry, error) {
	log.Printf("Performing search with query: %q (limit: %d, threshold: %.2f)", query, limit, threshold)

	embedding, err := db.e.GetEmbedding(query)
	if err != nil {
		log.Printf("Failed to get embedding: %v", err)
		return nil, err
	}

	results, err := db.findSimilar(ctx, embedding, limit, threshold)
	if err != nil {
		log.Printf("Failed to find similar vectors: %v", err)
		return nil, err
	}

	log.Printf("Search completed. Found %d results", len(results))
	return results, nil
}

func (db *DB) GetHierarchicalData(ctx context.Context) ([]HierarchicalEntry, error) {
	rows, err := db.conn.Query(ctx, `
		SELECT author, title, array_agg(chapter ORDER BY chapter) as chapters
		FROM metadata 
		WHERE author != '' 
		GROUP BY author, title 
		ORDER BY author, title
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

func (db *DB) IsProcessed(ctx context.Context, filePath string) (bool, error) {
	var vectorID int
	err := db.conn.QueryRow(ctx,
		"SELECT vector_id FROM metadata WHERE file_path = $1",
		filePath).Scan(&vectorID)
	if err != nil {
		// No row found or other error
		return false, nil
	}
	return vectorID != 0, nil
}

func (db *DB) SearchContent(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	rows, err := db.conn.Query(ctx, `
		SELECT m.author, m.title, m.chapter, v.content
		FROM metadata m
		JOIN vectors v ON m.vector_id = v.id
		WHERE v.content ILIKE $1
		ORDER BY m.author, m.title, m.chapter
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
	processedBookMap := make(map[string]bool)

	rows, err := db.conn.Query(ctx, `
		SELECT DISTINCT author, title, COUNT(*) as chapter_count
		FROM metadata
		WHERE vector_id IS NOT NULL
		GROUP BY author, title
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var author, title string
		var chapterCount int
		if err := rows.Scan(&author, &title, &chapterCount); err != nil {
			return nil, err
		}
		bookKey := author + "|" + title
		if !processedBookMap[bookKey] {
			processedBookMap[bookKey] = true
			stats.ProcessedBooks++
		}
		stats.ProcessedChapters += chapterCount
	}

	return stats, rows.Err()
}

func (db *DB) insertContent(ctx context.Context, content string) (int, error) {
	embedding, err := db.e.GetEmbedding(content)
	if err != nil {
		return 0, fmt.Errorf("failed to generate embedding: %v", err)
	}

	var id int
	err = db.conn.QueryRow(ctx,
		"INSERT INTO vectors (embedding, content) VALUES ($1, $2) RETURNING id",
		pgvector.NewVector(embedding), content).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (db *DB) findSimilar(ctx context.Context, vec []float32, limit int, threshold float64) ([]VectorEntry, error) {
	var query string
	if threshold == 0 {
		query = `
			SELECT id, content,
				   1 - (embedding <=> $1) as similarity
			FROM vectors 
			ORDER BY embedding <=> $1
			LIMIT $2
		`
	} else {
		query = `
			SELECT id, content,
				   1 - (embedding <=> $1) as similarity
			FROM vectors 
			WHERE 1 - (embedding <=> $1) >= $3
			ORDER BY embedding <=> $1
			LIMIT $2
		`
	}

	var rows pgx.Rows
	var err error
	vectorArg := pgvector.NewVector(vec)

	if threshold == 0 {
		rows, err = db.conn.Query(ctx, query, vectorArg, limit)
	} else {
		rows, err = db.conn.Query(ctx, query, vectorArg, limit, threshold)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []VectorEntry
	for rows.Next() {
		var entry VectorEntry
		var similarity float64
		if err := rows.Scan(&entry.ID, &entry.Content, &similarity); err != nil {
			return nil, err
		}
		results = append(results, entry)
	}
	return results, nil
}

func (db *DB) Reset(ctx context.Context) error {
	fmt.Println("Resetting database state...")
	// Start a transaction
	tx, err := db.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback(ctx)

	// Truncate all tables in the correct order to respect foreign keys
	if _, err := tx.Exec(ctx, `
		TRUNCATE TABLE metadata, vectors CASCADE
	`); err != nil {
		return fmt.Errorf("failed to truncate tables: %v", err)
	}

	// Reset the sequence for serial columns
	if _, err := tx.Exec(ctx, `
		ALTER SEQUENCE metadata_id_seq RESTART WITH 1;
		ALTER SEQUENCE vectors_id_seq RESTART WITH 1;
	`); err != nil {
		return fmt.Errorf("failed to reset sequences: %v", err)
	}

	fmt.Println("Database state reset successfully")
	return tx.Commit(ctx)
}
