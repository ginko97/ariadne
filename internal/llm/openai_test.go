package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
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

// A server that omits tool-call ids must not produce two blocks with the same
// empty id: resume keys its completion record on that id, so two empty ids
// collapse into one and the second call would never run.
func TestFromWireSynthesisesMissingCallIDs(t *testing.T) {
	body := []byte(`{
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": null,
      "tool_calls": [
        {"type":"function","function":{"name":"calc","arguments":"{\"expr\":\"1+1\"}"}},
        {"type":"function","function":{"name":"calc","arguments":"{\"expr\":\"2+2\"}"}}
      ]
    },
    "finish_reason": "tool_calls"
  }],
  "usage": {"prompt_tokens":1,"completion_tokens":1}
}`)

	got, err := fromWire(body)
	if err != nil {
		t.Fatal(err)
	}

	calls := got.ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].ID == "" || calls[1].ID == "" {
		t.Fatalf("empty id survived: %+v", calls)
	}
	if calls[0].ID == calls[1].ID {
		t.Errorf("ids collide: both %q", calls[0].ID)
	}
}

// A gateway that reports what it charged is believed over any local price
// table. OpenRouter sends usage.cost; providers that do not leave it zero.
func TestFromWireCarriesReportedCost(t *testing.T) {
	withCost := []byte(`{
  "choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
  "usage":{"prompt_tokens":2,"completion_tokens":10,"total_tokens":12,"cost":2.56e-05}
}`)
	got, err := fromWire(withCost)
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage.Cost != 2.56e-05 {
		t.Errorf("Cost = %v, want 2.56e-05", got.Usage.Cost)
	}

	// Gemini direct: no cost field, and its absence must not be an error.
	without := []byte(`{
  "choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
  "usage":{"prompt_tokens":2,"completion_tokens":10}
}`)
	got, err = fromWire(without)
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage.Cost != 0 {
		t.Errorf("Cost = %v, want 0 when unreported", got.Usage.Cost)
	}
}

// Some gateways (e.g. OpenRouter proxies or LiteLLM) send finish_reason: "stop"
// even when tool_calls are present. The tool calls must not be dropped.
func TestFromWireToolCallsWithStopFinishReason(t *testing.T) {
	body := []byte(`{
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "Let me check that.",
      "tool_calls": [
        {"id":"c1","type":"function","function":{"name":"calc","arguments":"{\"expr\":\"1+1\"}"}}
      ]
    },
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens":5,"completion_tokens":10}
}`)

	got, err := fromWire(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stop != StopToolUse {
		t.Errorf("Stop = %v, want %v (tool calls must not be dropped)", got.Stop, StopToolUse)
	}
	if len(got.ToolCalls()) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(got.ToolCalls()))
	}
}

func TestOpenAIStringRedaction(t *testing.T) {
	const secretKey = "sk-secret-test-key-xyz"
	o := NewOpenAI(secretKey, WithBaseURL("https://api.test.example"))

	for _, fmtStr := range []string{"%s", "%v", "%+v"} {
		got := fmt.Sprintf(fmtStr, o)
		if strings.Contains(got, secretKey) {
			t.Errorf("fmt.Sprintf(%q, o) leaked secret key: %s", fmtStr, got)
		}
		if !strings.Contains(got, "<redacted>") {
			t.Errorf("fmt.Sprintf(%q, o) = %q, want '<redacted>'", fmtStr, got)
		}
	}

	unset := &OpenAI{}
	gotUnset := fmt.Sprintf("%v", unset)
	if !strings.Contains(gotUnset, "<unset>") {
		t.Errorf("unset OpenAI string = %q, want '<unset>'", gotUnset)
	}

	// A copy must redact too. With a pointer receiver only *OpenAI satisfied
	// Stringer, so printing a dereferenced value fell back to the default
	// formatter and wrote the key out in full.
	for _, fmtStr := range []string{"%s", "%v", "%+v"} {
		got := fmt.Sprintf(fmtStr, *o)
		if strings.Contains(got, secretKey) {
			t.Errorf("fmt.Sprintf(%q, *o) leaked secret key: %s", fmtStr, got)
		}
		if !strings.Contains(got, "<redacted>") {
			t.Errorf("fmt.Sprintf(%q, *o) = %q, want '<redacted>'", fmtStr, got)
		}
	}

	// And when embedded in something else, which is how it reaches most logs.
	wrapper := struct{ Provider OpenAI }{Provider: *o}
	if got := fmt.Sprintf("%+v", wrapper); strings.Contains(got, secretKey) {
		t.Errorf("embedded OpenAI leaked secret key: %s", got)
	}
}

func TestWithTimeoutPreservesHTTPClientTransport(t *testing.T) {
	rt := &RecordedTransport{}
	client := &http.Client{Transport: rt}
	o := NewOpenAI("key", WithHTTPClient(client), WithTimeout(45*time.Second))

	if o.HTTP == nil || o.HTTP.Transport != rt {
		t.Errorf("WithTimeout wiped out custom Transport: %+v", o.HTTP)
	}
	if o.HTTP.Timeout != 45*time.Second {
		t.Errorf("Timeout = %v, want 45s", o.HTTP.Timeout)
	}
}
