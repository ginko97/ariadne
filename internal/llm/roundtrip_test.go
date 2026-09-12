package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRecordedTransportRoundTrips(t *testing.T) {
	rt := &RecordedTransport{Responses: [][]byte{[]byte(`{"ok":true}`)}}
	c := &http.Client{Transport: rt}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://example.test/v1/chat", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")

	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != `{"ok":true}` {
		t.Errorf("body = %s", got)
	}

	// and the direction that actually matters: what did we send?
	if len(rt.Bodies) != 1 || string(rt.Bodies[0]) != `{"model":"x"}` {
		t.Errorf("captured bodies = %q", rt.Bodies)
	}
	if h := rt.Requests[0].Header.Get("Authorization"); h != "Bearer secret" {
		t.Errorf("auth header = %q", h)
	}
}

func TestRecordedTransportExhausted(t *testing.T) {
	rt := &RecordedTransport{Responses: [][]byte{[]byte(`{}`)}}
	c := &http.Client{Transport: rt}

	resp, err := c.Get("https://example.test/")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	resp.Body.Close()

	if _, err := c.Get("https://example.test/"); !errors.Is(err, ErrTransportExhausted) {
		t.Fatalf("second call: got %v, want ErrTransportExhausted", err)
	}
	// The failed call is still recorded: a backoff test needs to see that a
	// request was attempted, not just that it failed.
	if len(rt.Requests) != 2 {
		t.Errorf("recorded %d requests, want 2", len(rt.Requests))
	}
}

func TestRecordedTransportStatus(t *testing.T) {
	rt := &RecordedTransport{
		Responses: [][]byte{[]byte(`{"error":"rate limited"}`)},
		Statuses:  []int{http.StatusTooManyRequests},
	}
	c := &http.Client{Transport: rt}

	resp, err := c.Get("https://example.test/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
}

func TestRecordedTransportCancelled(t *testing.T) {
	rt := &RecordedTransport{Responses: [][]byte{[]byte(`{}`)}}
	c := &http.Client{Transport: rt}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if len(rt.Requests) != 0 {
		t.Errorf("recorded %d requests, want 0 — Client short-circuits before the transport", len(rt.Requests))
	}
}

// 429 backoff: when a provider returns 429 with Google's RetryInfo body
// (as in rate_limit_429.json), Complete extracts the retry delay, sleeps,
// retries, and succeeds without failing the step.
func TestCompleteRetriesOn429WithGeminiFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/rate_limit_429.json")
	if err != nil {
		t.Fatalf("failed to read 429 fixture: %v", err)
	}

	okResp := []byte(`{"choices":[{"message":{"content":"retry succeeded"},"finish_reason":"stop"}]}`)

	rt := &RecordedTransport{
		Responses: [][]byte{fixture, okResp},
		Statuses:  []int{http.StatusTooManyRequests, http.StatusOK},
	}
	c := &http.Client{Transport: rt}

	var slept []time.Duration
	o := NewOpenAI("key",
		WithHTTPClient(c),
		WithBaseURL("https://example.test/v1"),
		WithSleep(func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}),
	)

	resp, err := o.Complete(context.Background(), Request{Model: "gemini-2.5-flash"})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if resp.Text() != "retry succeeded" {
		t.Errorf("got %q, want %q", resp.Text(), "retry succeeded")
	}

	if len(slept) != 1 || slept[0] != 20*time.Second {
		t.Errorf("slept = %v, want exactly [20s] from fixture RetryInfo", slept)
	}
	if len(rt.Requests) != 2 {
		t.Errorf("made %d requests, want 2", len(rt.Requests))
	}
}

// When a standard Retry-After header is provided with seconds, it takes precedence.
func TestCompleteRetriesOnRetryAfterHeader(t *testing.T) {
	okResp := []byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)

	rt := &RecordedTransport{
		Responses: [][]byte{[]byte(`{"error":"rate limited"}`), okResp},
		Statuses:  []int{http.StatusTooManyRequests, http.StatusOK},
		Headers: []http.Header{
			{"Retry-After": []string{"3"}},
			nil,
		},
	}
	c := &http.Client{Transport: rt}

	var slept []time.Duration
	o := NewOpenAI("key",
		WithHTTPClient(c),
		WithBaseURL("https://example.test/v1"),
		WithSleep(func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}),
	)

	resp, err := o.Complete(context.Background(), Request{Model: "test"})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if resp.Text() != "ok" {
		t.Errorf("got %q, want ok", resp.Text())
	}

	if len(slept) != 1 || slept[0] != 3*time.Second {
		t.Errorf("slept = %v, want [3s] from Retry-After header", slept)
	}
}

// When retries are exhausted, Complete fails with the provider's error.
func TestCompleteExhaustsRetriesOnRepeated429(t *testing.T) {
	rateErr := []byte(`{"error":{"code":429,"message":"quota exhausted"}}`)

	rt := &RecordedTransport{
		Responses: [][]byte{rateErr, rateErr, rateErr, rateErr},
		Statuses:  []int{429, 429, 429, 429},
	}
	c := &http.Client{Transport: rt}

	var sleeps int
	o := NewOpenAI("key",
		WithHTTPClient(c),
		WithBaseURL("https://example.test/v1"),
		WithMaxRetries(2), // 1 initial + 2 retries = 3 requests total
		WithSleep(func(_ context.Context, _ time.Duration) error {
			sleeps++
			return nil
		}),
	)

	_, err := o.Complete(context.Background(), Request{Model: "test"})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %v, want 429 error", err)
	}
	if sleeps != 2 {
		t.Errorf("sleeps = %d, want 2", sleeps)
	}
	if len(rt.Requests) != 3 {
		t.Errorf("made %d requests, want 3", len(rt.Requests))
	}
}

// Context cancellation during backoff sleep aborts immediately without retrying.
func TestCompleteCancellationDuringBackoff(t *testing.T) {
	rateErr := []byte(`{"error":"rate limited"}`)
	rt := &RecordedTransport{
		Responses: [][]byte{rateErr},
		Statuses:  []int{429},
	}
	c := &http.Client{Transport: rt}

	ctx, cancel := context.WithCancel(context.Background())
	o := NewOpenAI("key",
		WithHTTPClient(c),
		WithBaseURL("https://example.test/v1"),
		WithSleep(func(cCtx context.Context, _ time.Duration) error {
			cancel()
			return cCtx.Err()
		}),
	)

	_, err := o.Complete(ctx, Request{Model: "test"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if len(rt.Requests) != 1 {
		t.Errorf("made %d requests, want 1", len(rt.Requests))
	}
}

// A number the server named is an answer, not an estimate. Retrying at 30s
// into a window the server said was 60s is a guaranteed second 429, so the
// clamped version of the instruction is not a shorter wait — it is a wait that
// fails. Refusing immediately turns ninety seconds of thrashing into an error
// the caller can act on.
func TestCompleteRefusesRetryAfterBeyondCeiling(t *testing.T) {
	body := []byte(`{"error":{"message":"quota exhausted"}}`)
	hdr := http.Header{"Retry-After": []string{"60"}}
	rt := &RecordedTransport{
		Responses: [][]byte{body, body, body, body},
		Statuses:  []int{429, 429, 429, 429},
		Headers:   []http.Header{hdr, hdr, hdr, hdr},
	}

	var slept []time.Duration
	o := NewOpenAI("key",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithBaseURL("https://example.test/v1"),
		WithSleep(func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}),
	)

	_, err := o.Complete(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "1m0s") {
		t.Errorf("error should name what the server asked for: %v", err)
	}
	if len(slept) != 0 {
		t.Errorf("slept %v; an instruction we cannot follow is not a reason to wait", slept)
	}
	if len(rt.Requests) != 1 {
		t.Errorf("made %d requests, want 1", len(rt.Requests))
	}
}

// Per-attempt ceilings do not compose. Three legitimate 30s waits is a minute
// and a half inside a single step, with nothing having asked for that.
func TestCompleteStopsAtTotalBackoffCeiling(t *testing.T) {
	body := []byte(`{"error":{"message":"slow down"}}`)
	hdr := http.Header{"Retry-After": []string{"30"}}
	rt := &RecordedTransport{
		Responses: [][]byte{body, body, body, body, body, body},
		Statuses:  []int{429, 429, 429, 429, 429, 429},
		Headers:   []http.Header{hdr, hdr, hdr, hdr, hdr, hdr},
	}

	var total time.Duration
	o := NewOpenAI("key",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithBaseURL("https://example.test/v1"),
		WithMaxRetries(10),
		WithSleep(func(_ context.Context, d time.Duration) error {
			total += d
			return nil
		}),
	)

	_, err := o.Complete(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if total > maxTotalBackoff {
		t.Errorf("waited %s in total, past the %s ceiling", total, maxTotalBackoff)
	}
	if !strings.Contains(err.Error(), "gave up after") {
		t.Errorf("error should say it gave up on a budget: %v", err)
	}
}

// A retry is otherwise invisible: the loop times the whole Complete call, so
// without this hook backoff is recorded as model latency and quietly pollutes
// the one number evals read timings from.
func TestCompleteReportsRetriesToTheCaller(t *testing.T) {
	ok := []byte(`{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	rt := &RecordedTransport{
		Responses: [][]byte{[]byte(`{}`), []byte(`{}`), ok},
		Statuses:  []int{429, 503, 200},
	}

	type call struct {
		attempt, status int
		delay           time.Duration
	}
	var seen []call

	o := NewOpenAI("key",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithBaseURL("https://example.test/v1"),
		WithSleep(func(context.Context, time.Duration) error { return nil }),
		WithOnRetry(func(attempt, status int, delay time.Duration) {
			seen = append(seen, call{attempt, status, delay})
		}),
	)

	resp, err := o.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text() != "hi" {
		t.Errorf("text = %q", resp.Text())
	}

	if len(seen) != 2 {
		t.Fatalf("OnRetry fired %d times, want 2: %+v", len(seen), seen)
	}
	// The status matters: a 429 is a quota problem and a 503 is not, and the
	// difference decides whether a slower sweep would have helped.
	if seen[0].status != 429 || seen[1].status != 503 {
		t.Errorf("statuses = %d, %d; want 429 then 503", seen[0].status, seen[1].status)
	}
	if seen[0].delay != defaultInitialBackoff || seen[1].delay != 2*defaultInitialBackoff {
		t.Errorf("delays = %v, %v; want the doubling sequence", seen[0].delay, seen[1].delay)
	}
	if seen[0].attempt != 0 || seen[1].attempt != 1 {
		t.Errorf("attempts = %d, %d; want 0 then 1", seen[0].attempt, seen[1].attempt)
	}
}

// A status the server is not going to change its mind about must not be
// retried: three more round trips delay a failure the caller can already act
// on, and a 400 is not going to become a 200.
func TestCompleteDoesNotRetryClientErrors(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 422, 500} {
		body := []byte(`{"error":{"message":"nope"}}`)
		rt := &RecordedTransport{
			Responses: [][]byte{body, body, body, body},
			Statuses:  []int{code, code, code, code},
		}
		slept := 0
		o := NewOpenAI("key",
			WithHTTPClient(&http.Client{Transport: rt}),
			WithBaseURL("https://example.test/v1"),
			WithSleep(func(context.Context, time.Duration) error { slept++; return nil }),
		)
		if _, err := o.Complete(context.Background(), Request{Model: "m"}); err == nil {
			t.Errorf("%d: expected an error", code)
		}
		if len(rt.Requests) != 1 {
			t.Errorf("%d: made %d requests, want 1", code, len(rt.Requests))
		}
		if slept != 0 {
			t.Errorf("%d: slept %d times on a status that will not change", code, slept)
		}
	}
}
