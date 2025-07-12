package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMCPCommandDefinition(t *testing.T) {
	// Test that the MCP command is properly defined
	assert.NotNil(t, MCPCmd, "MCP command should be defined")
	assert.Equal(t, "mcp", MCPCmd.Use, "Expected command use to be 'mcp'")
	assert.NotEmpty(t, MCPCmd.Short, "Expected command to have a short description")
	assert.NotEmpty(t, MCPCmd.Long, "Expected command to have a long description")
	assert.NotNil(t, MCPCmd.Run, "Expected command to have a run function")
}