package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// upstream serves a canned OpenRouter response and counts how often it is hit,
// which is how the cache is observed rather than assumed.
func upstream(t *testing.T, body string, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

// Two tool-capable models, one without tools, and one priced "-1" — the shape
// the live API actually returns, trimmed to what is read.
const modelsFixture = `{"data":[
 {"id":"b/tools-two","name":"Two","context_length":128000,
  "pricing":{"prompt":"0.000003","completion":"0.000015"},
  "supported_parameters":["temperature","tools"]},
 {"id":"a/tools-one","name":"One","context_length":1048576,
  "pricing":{"prompt":"0.00000096","completion":"0.00000288"},
  "supported_parameters":["tools","top_p"]},
 {"id":"c/no-tools","name":"No Tools","context_length":8192,
  "pricing":{"prompt":"0.0000001","completion":"0.0000002"},
  "supported_parameters":["temperature"]},
 {"id":"d/unquoted","name":"Unquoted","context_length":4096,
  "pricing":{"prompt":"-1","completion":"-1"},
  "supported_parameters":["tools"]}
]}`

func newCache(t *testing.T, url string) *ModelCache {
	t.Helper()
	m := NewModelCache("configured/model")
	m.URL = url
	m.Client = &http.Client{}
	return m
}

// A model that cannot call tools cannot run this agent, so offering one is
// offering a broken conversation.
func TestModelsKeepsOnlyToolCapableAndSortsThem(t *testing.T) {
	ts, _ := upstream(t, modelsFixture, http.StatusOK)
	rows, source, warning := newCache(t, ts.URL).Get(context.Background())

	if source != "live" || warning != "" {
		t.Fatalf("source=%q warning=%q, want a clean live fetch", source, warning)
	}
	if len(rows) != 3 {
		t.Fatalf("kept %d models, want the 3 tool-capable ones", len(rows))
	}
	for _, r := range rows {
		if r.ID == "c/no-tools" {
			t.Error("a model without tool support reached the picker")
		}
	}
	if rows[0].ID != "a/tools-one" {
		t.Errorf("first row = %q, want the list sorted by id", rows[0].ID)
	}
}

// Dollars per token is unreadable at a glance and the picker exists to be
// glanced at. 0.000003 per token is $3 per million.
func TestModelsReportsPricePerMillionTokens(t *testing.T) {
	ts, _ := upstream(t, modelsFixture, http.StatusOK)
	rows, _, _ := newCache(t, ts.URL).Get(context.Background())

	byID := map[string]ModelRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if got := byID["b/tools-two"].PromptPerMTok; got != 3 {
		t.Errorf("prompt price = %v, want 3 per million", got)
	}
	if got := byID["b/tools-two"].CompletionPerMTok; got != 15 {
		t.Errorf("completion price = %v, want 15 per million", got)
	}
	// "-1" is what the API uses for a price it will not quote. Still a model
	// you can pick, so the row survives with an unknown price rather than
	// vanishing.
	if _, ok := byID["d/unquoted"]; !ok {
		t.Error("a model with unquoted pricing was dropped")
	}
	if got := byID["d/unquoted"].PromptPerMTok; got != 0 {
		t.Errorf("unquoted price = %v, want 0", got)
	}
}

func TestModelsCachesWithinTTLAndRefetchesAfter(t *testing.T) {
	ts, hits := upstream(t, modelsFixture, http.StatusOK)
	m := newCache(t, ts.URL)
	m.TTL = 50 * time.Millisecond

	m.Get(context.Background())
	_, source, _ := m.Get(context.Background())
	if hits.Load() != 1 {
		t.Errorf("hit upstream %d times inside the TTL, want 1", hits.Load())
	}
	if source != "cache" {
		t.Errorf("source = %q on the second call, want cache", source)
	}

	time.Sleep(60 * time.Millisecond)
	if _, source, _ := m.Get(context.Background()); source != "live" {
		t.Errorf("source = %q after the TTL expired, want live", source)
	}
	if hits.Load() != 2 {
		t.Errorf("hit upstream %d times overall, want 2", hits.Load())
	}
}

// Stale beats absent: an expired list is still the models that existed
// yesterday, which is very nearly the same list.
func TestModelsServesAStaleListWhenTheRefreshFails(t *testing.T) {
	var fail atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(modelsFixture))
	}))
	defer ts.Close()

	m := newCache(t, ts.URL)
	m.TTL = 10 * time.Millisecond
	if _, source, _ := m.Get(context.Background()); source != "live" {
		t.Fatalf("first fetch source = %q, want live", source)
	}

	fail.Store(true)
	time.Sleep(20 * time.Millisecond)

	rows, source, warning := m.Get(context.Background())
	if source != "cache" {
		t.Errorf("source = %q, want cache — a failed refresh discarded a good list", source)
	}
	if len(rows) != 3 {
		t.Errorf("served %d models from the stale cache, want 3", len(rows))
	}
	if warning == "" {
		t.Error("a stale list was served with no indication that it is stale")
	}
}

// Nothing has ever been fetched and the network is gone. The configured model
// is the one thing known to work, because the process was started with it.
func TestModelsFallsBackToTheConfiguredModel(t *testing.T) {
	ts, _ := upstream(t, "", http.StatusInternalServerError)
	rows, source, warning := newCache(t, ts.URL).Get(context.Background())

	if source != "fallback" {
		t.Fatalf("source = %q, want fallback", source)
	}
	if len(rows) != 1 || rows[0].ID != "configured/model" {
		t.Fatalf("rows = %v, want just the configured model", rows)
	}
	if warning == "" {
		t.Error("the fallback was served with no reason attached")
	}
}

// A 200 carrying nothing usable is a failure. Treating it as success would
// overwrite a good cache with an empty picker.
func TestModelsTreatsAnEmptyListAsAFailure(t *testing.T) {
	ts, _ := upstream(t, `{"data":[]}`, http.StatusOK)
	_, source, _ := newCache(t, ts.URL).Get(context.Background())
	if source != "fallback" {
		t.Errorf("source = %q for an empty list, want fallback", source)
	}
}

// The picker is a convenience; the conversation works on the model the process
// already has. So this endpoint never takes the page down with it.
func TestModelsEndpointNeverReturnsAnError(t *testing.T) {
	up, _ := upstream(t, "", http.StatusInternalServerError)
	s, _ := newTestServer(t)
	s.Models = newCache(t, up.URL)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with the upstream down", resp.StatusCode)
	}

	var out modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Source != "fallback" || len(out.Models) != 1 {
		t.Errorf("source=%q models=%d, want the fallback row", out.Source, len(out.Models))
	}
	if out.Warning == "" {
		t.Error("no warning on a degraded response")
	}
}
