package cmd

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/spf13/cobra"
)

var (
	searchLimit     int
	textMatch       bool
	searchThreshold float64
)

func runSearch(cmd *cobra.Command, args []string) {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s search [options] <query>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Search for content in the database. By default, uses semantic vector similarity search.\n")
		os.Exit(1)
	}

	query := args[0]

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	database, err := db.New(cfg)
	if err != nil {
		fmt.Printf("Error connecting to database: %v\n", err)
		return
	}
	defer database.Close()

	ctx := context.Background()

	if textMatch {
		results, err := database.TextSearch(ctx, query, searchLimit)
		if err != nil {
			fmt.Printf("Error searching database: %v\n", err)
			return
		}
		printSearchResults(results)
		return
	}

	if searchThreshold == 0 {
		searchThreshold = 0.3 // default threshold
	}

	results, err := database.Search(ctx, query, searchLimit, searchThreshold)
	if err != nil {
		fmt.Printf("Error searching database: %v\n", err)
		return
	}
	if len(results) == 0 {
		fmt.Println("No results found")
		return
	}

	fmt.Printf("Found %d results:\n\n", len(results))
	for i, result := range results {
		metadata, err := database.GetMetadataByVectorID(ctx, result.ID)
		if err != nil {
			fmt.Printf("Error getting metadata for result %d: %v\n", i+1, err)
			continue
		}
		fmt.Printf("%d. Author: %s\n", i+1, metadata.Author)
		fmt.Printf("   Title: %s\n", metadata.Title)
		if metadata.Chapter != "" {
			fmt.Printf("   Chapter %d/%d: %s\n", result.ChapterIndex, result.TotalChapters, result.Chapter)
		}
		fmt.Printf("   Chunk %d/%d (similarity: %.2f)\n", result.ChunkIndex+1, result.TotalChunks, result.Similarity)
		fmt.Printf("   Content: %s\n\n", result.Content)
	}
}

func printSearchResults(results []db.SearchResultWithMetadata) {
	if len(results) == 0 {
		fmt.Println("No results found")
		return
	}

	fmt.Printf("Found %d results:\n\n", len(results))
	for i, result := range results {
		fmt.Printf("%d. Author: %s\n", i+1, result.Author)
		fmt.Printf("   Title: %s\n", result.Title)
		if result.Chapter != "" {
			fmt.Printf("   Chapter %d/%d: %s\n", result.ChapterIndex, result.TotalChapters, result.Chapter)
		}
		fmt.Printf("   Chunk %d/%d (similarity: %.2f)\n", result.ChunkIndex+1, result.TotalChunks, result.Similarity)
		fmt.Printf("   Content: %s\n\n", result.Content)
	}
}
