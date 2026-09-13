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

// The whole of multi-turn in one test: a conversation is a run you keep adding
// to, and the second turn can see the first.
func TestChatTurnContinuesTheSameRun(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		endResponse("Hello.", 10, 10),
		endResponse("You said hello.", 10, 10),
	}}
	a := &Agent{Provider: fake, Model: "test", MaxSteps: 10}

	s := NewState("run_test", "hello")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	answer, err := a.ChatTurn(context.Background(), s, "what did I just say?")
	if err != nil {
		t.Fatalf("ChatTurn: %v", err)
	}
	if answer != "You said hello." {
		t.Errorf("answer = %q", answer)
	}

	// The second request must carry the first exchange, or it is not a
	// conversation, it is two runs that happen to share a file.
	second := fake.Calls[1].Messages
	if len(second) < 3 {
		t.Fatalf("second request carried %d messages", len(second))
	}
	var joined strings.Builder
	for _, m := range second {
		for _, b := range m.Blocks {
			joined.WriteString(b.Text + "\n")
		}
	}
	for _, want := range []string{"hello", "Hello.", "what did I just say?"} {
		if !strings.Contains(joined.String(), want) {
			t.Errorf("the second turn did not carry %q", want)
		}
	}
	if s.Turns() != 2 {
		t.Errorf("Turns() = %d, want 2", s.Turns())
	}
}

// The refusal that is the reason AddUserMessage exists at all.
//
// A user turn between an assistant's tool_use and its results is two failures:
// the provider rejects the request, and pendingToolCalls can no longer find the
// pair it reads the completion record from — so a resumed run could re-fire a
// tool that already ran.
func TestAddUserMessageRefusesMidBatch(t *testing.T) {
	s := NewState("run_test", "do something")
	s.Messages = append(s.Messages, llm.Message{
		Role: llm.RoleAssistant,
		Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{}`)},
		},
	})

	before := len(s.Messages)
	err := s.AddUserMessage("actually, never mind")
	if !errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("err = %v, want ErrTurnInFlight", err)
	}
	if len(s.Messages) != before {
		t.Error("a refused message was appended anyway")
	}
	// And the pending call is still findable, which is the point.
	if len(s.pendingToolCalls()) != 1 {
		t.Error("the pending call was disturbed")
	}
}

func TestAddUserMessageRejectsEmpty(t *testing.T) {
	s := NewState("run_test", "task")
	for _, text := range []string{"", "   ", "\n\t "} {
		if err := s.AddUserMessage(text); err == nil {
			t.Errorf("accepted an empty message %q", text)
		}
	}
	if len(s.Messages) != 1 {
		t.Error("an empty message was appended")
	}
}

// MaxSteps bounds one turn, not the conversation. Counting cumulatively meant
// turn two started with the budget partly spent and a later turn failed before
// saying anything — a limit meant to stop a runaway loop instead ended a
// working chat.
func TestStepLimitIsPerTurnNotPerConversation(t *testing.T) {
	var resp []llm.Response
	for i := 0; i < 8; i++ {
		resp = append(resp, endResponse("ok", 10, 10))
	}
	fake := &llm.Fake{Responses: resp}
	a := &Agent{Provider: fake, Model: "test", MaxSteps: 2}

	s := NewState("run_test", "first")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	// Four more turns. Cumulative counting would fail on the second.
	for i := 0; i < 4; i++ {
		if _, err := a.ChatTurn(context.Background(), s, "again"); err != nil {
			t.Fatalf("turn %d failed: %v", i+2, err)
		}
	}
	if s.Turns() != 5 {
		t.Errorf("Turns() = %d, want 5", s.Turns())
	}
	if s.Steps != 5 {
		t.Errorf("Steps = %d; cost and steps stay cumulative", s.Steps)
	}
}

// A runaway turn is still bounded — per turn is not unbounded.
func TestStepLimitStillStopsARunawayTurn(t *testing.T) {
	var resp []llm.Response
	for i := 0; i < 10; i++ {
		resp = append(resp, toolUseResponse("c", "calc", `{}`, 10, 10))
	}
	a := &Agent{
		Provider: &llm.Fake{Responses: resp}, Model: "test", MaxSteps: 3,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "ok"}, nil
		},
	}
	s := NewState("run_test", "loop forever")
	if _, err := a.Run(context.Background(), s); !errors.Is(err, ErrStepLimit) {
		t.Fatalf("err = %v, want ErrStepLimit", err)
	}
}

// Turns counts what the person said. Tool results are user-role messages and
// must not inflate it.
func TestTurnsIgnoresToolResults(t *testing.T) {
	s := conversation("the task", 3) // three tool round trips
	if got := s.Turns(); got != 1 {
		t.Errorf("Turns() = %d, want 1 — tool results are not turns", got)
	}
}

// A conversation is checkpointed like anything else, so closing the terminal
// mid-chat loses nothing.
func TestChatTurnIsCheckpointed(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		endResponse("one", 10, 10), endResponse("two", 10, 10),
	}}
	var saved int
	a := &Agent{
		Provider: fake, Model: "test", MaxSteps: 10,
		Checkpoint: func(*State) error { saved++; return nil },
	}

	s := NewState("run_test", "first")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	before := saved
	if _, err := a.ChatTurn(context.Background(), s, "second"); err != nil {
		t.Fatal(err)
	}
	if saved <= before {
		t.Error("a chat turn was not checkpointed")
	}
}

func TestSetModelAndSwitchingAcrossTurns(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		endResponse("from model a", 10, 10),
		endResponse("from model b", 10, 10),
	}}
	a1 := &Agent{Provider: fake, Model: "model-a", MaxSteps: 10}
	s := NewState("run_test", "hello")

	if _, err := a1.ChatTurn(context.Background(), s, "first"); err != nil {
		t.Fatal(err)
	}
	if s.Model != "model-a" {
		t.Errorf("s.Model = %q, want model-a", s.Model)
	}

	// Model switch for turn 2.
	a2 := &Agent{Provider: fake, Model: "model-b", MaxSteps: 10}
	if _, err := a2.ChatTurn(context.Background(), s, "second"); err != nil {
		t.Fatal(err)
	}
	if s.Model != "model-b" {
		t.Errorf("s.Model = %q, want model-b", s.Model)
	}
	if fake.Calls[1].Model != "model-b" {
		t.Errorf("fake.Calls[1].Model = %q, want model-b", fake.Calls[1].Model)
	}

	// Model switch refused mid-batch.
	s.Messages = append(s.Messages, llm.Message{
		Role: llm.RoleAssistant,
		Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "call_mid", Name: "calc", Args: json.RawMessage(`{}`)},
		},
	})
	if err := s.SetModel("model-c"); !errors.Is(err, ErrTurnInFlight) {
		t.Errorf("SetModel mid-batch err = %v, want ErrTurnInFlight", err)
	}
}

// Task titles the conversation and run_start describes the turn. Keeping both
// is the point: a conversation that opened with a migration plan and whose last
// message was "thanks" must still be listed under the migration, while the trace
// for that turn must still say what the turn was.
func TestTaskStaysTheOpenerAndRunStartCarriesTheTurn(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		endResponse("a plan", 40, 5),
		endResponse("you are welcome", 60, 4),
	}}

	var starts []string
	a := &Agent{
		Provider: fake, Model: "test/model", MaxSteps: 10,
		Trace: func(e trace.Event) {
			if e.Kind == trace.KindRunStart {
				starts = append(starts, e.Text)
			}
		},
	}

	s := NewState("run_task", "help me plan the Northwind migration")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ChatTurn(context.Background(), s, "thanks"); err != nil {
		t.Fatal(err)
	}

	if s.Task != "help me plan the Northwind migration" {
		t.Errorf("s.Task = %q, want the opening message", s.Task)
	}
	want := []string{"help me plan the Northwind migration", "thanks"}
	if len(starts) != len(want) {
		t.Fatalf("run_start texts = %v, want %v", starts, want)
	}
	for i := range want {
		if starts[i] != want[i] {
			t.Errorf("run_start[%d] = %q, want %q", i, starts[i], want[i])
		}
	}
}

// Resumed mid-batch the trailing message is tool results, so there is no turn
// text to find and the opener is what run_start has left to report.
func TestTurnPromptFallsBackToTaskMidBatch(t *testing.T) {
	s := NewState("run_mid", "summarise the invoice")
	s.Messages = append(s.Messages,
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockToolUse, ID: "call_1", Name: "calc", Args: json.RawMessage(`{}`)},
		}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, ID: "call_1", Text: "36"},
		}},
	)
	if got := s.turnPrompt(); got != "summarise the invoice" {
		t.Errorf("turnPrompt() = %q, want the opener", got)
	}
}
