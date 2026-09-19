package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Setting up a provider from the page: which endpoint, which key, which model.
//
// The conditions were settled on 2026-09-10, before any of this existed, and
// they are the design:
//
//   - loopback only, and Host and Origin checked, and a CSRF token on every
//     POST — all three are guard's, and apply here as everywhere;
//   - the key is never sent back to the browser, not even masked. SetupStatus
//     has no field that could carry it, so there is nothing to forget to
//     blank, and an error is scrubbed of the key that was just submitted
//     before it is returned (a provider's 401 can quote it);
//   - the request body is not logged. Nothing in this package logs bodies,
//     and this handler adds nothing that does.
//
// What a key is checked against, and where it is stored, is cmd/ariadne's:
// the same check and the same config.env as `ariadne setup`.

// SetupRequest is what the page submits. Key may be empty to keep the key the
// process already has for the same endpoint, or for a local server that needs
// none.
type SetupRequest struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Key      string `json:"key"`
	Model    string `json:"model"`
}

// SetupStatus is what the page may know about the configuration: which
// provider, endpoint and model, and whether a key is set. Deliberately no key.
type SetupStatus struct {
	Configured bool            `json:"configured"`
	Provider   string          `json:"provider"`
	BaseURL    string          `json:"base_url"`
	Model      string          `json:"model"`
	KeySet     bool            `json:"key_set"`
	Notes      []string        `json:"notes,omitempty"`
	Providers  []SetupProvider `json:"providers"`
}

// SetupProvider is one choice offered on the setup card.
type SetupProvider struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	NeedsKey     bool   `json:"needs_key"`
	KeyName      string `json:"key_name,omitempty"`
	DefaultModel string `json:"default_model,omitempty"`
}

// Setup is how the server reads and changes the provider. Nil means the
// process cannot be set up from the page, and the endpoints say so.
type Setup struct {
	Status       func() SetupStatus
	Configure    func(ctx context.Context, req SetupRequest) (SetupStatus, error)
	OllamaModels func(ctx context.Context, baseURL string) ([]string, error)
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, _ *http.Request) {
	if s.Setup == nil {
		httpError(w, http.StatusNotImplemented, "setup from the page is not available")
		return
	}
	writeJSON(w, http.StatusOK, s.Setup.Status())
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.Setup == nil {
		httpError(w, http.StatusNotImplemented, "setup from the page is not available")
		return
	}
	var req SetupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	st, err := s.Setup.Configure(r.Context(), req)
	if err != nil {
		httpError(w, http.StatusBadRequest, scrubKey(err.Error(), req.Key))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSetupOllama(w http.ResponseWriter, r *http.Request) {
	if s.Setup == nil || s.Setup.OllamaModels == nil {
		httpError(w, http.StatusNotImplemented, "setup from the page is not available")
		return
	}
	var req struct {
		BaseURL string `json:"base_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	models, err := s.Setup.OllamaModels(r.Context(), req.BaseURL)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// configured reports whether a conversation can start. A server without Setup
// was started with a working provider, or it would not have started.
func (s *Server) configured() bool {
	return s.Setup == nil || s.Setup.Status().Configured
}

// scrubKey removes a submitted key from text going back to the browser. Keys
// shorter than 8 characters are left alone: replacing "ab" everywhere would
// mangle the message and hide nothing worth hiding.
func scrubKey(text, key string) string {
	if len(key) < 8 {
		return text
	}
	return strings.ReplaceAll(text, key, "[key]")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
