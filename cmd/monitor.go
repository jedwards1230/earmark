package cmd

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/correction"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/monitor"
	"github.com/jedwards1230/lil-whisper/internal/queue"
	"github.com/jedwards1230/lil-whisper/internal/worker"
	"github.com/spf13/cobra"
)

func runMonitor(cmd *cobra.Command, args []string) {
	log.Println("Starting file monitoring and transcription service...")

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

	if cfg.DebugDBReset {
		log.Println("🚨 WARNING: DEBUG_DB_RESET=true - This will DESTROY ALL DATA!")
		log.Println("🚨 WARNING: Deleting all database tables and transcription text files...")
		
		// Clear all transcription text files
		fileManager := correction.NewFileManager(cfg)
		if err := fileManager.ClearAllFiles(); err != nil {
			log.Printf("Warning: Failed to clear transcription files: %v", err)
		}
		
		// Reset database
		if err := database.Reset(context.Background()); err != nil {
			database.Close()
			log.Fatalf("Failed to reset database: %v", err)
		}
		
		log.Println("✅ Debug reset completed - All data cleared")
	}

	workQueue := queue.NewQueue()
	fileMonitor := monitor.NewFileMonitor(cfg, workQueue, database)
	worker := worker.NewWorker(workQueue, database, cfg)

	// Start monitor first and wait for initial scan to complete
	monitorReady := make(chan struct{})
	go func() {
		fileMonitor.Start(monitorReady)
	}()

	// Wait for monitor to complete initialization
	<-monitorReady

	// Start worker
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Start(cfg)
	}()

	log.Println("Monitor service started. Processing files and transcribing...")

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Received shutdown signal, starting graceful shutdown...")

	// Stop all services
	fileMonitor.Stop()
	worker.Stop()

	// Wait for all goroutines to finish
	log.Println("Waiting for all tasks to complete...")
	wg.Wait()

	// Close database connection
	database.Close()

	log.Println("Monitor service shutdown complete")
}
