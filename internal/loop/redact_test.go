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

const plantedKey = "sk-or-v1-planted-for-this-test-only"

func redactPlanted(s string) string {
	return strings.ReplaceAll(s, plantedKey, "[REDACTED OPENROUTER_API_KEY]")
}

// A tool that prints the provider key — an approved `type ..\.env` — must not
// put the key in any of the three places tool output goes: the next request to
// the provider, the trace, and the checkpoint.
func TestRedactScrubsToolOutputEverywhereItGoes(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "exec", `{"argv":["cmd","/c","type","..\\.env"]}`, 10, 10),
		endResponse("I read the file.", 10, 10),
	}}

	var events []trace.Event
	var saved []byte
	a := &Agent{
		Provider: fake,
		Model:    "test",
		MaxSteps: 5,
		Redact:   redactPlanted,
		Trace:    func(e trace.Event) { events = append(events, e) },
		Checkpoint: func(s *State) error {
			b, err := json.Marshal(s)
			saved = b
			return err
		},
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{
				Content:   "OPENROUTER_API_KEY=" + plantedKey + "\nGEMINI_API_KEY=",
				Untrusted: true,
			}, nil
		},
	}
	if _, err := a.Run(context.Background(), NewState("run_redact", "read the env file")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sent, _ := json.Marshal(fake.Calls[1])
	if strings.Contains(string(sent), plantedKey) {
		t.Error("the key was sent to the provider")
	}
	if !strings.Contains(string(sent), "[REDACTED OPENROUTER_API_KEY]") {
		t.Error("the redaction label did not reach the model; it cannot say what it found")
	}
	for _, e := range events {
		if b, _ := json.Marshal(e); strings.Contains(string(b), plantedKey) {
			t.Errorf("the key is in a %s trace event", e.Kind)
		}
	}
	if strings.Contains(string(saved), plantedKey) {
		t.Error("the key was written to the checkpoint")
	}
}

// A tool error message is output too, and is recorded by the same path.
func TestRedactScrubsToolErrors(t *testing.T) {
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "fetch", `{"path":"x"}`, 10, 10),
		endResponse("It failed.", 10, 10),
	}}
	a := &Agent{
		Provider: fake,
		Model:    "test",
		MaxSteps: 5,
		Redact:   redactPlanted,
		RunTool: func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{}, errors.New("upstream said: bad token " + plantedKey)
		},
	}
	if _, err := a.Run(context.Background(), NewState("run_redact_err", "fetch x")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sent, _ := json.Marshal(fake.Calls[1]); strings.Contains(string(sent), plantedKey) {
		t.Error("a key inside a tool error was sent to the provider")
	}
}
