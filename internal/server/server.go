package server

import (
	"fmt"
	"net/http"

	"transcriber/internal/config"
	"transcriber/internal/db"
	"transcriber/internal/log"
)

type Server struct {
	cfg *config.Config
	db  *db.DB
	log log.Logger
}

func NewServer(database *db.DB, cfg *config.Config) *Server {
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
