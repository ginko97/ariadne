package server

import (
	"encoding/json"
	"net/http"
	"time"
)

type memoryItem struct {
	Index int       `json:"index"`
	Text  string    `json:"text"`
	RunID string    `json:"run_id"`
	At    time.Time `json:"at"`
}

type memoryDeleteRequest struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// handleMemoryList returns the remembered facts from MEMORY.md.
func (s *Server) handleMemoryList(w http.ResponseWriter, r *http.Request) {
	if s.MemoryStore.Path == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]\n"))
		return
	}

	notes, err := s.MemoryStore.Load()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to load memory notes: "+err.Error())
		return
	}

	items := make([]memoryItem, 0, len(notes))
	for i, n := range notes {
		items = append(items, memoryItem{
			Index: i,
			Text:  n.Text,
			RunID: n.RunID,
			At:    n.At,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(items); err != nil {
		httpError(w, http.StatusInternalServerError, "failed to encode memory notes")
	}
}

// handleMemoryDelete deletes a remembered note matching index and text.
func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request) {
	if s.MemoryStore.Path == "" {
		httpError(w, http.StatusNotFound, "memory store is not configured")
		return
	}

	var req memoryDeleteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}

	if err := s.MemoryStore.Delete(req.Index, req.Text); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}` + "\n"))
}
