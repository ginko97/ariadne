package tool_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
	"github.com/ginko97/ariadne/internal/tool"
)

// End to end: a real tool, dispatched through the registry, driven by the loop,
// against scripted model responses.
//
// Lives in tool_test (external test package) so it exercises the same public
// surface a caller would, and so loop never has to import tool.
func TestAgentWithRealCalcTool(t *testing.T) {
	reg := tool.New(tool.Calc{})

	fake := &llm.Fake{Responses: []llm.Response{
		{
			Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "Let me compute that."},
				{Type: llm.BlockToolUse, ID: "call_1", Name: "calc",
					Args: json.RawMessage(`{"expr":"240*0.15"}`)},
			},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 50, OutputTokens: 20},
		},
		{
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "15% of 240 is 36."}},
			Stop:   llm.StopEnd,
			Usage:  llm.Usage{InputTokens: 90, OutputTokens: 10},
		},
	}}

	a := &loop.Agent{
		Provider: fake,
		Model:    "test",
		Tools:    reg.Defs(), // what the model is told it may call
		RunTool:  reg.Call,   // what actually runs — same registry, one source of truth
		MaxSteps: 10,
		MaxCost:  1.0,
		Price:    loop.Price{InputPerMTok: 3, OutputPerMTok: 15},
	}

	s := loop.NewState("run_integration", "what is 15% of 240?")
	answer, err := a.Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if answer != "15% of 240 is 36." {
		t.Errorf("answer = %q", answer)
	}

	// The tool definitions reached the provider on every call.
	if len(fake.Calls[0].Tools) != 1 || fake.Calls[0].Tools[0].Name != "calc" {
		t.Errorf("tools sent to model = %+v", fake.Calls[0].Tools)
	}

	// Calc really ran: 240*0.15 was computed, not echoed from the fixture.
	tr := fake.Calls[1].Messages[2].Blocks[0]
	if tr.Type != llm.BlockToolResult || tr.CallID != "call_1" {
		t.Fatalf("tool_result = %+v", tr)
	}
	if tr.Content != "36" {
		t.Errorf("calc returned %q, want 36", tr.Content)
	}
	if tr.IsError {
		t.Error("tool reported an error")
	}
}

// A model hallucinating a tool name must not kill the run — it gets an error
// block back and answers anyway.
func TestAgentRecoversFromUnknownTool(t *testing.T) {
	reg := tool.New(tool.Calc{})

	fake := &llm.Fake{Responses: []llm.Response{
		{
			Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c1", Name: "wolfram_alpha",
				Args: json.RawMessage(`{}`)}},
			Stop: llm.StopToolUse,
		},
		{
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "I only have calc."}},
			Stop:   llm.StopEnd,
		},
	}}

	a := &loop.Agent{
		Provider: fake, Model: "test",
		Tools: reg.Defs(), RunTool: reg.Call,
		MaxSteps: 10, Price: loop.Price{},
	}

	if _, err := a.Run(context.Background(), loop.NewState("r", "use wolfram")); err != nil {
		t.Fatalf("run should survive an unknown tool, got %v", err)
	}

	tr := fake.Calls[1].Messages[2].Blocks[0]
	if !tr.IsError {
		t.Error("unknown tool did not produce an error block")
	}
}
