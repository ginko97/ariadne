package server

import (
	"encoding/json"
	"net/http"

	"github.com/ginko97/ariadne/internal/llm"
)

type modelsResponse struct {
	Models []llm.ModelRow `json:"models"`
	Source string         `json:"source"`
	// Configured is the model this process was started with. Without it a
	// picker has no defensible default and falls back to whatever sorts first,
	// which means the first turn of every conversation runs on a model nobody
	// chose — and on this gateway the alphabet is not a ranking.
	Configured string `json:"configured,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

// handleModels serves the picker.
//
// It never returns an error status. Every failure downgrades to a smaller
// answer with the reason attached, because a picker that 500s takes the whole
// page with it over a list that is only ever a convenience — the conversation
// works fine on the model the process already has.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if s.Models == nil {
		httpError(w, http.StatusNotImplemented, "no model list configured")
		return
	}
	rows, source, warning := s.Models.Get(r.Context())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelsResponse{
		Models:     rows,
		Source:     source,
		Configured: s.Models.Configured(),
		Warning:    warning,
	})
}
