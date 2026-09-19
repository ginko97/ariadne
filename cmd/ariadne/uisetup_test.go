package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

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

// stubCheck replaces the key check with one that records what it was given,
// so a named provider can be configured without a request to it.
func stubCheck(t *testing.T) *[]string {
	t.Helper()
	var got []string
	saved := checkKeyFn
	checkKeyFn = func(_ context.Context, url, key, model string) error {
		got = append(got, url+" "+key+" "+model)
		return nil
	}
	t.Cleanup(func() { checkKeyFn = saved })
	return &got
}

// Two keys saved: switching provider with the key field empty uses the key
// saved under that provider's own name — not ARIADNE_API_KEY, which would go
// to every endpoint — and does not copy it into config.env.
func TestUIConfigureSwitchesToASavedProviderKey(t *testing.T) {
	cfg := isolateSettings(t)
	checks := stubCheck(t)
	os.Setenv("OPENROUTER_API_KEY", "sk-or-saved-111111")
	os.Setenv("GEMINI_API_KEY", "gm-saved-key-222222")
	os.Setenv("ARIADNE_API_KEY", "wildcard-333333333")
	p := &uiProvider{baseURL: providers["openrouter"], key: "sk-or-saved-111111", model: "m"}

	if _, err := p.configure(context.Background(), server.SetupRequest{Provider: "gemini"}); err != nil {
		t.Fatal(err)
	}
	want := providers["gemini"] + " gm-saved-key-222222 " + defaultModelFor(providers["gemini"])
	if len(*checks) != 1 || (*checks)[0] != want {
		t.Errorf("checked %q, want %q", *checks, want)
	}
	if b, k, _ := p.current(); b != providers["gemini"] || k != "gm-saved-key-222222" {
		t.Errorf("current = %q %q", b, k)
	}
	data, _ := os.ReadFile(cfg)
	if strings.Contains(string(data), "gm-saved") || strings.Contains(string(data), "GEMINI_API_KEY") {
		t.Errorf("the saved key was copied into config.env:\n%s", data)
	}
	if !strings.Contains(string(data), "ARIADNE_BASE_URL="+providers["gemini"]) {
		t.Errorf("config.env does not point at Gemini:\n%s", data)
	}

	// And back: OpenRouter's own key, again from the empty field.
	if _, err := p.configure(context.Background(), server.SetupRequest{Provider: "openrouter"}); err != nil {
		t.Fatal(err)
	}
	if _, k, _ := p.current(); k != "sk-or-saved-111111" {
		t.Errorf("switched back with key %q", k)
	}
}

// A URL typed as "Other" never receives a saved key: ARIADNE_API_KEY is set,
// and the empty field still asks for one.
func TestUIConfigureNeverSendsASavedKeyToATypedURL(t *testing.T) {
	isolateSettings(t)
	checks := stubCheck(t)
	os.Setenv("ARIADNE_API_KEY", "wildcard-333333333")
	p := &uiProvider{baseURL: providers["openrouter"], key: "sk-or", model: "m"}
	_, err := p.configure(context.Background(), server.SetupRequest{Provider: "other", BaseURL: "https://api.example.test/v1"})
	if err == nil || !strings.Contains(err.Error(), "enter the key") {
		t.Errorf("err = %v, want a request for the key", err)
	}
	if len(*checks) != 0 {
		t.Errorf("a key was checked against the typed URL: %q", *checks)
	}
}

// The page learns that a key is saved, never the key.
func TestUIStatusSaysWhichKeysAreSaved(t *testing.T) {
	isolateSettings(t)
	os.Setenv("GEMINI_API_KEY", "gm-saved-key-222222")
	st := (&uiProvider{}).status()
	saved := map[string]bool{}
	for _, pr := range st.Providers {
		saved[pr.ID] = pr.KeySaved
	}
	if !saved["gemini"] || saved["openai"] || saved["openrouter"] {
		t.Errorf("key_saved = %v, want gemini only", saved)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "gm-saved") {
		t.Errorf("the status carries the key: %s", b)
	}
}

func TestDescribeCheckErrorRuneBoundary(t *testing.T) {
	prefix := strings.Repeat("a", 299)
	err := errors.New(prefix + "€" + "tail")
	msg := describeCheckError(err)
	if !utf8.ValidString(msg) {
		t.Errorf("describeCheckError produced invalid UTF-8: %q", msg)
	}
	if !strings.HasSuffix(msg, "…") {
		t.Errorf("describeCheckError did not append ellipsis: %q", msg)
	}
	if strings.Contains(msg, "€") {
		t.Errorf("describeCheckError should have truncated before the split rune")
	}
}
