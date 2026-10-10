package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A catalogue that does not answer must not hold the turn for the cache's
// whole timeout once the turn is stopped: BudgetFor and PriceFor run inside
// the loop, and Stop cancels the turn's context.
func TestCatalogueLookupStopsWithTheTurn(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); ts.Close() })

	for name, lookup := range map[string]func(context.Context, *ModelCache){
		"BudgetFor": func(ctx context.Context, m *ModelCache) { m.BudgetFor(ctx, "b/tools-two") },
		"PriceFor":  func(ctx context.Context, m *ModelCache) { m.PriceFor(ctx, "b/tools-two") },
	} {
		m := newCache(t, ts.URL) // its own timeout is the default 10s
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		lookup(ctx, m)
		cancel()
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s took %v after its context ended, want it to return with the context", name, d)
		}
	}
}
