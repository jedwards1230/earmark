package serve

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/server"
	"github.com/spf13/cobra"
)

var ServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the HTTP API server",
	Long: `Start the HTTP API server that provides search endpoints for the
transcribed audiobook content.

This service handles:
- HTTP API endpoints for search functionality
- Vector similarity search using OpenAI embeddings
- Full-text search across transcriptions
- RESTful API responses

The serve command does NOT start file monitoring or transcription. Use the
'monitor' command to start the file monitoring and transcription service.`,
	Run: runServe,
}

func runServe(cmd *cobra.Command, args []string) {
	log.Println("Starting HTTP API server...")

	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	cfg.PrintEnvVars()

	database, err := db.New(cfg)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %v", err)
	}
	defer database.Close()

	// Search embeds queries with the embed step's effective model (CONTRACT
	// §2.20): follow the model_settings row, re-read every 30 s.
	// The first apply is synchronous, so no query is served with the env
	// embedder while an embed row is set; a failure is logged and search
	// keeps the env embedder (shown on the Models page).
	if _, err := database.ApplyEmbedSettings(context.Background(), database, cfg); err != nil {
		log.Printf("ERROR: embed model setting not applied; search uses the env embedder: %v", err)
	}
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go database.WatchEmbedSettings(watchCtx, database, cfg, db.ModelSettingsRefresh)

	// Initialize and start HTTP server
	srv := server.NewServer(database, cfg)
	httpServer := srv.Start()

	log.Println("HTTP server started and ready to serve requests...")

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Received shutdown signal, starting graceful shutdown...")

	// Shutdown HTTP server with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	// Close database connection
	database.Close()

	log.Println("HTTP server shutdown complete")
}
