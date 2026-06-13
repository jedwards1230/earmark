package search

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/spf13/cobra"
)

var (
	searchLimit     int
	textMatch       bool
	searchThreshold float64
)

var SearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search for content in the database",
	Long:  `Search for content in the database. By default, uses semantic vector similarity search.`,
	Run:   runSearch,
}

func init() {
	SearchCmd.Flags().IntVarP(&searchLimit, "limit", "l", 10, "maximum number of results to return")
	SearchCmd.Flags().BoolVarP(&textMatch, "text", "t", false, "use text-based search instead of semantic search")
	SearchCmd.Flags().Float64VarP(&searchThreshold, "precision", "p", 0.3, "minimum similarity threshold (0..1)")
}

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
		searchThreshold = 0.3
	}

	results, err := database.Search(ctx, query, searchLimit, searchThreshold)
	if err != nil {
		fmt.Printf("Error searching database: %v\n", err)
		return
	}
	printSearchResults(results)
}

func printSearchResults(results []db.SearchResultWithMetadata) {
	if len(results) == 0 {
		fmt.Println("No results found")
		return
	}

	fmt.Printf("Found %d results:\n\n", len(results))
	for i, result := range results {
		fmt.Printf("%d. File: %s\n", i+1, result.FilePath)
		if result.Author != "" {
			fmt.Printf("   Author: %s\n", result.Author)
		}
		if result.Title != "" {
			fmt.Printf("   Title: %s\n", result.Title)
		}
		fmt.Printf("   Chunk %d/%d (similarity: %.2f)\n", result.ChunkIndex+1, result.TotalChunks, result.Similarity)
		if result.StartSec > 0 || result.EndSec > 0 {
			fmt.Printf("   Time: %.1fs – %.1fs\n", result.StartSec, result.EndSec)
		}
		fmt.Printf("   Content: %s\n\n", result.Content)
	}
}
