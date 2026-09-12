package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
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
