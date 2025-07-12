package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestListCommandDefinition(t *testing.T) {
	// Test that the list command is properly defined
	assert.NotNil(t, listCmd, "List command should be defined")
	assert.Equal(t, "list", listCmd.Use, "List command use should be 'list'")
	assert.NotEmpty(t, listCmd.Short, "List command should have a short description")
	assert.NotNil(t, listCmd.Run, "List command should have a run function")
}

func TestListCommandStructure(t *testing.T) {
	// Test that the list command has the correct structure
	assert.Equal(t, "list", listCmd.Use)
	assert.Contains(t, listCmd.Short, "List")
	assert.NotNil(t, listCmd.Run)
}
