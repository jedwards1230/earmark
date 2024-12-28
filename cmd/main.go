package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"transcriber/internal/config"
	"transcriber/internal/monitor"
	"transcriber/internal/queue"
	"transcriber/internal/state"
	"transcriber/internal/transcribe"
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

	workQueue := queue.NewQueue()
	transcriber := transcribe.NewTranscriber(cfg, stateManager, workQueue)
	fileMonitor := monitor.NewFileMonitor(cfg, workQueue, stateManager)

	go fileMonitor.Start()
	go transcriber.StartWorker()

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down...")

	log.Println("Shutdown complete")
}
