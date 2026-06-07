package mcp

import (
	"context"
	"testing"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
)

// SimpleMockDB implements DBInterface for simple testing
type SimpleMockDB struct{}

func (m *SimpleMockDB) Search(ctx context.Context, query string, limit int, threshold float64) ([]db.SearchResultWithMetadata, error) {
	return []db.SearchResultWithMetadata{
		{
			ID:         "chunk-1",
			Content:    "Test content about dragons",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 0,
			Similarity: 0.85,
		},
	}, nil
}

func (m *SimpleMockDB) TextSearch(ctx context.Context, query string, limit int) ([]db.SearchResultWithMetadata, error) {
	return []db.SearchResultWithMetadata{
		{
			ID:         "chunk-2",
			Content:    "Text search result about dragons",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 0,
		},
	}, nil
}

func (m *SimpleMockDB) GetHierarchicalData(ctx context.Context) ([]db.HierarchicalEntry, error) {
	return []db.HierarchicalEntry{
		{
			FilePath:   "/books/Christopher Paolini/Eragon/chapter1.mp3",
			ChunkCount: 42,
		},
	}, nil
}

func (m *SimpleMockDB) GetChunkContext(ctx context.Context, chunkID string, contextWindow int) ([]db.SearchResultWithMetadata, error) {
	return []db.SearchResultWithMetadata{
		{
			ID:         "chunk-3",
			Content:    "Previous chunk content",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 0,
			ChunkID:    "chunk-3",
		},
		{
			ID:         "chunk-4",
			Content:    "Current chunk content",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 1,
			ChunkID:    "chunk-4",
		},
		{
			ID:         "chunk-5",
			Content:    "Next chunk content",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 2,
			ChunkID:    "chunk-5",
		},
	}, nil
}

// TestMCPServerCreation tests that we can create an MCP server without errors
func TestMCPServerCreation(t *testing.T) {
	mockDB := &SimpleMockDB{}
	cfg := &config.Config{}

	server := NewMCPServer(mockDB, cfg)
	if server == nil {
		t.Fatal("Expected server to be created, got nil")
	}

	name, version := server.GetServerInfo()
	expectedName := "lilbro-whisper"
	expectedVersion := "1.0.0"

	if name != expectedName {
		t.Errorf("Expected server name %s, got %s", expectedName, name)
	}
	if version != expectedVersion {
		t.Errorf("Expected server version %s, got %s", expectedVersion, version)
	}

	err := server.Close()
	if err != nil {
		t.Errorf("Expected no error on close, got %v", err)
	}
}

// TestMCPServerTools tests that tools are properly registered
func TestMCPServerTools(t *testing.T) {
	mockDB := &SimpleMockDB{}
	cfg := &config.Config{}

	server := NewMCPServer(mockDB, cfg)
	if server == nil {
		t.Fatal("Expected server to be created, got nil")
	}

	if server.handlers == nil {
		t.Error("Expected handlers to be initialized")
	}

	if server.handlers.db == nil {
		t.Error("Expected database interface to be set in handlers")
	}
}

// TestStartMCPServiceConfiguration tests environment variable handling
func TestStartMCPServiceConfiguration(t *testing.T) {
	mockDB := &SimpleMockDB{}
	cfg := &config.Config{}

	t.Setenv("MCP_TRANSPORT", "unsupported")

	err := StartMCPService(mockDB, cfg)
	if err == nil {
		t.Error("Expected error for unsupported transport, got nil")
	}

	expectedError := "unsupported MCP transport: unsupported (use 'stdio' or 'http')"
	if err.Error() != expectedError {
		t.Errorf("Expected error %q, got %q", expectedError, err.Error())
	}
}
