package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/server"
)

// isolateSettings gives a test its own config.env and restores every setting
// variable afterwards: configure changes the process environment on purpose.
func isolateSettings(t *testing.T) string {
	t.Helper()
	cfg := useConfigFile(t)
	for _, n := range settingNames {
		t.Setenv(n, "")
		os.Unsetenv(n)
	}
	saved := shellEnv
	shellEnv = map[string]bool{}
	t.Cleanup(func() { shellEnv = saved })
	return cfg
}

// fakeProvider answers chat completions with status, recording the bearer
// token of each request.
func fakeProvider(t *testing.T, status int, body string) (url string, auth *atomic.Value) {
	t.Helper()
	auth = &atomic.Value{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", auth
}

// A working key: checked against the endpoint, written to config.env, and
// in use by this process at once — apiKey, the provider, the model list.
func TestUIConfigureSwitchesTheProvider(t *testing.T) {
	cfg := isolateSettings(t)
	url, auth := fakeProvider(t, 200, okCompletion)
	p := &uiProvider{models: llm.NewModelCache("old-model")}

	st, err := p.configure(context.Background(), server.SetupRequest{Provider: "other", BaseURL: url, Key: "sk-new-key-123456", Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := auth.Load(); got != "Bearer sk-new-key-123456" {
		t.Errorf("the check carried %v, not the submitted key", got)
	}
	if !st.Configured || st.Model != "m1" || st.BaseURL != url {
		t.Errorf("status = %+v", st)
	}
	data, _ := os.ReadFile(cfg)
	for _, want := range []string{"ARIADNE_API_KEY=sk-new-key-123456", "ARIADNE_BASE_URL=" + url, "ARIADNE_MODEL=m1"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config.env lacks %q:\n%s", want, data)
		}
	}
	if b, k, m := p.current(); b != url || k != "sk-new-key-123456" || m != "m1" {
		t.Errorf("current = %q %q %q", b, k, m)
	}
	if k, _ := apiKey(url); k != "sk-new-key-123456" {
		t.Errorf("apiKey after configure = %q", k)
	}
	if p.models.Configured() != "m1" {
		t.Errorf("the model list still offers %q", p.models.Configured())
	}
}

// A refused key: nothing written, nothing switched, and the message the page
// gets does not carry the key even when the provider's body quotes it.
func TestUIConfigureRefusedKeyChangesNothing(t *testing.T) {
	cfg := isolateSettings(t)
	url, _ := fakeProvider(t, 401, `{"error":{"message":"Incorrect API key provided: sk-bad-key-987654"}}`)
	p := &uiProvider{baseURL: "https://openrouter.ai/api/v1", key: "sk-or-kept", model: "kept"}

	_, err := p.configure(context.Background(), server.SetupRequest{Provider: "other", BaseURL: url, Key: "sk-bad-key-987654", Model: "m"})
	if err == nil {
		t.Fatal("a refused key was accepted")
	}
	if strings.Contains(err.Error(), "sk-bad-key") || !strings.Contains(err.Error(), "refused the key") {
		t.Errorf("error = %q; want the fixed refusal and no key", err)
	}
	if _, statErr := os.Stat(cfg); !os.IsNotExist(statErr) {
		t.Errorf("config.env written for a refused key (%v)", statErr)
	}
	if b, k, m := p.current(); b != "https://openrouter.ai/api/v1" || k != "sk-or-kept" || m != "kept" {
		t.Errorf("provider switched after a refusal: %q %q %q", b, k, m)
	}
}

// An empty key keeps the one in use for the same endpoint, so the model can
// change without pasting the key again; for a new endpoint it is refused.
func TestUIConfigureBlankKey(t *testing.T) {
	isolateSettings(t)
	url, auth := fakeProvider(t, 200, okCompletion)
	p := &uiProvider{baseURL: url, key: "sk-kept-key-123456", model: "old"}
	if _, err := p.configure(context.Background(), server.SetupRequest{Provider: "other", BaseURL: url, Model: "new"}); err != nil {
		t.Fatal(err)
	}
	if got := auth.Load(); got != "Bearer sk-kept-key-123456" {
		t.Errorf("check carried %v, want the kept key", got)
	}

	p = &uiProvider{baseURL: "https://openrouter.ai/api/v1", key: "sk-or-other", model: "x"}
	if _, err := p.configure(context.Background(), server.SetupRequest{Provider: "openai"}); err == nil || !strings.Contains(err.Error(), "enter the key") {
		t.Errorf("blank key for a new provider: %v, want a request for the key", err)
	}
}

// A field with a newline would be a second line in config.env. The model is
// the dangerous one: it travels in the JSON body, so the check request
// succeeds with it, where a newline in the key is refused by net/http as a
// header before anything is sent.
func TestUIConfigureRefusesControlCharacters(t *testing.T) {
	cfg := isolateSettings(t)
	url, _ := fakeProvider(t, 200, okCompletion)
	p := &uiProvider{}
	_, err := p.configure(context.Background(), server.SetupRequest{
		Provider: "other", BaseURL: url, Key: "sk-key-123456789", Model: "m1\nOPENROUTER_API_KEY=sk-attacker"})
	if err == nil {
		t.Error("a model with a newline was accepted")
	}
	if data, _ := os.ReadFile(cfg); strings.Contains(string(data), "sk-attacker") {
		t.Errorf("a newline in the model wrote a second setting:\n%s", data)
	}
}

// A value exported in the shell outranks config.env on every start, so the
// page leaves it and says so, rather than showing a setting a restart undoes.
func TestUIConfigureLeavesShellSettings(t *testing.T) {
	isolateSettings(t)
	url, _ := fakeProvider(t, 200, okCompletion)
	t.Setenv("ARIADNE_MODEL", "from-shell")
	shellEnv["ARIADNE_MODEL"] = true

	st, err := (&uiProvider{}).configure(context.Background(), server.SetupRequest{Provider: "other", BaseURL: url, Key: "sk-key-123456789", Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ARIADNE_MODEL") != "from-shell" {
		t.Errorf("ARIADNE_MODEL = %q; the shell's value was replaced", os.Getenv("ARIADNE_MODEL"))
	}
	if len(st.Notes) != 1 || !strings.Contains(st.Notes[0], "ARIADNE_MODEL") {
		t.Errorf("notes = %q, want one about ARIADNE_MODEL", st.Notes)
	}
}
