package loop

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
)

// Prove that when the assistant requests multiple tool calls in a single turn,
// they are dispatched concurrently rather than serially.
func TestParallelToolsRunConcurrently(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		{
			Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{"n":1}`)},
				{Type: llm.BlockToolUse, ID: "c2", Name: "calc", Args: json.RawMessage(`{"n":2}`)},
			},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 10},
		},
		endResponse("both done", 10, 10),
	}}

	// A barrier: each call increments started, and waits until both have started.
	// If the loop were serial, call 1 would block waiting for started == 2, and the
	// test would deadlock/timeout. Concurrency lets both start and release the barrier.
	var started atomic.Int32
	ready := make(chan struct{})

	a := &Agent{
		Provider: fake,
		Model:    "test",
		MaxSteps: 10,
		Price:    testPrice,
		Tools:    []llm.ToolDef{{Name: "calc"}},
		RunTool: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			if started.Add(1) == 2 {
				close(ready)
			}
			select {
			case <-ready:
				return llm.ToolResult{Content: c.ID + " ok"}, nil
			case <-time.After(2 * time.Second):
				return llm.ToolResult{}, errors.New("timeout waiting for parallel sibling")
			}
		},
	}

	s := NewState("run_par", "compute two things")
	ans, err := a.Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if ans != "both done" {
		t.Errorf("got ans %q, want %q", ans, "both done")
	}
}

// Regardless of which tool completes first, tool results must be recorded in the
// exact order that the assistant requested them.
func TestParallelToolsPreservesOrder(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		{
			Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ID: "call_slow", Name: "fetch", Args: json.RawMessage(`{}`)},
				{Type: llm.BlockToolUse, ID: "call_fast", Name: "calc", Args: json.RawMessage(`{}`)},
			},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 10},
		},
		endResponse("ordered", 10, 10),
	}}

	fastDone := make(chan struct{})
	a := &Agent{
		Provider: fake,
		Model:    "test",
		MaxSteps: 10,
		Price:    testPrice,
		Tools:    []llm.ToolDef{{Name: "fetch"}, {Name: "calc"}},
		RunTool: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			if c.ID == "call_slow" {
				// Ensure fast finishes first
				select {
				case <-fastDone:
				case <-time.After(2 * time.Second):
					return llm.ToolResult{}, errors.New("slow timed out")
				}
				return llm.ToolResult{Content: "slow result"}, nil
			}
			// call_fast
			close(fastDone)
			return llm.ToolResult{Content: "fast result"}, nil
		},
	}

	s := NewState("run_order", "task")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Message 0: User task
	// Message 1: Assistant with call_slow, call_fast
	// Message 2: Tool results
	if len(s.Messages) < 3 {
		t.Fatalf("expected at least 3 messages, got %d", len(s.Messages))
	}
	results := s.Messages[2].Blocks
	if len(results) != 2 {
		t.Fatalf("expected 2 result blocks, got %d", len(results))
	}
	if results[0].CallID != "call_slow" || results[1].CallID != "call_fast" {
		t.Errorf("results order mismatch: got [%s, %s], want [call_slow, call_fast]",
			results[0].CallID, results[1].CallID)
	}
}

// Partial failure: one tool failing (via error or IsError) must not prevent other
// concurrent tools from executing or recording their results.
func TestParallelToolsPartialFailure(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		{
			Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ID: "c_fail", Name: "calc", Args: json.RawMessage(`{"div":0}`)},
				{Type: llm.BlockToolUse, ID: "c_ok", Name: "calc", Args: json.RawMessage(`{"div":1}`)},
			},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 10},
		},
		endResponse("recovered", 10, 10),
	}}

	a := &Agent{
		Provider: fake,
		Model:    "test",
		MaxSteps: 10,
		Price:    testPrice,
		Tools:    []llm.ToolDef{{Name: "calc"}},
		RunTool: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			if c.ID == "c_fail" {
				return llm.ToolResult{}, errors.New("cannot divide by zero")
			}
			return llm.ToolResult{Content: "result ok"}, nil
		},
	}

	s := NewState("run_partial", "math")
	ans, err := a.Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run should survive partial failure, got %v", err)
	}
	if ans != "recovered" {
		t.Errorf("ans = %q, want recovered", ans)
	}

	results := s.Messages[2].Blocks
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].CallID != "c_fail" || !results[0].IsError || results[0].Content != "cannot divide by zero" {
		t.Errorf("c_fail result incorrect: %+v", results[0])
	}
	if results[1].CallID != "c_ok" || results[1].IsError || results[1].Content != "result ok" {
		t.Errorf("c_ok result incorrect: %+v", results[1])
	}
}

// Pre-flight checks (allow-list and approval gate) must be evaluated cleanly before
// running parallel calls, ensuring denied tools are recorded and approval isn't raced.
func TestParallelToolsPreflightApprovalAndAllowList(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		{
			Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ID: "c_allow", Name: "calc", Args: json.RawMessage(`{}`)},
				{Type: llm.BlockToolUse, ID: "c_deny", Name: "write_file", Args: json.RawMessage(`{}`)},
				{Type: llm.BlockToolUse, ID: "c_gate", Name: "gated_tool", Args: json.RawMessage(`{}`)},
			},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 10},
		},
		endResponse("handled", 10, 10),
	}}

	ran := make(map[string]bool)
	var ranMu sync.Mutex

	a := &Agent{
		Provider:        fake,
		Model:           "test",
		MaxSteps:        10,
		Price:           testPrice,
		Allow:           []string{"calc", "gated_tool"}, // write_file is not permitted
		RequireApproval: []string{"gated_tool"},         // gated_tool requires approval
		Tools: []llm.ToolDef{
			{Name: "calc"},
			{Name: "write_file"},
			{Name: "gated_tool"},
		},
		Approve: func(ctx context.Context, c llm.ToolCall) (bool, error) {
			if c.Name == "gated_tool" {
				return false, nil // reject approval
			}
			return true, nil
		},
		RunTool: func(ctx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			ranMu.Lock()
			ran[c.ID] = true
			ranMu.Unlock()
			return llm.ToolResult{Content: "ran " + c.Name}, nil
		},
	}

	s := NewState("run_preflight", "task")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !ran["c_allow"] {
		t.Error("allowed tool c_allow did not run")
	}
	if ran["c_deny"] {
		t.Error("unpermitted tool c_deny ran")
	}
	if ran["c_gate"] {
		t.Error("unapproved tool c_gate ran")
	}

	results := s.Messages[2].Blocks
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].CallID != "c_allow" || results[0].IsError {
		t.Errorf("result 0 mismatch: %+v", results[0])
	}
	if results[1].CallID != "c_deny" || !results[1].IsError {
		t.Errorf("result 1 mismatch: %+v", results[1])
	}
	if results[2].CallID != "c_gate" || !results[2].IsError {
		t.Errorf("result 2 mismatch: %+v", results[2])
	}
}

// Context cancellation during parallel execution aborts cleanly without corruption.
func TestParallelToolsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	fake := &llm.Fake{Responses: []llm.Response{
		{
			Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{}`)},
				{Type: llm.BlockToolUse, ID: "c2", Name: "calc", Args: json.RawMessage(`{}`)},
			},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 10},
		},
		endResponse("never", 10, 10),
	}}

	started := make(chan struct{})
	a := &Agent{
		Provider: fake,
		Model:    "test",
		MaxSteps: 10,
		Price:    testPrice,
		Tools:    []llm.ToolDef{{Name: "calc"}},
		RunTool: func(cCtx context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			if c.ID == "c1" {
				close(started)
			}
			<-cCtx.Done()
			return llm.ToolResult{}, cCtx.Err()
		},
	}

	go func() {
		<-started
		cancel()
	}()

	s := NewState("run_cancel", "task")
	_, err := a.Run(ctx, s)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// A crash during parallel tool execution leaves completed calls checkpointed.
// Resuming executes only the remaining unfinished calls without re-firing
// the already completed ones, and canonicalizes the final result order.
func TestParallelToolsCrashAndResume(t *testing.T) {
	// Pre-condition: Assistant turn with 3 parallel calls.
	// c1 finished before the crash and was checkpointed.
	// c2 and c3 are still pending.
	s := NewState("run_par_resume", "task")
	s.Model = "test"
	s.Steps = 1
	s.Messages = append(s.Messages,
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{}`)},
			{Type: llm.BlockToolUse, ID: "c2", Name: "calc", Args: json.RawMessage(`{}`)},
			{Type: llm.BlockToolUse, ID: "c3", Name: "calc", Args: json.RawMessage(`{}`)},
		}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "c1", Content: "c1 result"},
		}},
	)

	fake := &llm.Fake{Responses: []llm.Response{
		endResponse("finished all three", 10, 10),
	}}

	var ran []string
	var ranMu sync.Mutex

	a := &Agent{
		Provider: fake,
		Model:    "test",
		MaxSteps: 10,
		Price:    testPrice,
		Tools:    []llm.ToolDef{{Name: "calc"}},
		RunTool: func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			ranMu.Lock()
			ran = append(ran, c.ID)
			ranMu.Unlock()
			return llm.ToolResult{Content: c.ID + " result"}, nil
		},
	}

	ans, err := a.Run(context.Background(), s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ans != "finished all three" {
		t.Errorf("ans = %q, want finished all three", ans)
	}

	// c1 was already done, so only c2 and c3 should have run on resume
	if len(ran) != 2 {
		t.Fatalf("expected 2 tools run on resume, got %d: %v", len(ran), ran)
	}
	for _, id := range ran {
		if id == "c1" {
			t.Fatal("c1 was re-fired on resume")
		}
	}

	// In s.Messages[2].Blocks, all 3 results must be present in canonical order [c1, c2, c3]
	results := s.Messages[2].Blocks
	if len(results) != 3 {
		t.Fatalf("expected 3 total results, got %d", len(results))
	}
	if results[0].CallID != "c1" || results[1].CallID != "c2" || results[2].CallID != "c3" {
		t.Errorf("results not canonically ordered: got [%s, %s, %s], want [c1, c2, c3]",
			results[0].CallID, results[1].CallID, results[2].CallID)
	}
}

// A tool that has already run must have its result recorded, even if the
// context was cancelled while it ran.
//
// This is the regression that made the check worth writing. Cancelling after
// RunTool returned discarded a completed call: nothing was appended, nothing
// was checkpointed, and the call stayed pending on disk — so the next resume
// fired it again. For calc that is a wasted request; for a tool that moves
// money it is the failure this whole design exists to prevent. Cancellation
// stops the next call, never the record of the last one.
func TestCancelAfterToolRanStillRecordsTheResult(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(map[int]string{1: "single", 2: "parallel"}[n], func(t *testing.T) {
			var blocks []llm.Block
			for i := 0; i < n; i++ {
				blocks = append(blocks, llm.Block{
					Type: llm.BlockToolUse,
					ID:   string(rune('a' + i)),
					Name: "send_money",
					Args: json.RawMessage(`{}`),
				})
			}
			fake := &llm.Fake{Responses: []llm.Response{{Blocks: blocks, Stop: llm.StopToolUse}}}

			ctx, cancel := context.WithCancel(context.Background())
			var fired atomic.Int64

			a := &Agent{
				Provider:   fake,
				Model:      "test",
				MaxSteps:   10,
				Checkpoint: func(*State) error { return nil },
				RunTool: func(_ context.Context, _ llm.ToolCall) (llm.ToolResult, error) {
					fired.Add(1)
					cancel() // the caller walks away the instant the money has moved
					return llm.ToolResult{Content: "sent"}, nil
				},
			}

			s := NewState("run_test", "pay the invoices")
			if _, err := a.Run(ctx, s); !errors.Is(err, context.Canceled) {
				t.Fatalf("Run err = %v, want context.Canceled", err)
			}

			var recorded int
			for _, m := range s.Messages {
				for _, b := range m.Blocks {
					if b.Type == llm.BlockToolResult {
						recorded++
					}
				}
			}
			if got := int(fired.Load()); recorded < got {
				t.Errorf("%d tool(s) ran but only %d result(s) recorded", got, recorded)
			}
			// The decisive one: nothing a resume would fire a second time.
			if p := s.pendingToolCalls(); len(p) != 0 {
				t.Errorf("%d completed call(s) still pending; a resume re-fires them", len(p))
			}
		})
	}
}

// The canonical order has to be the order on disk, not only the order in
// memory. Each call checkpoints as it finishes, so without a write after the
// sort the last checkpoint of the batch holds completion order instead.
//
// The batch is deliberately the last thing that happens: only one response is
// scripted, so the loop asks a second time, the fake refuses, and the run ends
// without another checkpoint. Otherwise the ordinary end-of-run write lands on
// top and hides the window entirely — which is exactly what happened to the
// first version of this test, and why it passed against the unfixed code.
func TestParallelOrderIsCheckpointed(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		{
			Blocks: []llm.Block{
				{Type: llm.BlockToolUse, ID: "a", Name: "slow", Args: json.RawMessage(`{}`)},
				{Type: llm.BlockToolUse, ID: "b", Name: "fast", Args: json.RawMessage(`{}`)},
			},
			Stop: llm.StopToolUse,
		},
	}}

	ids := func(s *State) []string {
		var out []string
		for _, m := range s.Messages {
			for _, b := range m.Blocks {
				if b.Type == llm.BlockToolResult {
					out = append(out, b.CallID)
				}
			}
		}
		return out
	}

	// "b" finishes and is checkpointed first, so completion order is [b a] and
	// only the sort can make the final checkpoint [a b].
	bLanded := make(chan struct{})
	var mu sync.Mutex
	var lastSaved []string

	a := &Agent{
		Provider: fake,
		Model:    "test",
		MaxSteps: 10,
		Checkpoint: func(s *State) error {
			got := ids(s)
			mu.Lock()
			if len(got) > 0 {
				lastSaved = got
			}
			mu.Unlock()
			if len(got) == 1 && got[0] == "b" {
				select {
				case <-bLanded:
				default:
					close(bLanded)
				}
			}
			return nil
		},
		RunTool: func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			if c.Name == "slow" {
				<-bLanded
			}
			return llm.ToolResult{Content: c.Name}, nil
		},
	}

	s := NewState("run_test", "two calls")
	if _, err := a.Run(context.Background(), s); !errors.Is(err, llm.ErrFakeExhausted) {
		t.Fatalf("Run err = %v, want the fake to run out after the batch", err)
	}

	want := []string{"a", "b"}
	got := ids(s)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("in memory = %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lastSaved) != 2 || lastSaved[0] != want[0] || lastSaved[1] != want[1] {
		t.Errorf("last checkpoint = %v, want %v — the sorted order never reached disk", lastSaved, want)
	}
}
