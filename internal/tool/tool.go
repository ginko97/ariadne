package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ginko97/ariadne/internal/llm"
)

// Tool is one thing the model can ask for.
//
// STAGE 3 — PROVISIONAL. At the stage-4b freeze this widens to
//
//	Call(ctx context.Context, callID string, args json.RawMessage) (Result, error)
//
// because a string cannot carry is_error, MCP content blocks, or an idempotency
// key. Do not build Module 1 (checkpoint/resume) on the current shape.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Call(ctx context.Context, args json.RawMessage) (string, error)
}

var ErrUnknownTool = fmt.Errorf("tool: unknown tool")

// Registry maps a name to a Tool. It is the typed replacement for Python's
// dynamic dispatch on tool name — the model sends a string, this turns it into
// a call on a concrete implementation, or a clean error.
type Registry struct {
	tools map[string]Tool
}

func New(tools ...Tool) *Registry {
	r := &Registry{tools: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		r.tools[t.Name()] = t
	}
	return r
}

// Defs renders the registry as the tool list sent to the model, sorted by name.
//
// Sorted on purpose: a stable order keeps the prompt byte-identical between
// runs, which keeps provider prompt caches warm and keeps golden fixtures from
// churning on map iteration order.
func (r *Registry) Defs() []llm.ToolDef {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)

	defs := make([]llm.ToolDef, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		defs = append(defs, llm.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		})
	}
	return defs
}

// Call dispatches one tool call. Its signature matches loop.ToolRunner, so an
// Agent is wired with `agent.RunTool = registry.Call`.
//
// An unknown name is a normal error, not a panic: the loop turns it into a
// tool_result with IsError set, and the model gets to pick a different tool.
func (r *Registry) Call(ctx context.Context, c llm.ToolCall) (string, error) {
	t, ok := r.tools[c.Name]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTool, c.Name)
	}
	return t.Call(ctx, c.Args)
}
