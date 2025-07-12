package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMCPCommandDefinition(t *testing.T) {
	// Test that the MCP command is properly defined
	assert.NotNil(t, mcpCmd, "MCP command should be defined")
	assert.Equal(t, "mcp", mcpCmd.Use, "Expected command use to be 'mcp'")
	assert.NotEmpty(t, mcpCmd.Short, "Expected command to have a short description")
	assert.NotEmpty(t, mcpCmd.Long, "Expected command to have a long description")
	assert.NotNil(t, mcpCmd.Run, "Expected command to have a run function")
}

func TestMCPCommandRegistration(t *testing.T) {
	// Test that the MCP command is registered with the root command
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Use == "mcp" {
			found = true
			break
		}
	}
	assert.True(t, found, "MCP command should be registered with root command")
}
