package utils

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseFilePath(t *testing.T) {
	tests := []struct {
		name                string
		path                string
		expectedAuthor      string
		expectedTitle       string
		expectedChapterIdx  int
		expectedChapterName string
	}{
		{
			name:                "audible_format_with_chapter",
			path:                "/audiobooks/Christopher Paolini/Brisingr: The Inheritance Cycle, Book 3 [B002V0A444] - 01 - The Gates of Death.m4b",
			expectedAuthor:      "Christopher Paolini",
			expectedTitle:       "Brisingr: The Inheritance Cycle, Book 3",
			expectedChapterIdx:  1,
			expectedChapterName: "The Gates of Death",
		},
		{
			name:                "audible_format_chapter_2",
			path:                "/audiobooks/Christopher Paolini/Brisingr: The Inheritance Cycle, Book 3 [B002V0A444] - 02 - Around the Campfire.m4b",
			expectedAuthor:      "Christopher Paolini",
			expectedTitle:       "Brisingr: The Inheritance Cycle, Book 3",
			expectedChapterIdx:  2,
			expectedChapterName: "Around the Campfire",
		},
		{
			name:                "libro_format",
			path:                "/audiobooks/Andy Clark/The Experience Machine/Chapter_01.mp3",
			expectedAuthor:      "Andy Clark",
			expectedTitle:       "The Experience Machine",
			expectedChapterIdx:  0,
			expectedChapterName: "",
		},
		{
			name:                "simple_author_title",
			path:                "/audiobooks/Jane Doe/Simple Book/file.mp3",
			expectedAuthor:      "Jane Doe",
			expectedTitle:       "Simple Book",
			expectedChapterIdx:  0,
			expectedChapterName: "",
		},
		{
			name:                "no_audiobooks_in_path",
			path:                "/some/other/path/file.mp3",
			expectedAuthor:      "",
			expectedTitle:       "",
			expectedChapterIdx:  0,
			expectedChapterName: "",
		},
		{
			name:                "too_short_path",
			path:                "/short/path.mp3",
			expectedAuthor:      "",
			expectedTitle:       "",
			expectedChapterIdx:  0,
			expectedChapterName: "",
		},
		{
			name:                "metadata_json_file",
			path:                "/audiobooks/Author Name/metadata.json",
			expectedAuthor:      "Author Name",
			expectedTitle:       "",
			expectedChapterIdx:  0,
			expectedChapterName: "",
		},
		{
			name:                "windows_style_path_unix_separators",
			path:                "/audiobooks/J.K. Rowling/Harry Potter [B123456] - 01 - The Boy Who Lived.mp3",
			expectedAuthor:      "J.K. Rowling",
			expectedTitle:       "Harry Potter",
			expectedChapterIdx:  1,
			expectedChapterName: "The Boy Who Lived",
		},
		{
			name:                "complex_title_with_punctuation",
			path:                "/audiobooks/Douglas Adams/The Hitchhiker's Guide to the Galaxy [B789012] - 05 - So Long, and Thanks for All the Fish.m4a",
			expectedAuthor:      "Douglas Adams",
			expectedTitle:       "The Hitchhiker's Guide to the Galaxy",
			expectedChapterIdx:  5,
			expectedChapterName: "So Long, and Thanks for All the Fish",
		},
		{
			name:                "no_asin_brackets",
			path:                "/audiobooks/George Orwell/1984 - 03 - Big Brother.mp3",
			expectedAuthor:      "George Orwell",
			expectedTitle:       "1984",
			expectedChapterIdx:  3,
			expectedChapterName: "Big Brother",
		},
		{
			name:                "deeply_nested_path",
			path:                "/media/audiobooks/Brandon Sanderson/The Way of Kings [B456789] - 12 - The Weeping.m4b",
			expectedAuthor:      "Brandon Sanderson",
			expectedTitle:       "The Way of Kings",
			expectedChapterIdx:  12,
			expectedChapterName: "The Weeping",
		},
		{
			name:                "non_numeric_chapter",
			path:                "/audiobooks/Terry Pratchett/Discworld - Prologue - Introduction.mp3",
			expectedAuthor:      "Terry Pratchett",
			expectedTitle:       "Discworld",
			expectedChapterIdx:  0,
			expectedChapterName: "Introduction",
		},
		{
			name:                "empty_path",
			path:                "",
			expectedAuthor:      "",
			expectedTitle:       "",
			expectedChapterIdx:  0,
			expectedChapterName: "",
		},
		{
			name:                "just_filename",
			path:                "book.mp3",
			expectedAuthor:      "",
			expectedTitle:       "",
			expectedChapterIdx:  0,
			expectedChapterName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			author, title, chapterIdx, chapterName := ParseFilePath(tt.path)

			assert.Equal(t, tt.expectedAuthor, author, "Author mismatch")
			assert.Equal(t, tt.expectedTitle, title, "Title mismatch")
			assert.Equal(t, tt.expectedChapterIdx, chapterIdx, "Chapter index mismatch")
			assert.Equal(t, tt.expectedChapterName, chapterName, "Chapter name mismatch")
		})
	}
}

func TestParseFilePathEdgeCases(t *testing.T) {
	t.Run("path_with_multiple_audiobooks_directories", func(t *testing.T) {
		path := "/first/audiobooks/Christopher Paolini/Book [ID] - 01 - Chapter.mp3"
		author, title, chapterIdx, chapterName := ParseFilePath(path)

		// Should find the first occurrence of "audiobooks" and take the next directory
		assert.Equal(t, "Christopher Paolini", author)
		assert.Equal(t, "Book", title)
		assert.Equal(t, 1, chapterIdx)
		assert.Equal(t, "Chapter", chapterName)
	})

	t.Run("path_with_special_characters", func(t *testing.T) {
		path := "/audiobooks/Björk Guðmundsdóttir/Íslenska Bókin [ÍSL123] - 01 - Kafli Einn.mp3"
		author, title, chapterIdx, chapterName := ParseFilePath(path)

		assert.Equal(t, "Björk Guðmundsdóttir", author)
		assert.Equal(t, "Íslenska Bókin", title)
		assert.Equal(t, 1, chapterIdx)
		assert.Equal(t, "Kafli Einn", chapterName)
	})

	t.Run("very_long_chapter_numbers", func(t *testing.T) {
		path := "/audiobooks/Long Author/Long Title [ID] - 999 - Final Chapter.mp3"
		author, title, chapterIdx, chapterName := ParseFilePath(path)

		assert.Equal(t, "Long Author", author)
		assert.Equal(t, "Long Title", title)
		assert.Equal(t, 999, chapterIdx)
		assert.Equal(t, "Final Chapter", chapterName)
	})
}

func TestParseFilePathDifferentFormats(t *testing.T) {
	tests := []struct {
		name           string
		path           string
		expectedResult map[string]interface{}
	}{
		{
			name: "libation_format",
			path: "/audiobooks/libation/Christopher Paolini/Brisingr: The Inheritance Cycle, Book 3 [B002V0A444] - 01 - The Gates of Death.m4b",
			expectedResult: map[string]interface{}{
				"author":      "libation", // Takes the directory immediately after audiobooks
				"title":       "Brisingr: The Inheritance Cycle, Book 3",
				"chapterIdx":  1,
				"chapterName": "The Gates of Death",
			},
		},
		{
			name: "libro_format",
			path: "/audiobooks/libro/Andy Clark/The Experience Machine/Chapter_01.mp3",
			expectedResult: map[string]interface{}{
				"author":      "libro",      // Takes the directory immediately after audiobooks
				"title":       "Andy Clark", // Since the filename doesn't match chapter pattern
				"chapterIdx":  0,
				"chapterName": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			author, title, chapterIdx, chapterName := ParseFilePath(tt.path)

			assert.Equal(t, tt.expectedResult["author"], author)
			assert.Equal(t, tt.expectedResult["title"], title)
			assert.Equal(t, tt.expectedResult["chapterIdx"], chapterIdx)
			assert.Equal(t, tt.expectedResult["chapterName"], chapterName)
		})
	}
}

// Benchmark the ParseFilePath function
func BenchmarkParseFilePath(b *testing.B) {
	testPath := "/audiobooks/Christopher Paolini/Brisingr: The Inheritance Cycle, Book 3 [B002V0A444] - 01 - The Gates of Death.m4b"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseFilePath(testPath)
	}
}

func BenchmarkParseFilePathComplex(b *testing.B) {
	testPath := "/very/deep/nested/path/audiobooks/Very Long Author Name/Very Long Book Title With Lots of Words [VERYLONGASIN123456789] - 123 - Very Long Chapter Name With Many Words.m4b"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseFilePath(testPath)
	}
}

// Test with different path separators for cross-platform compatibility
func TestParseFilePathCrossPlatform(t *testing.T) {
	if filepath.Separator == '\\' {
		// Windows
		t.Run("windows_native_path", func(t *testing.T) {
			path := "C:\\audiobooks\\Author\\Book [ID] - 01 - Chapter.mp3"
			author, title, chapterIdx, chapterName := ParseFilePath(path)

			assert.Equal(t, "Author", author)
			assert.Equal(t, "Book", title)
			assert.Equal(t, 1, chapterIdx)
			assert.Equal(t, "Chapter", chapterName)
		})
	} else {
		// Unix-like systems
		t.Run("unix_native_path", func(t *testing.T) {
			path := "/audiobooks/Author/Book [ID] - 01 - Chapter.mp3"
			author, title, chapterIdx, chapterName := ParseFilePath(path)

			assert.Equal(t, "Author", author)
			assert.Equal(t, "Book", title)
			assert.Equal(t, 1, chapterIdx)
			assert.Equal(t, "Chapter", chapterName)
		})
	}
}
