package llm

import (
	"context"
	"errors"
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

// Pointed at a provider that is not OpenRouter, the picker offers the
// configured model and says why — and never reaches the network at all.
// Listing OpenRouter's catalogue here would fill the dropdown with namespaced
// ids (anthropic/…, openai/…) that the configured endpoint rejects: every row
// a model that cannot be selected.
func TestModelsUnsupportedProviderOffersTheConfiguredModelOnly(t *testing.T) {
	ts, hits := upstream(t, modelsFixture, http.StatusOK)
	m := newCache(t, ts.URL)
	m.Unsupported = "api.example.com publishes no readable model list"

	rows, source, warning := m.Get(context.Background())
	if source != "fallback" {
		t.Errorf("source = %q, want fallback", source)
	}
	if len(rows) != 1 || rows[0].ID != "configured/model" {
		t.Errorf("rows = %v, want just the configured model", rows)
	}
	if warning != m.Unsupported {
		t.Errorf("warning = %q, want the reason this provider has no list", warning)
	}
	if hits.Load() != 0 {
		t.Errorf("hit upstream %d times, want 0 — a list was fetched from a gateway "+
			"this process is not talking to", hits.Load())
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

// Upstream failures must enter a cooldown rather than blocking every subsequent
// request for the full network timeout.
func TestModelsFailureCooldownPreventsHammeringUpstream(t *testing.T) {
	ts, hits := upstream(t, "", http.StatusInternalServerError)
	m := newCache(t, ts.URL)
	m.TTL = 50 * time.Millisecond

	// First call fails and records failedAt.
	_, source1, _ := m.Get(context.Background())
	if source1 != "fallback" {
		t.Fatalf("first call source = %q, want fallback", source1)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}

	// Immediate second call is inside cooldown: must not hit upstream again.
	_, source2, _ := m.Get(context.Background())
	if source2 != "fallback" {
		t.Fatalf("second call source = %q, want fallback", source2)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d inside cooldown, want 1 — upstream was hammered again", hits.Load())
	}

	// After cooldown expires, the next call attempts a refresh again.
	time.Sleep(60 * time.Millisecond)
	_, _, _ = m.Get(context.Background())
	if hits.Load() != 2 {
		t.Errorf("hits = %d after cooldown expired, want 2", hits.Load())
	}
}

// poisonTransport fails any request it sees, so a test can prove a client is
// not the one being used.
type poisonTransport struct{}

func (poisonTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("http.DefaultClient was used")
}

// A ModelCache built as a literal, with no Client, does not fall back to
// http.DefaultClient — shared process state that any imported package can
// reconfigure. Proved by poisoning it for the duration: the fetch still works.
// Not parallel-safe, and nothing in this package runs in parallel.
func TestModelsNeverUsesTheDefaultClient(t *testing.T) {
	ts, _ := upstream(t, modelsFixture, http.StatusOK)

	saved := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: poisonTransport{}}
	t.Cleanup(func() { http.DefaultClient = saved })

	m := &ModelCache{URL: ts.URL, TTL: time.Minute, Fallback: "configured/model"}
	rows, source, warning := m.Get(context.Background())
	if source != "live" || len(rows) == 0 {
		t.Errorf("source=%q rows=%d warning=%q; want a live fetch without the default client", source, len(rows), warning)
	}
}

func TestModelsConcurrentGetDoesNotBlockStaleCache(t *testing.T) {
	// One handler for the whole test: swapping ts.Config.Handler while the
	// server is running is itself a data race. Phase 0 answers at once; phase
	// 1 reports that a fetch started, then holds it until release is closed.
	var phase atomic.Int32
	fetchStarted := make(chan struct{}, 8)
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 1 {
			select {
			case fetchStarted <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write([]byte(modelsFixture))
	}))
	defer ts.Close()
	// Registered after ts.Close, so it runs first: a held fetch is let go
	// before Close waits for it, and a failing test fails instead of hanging.
	var releaseOnce atomic.Bool
	unblock := func() {
		if releaseOnce.CompareAndSwap(false, true) {
			close(release)
		}
	}
	defer unblock()

	m := newCache(t, ts.URL)
	m.TTL = 10 * time.Millisecond
	if _, source, _ := m.Get(context.Background()); source != "live" {
		t.Fatalf("initial source = %q, want live", source)
	}
	time.Sleep(20 * time.Millisecond) // the cached list is now stale

	phase.Store(1)
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		m.Get(context.Background())
	}()
	select {
	case <-fetchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the background refresh never reached the server")
	}

	// A second caller while that fetch is in flight gets the stale list at
	// once, and does not start a fetch of its own.
	type result struct {
		rows   []ModelRow
		source string
	}
	got := make(chan result, 1)
	go func() {
		rows, source, _ := m.Get(context.Background())
		got <- result{rows, source}
	}()
	select {
	case r := <-got:
		if r.source != "cache" || len(r.rows) != 3 {
			t.Errorf("during an in-flight fetch: source=%q rows=%d, want cache and 3", r.source, len(r.rows))
		}
	case <-time.After(2 * time.Second):
		t.Error("Get blocked behind the in-flight fetch instead of serving the stale list")
	}
	select {
	case <-fetchStarted:
		t.Error("a second fetch started while one was in flight")
	default:
	}

	unblock()
	select {
	case <-refreshDone:
	case <-time.After(5 * time.Second):
		t.Error("the background refresh did not finish after release")
	}
}

// With a local list set, the picker shows what the local server has, asked
// fresh each time, and OpenRouter is never contacted.
func TestLocalListReplacesTheCatalogue(t *testing.T) {
	ts, hits := upstream(t, `{"data":[]}`, http.StatusOK)
	m := NewModelCache("qwen3:4b")
	m.URL = ts.URL
	pulled := []string{"qwen3:4b", "llama3.2:3b"}
	m.Reconfigure("qwen3:4b", "", func(context.Context) ([]string, error) { return pulled, nil })

	rows, source, warning := m.Get(context.Background())
	if source != "local" || warning != "" || len(rows) != 2 || rows[1].ID != "llama3.2:3b" {
		t.Fatalf("got %v %q %q", rows, source, warning)
	}
	pulled = append(pulled, "qwen3:1.7b") // pulled a moment ago
	if rows, _, _ := m.Get(context.Background()); len(rows) != 3 {
		t.Errorf("a newly pulled model is missing: %v", rows)
	}
	if hits.Load() != 0 {
		t.Errorf("OpenRouter was asked %d times while the list was local", hits.Load())
	}

	// Back to a remote endpoint: the local list is gone.
	m.Reconfigure("gpt-4o-mini", "no list here", nil)
	if _, source, warning := m.Get(context.Background()); source != "fallback" || warning != "no list here" {
		t.Errorf("after reconfiguring away from local: %q %q", source, warning)
	}
}

// The configured model stays in the picker even when the server no longer
// has it, with a warning that says so; a server that cannot be reached
// leaves the configured model and the reason.
func TestLocalListKeepsTheConfiguredModelAndSaysWhy(t *testing.T) {
	m := NewModelCache("qwen3:8b")
	m.Reconfigure("qwen3:8b", "", func(context.Context) ([]string, error) { return []string{"qwen3:4b"}, nil })
	rows, _, warning := m.Get(context.Background())
	if len(rows) != 2 || rows[0].ID != "qwen3:8b" || warning == "" {
		t.Errorf("configured model not pulled: %v, warning %q", rows, warning)
	}

	m.Reconfigure("qwen3:8b", "", func(context.Context) ([]string, error) { return nil, errors.New("connection refused") })
	rows, source, warning := m.Get(context.Background())
	if source != "fallback" || len(rows) != 1 || rows[0].ID != "qwen3:8b" || warning == "" {
		t.Errorf("unreachable: %v %q %q", rows, source, warning)
	}
}
