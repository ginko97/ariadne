package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/trace"
)

// assertWellFormed is the invariant compaction exists to not break.
//
// Every tool_use must have a result and every result must have a call. Either
// half alone is a provider 400 — the failure mode of a naive sliding window
// here is not a worse answer, it is a run that stops working, and it shows up
// only against a real endpoint.
func assertWellFormed(t *testing.T, msgs []llm.Message) {
	t.Helper()

	calls := map[string]bool{}
	results := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.BlockToolUse:
				calls[b.ID] = true
			case llm.BlockToolResult:
				results[b.CallID] = true
			}
		}
	}
	for id := range calls {
		if !results[id] {
			t.Errorf("tool_use %q has no result: the request is invalid", id)
		}
	}
	for id := range results {
		if !calls[id] {
			t.Errorf("tool_result %q has no call: the request is invalid", id)
		}
	}
}

// conversation builds n complete tool round trips after the task, each padded
// so the sizes are worth comparing.
func conversation(task string, n int) *State {
	s := NewState("run_test", task)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call_%d", i)
		s.Messages = append(s.Messages,
			llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Type: llm.BlockText, Text: strings.Repeat("thinking ", 20)},
				{Type: llm.BlockToolUse, ID: id, Name: "fetch",
					Args: json.RawMessage(fmt.Sprintf(`{"path":"doc-%d.html"}`, i))},
			}},
			llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
				{Type: llm.BlockToolResult, CallID: id,
					Content: strings.Repeat("x", 200) + fmt.Sprintf(" result-%d", i)},
			}},
		)
	}
	return s
}

func TestCompactNeverSplitsAToolPair(t *testing.T) {
	s := conversation("summarise the documents", 10)
	assertWellFormed(t, s.Messages)

	if n := compact(s, 100_000, 10_000); n == 0 {
		t.Fatal("nothing was dropped")
	}
	assertWellFormed(t, s.Messages)

	// And the survivors must still be in order, results after their call.
	for i, m := range s.Messages {
		if isToolResults(m) && i > 0 && !hasToolUse(s.Messages[i-1]) {
			t.Errorf("results at %d do not follow a call", i)
		}
	}
}

// An agent that forgets what it was asked has not been compacted.
func TestCompactKeepsTheTask(t *testing.T) {
	s := conversation("count the invoices in the workspace", 12)
	compact(s, 100_000, 5_000)

	if len(s.Messages) == 0 || s.Messages[0].Role != llm.RoleUser {
		t.Fatal("the task message is gone")
	}
	if got := s.Messages[0].Blocks[0].Text; got != "count the invoices in the workspace" {
		t.Errorf("block 0 of the task message is %q", got)
	}
	if s.Task != "count the invoices in the workspace" {
		t.Errorf("State.Task = %q", s.Task)
	}
}

// The digest is what stops compaction being amnesia: the model should still be
// able to see which tools it already ran and what came back.
func TestCompactLeavesADigestOfWhatWent(t *testing.T) {
	s := conversation("summarise", 10)
	dropped := compact(s, 100_000, 8_000)
	if dropped == 0 {
		t.Fatal("nothing was dropped")
	}

	if len(s.Messages[0].Blocks) != 2 {
		t.Fatalf("task message has %d blocks, want task + digest", len(s.Messages[0].Blocks))
	}
	digest := s.Messages[0].Blocks[1].Text
	if !strings.Contains(digest, "fetch(") {
		t.Errorf("digest does not name the dropped calls:\n%s", digest)
	}
	if !strings.Contains(digest, "result-0") {
		t.Errorf("digest does not carry what the calls returned:\n%s", digest)
	}
	if !strings.Contains(digest, fmt.Sprint(dropped)) {
		t.Errorf("digest does not say how much went:\n%s", digest)
	}
}

// A second compaction must not throw away what the first one recorded, and must
// not stack a second digest behind the first either.
func TestSecondCompactionAccumulatesOneDigest(t *testing.T) {
	s := conversation("summarise", 8)
	if compact(s, 100_000, 9_000) == 0 {
		t.Fatal("first compaction dropped nothing")
	}
	first := s.Messages[0].Blocks[1].Text

	// Grow it again, then compact again.
	s2 := conversation("summarise", 8)
	s.Messages = append(s.Messages, s2.Messages[1:]...)
	if compact(s, 100_000, 6_000) == 0 {
		t.Fatal("second compaction dropped nothing")
	}

	if len(s.Messages[0].Blocks) != 2 {
		t.Fatalf("task message has %d blocks; the digest stacked", len(s.Messages[0].Blocks))
	}
	digest := s.Messages[0].Blocks[1].Text
	if !strings.Contains(digest, fmt.Sprint(s.Dropped)) {
		t.Errorf("digest count is not cumulative (Dropped=%d):\n%s", s.Dropped, digest)
	}
	if strings.Count(digest, "earlier messages have been dropped") != 1 {
		t.Errorf("more than one digest header:\n%s", digest)
	}
	if len(digest) > maxDigestChars+64 {
		t.Errorf("digest is %d chars, past its cap", len(digest))
	}
	_ = first
}

// Under budget, nothing moves. A conversation that never approaches the window
// should never lose anything.
func TestCompactDoesNothingUnderBudget(t *testing.T) {
	s := conversation("short job", 3)
	before := len(s.Messages)

	if n := compact(s, 500, 10_000); n != 0 {
		t.Errorf("dropped %d messages while under budget", n)
	}
	if len(s.Messages) != before || len(s.Messages[0].Blocks) != 1 {
		t.Error("the conversation was modified under budget")
	}
	if n := compact(s, 500, 0); n != 0 {
		t.Errorf("dropped %d messages with compaction disabled", n)
	}
}

// Trimming to exactly the budget puts the run back over it on the very next
// step. The gap is what stops every step from being a compaction.
func TestCompactTrimsBelowTheBudgetNotOntoIt(t *testing.T) {
	s := conversation("summarise", 20)
	before := totalSize(s.Messages)

	compact(s, 40_000, 20_000) // twice over: roughly half must go
	after := totalSize(s.Messages)

	ratio := float64(after) / float64(before)
	if ratio > 0.5*compactTarget+0.15 {
		t.Errorf("kept %.0f%% of the conversation; expected roughly %.0f%%",
			ratio*100, 50*compactTarget)
	}
	if after == 0 {
		t.Error("everything went")
	}
}

// Even an impossible budget leaves the task and the last exchange. A
// conversation trimmed to nothing is not cheaper, it is a restart — the run
// would repeat work it has already paid for.
func TestCompactKeepsTheLastExchange(t *testing.T) {
	s := conversation("summarise", 6)
	compact(s, 1_000_000, 1)

	if len(s.Messages) < 3 {
		t.Fatalf("only %d messages left; task plus one exchange is the floor", len(s.Messages))
	}
	assertWellFormed(t, s.Messages)
}

// The loop compacts on the provider's number, not on a guess of its own.
func TestRunCompactsWhenTheProviderSaysThePromptIsTooBig(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		// A first turn that reports a prompt far past the budget.
		{
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "still working"}},
			Stop:   llm.StopToolUse,
		},
		endResponse("done", 10, 10),
	}}
	// Give the first response a tool call so the conversation grows normally.
	fake.Responses[0].Blocks = append(fake.Responses[0].Blocks, llm.Block{
		Type: llm.BlockToolUse, ID: "c1", Name: "fetch", Args: json.RawMessage(`{}`),
	})
	fake.Responses[0].Usage = llm.Usage{InputTokens: 80_000, OutputTokens: 10}

	var events []trace.Event
	a := &Agent{
		Provider:      fake,
		Model:         "test",
		MaxSteps:      10,
		ContextBudget: 8_000,
		Trace:         func(e trace.Event) { events = append(events, e) },
		RunTool: func(_ context.Context, _ llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "ok"}, nil
		},
	}

	s := conversation("long job", 12)
	before := len(s.Messages)

	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if s.Dropped == 0 {
		t.Fatal("the run never compacted despite the reported prompt size")
	}
	if len(s.Messages) >= before {
		t.Errorf("conversation is %d messages, was %d", len(s.Messages), before)
	}
	assertWellFormed(t, s.Messages)

	// The compaction has to reach the wire, or it saved nothing.
	if len(fake.Calls) < 2 {
		t.Fatalf("only %d requests", len(fake.Calls))
	}
	if len(fake.Calls[1].Messages) >= len(fake.Calls[0].Messages) {
		t.Errorf("second request carried %d messages, first carried %d",
			len(fake.Calls[1].Messages), len(fake.Calls[0].Messages))
	}

	var compacts int
	for _, e := range events {
		if e.Kind == trace.KindCompact {
			compacts++
			if e.InTokens != 80_000 {
				t.Errorf("compact event records %d input tokens", e.InTokens)
			}
		}
	}
	if compacts != 1 {
		t.Errorf("compact events = %d, want 1", compacts)
	}
}

// Compaction must not eat the record a resumed run reads to finish a batch.
func TestCompactLeavesPendingCallsResumable(t *testing.T) {
	s := conversation("long job", 10)
	// A batch that was requested and never finished — what a crash leaves.
	s.Messages = append(s.Messages, llm.Message{
		Role: llm.RoleAssistant,
		Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "pending_1", Name: "calc", Args: json.RawMessage(`{}`)},
		},
	})

	if len(s.pendingToolCalls()) != 1 {
		t.Fatal("fixture is wrong: the call should be pending")
	}

	compact(s, 100_000, 6_000)

	pending := s.pendingToolCalls()
	if len(pending) != 1 || pending[0].ID != "pending_1" {
		t.Fatalf("compaction lost the pending call: %+v", pending)
	}
}

// The prompt size the provider reported is state, not a local variable: a
// resumed run has to make the same decision the original would have, or the
// first request after a resume is the one that blows the window.
func TestInputTokensSurvivesForResume(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{endResponse("done", 0, 0)}}
	fake.Responses[0].Usage = llm.Usage{InputTokens: 4321, OutputTokens: 7}

	a := &Agent{Provider: fake, Model: "test", MaxSteps: 10}
	s := NewState("run_test", "hi")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.InputTokens != 4321 {
		t.Errorf("State.InputTokens = %d, want the provider's count", s.InputTokens)
	}
}

// If an error or interruption happens immediately after compaction (e.g.
// network failure on Complete), resume must not compact a second time against
// the already compacted conversation.
func TestResumeAfterCompactionDoesNotDoubleCompact(t *testing.T) {
	fakeFail := &llm.Fake{Errs: []error{errors.New("network down")}}
	checkpointed := false
	var savedState *State
	a := &Agent{
		Provider:      fakeFail,
		Model:         "test",
		MaxSteps:      10,
		ContextBudget: 8_000,
		Checkpoint: func(st *State) error {
			checkpointed = true
			data, _ := json.Marshal(st)
			var cp State
			_ = json.Unmarshal(data, &cp)
			savedState = &cp
			return nil
		},
	}

	s := conversation("long job", 12)
	s.InputTokens = 80_000

	// First run compacts, checkpoints, and then Complete fails with network error.
	_, err := a.Run(context.Background(), s)
	if err == nil {
		t.Fatal("expected run to fail")
	}
	if !checkpointed || savedState == nil {
		t.Fatal("expected checkpoint to occur after compaction")
	}
	if savedState.Dropped == 0 {
		t.Fatal("expected compaction to have dropped messages")
	}

	droppedFirst := savedState.Dropped
	messagesFirst := len(savedState.Messages)

	// Resume run with a working provider.
	fakeOK := &llm.Fake{Responses: []llm.Response{
		endResponse("recovered", 2000, 10),
	}}
	a.Provider = fakeOK

	_, err = a.Run(context.Background(), savedState)
	if err != nil {
		t.Fatalf("resumed run failed: %v", err)
	}

	if savedState.Dropped != droppedFirst {
		t.Errorf("resumed run double-compacted: dropped %d, was %d",
			savedState.Dropped, droppedFirst)
	}
	if len(savedState.Messages) != messagesFirst+1 {
		t.Errorf("expected %d messages, got %d", messagesFirst+1, len(savedState.Messages))
	}
}

func TestStateContextBudgetInheritedOnResume(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{endResponse("done", 100, 10)}}
	// Agent has no ContextBudget set (simulating `ariadne resume` without flag),
	// but State has ContextBudget = 5000.
	a := &Agent{Provider: fake, Model: "test", MaxSteps: 10}
	s := NewState("run_test", "hi")
	s.ContextBudget = 5000

	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.ContextBudget != 5000 {
		t.Errorf("Agent.ContextBudget = %d, want 5000 from State", a.ContextBudget)
	}
}
