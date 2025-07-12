package list

import (
	"context"
	"fmt"
	"log"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/spf13/cobra"
)

const (
	treeVertical = "│"
	treeBranch   = "├──"
	treeLeaf     = "└──"
)

var ListCmd = &cobra.Command{
	Use:   "list",
	Short: "List content from the database",
	Run:   runList,
}

func runList(cmd *cobra.Command, args []string) {
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

	entries, err := database.GetHierarchicalData(context.Background())
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
				fmt.Printf("%s%s Chapter %d: %s\n", chapterIndent, chapterPrefix, j+1, chapter)
			}
		}
	}
}