package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"transcriber/internal/config"
	"transcriber/internal/db"
	"transcriber/internal/monitor"
	"transcriber/internal/queue"
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

	go fileMonitor.Start()
	go worker.Start(cfg, stateManager)

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")
	worker.Stop()
	log.Println("Shutdown complete")
}
