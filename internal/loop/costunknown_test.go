package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
)

// A provider that reports tokens but no cost, with no price known for the
// model: Ollama, OpenAI direct, Gemini, Hugging Face. A spending limit over
// that total cannot hold, so the run stops after the step that showed it —
// with that step kept and its tool run — rather than spending unmetered.
func TestMaxCostStopsWhenACostIsUnknown(t *testing.T) {
	ran := 0
	agent := func(maxCost float64) *Agent {
		return &Agent{
			Provider: &llm.Fake{Responses: []llm.Response{
				toolUseResponse("c1", "calc", `{"n":1}`, 2_000_000, 500_000),
				toolUseResponse("c2", "calc", `{"n":2}`, 2_000_000, 500_000),
				endResponse("done", 2_000_000, 500_000),
			}},
			Model: "unpriced", MaxSteps: 10, MaxCost: maxCost,
			RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
				ran++
				return llm.ToolResult{Content: "1"}, nil
			},
		}
	}

	s := NewState("r", "go")
	_, err := agent(0.01).Run(context.Background(), s)
	if !errors.Is(err, ErrCostUnknown) {
		t.Fatalf("err = %v, want ErrCostUnknown", err)
	}
	if s.Steps != 1 || ran != 1 {
		t.Errorf("steps = %d, tools run = %d; want the one step that revealed it, with its tool run", s.Steps, ran)
	}
	if s.HasPendingToolCalls() {
		t.Error("the step's tool call is left pending; a resume would run it again")
	}

	// No limit, nothing to enforce: the conversation runs as before.
	ran = 0
	s = NewState("r2", "go")
	if _, err := agent(0).Run(context.Background(), s); err != nil || s.Steps != 3 {
		t.Errorf("without -max-cost: err = %v, steps = %d; want the whole run", err, s.Steps)
	}
}
