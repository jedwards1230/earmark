package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/jedwards1230/earmark/internal/db"
)

func (s *Server) SearchHandler(w http.ResponseWriter, r *http.Request) {
	s.log.Info("Received search request",
		"remote_addr", r.RemoteAddr,
		"url", r.URL.String())

	query := r.URL.Query().Get("q")
	if query == "" {
		s.log.Warn("Bad request: missing query parameter",
			"remote_addr", r.RemoteAddr)
		http.Error(w, "missing query parameter", http.StatusBadRequest)
		return
	}

	thresholdStr := r.URL.Query().Get("p")
	if thresholdStr == "" {
		thresholdStr = "0.3"
	}

	threshold, err := strconv.ParseFloat(thresholdStr, 64)
	if err != nil {
		s.log.Warn("Bad request: invalid threshold parameter",
			"remote_addr", r.RemoteAddr,
			"threshold", thresholdStr)
		http.Error(w, "invalid threshold parameter", http.StatusBadRequest)
		return
	}

	itemLimitStr := r.URL.Query().Get("k")
	if itemLimitStr == "" {
		itemLimitStr = "10"
	}

	itemLimit, err := strconv.Atoi(itemLimitStr)
	if err != nil {
		s.log.Warn("Bad request: invalid item limit parameter",
			"remote_addr", r.RemoteAddr,
			"limit", itemLimitStr)
		http.Error(w, "invalid item limit parameter", http.StatusBadRequest)
		return
	}

	s.log.Info("Performing search",
		"query", query,
		"threshold", threshold,
		"limit", itemLimit)

	results, err := s.db.Search(r.Context(), query, itemLimit, threshold)
	if err != nil {
		s.log.Error("Search error", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.log.Info("Search completed",
		"query", query,
		"result_count", len(results))

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
		s.log.Error("JSON marshaling error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if _, err := w.Write(resp); err != nil {
		s.log.Error("Failed to write response", "error", err)
	}
}
