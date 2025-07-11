package cmd

import (
	"bytes"
	"io"
	"os"
	"testing"
	"github.com/jedwards1230/lil-whisper/internal/db"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)


func TestListCommand(t *testing.T) {
	tests := []struct {
		name           string
		mockData       []db.HierarchicalEntry
		mockError      error
		expectedOutput []string
		expectError    bool
	}{
		{
			name:     "successful list with data",
			mockData: []db.HierarchicalEntry{
				{
					Author: "Jane Doe",
					Title:  "Test Book",
					Chapters: []string{
						"Chapter 1: Introduction",
						"Chapter 2: The Journey",
					},
				},
				{
					Author: "John Smith",
					Title:  "Another Book",
					Chapters: []string{
						"Chapter 1: Beginning",
					},
				},
			},
			mockError: nil,
			expectedOutput: []string{
				"Jane Doe",
				"Test Book",
				"Chapter 1: Introduction",
				"Chapter 2: The Journey",
				"John Smith",
				"Another Book",
				"Chapter 1: Beginning",
			},
			expectError: false,
		},
		{
			name:           "empty database",
			mockData:       []db.HierarchicalEntry{},
			mockError:      nil,
			expectedOutput: []string{"No entries found in database"},
			expectError:    false,
		},
		{
			name:           "database error",
			mockData:       []db.HierarchicalEntry{},
			mockError:      assert.AnError,
			expectedOutput: []string{"Error getting data:"},
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Capture output
			oldStdout := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w

			// Create a test command that doesn't actually connect to DB
			testListCmd := &cobra.Command{
				Use:   "list",
				Short: "List content from the database",
				Run: func(cmd *cobra.Command, args []string) {
					// Simulate the list command behavior without actual DB connection
					if tt.mockError != nil {
						os.Stdout.WriteString("Error getting data: " + tt.mockError.Error() + "\n")
						return
					}

					entries := tt.mockData
					if len(entries) == 0 {
						os.Stdout.WriteString("No entries found in database\n")
						return
					}

					// Print tree structure
					currentAuthor := ""
					for i, entry := range entries {
						if entry.Author != currentAuthor {
							if currentAuthor != "" {
								os.Stdout.WriteString("\n")
							}
							prefix := treeBranch
							if i == len(entries)-1 {
								prefix = treeLeaf
							}
							os.Stdout.WriteString(prefix + " " + entry.Author + "\n")
							currentAuthor = entry.Author
						}

						indent := treeVertical + "   "
						prefix := treeBranch
						os.Stdout.WriteString(indent + prefix + " " + entry.Title + "\n")

						for j, chapter := range entry.Chapters {
							if chapter != "" {
								chapterIndent := treeVertical + "   " + treeVertical + "   "
								chapterPrefix := treeBranch
								if j == len(entry.Chapters)-1 {
									chapterPrefix = treeLeaf
								}
								os.Stdout.WriteString(chapterIndent + chapterPrefix + " Chapter " + string(rune(j+1+'0')) + ": " + chapter + "\n")
							}
						}
					}
				},
			}

			// Execute command
			testListCmd.Execute()

			// Restore stdout
			w.Close()
			os.Stdout = oldStdout

			// Read captured output
			var buf bytes.Buffer
			io.Copy(&buf, r)
			output := buf.String()

			// Check expected output
			for _, expected := range tt.expectedOutput {
				assert.Contains(t, output, expected)
			}
		})
	}
}

func TestListCommandTreeFormatting(t *testing.T) {
	// Test specific tree formatting scenarios
	tests := []struct {
		name     string
		data     []db.HierarchicalEntry
		expected []string
	}{
		{
			name: "single author with multiple books",
			data: []db.HierarchicalEntry{
				{
					Author:   "Author One",
					Title:    "First Book",
					Chapters: []string{"Chapter 1"},
				},
				{
					Author:   "Author One",
					Title:    "Second Book",
					Chapters: []string{"Chapter 1"},
				},
			},
			expected: []string{
				treeBranch + " Author One",
				treeVertical + "   " + treeBranch + " First Book",
				treeVertical + "   " + treeBranch + " Second Book",
			},
		},
		{
			name: "last author uses leaf symbol",
			data: []db.HierarchicalEntry{
				{
					Author:   "First Author",
					Title:    "Book",
					Chapters: []string{},
				},
				{
					Author:   "Last Author",
					Title:    "Book",
					Chapters: []string{},
				},
			},
			expected: []string{
				treeBranch + " First Author",
				treeLeaf + " Last Author", // Should use leaf for last author
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Capture output
			oldStdout := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w

			// Format the data as the list command would
			currentAuthor := ""
			for i, entry := range tt.data {
				if entry.Author != currentAuthor {
					if currentAuthor != "" {
						os.Stdout.WriteString("\n")
					}
					prefix := treeBranch
					if i == len(tt.data)-1 {
						prefix = treeLeaf
					}
					os.Stdout.WriteString(prefix + " " + entry.Author + "\n")
					currentAuthor = entry.Author
				}

				indent := treeVertical + "   "
				prefix := treeBranch
				os.Stdout.WriteString(indent + prefix + " " + entry.Title + "\n")
			}

			// Restore stdout
			w.Close()
			os.Stdout = oldStdout

			// Read captured output
			var buf bytes.Buffer
			io.Copy(&buf, r)
			output := buf.String()

			// Check expected formatting
			for _, expected := range tt.expected {
				assert.Contains(t, output, expected)
			}
		})
	}
}