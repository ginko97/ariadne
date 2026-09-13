package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

// A model can emit tool arguments that are not valid JSON — truncated by a
// token limit, or simply wrong. The fixture is a real response shape with
// `{"expr": "240*0.15"` in the arguments field: one brace short.
//
// The decode must survive it. `arguments` is a *string* on the wire and stays
// json.RawMessage in the Response, so nothing in this layer parses it — the
// damage has to reach the tool, which turns it into an IsError result the model
// can react to. A provider layer that validated here would turn a recoverable
// mistake into a dead run.
func TestFromWireKeepsMalformedToolArguments(t *testing.T) {
	body, err := os.ReadFile("testdata/malformed_tool_args.json")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := fromWire(body)
	if err != nil {
		t.Fatalf("a malformed argument string must not fail the decode: %v", err)
	}

	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls", len(calls))
	}
	if calls[0].Name != "calc" || calls[0].ID != "call_malformed_1" {
		t.Errorf("call = %+v", calls[0])
	}
	if resp.Stop != StopToolUse {
		t.Errorf("stop = %q", resp.Stop)
	}

	// Passed through exactly as sent, damage included.
	if got := string(calls[0].Args); got != `{"expr": "240*0.15"` {
		t.Errorf("arguments were altered: %s", got)
	}
	var v map[string]any
	if json.Unmarshal(calls[0].Args, &v) == nil {
		t.Error("the fixture is supposed to be invalid JSON")
	}
}

// End to end over the transport, since that is how a run meets it.
func TestCompleteSurvivesMalformedToolArguments(t *testing.T) {
	body, err := os.ReadFile("testdata/malformed_tool_args.json")
	if err != nil {
		t.Fatal(err)
	}
	rt := &RecordedTransport{Responses: [][]byte{body}}
	o := NewOpenAI("key",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithBaseURL("https://example.test/v1"))

	resp, err := o.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls()) != 1 {
		t.Fatalf("the call was lost: %+v", resp)
	}
}
