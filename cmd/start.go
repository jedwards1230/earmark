package cmd

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"transcriber/internal/config"
	"transcriber/internal/db"
	"transcriber/internal/monitor"
	"transcriber/internal/queue"
	"transcriber/internal/server"
	"transcriber/internal/worker"

	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the transcription service",
	Run: func(cmd *cobra.Command, args []string) {
		runService()
	},
}

func runService() {
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

	if cfg.ResetState {
		if err := database.Reset(context.Background()); err != nil {
			database.Close()
			log.Fatalf("Failed to reset database: %v", err)
		}
	}

	workQueue := queue.NewQueue()
	fileMonitor := monitor.NewFileMonitor(cfg, workQueue, database)
	worker := worker.NewWorker(workQueue, database)

	// Start monitor first and wait for initial scan to complete
	monitorReady := make(chan struct{})
	go func() {
		fileMonitor.Start(monitorReady)
	}()

	// Wait for monitor to complete initialization
	<-monitorReady

	// Then start worker and other services
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Start(cfg)
	}()

	// Initialize and start HTTP server
	srv := server.NewServer(database, cfg)
	httpServer := srv.Start()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Received shutdown signal, starting graceful shutdown...")

	// Stop all services
	fileMonitor.Stop()
	worker.Stop()

	// Shutdown HTTP server with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	// Wait for all goroutines to finish
	log.Println("Waiting for all tasks to complete...")
	wg.Wait()

	// Close database connection
	database.Close()

	log.Println("Graceful shutdown complete")
}
