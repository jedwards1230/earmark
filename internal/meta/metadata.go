package meta

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"transcriber/internal/log"
	"transcriber/internal/meta/fetcher"
)

var logger = log.NewLogger("metadata")

type FileMetadata struct {
	ID            int
	FilePath      string
	FileName      string
	Author        string
	Title         string
	ChapterIndex  int
	TotalChapters int
	Chapter       string
	ASIN          string
	ISBN          string
	VectorID      int
}

type ChapterInfo struct {
	Title         string `json:"title"`
	StartOffsetMs int64  `json:"start_offset_ms"`
	LengthMs      int64  `json:"length_ms"`
}

func NewMetadata(file_path, author, title, chapter, isbn string) *FileMetadata {
	return &FileMetadata{
		FilePath: file_path,
		FileName: filepath.Base(file_path),
		Author:   author,
		Title:    title,
		Chapter:  chapter,
		ISBN:     isbn,
	}
}

// BookMetadata represents common metadata across all formats
type BookMetadata struct {
	ID           int
	ISBN         string
	ASIN         string
	Title        string
	Author       string
	FileMetas    []FileMetadata
	ChaptersInfo []ChapterInfo `json:"chapters"`
}

// MetadataParser interface for different metadata formats
type MetadataParser interface {
	Parse(data []byte) (*BookMetadata, error)
}

func GetMetadataParsers() []MetadataParser {
	return []MetadataParser{
		&AudibleMetadataParser{},
		&StandardMetadataParser{},
	}
}

// AudibleMetadataParser for Audible's metadata format
type AudibleMetadataParser struct{}

func (p *AudibleMetadataParser) Parse(data []byte) (*BookMetadata, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		logger.Error("Error parsing Audible metadata", "error", err)
		return nil, err
	}

	asin, ok := raw["asin"].(string)
	if !ok {
		logger.Debug("No ASIN found in metadata file")
		return nil, fmt.Errorf("no ASIN found")
	}

	// Parse chapter info if available
	var chapters []ChapterInfo
	if chapterInfo, exists := raw["ChapterInfo"].(map[string]interface{}); exists {
		if chaptersList, ok := chapterInfo["chapters"].([]interface{}); ok {
			for _, ch := range chaptersList {
				if chMap, ok := ch.(map[string]interface{}); ok {
					chapter := ChapterInfo{
						Title:         chMap["title"].(string),
						StartOffsetMs: int64(chMap["start_offset_ms"].(float64)),
						LengthMs:      int64(chMap["length_ms"].(float64)),
					}
					chapters = append(chapters, chapter)
				}
			}
		}
	}

	bookInfo, err := fetcher.GetBookByASIN(asin)
	if err != nil {
		logger.Error("Failed to fetch Audible metadata", "asin", asin, "error", err)
		return nil, fmt.Errorf("failed to fetch ASIN metadata: %w", err)
	}

	return &BookMetadata{
		ASIN:         bookInfo.ASIN,
		Title:        bookInfo.Title,
		Author:       bookInfo.Author,
		ChaptersInfo: chapters,
	}, nil
}

// StandardMetadataParser for the ISBN-based format
type StandardMetadataParser struct{}

func (p *StandardMetadataParser) Parse(data []byte) (*BookMetadata, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		logger.Error("Error parsing standard metadata", "error", err)
		return nil, err
	}

	book, ok := raw["book"].(map[string]interface{})
	if !ok {
		logger.Error("No book data found in metadata")
		return nil, fmt.Errorf("no book data found")
	}

	title := book["title"].(string)
	authorsInterface := book["authors"].([]interface{})
	var authors []string
	for _, a := range authorsInterface {
		authors = append(authors, a.(string))
	}
	author := authors[0]

	// Handle ISBN which might be in scientific notation
	var isbn string
	switch v := book["isbn"].(type) {
	case string:
		isbn = v
	case float64:
		isbn = fmt.Sprintf("%.0f", v)
	default:
		logger.Error("ISBN has unexpected type", "type", fmt.Sprintf("%T", v))
		return nil, fmt.Errorf("invalid ISBN format")
	}

	if isbn == "" {
		logger.Debug("No ISBN found in metadata", "book", book)
		return nil, fmt.Errorf("no ISBN found")
	}

	logger.Debug("Found ISBN", "isbn", isbn)

	bookInfo, err := fetcher.GetBookByISBN(isbn)
	if err == nil {
		if title == "" && bookInfo.Title != "" {
			title = bookInfo.Title
		}

		if author == "" && bookInfo.Author != "" {
			author = bookInfo.Author
		}
	} else {
		logger.Error("Failed to fetch ISBN metadata", "isbn", isbn, "error", err)
	}

	return &BookMetadata{
		ISBN:   isbn,
		Title:  title,
		Author: author,
	}, nil
}
