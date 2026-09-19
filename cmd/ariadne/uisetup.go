package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/server"
)

// shellEnv names the settings that were in the process environment before any
// file was loaded: what the person exported in their shell. Those outrank
// config.env on every start, so setup from the page must not quietly replace
// them in this process either — a restart would bring the shell's value back,
// and the page would have shown a configuration that does not survive it.
var shellEnv = map[string]bool{}

// settingNames are the variables a configuration can write.
var settingNames = []string{
	"ARIADNE_API_KEY", "ARIADNE_BASE_URL", "ARIADNE_MODEL",
	"OPENROUTER_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY",
}

// snapshotShellEnv records which settings came from the shell. Called first
// thing in main, before .env or config.env can add to the environment.
func snapshotShellEnv() {
	for _, name := range settingNames {
		if _, ok := os.LookupEnv(name); ok {
			shellEnv[name] = true
		}
	}
}

// uiProvider is the endpoint, key and model `ariadne ui` is using, which the
// page can change while the server runs. Every new agent reads it once, so a
// turn never sees half of a change.
type uiProvider struct {
	mu      sync.Mutex
	baseURL string
	key     string
	model   string
	notes   []string
	models  *llm.ModelCache
}

func (p *uiProvider) current() (baseURL, key, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.baseURL, p.key, p.model
}

// status is what the page may know. No key: server.SetupStatus has no field
// for one.
func (p *uiProvider) status() server.SetupStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return server.SetupStatus{
		Configured: p.key != "",
		Provider:   providerID(p.baseURL),
		BaseURL:    p.baseURL,
		Model:      p.model,
		KeySet:     p.key != "" && p.key != localNoKey,
		Notes:      append([]string(nil), p.notes...),
		Providers:  setupProviders(),
	}
}

// checkKeyFn is checkKey, replaceable so a test can configure a named
// provider without a request to it.
var checkKeyFn = checkKey

// savedKey is the key already set up for a named provider's endpoint, under
// that provider's own name, or "".
func savedKey(endpoint string) string {
	if name := providerKeyName(endpoint); name != "" {
		return os.Getenv(name)
	}
	return ""
}

// setupProviders are the choices on the setup card, in the order the
// terminal's setup offers them, OpenRouter first.
func setupProviders() []server.SetupProvider {
	named := []struct{ id, name string }{
		{"openrouter", "OpenRouter"}, {"openai", "OpenAI"}, {"gemini", "Google Gemini"}, {"xai", "xAI"},
	}
	var out []server.SetupProvider
	for _, n := range named {
		u := providers[n.id]
		out = append(out, server.SetupProvider{
			ID: n.id, Name: n.name, BaseURL: u, NeedsKey: true, KeySaved: savedKey(u) != "",
			KeyName: providerKeyName(u), DefaultModel: defaultModelFor(u),
		})
	}
	return append(out,
		server.SetupProvider{ID: "ollama", Name: "Ollama (on this computer)", BaseURL: ollamaURL},
		server.SetupProvider{ID: "other", Name: "Other (OpenAI-compatible)", NeedsKey: true, KeyName: "ARIADNE_API_KEY"},
	)
}

// providerID names the setup choice an endpoint belongs to.
func providerID(baseURL string) string {
	for id, u := range providers {
		if u == baseURL && id != "ollama" {
			return id
		}
	}
	if isLoopbackURL(baseURL) && strings.Contains(baseURL, ":11434") {
		return "ollama"
	}
	return "other"
}

// configure checks a configuration the way `ariadne setup` does — one real
// request, with a tool on offer — writes it to the same config.env, and
// switches this process to it. Nothing is written or switched when the check
// fails.
func (p *uiProvider) configure(ctx context.Context, req server.SetupRequest) (server.SetupStatus, error) {
	for _, f := range []string{req.Provider, req.BaseURL, req.Key, req.Model} {
		// A newline in any of these would be a second line in config.env: a
		// key could set ARIADNE_BASE_URL on its way in.
		if strings.ContainsFunc(f, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return server.SetupStatus{}, errors.New("a field contains a control character")
		}
	}
	id := strings.ToLower(strings.TrimSpace(req.Provider))
	base := strings.TrimSpace(req.BaseURL)
	key := strings.TrimSpace(req.Key)
	model := strings.TrimSpace(req.Model)

	var endpoint string
	switch {
	case id == "ollama":
		endpoint = base
		if endpoint == "" {
			endpoint = ollamaURL
		}
	case id == "other":
		if base == "" {
			return server.SetupStatus{}, errors.New("enter the endpoint URL")
		}
		endpoint = base
	case providers[id] != "":
		endpoint = providers[id]
	default:
		return server.SetupStatus{}, fmt.Errorf("unknown provider %q", req.Provider)
	}
	if u, err := url.Parse(endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return server.SetupStatus{}, fmt.Errorf("%q is not an http or https URL", endpoint)
	}

	curURL, curKey, _ := p.current()
	reused := false
	if id == "ollama" {
		if model == "" {
			models, err := ollamaModels(ctx, endpoint)
			if err != nil {
				return server.SetupStatus{}, fmt.Errorf("Ollama is not answering at %s (%v); open the Ollama app or run `ollama serve`", ollamaRoot(endpoint), err)
			}
			if len(models) == 0 {
				return server.SetupStatus{}, errors.New("Ollama has no models; pull one that can call tools, e.g. `ollama pull qwen3`")
			}
			model = models[0]
		}
		key = ""
		if !isLoopbackURL(endpoint) {
			key = localNoKey
		}
	} else if key == "" {
		// Blank keeps a key already saved, so switching between providers
		// whose keys are both set up needs no pasting. For a named provider
		// that is the key under its own name — GEMINI_API_KEY for Gemini, never
		// ARIADNE_API_KEY, which goes everywhere. For any other endpoint, only
		// the one in use, so a saved key never goes to a URL typed here.
		switch {
		case endpoint == curURL && curKey != "" && curKey != localNoKey:
			key, reused = curKey, true
		case savedKey(endpoint) != "":
			key, reused = savedKey(endpoint), true
		case isLoopbackURL(endpoint):
		default:
			return server.SetupStatus{}, errors.New("enter the key")
		}
	}
	if model == "" {
		model = defaultModelFor(endpoint)
	}

	check := key
	if check == "" {
		check = localNoKey
	}
	if err := checkKeyFn(ctx, endpoint, check, model); err != nil {
		return server.SetupStatus{}, fmt.Errorf("checking %s with %s failed: %s", endpoint, model, describeCheckError(err))
	}

	vals := configVals(endpoint, key, model)
	if name := providerKeyName(endpoint); reused && name != "" {
		// The saved key may live in the environment or a checkout's .env
		// rather than config.env; it is not copied anywhere new.
		delete(vals, name)
	}
	if err := writeConfigEnv(configEnvFile, vals); err != nil {
		return server.SetupStatus{}, err
	}
	notes := applySettings(vals)

	// What apiKey now resolves, rather than the key just typed: a key
	// exported in the shell still wins, and this process should do what a
	// restart would.
	effective, _ := apiKey(endpoint)
	unsupported := ""
	if !strings.Contains(endpoint, "openrouter.ai") {
		unsupported = noModelList(endpoint)
	}
	p.mu.Lock()
	p.baseURL, p.key, p.model, p.notes = endpoint, effective, model, notes
	p.mu.Unlock()
	if p.models != nil {
		p.models.Reconfigure(model, unsupported)
	}
	return p.status(), nil
}

// applySettings makes this process see what config.env now says, except where
// the shell set a value: that one outranks config.env on every start, so it is
// left in place and reported.
func applySettings(vals map[string]string) []string {
	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Strings(names)
	var notes []string
	for _, n := range names {
		v := vals[n]
		if shellEnv[n] {
			if os.Getenv(n) != v {
				notes = append(notes, n+" is set in your shell and outranks config.env; unset it for this setting to take effect")
			}
			continue
		}
		if v == "" {
			_ = os.Unsetenv(n)
		} else {
			_ = os.Setenv(n, v)
		}
	}
	return notes
}

// describeCheckError turns a failed check into something to show on the page.
// A refused key gets a fixed message: a provider's 401 body can quote the key
// back, masked or not, and the page must never receive it.
func describeCheckError(err error) string {
	msg := err.Error()
	for _, code := range []string{"401", "403"} {
		if strings.Contains(msg, ": "+code+" ") {
			return "the provider refused the key (HTTP " + code + "); check it and try again"
		}
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}
