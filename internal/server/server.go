package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jedwards1230/lil-whisper/internal/config"
	"github.com/jedwards1230/lil-whisper/internal/db"
	"github.com/jedwards1230/lil-whisper/internal/log"
)

// DBInterface defines the database methods used by the server
type DBInterface interface {
	Search(ctx context.Context, query string, limit int, threshold float64) ([]db.SearchResultWithMetadata, error)
}

type Server struct {
	cfg *config.Config
	db  DBInterface
	log log.Logger
}

func NewServer(database DBInterface, cfg *config.Config) *Server {
	logger := log.NewLogger("server")
	return &Server{
		cfg: cfg,
		db:  database,
		log: logger,
	}
}

func (s *Server) Start() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", s.SearchHandler)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		s.log.Info("HTTP server listening",
			"address", srv.Addr,
			"url", fmt.Sprintf("http://localhost%s", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Error("Server error", "error", err)
			panic(err)
		}
	}()

	return srv
}
