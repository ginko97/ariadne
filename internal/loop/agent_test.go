package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/trace"
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

// A full two-step tool-using run, with no network and no API key.
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

func TestRunRecordsBaseURL(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		endResponse("done", 10, 5),
	}}
	a := &Agent{
		Provider: fake,
		Model:    "test-model",
		BaseURL:  "https://custom.endpoint/v1",
	}

	s := NewState("r", "test")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.BaseURL != "https://custom.endpoint/v1" {
		t.Errorf("BaseURL = %q, want https://custom.endpoint/v1", s.BaseURL)
	}
	if s.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", s.Model)
	}

	// Resuming a state with an existing BaseURL must not overwrite it:
	s2 := &State{
		RunID:   "r2",
		Task:    "test",
		Model:   "original-model",
		BaseURL: "https://original.endpoint/v1",
		Messages: []llm.Message{{
			Role:   llm.RoleUser,
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "test"}},
		}},
	}
	fake2 := &llm.Fake{Responses: []llm.Response{
		endResponse("done2", 10, 5),
	}}
	a2 := &Agent{
		Provider: fake2,
		Model:    "different-model",
		BaseURL:  "https://different.endpoint/v1",
	}
	if _, err := a2.Run(context.Background(), s2); err != nil {
		t.Fatal(err)
	}
	if s2.BaseURL != "https://original.endpoint/v1" {
		t.Errorf("BaseURL overwritten: got %q", s2.BaseURL)
	}
	if s2.Model != "original-model" {
		t.Errorf("Model overwritten: got %q", s2.Model)
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

	// Per call, not per step: as the run starts, before any model call (1
	// message, the question); after the assistant turn requests a tool (2,
	// nothing executed); after the result lands (3); and after the final
	// answer (4). The first keeps a question whose first answer never came;
	// the second is what makes resume safe.
	want := []int{1, 2, 3, 4}
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

// The claim: a resumed run finishes a half-executed batch without re-firing the
// calls that already completed, and without asking the model again — which
// would mint fresh call IDs and defeat any idempotency key.
func TestResumeFinishesBatchWithoutRefiring(t *testing.T) {
	// A checkpoint as a crash would leave it: two calls requested, one done.
	s := NewState("run_resume", "two things")
	s.Model = "test"
	s.Steps = 1
	s.Messages = append(s.Messages,
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "call_a", Name: "calc", Args: json.RawMessage(`{"expr":"1+1"}`)},
			{Type: llm.BlockToolUse, ID: "call_b", Name: "calc", Args: json.RawMessage(`{"expr":"2+2"}`)},
		}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "call_a", Content: "2"},
		}},
	)

	// Only one provider response is scripted: if the loop asks the model before
	// finishing the batch, Fake runs dry and the test fails loudly.
	fake := &llm.Fake{Responses: []llm.Response{endResponse("done", 1, 1)}}

	var ran []string
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10, Price: testPrice,
		RunTool: func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			ran = append(ran, c.ID)
			return llm.ToolResult{Content: "4"}, nil
		},
	}

	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// call_a already had its side effect. Running it again is the bug.
	if len(ran) != 1 || ran[0] != "call_b" {
		t.Fatalf("executed %v, want only [call_b]", ran)
	}

	// The finished batch reached the provider as one message with both results,
	// each keyed to its original id.
	sent := fake.Calls[0].Messages
	results := sent[len(sent)-1].Blocks
	if len(results) != 2 || results[0].CallID != "call_a" || results[1].CallID != "call_b" {
		t.Errorf("results message = %+v, want both original call ids", results)
	}
}

// A crash after the calls were requested but before any ran: all of them are
// pending, none have been executed, and the model is still not re-asked.
func TestResumeRunsWholeBatchWhenNoneCompleted(t *testing.T) {
	s := NewState("run_resume2", "one thing")
	s.Model = "test"
	s.Steps = 1
	s.Messages = append(s.Messages, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Type: llm.BlockToolUse, ID: "call_z", Name: "calc", Args: json.RawMessage(`{"expr":"3+3"}`)},
	}})

	fake := &llm.Fake{Responses: []llm.Response{endResponse("done", 1, 1)}}

	var ran []string
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10, Price: testPrice,
		RunTool: func(_ context.Context, c llm.ToolCall) (llm.ToolResult, error) {
			ran = append(ran, c.ID)
			return llm.ToolResult{Content: "6"}, nil
		},
	}

	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ran) != 1 || ran[0] != "call_z" {
		t.Fatalf("executed %v, want [call_z]", ran)
	}
}

// A run that ends in an error still writes what the model said. Without this the
// on-disk state reverts to the previous checkpoint and the evidence of what
// caused the failure is gone — which is exactly what trace analysis wants.
func TestCheckpointOnErrorPaths(t *testing.T) {
	cases := []struct {
		name string
		resp llm.Response
	}{
		{"truncated", llm.Response{
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "half a th"}},
			Stop:   llm.StopMaxToken,
		}},
		{"empty tool use", llm.Response{
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "thinking"}},
			Stop:   llm.StopToolUse,
		}},
		{"unknown stop", llm.Response{
			Blocks: []llm.Block{{Type: llm.BlockText, Text: "?"}},
			Stop:   llm.StopReason("wat"),
		}},
		{"no tool runner", llm.Response{
			Blocks: []llm.Block{
				{Type: llm.BlockText, Text: "using tool"},
				{Type: llm.BlockToolUse, ID: "call_1", Name: "calc", Args: json.RawMessage(`{}`)},
			},
			Stop: llm.StopToolUse,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var saved int
			a := &Agent{
				Provider: &llm.Fake{Responses: []llm.Response{tc.resp}},
				Model:    "test", MaxSteps: 10, Price: testPrice,
				Checkpoint: func(s *State) error { saved = len(s.Messages); return nil },
			}

			if _, err := a.Run(context.Background(), NewState("run_err", "t")); err == nil {
				t.Fatal("want an error")
			}
			// task + the assistant turn that caused the failure
			if saved != 2 {
				t.Errorf("checkpoint saw %d messages, want 2 — the failing turn was not written", saved)
			}
		})
	}
}

// The gateway's own figure beats the local table; the table is the fallback.
func TestPriceUsesReportedCost(t *testing.T) {
	p := Price{InputPerMTok: 1000, OutputPerMTok: 1000} // deliberately absurd

	reported, known := p.Cost(llm.Usage{InputTokens: 1, OutputTokens: 1, Cost: 0.25, CostReported: true})
	if reported != 0.25 || !known {
		t.Errorf("got %v known=%v, want the reported 0.25 — the table should not win", reported, known)
	}

	// A free model on a gateway reports zero, and zero is what it cost.
	free, known := p.Cost(llm.Usage{InputTokens: 1, OutputTokens: 1, CostReported: true})
	if free != 0 || !known {
		t.Errorf("got %v known=%v, want a known 0 — a reported zero is free, not unknown", free, known)
	}

	fallback, known := p.Cost(llm.Usage{InputTokens: 1_000_000, OutputTokens: 0})
	if fallback != 1000 || !known {
		t.Errorf("got %v known=%v, want 1000 from the table when cost is unreported", fallback, known)
	}

	// Nothing reported and no table: unmeasured, not free.
	if _, known := (Price{}).Cost(llm.Usage{InputTokens: 1_000_000, OutputTokens: 1000}); known {
		t.Error("no reported cost and no price came back as known; it would print as $0.0000")
	}
}

// A trace has to describe the whole run, in order, with the facts that error
// analysis needs: which tool, which arguments, what came back, what it cost.
func TestTraceRecordsWholeRun(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("call_a1", "calc", `{"expr":"240*0.15"}`, 52, 18),
		endResponse("15% of 240 is 36.", 94, 11),
	}}

	var events []trace.Event
	a := &Agent{
		Provider: fake, Model: "test/model", MaxSteps: 10, Price: testPrice,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "36"}, nil
		},
		Trace: func(e trace.Event) { events = append(events, e) },
	}

	if _, err := a.Run(context.Background(), NewState("run_tr", "what is 15% of 240?")); err != nil {
		t.Fatal(err)
	}

	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{
		trace.KindRunStart,
		trace.KindRequest, trace.KindResponse,
		trace.KindToolCall, trace.KindToolResult,
		trace.KindRequest, trace.KindResponse,
		trace.KindRunEnd,
	}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v\nwant  %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event %d = %q, want %q\nfull: %v", i, kinds[i], want[i], kinds)
		}
	}

	// The tool call carries what it was asked to do, not just that it happened.
	call := events[3]
	if call.Tool != "calc" || call.CallID != "call_a1" || string(call.Args) != `{"expr":"240*0.15"}` {
		t.Errorf("tool_call lost detail: %+v", call)
	}
	if events[4].Content != "36" {
		t.Errorf("tool_result content = %q", events[4].Content)
	}
	if events[2].InTokens != 52 || events[2].Cost <= 0 {
		t.Errorf("response lost usage: %+v", events[2])
	}
	if last := events[len(events)-1]; last.Error != "" || last.Cost <= 0 {
		t.Errorf("run_end should record success and total cost: %+v", last)
	}
}

// A failed run still has to say how it ended, or the failure is invisible to
// analysis — which is when you most want the trace.
func TestTraceRecordsFailure(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "calc", `{}`, 10, 10),
		toolUseResponse("c2", "calc", `{}`, 10, 10),
	}}

	var events []trace.Event
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 1, Price: testPrice,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "ok"}, nil
		},
		Trace: func(e trace.Event) { events = append(events, e) },
	}

	if _, err := a.Run(context.Background(), NewState("run_fail", "t")); err == nil {
		t.Fatal("want an error")
	}

	last := events[len(events)-1]
	if last.Kind != trace.KindRunEnd {
		t.Fatalf("last event = %q, want run_end", last.Kind)
	}
	if !strings.Contains(last.Error, "step limit") {
		t.Errorf("run_end lost the cause: %q", last.Error)
	}
}

func TestTraceRecordsNoToolRunnerFailure(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "calc", `{}`, 10, 10),
	}}

	var events []trace.Event
	a := &Agent{
		Provider: fake,
		Model:    "test",
		Trace:    func(e trace.Event) { events = append(events, e) },
	}

	if _, err := a.Run(context.Background(), NewState("run_fail", "t")); !errors.Is(err, ErrNoToolRunner) {
		t.Fatalf("got %v, want ErrNoToolRunner", err)
	}

	if len(events) == 0 {
		t.Fatal("no trace events emitted")
	}
	last := events[len(events)-1]
	if last.Kind != trace.KindRunEnd {
		t.Fatalf("last event = %q, want run_end", last.Kind)
	}
	if !strings.Contains(last.Error, "no ToolRunner") {
		t.Errorf("run_end error = %q, want mention of ToolRunner", last.Error)
	}
}

// The run records who answered, kept separate from the model it asks for.
// Folding them together would change what a resume requests; keeping only the
// request loses the one variable a comparison across sweeps needs.
func TestRunRecordsWhoAnsweredWithoutChangingWhatItAsksFor(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{{
		Blocks:   []llm.Block{{Type: llm.BlockText, Text: "done"}},
		Stop:     llm.StopEnd,
		Usage:    llm.Usage{InputTokens: 10, OutputTokens: 2},
		Model:    "openai/gpt-oss-20b",
		Provider: "Darkbloom",
	}}}

	var events []trace.Event
	a := &Agent{
		Provider: fake,
		Model:    "openai/gpt-oss-20b",
		MaxSteps: 3,
		Trace:    func(e trace.Event) { events = append(events, e) },
	}
	s := NewState("run_provider", "do a thing")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	if s.Provider != "Darkbloom" {
		t.Errorf("State.Provider = %q, want the backend that served it", s.Provider)
	}
	if s.Model != "openai/gpt-oss-20b" {
		t.Errorf("State.Model = %q — the requested model must not move", s.Model)
	}

	var resp *trace.Event
	for i := range events {
		if events[i].Kind == trace.KindResponse {
			resp = &events[i]
		}
	}
	if resp == nil {
		t.Fatal("no response event")
	}
	if resp.Provider != "Darkbloom" {
		t.Errorf("response event provider = %q, want Darkbloom", resp.Provider)
	}
}

// hangingProvider is a model that has not answered yet: it waits for its
// context to end.
type hangingProvider struct{ called chan struct{} }

func (p hangingProvider) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	close(p.called)
	<-ctx.Done()
	return llm.Response{}, ctx.Err()
}

// A run stopped or killed during its very first model call still exists on
// disk, with its question. Nothing else writes until the model answers, so
// without the checkpoint at the start of Run a fresh conversation lived only
// in memory until then: Stop in the browser left the page holding a run id
// the server had never saved, and the next message got "no such conversation".
func TestRunIsSavedBeforeTheFirstModelCall(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	p := hangingProvider{called: make(chan struct{})}
	a := &Agent{Provider: p, Model: "test", MaxSteps: 5, Checkpoint: store.Save}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, NewState("run_first_call", "a question worth keeping"))
		done <- err
	}()
	<-p.called
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}

	st, err := store.Load("run_first_call")
	if err != nil {
		t.Fatalf("the stopped run was never saved: %v", err)
	}
	if st.Task != "a question worth keeping" || len(st.Messages) != 1 {
		t.Errorf("saved state = task %q, %d messages; want the question and nothing else", st.Task, len(st.Messages))
	}
}

// A provider that reports tokens but no cost, with no price table, leaves the
// run's cost unmeasured. The state and the trace must say so, so that nothing
// downstream prints $0.0000 for a run that was billed.
func TestUnreportedCostIsUnknownNotZero(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("call_a1", "calc", `{"expr":"1+1"}`, 52, 18),
		endResponse("2", 94, 11),
	}}
	var events []trace.Event
	a := &Agent{
		Provider: fake, Model: "test/model", MaxSteps: 10,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "2"}, nil
		},
		Trace: func(e trace.Event) { events = append(events, e) },
	}
	s := NewState("run_unpriced", "1+1?")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.UnpricedSteps != 2 {
		t.Errorf("UnpricedSteps = %d, want 2", s.UnpricedSteps)
	}
	for _, e := range events {
		if (e.Kind == trace.KindResponse || e.Kind == trace.KindRunEnd) && !e.CostUnknown {
			t.Errorf("%s event has CostUnknown=false; its $%v is not a measured cost", e.Kind, e.Cost)
		}
	}

	// The same run on a provider that reports a cost is measured throughout.
	priced := func(r llm.Response) llm.Response {
		r.Usage.Cost, r.Usage.CostReported = 0.001, true
		return r
	}
	a.Provider = &llm.Fake{Responses: []llm.Response{
		priced(toolUseResponse("call_b1", "calc", `{"expr":"1+1"}`, 52, 18)),
		priced(endResponse("2", 94, 11)),
	}}
	events = nil
	s = NewState("run_priced", "1+1?")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.UnpricedSteps != 0 {
		t.Errorf("UnpricedSteps = %d on a provider that reports cost, want 0", s.UnpricedSteps)
	}
	for _, e := range events {
		if e.CostUnknown {
			t.Errorf("%s event marked CostUnknown on a provider that reports cost", e.Kind)
		}
	}
}
