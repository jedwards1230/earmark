# MCP Server for lilbro-whisper

This directory contains the Model Context Protocol (MCP) server implementation for the lilbro-whisper audiobook transcription service.

## Overview

The MCP server provides three main tools for interacting with the audiobook transcription database:

1. **semantic_search_audiobooks** - Search using semantic similarity
2. **text_search_audiobooks** - Search using full-text search  
3. **browse_audiobook_library** - Browse the library structure

## Architecture

The MCP implementation follows the existing project patterns:

### Files

- `types.go` - Core formatting functions for MCP responses
- `tools.go` - Tool handlers that implement the MCP tool interface
- `server.go` - MCP server setup and configuration
- `*_test.go` - Comprehensive test coverage using TDD approach

### Design Principles

- **Interface-based**: Uses `DBInterface` for clean separation between MCP layer and database
- **DRY Architecture**: Shared formatting functions avoid code duplication
- **Testable**: All components have comprehensive test coverage
- **Error Handling**: Proper error handling with structured logging

## Usage

### Running the MCP Server

The MCP server is integrated as a Cobra command in the main lilbro-whisper CLI:

```bash
# Build the main binary
go build -o lil-whisper

# Run with stdio transport (default)
./lil-whisper mcp

# Run with HTTP transport
MCP_TRANSPORT=http ./lil-whisper mcp

# Run with custom HTTP address
MCP_TRANSPORT=http MCP_HTTP_ADDR=:9000 ./lil-whisper mcp

# Get help for the MCP command
./lil-whisper mcp --help
```

### Environment Variables

- `MCP_TRANSPORT` - Transport type: "stdio" (default) or "http"
- `MCP_HTTP_ADDR` - HTTP server address (default: ":8081")

### Integration with Existing Service

The MCP server can run alongside the existing HTTP server:

```go
// In your main service
import "github.com/jedwards1230/lil-whisper/internal/mcp"

// Start MCP service
go func() {
    if err := mcp.StartMCPService(database, cfg); err != nil {
        log.Errorf("MCP service failed: %v", err)
    }
}()
```

## Tools

### semantic_search_audiobooks

Search audiobook transcriptions using semantic similarity.

**Parameters:**
- `query` (string, required) - The search query
- `threshold` (number, optional, default: 0.3) - Similarity threshold (0.0-1.0)  
- `limit` (number, optional, default: 10) - Maximum results to return

**Example:**
```json
{
  "query": "dragons and magic",
  "threshold": 0.3,
  "limit": 5
}
```

### text_search_audiobooks

Search audiobook transcriptions using PostgreSQL full-text search.

**Parameters:**
- `query` (string, required) - The search query
- `limit` (number, optional, default: 10) - Maximum results to return

**Example:**
```json
{
  "query": "Eragon spoke to Saphira",
  "limit": 10
}
```

### browse_audiobook_library

Browse the audiobook library structure with optional filtering.

**Parameters:**
- `author` (string, optional) - Filter by author name (case-insensitive)
- `book` (string, optional) - Filter by book title (case-insensitive)

**Example:**
```json
{
  "author": "Paolini",
  "book": "Eragon"
}
```

## Testing

The MCP implementation includes comprehensive test coverage:

```bash
# Run all MCP tests
go test ./internal/mcp/...

# Run with verbose output
go test -v ./internal/mcp/...
```

### Test Coverage

- **types_test.go** - Tests all formatting functions
- **tools_test.go** - Tests tool handlers with mock database
- **server_test.go** - Tests server creation and configuration

## Development

### Adding New Tools

1. Add method to `DBInterface` in `tools.go`
2. Implement handler method in `ToolHandlers` struct
3. Add tool registration in `NewMCPServer` function
4. Add comprehensive tests

### Testing Approach

The project follows Test-Driven Development (TDD):

1. Write tests first
2. Use the project's `runTests` tool (not direct `go test`)
3. Mock external dependencies with interfaces
4. Test error conditions and edge cases

### Code Style

- Follow existing project conventions
- Use structured logging (`log.Logger`)
- Implement proper error handling
- Use descriptive variable names
- Add comprehensive documentation

## Integration with MCP Clients

The server can be used with any MCP-compatible client:

### Claude Desktop

Add to your MCP settings:

```json
{
  "mcpServers": {
    "lilbro-whisper": {
      "command": "/path/to/lil-whisper",
      "args": ["mcp"]
    }
  }
}
```

### HTTP Client

Connect to the HTTP endpoint:

```
GET http://localhost:8081/mcp
```

## Troubleshooting

### Common Issues

1. **Database Connection**: Ensure PostgreSQL is running and configured
2. **Environment Variables**: Check that required config is set
3. **Transport Issues**: Verify MCP_TRANSPORT is "stdio" or "http"

### Debugging

Enable debug logging:

```bash
DEBUG=true ./mcp-server
```

### Logs

The MCP server uses structured logging with the "mcp-server" and "mcp-tools" loggers.
