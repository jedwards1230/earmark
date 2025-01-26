package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"transcriber/internal/db"
)

func (s *Server) SearchHandler(w http.ResponseWriter, r *http.Request) {
	s.log.Printf("Received search request from %s: %s", r.RemoteAddr, r.URL.String())

	query := r.URL.Query().Get("q")
	if query == "" {
		s.log.Printf("Bad request: missing query parameter from %s", r.RemoteAddr)
		http.Error(w, "missing query parameter", http.StatusBadRequest)
		return
	}

	thresholdStr := r.URL.Query().Get("p")
	if thresholdStr == "" {
		thresholdStr = "0.3"
	}

	threshold, err := strconv.ParseFloat(thresholdStr, 64)
	if err != nil {
		s.log.Printf("Bad request: invalid threshold parameter from %s", r.RemoteAddr)
		http.Error(w, "invalid threshold parameter", http.StatusBadRequest)
		return
	}

	itemLimitStr := r.URL.Query().Get("k")
	if itemLimitStr == "" {
		itemLimitStr = "10"
	}

	itemLimit, err := strconv.Atoi(itemLimitStr)
	if err != nil {
		s.log.Printf("Bad request: invalid item limit parameter from %s", r.RemoteAddr)
		http.Error(w, "invalid item limit parameter", http.StatusBadRequest)
		return
	}

	// Log the search parameters
	s.log.Printf("Performing search with query: %q", query)
	results, err := s.db.Search(context.Background(), query, itemLimit, threshold)
	if err != nil {
		s.log.Printf("Search error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.log.Printf("Search returned %d results", len(results))
	// Ensure we return an empty array instead of null when no results
	if results == nil {
		results = []db.SearchResultWithMetadata{}
	}

	w.Header().Set("Content-Type", "application/json")
	resp, err := json.Marshal(map[string]interface{}{
		"query":   query,
		"count":   len(results),
		"results": results,
	})
	if err != nil {
		s.log.Printf("JSON marshaling error: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Write(resp)
}
