package utils

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ParseFilePath extracts author, title, and chapter information from a filepath
func ParseFilePath(path string) (author, title string, chapterIndex int, chapterName string) {
	// Remove the base audiobooks directory
	parts := strings.Split(path, string(os.PathSeparator))
	if len(parts) < 3 {
		return "", "", 0, ""
	}

	// Author is typically the first directory after "audiobooks"
	for i, part := range parts {
		if part == "audiobooks" && i+1 < len(parts) {
			author = parts[i+1]
			break
		}
	}

	// Get the filename without extension
	filename := filepath.Base(path)
	ext := filepath.Ext(filename)
	filename = strings.TrimSuffix(filename, ext)

	// Check if the filename contains chapter information
	if parts := strings.Split(filename, " - "); len(parts) >= 3 {
		// Last two parts are chapter index and name
		chapterName = parts[len(parts)-1]
		chapterIndexStr := parts[len(parts)-2]
		if idx, err := strconv.Atoi(chapterIndexStr); err == nil {
			chapterIndex = idx
		}

		// Title is everything before the chapter parts, up to any ASIN/ISBN
		titlePart := strings.Join(parts[:len(parts)-2], " - ")
		if idx := strings.Index(titlePart, "["); idx != -1 {
			title = strings.TrimSpace(titlePart[:idx])
		} else {
			title = titlePart
		}
	} else {
		// Extract title up to the ASIN/ISBN if no chapter information is found
		if titleIdx := strings.LastIndex(path, author) + len(author) + 1; titleIdx < len(path) {
			title = path[titleIdx:]
			titleParts := strings.Split(title, string(os.PathSeparator))
			if len(titleParts) > 0 {
				title = titleParts[0]
				// Skip if the title is metadata.json
				if title == "metadata.json" {
					title = ""
				}
			}
			if idx := strings.Index(title, "["); idx != -1 {
				title = strings.TrimSpace(title[:idx])
			}
		}
	}

	return author, title, chapterIndex, chapterName
}
