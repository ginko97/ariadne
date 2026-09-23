package main

import (
	"strings"

	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/mcp"
	"github.com/ginko97/ariadne/internal/server"
)

// toolsOf is what a conversation run by a is offered, and which of those ask
// first — read off the agent itself rather than recomputed from the flags, so
// the page cannot describe a gate the loop does not enforce. The loop asks for
// exactly the names in RequireApproval, and refuses anything outside a
// non-empty Allow, so those are the two rules applied here.
func toolsOf(a *loop.Agent) []server.ToolInfo {
	var out []server.ToolInfo
	for _, d := range a.Tools {
		if len(a.Allow) > 0 && !contains(a.Allow, d.Name) {
			continue
		}
		out = append(out, server.ToolInfo{
			Name: d.Name,
			Asks: contains(a.RequireApproval, d.Name),
			MCP:  strings.Contains(d.Name, mcp.Separator),
		})
	}
	return out
}

// probeTools builds one agent from the UI's options, never runs it, and lists
// its tools. Nothing is traced or written. The run id is a placeholder, but not
// empty: remember is only offered to an agent with a run to stamp its notes
// with, and the first version of this passed "" and left it off the list.
func probeTools(optsFor func(runID string) agentOpts) []server.ToolInfo {
	return toolsOf(newAgentFor(optsFor("run_about")))
}
