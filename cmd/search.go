package cmd

import (
	"fmt"
	"log"
	"transcriber/internal/config"
	"transcriber/internal/db"

	"github.com/spf13/cobra"
)

var (
	limit     int
	textMatch bool
	threshold float64
)

var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search for content in the database",
	Long: `Search for content in the database. By default, uses semantic vector similarity search.
Use --text flag to switch to text-based fuzzy matching.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		query := args[0]

		cfg, err := config.LoadConfig()
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}

		db, err := initDB(cfg)
		if err != nil {
			fmt.Printf("Error connecting to database: %v\n", err)
			return
		}
		defer db.Close()

		if textMatch {
			results, err := db.SearchContent(cmd.Context(), query, limit)
			if err != nil {
				fmt.Printf("Error searching database: %v\n", err)
				return
			}
			printResults(results)
			return
		}

		// Vector similarity search
		vectors, err := db.Search(cmd.Context(), query, limit, threshold)
		if err != nil {
			fmt.Printf("Error searching database: %v\n", err)
			return
		}

		if len(vectors) == 0 {
			fmt.Println("No results found")
			return
		}

		fmt.Printf("Found %d results:\n\n", len(vectors))
		for i, result := range vectors {
			metadata, err := db.GetMetadataByVectorID(cmd.Context(), result.ID)
			if err != nil {
				fmt.Printf("Error getting metadata for result %d: %v\n", i+1, err)
				continue
			}
			fmt.Printf("%d. Author: %s\n", i+1, metadata.Author)
			fmt.Printf("   Title: %s\n", metadata.Title)
			if metadata.Chapter != "" {
				fmt.Printf("   Chapter: %s\n", metadata.Chapter)
			}
			fmt.Printf("   Content: %s\n\n", result.Content)
		}
	},
}

func init() {
	searchCmd.Flags().IntVarP(&limit, "limit", "k", 10, "maximum number of results to return")
	searchCmd.Flags().BoolVarP(&textMatch, "text", "t", false, "use text-based fuzzy matching instead of semantic search")
	searchCmd.Flags().Float64VarP(&threshold, "precision", "p", 0, "minimum similarity threshold (0..1)")
}

func printResults(results []db.SearchResult) {
	if len(results) == 0 {
		fmt.Println("No results found")
		return
	}

	fmt.Printf("Found %d results:\n\n", len(results))
	for i, result := range results {
		fmt.Printf("%d. Author: %s\n", i+1, result.Author)
		fmt.Printf("   Title: %s\n", result.Title)
		if result.Chapter != "" {
			fmt.Printf("   Chapter: %s\n", result.Chapter)
		}
		fmt.Printf("   Content: %s\n\n", result.Content)
	}
}
