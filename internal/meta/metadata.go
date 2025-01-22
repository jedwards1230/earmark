package meta

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"transcriber/internal/meta/fetcher"
)

type FileMetadata struct {
	ID           int
	FilePath     string
	FileName     string
	Author       string
	Title        string
	ChapterIndex int    // renamed from Chapter
	Chapter      string // new field for chapter name
	ASIN         string
	ISBN         string
	VectorID     int
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
	ISBN         string
	ASIN         string
	Title        string
	Author       string
	FileMetas    []FileMetadata
	ChaptersInfo []ChapterInfo `json:"chapters"` // new field for chapter information
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
		return nil, err
	}

	asin, ok := raw["asin"].(string)
	if !ok {
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

	bookInfo, err := fetcher.GetBookMetadata(asin, true)
	if err != nil {
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
		return nil, err
	}

	book, ok := raw["book"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no book data found")
	}

	isbn, ok := book["isbn"].(string)
	if !ok {
		return nil, fmt.Errorf("no ISBN found")
	}

	bookInfo, err := fetcher.GetBookMetadata(isbn, false)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch ISBN metadata: %w", err)
	}

	return &BookMetadata{
		ISBN:   bookInfo.ISBN,
		Title:  bookInfo.Title,
		Author: bookInfo.Author,
	}, nil
}
