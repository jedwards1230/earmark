package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"transcriber/internal/config"
	"transcriber/internal/db"
)

type Server struct {
	cfg *config.Config
	db  *db.DB
}

func NewServer(database *db.DB, cfg *config.Config) *Server {
	return &Server{
		cfg: cfg,
		db:  database,
	}
}

func (s *Server) SearchHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("Received search request from %s: %s", r.RemoteAddr, r.URL.String())

	query := r.URL.Query().Get("q")
	if query == "" {
		log.Printf("Bad request: missing query parameter from %s", r.RemoteAddr)
		http.Error(w, "missing query parameter", http.StatusBadRequest)
		return
	}

	thresholdStr := r.URL.Query().Get("p")
	if thresholdStr == "" {
		thresholdStr = "0.3"
	}

	threshold, err := strconv.ParseFloat(thresholdStr, 64)
	if err != nil {
		log.Printf("Bad request: invalid threshold parameter from %s", r.RemoteAddr)
		http.Error(w, "invalid threshold parameter", http.StatusBadRequest)
		return
	}

	itemLimitStr := r.URL.Query().Get("k")
	if itemLimitStr == "" {
		itemLimitStr = "10"
	}

	itemLimit, err := strconv.Atoi(itemLimitStr)
	if err != nil {
		log.Printf("Bad request: invalid item limit parameter from %s", r.RemoteAddr)
		http.Error(w, "invalid item limit parameter", http.StatusBadRequest)
		return
	}

	// Log the search parameters
	log.Printf("Performing search with query: %q", query)
	results, err := s.db.Search(context.Background(), query, itemLimit, threshold) // Added minimum similarity threshold
	if err != nil {
		log.Printf("Search error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Search returned %d results", len(results))
	// Ensure we return an empty array instead of null when no results
	if results == nil {
		results = []db.VectorEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	resp, err := json.Marshal(map[string]interface{}{
		"query":   query,
		"count":   len(results),
		"results": results,
	})
	if err != nil {
		log.Printf("JSON marshaling error: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Write(resp)
}

func (s *Server) Start() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/search", s.SearchHandler)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		log.Printf("HTTP server listening on http://localhost%s\n", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()

	return srv
}
