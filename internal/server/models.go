package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// The model list is fetched, never curated.
//
// A hardcoded list was checked against the live API before this was written:
// of five plausible entries, one had already been withdrawn. A curated list
// starts stale and gets worse, and the endpoint is unauthenticated, so there is
// no reason to keep one. Fetching also makes "let me type a model id by hand"
// stop being a separate feature — the list is the truth and anything in it works.
const (
	openRouterModelsURL = "https://openrouter.ai/api/v1/models"
	modelsTTL           = 24 * time.Hour
	modelsTimeout       = 10 * time.Second
)

// ModelRow is one row of the picker: what to choose between models on.
//
// Prices are per million tokens. The API reports dollars per token, which for
// a cheap model is 0.00000015 — a number nobody can compare at a glance, and
// one that loses precision to the eye long before it loses it to a float.
type ModelRow struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	ContextLength     int     `json:"context_length"`
	PromptPerMTok     float64 `json:"prompt_per_mtok"`
	CompletionPerMTok float64 `json:"completion_per_mtok"`
}

// ModelCache serves the model list without ever being the reason a turn fails.
//
// This is the only part of the server that depends on somebody else's uptime,
// so every failure has an answer that is not an error: a stale list is better
// than no list, and the configured model alone is better than an empty picker.
// The chain is live, then whatever was last fetched however old, then the model
// this process was started with.
type ModelCache struct {
	URL      string
	TTL      time.Duration
	Timeout  time.Duration
	Client   *http.Client
	Fallback string // the configured -model, used before any fetch has succeeded

	mu      sync.Mutex
	rows    []ModelRow
	fetched time.Time
	lastErr string
}

func NewModelCache(fallback string) *ModelCache {
	return &ModelCache{
		URL:      openRouterModelsURL,
		TTL:      modelsTTL,
		Timeout:  modelsTimeout,
		Client:   &http.Client{},
		Fallback: fallback,
	}
}

// Get returns the list and where it came from: "live", "cache" or "fallback".
//
// The source is reported rather than hidden because the three are not the same
// claim. A stale list may be missing a model that now exists or offering one
// that no longer does, and a picker that cannot say which it is showing invites
// the question this project keeps answering the hard way — is the thing I am
// looking at a measurement or a guess.
func (m *ModelCache) Get(ctx context.Context) (rows []ModelRow, source string, warning string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.rows != nil && time.Since(m.fetched) < m.ttl() {
		return m.rows, "cache", ""
	}

	fetched, err := m.fetch(ctx)
	if err == nil {
		m.rows, m.fetched, m.lastErr = fetched, time.Now(), ""
		return m.rows, "live", ""
	}
	m.lastErr = err.Error()

	// Stale beats absent. An expired list is still a list of models that
	// existed yesterday, which is very nearly the same list.
	if m.rows != nil {
		return m.rows, "cache", "refresh failed, showing the last list: " + m.lastErr
	}

	// Nothing has ever been fetched. The configured model is the one thing
	// known to work, because the process was started with it.
	if m.Fallback == "" {
		return []ModelRow{}, "fallback", "no model list and no configured model: " + m.lastErr
	}
	return []ModelRow{{ID: m.Fallback, Name: m.Fallback}}, "fallback",
		"could not reach the model list, offering the configured model only: " + m.lastErr
}

func (m *ModelCache) ttl() time.Duration {
	if m.TTL <= 0 {
		return modelsTTL
	}
	return m.TTL
}

// wire is the half of OpenRouter's response this cares about. Every other
// field — descriptions, hugging face ids, per-request limits — is real and not
// wanted, and naming only what is used keeps a schema change from being a
// decode error.
type wireModels struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int    `json:"context_length"`
		Pricing       struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
		} `json:"pricing"`
		SupportedParameters []string `json:"supported_parameters"`
	} `json:"data"`
}

func (m *ModelCache) fetch(ctx context.Context) ([]ModelRow, error) {
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = modelsTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return nil, err
	}
	client := m.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: http %d", resp.StatusCode)
	}

	var w wireModels
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		return nil, fmt.Errorf("models: decode: %w", err)
	}

	var out []ModelRow
	for _, d := range w.Data {
		// Tool use is not optional here: a model that cannot call tools cannot
		// run this agent, and offering one is offering a broken conversation.
		if !containsStr(d.SupportedParameters, "tools") {
			continue
		}
		out = append(out, ModelRow{
			ID:                d.ID,
			Name:              d.Name,
			ContextLength:     d.ContextLength,
			PromptPerMTok:     perMTok(d.Pricing.Prompt),
			CompletionPerMTok: perMTok(d.Pricing.Completion),
		})
	}
	if len(out) == 0 {
		// A 200 with nothing usable in it is a failure, not an empty picker.
		// Reporting it as success would overwrite a good cache with nothing.
		return nil, fmt.Errorf("models: no tool-capable models in %d entries", len(w.Data))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// perMTok converts dollars per token to dollars per million. Unparseable
// pricing yields 0 rather than failing the row: a model with an unknown price
// is still a model you can pick, and "-1" is what the API uses for pricing it
// will not quote.
func perMTok(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v * 1_000_000
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

type modelsResponse struct {
	Models  []ModelRow `json:"models"`
	Source  string     `json:"source"`
	Warning string     `json:"warning,omitempty"`
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
		Models:  rows,
		Source:  source,
		Warning: warning,
	})
}
