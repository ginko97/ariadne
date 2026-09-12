package loop

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
)

// testPrice makes one step of the fixtures below cost exactly $0.001, so cost
// assertions read as "two steps" rather than as arbitrary decimals.
var testPrice = Price{InputPerMTok: 10, OutputPerMTok: 10}

func toolUseResponse(id, name, args string, in, out int) llm.Response {
	return llm.Response{
		Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "Let me compute that."},
			{Type: llm.BlockToolUse, ID: id, Name: name, Args: json.RawMessage(args)},
		},
		Stop:  llm.StopToolUse,
		Usage: llm.Usage{InputTokens: in, OutputTokens: out},
	}
}

func endResponse(text string, in, out int) llm.Response {
	return llm.Response{
		Blocks: []llm.Block{{Type: llm.BlockText, Text: text}},
		Stop:   llm.StopEnd,
		Usage:  llm.Usage{InputTokens: in, OutputTokens: out},
	}
}

// The week 1-2 deliverable: a full two-step tool-using run, no network, no API key.
func TestRunTwoStep(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("call_a1", "calc", `{"expr":"240*0.15"}`, 52, 18),
		endResponse("15% of 240 is 36.", 94, 11),
	}}

	var gotCall llm.ToolCall
	a := &Agent{
		Provider: fake,
		Model:    "test",
		MaxSteps: 10,
		Price:    testPrice,
		RunTool: func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			gotCall = c
			return llm.ToolResult{Content: "36"}, nil
		},
	}

	s := NewState("run_test", "what is 15% of 240?")
	answer, err := a.Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if answer != "15% of 240 is 36." {
		t.Errorf("answer = %q", answer)
	}
	if s.Steps != 2 {
		t.Errorf("Steps = %d, want 2", s.Steps)
	}
	if gotCall.Name != "calc" || gotCall.ID != "call_a1" {
		t.Errorf("tool call = %+v, want name=calc id=call_a1", gotCall)
	}

	// 4 messages: user task, assistant tool_use, user tool_result, assistant answer.
	if len(s.Messages) != 4 {
		t.Fatalf("Messages = %d, want 4", len(s.Messages))
	}

	// The assertion that matters: the tool result reached the provider on turn 2.
	if len(fake.Calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(fake.Calls))
	}
	second := fake.Calls[1].Messages
	if len(second) != 3 {
		t.Fatalf("second request carried %d messages, want 3", len(second))
	}
	tr := second[2].Blocks[0]
	if tr.Type != llm.BlockToolResult || tr.CallID != "call_a1" || tr.Content != "36" {
		t.Errorf("tool_result = %+v", tr)
	}
}

// A confused model must not loop forever.
func TestRunStepLimit(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "calc", `{}`, 10, 10),
		toolUseResponse("c2", "calc", `{}`, 10, 10),
		toolUseResponse("c3", "calc", `{}`, 10, 10),
	}}
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 2, Price: testPrice,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) { return llm.ToolResult{Content: "ok"}, nil },
	}

	_, err := a.Run(context.Background(), NewState("r", "loop forever"))
	if !errors.Is(err, ErrStepLimit) {
		t.Fatalf("got %v, want ErrStepLimit", err)
	}
	if len(fake.Calls) != 2 {
		t.Errorf("provider called %d times, want 2 — the guard fired late", len(fake.Calls))
	}
}

// And must not spend forever either.
func TestRunCostLimit(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "calc", `{}`, 1_000_000, 0), // $10 in one step
		endResponse("never reached", 0, 0),
	}}
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10, MaxCost: 1.0, Price: testPrice,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) { return llm.ToolResult{Content: "ok"}, nil },
	}

	s := NewState("r", "expensive")
	_, err := a.Run(context.Background(), s)
	if !errors.Is(err, ErrCostLimit) {
		t.Fatalf("got %v, want ErrCostLimit", err)
	}
	if s.Cost < 1.0 {
		t.Errorf("Cost = %v, want >= 1.0", s.Cost)
	}
}

// Cancellation must stop the run, not be swallowed as a tool error.
func TestRunCancelled(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{endResponse("unreachable", 1, 1)}}
	a := &Agent{Provider: fake, Model: "test", MaxSteps: 10, Price: testPrice}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := a.Run(ctx, NewState("r", "task"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("provider was called %d times after cancel", len(fake.Calls))
	}
}

// A tool that fails is information for the model, not the end of the run.
func TestRunToolErrorBecomesBlock(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "calc", `{"expr":"1/0"}`, 10, 10),
		endResponse("I could not divide by zero.", 10, 10),
	}}
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10, Price: testPrice,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{}, errors.New("division by zero")
		},
	}

	s := NewState("r", "what is 1/0?")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run should recover from a tool error, got %v", err)
	}

	tr := fake.Calls[1].Messages[2].Blocks[0]
	if !tr.IsError {
		t.Errorf("tool_result.IsError = false, want true")
	}
	if tr.Content != "division by zero" {
		t.Errorf("tool_result.Content = %q", tr.Content)
	}
}

// A malformed response — stop=tool_use with nothing to call — fails loudly.
func TestRunEmptyToolUse(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{{
		Blocks: []llm.Block{{Type: llm.BlockText, Text: "thinking"}},
		Stop:   llm.StopToolUse,
	}}}
	a := &Agent{Provider: fake, Model: "test", MaxSteps: 10, Price: testPrice}

	if _, err := a.Run(context.Background(), NewState("r", "t")); !errors.Is(err, ErrEmptyToolUse) {
		t.Fatalf("got %v, want ErrEmptyToolUse", err)
	}
}

// The hook fires once per step and once more before Run returns, and the final
// call sees the finished conversation. This is the test that catches a hook
// that is declared but never called — which compiles and passes everything else.
func TestCheckpointCalledEveryStep(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("call_a1", "calc", `{"expr":"240*0.15"}`, 52, 18),
		endResponse("15% of 240 is 36.", 94, 11),
	}}

	var seen []int // messages present at each checkpoint
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10, Price: testPrice,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "36"}, nil
		},
		Checkpoint: func(s *State) error {
			seen = append(seen, len(s.Messages))
			return nil
		},
	}

	if _, err := a.Run(context.Background(), NewState("run_cp", "t")); err != nil {
		t.Fatal(err)
	}

	// step 1 writes after the tool result (3 messages), then StopEnd writes the
	// finished conversation (4 messages).
	want := []int{3, 4}
	if len(seen) != len(want) {
		t.Fatalf("checkpoint called %d times with %v, want %d", len(seen), seen, len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("checkpoint %d saw %d messages, want %d", i, seen[i], want[i])
		}
	}
}

// A run that cannot be checkpointed cannot be resumed, so it must fail rather
// than continue and quietly stop being durable.
func TestCheckpointFailureFailsRun(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{endResponse("done", 1, 1)}}

	boom := errors.New("disk full")
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10, Price: testPrice,
		Checkpoint: func(*State) error { return boom },
	}

	_, err := a.Run(context.Background(), NewState("run_cp2", "t"))
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the checkpoint error", err)
	}
}
