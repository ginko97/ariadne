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

// No shell on Windows: `cmd /c start` would split a URL at &.
func TestBrowserCommand(t *testing.T) {
	const u = "http://127.0.0.1:7357/?a=1&b=2"
	for goos, want := range map[string]string{
		"windows": "rundll32 url.dll,FileProtocolHandler " + u,
		"darwin":  "open " + u,
		"linux":   "xdg-open " + u,
	} {
		if got := strings.Join(browserCommand(goos, u), " "); got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
}

// TestDefaultModelFor checks that known endpoints are paired with models that exist there.
func TestDefaultModelFor(t *testing.T) {
	for _, tc := range []struct {
		baseURL string
		want    string
	}{
		{"https://openrouter.ai/api/v1", defaultOpenRouterModel},
		{"https://api.openai.com/v1", defaultOpenAIModel},
		{"https://api.x.ai/v1", defaultXAIModel},
		{"https://generativelanguage.googleapis.com/v1beta/openai", defaultModel},
		{"http://localhost:11434/v1", defaultModel},
		{"https://max.ai/v1", defaultModel},
		{"api.openai.com", defaultOpenAIModel},
	} {
		if got := defaultModelFor(tc.baseURL); got != tc.want {
			t.Errorf("defaultModelFor(%q) = %q, want %q", tc.baseURL, got, tc.want)
		}
	}
}

// Switching from a generic endpoint to a named provider clears ARIADNE_API_KEY
// from config.env, so the wildcard key cannot shadow the provider's own key.
func TestSetupClearsWildcardApiKeyWhenConfiguringNamedProvider(t *testing.T) {
	cfg := useConfigFile(t)
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("ARIADNE_API_KEY=sk-old-wildcard\nARIADNE_BASE_URL=http://localhost:11434/v1\nARIADNE_MODEL=llama3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code := runSetup(strings.NewReader("sk-or-v1-new\n"), &out, []string{"-provider", "openrouter", "-no-check"}); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	data, _ := os.ReadFile(cfg)
	got := string(data)
	if strings.Contains(got, "ARIADNE_API_KEY") {
		t.Errorf("wildcard key ARIADNE_API_KEY was not cleared:\n%s", got)
	}
	if !strings.Contains(got, "OPENROUTER_API_KEY=sk-or-v1-new") {
		t.Errorf("new provider key missing:\n%s", got)
	}
}

// An empty config.env does not end up with a leading blank line after writeConfigEnv.
func TestWriteConfigEnvEmptyFileNoLeadingBlankLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(p, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigEnv(p, map[string]string{"ARIADNE_MODEL": "m"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if strings.HasPrefix(string(data), "\n") {
		t.Errorf("config.env started with a leading blank line:\n%q", string(data))
	}
	if got := string(data); got != "ARIADNE_MODEL=m\n" {
		t.Errorf("got %q, want %q", got, "ARIADNE_MODEL=m\n")
	}
}

// Setup defaults OpenAI and xAI to models that actually exist on their APIs.
func TestSetupDefaultModelForOpenAIAndXAI(t *testing.T) {
	for _, tc := range []struct {
		provider  string
		wantModel string
	}{
		{"openai", "ARIADNE_MODEL=gpt-4o-mini"},
		{"xai", "ARIADNE_MODEL=grok-2"},
	} {
		cfg := useConfigFile(t)
		var out strings.Builder
		if code := runSetup(strings.NewReader("secret\n"), &out, []string{"-provider", tc.provider, "-no-check"}); code != exitOK {
			t.Fatalf("%s setup exit %d:\n%s", tc.provider, code, out.String())
		}
		data, _ := os.ReadFile(cfg)
		got := string(data)
		if !strings.Contains(got, tc.wantModel) {
			t.Errorf("%s missing default model %q:\n%s", tc.provider, tc.wantModel, got)
		}
	}
}

// Setup does not warn about the environment taking precedence when the old
// values were only in config.env, not exported in the process environment.
func TestSetupDoesNotWarnOnValuesOnlyInConfigFile(t *testing.T) {
	cfg := useConfigFile(t)
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("OPENROUTER_API_KEY=sk-or-v1-old\nARIADNE_BASE_URL=https://openrouter.ai/api/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code := runSetup(strings.NewReader("sk-or-v1-new\n"), &out, []string{"-provider", "openrouter", "-no-check"}); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "is also set in your environment") {
		t.Errorf("spurious environment warning when key was only in config.env:\n%s", out.String())
	}
}

// When an API key really is set in the shell environment, setup notes it so
// the operator knows why the typed value is shadowed.
func TestSetupWarnsWhenEnvVarIsExportedInProcess(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-from-shell")
	_ = useConfigFile(t)
	var out strings.Builder
	if code := runSetup(strings.NewReader("sk-or-v1-new\n"), &out, []string{"-provider", "openrouter", "-no-check"}); code != exitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "OPENROUTER_API_KEY is also set in your environment") {
		t.Errorf("expected warning about process environment, got:\n%s", out.String())
	}
}
