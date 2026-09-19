package llm

import (
	"context"
	"encoding/json"
	"strings"
)

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Cost is what the provider says it charged, in USD.
	//
	// Gateways that know the real price report it — OpenRouter does, in
	// usage.cost. Providers that do not leave this zero, and the caller falls
	// back to its own price table. Preferring the reported number means the
	// cost ceiling does not depend on a table we have to keep in step with
	// somebody else's pricing page.
	Cost float64 `json:"cost,omitempty"`
	// CostReported says the provider sent a cost at all. Without it a zero
	// Cost is ambiguous: a free model on OpenRouter reports 0 and means it,
	// while most providers say nothing, and "nothing" is not "free".
	CostReported bool `json:"cost_reported,omitempty"`
}

// Reported says this carries anything from the provider: a streamed chunk
// with usage in it, as opposed to one without.
func (u Usage) Reported() bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.Cost > 0 || u.CostReported
}

type StopReason string

const (
	StopEnd      StopReason = "end"
	StopToolUse  StopReason = "tool_use"
	StopMaxToken StopReason = "max_tokens"
)

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
}

type Response struct {
	Blocks []Block    `json:"blocks"`
	Stop   StopReason `json:"stop"`
	Usage  Usage      `json:"usage"`

	// Model is what answered, as the provider reports it — not what was asked
	// for. A gateway may route an alias or a fallback, and a run that records
	// only the request cannot tell the difference.
	Model string `json:"model,omitempty"`
	// Provider is the backend that served it, where the gateway says. One model
	// id is served by several, and they differ in quantisation, context
	// handling and latency, so "the same model" across two sweeps is a weaker
	// claim than it looks. Empty when the endpoint does not report one.
	Provider string `json:"provider,omitempty"`
}

// ToolResult is what a tool produced. It becomes a BlockToolResult in the
// conversation, which is why the fields mirror that block's.
type ToolResult struct {
	// Content is the output, flattened to a string. MCP's CallToolResult carries
	// []Content (text, image, audio, embedded resource); we keep text only,
	// because Block.Content is a string on the wire regardless.
	Content string

	// IsError means the tool ran and reported failure — distinct from Call
	// returning an error, which means the tool could not be reached at all.
	IsError bool

	// Untrusted marks content that came from outside the system — a fetched
	// document, a scraped page, anything somebody else wrote. The loop fences
	// such content so the model can tell data from instructions.
	//
	// It is a property of the tool, not of the content: a tool that can return
	// attacker-controlled text always returns untrusted text, whether or not any
	// particular result looks hostile.
	Untrusted bool

	// Metadata carries opaque tokens only, e.g. MCP's RequestState for resuming
	// across retries. Not a general bag: if you want a typed field, add one.
	Metadata map[string]string
}

// Text joins the text blocks with newlines. Blocks stay authoritative — this is
// derived, never stored, so a resumed run can never disagree with a fresh one.
func (r Response) Text() string {
	var parts []string
	for _, b := range r.Blocks {
		if b.Type == BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ToolCalls extracts the tool_use blocks as the shape the loop wants.
// Returns nil (not an empty slice) when there are none, so `len(...) == 0`
// and `== nil` agree.
func (r Response) ToolCalls() []ToolCall {
	var calls []ToolCall
	for _, b := range r.Blocks {
		if b.Type != BlockToolUse {
			continue
		}
		calls = append(calls, ToolCall{ID: b.ID, Name: b.Name, Args: b.Args})
	}
	return calls
}

type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}
