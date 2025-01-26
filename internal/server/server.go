package server

import (
	"log"
	"net/http"
	"os"
	"transcriber/internal/config"
	"transcriber/internal/db"
)

type Server struct {
	cfg *config.Config
	db  *db.DB
	log *log.Logger
}

func NewServer(database *db.DB, cfg *config.Config) *Server {
	logger := log.New(os.Stdout, "(server) ", 0)
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
		s.log.Printf("HTTP server listening on http://localhost%s\n", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()

	return srv
}
