package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

// blockingProvider stands in for a model that is still answering: it blocks
// until its context ends and reports that it did.
//
// release lets the test end it when the context never does. Without it, a
// failing run hangs instead of failing: the model blocks forever, the handler
// never returns, and httptest.Server.Close waits on the handler.
type blockingProvider struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (p *blockingProvider) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	close(p.started)
	select {
	case <-ctx.Done():
		close(p.cancelled)
		return llm.Response{}, ctx.Err()
	case <-p.release:
		return llm.Response{}, context.Canceled
	}
}

// Stop in the page is the browser aborting its own request. That is only a
// Stop if the turn on the server sees it: the handler runs the turn on
// r.Context(), and net/http cancels that context when the client's connection
// closes. This checks the whole chain over a real connection — a client
// aborting mid-stream, and the model call on the server being cancelled —
// which is what decides whether Stop needs an endpoint of its own. It does
// not: aborting is enough.
func TestAbortingTheRequestStopsTheTurn(t *testing.T) {
	store := &loop.Store{Dir: t.TempDir()}
	p := &blockingProvider{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	s := New(store,
		func(runID string, state *loop.State, onDelta func(llm.Chunk)) (*loop.Agent, func()) {
			return &loop.Agent{Provider: p, Model: "test-model", MaxSteps: 5, Checkpoint: store.Save}, nil
		},
		func() string { return "run_stop" },
	)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()
	defer close(p.release) // runs before ts.Close, so a failure cannot hang it

	ctx, abort := context.WithCancel(context.Background())
	defer abort()
	req, err := http.NewRequestWithContext(ctx, "POST", ts.URL+"/api/chat", strings.NewReader(`{"message":"a long question"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ariadne-CSRF", s.CSRFToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Mid-stream: the turn has started and the model is answering.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "event: start") {
		t.Fatalf("first line = %q, %v; want the start event", line, err)
	}
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the model was never called")
	}

	abort() // what the page's Stop button does

	select {
	case <-p.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("aborting the request did not cancel the model call; Stop would need an endpoint")
	}

	// The claim is released when the handler returns, so the conversation can
	// take its next message instead of answering 409 "a turn is already running".
	deadline := time.Now().Add(5 * time.Second)
	for {
		release, ok := s.claim("run_stop")
		if ok {
			release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stopped turn still holds the conversation")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
