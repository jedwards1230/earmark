// Package ingesthttp serves the ingest process's minimal HTTP surface.
//
// The ingest pod runs `earmark monitor` (file watcher + embed worker) and has
// no MCP server, so historically it had no HTTP port — its Kubernetes liveness
// probe shelled out to `pgrep -f earmark`, which is absent from the distroless
// image and so errored ("unknown state"). This package gives the ingest process
// a tiny HTTP listener exposing:
//
//   - GET /healthz — always 200 "ok" (liveness; no external dependencies).
//   - GET /metrics — Prometheus exposition (the handler is injected; nil → 404).
//
// It is intentionally separate from internal/mcp so the ingest pod stays a thin
// worker with no MCP/dashboard surface.
package ingesthttp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jedwards1230/earmark/internal/log"
)

// Server is the ingest process's HTTP listener. Build it with New, run it with
// Start (blocks until Shutdown), and stop it with Shutdown.
type Server struct {
	addr    string
	srv     *http.Server
	log     log.Logger
	metrics http.Handler // optional Prometheus handler; nil → /metrics 404s
}

// New builds an ingest HTTP server bound to addr (e.g. ":8082"). metrics is the
// Prometheus exposition handler mounted at /metrics; pass nil to leave /metrics
// returning 404 (Phase 0 wires /healthz only; Phase 5 supplies the handler).
func New(addr string, metrics http.Handler) *Server {
	s := &Server{
		addr:    addr,
		log:     log.NewLogger("ingest-http"),
		metrics: metrics,
	}

	mux := http.NewServeMux()
	// Liveness — no external dependencies; mirrors the mcp pod's /healthz.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	return s
}

// Start runs the listener until Shutdown is called. It blocks; run it in a
// goroutine. A clean shutdown (http.ErrServerClosed) returns nil.
func (s *Server) Start() error {
	s.log.Info("ingest HTTP listener started", "addr", s.addr)
	if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the listener, honoring ctx for the drain deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	s.log.Info("ingest HTTP listener shutting down")
	return s.srv.Shutdown(ctx)
}
