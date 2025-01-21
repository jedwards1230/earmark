package main

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
	"transcriber/internal/state"
	"transcriber/internal/worker"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	stateManager, err := state.NewStateManager(cfg.StateFile)
	if err != nil {
		log.Fatalf("Failed to initialize state manager: %v", err)
	}

	database, err := db.New(
		cfg.DBHost,
		cfg.DBUser,
		cfg.DBPassword,
		cfg.DBName,
	)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %v", err)
	}
	defer database.Close()

	workQueue := queue.NewQueue()
	worker := worker.NewWorker(workQueue, database)
	fileMonitor := monitor.NewFileMonitor(cfg, workQueue, stateManager)

	var wg sync.WaitGroup

	// Start services
	wg.Add(2)
	go func() {
		defer wg.Done()
		fileMonitor.Start()
	}()

	go func() {
		defer wg.Done()
		worker.Start(cfg, stateManager)
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
