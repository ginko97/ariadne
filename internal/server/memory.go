package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ginko97/ariadne/internal/memory"
)

type memoryItem struct {
	Index int       `json:"index"`
	Text  string    `json:"text"`
	RunID string    `json:"run_id"`
	At    time.Time `json:"at"`
}

type memoryAddRequest struct {
	Text string `json:"text"`
}

type memoryEditRequest struct {
	Index int    `json:"index"`
	Text  string `json:"text"`     // what the note read when it was shown
	New   string `json:"new_text"` // what it should read now
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

// handleMemoryAdd saves a fact the person typed, exactly as typed.
//
// No approval card: the card exists because the model proposes a note and
// may reword it on the way (it once turned "I live on Earth" into a claim
// that 2026 web pages were hypothetical). Here nobody else wrote the text.
// The limits are the store's, so a note saved here is the same shape as one
// the tool saves. Behind the CSRF token like the delete: a page that could
// write to memory would be putting words into every later conversation.
//
// Saving a note that is already there changes nothing; Added says whether
// this one was new.
func (s *Server) handleMemoryAdd(w http.ResponseWriter, r *http.Request) {
	if s.MemoryStore.Path == "" {
		httpError(w, http.StatusNotFound, "memory store is not configured")
		return
	}
	var req memoryAddRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	before, err := s.MemoryStore.Load()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to load memory notes: "+err.Error())
		return
	}
	if err := s.MemoryStore.Append(memory.Note{Text: req.Text, RunID: memory.ByOperator}); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	after, err := s.MemoryStore.Load()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "failed to load memory notes: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": len(after) > len(before), "count": len(after)})
}

// handleMemoryEdit changes a note in place, word for word, provided it still
// reads what the page showed (memory.Store.Replace). Like a note typed in,
// no card: the person wrote the new text. Behind the CSRF token.
func (s *Server) handleMemoryEdit(w http.ResponseWriter, r *http.Request) {
	if s.MemoryStore.Path == "" {
		httpError(w, http.StatusNotFound, "memory store is not configured")
		return
	}
	var req memoryEditRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if err := s.MemoryStore.Replace(req.Index, req.Text, req.New); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
