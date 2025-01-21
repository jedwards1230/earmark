package fetcher

import (
	"fmt"
	"log"

	goisbn "github.com/abx123/go-isbn"
	"github.com/bobbyrward/abs-importer/pkg/api/audible"
)

type BookInfo struct {
	Title       string
	Author      string
	Description string
	ISBN        string
	ASIN        string
}

func GetBookMetadata(identifier string, isASIN bool) (*BookInfo, error) {
	if isASIN {
		return GetBookByASIN(identifier)
	}
	return GetBookByISBN(identifier)
}

func GetBookByASIN(asin string) (*BookInfo, error) {
	log.Printf("Fetching book metadata for ASIN: %s", asin)

	book, err := audible.NewAudibleApiClient().GetMetadataFromAsin(asin)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch ASIN data: %w", err)
	}

	return &BookInfo{
		Title:       book.Title,
		Author:      book.Authors[0].Name,
		Description: book.Description,
		ASIN:        asin,
	}, nil
}

func GetBookByISBN(isbn string) (*BookInfo, error) {
	gi := goisbn.NewGoISBN(goisbn.DEFAULT_PROVIDERS)
	book, err := gi.Get(isbn)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch ISBN data: %w", err)
	}

	return &BookInfo{
		Title:       book.Title,
		Author:      book.Authors[0],
		Description: book.Description,
		ISBN:        isbn,
	}, nil
}
