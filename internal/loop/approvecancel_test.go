package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/trace"
)

// An approver that cannot get an answer — the tab closed, the request was
// cancelled — returns an error, and the call stays pending. Recording it as
// denied would put a "no" in the checkpoint that nobody gave, and a resume
// would never ask again.
func TestApprovalErrorLeavesTheCallPending(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "write_file", `{"path":"a.txt","content":"x"}`, 10, 10),
	}}
	var events []trace.Event
	ran := false
	a := &Agent{
		Provider:        fake,
		Model:           "test",
		MaxSteps:        5,
		RequireApproval: []string{"write_file"},
		Approve: func(context.Context, llm.ToolCall) (bool, error) {
			return false, context.Canceled
		},
		Trace: func(e trace.Event) { events = append(events, e) },
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			ran = true
			return llm.ToolResult{Content: "ok"}, nil
		},
	}

	s := NewState("run_cancel", "write a file")
	if _, err := a.Run(context.Background(), s); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if ran {
		t.Fatal("the tool ran without an approval")
	}
	if !s.HasPendingToolCalls() {
		t.Error("the call is not pending; a resume would never ask again")
	}
	for _, e := range events {
		if e.Kind == trace.KindToolDenied {
			t.Errorf("a denial was recorded that nobody gave: %+v", e)
		}
	}
}
