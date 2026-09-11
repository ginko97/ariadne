package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestToWire(t *testing.T) {
	req := Request{
		Model: "openai/gpt-4o-mini",
		Tools: []ToolDef{{
			Name:        "calc",
			Description: "do maths",
			Schema:      json.RawMessage(`{"type":"object"}`),
		}},
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{
				{Type: BlockText, Text: "what is 15% of 240?"},
			}},
			{Role: RoleAssistant, Blocks: []Block{
				{Type: BlockText, Text: "Let me compute that."},
				{Type: BlockToolUse, ID: "call_1", Name: "calc",
					Args: json.RawMessage(`{"expr":"240*0.15"}`)},
			}},
			{Role: RoleUser, Blocks: []Block{
				{Type: BlockToolResult, CallID: "call_1", Content: "36"},
			}},
		},
	}

	w, err := toWire(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	want := `{
  "model": "openai/gpt-4o-mini",
  "messages": [
    {
      "role": "user",
      "content": "what is 15% of 240?"
    },
    {
      "role": "assistant",
      "content": "Let me compute that.",
      "tool_calls": [
        {
          "id": "call_1",
          "type": "function",
          "function": {
            "name": "calc",
            "arguments": "{\"expr\":\"240*0.15\"}"
          }
        }
      ]
    },
    {
      "role": "tool",
      "content": "36",
      "tool_call_id": "call_1"
    }
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "calc",
        "description": "do maths",
        "parameters": {
          "type": "object"
        }
      }
    }
  ]
}`

	if string(got) != want {
		t.Errorf("toWire mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestToWireNullContentForToolOnlyTurn(t *testing.T) {
	req := Request{
		Model: "m",
		Messages: []Message{{
			Role: RoleAssistant,
			Blocks: []Block{
				{Type: BlockToolUse, ID: "c1", Name: "calc", Args: json.RawMessage(`{}`)},
			},
		}},
	}

	w, err := toWire(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(w)

	if !strings.Contains(string(got), `"content":null`) {
		t.Errorf("want content:null for a tool-only assistant turn, got %s", got)
	}
	if w.Messages[0].Content != nil {
		t.Errorf("Content should be nil, got %q", *w.Messages[0].Content)
	}
}

func TestFromWireToolCall(t *testing.T) {
	body := []byte(`{
  "id": "gen-123",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "Let me compute that.",
      "tool_calls": [{
        "id": "call_abc",
        "type": "function",
        "function": {"name": "calc", "arguments": "{\"expr\":\"240*0.15\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }],
  "usage": {"prompt_tokens": 52, "completion_tokens": 18}
}`)

	got, err := fromWire(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop != StopToolUse {
		t.Errorf("Stop = %q, want tool_use", got.Stop)
	}
	if got.Usage.InputTokens != 52 || got.Usage.OutputTokens != 18 {
		t.Errorf("Usage = %+v", got.Usage)
	}
	if len(got.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (text + tool_use)", len(got.Blocks))
	}
	calls := got.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "call_abc" || calls[0].Name != "calc" {
		t.Fatalf("calls = %+v", calls)
	}
	// unwrapped exactly once — not still escaped, not double-decoded
	if string(calls[0].Args) != `{"expr":"240*0.15"}` {
		t.Errorf("Args = %s", calls[0].Args)
	}
}

func TestFromWireTextOnly(t *testing.T) {
	body := []byte(`{
  "choices": [{"index":0,"message":{"role":"assistant","content":"15% of 240 is 36."},"finish_reason":"stop"}],
  "usage": {"prompt_tokens":94,"completion_tokens":11}
}`)

	got, err := fromWire(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop != StopEnd {
		t.Errorf("Stop = %q, want end", got.Stop)
	}
	if got.Text() != "15% of 240 is 36." {
		t.Errorf("Text() = %q", got.Text())
	}
	if len(got.ToolCalls()) != 0 {
		t.Error("unexpected tool calls")
	}
}

func TestFromWireErrorEnvelopeWith200(t *testing.T) {
	body := []byte(`{"error":{"message":"insufficient credits","type":"payment_required"}}`)

	if _, err := fromWire(body); err == nil {
		t.Fatal("want an error, got nil")
	} else if !strings.Contains(err.Error(), "insufficient credits") {
		t.Errorf("error must carry the message, got %v", err)
	}
}

func TestFromWireNoChoices(t *testing.T) {
	if _, err := fromWire([]byte(`{"choices":[]}`)); !errors.Is(err, ErrNoChoices) {
		t.Fatalf("got %v, want ErrNoChoices", err)
	}
}

func TestFromWireMissingFinishReason(t *testing.T) {
	body := []byte(`{
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": null,
      "tool_calls": [{"id":"c1","type":"function","function":{"name":"calc","arguments":"{}"}}]
    },
    "finish_reason": ""
  }],
  "usage": {"prompt_tokens":10,"completion_tokens":5}
}`)

	got, err := fromWire(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop != StopToolUse {
		t.Errorf("Stop = %q — the payload fallback in stopReason didn't fire", got.Stop)
	}
}

func TestCompleteSendsAndDecodes(t *testing.T) {
	rt := &RecordedTransport{Responses: [][]byte{[]byte(`{
  "choices": [{
    "index": 0,
    "message": {"role":"assistant","content":"36"},
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens":12,"completion_tokens":3}
}`)}}

	o := NewOpenAI("test-key",
		WithBaseURL("https://gateway.test/v1"),
		WithHeader("X-Title", "ariadne"),
		WithHTTPClient(&http.Client{Transport: rt}),
	)

	got, err := o.Complete(context.Background(), Request{
		Model:    "some/model",
		Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "15% of 240?"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// what came back
	if got.Text() != "36" || got.Stop != StopEnd {
		t.Errorf("response = %+v", got)
	}
	if got.Usage.InputTokens != 12 {
		t.Errorf("usage = %+v", got.Usage)
	}

	// what went out — the half that matters
	req := rt.Requests[0]
	if req.Method != http.MethodPost {
		t.Errorf("method = %s", req.Method)
	}
	if req.URL.String() != "https://gateway.test/v1/chat/completions" {
		t.Errorf("url = %s", req.URL)
	}
	if h := req.Header.Get("Authorization"); h != "Bearer test-key" {
		t.Errorf("auth = %q", h)
	}
	if h := req.Header.Get("Content-Type"); h != "application/json" {
		t.Errorf("content-type = %q", h)
	}
	if h := req.Header.Get("X-Title"); h != "ariadne" {
		t.Errorf("extra header lost: %q", h)
	}
	if !strings.Contains(string(rt.Bodies[0]), `"model":"some/model"`) {
		t.Errorf("body = %s", rt.Bodies[0])
	}
}
