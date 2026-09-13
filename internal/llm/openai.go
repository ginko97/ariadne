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
	// LLM calls are slow, and a large prompt is slower than the number here
	// used to allow. Dogfooding found the failure: two documents in the history
	// meant every request took longer than 120s, so the run failed, and the
	// resume failed identically — checkpointed, resumable in principle, and
	// permanently stuck in practice. A timeout that cannot be raised is a
	// ceiling on how big a job can be.
	//
	// ctx still carries the real deadline; this is the backstop for callers who
	// pass context.Background(), and WithTimeout is how a caller changes it.
	defaultTimeout        = 300 * time.Second
	defaultMaxRetries     = 3
	defaultInitialBackoff = 500 * time.Millisecond
	// maxBackoffDelay bounds a delay we invented. It is deliberately not
	// applied to a delay the server named: retrying at 30s into a window the
	// server said was 60s guarantees a second 429, so clamping an explicit
	// instruction spends the wait and fails anyway — worse than either
	// honouring it or refusing outright. See backoff.
	maxBackoffDelay = 30 * time.Second
	// maxTotalBackoff bounds the whole retry sequence. Per-attempt ceilings do
	// not compose: three legitimate 30s waits is a minute and a half inside one
	// step, with nothing on the command line having asked for that.
	maxTotalBackoff  = 90 * time.Second
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
	// MaxRetries is how many times a rate-limited request is tried again.
	// NewOpenAI sets it; the zero value means no retries, so a struct literal
	// gets the old behaviour rather than a surprise.
	MaxRetries int
	// Sleep is the wait between attempts, injectable so tests do not sleep.
	Sleep func(ctx context.Context, d time.Duration) error
	// OnRetry, if set, is called before each wait.
	//
	// A retry is otherwise invisible: the caller sees one Complete that took a
	// long time, with nothing to say whether the model was slow or the gateway
	// was refusing. The loop measures latency around this call, so without a
	// hook the backoff would be recorded as model latency and quietly pollute
	// the one thing evals read timings from.
	//
	// A callback rather than a trace import: internal/llm does not know what a
	// run is, and should not learn in order to say "waiting 20s".
	OnRetry func(attempt, status int, delay time.Duration)
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

// WithTimeout bounds one HTTP request, the whole of it including reading the
// body. 0 means no client-side limit, leaving only ctx.
func WithTimeout(d time.Duration) OpenAIOption {
	return func(o *OpenAI) { o.HTTP = &http.Client{Timeout: d} }
}

func WithMaxRetries(n int) OpenAIOption {
	return func(o *OpenAI) { o.MaxRetries = n }
}

func WithSleep(fn func(context.Context, time.Duration) error) OpenAIOption {
	return func(o *OpenAI) { o.Sleep = fn }
}

func WithOnRetry(fn func(attempt, status int, delay time.Duration)) OpenAIOption {
	return func(o *OpenAI) { o.OnRetry = fn }
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

	var spentBackoff time.Duration

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

		// ReadAll consumes to EOF, which is the drain: the connection goes back
		// to the pool reusable without a separate io.Copy.
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if readErr != nil {
			return Response{}, fmt.Errorf("openai: read response: %w", readErr)
		}

		if isRetryableStatus(resp.StatusCode) && attempt < maxRetries {
			wait := backoff(attempt, resp, body)

			// An instruction we cannot follow is not a reason to guess. Retrying
			// early into a window the server named is a guaranteed second 429,
			// so say what it asked for and stop — an error in a second beats the
			// same error after a minute and a half.
			if wait.explicit && wait.delay > maxBackoffDelay {
				return Response{}, fmt.Errorf(
					"openai: %s: server asked to wait %s, beyond the %s this client will hold a request: %s",
					resp.Status, wait.delay.Round(time.Second), maxBackoffDelay,
					truncate(body, maxErrBodyLength))
			}
			if spentBackoff+wait.delay > maxTotalBackoff {
				return Response{}, fmt.Errorf(
					"openai: %s: gave up after %s of backoff over %d attempts (ceiling %s): %s",
					resp.Status, spentBackoff.Round(time.Second), attempt+1, maxTotalBackoff,
					truncate(body, maxErrBodyLength))
			}

			if o.OnRetry != nil {
				o.OnRetry(attempt, resp.StatusCode, wait.delay)
			}
			if sleepErr := o.sleep(ctx, wait.delay); sleepErr != nil {
				return Response{}, sleepErr
			}
			spentBackoff += wait.delay
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return Response{}, fmt.Errorf("openai: %s: %s", resp.Status, truncate(body, maxErrBodyLength))
		}

		return fromWire(body)
	}
}

// retryWait is how long to wait, and whether the server said so or we guessed.
//
// The distinction decides what a ceiling means. Our own guess is an estimate
// and clamping it loses nothing. A number the server named is the answer to
// "when will this work", and a clamped version of it is not a smaller wait —
// it is a wait that fails.
type retryWait struct {
	delay    time.Duration
	explicit bool
}

// backoff reads the server's instruction if there is one, and otherwise doubles.
//
// Two places carry it. Retry-After is the HTTP standard; Gemini instead returns
// google.rpc.RetryInfo inside the error body, which is where the 20s in
// testdata/rate_limit_429.json lives. Header first: it is the one a gateway in
// front of the model is able to set.
func backoff(attempt int, resp *http.Response, body []byte) retryWait {
	if resp != nil {
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			return retryWait{delay: d, explicit: true}
		}
	}
	if d, ok := parseRetryDelayFromBody(body); ok {
		return retryWait{delay: d, explicit: true}
	}

	// No instruction, so double from the initial delay. Capped, because this
	// number is invented and an invented number should not stall a job.
	// The shift is clamped as a guard, not a repair: maxTotalBackoff stops the
	// sequence at attempt 7 even with MaxRetries set to 100, so 1<<attempt
	// cannot reach an overflow today. It becomes reachable the moment that
	// ceiling is raised, and an overflowed duration is negative — which would
	// sleep for no time and hammer a server that just asked for room.
	shift := attempt
	if shift > 10 {
		shift = 10
	}
	delay := defaultInitialBackoff * (1 << shift)
	if delay > maxBackoffDelay {
		delay = maxBackoffDelay
	}
	return retryWait{delay: delay}
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
