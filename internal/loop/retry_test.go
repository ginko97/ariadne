package loop

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
)

// The case that asked for this: a turn ran its tools, then the connection
// dropped while the model was writing the answer. Trying again must send the
// model exactly what the failed call sent — no "continue" in the history — and
// must not run the tool a second time.
func TestRetryFinishesATurnWhoseModelCallFailed(t *testing.T) {
	dropped := errors.New("read stream: connection forcibly closed by the remote host")
	fake := &llm.Fake{
		Responses: []llm.Response{
			toolUseResponse("call_1", "calc", `{"expr":"9600-4150+880"}`, 20, 5),
			{}, // never returned: Errs[1] stands in for it
			endResponse("The closing balance is 6330.", 40, 8),
		},
		Errs: []error{nil, dropped, nil},
	}
	ran := 0
	a := &Agent{
		Provider: fake,
		Model:    "test",
		MaxSteps: 10,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			ran++
			return llm.ToolResult{Content: "6330"}, nil
		},
	}
	s := NewState("run_retry", "what is the closing balance?")

	if _, err := a.Run(context.Background(), s); !errors.Is(err, dropped) {
		t.Fatalf("first attempt: err = %v, want the dropped connection", err)
	}
	if !s.AwaitsAnswer() {
		t.Fatal("after a failed model call the conversation does not report that it awaits an answer")
	}
	before := len(s.Messages)

	answer, err := a.Retry(context.Background(), s)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if answer != "The closing balance is 6330." {
		t.Errorf("answer = %q", answer)
	}
	if ran != 1 {
		t.Errorf("the tool ran %d times; trying again must not repeat it", ran)
	}
	if got := len(s.Messages); got != before+1 || s.Messages[got-1].Role != llm.RoleAssistant {
		t.Errorf("Retry added %d messages ending in %q; want only the answer", got-before, s.Messages[got-1].Role)
	}
	if len(fake.Calls) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(fake.Calls))
	}
	if failed, retried := len(fake.Calls[1].Messages), len(fake.Calls[2].Messages); failed != retried {
		t.Errorf("the retry sent %d messages where the failed call sent %d; it must ask the same question", retried, failed)
	}
	if s.AwaitsAnswer() {
		t.Error("a conversation with its answer still reports that it awaits one")
	}
}

// Retry is not a way to re-ask a question that was answered, or to skip an
// unfinished batch; both are refused before the provider is called.
func TestRetryRefusesAConversationThatIsNotWaiting(t *testing.T) {
	answered := NewState("run_done", "hi")
	answered.Messages = append(answered.Messages, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "hello"}},
	})
	pending := NewState("run_pending", "hi")
	pending.Messages = append(pending.Messages, llm.Message{
		Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{}`)}},
	})

	for name, s := range map[string]*State{"answered": answered, "pending batch": pending} {
		fake := &llm.Fake{Responses: []llm.Response{endResponse("again", 1, 1)}}
		a := &Agent{Provider: fake, Model: "test", MaxSteps: 5}
		if _, err := a.Retry(context.Background(), s); !errors.Is(err, ErrNothingToRetry) {
			t.Errorf("%s: err = %v, want ErrNothingToRetry", name, err)
		}
		if len(fake.Calls) != 0 {
			t.Errorf("%s: the provider was called %d times", name, len(fake.Calls))
		}
	}
}

func TestAwaitsAnswer(t *testing.T) {
	text := func(role llm.Role, s string) llm.Message {
		return llm.Message{Role: role, Blocks: []llm.Block{{Type: llm.BlockText, Text: s}}}
	}
	use := llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{}`)}}}
	two := llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
		{Type: llm.BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{}`)},
		{Type: llm.BlockToolUse, ID: "c2", Name: "calc", Args: json.RawMessage(`{}`)},
	}}
	result := llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockToolResult, CallID: "c1", Content: "2"}}}

	for _, c := range []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"empty", nil, false},
		{"question, no answer yet", []llm.Message{text(llm.RoleUser, "q")}, true},
		{"answered", []llm.Message{text(llm.RoleUser, "q"), text(llm.RoleAssistant, "a")}, false},
		{"batch finished, no answer yet", []llm.Message{text(llm.RoleUser, "q"), use, result}, true},
		{"batch unfinished", []llm.Message{text(llm.RoleUser, "q"), use}, false},
		// Ends on the person's side like a finished batch, but one call has no
		// result: that is Resume's case, and must not look like a lost answer.
		{"batch half done", []llm.Message{text(llm.RoleUser, "q"), two, result}, false},
	} {
		s := &State{Messages: c.msgs}
		if got := s.AwaitsAnswer(); got != c.want {
			t.Errorf("%s: AwaitsAnswer = %v, want %v", c.name, got, c.want)
		}
	}
}
