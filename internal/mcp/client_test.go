package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/mcp"
	"github.com/ginko97/ariadne/internal/tool"
)

// dial starts the echo server from testdata and returns a connected Server.
//
// `go run` compiles on first use, so the timeout is generous. No network, no
// API key — this exercises the real protocol over a real pipe, which is the
// only way to find out whether the adapter actually works.
func dial(t *testing.T) *mcp.Server {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	s, err := mcp.Dial(ctx, "go", "run", "./testdata/echoserver")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestToolsAreDiscovered(t *testing.T) {
	s := dial(t)

	tools, err := s.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}

	byName := map[string]tool.Tool{}
	for _, tl := range tools {
		byName[tl.Name()] = tl
	}

	echo, ok := byName["echo"]
	if !ok {
		t.Fatalf("no echo tool; got %v", byName)
	}
	// The description is what the model is shown, so it has to survive the trip
	// intact — not merely be non-empty.
	if got, want := echo.Description(), "Return the text you were given"; got != want {
		t.Errorf("description = %q, want %q", got, want)
	}

	// The schema must be valid JSON the model can be shown, and must describe
	// the argument the server actually inferred from its Go type.
	if !json.Valid(echo.Schema()) {
		t.Fatalf("schema is not valid JSON: %s", echo.Schema())
	}
	if !strings.Contains(string(echo.Schema()), `"text"`) {
		t.Errorf("schema does not mention the text property: %s", echo.Schema())
	}
}

// A remote tool must be indistinguishable from a local one once registered.
func TestRemoteToolThroughRegistry(t *testing.T) {
	s := dial(t)

	tools, err := s.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}

	reg := tool.New(append(tools, tool.Calc{})...)

	if got := len(reg.Defs()); got != 3 {
		t.Fatalf("registry has %d tools, want 3 (echo, upper, calc)", got)
	}

	res, err := reg.Call(context.Background(), llmToolCall("call_1", "upper", `{"text":"hello"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", res.Content)
	}
	if res.Content != "HELLO" {
		t.Errorf("content = %q, want HELLO", res.Content)
	}
}

// A tool that runs and reports failure must come back as IsError, not as a
// returned error — the distinction the stage-4b freeze introduced.
func TestRemoteToolIsErrorSurvives(t *testing.T) {
	s := dial(t)

	tools, err := s.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	reg := tool.New(tools...)

	res, err := reg.Call(context.Background(), llmToolCall("call_2", "upper", `{"text":""}`))
	if err != nil {
		t.Fatalf("want an IsError result, got a returned error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "required") {
		t.Errorf("content = %q", res.Content)
	}
}

func llmToolCall(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Args: json.RawMessage(args)}
}
