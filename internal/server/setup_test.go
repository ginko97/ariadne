package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

const testKey = "sk-test-SECRET-0123456789"

// setupServer wires a Setup whose Configure records what it was given and
// answers with err, or with a configured status.
func setupServer(t *testing.T, configured bool, err error) (*Server, string, *atomic.Int32, *atomic.Value) {
	t.Helper()
	s, ts := newTestServer(t, endResponse("hello"))
	calls := &atomic.Int32{}
	got := &atomic.Value{}
	status := SetupStatus{Configured: configured, Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "m", KeySet: configured}
	s.Setup = &Setup{
		Status: func() SetupStatus { return status },
		Configure: func(_ context.Context, req SetupRequest) (SetupStatus, error) {
			calls.Add(1)
			got.Store(req)
			if err != nil {
				return SetupStatus{}, err
			}
			return SetupStatus{Configured: true, Provider: req.Provider, Model: req.Model, KeySet: true}, nil
		},
	}
	return s, ts.URL, calls, got
}

func postSetup(t *testing.T, url, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Ariadne-CSRF", token)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The key reaches Configure, and never comes back: not in the status on
// success, and not in an error that quotes it.
func TestSetupNeverReturnsTheKey(t *testing.T) {
	s, url, _, got := setupServer(t, false, nil)
	code, body := postSetup(t, url, "/api/setup", s.CSRFToken,
		`{"provider":"openrouter","key":"`+testKey+`","model":"m"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	if req := got.Load().(SetupRequest); req.Key != testKey {
		t.Errorf("Configure got key %q, want the submitted one", req.Key)
	}
	if strings.Contains(body, testKey) || strings.Contains(body, "SECRET") {
		t.Errorf("the key came back in the response: %s", body)
	}

	// A provider's refusal can quote the key; the handler scrubs it.
	s, url, _, _ = setupServer(t, false, errors.New("openai: 401 Unauthorized: Incorrect API key provided: "+testKey))
	code, body = postSetup(t, url, "/api/setup", s.CSRFToken, `{"provider":"openai","key":"`+testKey+`"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", code, body)
	}
	if strings.Contains(body, testKey) || strings.Contains(body, "SECRET") {
		t.Errorf("the key came back inside an error: %s", body)
	}
}

// guard's CSRF check covers the setup route: a page that is not ariadne's
// cannot point conversations at another endpoint.
func TestSetupNeedsTheCSRFToken(t *testing.T) {
	_, url, calls, _ := setupServer(t, false, nil)
	code, _ := postSetup(t, url, "/api/setup", "", `{"provider":"other","base_url":"https://evil.example/v1","key":"x","model":"m"}`)
	if code != http.StatusForbidden {
		t.Errorf("status %d without the token, want 403", code)
	}
	if calls.Load() != 0 {
		t.Error("Configure ran for a request without the token")
	}
}

// Until a provider is set up there is nothing to answer with, and a
// conversation must not start and fail halfway.
func TestChatWaitsForSetup(t *testing.T) {
	s, url, _, _ := setupServer(t, false, nil)
	code, body := postSetup(t, url, "/api/chat", s.CSRFToken, `{"message":"hi"}`)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "Settings") {
		t.Errorf("chat before setup: %d %s, want 503 pointing at Settings", code, body)
	}

	s, url, _, _ = setupServer(t, true, nil)
	if code, body := postSetup(t, url, "/api/chat", s.CSRFToken, `{"message":"hi"}`); code != http.StatusOK {
		t.Errorf("chat after setup: %d %s", code, body)
	}
}
