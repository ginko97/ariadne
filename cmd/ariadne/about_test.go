package main

import (
	"testing"

	"github.com/ginko97/ariadne/internal/server"
)

// The page's tool list is read off the agent the UI builds. It must show a
// tool as asking exactly when the loop will stop for it: exec and remember are
// forced, the default-gated built-ins ask unless -trust names them, and the
// rest run as soon as the model asks.
func TestToolsOfMatchesWhatTheAgentGates(t *testing.T) {
	a := newAgentFor(agentOpts{
		Key: "k", Model: "m", BaseURL: "http://127.0.0.1:1/v1",
		RunID: "run_about", Workspace: t.TempDir(), Exec: true, Memory: true,
		Trust: []string{"web_fetch"},
	})
	got := map[string]server.ToolInfo{}
	for _, ti := range toolsOf(a) {
		got[ti.Name] = ti
	}
	for name, asks := range map[string]bool{
		"exec": true, "remember": true, "write_file": true, "edit_file": true,
		"web_fetch": false, // trusted
		"calc":      false, "fetch": false, "list_files": false,
	} {
		ti, ok := got[name]
		if !ok {
			t.Errorf("%s is missing from the list", name)
			continue
		}
		if ti.Asks != asks {
			t.Errorf("%s: asks = %v, want %v", name, ti.Asks, asks)
		}
		if ti.MCP {
			t.Errorf("%s is a built-in, listed as MCP", name)
		}
	}
	if len(got) != 8 {
		t.Errorf("listed %d tools, want the 8 this agent is offered: %v", len(got), got)
	}
}

// -allow narrows what a conversation may call, and the loop refuses the rest;
// the page must not list a tool the model cannot use.
func TestToolsOfLeavesOutWhatAllowExcludes(t *testing.T) {
	a := newAgentFor(agentOpts{
		Key: "k", Model: "m", BaseURL: "http://127.0.0.1:1/v1",
		Workspace: t.TempDir(), Allow: []string{"calc", "write_file"},
	})
	tools := toolsOf(a)
	if len(tools) != 2 {
		t.Fatalf("listed %v, want only calc and write_file", tools)
	}
	for _, ti := range tools {
		if ti.Name == "write_file" && !ti.Asks {
			t.Error("write_file listed as running without a card")
		}
	}
}

// The probe `ariadne ui` lists its tools from must be offered what a real
// conversation is — remember included, which needs a run id to exist.
func TestProbeListsWhatAConversationIsOffered(t *testing.T) {
	ws := t.TempDir()
	tools := probeTools(func(runID string) agentOpts {
		return agentOpts{Key: "k", Model: "m", BaseURL: "http://127.0.0.1:1/v1", RunID: runID, Workspace: ws, Memory: true}
	})
	for _, ti := range tools {
		if ti.Name == "remember" {
			if !ti.Asks {
				t.Error("remember listed as running without a card")
			}
			return
		}
	}
	t.Errorf("remember is missing from the probe's list: %v", tools)
}
