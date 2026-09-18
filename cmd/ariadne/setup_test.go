package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const okCompletion = `{"choices":[{"message":{"role":"assistant","content":"ready"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`

// useConfigFile points setup at a temporary config.env for one test.
func useConfigFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ariadne", "config.env")
	saved := configEnvFile
	configEnvFile = p
	t.Cleanup(func() { configEnvFile = saved })
	return p
}

// The whole path offline: an endpoint, a key typed on stdin, one real request
// carrying that key, and a config.env an installed binary can start from.
func TestSetupChecksTheKeyAndWritesConfig(t *testing.T) {
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okCompletion))
	}))
	defer srv.Close()
	cfg := useConfigFile(t)

	var out strings.Builder
	code := runSetup(strings.NewReader("sk-test-key-123456\n"), &out,
		[]string{"-provider", "other", "-base-url", srv.URL, "-model", "m"})
	if code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if got := gotAuth.Load(); got != "Bearer sk-test-key-123456" {
		t.Errorf("the check request carried %q, not the typed key", got)
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ARIADNE_API_KEY=sk-test-key-123456", "ARIADNE_BASE_URL=" + srv.URL, "ARIADNE_MODEL=m"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config.env lacks %q:\n%s", want, data)
		}
	}
}

// A key that does not work writes nothing: a setup that reports success and
// then fails on the first real question is worse than none.
func TestSetupWritesNothingWhenTheKeyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"invalid key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	cfg := useConfigFile(t)

	var out strings.Builder
	code := runSetup(strings.NewReader("sk-wrong\n"), &out,
		[]string{"-provider", "other", "-base-url", srv.URL, "-model", "m"})
	if code != exitFail {
		t.Errorf("exit %d, want %d:\n%s", code, exitFail, out.String())
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("config.env was written for a key that failed (%v)", err)
	}
}

// A known provider's key is stored under that provider's own name, so apiKey
// only ever sends it to that provider's host — never as ARIADNE_API_KEY,
// which goes everywhere.
func TestSetupStoresAProvidersKeyUnderItsOwnName(t *testing.T) {
	cfg := useConfigFile(t)
	var out strings.Builder
	if code := runSetup(strings.NewReader("sk-or-v1-abc\n"), &out, []string{"-provider", "openrouter", "-no-check"}); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), "OPENROUTER_API_KEY=sk-or-v1-abc") || strings.Contains(string(data), "ARIADNE_API_KEY") {
		t.Errorf("key stored under the wrong name:\n%s", data)
	}
	if !strings.Contains(string(data), "ARIADNE_MODEL="+defaultOpenRouterModel) {
		t.Errorf("no model paired with the provider:\n%s", data)
	}
}

// Rerunning setup replaces what it owns and keeps everything else a person
// put in the file.
func TestWriteConfigEnvKeepsOtherLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(p, []byte("# mine\nOPENROUTER_API_KEY=old\nSOMETHING_ELSE=kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigEnv(p, map[string]string{"OPENROUTER_API_KEY": "new", "ARIADNE_MODEL": "m"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	got := string(data)
	for _, want := range []string{"# mine", "OPENROUTER_API_KEY=new", "SOMETHING_ELSE=kept", "ARIADNE_MODEL=m"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "=old") {
		t.Errorf("the old key survived:\n%s", got)
	}
}
