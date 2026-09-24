package loop

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
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
