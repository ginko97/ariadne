package main

import (
	"encoding/json"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
)

// The grant key is the destination a URL sends data to. Spellings of one
// destination share a key; different destinations never do.
func TestWebFetchGrantKeyIsTheOrigin(t *testing.T) {
	call := func(name, u string) llm.ToolCall {
		args, _ := json.Marshal(map[string]string{"url": u})
		return llm.ToolCall{Name: name, Args: args}
	}
	for _, tc := range []struct {
		name, url, want string
		ok              bool
	}{
		{"web_fetch", "https://github.com/ginko97/ariadne", "https://github.com", true},
		{"web_fetch", "https://GitHub.COM./x", "https://github.com", true},
		{"web_fetch", "https://github.com:8443/x", "https://github.com:8443", true},
		// The same host over plaintext is a different destination: the URL
		// is what carries data out, and here anyone on the path can read it.
		{"web_fetch", "http://github.com/x", "http://github.com", true},
		// A subdomain is another host, and asks separately.
		{"web_fetch", "https://api.github.com/repos", "https://api.github.com", true},
		{"web_fetch", "https://[::1]:8080/x", "https://[::1]:8080", true},
		{"web_fetch", "ftp://github.com/x", "", false},
		{"web_fetch", "not a url", "", false},
		// Nothing but web_fetch is ever grantable.
		{"write_file", "https://github.com/x", "", false},
		{"exec", "https://github.com/x", "", false},
		{"fs__read_text_file", "https://github.com/x", "", false},
	} {
		got, ok := webFetchGrantKey(call(tc.name, tc.url))
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s %q = (%q, %v), want (%q, %v)", tc.name, tc.url, got, ok, tc.want, tc.ok)
		}
	}
}

// Every agent carries the policy, so a front end that can grant gets the same
// rule wherever it builds its agent.
func TestAgentsCarryTheGrantPolicy(t *testing.T) {
	a := newAgentFor(agentOpts{Key: "k", Model: "m", BaseURL: "https://example.test/v1", RunID: "run_test", MaxSteps: 5,
		Store: nil})
	if a.GrantKey == nil {
		t.Fatal("newAgentFor built an agent with no grant policy")
	}
	// A url field on purpose: without one the URL parse refuses the call and
	// this would pass whatever the rule about tool names said. Only the name
	// may make the difference here.
	args, _ := json.Marshal(map[string]string{"url": "https://github.com/x", "path": "x.txt", "content": "y"})
	for _, name := range []string{"write_file", "edit_file", "exec"} {
		if _, ok := a.GrantKey(llm.ToolCall{Name: name, Args: args}); ok {
			t.Errorf("%s is grantable; each change must be its own decision", name)
		}
	}
}
