package llm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestFakeTwoStepExchange(t *testing.T) {
	f := &Fake{Responses: []Response{
		{ // turn 1: prose + a tool call
			Blocks: []Block{
				{Type: BlockText, Text: "Let me compute that."},
				{Type: BlockToolUse, ID: "call_a1", Name: "calc",
					Args: json.RawMessage(`{"expr":"240*0.15"}`)},
			},
			Stop:  StopToolUse,
			Usage: Usage{InputTokens: 52, OutputTokens: 18},
		},
		{ // turn 2: final answer
			Blocks: []Block{{Type: BlockText, Text: "15% of 240 is 36."}},
			Stop:   StopEnd,
			Usage:  Usage{InputTokens: 94, OutputTokens: 11},
		},
	}}

	ctx := context.Background()
	history := []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "what is 15% of 240?"}}}}

	// --- turn 1 ---
	r1, err := f.Complete(ctx, Request{Model: "test", Messages: history})
	if err != nil {
		t.Fatal(err)
	}

	// The model asked for one tool. Proves ToolCalls() maps blocks → calls.
	calls := r1.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("turn 1: got %d tool calls, want 1", len(calls))
	}
	if calls[0].ID != "call_a1" || calls[0].Name != "calc" {
		t.Fatalf("turn 1: got %+v, want ID=call_a1 Name=calc", calls[0])
	}
	// Prose and a tool call arrived in the same turn — the reason Message holds []Block.
	if got, want := r1.Text(), "Let me compute that."; got != want {
		t.Fatalf("turn 1 text: got %q, want %q", got, want)
	}

	// the loop would do this: record what the model said, then the tool result
	history = append(history,
		Message{Role: RoleAssistant, Blocks: r1.Blocks},
		Message{Role: RoleUser, Blocks: []Block{
			{Type: BlockToolResult, CallID: "call_a1", Content: "36"},
		}},
	)

	// --- turn 2 ---
	r2, err := f.Complete(ctx, Request{Model: "test", Messages: history})
	if err != nil {
		t.Fatal(err)
	}

	// The loop's terminate condition.
	if r2.Stop != StopEnd {
		t.Fatalf("turn 2: got stop %q, want %q", r2.Stop, StopEnd)
	}
	if got, want := r2.Text(), "15% of 240 is 36."; got != want {
		t.Fatalf("turn 2 text: got %q, want %q", got, want)
	}

	// The assertion that matters: the tool result actually travelled back into
	// the next request. Everything above would still pass if Message were a string.
	if len(f.Calls) != 2 {
		t.Fatalf("provider received %d calls, want 2", len(f.Calls))
	}
	second := f.Calls[1].Messages
	if len(second) != 3 {
		t.Fatalf("second request carried %d messages, want 3 (user, assistant, tool_result)", len(second))
	}
	if len(second[2].Blocks) != 1 {
		t.Fatalf("tool_result message had %d blocks, want 1", len(second[2].Blocks))
	}
	tr := second[2].Blocks[0]
	if tr.Type != BlockToolResult || tr.CallID != "call_a1" || tr.Content != "36" {
		t.Fatalf("tool_result block = %+v, want type=tool_result call_id=call_a1 content=36", tr)
	}
}

// The fake fails loudly when the loop wants more turns than the test scripted,
// rather than panicking or returning a zero Response that silently passes.
func TestFakeExhausted(t *testing.T) {
	f := &Fake{Responses: []Response{{Stop: StopEnd}}}
	ctx := context.Background()

	if _, err := f.Complete(ctx, Request{}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := f.Complete(ctx, Request{})
	if !errors.Is(err, ErrFakeExhausted) {
		t.Fatalf("second call: got %v, want ErrFakeExhausted", err)
	}
}

// Cancellation is checked before anything else, so cancel-mid-run tests behave
// against the fake exactly as they will against real HTTP.
func TestFakeRespectsContext(t *testing.T) {
	f := &Fake{Responses: []Response{{Stop: StopEnd}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.Complete(ctx, Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("cancelled call was recorded: %d calls", len(f.Calls))
	}
}
