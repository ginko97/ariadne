package loop

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/trace"
)

func systemOf(t *testing.T, req llm.Request) string {
	t.Helper()
	if len(req.Messages) == 0 || req.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("request has no system message: %+v", req.Messages)
	}
	return req.Messages[0].Blocks[0].Text
}

// Every request says what day it is, read at the time of the request; the
// saved conversation does not, so a conversation continued on another day
// is told that day.
func TestEachRequestCarriesTodaysDateAndTheCheckpointDoesNot(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		endResponse("first", 10, 5),
		endResponse("second", 10, 5),
	}}
	day := time.Date(2026, 9, 24, 23, 0, 0, 0, time.Local)
	a := &Agent{
		Provider: fake, Model: "m", System: "You are Ariadne.",
		Today: func() time.Time { return day },
	}

	s := NewState("r", "what year is it?")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	first := systemOf(t, fake.Calls[0])
	if first != "You are Ariadne.\n\nToday's date is Thursday 24 September 2026." {
		t.Errorf("system prompt = %q", first)
	}
	if strings.Contains(s.System, "2026") {
		t.Errorf("the date was saved into the conversation: %q", s.System)
	}

	day = day.Add(2 * time.Hour) // past midnight
	if _, err := a.ChatTurn(context.Background(), s, "and now?"); err != nil {
		t.Fatal(err)
	}
	if got := systemOf(t, fake.Calls[1]); !strings.HasSuffix(got, "Today's date is Friday 25 September 2026.") {
		t.Errorf("next day's request = %q, want Friday 25 September", got)
	}
}

// An agent that is not told the date sends the system prompt unchanged.
// cmd/ariadne sets Today on every agent it builds, eval included.
func TestNoDateWithoutToday(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{endResponse("ok", 10, 5)}}
	a := &Agent{Provider: fake, Model: "m", System: "You are Ariadne."}
	if _, err := a.Run(context.Background(), NewState("r", "hi")); err != nil {
		t.Fatal(err)
	}
	if got := systemOf(t, fake.Calls[0]); got != "You are Ariadne." {
		t.Errorf("system prompt = %q, want it unchanged", got)
	}
}

// Notes are read once per turn: a note saved between two messages is in the
// second one's request, every step of one turn sees the same notes, nothing
// is stored on the conversation, and the trace records what each turn got.
func TestMemoryIsReadOncePerTurn(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "calc", `{}`, 10, 10),
		endResponse("first", 10, 5),
		endResponse("second", 10, 5),
	}}
	notes := "<memory>\n- one\n</memory>"
	reads := 0
	var events []trace.Event
	a := &Agent{
		Provider: fake, Model: "m", System: "You are Ariadne.", MaxSteps: 5,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) { return llm.ToolResult{Content: "2"}, nil },
		Memory:  func() string { reads++; return notes },
		Trace:   func(e trace.Event) { events = append(events, e) },
	}
	s := NewState("r", "q")
	if _, err := a.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Errorf("a two-step turn read memory %d times, want 1", reads)
	}
	for i := 0; i < 2; i++ {
		if got := systemOf(t, fake.Calls[i]); got != "You are Ariadne.\n\n<memory>\n- one\n</memory>" {
			t.Errorf("step %d system prompt = %q", i+1, got)
		}
	}

	notes = "<memory>\n- one\n- two\n</memory>" // saved between messages
	if _, err := a.ChatTurn(context.Background(), s, "and now?"); err != nil {
		t.Fatal(err)
	}
	if got := systemOf(t, fake.Calls[2]); !strings.Contains(got, "- two") {
		t.Errorf("a note saved between messages is missing from the next turn: %q", got)
	}
	if s.System != "You are Ariadne." {
		t.Errorf("notes were stored on the conversation: %q", s.System)
	}
	var recorded []string
	for _, e := range events {
		if e.Kind == trace.KindMemory {
			recorded = append(recorded, e.Content)
		}
	}
	if len(recorded) != 2 || !strings.Contains(recorded[1], "- two") {
		t.Errorf("memory trace events = %q, want one per turn with that turn's notes", recorded)
	}
}
