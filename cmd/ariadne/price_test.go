package main

import "testing"

// Ollama runs the model on this machine, so its steps cost a known nothing
// and -max-cost does not stop them as unpriced. Any other endpoint without a
// listed price stays unknown: a loopback address can be a proxy to a paid API.
func TestOllamaIsPricedFree(t *testing.T) {
	for base, wantFree := range map[string]bool{
		"http://localhost:11434/v1": true,
		"http://127.0.0.1:11434/v1": true,
		"http://localhost:4000/v1":  false,
		"https://example.test/v1":   false,
	} {
		a := newAgentFor(agentOpts{Key: "k", Model: "m", BaseURL: base, RunID: "run_test", MaxSteps: 5})
		if got := a.PriceResolver("qwen3:4b").Free; got != wantFree {
			t.Errorf("%s: Free = %v, want %v", base, got, wantFree)
		}
	}
}
