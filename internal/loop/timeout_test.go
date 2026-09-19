package loop

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/trace"
)

// A tool that ignores its context is the case worth bounding — a blocking
// syscall, a client with its own timeouts, a tight loop. Passing a deadline and
// hoping does nothing about any of them.
func TestToolTimeoutBoundsAToolThatIgnoresContext(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "slow", `{}`, 10, 10),
		endResponse("gave up on that one", 10, 10),
	}}

	var events []trace.Event
	a := &Agent{
		Provider:    fake,
		Model:       "test",
		MaxSteps:    10,
		ToolTimeout: 50 * time.Millisecond,
		Trace:       func(e trace.Event) { events = append(events, e) },
		RunTool: func(ctx context.Context, _ llm.ToolCall) (llm.ToolResult, error) {
			time.Sleep(3 * time.Second) // never looks at ctx
			return llm.ToolResult{Content: "eventually"}, nil
		},
	}

	started := time.Now()
	s := NewState("run_test", "hi")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("run took %s; the timeout did not bound it", elapsed)
	}

	// The model has to be told, and told the truth: abandoned, not failed.
	b := fake.Calls[1].Messages[2].Blocks[0]
	if !b.IsError {
		t.Error("the timeout was not reported as an error result")
	}
	for _, want := range []string{"abandoned", "may still be running"} {
		if !strings.Contains(b.Content, want) {
			t.Errorf("result should say %q, got: %s", want, b.Content)
		}
	}

	var timeouts int
	for _, e := range events {
		if e.Kind == trace.KindToolTimeout {
			timeouts++
			if e.Tool != "slow" {
				t.Errorf("timeout names %q", e.Tool)
			}
			if e.Step != 1 {
				t.Errorf("timeout step = %d, want 1", e.Step)
			}
		}
	}
	if timeouts != 1 {
		t.Errorf("tool_timeout events = %d, want 1", timeouts)
	}
}

// An abandoned call still has a result, so a resumed run must not fire it
// again. A tool that moves money and merely ran slowly must not run twice.
func TestTimedOutCallIsNotPendingOnResume(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "slow", `{}`, 10, 10),
		endResponse("done", 10, 10),
	}}
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10,
		ToolTimeout: 30 * time.Millisecond,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			time.Sleep(time.Second)
			return llm.ToolResult{Content: "late"}, nil
		},
	}

	s := NewState("run_test", "hi")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if p := s.pendingToolCalls(); len(p) != 0 {
		t.Errorf("%d calls still pending; a resume would re-fire an abandoned tool", len(p))
	}
}

// Zero is unlimited, which is what this was before the limit existed.
func TestZeroToolTimeoutIsUnlimited(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "slow", `{}`, 10, 10),
		endResponse("done", 10, 10),
	}}
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			time.Sleep(80 * time.Millisecond)
			return llm.ToolResult{Content: "slow but fine"}, nil
		},
	}

	s := NewState("run_test", "hi")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := fake.Calls[1].Messages[2].Blocks[0].Content; got != "slow but fine" {
		t.Errorf("result = %q; a zero timeout should not bound anything", got)
	}
}

// A run cancelled while a tool is slow is the caller leaving, which is a
// different fact from the tool being slow, and has to stay distinguishable.
func TestCancellationDuringASlowToolIsNotATimeout(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "slow", `{}`, 10, 10),
		endResponse("done", 10, 10),
	}}
	ctx, cancel := context.WithCancel(context.Background())

	var timeouts atomic.Int64
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10,
		ToolTimeout: 5 * time.Second,
		Trace: func(e trace.Event) {
			if e.Kind == trace.KindToolTimeout {
				timeouts.Add(1)
			}
		},
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			cancel()
			time.Sleep(2 * time.Second)
			return llm.ToolResult{Content: "late"}, nil
		},
	}

	_, err := a.Run(ctx, NewState("run_test", "hi"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := timeouts.Load(); n != 0 {
		t.Errorf("%d timeout events; cancellation is not a timeout", n)
	}
}

// A tool that finishes inside the limit behaves exactly as before, including
// returning a real error rather than a timeout message.
func TestToolTimeoutPassesThroughNormalResults(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "quick", `{}`, 10, 10),
		endResponse("done", 10, 10),
	}}
	boom := errors.New("the tool could not be reached")
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10,
		ToolTimeout: time.Second,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{}, boom
		},
	}

	s := NewState("run_test", "hi")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := fake.Calls[1].Messages[2].Blocks[0].Content; got != boom.Error() {
		t.Errorf("result = %q, want the tool's own error", got)
	}
}
