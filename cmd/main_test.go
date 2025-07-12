package cmd

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func TestRun(t *testing.T) {
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
			args:         []string{"lil-whisper"},
			expectError:  false,
			expectOutput: "A transcription service using Yap and MacOS native APIs",
		},
		{
			name:         "help flag",
			args:         []string{"lil-whisper", "--help"},
			expectError:  false,
			expectOutput: "A transcription service using Yap and MacOS native APIs",
		},
		{
			name:         "invalid command",
			args:         []string{"lil-whisper", "invalid"},
			expectError:  true,
			expectOutput: "unknown command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset cobra command for each test
			rootCmd = &cobra.Command{
				Use:   "lil-whisper",
				Short: "A transcription service using Yap and MacOS native APIs",
			}
			rootCmd.AddCommand(monitorCmd)
			rootCmd.AddCommand(serveCmd)
			rootCmd.AddCommand(listCmd)
			rootCmd.AddCommand(searchCmd)
			rootCmd.AddCommand(mcpCmd)

			// Capture output
			oldStdout := os.Stdout
			oldStderr := os.Stderr
			r, w, _ := os.Pipe()
			os.Stdout = w
			os.Stderr = w

			// Set args
			os.Args = tt.args
			rootCmd.SetArgs(tt.args[1:])

			// Execute
			err := rootCmd.Execute()

			// Restore stdout/stderr
			w.Close()
			os.Stdout = oldStdout
			os.Stderr = oldStderr

			// Read captured output
			var buf bytes.Buffer
			io.Copy(&buf, r)
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

func TestCommandsExist(t *testing.T) {
	// Reset cobra command
	rootCmd = &cobra.Command{
		Use:   "lil-whisper",
		Short: "A transcription service using Yap and MacOS native APIs",
	}
	rootCmd.AddCommand(monitorCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(mcpCmd)

	// Test that all expected commands are registered
	commands := []string{"monitor", "serve", "list", "search", "mcp"}

	for _, cmdName := range commands {
		cmd, _, err := rootCmd.Find([]string{cmdName})
		assert.NoError(t, err)
		assert.NotNil(t, cmd)
		assert.Equal(t, cmdName, cmd.Name())
	}
}
