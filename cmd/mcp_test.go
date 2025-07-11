package cmd

import (
	"testing"
)

func TestMCPCommand(t *testing.T) {
	// Test that the MCP command is properly configured
	if mcpCmd.Use != "mcp" {
		t.Errorf("Expected command use to be 'mcp', got %s", mcpCmd.Use)
	}

	if mcpCmd.Short == "" {
		t.Error("Expected command to have a short description")
	}

	if mcpCmd.Long == "" {
		t.Error("Expected command to have a long description")
	}

	if mcpCmd.Run == nil {
		t.Error("Expected command to have a run function")
	}
}
