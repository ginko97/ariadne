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
