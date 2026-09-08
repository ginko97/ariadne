package llm

import (
	"context"
	"encoding/json"
	"strings"
)

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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
