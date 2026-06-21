package monitor

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jedwards1230/earmark/internal/config"
	"github.com/jedwards1230/earmark/internal/db"
	"github.com/jedwards1230/earmark/internal/ingesthttp"
	"github.com/jedwards1230/earmark/internal/metaprovider"
	"github.com/jedwards1230/earmark/internal/monitor"
	"github.com/jedwards1230/earmark/internal/queue"
	"github.com/jedwards1230/earmark/internal/worker"
	"github.com/spf13/cobra"
)

var MonitorCmd = &cobra.Command{
	Use:   "monitor",
	Short: "Start the file monitoring and transcription service",
	Long: `Start the file monitoring service that watches for new audio files,
enqueues them for transcription by the external ASR runner (NeMo Parakeet), and
embeds completed transcripts into pgvector for semantic search.

The monitor service does NOT start the HTTP server. Use the 'serve' command
to start the HTTP API server separately.`,
	Run: runMonitor,
}

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
	defer database.Close() // single close via defer; no explicit calls below

	if cfg.DebugDBReset {
		log.Println("WARNING: DEBUG_DB_RESET=true - This will DESTROY ALL DATA!")
		if err := database.Reset(context.Background()); err != nil {
			log.Fatalf("Failed to reset database: %v", err)
		}
		log.Println("Debug reset completed - All data cleared")
	}

	meta := metaprovider.New(cfg)

	workQueue := queue.NewQueue()
	fileMonitor := monitor.NewFileMonitor(cfg, database, meta)
	w := worker.NewWorker(workQueue, database, cfg)

	// Minimal HTTP listener for the ingest pod: /healthz (liveness) — and, in a
	// later phase, /metrics. The ingest process has no MCP server, so this is its
	// only HTTP surface (replaces the broken `pgrep` liveness probe). A bind
	// failure is logged but non-fatal — the worker/monitor are the real work and
	// must not be blocked by a probe-only port being unavailable.
	ingestSrv := ingesthttp.New(cfg.IngestHTTPAddr, nil)
	go func() {
		if err := ingestSrv.Start(); err != nil {
			log.Printf("ingest HTTP listener error: %v", err)
		}
	}()

	// Start monitor first and wait for initial scan to complete.
	monitorReady := make(chan struct{})
	go func() {
		fileMonitor.Start(monitorReady)
	}()

	// Wait for monitor to complete initialization.
	<-monitorReady

	// Start worker.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Start(cfg)
	}()

	log.Println("Monitor service started. Processing files and embedding transcripts...")

	// Handle shutdown signals.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Received shutdown signal, starting graceful shutdown...")

	fileMonitor.Stop()
	w.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ingestSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("ingest HTTP listener shutdown error: %v", err)
	}

	log.Println("Waiting for all tasks to complete...")
	wg.Wait()

	log.Println("Monitor service shutdown complete")
}
