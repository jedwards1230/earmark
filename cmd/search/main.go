package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
)

var (
	limit     = flag.Int("limit", 10, "maximum number of results to return")
	textMatch = flag.Bool("text", false, "use text-based search instead of semantic search")
	threshold = flag.Float64("precision", 0.3, "minimum similarity threshold (0..1)")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <query>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Search for content in the database. By default, uses semantic vector similarity search.\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	query := flag.Arg(0)

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

	if *textMatch {
		results, err := database.TextSearch(ctx, query, *limit)
		if err != nil {
			fmt.Printf("Error searching database: %v\n", err)
			return
		}
		printResults(results)
		return
	}

	if *threshold == 0 {
		*threshold = 0.3 // default threshold
	}

	results, err := database.Search(ctx, query, *limit, *threshold)
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

func printResults(results []db.SearchResultWithMetadata) {
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