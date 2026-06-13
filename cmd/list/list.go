package list

import (
	"context"
	"fmt"
	"log"
	"path/filepath"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/spf13/cobra"
)

const (
	treeBranch = "├──"
	treeLeaf   = "└──"
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

	for i, entry := range entries {
		prefix := treeBranch
		if i == len(entries)-1 {
			prefix = treeLeaf
		}
		fmt.Printf("%s %s (%d chunks)\n", prefix, filepath.Base(entry.FilePath), entry.ChunkCount)
	}
}
