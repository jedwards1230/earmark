package list

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestListCommandDefinition(t *testing.T) {
	// Test that the list command is properly defined
	assert.NotNil(t, ListCmd, "List command should be defined")
	assert.Equal(t, "list", ListCmd.Use, "List command use should be 'list'")
	assert.NotEmpty(t, ListCmd.Short, "List command should have a short description")
	assert.NotNil(t, ListCmd.Run, "List command should have a run function")
}

func TestListCommandStructure(t *testing.T) {
	// Test that the list command has the correct structure
	assert.Equal(t, "list", ListCmd.Use)
	assert.Contains(t, ListCmd.Short, "List")
	assert.NotNil(t, ListCmd.Run)
}