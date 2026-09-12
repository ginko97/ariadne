package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Wire types mirror OpenAI's JSON exactly. Unexported — nothing outside this
// file should know this shape exists.

type oaRequest struct {
	Model    string      `json:"model"`
	Messages []oaMessage `json:"messages"`
	Tools    []oaTool    `json:"tools,omitempty"`
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    *string      `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"` // always "function"
	Function oaFunction `json:"function"`
}

type oaFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaTool struct {
	Type     string   `json:"type"` // "function"
	Function oaToolFn `json:"function"`
}

type oaToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaResponse struct {
	Choices []oaChoice `json:"choices"`
	Usage   oaUsage    `json:"usage"`
	Error   *oaError   `json:"error"`
}

type oaChoice struct {
	Index        int       `json:"index"`
	Message      oaMessage `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type oaError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// ErrNoChoices is returned when a well-formed response carries no choices.
// Distinct from a decode failure: the server answered, it just said nothing.
var ErrNoChoices = errors.New("openai: response contained no choices")

// stopReason maps finish_reason, falling back to the payload when the value is
// missing or unrecognised. OpenRouter proxies many backends and not all of them
// send what the spec says.

func toWire(req Request) (oaRequest, error) {
	out := oaRequest{Model: req.Model}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, oaTool{
			Type: "function",
			Function: oaToolFn{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}

	for _, m := range req.Messages {
		var text []string
		var calls []oaToolCall

		for _, b := range m.Blocks {
			switch b.Type {
			case BlockText:
				if b.Text != "" {
					text = append(text, b.Text)
				}

			case BlockToolUse:
				args := string(b.Args)
				if args == "" {
					args = "{}" // an empty string is not valid JSON; servers reject it
				}
				calls = append(calls, oaToolCall{
					ID:       b.ID,
					Type:     "function",
					Function: oaFunction{Name: b.Name, Arguments: args},
				})

			case BlockToolResult:
				// Each result is its own top-level message with role "tool".
				content := b.Content
				out.Messages = append(out.Messages, oaMessage{
					Role:       "tool",
					ToolCallID: b.CallID,
					Content:    &content,
				})

			default:
				return oaRequest{}, fmt.Errorf("openai: unknown block type %q", b.Type)
			}
		}

		if len(text) == 0 && len(calls) == 0 {
			continue // message held only tool results, already emitted
		}

		msg := oaMessage{Role: string(m.Role), ToolCalls: calls}
		if len(text) > 0 {
			joined := strings.Join(text, "\n")
			msg.Content = &joined // nil when there are only tool calls → "content": null
		}
		out.Messages = append(out.Messages, msg)
	}

	return out, nil
}

func fromWire(body []byte) (Response, error) {
	var raw oaResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return Response{}, fmt.Errorf("openai: decode response: %w", err)
	}

	// First, because gateways return this envelope with HTTP 200.
	if raw.Error != nil {
		return Response{}, fmt.Errorf("openai: %s: %s", raw.Error.Type, raw.Error.Message)
	}
	if len(raw.Choices) == 0 {
		return Response{}, ErrNoChoices
	}

	c := raw.Choices[0]

	var blocks []Block
	if c.Message.Content != nil && *c.Message.Content != "" {
		blocks = append(blocks, Block{Type: BlockText, Text: *c.Message.Content})
	}
	for i, tc := range c.Message.ToolCalls {
		// Pass the arguments through unparsed. Weak models emit malformed JSON
		// here; the right place for that to fail is the tool's Unmarshal, which
		// becomes an IsError block the model can recover from.
		args := json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}

		// An id is mandatory downstream: it pairs the result back to the call,
		// and it is the completion key resume reads out of the conversation.
		// Two calls with an empty id would collapse into one and the second
		// would never run — silently. Synthesise rather than fail: a missing id
		// is the server's bug, and the run can still succeed with a local one.
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_synth_%d", i)
		}

		blocks = append(blocks, Block{
			Type: BlockToolUse,
			ID:   id,
			Name: tc.Function.Name,
			Args: args,
		})
	}

	return Response{
		Blocks: blocks,
		Stop:   stopReason(c.FinishReason, len(c.Message.ToolCalls) > 0),
		Usage: Usage{
			InputTokens:  raw.Usage.PromptTokens,
			OutputTokens: raw.Usage.CompletionTokens,
		},
	}, nil
}

func stopReason(finish string, hasToolCalls bool) StopReason {
	switch finish {
	case "stop":
		return StopEnd
	case "tool_calls", "function_call":
		return StopToolUse
	case "length":
		return StopMaxToken
	}
	if hasToolCalls {
		return StopToolUse
	}
	return StopEnd
}
