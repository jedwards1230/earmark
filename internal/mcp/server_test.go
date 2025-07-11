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
			ID:         1,
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
			ID:         1,
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
			Author:   "Christopher Paolini",
			Title:    "Eragon",
			Chapters: []string{"Chapter 1", "Chapter 2"},
		},
	}, nil
}

func (m *SimpleMockDB) GetChunkContext(ctx context.Context, chunkID string, contextWindow int) ([]db.SearchResultWithMetadata, error) {
	return []db.SearchResultWithMetadata{
		{
			ID:         1,
			Content:    "Previous chunk content",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 0,
			ChunkID:    "Christopher Paolini_Eragon_1_0",
		},
		{
			ID:         2,
			Content:    "Current chunk content",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 1,
			ChunkID:    "Christopher Paolini_Eragon_1_1",
		},
		{
			ID:         3,
			Content:    "Next chunk content",
			Author:     "Christopher Paolini",
			Title:      "Eragon",
			Chapter:    "Chapter 1",
			ChunkIndex: 2,
			ChunkID:    "Christopher Paolini_Eragon_1_2",
		},
	}, nil
}

// TestMCPServerCreation tests that we can create an MCP server without errors
func TestMCPServerCreation(t *testing.T) {
	// Create simple mock database
	mockDB := &SimpleMockDB{}

	// Create config
	cfg := &config.Config{}

	// Test server creation
	server := NewMCPServer(mockDB, cfg)
	if server == nil {
		t.Fatal("Expected server to be created, got nil")
	}

	// Test server info
	name, version := server.GetServerInfo()
	expectedName := "lilbro-whisper"
	expectedVersion := "1.0.0"

	if name != expectedName {
		t.Errorf("Expected server name %s, got %s", expectedName, name)
	}
	if version != expectedVersion {
		t.Errorf("Expected server version %s, got %s", expectedVersion, version)
	}

	// Test graceful shutdown
	err := server.Close()
	if err != nil {
		t.Errorf("Expected no error on close, got %v", err)
	}
}

// TestMCPServerTools tests that tools are properly registered
func TestMCPServerTools(t *testing.T) {
	// Create simple mock database
	mockDB := &SimpleMockDB{}

	// Create config
	cfg := &config.Config{}

	// Test server creation
	server := NewMCPServer(mockDB, cfg)
	if server == nil {
		t.Fatal("Expected server to be created, got nil")
	}

	// Verify that the server has handlers
	if server.handlers == nil {
		t.Error("Expected handlers to be initialized")
	}

	// Verify database interface is set
	if server.handlers.db == nil {
		t.Error("Expected database interface to be set in handlers")
	}
}

// TestStartMCPServiceConfiguration tests environment variable handling
func TestStartMCPServiceConfiguration(t *testing.T) {
	// This test verifies the configuration logic without actually starting servers
	// We'll test the error case for unsupported transport

	// Create simple mock database
	mockDB := &SimpleMockDB{}
	cfg := &config.Config{}

	// Set an unsupported transport
	t.Setenv("MCP_TRANSPORT", "unsupported")

	// This should return an error
	err := StartMCPService(mockDB, cfg)
	if err == nil {
		t.Error("Expected error for unsupported transport, got nil")
	}

	expectedError := "unsupported MCP transport: unsupported (use 'stdio' or 'http')"
	if err.Error() != expectedError {
		t.Errorf("Expected error %q, got %q", expectedError, err.Error())
	}
}
