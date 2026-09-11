package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
	// LLM calls are slow. ctx carries the real deadline; this is the backstop
	// for callers who pass context.Background().
	defaultTimeout   = 120 * time.Second
	maxErrBodyLength = 512
)

// OpenAI speaks the OpenAI chat-completions protocol. That protocol is a de
// facto standard, so the same type reaches OpenRouter, Gemini's compatibility
// endpoint, Groq, Together and a local Ollama — only BaseURL changes.
type OpenAI struct {
	APIKey  string
	BaseURL string
	Extra   http.Header // e.g. OpenRouter's HTTP-Referer / X-Title
	HTTP    *http.Client
}

// Compile-time proof. Fails at build time rather than at a call site elsewhere.
var _ Provider = (*OpenAI)(nil)

// String redacts the key. This struct will end up inside a %+v eventually — in
// a log line, or wrapped into an error — and that is how keys actually leak.
func (o *OpenAI) String() string {
	key := "<unset>"
	if o.APIKey != "" {
		key = "<redacted>"
	}
	return fmt.Sprintf("OpenAI{BaseURL:%q APIKey:%s}", o.BaseURL, key)
}

type OpenAIOption func(*OpenAI)

func WithBaseURL(u string) OpenAIOption {
	return func(o *OpenAI) { o.BaseURL = strings.TrimRight(u, "/") }
}

func WithHeader(k, v string) OpenAIOption {
	return func(o *OpenAI) {
		if o.Extra == nil {
			o.Extra = http.Header{}
		}
		o.Extra.Set(k, v)
	}
}

func WithHTTPClient(c *http.Client) OpenAIOption {
	return func(o *OpenAI) { o.HTTP = c }
}

func NewOpenAI(apiKey string, opts ...OpenAIOption) *OpenAI {
	o := &OpenAI{
		APIKey:  apiKey,
		BaseURL: defaultBaseURL,
		HTTP:    &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

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
	for _, tc := range c.Message.ToolCalls {
		// Pass the arguments through unparsed. Weak models emit malformed JSON
		// here; the right place for that to fail is the tool's Unmarshal, which
		// becomes an IsError block the model can recover from.
		args := json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		blocks = append(blocks, Block{
			Type: BlockToolUse,
			ID:   tc.ID,
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

func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	wire, err := toWire(req)
	if err != nil {
		return Response{}, err
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return Response{}, fmt.Errorf("openai: encode request: %w", err)
	}

	base := o.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("openai: build request: %w", err)
	}

	// Extra first, ours second — so nothing in Extra can clobber auth.
	for k, vs := range o.Extra {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)

	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("openai: request failed: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body) // drain, so the connection returns to the pool usable
		resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("openai: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Response{}, fmt.Errorf("openai: %s: %s", resp.Status, truncate(body, maxErrBodyLength))
	}

	return fromWire(body)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
