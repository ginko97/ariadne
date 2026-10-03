package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// countingBody serves n bytes of filler and counts what was read.
type countingBody struct {
	left, read int
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.left)
	for i := range p[:n] {
		p[i] = 'x'
	}
	b.left -= n
	b.read += n
	return n, nil
}

func (b *countingBody) Close() error { return nil }

type oneResponse struct {
	status int
	body   io.ReadCloser
}

func (o oneResponse) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: o.status, Status: http.StatusText(o.status),
		Header: http.Header{}, Body: o.body}, nil
}

func userRequest() Request {
	return Request{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}}}}
}

// An error response is read only as far as the error needs: a gateway that
// answers 400 with megabytes of HTML is not held in memory.
func TestErrorBodyIsReadOnlyAsFarAsNeeded(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests} {
		body := &countingBody{left: 4 << 20}
		o := NewOpenAI("k", WithHTTPClient(&http.Client{Transport: oneResponse{status, body}}), WithMaxRetries(0))
		if _, err := o.Complete(context.Background(), userRequest()); err == nil {
			t.Fatalf("%d: no error", status)
		}
		if body.read > 64<<10 {
			t.Errorf("%d: read %d bytes of the error body, want at most 64KB", status, body.read)
		}
	}
}

// ...but far enough for what is in it: Gemini's retryDelay sits in the details,
// after the message, which can be long.
func TestRetryDelayPastALongMessageIsStillRead(t *testing.T) {
	body := `{"error":{"code":429,"message":"` + strings.Repeat("quota ", 2000) + `","status":"RESOURCE_EXHAUSTED",` +
		`"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"7s"}]}}`
	var waited []time.Duration
	calls := 0
	o := NewOpenAI("k",
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{StatusCode: 429, Status: "429 Too Many Requests", Header: http.Header{},
					Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))}, nil
		})}),
		WithSleep(func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }))
	if _, err := o.Complete(context.Background(), userRequest()); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != 7*time.Second {
		t.Errorf("waited %v, want the body's 7s", waited)
	}
}
