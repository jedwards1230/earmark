package db

import (
	"context"
	"fmt"
	"log"
	"transcriber/internal/meta"

	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/sashabaranov/go-openai"
)

type VectorEntry struct {
	ID      int
	Vector  []float32
	Content string
}

type DB struct {
	pool *pgxpool.Pool
}

func New(host, user, password, dbName string) (*DB, error) {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:5432/%s", user, password, host, dbName)
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.ConnectConfig(context.Background(), config)
	if err != nil {
		return nil, err
	}

	db := &DB{pool: pool}
	if err := db.initialize(context.Background()); err != nil {
		pool.Close()
		return nil, err
	}

	return db, nil
}

func (db *DB) initialize(ctx context.Context) error {
	// Create the extension if it doesn't exist
	if _, err := db.pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("failed to create vector extension: %v", err)
	}

	// Create vectors table
	if _, err := db.pool.Exec(ctx, `
        CREATE TABLE IF NOT EXISTS vectors (
            id SERIAL PRIMARY KEY,
            embedding vector(1536),
            content TEXT NOT NULL
        )
    `); err != nil {
		return fmt.Errorf("failed to create vectors table: %v", err)
	}

	// Create metadata table with new fields
	if _, err := db.pool.Exec(ctx, `
        CREATE TABLE IF NOT EXISTS metadata (
            id SERIAL PRIMARY KEY,
            file_path TEXT NOT NULL,
            file_name TEXT NOT NULL DEFAULT '',
            author TEXT NOT NULL DEFAULT '',
            title TEXT NOT NULL DEFAULT '',
            chapter TEXT NOT NULL DEFAULT '',
            isbn TEXT NOT NULL DEFAULT '',
            vector_id INTEGER REFERENCES vectors(id),
            UNIQUE(file_path)
        )
    `); err != nil {
		return fmt.Errorf("failed to create metadata table: %v", err)
	}

	// Create index on vector_id
	if _, err := db.pool.Exec(ctx, `
        CREATE INDEX IF NOT EXISTS idx_metadata_vector_id ON metadata(vector_id)
    `); err != nil {
		return fmt.Errorf("failed to create index: %v", err)
	}

	return nil
}

func (db *DB) Store(ctx context.Context, vec []float32, content string) error {
	_, err := db.pool.Exec(ctx,
		"INSERT INTO vectors (embedding, content) VALUES ($1, $2)",
		vec, content)
	return err
}

func (db *DB) StoreWithMetadata(ctx context.Context, vec []float32, content string, meta *meta.FileMetadata) error {
	log.Printf("Storing metadata for file: %s", meta.FilePath)

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		log.Printf("Failed to begin transaction: %v", err)
		return err
	}
	defer tx.Rollback(ctx)

	var existingID int
	err = tx.QueryRow(ctx, "SELECT id FROM metadata WHERE file_path = $1", meta.FilePath).Scan(&existingID)
	if err == nil {
		log.Printf("Duplicate entry found for file: %s (ID: %d)", meta.FilePath, existingID)
		return fmt.Errorf("metadata for file_path '%s' already exists (ID: %d)", meta.FilePath, existingID)
	}

	var vectorID int
	err = tx.QueryRow(ctx,
		"INSERT INTO vectors (embedding, content) VALUES ($1, $2) RETURNING id",
		vec, content).Scan(&vectorID)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO metadata (file_path, file_name, author, title, chapter, isbn, vector_id) 
         VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		meta.FilePath, meta.FileName, meta.Author, meta.Title, meta.Chapter, meta.ISBN, vectorID)
	if err != nil {
		return err
	}

	log.Printf("Successfully stored metadata for file: %s", meta.FilePath)
	return tx.Commit(ctx)
}

func (db *DB) GetByID(ctx context.Context, id int) (*VectorEntry, error) {
	var entry VectorEntry
	err := db.pool.QueryRow(ctx,
		"SELECT id, embedding, content FROM vectors WHERE id = $1",
		id).Scan(&entry.ID, &entry.Vector, &entry.Content)
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

func (db *DB) GetMetadata(ctx context.Context, filePath string) (*meta.FileMetadata, error) {
	var meta meta.FileMetadata
	err := db.pool.QueryRow(ctx,
		`SELECT id, file_path, file_name, author, title, chapter, isbn, vector_id 
         FROM metadata WHERE file_path = $1`,
		filePath).Scan(&meta.ID, &meta.FilePath, &meta.FileName, &meta.Author,
		&meta.Title, &meta.Chapter, &meta.ISBN, &meta.VectorID)
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

func (db *DB) FindSimilar(ctx context.Context, vec []float32, limit int) ([]VectorEntry, error) {
	rows, err := db.pool.Query(ctx, `
        SELECT id, embedding, content 
        FROM vectors 
        ORDER BY embedding <-> $1 
        LIMIT $2
    `, vec, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []VectorEntry
	for rows.Next() {
		var entry VectorEntry
		if err := rows.Scan(&entry.ID, &entry.Vector, &entry.Content); err != nil {
			return nil, err
		}
		results = append(results, entry)
	}
	return results, nil
}

func (db *DB) Search(ctx context.Context, query string, limit int, apiKey string) ([]VectorEntry, error) {
	log.Printf("Performing search with query: %q (limit: %d)", query, limit)

	embedding, err := db.GetEmbedding(query, apiKey)
	if err != nil {
		log.Printf("Failed to get embedding: %v", err)
		return nil, err
	}

	results, err := db.FindSimilar(ctx, embedding, limit)
	if err != nil {
		log.Printf("Failed to find similar vectors: %v", err)
		return nil, err
	}

	log.Printf("Search completed. Found %d results", len(results))
	return results, nil
}

func (db *DB) Close() {
	db.pool.Close()
}

// GetEmbedding returns a vector embedding suitable for similarity search
func (db *DB) GetEmbedding(content string, apiKey string) ([]float32, error) {
	log.Printf("Requesting embedding from OpenAI (content length: %d)", len(content))

	client := openai.NewClient(apiKey)
	resp, err := client.CreateEmbeddings(
		context.Background(),
		openai.EmbeddingRequest{
			Input: []string{content},
			Model: openai.SmallEmbedding3,
		},
	)
	if err != nil {
		log.Printf("OpenAI API error: %v", err)
		return nil, fmt.Errorf("creating embedding: %v", err)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("no embeddings returned")
	}

	return resp.Data[0].Embedding, nil
}
