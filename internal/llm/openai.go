package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
	// LLM calls are slow. ctx carries the real deadline; this is the backstop
	// for callers who pass context.Background().
	defaultTimeout        = 120 * time.Second
	defaultMaxRetries     = 3
	defaultInitialBackoff = 500 * time.Millisecond
	maxBackoffDelay       = 30 * time.Second
	maxErrBodyLength      = 512
)

// OpenAI speaks the OpenAI chat-completions protocol. That protocol is a de
// facto standard, so the same type reaches OpenRouter, Gemini's compatibility
// endpoint, Groq, Together and a local Ollama — only BaseURL changes.
type OpenAI struct {
	APIKey     string
	BaseURL    string
	Extra      http.Header // e.g. OpenRouter's HTTP-Referer / X-Title
	HTTP       *http.Client
	MaxRetries int
	Sleep      func(ctx context.Context, d time.Duration) error
}

// Compile-time proof. Fails at build time rather than at a call site elsewhere.
var _ Provider = (*OpenAI)(nil)

// String redacts the key. This struct will end up inside a %+v eventually — in
// a log line, or wrapped into an error — and that is how keys actually leak.
//
// Value receiver on purpose. With a pointer receiver only *OpenAI satisfies
// Stringer, so fmt printing a copy — `%+v` on a dereferenced value, or a struct
// that embeds one — falls back to the default formatter and prints APIKey in
// full. The pointer gets this method either way.
func (o OpenAI) String() string {
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

func WithMaxRetries(n int) OpenAIOption {
	return func(o *OpenAI) { o.MaxRetries = n }
}

func WithSleep(fn func(context.Context, time.Duration) error) OpenAIOption {
	return func(o *OpenAI) { o.Sleep = fn }
}

func NewOpenAI(apiKey string, opts ...OpenAIOption) *OpenAI {
	o := &OpenAI{
		APIKey:     apiKey,
		BaseURL:    defaultBaseURL,
		HTTP:       &http.Client{Timeout: defaultTimeout},
		MaxRetries: defaultMaxRetries,
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

func (o *OpenAI) sleep(ctx context.Context, d time.Duration) error {
	if o.Sleep != nil {
		return o.Sleep(ctx, d)
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == http.StatusServiceUnavailable ||
		code == 529 // Site Overloaded (Anthropic / Cloudflare)
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

	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}

	maxRetries := o.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return Response{}, err
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

		resp, err := client.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return Response{}, ctx.Err()
			}
			return Response{}, fmt.Errorf("openai: request failed: %w", err)
		}

		body, readErr := io.ReadAll(resp.Body)
		io.Copy(io.Discard, resp.Body) // drain, so the connection returns to the pool usable
		resp.Body.Close()

		if readErr != nil {
			return Response{}, fmt.Errorf("openai: read response: %w", readErr)
		}

		if isRetryableStatus(resp.StatusCode) && attempt < maxRetries {
			delay := calculateBackoff(attempt, resp, body)
			if sleepErr := o.sleep(ctx, delay); sleepErr != nil {
				return Response{}, sleepErr
			}
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return Response{}, fmt.Errorf("openai: %s: %s", resp.Status, truncate(body, maxErrBodyLength))
		}

		return fromWire(body)
	}
}

func calculateBackoff(attempt int, resp *http.Response, body []byte) time.Duration {
	if resp != nil {
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			if d > maxBackoffDelay {
				return maxBackoffDelay
			}
			return d
		}
	}
	if d, ok := parseRetryDelayFromBody(body); ok {
		if d > maxBackoffDelay {
			return maxBackoffDelay
		}
		return d
	}
	delay := defaultInitialBackoff * (1 << attempt)
	if delay > maxBackoffDelay {
		delay = maxBackoffDelay
	}
	return delay
}

func parseRetryAfter(header string) (time.Duration, bool) {
	if header == "" {
		return 0, false
	}
	if sec, err := strconv.ParseFloat(header, 64); err == nil && sec > 0 {
		return time.Duration(sec * float64(time.Second)), true
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d, true
		}
	}
	return 0, false
}

type rpcDetail struct {
	Type       string `json:"@type"`
	RetryDelay string `json:"retryDelay"`
}

type rpcError struct {
	Details []rpcDetail `json:"details"`
}

type rpcEnvelope struct {
	Error rpcError `json:"error"`
}

func parseRetryDelayFromBody(body []byte) (time.Duration, bool) {
	var arr []rpcEnvelope
	if err := json.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
		for _, item := range arr {
			if d, ok := extractRetryDelay(item.Error.Details); ok {
				return d, true
			}
		}
	}
	var obj rpcEnvelope
	if err := json.Unmarshal(body, &obj); err == nil {
		if d, ok := extractRetryDelay(obj.Error.Details); ok {
			return d, true
		}
	}
	return 0, false
}

func extractRetryDelay(details []rpcDetail) (time.Duration, bool) {
	for _, det := range details {
		if strings.HasSuffix(det.Type, "RetryInfo") && det.RetryDelay != "" {
			if d, err := time.ParseDuration(det.RetryDelay); err == nil && d > 0 {
				return d, true
			}
		}
	}
	return 0, false
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
