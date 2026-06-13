package cmd

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRootCommand(t *testing.T) {
	// Test that the root command is set up correctly
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	tests := []struct {
		name         string
		args         []string
		expectError  bool
		expectOutput string
	}{
		{
			name:         "no arguments shows help",
			args:         []string{"earmark"},
			expectError:  false,
			expectOutput: "earmark: audiobook knowledge layer",
		},
		{
			name:         "help flag",
			args:         []string{"earmark", "--help"},
			expectError:  false,
			expectOutput: "earmark: audiobook knowledge layer",
		},
		{
			name:         "invalid command",
			args:         []string{"earmark", "invalid"},
			expectError:  false,
			expectOutput: "earmark: audiobook knowledge layer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset cobra command for each test
			testRootCmd := &cobra.Command{
				Use:   "earmark",
				Short: "earmark: audiobook knowledge layer — chunk, embed, and search transcripts",
			}

			// Capture output
			oldStdout := os.Stdout
			oldStderr := os.Stderr
			r, w, _ := os.Pipe()
			os.Stdout = w
			os.Stderr = w

			// Set args
			os.Args = tt.args
			testRootCmd.SetArgs(tt.args[1:])

			// Execute
			err := testRootCmd.Execute()

			// Restore stdout/stderr
			_ = w.Close()
			os.Stdout = oldStdout
			os.Stderr = oldStderr

			// Read captured output
			var buf bytes.Buffer
			_, copyErr := io.Copy(&buf, r)
			require.NoError(t, copyErr)
			output := buf.String()

			// Check expectations
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tt.expectOutput != "" {
				assert.Contains(t, output, tt.expectOutput)
			}
		})
	}
}

func TestGetRootCmd(t *testing.T) {
	// Test that GetRootCmd returns the correct root command
	cmd := GetRootCmd()
	assert.NotNil(t, cmd)
	assert.Equal(t, "earmark", cmd.Use)
	assert.Contains(t, cmd.Short, "earmark")
}
