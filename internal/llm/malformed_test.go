package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
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

	// The damage is preserved, but in a form State can hold: json.RawMessage
	// must contain valid JSON or the checkpoint cannot be written, and a run
	// that cannot be checkpointed stops. So the original text survives as a
	// JSON string rather than as raw bytes.
	//
	// This assertion used to demand the bytes pass through untouched, which is
	// what a decode layer *should* do — and was the behaviour that killed a run
	// during dogfooding, one step before the tool ever saw the arguments.
	if !json.Valid(calls[0].Args) {
		t.Fatalf("Args is not valid JSON, so no state holding it can be saved: %s", calls[0].Args)
	}
	var recovered string
	if err := json.Unmarshal(calls[0].Args, &recovered); err != nil {
		t.Fatalf("the original text is not recoverable: %v", err)
	}
	if recovered != `{"expr": "240*0.15"` {
		t.Errorf("the damage was not preserved: %q", recovered)
	}

	// And it must still fail to decode into a tool's argument struct, or a
	// malformed call would look like a valid one with empty fields.
	var v map[string]any
	if json.Unmarshal(calls[0].Args, &v) == nil {
		t.Error("malformed arguments decoded into an object")
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

// Malformed arguments must survive a JSON round trip, because State does.
//
// json.RawMessage has to hold *valid* JSON or it cannot be marshalled. A
// truncated argument string therefore made the entire conversation
// unserialisable: the checkpoint write failed, and by the loop's contract a run
// that cannot be checkpointed stops. The damage never reached the tool at all —
// it killed the run one step earlier, unresumably, because the thing that broke
// was the write that makes resuming possible.
//
// Dogfooding found this. The fixture above did not, because it stops at the
// decode and never asks whether the result can be stored.
func TestMalformedArgumentsSurviveAJSONRoundTrip(t *testing.T) {
	for _, tc := range []struct{ name, arguments string }{
		{"truncated", `{"expr": "240*0.15"`},
		{"empty", ""},
		{"whitespace", "   "},
		{"not json at all", "expr=240*0.15"},
		{"half an array", `[1, 2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := fromWire(bodyWithArgs(tc.arguments))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			calls := resp.ToolCalls()
			if len(calls) != 1 {
				t.Fatalf("got %d calls", len(calls))
			}

			// The property the checkpoint depends on.
			if !json.Valid(calls[0].Args) {
				t.Fatalf("Args is not valid JSON: %s", calls[0].Args)
			}
			if _, err := json.Marshal(struct {
				A json.RawMessage `json:"a"`
			}{calls[0].Args}); err != nil {
				t.Fatalf("a state holding this cannot be checkpointed: %v", err)
			}

			// And the damage still has to reach the tool, or a malformed call
			// would look like a successful one with no arguments.
			var into struct {
				Expr string `json:"expr"`
			}
			if tc.arguments != "" && strings.TrimSpace(tc.arguments) != "" {
				if err := json.Unmarshal(calls[0].Args, &into); err == nil && into.Expr != "" {
					t.Errorf("malformed arguments decoded cleanly into a tool struct: %s", calls[0].Args)
				}
			}
		})
	}
}

// The streaming path builds blocks itself and needs the same guarantee — a
// stream that ends mid-argument leaves a fragment that is not valid JSON.
func TestStreamedMalformedArgumentsSurviveAJSONRoundTrip(t *testing.T) {
	acc := newAccumulator()
	acc.add(Chunk{ToolCall: &ToolDelta{Index: 0, ID: "c1", Name: "calc", Args: `{"expr": "240*0.1`}})
	acc.add(Chunk{Stop: StopToolUse})

	calls := acc.response().ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls", len(calls))
	}
	if !json.Valid(calls[0].Args) {
		t.Fatalf("a truncated stream produced unserialisable Args: %s", calls[0].Args)
	}
}

func bodyWithArgs(arguments string) []byte {
	quoted, err := json.Marshal(arguments)
	if err != nil {
		panic(err)
	}
	return []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"calc",` +
		`"arguments":` + string(quoted) + `}}]},"finish_reason":"tool_calls"}]}`)
}
