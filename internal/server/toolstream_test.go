package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

func toolUseResponse(id, name, args string) llm.Response {
	return llm.Response{
		Blocks: []llm.Block{{Type: llm.BlockToolUse, ID: id, Name: name, Args: []byte(args)}},
		Stop:   llm.StopToolUse,
		Usage:  llm.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

// toolServer is newTestServer with a tool and the agent fields the live tool
// events depend on.
func toolServer(t *testing.T, runTool func(context.Context, llm.ToolCall) (llm.ToolResult, error),
	configure func(*loop.Agent)) (*Server, *httptest.Server) {
	t.Helper()
	store := &loop.Store{Dir: t.TempDir()}
	fake := &llm.Fake{Responses: []llm.Response{
		toolUseResponse("c1", "calc", `{"expr":"1+1"}`),
		endResponse("done"),
	}}
	s := New(store,
		func(runID string, state *loop.State, onDelta func(llm.Chunk)) (*loop.Agent, func()) {
			a := &loop.Agent{
				Provider:   fake,
				Model:      "test-model",
				MaxSteps:   5,
				Checkpoint: store.Save,
				RunTool:    runTool,
			}
			configure(a)
			return a, nil
		},
		func() string { return "run_tools" },
	)
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func toolResultEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range events(t, body) {
		if e.Name == "tool_result" {
			out = append(out, e.Data)
		}
	}
	return out
}

// The page is shown a tool's result as it happens. It must be the redacted
// result, the one the checkpoint keeps: an approved exec of `type ..\.env`
// otherwise puts ariadne's own key on the screen, and a reload hides that it
// ever did.
func TestStreamedToolResultIsRedacted(t *testing.T) {
	const key = "sk-SECRET-KEY-123"
	s, ts := toolServer(t,
		func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			return llm.ToolResult{Content: "ARIADNE_API_KEY=" + key}, nil
		},
		func(a *loop.Agent) {
			a.Redact = func(s string) string { return strings.ReplaceAll(s, key, "[redacted]") }
		})

	body := bodyOf(t, post(t, s, ts, `{"message":"read the env file"}`, nil))
	if strings.Contains(body, key) {
		t.Fatalf("the key reached the page in the stream:\n%s", body)
	}
	results := toolResultEvents(t, body)
	if len(results) != 1 || results[0]["text"] != "ARIADNE_API_KEY=[redacted]" || results[0]["id"] != "c1" {
		t.Errorf("tool_result events = %v, want one for c1 with the redacted text", results)
	}
}

// A tool abandoned at the timeout is still shown a result: the notice the
// model is given, not a block left waiting. The tool finishing afterwards,
// after the turn and the handler are over, must not write to the response;
// -race reported exactly that when the result went out from RunTool.
func TestAbandonedToolShowsTheTimeoutAndWritesNothingLater(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	s, ts := toolServer(t,
		func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			defer close(finished)
			<-release
			return llm.ToolResult{Content: "late"}, nil
		},
		func(a *loop.Agent) { a.ToolTimeout = 20 * time.Millisecond })

	body := bodyOf(t, post(t, s, ts, `{"message":"slow"}`, nil))
	results := toolResultEvents(t, body)
	if len(results) != 1 || !strings.Contains(results[0]["text"].(string), "abandoned") || results[0]["is_error"] != true {
		t.Errorf("tool_result events = %v, want the abandoned notice as an error", results)
	}
	close(release)
	<-finished
}

func TestSSEWriterDropsEventsOnceClosed(t *testing.T) {
	rec := httptest.NewRecorder()
	out := &sseWriter{w: rec, f: rec}
	out.event("start", map[string]any{})
	out.close()
	out.event("tool_result", map[string]any{"text": "late"})
	if strings.Contains(rec.Body.String(), "late") {
		t.Errorf("an event was written after close:\n%s", rec.Body.String())
	}
}

// A tool denied by policy (such as not being in the agent's allow-list) emits a
// tool_result event with is_error=true carrying the refusal text, ensuring the
// Web UI updates rather than hanging on calling...
func TestDeniedToolEmitsToolResultWithError(t *testing.T) {
	s, ts := toolServer(t,
		func(context.Context, llm.ToolCall) (llm.ToolResult, error) {
			t.Fatal("RunTool must not be called for a denied tool")
			return llm.ToolResult{}, nil
		},
		func(a *loop.Agent) {
			a.Allow = []string{"fetch"}
		})

	body := bodyOf(t, post(t, s, ts, `{"message":"run disallowed tool"}`, nil))
	results := toolResultEvents(t, body)
	if len(results) != 1 {
		t.Fatalf("tool_result events = %v, want 1 event for denied tool", results)
	}
	r := results[0]
	if r["id"] != "c1" || r["name"] != "calc" {
		t.Errorf("got id=%v, name=%v; want id=c1, name=calc", r["id"], r["name"])
	}
	if r["is_error"] != true {
		t.Errorf("got is_error=%v, want true", r["is_error"])
	}
	text, _ := r["text"].(string)
	if !strings.Contains(text, "not permitted") {
		t.Errorf("text = %q, want notice containing 'not permitted'", text)
	}
}
