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
	"github.com/jedwards1230/earmark/internal/eval"
	"github.com/jedwards1230/earmark/internal/ingesthttp"
	"github.com/jedwards1230/earmark/internal/metaprovider"
	"github.com/jedwards1230/earmark/internal/metrics"
	"github.com/jedwards1230/earmark/internal/monitor"
	"github.com/jedwards1230/earmark/internal/queue"
	"github.com/jedwards1230/earmark/internal/recipe"
	"github.com/jedwards1230/earmark/internal/telemetry"
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

	// OpenTelemetry (CONTRACT §2.16): configured from the OTEL_* environment
	// only. A setup error is logged, never fatal — telemetry must not stop
	// ingest.
	tel, err := telemetry.Setup(context.Background())
	if err != nil {
		log.Printf("WARNING: OpenTelemetry disabled: %v", err)
	}

	registerCurrentRecipes(database, cfg)
	publishRecipeInfo(database, tel)

	// The embedded ASIN tag the runner reports is the third ASIN source
	// (CONTRACT §1.6); only this write path pays for consulting it.
	meta := metaprovider.New(cfg, metaprovider.WithEmbeddedASINSource(database))

	workQueue := queue.NewQueue()
	fileMonitor := monitor.NewFileMonitor(cfg, database, meta)
	w := worker.NewWorker(workQueue, database, cfg)

	// Prometheus metrics for the ingest pod (CONTRACT §2.16). The scrape-time
	// collector reads the DB for current-state gauges; the worker records stage
	// durations + counters through the same registry.
	reg := metrics.New(database, 2*time.Second)
	w.SetMetrics(reg)
	// OTel instruments are served on the same /metrics; the legacy registry
	// rides along in the OTLP push (when configured).
	reg.AddGatherer(tel.Gatherer())
	tel.SetLegacyGatherer(reg.Gatherer())
	tel.StartStaleRefresh(staleItemsInterval, 30*time.Second, database.StaleItemCounts)

	// Once the runner reports a file's embedded ASIN tag, re-derive that
	// book's metadata so the tag is applied as soon as it exists.
	w.SetBookRefresher(fileMonitor.RefreshBookMetadata)

	// Minimal HTTP listener for the ingest pod: /healthz (liveness) + /metrics
	// (Prometheus). The ingest process has no MCP server, so this is its only HTTP
	// surface (replaces the broken `pgrep` liveness probe). A bind failure is
	// logged but non-fatal — the worker/monitor are the real work and must not be
	// blocked by a probe-only port being unavailable.
	ingestSrv := ingesthttp.New(cfg.IngestHTTPAddr, reg.Handler())
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

	// Flush buffered spans and a final metric export before exiting.
	telCtx, telCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer telCancel()
	if err := tel.Shutdown(telCtx); err != nil {
		log.Printf("OpenTelemetry shutdown: %v", err)
	}

	log.Println("Monitor service shutdown complete")
}

// staleItemsInterval is how often earmark_stale_items re-counts the stale_work
// view. Staleness only changes when a recipe or a model changes, so a slow
// timer is plenty; one count is a single aggregate per step.
const staleItemsInterval = 5 * time.Minute

// publishRecipeInfo loads current_recipes into earmark_recipe_info. Best-effort.
func publishRecipeInfo(database *db.DB, tel *telemetry.Telemetry) {
	if tel == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cur, err := database.ListCurrentRecipes(ctx)
	if err != nil {
		log.Printf("WARNING: earmark_recipe_info not published: %v", err)
		return
	}
	infos := make([]telemetry.RecipeInfo, 0, len(cur))
	for _, c := range cur {
		infos = append(infos, telemetry.RecipeInfo{
			Step: c.Step, RecipeID: c.RecipeID, Model: c.Model,
			Revision: c.Revision, PromptVersion: c.PromptVersion,
		})
	}
	tel.SetRecipes(infos)
}

// registerCurrentRecipes records, per step, the recipe this deployment would
// use right now (CONTRACT §1.9): the embed recipe always, the propose (judge)
// recipe when an eval chat endpoint is configured. The stale_work view compares
// every output row against these. Only the ingest process does this, so there
// is one writer of "current"; an ad-hoc `earmark eval` with other settings
// stamps its own recipe on its findings without redefining current.
//
// Best-effort: stamping does not depend on it (writes register their recipe
// themselves), so a failure is logged, not fatal.
func registerCurrentRecipes(database *db.DB, cfg *config.Config) {
	current := []recipe.Recipe{database.EmbedRecipe()}
	r, ok, err := eval.CurrentRecipe(cfg)
	if err != nil {
		log.Printf("WARNING: eval chat endpoint is misconfigured, no current propose recipe registered: %v", err)
	}
	if ok {
		current = append(current, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.SetCurrentRecipes(ctx, current...); err != nil {
		log.Printf("WARNING: could not register current recipes: %v", err)
		return
	}
	for _, r := range current {
		id, err := r.ID()
		if err != nil {
			log.Printf("WARNING: could not compute the current %s recipe id: %v", r.Step, err)
			continue
		}
		log.Printf("current %s recipe %s (model %s, resolved %s)", r.Step, id, r.ModelAlias, r.ModelResolved)
	}
}
