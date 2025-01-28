package fetcher

import (
	"fmt"
	"transcriber/internal/log"

	goisbn "github.com/abx123/go-isbn"
	"github.com/bobbyrward/abs-importer/pkg/api/audible"
)

var logger = log.NewLogger("book-fetcher")

type BookInfo struct {
	Title       string
	Author      string
	Description string
	ISBN        string
	ASIN        string
}

func GetBookByASIN(asin string) (*BookInfo, error) {
	logger.Debug("Fetching book metadata for ASIN", "asin", asin)

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
	logger.Debug("Fetching book metadata for ISBN", "isbn", isbn)

	gi := goisbn.NewGoISBN([]string{
		goisbn.ProviderGoogle,
		goisbn.ProviderOpenLibrary,
	})
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
