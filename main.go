package main

import (
	"log"
	"os"

	"github.com/jedwards1230/earmark/cmd"
	"github.com/jedwards1230/earmark/cmd/backfill"
	"github.com/jedwards1230/earmark/cmd/eval"
	"github.com/jedwards1230/earmark/cmd/list"
	"github.com/jedwards1230/earmark/cmd/mcp"
	"github.com/jedwards1230/earmark/cmd/monitor"
	"github.com/jedwards1230/earmark/cmd/requeue"
	"github.com/jedwards1230/earmark/cmd/search"
	"github.com/jedwards1230/earmark/cmd/serve"
	"github.com/jedwards1230/earmark/cmd/update"
	"github.com/jedwards1230/earmark/cmd/version"
)

func main() {
	rootCmd := cmd.GetRootCmd()

	// Add all commands to root
	rootCmd.AddCommand(monitor.MonitorCmd)
	rootCmd.AddCommand(serve.ServeCmd)
	rootCmd.AddCommand(list.ListCmd)
	rootCmd.AddCommand(search.SearchCmd)
	rootCmd.AddCommand(requeue.RequeueCmd)
	rootCmd.AddCommand(eval.EvalCmd)
	rootCmd.AddCommand(backfill.BackfillCmd)
	rootCmd.AddCommand(mcp.MCPCmd)
	rootCmd.AddCommand(version.VersionCmd)
	rootCmd.AddCommand(update.UpdateCmd)

	if err := rootCmd.Execute(); err != nil {
		log.Printf("Error executing command: %v", err)
		os.Exit(1)
	}
}
