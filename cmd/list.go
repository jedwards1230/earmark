package cmd

import (
	"fmt"
	"log"
	"transcriber/internal/config"

	"github.com/spf13/cobra"
)

const (
	treeVertical = "│"
	treeBranch   = "├──"
	treeLeaf     = "└──"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List content from the database",
	Run: func(cmd *cobra.Command, args []string) {
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

		entries, err := db.GetHierarchicalData(cmd.Context())
		if err != nil {
			fmt.Printf("Error getting data: %v\n", err)
			return
		}

		if len(entries) == 0 {
			fmt.Println("No entries found in database")
			return
		}

		// Print tree structure
		currentAuthor := ""
		for i, entry := range entries {
			if entry.Author != currentAuthor {
				if currentAuthor != "" {
					fmt.Println()
				}
				prefix := treeBranch
				if i == len(entries)-1 {
					prefix = treeLeaf
				}
				fmt.Printf("%s %s\n", prefix, entry.Author)
				currentAuthor = entry.Author
			}

			indent := fmt.Sprintf("%s   ", treeVertical)
			prefix := treeBranch
			fmt.Printf("%s%s %s\n", indent, prefix, entry.Title)

			for j, chapter := range entry.Chapters {
				if chapter != "" {
					chapterIndent := fmt.Sprintf("%s   %s   ", treeVertical, treeVertical)
					chapterPrefix := treeBranch
					if j == len(entry.Chapters)-1 {
						chapterPrefix = treeLeaf
					}
					fmt.Printf("%s%s %s\n", chapterIndent, chapterPrefix, chapter)
				}
			}
		}
	},
}
