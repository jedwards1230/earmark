package meta

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"transcriber/internal/meta/fetcher"
)

type FileMetadata struct {
	ID       int
	FilePath string
	FileName string
	Author   string
	Title    string
	Chapter  string
	ISBN     string
	VectorID int
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
	ISBN      string
	ASIN      string
	Title     string
	Author    string
	FileMetas []FileMetadata
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

	bookInfo, err := fetcher.GetBookMetadata(asin, true)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch ASIN metadata: %w", err)
	}

	return &BookMetadata{
		ASIN:   bookInfo.ASIN,
		Title:  bookInfo.Title,
		Author: bookInfo.Author,
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
