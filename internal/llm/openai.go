package llm

import (
	"bytes"
	"context"
	"encoding/json"
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

func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	wire, err := toWire(req)
	if err != nil {
		return Response{}, err
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return Response{}, fmt.Errorf("openai: encode request: %w", err)
	}

	base := strings.TrimRight(o.BaseURL, "/")
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
