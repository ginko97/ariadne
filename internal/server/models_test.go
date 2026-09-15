package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
)

// upstreamDown is a stand-in gateway that always fails, so the endpoint can be
// exercised in its degraded state without reaching the network.
func upstreamDown(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// The picker is a convenience; the conversation works on the model the process
// already has. So this endpoint never takes the page down with it.
func TestModelsEndpointNeverReturnsAnError(t *testing.T) {
	up := upstreamDown(t)
	s, _ := newTestServer(t)
	m := llm.NewModelCache("configured/model")
	m.URL = up.URL
	s.Models = m
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
